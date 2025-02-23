package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/samber/lo"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/flaps"
	machcmd "github.com/superfly/flyctl/internal/command/machine"
	"github.com/superfly/flyctl/internal/machine"
	"github.com/superfly/flyctl/terminal"
	"golang.org/x/exp/slices"
)

type ProcessGroupsDiff struct {
	machinesToRemove      []machine.LeasableMachine
	groupsToRemove        map[string]int
	groupsNeedingMachines map[string]bool
}

func (md *machineDeployment) DeployMachinesApp(ctx context.Context) error {
	ctx = flaps.NewContext(ctx, md.flapsClient)

	if err := md.updateReleaseInBackend(ctx, "running"); err != nil {
		return fmt.Errorf("failed to set release status to 'running': %w", err)
	}

	var err error
	if md.restartOnly {
		err = md.restartMachinesApp(ctx)
	} else {
		err = md.deployMachinesApp(ctx)
	}
	status := "complete"
	if err != nil {
		status = "failed"
	}

	if updateErr := md.updateReleaseInBackend(ctx, status); updateErr != nil {
		if err == nil {
			err = fmt.Errorf("failed to set final release status: %w", updateErr)
		} else {
			terminal.Warnf("failed to set final release status after deployment failure: %v\n", updateErr)
		}
	}
	return err
}

// restartMachinesApp only restarts existing machines but updates their release metadata
func (md *machineDeployment) restartMachinesApp(ctx context.Context) error {
	if err := md.machineSet.AcquireLeases(ctx, md.leaseTimeout); err != nil {
		return err
	}
	defer md.machineSet.ReleaseLeases(ctx) // skipcq: GO-S2307
	md.machineSet.StartBackgroundLeaseRefresh(ctx, md.leaseTimeout, md.leaseDelayBetween)

	machineUpdateEntries := lo.Map(md.machineSet.GetMachines(), func(lm machine.LeasableMachine, _ int) *machineUpdateEntry {
		return &machineUpdateEntry{leasableMachine: lm, launchInput: md.launchInputForRestart(lm.Machine())}
	})

	return md.updateExistingMachines(ctx, machineUpdateEntries)
}

// deployMachinesApp executes the following flow:
//   - Run release command
//   - Remove spare machines from removed groups
//   - Launch new machines on new groups
//   - Update existing machines
func (md *machineDeployment) deployMachinesApp(ctx context.Context) error {
	if err := md.runReleaseCommand(ctx); err != nil {
		return fmt.Errorf("release command failed - aborting deployment. %w", err)
	}

	if err := md.machineSet.AcquireLeases(ctx, md.leaseTimeout); err != nil {
		return err
	}
	defer md.machineSet.ReleaseLeases(ctx) // skipcq: GO-S2307
	md.machineSet.StartBackgroundLeaseRefresh(ctx, md.leaseTimeout, md.leaseDelayBetween)

	processGroupMachineDiff := md.resolveProcessGroupChanges()
	md.warnAboutProcessGroupChanges(ctx, processGroupMachineDiff)

	if len(processGroupMachineDiff.machinesToRemove) > 0 {
		// Destroy machines that don't fit the current process groups
		if err := md.machineSet.RemoveMachines(ctx, processGroupMachineDiff.machinesToRemove); err != nil {
			return err
		}
		for _, mach := range processGroupMachineDiff.machinesToRemove {
			if err := machcmd.Destroy(ctx, md.app, mach.Machine(), true); err != nil {
				return err
			}
		}
	}

	// Create machines for new process groups
	if len(processGroupMachineDiff.groupsNeedingMachines) > 0 {
		i := 0
		for name := range processGroupMachineDiff.groupsNeedingMachines {
			if err := md.spawnMachineInGroup(ctx, name, i, len(processGroupMachineDiff.groupsNeedingMachines)); err != nil {
				return err
			}
			i++
		}
		fmt.Fprintf(md.io.ErrOut, "Finished launching new machines\n")
	}

	var machineUpdateEntries []*machineUpdateEntry
	for _, lm := range md.machineSet.GetMachines() {
		li, err := md.launchInputForUpdate(lm.Machine())
		if err != nil {
			return fmt.Errorf("failed to update machine configuration for %s: %w", lm.FormattedMachineId(), err)
		}
		machineUpdateEntries = append(machineUpdateEntries, &machineUpdateEntry{leasableMachine: lm, launchInput: li})
	}

	return md.updateExistingMachines(ctx, machineUpdateEntries)
}

type machineUpdateEntry struct {
	leasableMachine machine.LeasableMachine
	launchInput     *api.LaunchMachineInput
}

func formatIndex(n, total int) string {
	pad := 0
	for i := total; i != 0; i /= 10 {
		pad++
	}
	return fmt.Sprintf("[%0*d/%d]", pad, n+1, total)
}

func (md *machineDeployment) updateExistingMachines(ctx context.Context, updateEntries []*machineUpdateEntry) error {
	// FIXME: handle deploy strategy: rolling, immediate, canary, bluegreen
	fmt.Fprintf(md.io.Out, "Updating existing machines in '%s' with %s strategy\n", md.colorize.Bold(md.app.Name), md.strategy)
	for i, e := range updateEntries {
		lm := e.leasableMachine
		launchInput := e.launchInput
		indexStr := formatIndex(i, len(updateEntries))

		if launchInput.ID != lm.Machine().ID {
			// If IDs don't match, destroy the original machine and launch a new one
			// This can be the case for machines that changes its volumes or any other immutable config
			fmt.Fprintf(md.io.ErrOut, "  %s Replacing %s by new machine\n", indexStr, md.colorize.Bold(lm.FormattedMachineId()))
			if err := lm.Destroy(ctx, true); err != nil {
				if md.strategy != "immediate" {
					return err
				}
				fmt.Fprintf(md.io.ErrOut, "Continuing after error: %s\n", err)
			}

			newMachineRaw, err := md.flapsClient.Launch(ctx, *launchInput)
			if err != nil {
				if md.strategy != "immediate" {
					return err
				}
				fmt.Fprintf(md.io.ErrOut, "Continuing after error: %s\n", err)
				continue
			}

			lm = machine.NewLeasableMachine(md.flapsClient, md.io, newMachineRaw)
			fmt.Fprintf(md.io.ErrOut, "  %s Created machine %s\n", indexStr, md.colorize.Bold(lm.FormattedMachineId()))

		} else {
			fmt.Fprintf(md.io.ErrOut, "  %s Updating %s\n", indexStr, md.colorize.Bold(lm.FormattedMachineId()))
			if err := lm.Update(ctx, *launchInput); err != nil {
				if md.strategy != "immediate" {
					return err
				}
				fmt.Fprintf(md.io.ErrOut, "Continuing after error: %s\n", err)
			}
		}

		if md.strategy == "immediate" {
			continue
		}

		if err := lm.WaitForState(ctx, api.MachineStateStarted, md.waitTimeout, indexStr); err != nil {
			return err
		}

		if !md.skipHealthChecks {
			if err := lm.WaitForHealthchecksToPass(ctx, md.waitTimeout, indexStr); err != nil {
				return err
			}
			// FIXME: combine this wait with the wait for start as one update line (or two per in noninteractive case)
			md.logClearLinesAbove(1)
			fmt.Fprintf(md.io.ErrOut, "  %s Machine %s update finished: %s\n",
				indexStr,
				md.colorize.Bold(lm.FormattedMachineId()),
				md.colorize.Green("success"),
			)
		}
	}

	fmt.Fprintf(md.io.ErrOut, "  Finished deploying\n")
	return nil
}

func (md *machineDeployment) spawnMachineInGroup(ctx context.Context, groupName string, i, total int) error {
	if groupName == "" {
		// If the group is unspecified, it should have been translated to "app" by this point
		panic("spawnMachineInGroup requires a non-empty group name. this is a bug!")
	}
	fmt.Fprintf(md.io.Out, "No machines in group '%s', launching one new machine\n", md.colorize.Bold(groupName))
	launchInput, err := md.launchInputForLaunch(groupName, md.machineGuest)
	if err != nil {
		return fmt.Errorf("error creating machine configuration: %w", err)
	}

	newMachineRaw, err := md.flapsClient.Launch(ctx, *launchInput)
	if err != nil {
		relCmdWarning := ""
		if strings.Contains(err.Error(), "please add a payment method") && !md.releaseCommandMachine.IsEmpty() {
			relCmdWarning = "\nPlease note that release commands run in their own ephemeral machine, and therefore count towards the machine limit."
		}
		return fmt.Errorf("error creating a new machine: %w%s", err, relCmdWarning)
	}

	newMachine := machine.NewLeasableMachine(md.flapsClient, md.io, newMachineRaw)

	indexStr := formatIndex(i, total)

	// FIXME: dry this up with release commands and non-empty update
	fmt.Fprintf(md.io.ErrOut, "  Created release_command machine %s\n", md.colorize.Bold(newMachineRaw.ID))
	if md.strategy != "immediate" {
		err := newMachine.WaitForState(ctx, api.MachineStateStarted, md.waitTimeout, indexStr)
		if err != nil {
			return err
		}
	}
	if md.strategy != "immediate" && !md.skipHealthChecks {
		err := newMachine.WaitForHealthchecksToPass(ctx, md.waitTimeout, indexStr)
		// FIXME: combine this wait with the wait for start as one update line (or two per in noninteractive case)
		if err != nil {
			return err
		} else {
			md.logClearLinesAbove(1)
			fmt.Fprintf(md.io.ErrOut, "  Machine %s update finished: %s\n",
				md.colorize.Bold(newMachine.FormattedMachineId()),
				md.colorize.Green("success"),
			)
		}
	}
	return nil
}

func (md *machineDeployment) resolveProcessGroupChanges() ProcessGroupsDiff {
	output := ProcessGroupsDiff{
		groupsToRemove:        map[string]int{},
		groupsNeedingMachines: map[string]bool{},
	}

	groupsInConfig := md.appConfig.ProcessNames()
	groupHasMachine := map[string]bool{}

	for _, leasableMachine := range md.machineSet.GetMachines() {
		name := leasableMachine.Machine().ProcessGroup()
		if slices.Contains(groupsInConfig, name) {
			groupHasMachine[name] = true
		} else {
			output.groupsToRemove[name] += 1
			output.machinesToRemove = append(output.machinesToRemove, leasableMachine)
		}
	}

	for _, name := range groupsInConfig {
		if ok := groupHasMachine[name]; !ok {
			output.groupsNeedingMachines[name] = true
		}
	}

	return output
}

func (md *machineDeployment) warnAboutProcessGroupChanges(ctx context.Context, diff ProcessGroupsDiff) {
	willAddMachines := len(diff.groupsNeedingMachines) != 0
	willRemoveMachines := diff.machinesToRemove != nil

	if !willAddMachines && !willRemoveMachines {
		return
	}

	fmt.Fprintln(md.io.Out, "Process groups have changed. This will:")

	if willRemoveMachines {
		bullet := md.colorize.Red("*")
		for grp, numMach := range diff.groupsToRemove {
			pluralS := lo.Ternary(numMach == 1, "", "s")
			fmt.Fprintf(md.io.Out, " %s destroy %d \"%s\" machine%s\n", bullet, numMach, grp, pluralS)
		}
	}
	if willAddMachines {
		bullet := md.colorize.Green("*")
		for name := range diff.groupsNeedingMachines {
			fmt.Fprintf(md.io.Out, " %s create 1 \"%s\" machine\n", bullet, name)
		}
	}
	fmt.Fprint(md.io.Out, "\n")
}

package deploy

import (
	"fmt"
	"strconv"

	"github.com/samber/lo"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/internal/machine"
	"github.com/superfly/flyctl/terminal"
)

func (md *machineDeployment) launchInputForRestart(origMachineRaw *api.Machine) *api.LaunchMachineInput {
	Config := machine.CloneConfig(origMachineRaw.Config)
	md.setMachineReleaseData(Config)

	return &api.LaunchMachineInput{
		ID:      origMachineRaw.ID,
		AppID:   md.app.Name,
		OrgSlug: md.app.Organization.ID,
		Config:  Config,
		Region:  origMachineRaw.Region,
	}
}

func (md *machineDeployment) launchInputForLaunch(processGroup string, guest *api.MachineGuest) (*api.LaunchMachineInput, error) {
	mConfig, err := md.appConfig.ToMachineConfig(processGroup, nil)
	if err != nil {
		return nil, err
	}
	mConfig.Guest = guest
	mConfig.Image = md.img
	md.setMachineReleaseData(mConfig)
	// Get the final process group and prevent empty string
	processGroup = mConfig.ProcessGroup()

	if len(mConfig.Mounts) > 0 {
		mount0 := &mConfig.Mounts[0]
		if len(md.volumes[mount0.Name]) == 0 {
			return nil, fmt.Errorf("New machine in group '%s' needs an unattached volume named '%s'", processGroup, mount0.Name)
		}
		mount0.Volume = md.volumes[mount0.Name][0].ID
	}

	return &api.LaunchMachineInput{
		AppID:   md.app.Name,
		OrgSlug: md.app.Organization.ID,
		Region:  md.appConfig.PrimaryRegion,
		Config:  mConfig,
	}, nil
}

func (md *machineDeployment) launchInputForUpdate(origMachineRaw *api.Machine) (*api.LaunchMachineInput, error) {
	mID := origMachineRaw.ID
	processGroup := origMachineRaw.Config.ProcessGroup()

	mConfig, err := md.appConfig.ToMachineConfig(processGroup, origMachineRaw.Config)
	if err != nil {
		return nil, err
	}
	mConfig.Image = md.img
	md.setMachineReleaseData(mConfig)
	// Get the final process group and prevent empty string
	processGroup = mConfig.ProcessGroup()

	// Mounts needs special treatment:
	//   * Volumes attached to existings machines can't be swapped by other volumes
	//   * The only allowed in-place operation is to update its destination mount path
	//   * The other option is to force a machine replacement to remove or attach a different volume
	mMounts := mConfig.Mounts
	oMounts := origMachineRaw.Config.Mounts
	if len(oMounts) != 0 {
		switch {
		case len(mMounts) == 0:
			// The mounts section was removed from fly.toml
			mID = "" // Forces machine replacement
			terminal.Warnf("Machine %s has a volume attached but fly.toml doesn't have a [mounts] section\n", mID)
		case oMounts[0].Name == "":
			// It's rare but can happen, we don't know the mounted volume name
			// so can't be sure it matches the mounts defined in fly.toml, in this
			// case assume we want to retain existing mount
			mMounts[0] = oMounts[0]
		case mMounts[0].Name != oMounts[0].Name:
			// The expected volume name for the machine and fly.toml are out sync
			// As we can't change the volume for a running machine, the only
			// way is to destroy the current machine and launch a new one with the new volume attached
			terminal.Warnf("Machine %s has volume '%s' attached but fly.toml have a different name: '%s'\n", mID, oMounts[0].Name, mMounts[0].Name)
			if len(md.volumes[mMounts[0].Name]) == 0 {
				return nil, fmt.Errorf("machine in group '%s' needs an unattached volume named '%s'", processGroup, mMounts[0].Name)
			}
			mMounts[0].Volume = md.volumes[mMounts[0].Name][0].ID
			mID = "" // Forces machine replacement
		case mMounts[0].Path != oMounts[0].Path:
			// The volume is the same but its mount path changed. Not a big deal.
			terminal.Warnf(
				"Updating the mount path for volume %s on machine %s from %s to %s due to fly.toml [mounts] destination value\n",
				oMounts[0].Volume, mID, oMounts[0].Path, mMounts[0].Path,
			)
			// Copy the volume id over because path is already correct
			mMounts[0].Volume = oMounts[0].Volume
		default:
			// In any other case retain the existing machine mounts
			mMounts[0] = oMounts[0]
		}
	} else if len(mMounts) != 0 {
		// Replace the machine because [mounts] section was added to fly.toml
		// and it is not possible to attach a volume to an existing machine.
		// The volume could be in a different zone than the machine.
		mount0 := &mMounts[0]
		if len(md.volumes[mount0.Name]) == 0 {
			return nil, fmt.Errorf("machine in group '%s' needs an unattached volume named '%s'", processGroup, mMounts[0].Name)
		}
		mount0.Volume = md.volumes[mount0.Name][0].ID
		mID = "" // Forces machine replacement
	}

	return &api.LaunchMachineInput{
		ID:      mID,
		AppID:   md.app.Name,
		OrgSlug: md.app.Organization.ID,
		Region:  origMachineRaw.Region,
		Config:  mConfig,
	}, nil
}

func (md *machineDeployment) setMachineReleaseData(mConfig *api.MachineConfig) {
	mConfig.Metadata = lo.Assign(mConfig.Metadata, map[string]string{
		api.MachineConfigMetadataKeyFlyReleaseId:      md.releaseId,
		api.MachineConfigMetadataKeyFlyReleaseVersion: strconv.Itoa(md.releaseVersion),
	})

	// These defaults should come from appConfig.ToMachineConfig() and set on launch;
	// leave them here for the moment becase very old machines may not have them
	// and we want to set in case of simple app restarts
	if _, ok := mConfig.Metadata[api.MachineConfigMetadataKeyFlyPlatformVersion]; !ok {
		mConfig.Metadata[api.MachineConfigMetadataKeyFlyPlatformVersion] = api.MachineFlyPlatformVersion2
	}
	if _, ok := mConfig.Metadata[api.MachineConfigMetadataKeyFlyProcessGroup]; !ok {
		mConfig.Metadata[api.MachineConfigMetadataKeyFlyProcessGroup] = api.MachineProcessGroupApp
	}

	// FIXME: Move this as extra metadata read from a machineDeployment argument
	// It is not clear we have to cleanup the postgres metadata
	if md.app.IsPostgresApp() {
		mConfig.Metadata[api.MachineConfigMetadataKeyFlyManagedPostgres] = "true"
	} else {
		delete(mConfig.Metadata, api.MachineConfigMetadataKeyFlyManagedPostgres)
	}
}

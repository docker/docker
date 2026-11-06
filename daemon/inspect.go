package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/storage"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
)

// ContainerInspect returns low-level information about a
// container. Returns an error if the container cannot be found, or if
// there is an error getting the data.
func (daemon *Daemon) ContainerInspect(ctx context.Context, name string, options backend.ContainerInspectOptions) (_ *containertypes.InspectResponse, desiredMACAddress networktypes.HardwareAddr, _ error) {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return nil, nil, err
	}
	base, desiredMACAddress, err := daemon.containerInspect(&daemon.config().Config, ctr)
	if err != nil {
		return base, desiredMACAddress, err
	}

	// Don’t hold container lock for size calculation (see https://github.com/moby/moby/issues/31158)
	if options.Size {
		sizeRw, sizeRootFs, err := daemon.imageService.GetContainerLayerSize(ctx, base.ID)
		if err != nil {
			return nil, nil, err
		}
		base.SizeRw = &sizeRw
		base.SizeRootFs = &sizeRootFs
	}
	return base, desiredMACAddress, nil
}

func (daemon *Daemon) containerInspect(daemonCfg *config.Config, ctr *container.Container) (_ *containertypes.InspectResponse, desiredMACAddress networktypes.HardwareAddr, _ error) {
	ctr.Lock()
	defer ctr.Unlock()

	base, desiredMACAddress, err := daemon.getInspectData(daemonCfg, ctr)
	if err != nil {
		return nil, nil, err
	}

	if !daemon.UsesSnapshotter() {
		if ctr.RWLayer == nil {
			if ctr.State.Dead {
				return base, desiredMACAddress, nil
			}
			return nil, nil, errdefs.System(errors.New("RWLayer of container " + ctr.ID + " is unexpectedly nil"))
		}

		graphDriverData, err := ctr.RWLayer.Metadata()
		if err != nil {
			if ctr.State.Dead {
				// container is marked as Dead, and its graphDriver metadata may
				// have been removed; we can ignore errors.
				return base, desiredMACAddress, nil
			}
			return nil, nil, errdefs.System(err)
		}

		base.GraphDriver.Data = graphDriverData
	}

	return base, desiredMACAddress, nil
}

func (daemon *Daemon) getInspectData(daemonCfg *config.Config, ctr *container.Container) (_ *containertypes.InspectResponse, desiredMACAddress networktypes.HardwareAddr, _ error) {
	// make a copy to play with
	hostConfig := *ctr.HostConfig

	// Add information for legacy links
	children := daemon.linkIndex.children(ctr)
	hostConfig.Links = nil // do not expose the internal structure
	for linkAlias, child := range children {
		hostConfig.Links = append(hostConfig.Links, fmt.Sprintf("%s:%s", child.Name, linkAlias))
	}

	// We merge the Ulimits from hostConfig with daemon default
	daemon.mergeUlimits(&hostConfig, daemonCfg)

	// Migrate the container's default network's MacAddress to the top-level
	// Config.MacAddress field for older API versions (< 1.44). We set it here
	// unconditionally, to keep backward compatibility with clients that use
	// unversioned API endpoints.
	var macAddress networktypes.HardwareAddr
	if ctr.Config != nil {
		if nwm := hostConfig.NetworkMode; nwm.IsBridge() || nwm.IsUserDefined() {
			if epConf, ok := ctr.NetworkSettings.Networks[nwm.NetworkName()]; ok {
				macAddress = epConf.DesiredMacAddress
			}
		}
	}

	var containerHealth *containertypes.Health
	if ctr.State.Health != nil {
		containerHealth = &containertypes.Health{
			Status:        ctr.State.Health.Status(),
			FailingStreak: ctr.State.Health.Health.FailingStreak,
			Log:           append([]*containertypes.HealthcheckResult{}, ctr.State.Health.Health.Log...),
		}
	}

	inspectResponse := &containertypes.InspectResponse{
		ID:      ctr.ID,
		Created: ctr.Created.Format(time.RFC3339Nano),
		Path:    ctr.Path,
		Args:    ctr.Args,
		State: &containertypes.State{
			Status:     ctr.State.State(),
			Running:    ctr.State.Running,
			Paused:     ctr.State.Paused,
			Restarting: ctr.State.Restarting,
			OOMKilled:  ctr.State.OOMKilled,
			Dead:       ctr.State.Dead,
			Pid:        ctr.State.Pid,
			ExitCode:   ctr.State.ExitCode,
			Error:      ctr.State.ErrorMsg,
			StartedAt:  ctr.State.StartedAt.Format(time.RFC3339Nano),
			FinishedAt: ctr.State.FinishedAt.Format(time.RFC3339Nano),
			Health:     containerHealth,
		},
		Image:           ctr.ImageID.String(),
		ResolvConfPath:  ctr.ResolvConfPath, // Only used on Linux.
		HostnamePath:    ctr.HostnamePath,   // Only used on Linux.
		HostsPath:       ctr.HostsPath,      // Only used on Linux.
		LogPath:         ctr.LogPath,
		Name:            ctr.Name,
		RestartCount:    ctr.RestartCount,
		Driver:          ctr.Driver,
		Platform:        ctr.ImagePlatform.OS,
		MountLabel:      ctr.MountLabel,      // Only used on Linux.
		ProcessLabel:    ctr.ProcessLabel,    // Only used on Linux.
		AppArmorProfile: ctr.AppArmorProfile, // Only used on Linux.
		ExecIDs:         ctr.GetExecIDs(),
		HostConfig:      &hostConfig,
		Config:          ctr.Config,
	}

	// TODO(thaJeztah): do we need a deep copy here? Otherwise we could use maps.Clone (see https://github.com/moby/moby/commit/7917a36cc787ada58987320e67cc6d96858f3b55)
	ports := make(networktypes.PortMap, len(ctr.NetworkSettings.Ports))
	maps.Copy(ports, ctr.NetworkSettings.Ports)

	apiNetworks := make(map[string]*networktypes.EndpointSettings)
	for nwName, epConf := range ctr.NetworkSettings.Networks {
		if epConf.EndpointSettings != nil {
			// We must make a copy of this pointer object otherwise it can race with other operations
			apiNetworks[nwName] = epConf.EndpointSettings.Copy()
		}
	}

	inspectResponse.NetworkSettings = &containertypes.NetworkSettings{
		SandboxID:  ctr.NetworkSettings.SandboxID,
		SandboxKey: ctr.NetworkSettings.SandboxKey,
		Ports:      ports,
		Networks:   apiNetworks,
	}
	inspectResponse.Mounts = ctr.GetMountPoints()

	imageManifest := ctr.ImageManifest
	if imageManifest != nil && imageManifest.Platform == nil {
		// Copy the image manifest to avoid mutating the original
		c := *imageManifest
		imageManifest = &c

		imageManifest.Platform = &ctr.ImagePlatform
	}

	inspectResponse.ImageManifestDescriptor = imageManifest

	if daemon.UsesSnapshotter() {
		inspectResponse.Storage = &storage.Storage{
			RootFS: &storage.RootFSStorage{
				Snapshot: &storage.RootFSStorageSnapshot{
					Name: ctr.Driver,
				},
			},
		}
	} else {
		inspectResponse.GraphDriver = &storage.DriverData{
			Name: ctr.Driver,
		}
	}
	return inspectResponse, macAddress, nil
}

// ContainerExecInspect returns low-level information about the exec
// command. An error is returned if the exec cannot be found.
func (daemon *Daemon) ContainerExecInspect(id string) (*containertypes.ExecInspectResponse, error) {
	e := daemon.execCommands.Get(id)
	if e == nil {
		return nil, errExecNotFound(id)
	}

	if ctr := daemon.containers.Get(e.Container.ID); ctr == nil {
		return nil, errExecNotFound(id)
	}

	e.Lock()
	defer e.Unlock()
	var pid int
	if e.Process != nil {
		pid = int(e.Process.Pid())
	}
	var privileged *bool
	if runtime.GOOS != "windows" || e.Privileged {
		// Privileged is not used on Windows, so should always be false
		// (and omitted in the response), but set it if it happened to
		// be true. On non-Windows, we always set it, and the field should
		// not be omitted.
		privileged = &e.Privileged
	}

	return &containertypes.ExecInspectResponse{
		ID:       e.ID,
		Running:  e.Running,
		ExitCode: e.ExitCode,
		ProcessConfig: &containertypes.ExecProcessConfig{
			Tty:        e.Tty,
			Entrypoint: e.Entrypoint,
			Arguments:  e.Args,
			Privileged: privileged, // Privileged is not used on Windows
			User:       e.User,     // User is not used on Windows
		},
		OpenStdin:   e.OpenStdin,
		OpenStdout:  e.OpenStdout,
		OpenStderr:  e.OpenStderr,
		CanRemove:   e.CanRemove,
		ContainerID: e.Container.ID,
		DetachKeys:  e.DetachKeys,
		Pid:         pid,
		Labels:      e.Labels,
	}, nil
}

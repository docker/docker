package container // import "github.com/docker/docker/container"

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/system"
)

const (
	containerSecretMountPath         = `C:\ProgramData\Docker\secrets`
	containerInternalSecretMountPath = `C:\ProgramData\Docker\internal\secrets`
	containerInternalConfigsDirPath  = `C:\ProgramData\Docker\internal\configs`

	// DefaultStopTimeout is the timeout (in seconds) for the shutdown call on a container
	DefaultStopTimeout = 30
)

// UnmountIpcMount unmounts Ipc related mounts.
// This is a NOOP on windows.
func (container *Container) UnmountIpcMount() error {
	return nil
}

// IpcMounts returns the list of Ipc related mounts.
func (container *Container) IpcMounts() []Mount {
	return nil
}

// CreateSecretSymlinks creates symlinks to files in the secret mount.
func (container *Container) CreateSecretSymlinks() error {
	for _, r := range container.SecretReferences {
		if r.File == nil {
			continue
		}
		resolvedPath, _, err := container.ResolvePath(getSecretTargetPath(r))
		if err != nil {
			return err
		}
		if err := system.MkdirAll(filepath.Dir(resolvedPath), 0, ""); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(containerInternalSecretMountPath, r.SecretID), resolvedPath); err != nil {
			return err
		}
	}

	return nil
}

// SecretMounts returns the mount for the secret path.
// All secrets are stored in a single mount on Windows. Target symlinks are
// created for each secret, pointing to the files in this mount.
func (container *Container) SecretMounts() ([]Mount, error) {
	var mounts []Mount
	if len(container.SecretReferences) > 0 {
		src, err := container.SecretMountPath()
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{
			Source:      src,
			Destination: containerInternalSecretMountPath,
			Writable:    false,
		})
	}

	return mounts, nil
}

// UnmountSecrets unmounts the fs for secrets
func (container *Container) UnmountSecrets() error {
	p, err := container.SecretMountPath()
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

// CreateConfigSymlinks creates symlinks to files in the config mount.
func (container *Container) CreateConfigSymlinks() error {
	for _, configRef := range container.ConfigReferences {
		if configRef.File == nil {
			continue
		}
		resolvedPath, _, err := container.ResolvePath(configRef.File.Name)
		if err != nil {
			return err
		}
		if err := system.MkdirAll(filepath.Dir(resolvedPath), 0, ""); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(containerInternalConfigsDirPath, configRef.ConfigID), resolvedPath); err != nil {
			return err
		}
	}

	return nil
}

// ConfigMounts returns the mount for configs.
// TODO: Right now Windows doesn't really have a "secure" storage for secrets,
// however some configs may contain secrets. Once secure storage is worked out,
// configs and secret handling should be merged.
func (container *Container) ConfigMounts() []Mount {
	var mounts []Mount
	if len(container.ConfigReferences) > 0 {
		mounts = append(mounts, Mount{
			Source:      container.ConfigsDirPath(),
			Destination: containerInternalConfigsDirPath,
			Writable:    false,
		})
	}

	return mounts
}

// DetachAndUnmount unmounts all volumes.
// On Windows it only delegates to `UnmountVolumes` since there is nothing to
// force unmount.
func (container *Container) DetachAndUnmount(volumeEventLog func(name, action string, attributes map[string]string)) error {
	return container.UnmountVolumes(volumeEventLog)
}

// TmpfsMounts returns the list of tmpfs mounts
func (container *Container) TmpfsMounts() ([]Mount, error) {
	var mounts []Mount
	return mounts, nil
}

// UpdateContainer updates configuration of a container. Callers must hold a Lock on the Container.
func (container *Container) UpdateContainer(hostConfig *containertypes.HostConfig) error {
	resources := hostConfig.Resources
	if resources.CPUShares != 0 ||
		resources.Memory != 0 ||
		resources.NanoCPUs != 0 ||
		resources.CgroupParent != "" ||
		resources.BlkioWeight != 0 ||
		len(resources.BlkioWeightDevice) != 0 ||
		len(resources.BlkioDeviceReadBps) != 0 ||
		len(resources.BlkioDeviceWriteBps) != 0 ||
		len(resources.BlkioDeviceReadIOps) != 0 ||
		len(resources.BlkioDeviceWriteIOps) != 0 ||
		resources.CPUPeriod != 0 ||
		resources.CPUQuota != 0 ||
		resources.CPURealtimePeriod != 0 ||
		resources.CPURealtimeRuntime != 0 ||
		resources.CpusetCpus != "" ||
		resources.CpusetMems != "" ||
		len(resources.Devices) != 0 ||
		len(resources.DeviceCgroupRules) != 0 ||
		resources.KernelMemory != 0 ||
		resources.MemoryReservation != 0 ||
		resources.MemorySwap != 0 ||
		resources.MemorySwappiness != nil ||
		resources.OomKillDisable != nil ||
		(resources.PidsLimit != nil && *resources.PidsLimit != 0) ||
		len(resources.Ulimits) != 0 ||
		resources.CPUCount != 0 ||
		resources.CPUPercent != 0 ||
		resources.IOMaximumIOps != 0 ||
		resources.IOMaximumBandwidth != 0 {
		return fmt.Errorf("resource updating isn't supported on Windows")
	}
	// update HostConfig of container
	if hostConfig.RestartPolicy.Name != "" {
		if container.HostConfig.AutoRemove && !hostConfig.RestartPolicy.IsNone() {
			return fmt.Errorf("Restart policy cannot be updated because AutoRemove is enabled for the container")
		}
		container.HostConfig.RestartPolicy = hostConfig.RestartPolicy
	}
	return nil
}

// BuildHostnameFile writes the container's hostname file.
func (container *Container) BuildHostnameFile() error {
	return nil
}

// GetMountPoints gives a platform specific transformation to types.MountPoint. Callers must hold a Container lock.
func (container *Container) GetMountPoints() []types.MountPoint {
	mountPoints := make([]types.MountPoint, 0, len(container.MountPoints))
	for _, m := range container.MountPoints {
		mountPoints = append(mountPoints, types.MountPoint{
			Type:        m.Type,
			Name:        m.Name,
			Source:      m.Path(),
			Destination: m.Destination,
			Driver:      m.Driver,
			RW:          m.RW,
		})
	}
	return mountPoints
}

func (container *Container) ConfigsDirPath() string {
	return filepath.Join(container.Root, "configs")
}

// ConfigFilePath returns the path to the on-disk location of a config.
func (container *Container) ConfigFilePath(configRef swarmtypes.ConfigReference) string {
	return filepath.Join(container.ConfigsDirPath(), configRef.ConfigID)
}

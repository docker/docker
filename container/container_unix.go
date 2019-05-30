// +build !windows

package container // import "github.com/docker/docker/container"

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/continuity/fs"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/volume"
	volumemounts "github.com/docker/docker/volume/mounts"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	// DefaultStopTimeout sets the default time, in seconds, to wait
	// for the graceful container stop before forcefully terminating it.
	DefaultStopTimeout = 10

	containerSecretMountPath = "/run/secrets"
)

// TrySetNetworkMount attempts to set the network mounts given a provided destination and
// the path to use for it; return true if the given destination was a network mount file
func (container *Container) TrySetNetworkMount(destination string, path string) bool {
	if destination == "/etc/resolv.conf" {
		container.ResolvConfPath = path
		return true
	}
	if destination == "/etc/hostname" {
		container.HostnamePath = path
		return true
	}
	if destination == "/etc/hosts" {
		container.HostsPath = path
		return true
	}

	return false
}

// BuildHostnameFile writes the container's hostname file.
func (container *Container) BuildHostnameFile() error {
	hostnamePath, err := container.GetRootResourcePath("hostname")
	if err != nil {
		return err
	}
	container.HostnamePath = hostnamePath
	return ioutil.WriteFile(container.HostnamePath, []byte(container.Config.Hostname+"\n"), 0644)
}

// NetworkMounts returns the list of network mounts.
func (container *Container) NetworkMounts() []Mount {
	var mounts []Mount
	shared := container.HostConfig.NetworkMode.IsContainer()
	parser := volumemounts.NewParser(container.OS)
	if container.ResolvConfPath != "" {
		if _, err := os.Stat(container.ResolvConfPath); err != nil {
			logrus.Warnf("ResolvConfPath set to %q, but can't stat this filename (err = %v); skipping", container.ResolvConfPath, err)
		} else {
			writable := !container.HostConfig.ReadonlyRootfs
			if m, exists := container.MountPoints["/etc/resolv.conf"]; exists {
				writable = m.RW
			} else {
				label.Relabel(container.ResolvConfPath, container.MountLabel, shared)
			}
			mounts = append(mounts, Mount{
				Source:      container.ResolvConfPath,
				Destination: "/etc/resolv.conf",
				Writable:    writable,
				Propagation: string(parser.DefaultPropagationMode()),
			})
		}
	}
	if container.HostnamePath != "" {
		if _, err := os.Stat(container.HostnamePath); err != nil {
			logrus.Warnf("HostnamePath set to %q, but can't stat this filename (err = %v); skipping", container.HostnamePath, err)
		} else {
			writable := !container.HostConfig.ReadonlyRootfs
			if m, exists := container.MountPoints["/etc/hostname"]; exists {
				writable = m.RW
			} else {
				label.Relabel(container.HostnamePath, container.MountLabel, shared)
			}
			mounts = append(mounts, Mount{
				Source:      container.HostnamePath,
				Destination: "/etc/hostname",
				Writable:    writable,
				Propagation: string(parser.DefaultPropagationMode()),
			})
		}
	}
	if container.HostsPath != "" {
		if _, err := os.Stat(container.HostsPath); err != nil {
			logrus.Warnf("HostsPath set to %q, but can't stat this filename (err = %v); skipping", container.HostsPath, err)
		} else {
			writable := !container.HostConfig.ReadonlyRootfs
			if m, exists := container.MountPoints["/etc/hosts"]; exists {
				writable = m.RW
			} else {
				label.Relabel(container.HostsPath, container.MountLabel, shared)
			}
			mounts = append(mounts, Mount{
				Source:      container.HostsPath,
				Destination: "/etc/hosts",
				Writable:    writable,
				Propagation: string(parser.DefaultPropagationMode()),
			})
		}
	}
	return mounts
}

// CopyImagePathContent copies files in destination to the volume.
func (container *Container) CopyImagePathContent(v volume.Volume, destination string) error {
	rootfs, err := container.GetResourcePath(destination)
	if err != nil {
		return err
	}

	if _, err := os.Stat(rootfs); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	id := stringid.GenerateNonCryptoID()
	path, err := v.Mount(id)
	if err != nil {
		return err
	}

	defer func() {
		if err := v.Unmount(id); err != nil {
			logrus.Warnf("error while unmounting volume %s: %v", v.Name(), err)
		}
	}()
	if err := label.Relabel(path, container.MountLabel, true); err != nil && err != unix.ENOTSUP {
		return err
	}
	return copyExistingContents(rootfs, path)
}

// ShmResourcePath returns path to shm
func (container *Container) ShmResourcePath() (string, error) {
	return container.MountsResourcePath("shm")
}

// HasMountFor checks if path is a mountpoint
func (container *Container) HasMountFor(path string) bool {
	_, exists := container.MountPoints[path]
	if exists {
		return true
	}

	// Also search among the tmpfs mounts
	for dest := range container.HostConfig.Tmpfs {
		if dest == path {
			return true
		}
	}

	return false
}

// UnmountIpcMount unmounts shm if it was mounted
func (container *Container) UnmountIpcMount() error {
	if container.HasMountFor("/dev/shm") {
		return nil
	}

	// container.ShmPath should not be used here as it may point
	// to the host's or other container's /dev/shm
	shmPath, err := container.ShmResourcePath()
	if err != nil {
		return err
	}
	if shmPath == "" {
		return nil
	}
	if err = mount.Unmount(shmPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IpcMounts returns the list of IPC mounts
func (container *Container) IpcMounts() []Mount {
	var mounts []Mount
	parser := volumemounts.NewParser(container.OS)

	if container.HasMountFor("/dev/shm") {
		return mounts
	}
	if container.ShmPath == "" {
		return mounts
	}

	label.SetFileLabel(container.ShmPath, container.MountLabel)
	mounts = append(mounts, Mount{
		Source:      container.ShmPath,
		Destination: "/dev/shm",
		Writable:    true,
		Propagation: string(parser.DefaultPropagationMode()),
	})

	return mounts
}

// SecretMounts returns the mounts for the secret path.
func (container *Container) SecretMounts() ([]Mount, error) {
	var mounts []Mount
	for _, r := range container.SecretReferences {
		if r.File == nil {
			continue
		}
		src, err := container.SecretFilePath(*r)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{
			Source:      src,
			Destination: getSecretTargetPath(r),
			Writable:    false,
		})
	}
	for _, r := range container.ConfigReferences {
		fPath, err := container.ConfigFilePath(*r)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{
			Source:      fPath,
			Destination: r.File.Name,
			Writable:    false,
		})
	}

	return mounts, nil
}

// UnmountSecrets unmounts the local tmpfs for secrets
func (container *Container) UnmountSecrets() error {
	p, err := container.SecretMountPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return mount.RecursiveUnmount(p)
}

type conflictingUpdateOptions string

func (e conflictingUpdateOptions) Error() string {
	return string(e)
}

func (e conflictingUpdateOptions) Conflict() {}

// UpdateContainer updates configuration of a container. Callers must hold a Lock on the Container.
func (container *Container) UpdateContainer(hostConfig *containertypes.HostConfig) error {
	// update resources of container
	resources := hostConfig.Resources
	cResources := &container.HostConfig.Resources

	// validate NanoCPUs, CPUPeriod, and CPUQuota
	// Because NanoCPU effectively updates CPUPeriod/CPUQuota,
	// once NanoCPU is already set, updating CPUPeriod/CPUQuota will be blocked, and vice versa.
	// In the following we make sure the intended update (resources) does not conflict with the existing (cResource).
	if resources.NanoCPUs > 0 && cResources.CPUPeriod > 0 {
		return conflictingUpdateOptions("Conflicting options: Nano CPUs cannot be updated as CPU Period has already been set")
	}
	if resources.NanoCPUs > 0 && cResources.CPUQuota > 0 {
		return conflictingUpdateOptions("Conflicting options: Nano CPUs cannot be updated as CPU Quota has already been set")
	}
	if resources.CPUPeriod > 0 && cResources.NanoCPUs > 0 {
		return conflictingUpdateOptions("Conflicting options: CPU Period cannot be updated as NanoCPUs has already been set")
	}
	if resources.CPUQuota > 0 && cResources.NanoCPUs > 0 {
		return conflictingUpdateOptions("Conflicting options: CPU Quota cannot be updated as NanoCPUs has already been set")
	}

	if resources.BlkioWeight != 0 {
		cResources.BlkioWeight = resources.BlkioWeight
	}
	if resources.CPUShares != 0 {
		cResources.CPUShares = resources.CPUShares
	}
	if resources.NanoCPUs != 0 {
		cResources.NanoCPUs = resources.NanoCPUs
	}
	if resources.CPUPeriod != 0 {
		cResources.CPUPeriod = resources.CPUPeriod
	}
	if resources.CPUQuota != 0 {
		cResources.CPUQuota = resources.CPUQuota
	}
	if resources.CpusetCpus != "" {
		cResources.CpusetCpus = resources.CpusetCpus
	}
	if resources.CpusetMems != "" {
		cResources.CpusetMems = resources.CpusetMems
	}
	if resources.Memory != 0 {
		// if memory limit smaller than already set memoryswap limit and doesn't
		// update the memoryswap limit, then error out.
		if resources.Memory > cResources.MemorySwap && resources.MemorySwap == 0 {
			return conflictingUpdateOptions("Memory limit should be smaller than already set memoryswap limit, update the memoryswap at the same time")
		}
		cResources.Memory = resources.Memory
	}
	if resources.MemorySwap != 0 {
		cResources.MemorySwap = resources.MemorySwap
	}
	if resources.MemoryReservation != 0 {
		cResources.MemoryReservation = resources.MemoryReservation
	}
	if resources.KernelMemory != 0 {
		cResources.KernelMemory = resources.KernelMemory
	}
	if resources.CPURealtimePeriod != 0 {
		cResources.CPURealtimePeriod = resources.CPURealtimePeriod
	}
	if resources.CPURealtimeRuntime != 0 {
		cResources.CPURealtimeRuntime = resources.CPURealtimeRuntime
	}
	if resources.PidsLimit != nil {
		cResources.PidsLimit = resources.PidsLimit
	}

	// update HostConfig of container
	if hostConfig.RestartPolicy.Name != "" {
		if container.HostConfig.AutoRemove && !hostConfig.RestartPolicy.IsNone() {
			return conflictingUpdateOptions("Restart policy cannot be updated because AutoRemove is enabled for the container")
		}
		container.HostConfig.RestartPolicy = hostConfig.RestartPolicy
	}

	return nil
}

// DetachAndUnmount uses a detached mount on all mount destinations, then
// unmounts each volume normally.
// This is used from daemon/archive for `docker cp`
func (container *Container) DetachAndUnmount(volumeEventLog func(name, action string, attributes map[string]string)) error {
	networkMounts := container.NetworkMounts()
	mountPaths := make([]string, 0, len(container.MountPoints)+len(networkMounts))

	for _, mntPoint := range container.MountPoints {
		dest, err := container.GetResourcePath(mntPoint.Destination)
		if err != nil {
			logrus.Warnf("Failed to get volume destination path for container '%s' at '%s' while lazily unmounting: %v", container.ID, mntPoint.Destination, err)
			continue
		}
		mountPaths = append(mountPaths, dest)
	}

	for _, m := range networkMounts {
		dest, err := container.GetResourcePath(m.Destination)
		if err != nil {
			logrus.Warnf("Failed to get volume destination path for container '%s' at '%s' while lazily unmounting: %v", container.ID, m.Destination, err)
			continue
		}
		mountPaths = append(mountPaths, dest)
	}

	for _, mountPath := range mountPaths {
		if err := mount.Unmount(mountPath); err != nil {
			logrus.WithError(err).WithField("container", container.ID).
				Warn("Unable to unmount")
		}
	}
	return container.UnmountVolumes(volumeEventLog)
}

// ignoreUnsupportedXAttrs ignores errors when extended attributes
// are not supported
func ignoreUnsupportedXAttrs() fs.CopyDirOpt {
	xeh := func(dst, src, xattrKey string, err error) error {
		if errors.Cause(err) != syscall.ENOTSUP {
			return err
		}
		return nil
	}
	return fs.WithXAttrErrorHandler(xeh)
}

// copyExistingContents copies from the source to the destination and
// ensures the ownership is appropriately set.
func copyExistingContents(source, destination string) error {
	dstList, err := ioutil.ReadDir(destination)
	if err != nil {
		return err
	}
	if len(dstList) != 0 {
		// destination is not empty, do not copy
		return nil
	}
	return fs.CopyDir(destination, source, ignoreUnsupportedXAttrs())
}

// TmpfsMounts returns the list of tmpfs mounts
func (container *Container) TmpfsMounts() ([]Mount, error) {
	parser := volumemounts.NewParser(container.OS)
	var mounts []Mount
	for dest, data := range container.HostConfig.Tmpfs {
		mounts = append(mounts, Mount{
			Source:      "tmpfs",
			Destination: dest,
			Data:        data,
		})
	}
	for dest, mnt := range container.MountPoints {
		if mnt.Type == mounttypes.TypeTmpfs {
			data, err := parser.ConvertTmpfsOptions(mnt.Spec.TmpfsOptions, mnt.Spec.ReadOnly)
			if err != nil {
				return nil, err
			}
			mounts = append(mounts, Mount{
				Source:      "tmpfs",
				Destination: dest,
				Data:        data,
			})
		}
	}
	return mounts, nil
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
			Mode:        m.Mode,
			RW:          m.RW,
			Propagation: m.Propagation,
		})
	}
	return mountPoints
}

// ConfigFilePath returns the path to the on-disk location of a config.
// On unix, configs are always considered secret
func (container *Container) ConfigFilePath(configRef swarmtypes.ConfigReference) (string, error) {
	mounts, err := container.SecretMountPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(mounts, configRef.ConfigID), nil
}

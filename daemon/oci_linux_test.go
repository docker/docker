package daemon

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/rootless"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/network"
	daemonoci "github.com/moby/moby/v2/daemon/pkg/oci"
	"github.com/moby/sys/user"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func setupFakeDaemon(t *testing.T, c *container.Container) *Daemon {
	t.Helper()
	root := t.TempDir()

	rootfs := filepath.Join(root, "rootfs")
	err := os.MkdirAll(rootfs, 0o755)
	assert.NilError(t, err)

	netController, err := libnetwork.New(t.Context())
	assert.NilError(t, err)

	d := &Daemon{
		// some empty structs to avoid getting a panic
		// caused by a null pointer dereference
		linkIndex:     newLinkIndex(),
		netController: netController,
		imageService:  &fakeImageService{},
	}

	c.Root = root
	c.BaseFS = rootfs

	if c.Config == nil {
		c.Config = new(containertypes.Config)
	}
	if c.HostConfig == nil {
		c.HostConfig = new(containertypes.HostConfig)
	}
	if c.NetworkSettings == nil {
		c.NetworkSettings = &network.Settings{Networks: make(map[string]*network.EndpointSettings)}
	}

	// HORRIBLE HACK: clean up shm mounts leaked by some tests. Otherwise the
	// offending tests would fail due to the mounts blocking the temporary
	// directory from being cleaned up.
	t.Cleanup(func() {
		if c.ShmPath != "" {
			var err error
			for err == nil { // Some tests over-mount over the same path multiple times.
				err = unix.Unmount(c.ShmPath, unix.MNT_DETACH)
			}
		}
	})

	return d
}

type fakeImageService struct {
	ImageService
}

func (i *fakeImageService) StorageDriver() string {
	return "overlay"
}

func TestWithUmask(t *testing.T) {
	t.Run("omitted", func(t *testing.T) {
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		s := daemonoci.DefaultSpec()

		err := WithUmask(c)(t.Context(), nil, nil, &s)
		assert.NilError(t, err)
		assert.Assert(t, s.Process.User.Umask == nil)
	})

	t.Run("set", func(t *testing.T) {
		umask := uint32(0o027)
		c := &container.Container{HostConfig: &containertypes.HostConfig{Umask: &umask}}
		s := daemonoci.DefaultSpec()

		err := WithUmask(c)(t.Context(), nil, nil, &s)
		assert.NilError(t, err)
		assert.Assert(t, s.Process.User.Umask != nil)
		assert.Equal(t, *s.Process.User.Umask, umask)
	})
}

func TestWithCommonOptionsDockerInit(t *testing.T) {
	initPath := filepath.Join(t.TempDir(), "docker-init")
	err := os.WriteFile(initPath, []byte("#!/bin/sh\n"), 0o755)
	assert.NilError(t, err)

	initEnabled := true
	initDisabled := false
	workloadArgs := []string{"/usr/local/bin/workload", "--entrypoint-option", "cmd-arg-1", "cmd-arg-2"}
	wrappedArgs := append([]string{inContainerInitPath, "--"}, workloadArgs...)

	tests := []struct {
		name          string
		containerInit *bool
		daemonInit    bool
		pidMode       containertypes.PidMode
		wantArgs      []string
		wantInitMount bool
	}{
		{
			name:          "container init enabled",
			containerInit: &initEnabled,
			wantArgs:      wrappedArgs,
			wantInitMount: true,
		},
		{
			name:          "daemon default init enabled",
			daemonInit:    true,
			wantArgs:      wrappedArgs,
			wantInitMount: true,
		},
		{
			name:          "container init explicitly disabled",
			containerInit: &initDisabled,
			daemonInit:    true,
			wantArgs:      workloadArgs,
		},
		{
			name:          "host PID namespace",
			containerInit: &initEnabled,
			pidMode:       containertypes.PidMode("host"),
			wantArgs:      workloadArgs,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &container.Container{
				BaseFS: t.TempDir(),
				Path:   workloadArgs[0],
				Args:   workloadArgs[1:],
				Config: &containertypes.Config{
					Entrypoint: workloadArgs[:2],
					Cmd:        workloadArgs[2:],
				},
				HostConfig: &containertypes.HostConfig{
					Init:    tc.containerInit,
					PidMode: tc.pidMode,
				},
				NetworkSettings: &network.Settings{Networks: make(map[string]*network.EndpointSettings)},
			}
			d := &Daemon{linkIndex: newLinkIndex()}
			daemonCfg := config.Config{Init: tc.daemonInit, InitPath: initPath}
			s := daemonoci.DefaultSpec()

			err := withCommonOptions(d, &daemonCfg, c)(t.Context(), nil, nil, &s)
			assert.NilError(t, err)
			assert.Check(t, is.DeepEqual(s.Process.Args, tc.wantArgs))

			var initMounts []specs.Mount
			for _, m := range s.Mounts {
				if m.Destination == inContainerInitPath {
					initMounts = append(initMounts, m)
				}
			}
			if !tc.wantInitMount {
				assert.Equal(t, len(initMounts), 0)
				return
			}
			assert.Check(t, is.DeepEqual(initMounts, []specs.Mount{{
				Destination: inContainerInitPath,
				Type:        "bind",
				Source:      initPath,
				Options:     []string{"bind", "ro"},
			}}))
		})
	}
}

func TestCreateSpecPreservesCDIAdditionalGIDs(t *testing.T) {
	cdiDir := t.TempDir()
	err := os.WriteFile(filepath.Join(cdiDir, "test-device.yaml"), []byte(`
cdiVersion: "0.7.0"
kind: "example.com/device"
devices:
- name: foo
  containerEdits:
    additionalGids:
    - 1234
`), 0o644)
	assert.NilError(t, err)

	origDeviceDrivers := maps.Clone(deviceDrivers)
	t.Cleanup(func() {
		deviceDrivers = origDeviceDrivers
	})
	RegisterCDIDriver(cdiDir)

	c := &container.Container{
		Config: &containertypes.Config{},
		HostConfig: &containertypes.HostConfig{
			Resources: containertypes.Resources{
				DeviceRequests: []containertypes.DeviceRequest{
					{
						Driver:    "cdi",
						DeviceIDs: []string{"example.com/device=foo"},
					},
				},
			},
		},
	}
	d := setupFakeDaemon(t, c)

	s, err := d.createSpec(t.Context(), &configStore{}, c, nil)
	assert.NilError(t, err)
	assert.Assert(t, slices.Contains(s.Process.User.AdditionalGids, uint32(1234)), "CDI additional GID not present in OCI spec")
}

// TestTmpfsDevShmNoDupMount checks that a user-specified /dev/shm tmpfs
// mount (as in "docker run --tmpfs /dev/shm:rw,size=NNN") does not result
// in "Duplicate mount point" error from the engine.
// https://github.com/moby/moby/issues/35455
func TestTmpfsDevShmNoDupMount(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	c := &container.Container{
		ShmPath: "foobar", // non-empty, for c.IpcMounts() to work
		HostConfig: &containertypes.HostConfig{
			IpcMode: containertypes.IPCModeShareable, // default mode
			// --tmpfs /dev/shm:rw,exec,size=NNN
			Tmpfs: map[string]string{
				"/dev/shm": "rw,exec,size=1g",
			},
		},
	}
	d := setupFakeDaemon(t, c)

	_, err := d.createSpec(t.Context(), &configStore{}, c, nil)
	assert.Check(t, err)
}

// TestIpcPrivateVsReadonly checks that in case of IpcMode: private
// and ReadonlyRootfs: true (as in "docker run --ipc private --read-only")
// the resulting /dev/shm mount is NOT made read-only.
// https://github.com/moby/moby/issues/36503
func TestIpcPrivateVsReadonly(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	c := &container.Container{
		HostConfig: &containertypes.HostConfig{
			IpcMode:        containertypes.IPCModePrivate,
			ReadonlyRootfs: true,
		},
	}
	d := setupFakeDaemon(t, c)

	s, err := d.createSpec(t.Context(), &configStore{}, c, nil)
	assert.Check(t, err)

	// Find the /dev/shm mount in ms, check it does not have ro
	for _, m := range s.Mounts {
		if m.Destination != "/dev/shm" {
			continue
		}
		assert.Check(t, is.Equal(false, slices.Contains(m.Options, "ro")))
	}
}

// TestSysctlOverride ensures that any implicit sysctls (such as
// Config.Domainname) are overridden by an explicit sysctl in the HostConfig.
func TestSysctlOverride(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	ctx := t.Context()
	c := &container.Container{
		Config: &containertypes.Config{
			Hostname:   "foobar",
			Domainname: "baz.cyphar.com",
		},
		HostConfig: &containertypes.HostConfig{
			NetworkMode: "bridge",
			Sysctls:     map[string]string{},
		},
	}
	d := setupFakeDaemon(t, c)

	// Ensure that the domainname is set, and the sysctl is not.
	s, err := d.createSpec(ctx, &configStore{}, c, nil)
	assert.NilError(t, err)
	assert.Equal(t, s.Hostname, "foobar")
	assert.Equal(t, s.Domainname, c.Config.Domainname)
	_, hasDomainnameSysctl := s.Linux.Sysctl["kernel.domainname"]
	assert.Check(t, !hasDomainnameSysctl)
	if sysctlExists("net.ipv4.ip_unprivileged_port_start") {
		assert.Equal(t, s.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"], "0")
	}
	if sysctlExists("net.ipv4.ping_group_range") {
		assert.Equal(t, s.Linux.Sysctl["net.ipv4.ping_group_range"], "0 2147483647")
	}

	// Set an explicit sysctl.
	c.HostConfig.Sysctls["kernel.domainname"] = "foobar.net"
	assert.Assert(t, c.HostConfig.Sysctls["kernel.domainname"] != c.Config.Domainname)
	c.HostConfig.Sysctls["net.ipv4.ip_unprivileged_port_start"] = "1024"

	s, err = d.createSpec(ctx, &configStore{}, c, nil)
	assert.NilError(t, err)
	assert.Equal(t, s.Hostname, "foobar")
	assert.Equal(t, s.Linux.Sysctl["kernel.domainname"], c.HostConfig.Sysctls["kernel.domainname"])
	assert.Equal(t, s.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"], c.HostConfig.Sysctls["net.ipv4.ip_unprivileged_port_start"])

	// Ensure the ping_group_range is not set on a daemon with user-namespaces enabled
	s, err = d.createSpec(ctx, &configStore{Config: config.Config{RemappedRoot: "dummy:dummy"}}, c, nil)
	assert.NilError(t, err)
	_, ok := s.Linux.Sysctl["net.ipv4.ping_group_range"]
	assert.Assert(t, !ok)

	// Ensure the ping_group_range is set on a container in "host" userns mode
	// on a daemon with user-namespaces enabled
	c.HostConfig.UsernsMode = "host"
	s, err = d.createSpec(ctx, &configStore{Config: config.Config{RemappedRoot: "dummy:dummy"}}, c, nil)
	assert.NilError(t, err)
	assert.Equal(t, s.Linux.Sysctl["net.ipv4.ping_group_range"], "0 2147483647")
}

// TestSysctlOverrideHost ensures that any implicit network sysctls are not set
// with host networking
func TestSysctlOverrideHost(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	ctx := t.Context()
	c := &container.Container{
		Config: &containertypes.Config{},
		HostConfig: &containertypes.HostConfig{
			NetworkMode: "host",
			Sysctls:     map[string]string{},
		},
	}
	d := setupFakeDaemon(t, c)

	// Ensure that the implicit sysctl is not set
	s, err := d.createSpec(ctx, &configStore{}, c, nil)
	assert.NilError(t, err)
	assert.Equal(t, s.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"], "")
	assert.Equal(t, s.Linux.Sysctl["net.ipv4.ping_group_range"], "")

	// Set an explicit sysctl.
	c.HostConfig.Sysctls["net.ipv4.ip_unprivileged_port_start"] = "1024"

	s, err = d.createSpec(ctx, &configStore{}, c, nil)
	assert.NilError(t, err)
	assert.Equal(t, s.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"], c.HostConfig.Sysctls["net.ipv4.ip_unprivileged_port_start"])
}

func TestGetSourceMount(t *testing.T) {
	// must be able to find source mount for /
	mnt, _, err := getSourceMount("/")
	assert.NilError(t, err)
	assert.Equal(t, mnt, "/")

	// must be able to find source mount for current directory
	cwd, err := os.Getwd()
	assert.NilError(t, err)
	_, _, err = getSourceMount(cwd)
	assert.NilError(t, err)
}

func TestDefaultResources(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root") // TODO: is this actually true? I'm guilty of following the cargo cult here.

	c := &container.Container{
		HostConfig: &containertypes.HostConfig{
			IpcMode: containertypes.IPCModeNone,
		},
	}
	d := setupFakeDaemon(t, c)

	s, err := d.createSpec(t.Context(), &configStore{}, c, nil)
	assert.NilError(t, err)
	checkResourcesAreUnset(t, s.Linux.Resources)
}

func checkResourcesAreUnset(t *testing.T, r *specs.LinuxResources) {
	t.Helper()

	if r != nil {
		if r.Memory != nil {
			assert.Check(t, is.DeepEqual(r.Memory, &specs.LinuxMemory{}))
		}
		if r.CPU != nil {
			assert.Check(t, is.DeepEqual(r.CPU, &specs.LinuxCPU{}))
		}
		assert.Check(t, is.Nil(r.Pids))
		if r.BlockIO != nil {
			assert.Check(t, is.DeepEqual(r.BlockIO, &specs.LinuxBlockIO{}, cmpopts.EquateEmpty()))
		}
		if r.Network != nil {
			assert.Check(t, is.DeepEqual(r.Network, &specs.LinuxNetwork{}, cmpopts.EquateEmpty()))
		}
	}
}

func TestMountIDMappings(t *testing.T) {
	skip.If(t, rootless.RunningWithRootlessKit(), "id-mapped mounts are rejected on rootless daemons")

	newSpec := func(uid, gid uint32) *specs.Spec {
		return &specs.Spec{Process: &specs.Process{User: specs.User{UID: uid, GID: gid}}}
	}
	// The mount source's ownership is read with a stat: use a directory
	// owned by the test runner and expect its actual IDs in the mappings.
	src := t.TempDir()
	srcUID := uint32(os.Getuid())
	srcGID := uint32(os.Getgid())

	t.Run("match-user with the container user", func(t *testing.T) {
		d := &Daemon{}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		uidMaps, gidMaps, err := d.mountIDMappings(c, newSpec(1001, 1002), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceMatchUser})
		assert.NilError(t, err)
		assert.Check(t, is.DeepEqual(uidMaps, []specs.LinuxIDMapping{{ContainerID: srcUID, HostID: 1001, Size: 1}}))
		assert.Check(t, is.DeepEqual(gidMaps, []specs.LinuxIDMapping{{ContainerID: srcGID, HostID: 1002, Size: 1}}))
	})

	t.Run("match-user with a numeric user", func(t *testing.T) {
		d := &Daemon{}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		c.Root = t.TempDir()
		c.BaseFS = c.Root
		uidMaps, gidMaps, err := d.mountIDMappings(c, newSpec(0, 0), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceMatchUser, User: "1234:5678"})
		assert.NilError(t, err)
		assert.Check(t, is.DeepEqual(uidMaps, []specs.LinuxIDMapping{{ContainerID: srcUID, HostID: 1234, Size: 1}}))
		assert.Check(t, is.DeepEqual(gidMaps, []specs.LinuxIDMapping{{ContainerID: srcGID, HostID: 5678, Size: 1}}))
	})

	t.Run("match-user with userns-remap", func(t *testing.T) {
		d := &Daemon{
			idMapping: user.IdentityMapping{
				UIDMaps: []user.IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
				GIDMaps: []user.IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
			},
		}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		uidMaps, gidMaps, err := d.mountIDMappings(c, newSpec(33, 33), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceMatchUser})
		assert.NilError(t, err)
		// The mount mapping targets the host-side representation of the
		// container user, which the user namespace maps back to it.
		assert.Check(t, is.DeepEqual(uidMaps, []specs.LinuxIDMapping{{ContainerID: srcUID, HostID: 100033, Size: 1}}))
		assert.Check(t, is.DeepEqual(gidMaps, []specs.LinuxIDMapping{{ContainerID: srcGID, HostID: 200033, Size: 1}}))
	})

	t.Run("match-user with a missing source", func(t *testing.T) {
		d := &Daemon{}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		_, _, err := d.mountIDMappings(c, newSpec(0, 0), filepath.Join(src, "missing"), &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceMatchUser})
		assert.ErrorContains(t, err, "cannot stat mount source")
	})

	t.Run("userns source without user namespace", func(t *testing.T) {
		d := &Daemon{}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		_, _, err := d.mountIDMappings(c, newSpec(0, 0), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceUserns})
		assert.ErrorContains(t, err, "private user namespace")
	})

	t.Run("userns source with userns-remap", func(t *testing.T) {
		d := &Daemon{
			idMapping: user.IdentityMapping{
				UIDMaps: []user.IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
				GIDMaps: []user.IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
			},
		}
		c := &container.Container{HostConfig: &containertypes.HostConfig{}}
		uidMaps, gidMaps, err := d.mountIDMappings(c, newSpec(0, 0), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceUserns})
		assert.NilError(t, err)
		assert.Check(t, is.DeepEqual(uidMaps, []specs.LinuxIDMapping{{ContainerID: 0, HostID: 100000, Size: 65536}}))
		assert.Check(t, is.DeepEqual(gidMaps, []specs.LinuxIDMapping{{ContainerID: 0, HostID: 200000, Size: 65536}}))
	})

	t.Run("userns source with userns host mode", func(t *testing.T) {
		d := &Daemon{
			idMapping: user.IdentityMapping{
				UIDMaps: []user.IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
				GIDMaps: []user.IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
			},
		}
		c := &container.Container{HostConfig: &containertypes.HostConfig{UsernsMode: "host"}}
		_, _, err := d.mountIDMappings(c, newSpec(0, 0), src, &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceUserns})
		assert.ErrorContains(t, err, "private user namespace")
	})
}

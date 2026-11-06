package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdcgroups "github.com/containerd/cgroups/v3"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestRootlessSystemdCgroupDiscovery(t *testing.T) {
	tests := []struct {
		name        string
		processPath string
		managerPath string
	}{
		{
			name:        "conventional",
			processPath: "/user.slice/user-1000.slice/user@1000.service/app.slice/docker.service",
			managerPath: "/user.slice/user-1000.slice/user@1000.service",
		},
		{
			name:        "prefixed",
			processPath: "/outer/prefix/user.slice/user-1000.slice/user@1000.service/app.slice/docker.service",
			managerPath: "/outer/prefix/user.slice/user-1000.slice/user@1000.service",
		},
		{
			name:        "generic WSL-shaped prefix",
			processPath: "/wsl-user/distro-280/systemd/user.slice/user-1000.slice/user@1000.service/app.slice/docker.service",
			managerPath: "/wsl-user/distro-280/systemd/user.slice/user-1000.slice/user@1000.service",
		},
		{
			name:        "daemon outside user manager",
			processPath: "/system.slice/docker-entrypoint.service",
			managerPath: "/user.slice/user-1000.slice/user@1000.service",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cgroupRoot := t.TempDir()
			groupDir := filepath.Join(cgroupRoot, strings.TrimPrefix(tc.managerPath, "/"))
			assert.NilError(t, os.MkdirAll(groupDir, 0o755))
			assert.NilError(t, os.WriteFile(filepath.Join(groupDir, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o600))
			processDir := filepath.Join(cgroupRoot, strings.TrimPrefix(tc.processPath, "/"))
			assert.NilError(t, os.MkdirAll(processDir, 0o755))
			assert.NilError(t, os.WriteFile(filepath.Join(processDir, "cgroup.controllers"), []byte("io\n"), 0o600))

			info, err := newRootlessSystemdCgroupFixture(tc.managerPath, nil, cgroupRoot).discover()
			assert.NilError(t, err)
			assert.Equal(t, info.groupPath, tc.managerPath)
			assert.DeepEqual(t, info.controllers, []string{"cpu", "memory", "pids"})
		})
	}
}

func TestRootlessSystemdCgroupDiscoveryErrors(t *testing.T) {
	tests := []struct {
		name        string
		managerPath string
		lookupErr   error
		expected    string
	}{
		{
			name:      "manager lookup error",
			lookupErr: errors.New("test D-Bus failure"),
			expected:  "resolving systemd user manager cgroup",
		},
		{
			name:        "empty path",
			managerPath: "",
			expected:    "empty control-group path",
		},
		{
			name:        "unclean path",
			managerPath: "/outer/../user.slice",
			expected:    "invalid systemd user manager cgroup path",
		},
		{
			name:        "relative path",
			managerPath: "user.slice/user-1000.slice",
			expected:    "invalid systemd user manager cgroup path",
		},
		{
			name:        "deleted cgroup",
			managerPath: "/user.slice/user@1000.service (deleted)",
			expected:    "has been deleted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cgroupRoot := t.TempDir()
			_, err := newRootlessSystemdCgroupFixture(tc.managerPath, tc.lookupErr, cgroupRoot).discover()
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	t.Run("missing controllers does not use conventional fallback", func(t *testing.T) {
		groupPath := "/outer/prefix/user.slice/user-1000.slice/user@1000.service"
		cgroupRoot := t.TempDir()
		_, err := newRootlessSystemdCgroupFixture(groupPath, nil, cgroupRoot).discover()
		assert.ErrorContains(t, err, groupPath)
		assert.ErrorContains(t, err, "cgroup.controllers")
	})

	t.Run("symlink cannot escape cgroup mount", func(t *testing.T) {
		base := t.TempDir()
		cgroupRoot := filepath.Join(base, "cgroup")
		outside := filepath.Join(base, "outside")
		assert.NilError(t, os.Mkdir(cgroupRoot, 0o755))
		assert.NilError(t, os.Mkdir(outside, 0o755))
		assert.NilError(t, os.WriteFile(filepath.Join(outside, "cgroup.controllers"), []byte("cpu\n"), 0o600))
		assert.NilError(t, os.Symlink(outside, filepath.Join(cgroupRoot, "escape")))

		_, err := newRootlessSystemdCgroupFixture("/escape", nil, cgroupRoot).discover()
		assert.ErrorContains(t, err, "reading controllers")
	})
}

func TestRootlessSystemdCgroupCallSites(t *testing.T) {
	t.Setenv("ROOTLESSKIT_PARENT_EUID", "1000")

	originalMode := rootlessSystemdCgroupMode
	originalDiscover := discoverRootlessSystemdCgroupInfo
	originalNewSysInfo := newSysInfo
	originalSysInfoOpt := withCgroup2ControllersSysInfo
	t.Cleanup(func() {
		rootlessSystemdCgroupMode = originalMode
		discoverRootlessSystemdCgroupInfo = originalDiscover
		newSysInfo = originalNewSysInfo
		withCgroup2ControllersSysInfo = originalSysInfoOpt
	})
	rootlessSystemdCgroupMode = func() cdcgroups.CGMode { return cdcgroups.Unified }

	const groupPath = "/outer/prefix/user.slice/user-1000.slice/user@1000.service"
	discoveryCalls := 0
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		discoveryCalls++
		return rootlessSystemdCgroupInfo{
			groupPath:   groupPath,
			controllers: []string{"cpu", "memory", "pids"},
		}, nil
	}
	var sysInfoGroupPath string
	var sysInfoControllers []string
	withCgroup2ControllersSysInfo = func(groupPath string, controllers []string) sysinfo.Opt {
		sysInfoGroupPath = groupPath
		sysInfoControllers = append([]string(nil), controllers...)
		return func(*sysinfo.SysInfo) {}
	}
	newSysInfo = func(opts ...sysinfo.Opt) *sysinfo.SysInfo {
		assert.Equal(t, len(opts), 1)
		return &sysinfo.SysInfo{}
	}

	cfg := rootlessSystemdConfig()
	s := &specs.Spec{Linux: &specs.Linux{CgroupsPath: "user.slice:docker:test"}}
	err := withRootless(nil, cfg)(context.Background(), nil, nil, s)
	assert.NilError(t, err)
	assert.Equal(t, s.Linux.CgroupsPath, "user.slice:docker:test")

	_ = getSysInfo(cfg)
	assert.Equal(t, discoveryCalls, 2, "OCI conversion and SysInfo must use the shared discovery helper")
	assert.Equal(t, sysInfoGroupPath, groupPath)
	assert.DeepEqual(t, sysInfoControllers, []string{"cpu", "memory", "pids"})
}

func TestRootlessSystemdCgroupDiscoveryErrorsAreConservativeInSysInfo(t *testing.T) {
	t.Setenv("ROOTLESSKIT_PARENT_EUID", "1000")

	originalMode := rootlessSystemdCgroupMode
	originalDiscover := discoverRootlessSystemdCgroupInfo
	originalNewSysInfo := newSysInfo
	t.Cleanup(func() {
		rootlessSystemdCgroupMode = originalMode
		discoverRootlessSystemdCgroupInfo = originalDiscover
		newSysInfo = originalNewSysInfo
	})
	rootlessSystemdCgroupMode = func() cdcgroups.CGMode { return cdcgroups.Unified }
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		return rootlessSystemdCgroupInfo{}, errors.New("test discovery failure")
	}

	cfg := rootlessSystemdConfig()
	s := &specs.Spec{Linux: &specs.Linux{CgroupsPath: "user.slice:docker:test"}}
	err := withRootless(nil, cfg)(context.Background(), nil, nil, s)
	assert.ErrorContains(t, err, "test discovery failure")

	newSysInfo = func(opts ...sysinfo.Opt) *sysinfo.SysInfo {
		assert.Equal(t, len(opts), 1, "SysInfo must receive an explicit empty controller set instead of probing the cgroup mount root")
		return &sysinfo.SysInfo{}
	}
	si := getSysInfo(cfg)
	assert.Assert(t, is.Contains(strings.Join(si.Warnings, "\n"), "test discovery failure"))
}

func TestWithRootlessSystemdFiltersControllers(t *testing.T) {
	t.Setenv("ROOTLESSKIT_PARENT_EUID", "1000")

	originalMode := rootlessSystemdCgroupMode
	originalDiscover := discoverRootlessSystemdCgroupInfo
	t.Cleanup(func() {
		rootlessSystemdCgroupMode = originalMode
		discoverRootlessSystemdCgroupInfo = originalDiscover
	})
	rootlessSystemdCgroupMode = func() cdcgroups.CGMode { return cdcgroups.Unified }
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		return rootlessSystemdCgroupInfo{
			groupPath:   "/user.slice/user-1000.slice/user@1000.service",
			controllers: []string{"cpu", "memory", "pids"},
		}, nil
	}

	memoryLimit := int64(64 * 1024 * 1024)
	cpuQuota := int64(50_000)
	pidsLimit := int64(100)
	weight := uint16(500)
	s := &specs.Spec{Linux: &specs.Linux{
		CgroupsPath: "user.slice:docker:test",
		Resources: &specs.LinuxResources{
			Memory:  &specs.LinuxMemory{Limit: &memoryLimit},
			CPU:     &specs.LinuxCPU{Quota: &cpuQuota, Cpus: "0"},
			Pids:    &specs.LinuxPids{Limit: &pidsLimit},
			BlockIO: &specs.LinuxBlockIO{Weight: &weight},
		},
	}}

	err := withRootless(nil, rootlessSystemdConfig())(context.Background(), nil, nil, s)
	assert.NilError(t, err)
	assert.Assert(t, s.Linux.Resources.Memory != nil, "memory controller was removed")
	assert.Assert(t, s.Linux.Resources.CPU != nil, "cpu controller was removed")
	assert.Assert(t, s.Linux.Resources.Pids != nil, "pids controller was removed")
	assert.Equal(t, s.Linux.Resources.CPU.Cpus, "", "cpuset settings must be removed when cpuset is unavailable")
	assert.Check(t, is.Nil(s.Linux.Resources.BlockIO), "block I/O settings must be removed when io is unavailable")
}

func TestWithRootlessSystemdWithoutDelegatedControllers(t *testing.T) {
	t.Setenv("ROOTLESSKIT_PARENT_EUID", "1000")

	originalMode := rootlessSystemdCgroupMode
	originalDiscover := discoverRootlessSystemdCgroupInfo
	t.Cleanup(func() {
		rootlessSystemdCgroupMode = originalMode
		discoverRootlessSystemdCgroupInfo = originalDiscover
	})
	rootlessSystemdCgroupMode = func() cdcgroups.CGMode { return cdcgroups.Unified }
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		return rootlessSystemdCgroupInfo{
			groupPath:   "/user.slice/user-1000.slice/user@1000.service",
			controllers: []string{},
		}, nil
	}

	s := &specs.Spec{Linux: &specs.Linux{
		CgroupsPath: "user.slice:docker:test",
		Resources:   &specs.LinuxResources{},
	}}
	err := withRootless(nil, rootlessSystemdConfig())(context.Background(), nil, nil, s)
	assert.NilError(t, err)
	assert.Equal(t, s.Linux.CgroupsPath, "")
	assert.Check(t, is.Nil(s.Linux.Resources))
}

func TestRootlessSystemdEnvironmentValidation(t *testing.T) {
	originalMode := rootlessSystemdCgroupMode
	originalDiscover := discoverRootlessSystemdCgroupInfo
	t.Cleanup(func() {
		rootlessSystemdCgroupMode = originalMode
		discoverRootlessSystemdCgroupInfo = originalDiscover
	})
	rootlessSystemdCgroupMode = func() cdcgroups.CGMode { return cdcgroups.Unified }
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		t.Fatal("discovery must not run with an invalid RootlessKit environment")
		return rootlessSystemdCgroupInfo{}, nil
	}

	t.Run("missing", func(t *testing.T) {
		t.Setenv("ROOTLESSKIT_PARENT_EUID", "")
		_, err := rootlessSystemdCgroupControllerInfo()
		assert.ErrorContains(t, err, "ROOTLESSKIT_PARENT_EUID is not set")
	})
	t.Run("malformed", func(t *testing.T) {
		t.Setenv("ROOTLESSKIT_PARENT_EUID", "not-a-uid")
		_, err := rootlessSystemdCgroupControllerInfo()
		assert.ErrorContains(t, err, "invalid $ROOTLESSKIT_PARENT_EUID")

		si := getSysInfo(rootlessSystemdConfig())
		assert.Assert(t, is.Contains(strings.Join(si.Warnings, "\n"), "invalid $ROOTLESSKIT_PARENT_EUID"))
	})
}

func TestRootfulAndCgroupfsSkipRootlessSystemdDiscovery(t *testing.T) {
	originalDiscover := discoverRootlessSystemdCgroupInfo
	originalNewSysInfo := newSysInfo
	t.Cleanup(func() {
		discoverRootlessSystemdCgroupInfo = originalDiscover
		newSysInfo = originalNewSysInfo
	})
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		t.Fatal("rootless systemd discovery must not run")
		return rootlessSystemdCgroupInfo{}, nil
	}
	newSysInfo = func(...sysinfo.Opt) *sysinfo.SysInfo {
		return &sysinfo.SysInfo{}
	}

	rootful := &config.Config{
		CommonConfig: config.CommonConfig{ExecOptions: []string{"native.cgroupdriver=systemd"}},
	}
	_ = getSysInfo(rootful)

	rootlessCgroupfs := &config.Config{
		CommonConfig: config.CommonConfig{ExecOptions: []string{"native.cgroupdriver=cgroupfs"}},
		Rootless:     true,
	}
	_ = getSysInfo(rootlessCgroupfs)
}

func newRootlessSystemdCgroupFixture(managerPath string, lookupErr error, cgroupRoot string) rootlessSystemdCgroupFiles {
	return rootlessSystemdCgroupFiles{
		userManagerControlGroup: func() (string, error) {
			return managerPath, lookupErr
		},
		cgroup2Root: cgroupRoot,
		readCgroupFile: func(dir, file string) (string, error) {
			relDir, err := filepath.Rel(cgroupRoot, dir)
			if err != nil {
				return "", err
			}
			root, err := os.OpenRoot(cgroupRoot)
			if err != nil {
				return "", err
			}
			defer root.Close()
			contents, err := root.ReadFile(filepath.Join(relDir, file))
			return string(contents), err
		},
	}
}

func rootlessSystemdConfig() *config.Config {
	return &config.Config{
		CommonConfig: config.CommonConfig{ExecOptions: []string{"native.cgroupdriver=systemd"}},
		Rootless:     true,
	}
}

// +build !windows

package daemon // import "github.com/docker/docker/daemon"

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/blkiodev"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/pkg/sysinfo"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

type fakeContainerGetter struct {
	containers map[string]*container.Container
}

func (f *fakeContainerGetter) GetContainer(cid string) (*container.Container, error) {
	container, ok := f.containers[cid]
	if !ok {
		return nil, errors.New("container not found")
	}
	return container, nil
}

// Unix test as uses settings which are not available on Windows
func TestAdjustSharedNamespaceContainerName(t *testing.T) {
	fakeID := "abcdef1234567890"
	hostConfig := &containertypes.HostConfig{
		IpcMode:     containertypes.IpcMode("container:base"),
		PidMode:     containertypes.PidMode("container:base"),
		NetworkMode: containertypes.NetworkMode("container:base"),
	}
	containerStore := &fakeContainerGetter{}
	containerStore.containers = make(map[string]*container.Container)
	containerStore.containers["base"] = &container.Container{
		ID: fakeID,
	}

	adaptSharedNamespaceContainer(containerStore, hostConfig)
	if hostConfig.IpcMode != containertypes.IpcMode("container:"+fakeID) {
		t.Errorf("Expected IpcMode to be container:%s", fakeID)
	}
	if hostConfig.PidMode != containertypes.PidMode("container:"+fakeID) {
		t.Errorf("Expected PidMode to be container:%s", fakeID)
	}
	if hostConfig.NetworkMode != containertypes.NetworkMode("container:"+fakeID) {
		t.Errorf("Expected NetworkMode to be container:%s", fakeID)
	}
}

// Unix test as uses settings which are not available on Windows
func TestAdjustCPUShares(t *testing.T) {
	tmp, err := ioutil.TempDir("", "docker-daemon-unix-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	daemon := &Daemon{
		repository: tmp,
		root:       tmp,
	}

	hostConfig := &containertypes.HostConfig{
		Resources: containertypes.Resources{CPUShares: linuxMinCPUShares - 1},
	}
	daemon.adaptContainerSettings(hostConfig, true)
	if hostConfig.CPUShares != linuxMinCPUShares {
		t.Errorf("Expected CPUShares to be %d", linuxMinCPUShares)
	}

	hostConfig.CPUShares = linuxMaxCPUShares + 1
	daemon.adaptContainerSettings(hostConfig, true)
	if hostConfig.CPUShares != linuxMaxCPUShares {
		t.Errorf("Expected CPUShares to be %d", linuxMaxCPUShares)
	}

	hostConfig.CPUShares = 0
	daemon.adaptContainerSettings(hostConfig, true)
	if hostConfig.CPUShares != 0 {
		t.Error("Expected CPUShares to be unchanged")
	}

	hostConfig.CPUShares = 1024
	daemon.adaptContainerSettings(hostConfig, true)
	if hostConfig.CPUShares != 1024 {
		t.Error("Expected CPUShares to be unchanged")
	}
}

// Unix test as uses settings which are not available on Windows
func TestAdjustCPUSharesNoAdjustment(t *testing.T) {
	tmp, err := ioutil.TempDir("", "docker-daemon-unix-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	daemon := &Daemon{
		repository: tmp,
		root:       tmp,
	}

	hostConfig := &containertypes.HostConfig{
		Resources: containertypes.Resources{CPUShares: linuxMinCPUShares - 1},
	}
	daemon.adaptContainerSettings(hostConfig, false)
	if hostConfig.CPUShares != linuxMinCPUShares-1 {
		t.Errorf("Expected CPUShares to be %d", linuxMinCPUShares-1)
	}

	hostConfig.CPUShares = linuxMaxCPUShares + 1
	daemon.adaptContainerSettings(hostConfig, false)
	if hostConfig.CPUShares != linuxMaxCPUShares+1 {
		t.Errorf("Expected CPUShares to be %d", linuxMaxCPUShares+1)
	}

	hostConfig.CPUShares = 0
	daemon.adaptContainerSettings(hostConfig, false)
	if hostConfig.CPUShares != 0 {
		t.Error("Expected CPUShares to be unchanged")
	}

	hostConfig.CPUShares = 1024
	daemon.adaptContainerSettings(hostConfig, false)
	if hostConfig.CPUShares != 1024 {
		t.Error("Expected CPUShares to be unchanged")
	}
}

// Unix test as uses settings which are not available on Windows
func TestParseSecurityOptWithDeprecatedColon(t *testing.T) {
	container := &container.Container{}
	config := &containertypes.HostConfig{}

	// test apparmor
	config.SecurityOpt = []string{"apparmor=test_profile"}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}
	if container.AppArmorProfile != "test_profile" {
		t.Fatalf("Unexpected AppArmorProfile, expected: \"test_profile\", got %q", container.AppArmorProfile)
	}

	// test seccomp
	sp := "/path/to/seccomp_test.json"
	config.SecurityOpt = []string{"seccomp=" + sp}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}
	if container.SeccompProfile != sp {
		t.Fatalf("Unexpected AppArmorProfile, expected: %q, got %q", sp, container.SeccompProfile)
	}

	// test valid label
	config.SecurityOpt = []string{"label=user:USER"}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}

	// test invalid label
	config.SecurityOpt = []string{"label"}
	if err := parseSecurityOpt(container, config); err == nil {
		t.Fatal("Expected parseSecurityOpt error, got nil")
	}

	// test invalid opt
	config.SecurityOpt = []string{"test"}
	if err := parseSecurityOpt(container, config); err == nil {
		t.Fatal("Expected parseSecurityOpt error, got nil")
	}
}

func TestParseSecurityOpt(t *testing.T) {
	container := &container.Container{}
	config := &containertypes.HostConfig{}

	// test apparmor
	config.SecurityOpt = []string{"apparmor=test_profile"}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}
	if container.AppArmorProfile != "test_profile" {
		t.Fatalf("Unexpected AppArmorProfile, expected: \"test_profile\", got %q", container.AppArmorProfile)
	}

	// test seccomp
	sp := "/path/to/seccomp_test.json"
	config.SecurityOpt = []string{"seccomp=" + sp}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}
	if container.SeccompProfile != sp {
		t.Fatalf("Unexpected SeccompProfile, expected: %q, got %q", sp, container.SeccompProfile)
	}

	// test valid label
	config.SecurityOpt = []string{"label=user:USER"}
	if err := parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected parseSecurityOpt error: %v", err)
	}

	// test invalid label
	config.SecurityOpt = []string{"label"}
	if err := parseSecurityOpt(container, config); err == nil {
		t.Fatal("Expected parseSecurityOpt error, got nil")
	}

	// test invalid opt
	config.SecurityOpt = []string{"test"}
	if err := parseSecurityOpt(container, config); err == nil {
		t.Fatal("Expected parseSecurityOpt error, got nil")
	}
}

func TestParseNNPSecurityOptions(t *testing.T) {
	daemon := &Daemon{
		configStore: &config.Config{NoNewPrivileges: true},
	}
	container := &container.Container{}
	config := &containertypes.HostConfig{}

	// test NNP when "daemon:true" and "no-new-privileges=false""
	config.SecurityOpt = []string{"no-new-privileges=false"}

	if err := daemon.parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected daemon.parseSecurityOpt error: %v", err)
	}
	if container.NoNewPrivileges {
		t.Fatalf("container.NoNewPrivileges should be FALSE: %v", container.NoNewPrivileges)
	}

	// test NNP when "daemon:false" and "no-new-privileges=true""
	daemon.configStore.NoNewPrivileges = false
	config.SecurityOpt = []string{"no-new-privileges=true"}

	if err := daemon.parseSecurityOpt(container, config); err != nil {
		t.Fatalf("Unexpected daemon.parseSecurityOpt error: %v", err)
	}
	if !container.NoNewPrivileges {
		t.Fatalf("container.NoNewPrivileges should be TRUE: %v", container.NoNewPrivileges)
	}
}

func TestNetworkOptions(t *testing.T) {
	daemon := &Daemon{}
	dconfigCorrect := &config.Config{
		CommonConfig: config.CommonConfig{
			ClusterStore:     "consul://localhost:8500",
			ClusterAdvertise: "192.168.0.1:8000",
		},
	}

	if _, err := daemon.networkOptions(dconfigCorrect, nil, nil); err != nil {
		t.Fatalf("Expect networkOptions success, got error: %v", err)
	}

	dconfigWrong := &config.Config{
		CommonConfig: config.CommonConfig{
			ClusterStore: "consul://localhost:8500://test://bbb",
		},
	}

	if _, err := daemon.networkOptions(dconfigWrong, nil, nil); err == nil {
		t.Fatal("Expected networkOptions error, got nil")
	}
}

func TestVerifyPlatformContainerResources(t *testing.T) {
	t.Parallel()
	var (
		no  = false
		yes = true
	)

	withMemoryLimit := func(si *sysinfo.SysInfo) {
		si.MemoryLimit = true
	}
	withSwapLimit := func(si *sysinfo.SysInfo) {
		si.SwapLimit = true
	}
	withOomKillDisable := func(si *sysinfo.SysInfo) {
		si.OomKillDisable = true
	}

	tests := []struct {
		name             string
		resources        containertypes.Resources
		sysInfo          sysinfo.SysInfo
		update           bool
		expectedWarnings []string
	}{
		{
			name:             "no-oom-kill-disable",
			resources:        containertypes.Resources{},
			sysInfo:          sysInfo(t, withMemoryLimit),
			expectedWarnings: []string{},
		},
		{
			name: "oom-kill-disable-disabled",
			resources: containertypes.Resources{
				OomKillDisable: &no,
			},
			sysInfo:          sysInfo(t, withMemoryLimit),
			expectedWarnings: []string{},
		},
		{
			name: "oom-kill-disable-not-supported",
			resources: containertypes.Resources{
				OomKillDisable: &yes,
			},
			sysInfo: sysInfo(t, withMemoryLimit),
			expectedWarnings: []string{
				"Your kernel does not support OomKillDisable. OomKillDisable discarded.",
			},
		},
		{
			name: "oom-kill-disable-without-memory-constraints",
			resources: containertypes.Resources{
				OomKillDisable: &yes,
				Memory:         0,
			},
			sysInfo: sysInfo(t, withMemoryLimit, withOomKillDisable, withSwapLimit),
			expectedWarnings: []string{
				"OOM killer is disabled for the container, but no memory limit is set, this can result in the system running out of resources.",
			},
		},
		{
			name: "oom-kill-disable-with-memory-constraints-but-no-memory-limit-support",
			resources: containertypes.Resources{
				OomKillDisable: &yes,
				Memory:         linuxMinMemory,
			},
			sysInfo: sysInfo(t, withOomKillDisable),
			expectedWarnings: []string{
				"Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded.",
				"OOM killer is disabled for the container, but no memory limit is set, this can result in the system running out of resources.",
			},
		},
		{
			name: "oom-kill-disable-with-memory-constraints",
			resources: containertypes.Resources{
				OomKillDisable: &yes,
				Memory:         linuxMinMemory,
			},
			sysInfo:          sysInfo(t, withMemoryLimit, withOomKillDisable, withSwapLimit),
			expectedWarnings: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			warnings, err := verifyPlatformContainerResources(&tc.resources, &tc.sysInfo, tc.update)
			assert.NilError(t, err)
			for _, w := range tc.expectedWarnings {
				assert.Assert(t, is.Contains(warnings, w))
			}
		})
	}
}

func sysInfo(t *testing.T, opts ...func(*sysinfo.SysInfo)) sysinfo.SysInfo {
	t.Helper()
	si := sysinfo.SysInfo{}

	for _, opt := range opts {
		opt(&si)
	}

	if si.OomKillDisable {
		t.Log(t.Name(), "OOM disable supported")
	}
	return si
}

const (
	// prepare major 0x1FD(509 in decimal) and minor 0x130(304)
	DEVNO  = 0x11FD30
	MAJOR  = 509
	MINOR  = 304
	WEIGHT = 1024
)

func deviceTypeMock(t *testing.T, testAndCheck func(string)) {
	if os.Getuid() != 0 {
		t.Skip("root required") // for mknod
	}

	t.Parallel()

	tempDir, err := ioutil.TempDir("", "tempDevDir"+t.Name())
	assert.NilError(t, err, "create temp file")
	tempFile := filepath.Join(tempDir, "dev")

	defer os.RemoveAll(tempDir)

	if err = unix.Mknod(tempFile, unix.S_IFCHR, DEVNO); err != nil {
		t.Fatalf("mknod error %s(%x): %v", tempFile, DEVNO, err)
	}

	testAndCheck(tempFile)
}

func TestGetBlkioWeightDevices(t *testing.T) {
	deviceTypeMock(t, func(tempFile string) {
		mockResource := containertypes.Resources{
			BlkioWeightDevice: []*blkiodev.WeightDevice{{Path: tempFile, Weight: WEIGHT}},
		}

		weightDevs, err := getBlkioWeightDevices(mockResource)

		assert.NilError(t, err, "getBlkioWeightDevices")
		assert.Check(t, is.Len(weightDevs, 1), "getBlkioWeightDevices")
		assert.Check(t, weightDevs[0].Major == MAJOR, "get major device type")
		assert.Check(t, weightDevs[0].Minor == MINOR, "get minor device type")
		assert.Check(t, *weightDevs[0].Weight == WEIGHT, "get device weight")
	})
}

func TestGetBlkioThrottleDevices(t *testing.T) {
	deviceTypeMock(t, func(tempFile string) {
		mockDevs := []*blkiodev.ThrottleDevice{{Path: tempFile, Rate: WEIGHT}}

		retDevs, err := getBlkioThrottleDevices(mockDevs)

		assert.NilError(t, err, "getBlkioThrottleDevices")
		assert.Check(t, is.Len(retDevs, 1), "getBlkioThrottleDevices")
		assert.Check(t, retDevs[0].Major == MAJOR, "get major device type")
		assert.Check(t, retDevs[0].Minor == MINOR, "get minor device type")
		assert.Check(t, retDevs[0].Rate == WEIGHT, "get device rate")
	})
}

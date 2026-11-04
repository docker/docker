package container

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestCreateWithCDIDevices(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "CDI devices are only supported on Linux")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run cdi tests with a remote daemon")

	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	cwd, err := os.Getwd()
	assert.NilError(t, err)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--cdi-spec-dir="+filepath.Join(cwd, "testdata", "cdi"), "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	apiClient := d.NewClientT(t)

	id := container.Run(ctx, t, apiClient,
		container.WithCmd("/bin/sh", "-c", "env"),
		container.WithCDIDevices("vendor1.com/device=foo"),
	)
	defer container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})

	res, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	assert.NilError(t, err)

	expectedRequests := []containertypes.DeviceRequest{
		{
			Driver:    "cdi",
			DeviceIDs: []string{"vendor1.com/device=foo"},
		},
	}
	assert.Check(t, is.DeepEqual(res.Container.HostConfig.DeviceRequests, expectedRequests))

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, id))
	reader, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)

	actualStdout := new(bytes.Buffer)
	actualStderr := io.Discard
	_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
	assert.NilError(t, err)

	outlines := strings.Split(actualStdout.String(), "\n")
	assert.Assert(t, is.Contains(outlines, "FOO=injected"))
}

func TestCDISpecDirsAreInSystemInfo(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows") // d.Start fails on Windows with `protocol not available`
	// TODO: This restriction can be relaxed with https://github.com/moby/moby/pull/46158
	skip.If(t, testEnv.IsRootless, "the t.TempDir test creates a folder with incorrect permissions for rootless")

	t.Parallel()

	testCases := []struct {
		description             string
		config                  string
		specDirs                []string
		expectedInfoCDISpecDirs []string
	}{
		{
			description:             "No config returns default CDI spec dirs",
			config:                  `{}`,
			specDirs:                nil,
			expectedInfoCDISpecDirs: []string{"/etc/cdi", "/var/run/cdi"},
		},
		{
			description:             "CDI explicitly enabled with no spec dirs specified returns default",
			config:                  `{"features": {"cdi": true}}`,
			specDirs:                nil,
			expectedInfoCDISpecDirs: []string{"/etc/cdi", "/var/run/cdi"},
		},
		{
			description:             "CDI enabled with specified spec dirs are returned",
			config:                  `{"features": {"cdi": true}}`,
			specDirs:                []string{"/foo/bar", "/baz/qux"},
			expectedInfoCDISpecDirs: []string{"/foo/bar", "/baz/qux"},
		},
		{
			description:             "CDI enabled with empty string as spec dir returns empty slice",
			config:                  `{"features": {"cdi": true}}`,
			specDirs:                []string{""},
			expectedInfoCDISpecDirs: []string{},
		},
		{
			description:             "CDI enabled with empty config option returns empty slice",
			config:                  `{"features": {"cdi": true}, "cdi-spec-dirs": []}`,
			expectedInfoCDISpecDirs: []string{},
		},
		{
			description:             "CDI explicitly disabled with no spec dirs specified returns empty slice",
			config:                  `{"features": {"cdi": false}}`,
			specDirs:                nil,
			expectedInfoCDISpecDirs: []string{},
		},
		{
			description:             "CDI explicitly disabled with specified spec dirs returns empty slice",
			config:                  `{"features": {"cdi": false}}`,
			specDirs:                []string{"/foo/bar", "/baz/qux"},
			expectedInfoCDISpecDirs: []string{},
		},
		{
			description:             "CDI explicitly disabled with empty string as spec dir returns empty slice",
			config:                  `{"features": {"cdi": false}}`,
			specDirs:                []string{""},
			expectedInfoCDISpecDirs: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			var opts []daemon.Option
			d := daemon.New(t, opts...)

			var args []string
			for _, specDir := range tc.specDirs {
				args = append(args, "--cdi-spec-dir="+specDir)
			}
			if tc.config != "" {
				configPath := filepath.Join(t.TempDir(), "daemon.json")

				err := os.WriteFile(configPath, []byte(tc.config), 0o644)
				assert.NilError(t, err)

				args = append(args, "--config-file="+configPath)
			}
			args = append(args, "--iptables=false", "--ip6tables=false")
			d.Start(t, args...)
			defer d.Stop(t)

			info := d.Info(t)

			assert.Check(t, is.DeepEqual(tc.expectedInfoCDISpecDirs, info.CDISpecDirs))
		})
	}
}

func TestCDIInfoDiscoveredDevices(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "CDI not supported on Windows")

	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	// Create a sample CDI spec file
	specContent := `{
		"cdiVersion": "0.5.0",
		"kind": "test.com/device",
		"devices": [
			{
				"name": "mygpu0",
				"containerEdits": {
					"deviceNodes": [
						{"path": "/dev/null"}
					]
				}
			}
		]
	}`

	cdiDir := testutil.TempDir(t)
	specFilePath := filepath.Join(cdiDir, "test-device.json")

	err := os.WriteFile(specFilePath, []byte(specContent), 0o644)
	assert.NilError(t, err, "Failed to write sample CDI spec file")

	d := daemon.New(t)
	d.Start(t, "--feature", "cdi", "--cdi-spec-dir="+cdiDir, "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	c := d.NewClientT(t)
	result, err := c.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)
	info := result.Info

	assert.Check(t, is.Len(info.CDISpecDirs, 1))
	assert.Check(t, is.Equal(info.CDISpecDirs[0], cdiDir))

	expectedDevice := system.DeviceInfo{
		Source: "cdi",
		ID:     "test.com/device=mygpu0",
	}

	assert.Check(t, is.Equal(len(info.DiscoveredDevices), 1), "Expected one discovered device")
	assert.Check(t, is.DeepEqual(info.DiscoveredDevices, []system.DeviceInfo{expectedDevice}))
}

// TestEtcCDI verifies that the daemon picks up CDI specs from /etc/cdi, even in rootless mode.
// Added for https://github.com/moby/moby/pull/51624
func TestEtcCDI(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "CDI not supported on Windows")

	ctx := testutil.StartSpan(baseContext, t)

	// Create a sample CDI spec file
	specContent := `{
		"cdiVersion": "0.5.0",
		"kind": "test.mobyproject.org/device",
		"devices": [
			{
				"name": "mygpu0",
				"containerEdits": {
					"deviceNodes": [
						{"path": "/dev/null"}
					]
				}
			}
		]
	}`

	cdiDir := "/etc/cdi"
	_, err := os.Stat(cdiDir)
	cdiDirExisted := !errors.Is(err, os.ErrNotExist)
	err = os.MkdirAll(cdiDir, 0o755)
	assert.NilError(t, err, "Failed to create /etc/cdi directory")

	t.Cleanup(func() {
		if !cdiDirExisted {
			rmErr := os.Remove(cdiDir)
			assert.NilError(t, rmErr, "Failed to remove /etc/cdi directory")
		}
	})

	specFilePath := filepath.Join(cdiDir, "moby-integration-test-device.json")
	err = os.WriteFile(specFilePath, []byte(specContent), 0o644)
	assert.NilError(t, err, "Failed to write sample CDI spec file")
	t.Cleanup(func() {
		rmErr := os.RemoveAll(specFilePath)
		assert.NilError(t, rmErr, "Failed to remove sample CDI spec file")
	})

	d := daemon.New(t)
	d.Start(t, "--feature", "cdi")
	defer d.Stop(t)

	c := d.NewClientT(t)
	result, err := c.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)
	info := result.Info

	expectedDevice := system.DeviceInfo{
		Source: "cdi",
		ID:     "test.mobyproject.org/device=mygpu0",
	}
	assert.Check(t, is.Contains(info.DiscoveredDevices, expectedDevice))
}

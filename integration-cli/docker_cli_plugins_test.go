package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	plugintypes "github.com/moby/moby/api/types/plugin"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/fixtures/plugin"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

const (
	pluginProcessName = "sample-volume-plugin"
	pName             = "tiborvass/sample-volume-plugin"
	npName            = "tiborvass/test-docker-netplugin"
	pTag              = "latest"
	pNameWithTag      = pName + ":" + pTag
	npNameWithTag     = npName + ":" + pTag
)

type DockerCLIPluginsSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIPluginsSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIPluginsSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (ps *DockerPluginSuite) TestPluginBasicOps(c *testing.T) {
	pluginName := ps.getPluginRepoWithTag()
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pluginName)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	assert.Check(c, is.Contains(out, "true"))
	id, _, err := dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", pluginName)
	id = strings.TrimSpace(id)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pluginName)
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "is enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", pluginName)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id))
	if !os.IsNotExist(err) {
		c.Fatal(err)
	}
}

func (ps *DockerPluginSuite) TestPluginForceRemove(c *testing.T) {
	pluginName := ps.getPluginRepoWithTag()

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pluginName)
	assert.NilError(c, err)

	out, _, _ := dockerCmdWithError("plugin", "remove", pluginName)
	assert.Check(c, is.Contains(out, "is enabled"))
	out, _, err = dockerCmdWithError("plugin", "remove", "--force", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
}

func (s *DockerCLIPluginsSuite) TestPluginActive(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pNameWithTag)
	assert.NilError(c, err)

	_, _, err = dockerCmdWithError("volume", "create", "-d", pNameWithTag, "--name", "testvol1")
	assert.NilError(c, err)

	out, _, _ := dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.Check(c, is.Contains(out, "in use"))
	_, _, err = dockerCmdWithError("volume", "rm", "testvol1")
	assert.NilError(c, err)

	_, _, err = dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pNameWithTag)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pNameWithTag))
}

func (s *DockerCLIPluginsSuite) TestPluginActiveNetwork(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", npNameWithTag)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("network", "create", "-d", npNameWithTag, "test")
	assert.NilError(c, err)

	nID := strings.TrimSpace(out)

	out, _, _ = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.Check(c, is.Contains(out, "is in use"))
	_, _, err = dockerCmdWithError("network", "rm", nID)
	assert.NilError(c, err)

	out, _, _ = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.Check(c, is.Contains(out, "is enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", npNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, npNameWithTag))
}

func (ps *DockerPluginSuite) TestPluginInstallDisable(c *testing.T) {
	pluginName := ps.getPluginRepoWithTag()

	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", "--disable", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, "false"))
	out, _, err = dockerCmdWithError("plugin", "enable", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	out, _, err = dockerCmdWithError("plugin", "disable", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	out, _, err = dockerCmdWithError("plugin", "remove", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
}

func (s *DockerCLIPluginsSuite) TestPluginInstallDisableVolumeLs(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)
	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", "--disable", pName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pName))
	cli.DockerCmd(c, "volume", "ls")
}

func (ps *DockerPluginSuite) TestPluginSet(c *testing.T) {
	client := testEnv.APIClient()

	name := "test"
	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 60*time.Second)
	defer cancel()

	initialValue := "0"
	mntSrc := "foo"
	devPath := "/dev/bar"

	// Create a new plugin with extra settings
	err := plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Env = []plugintypes.Env{{Name: "DEBUG", Value: &initialValue, Settable: []string{"value"}}}
		cfg.Mounts = []plugintypes.Mount{
			{Name: "pmount1", Settable: []string{"source"}, Type: "none", Source: &mntSrc},
			{Name: "pmount2", Settable: []string{"source"}, Type: "none"}, // Mount without source is invalid.
		}
		cfg.Linux.Devices = []plugintypes.Device{
			{Name: "pdev1", Path: &devPath, Settable: []string{"path"}},
			{Name: "pdev2", Settable: []string{"path"}}, // Device without Path is invalid.
		}
	})
	assert.Assert(c, err == nil, "failed to create test plugin")

	env := cli.DockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", name).Stdout()
	assert.Check(c, is.Equal(strings.TrimSpace(env), "[DEBUG=0]"))

	cli.DockerCmd(c, "plugin", "set", name, "DEBUG=1")

	env = cli.DockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", name).Stdout()
	assert.Check(c, is.Equal(strings.TrimSpace(env), "[DEBUG=1]"))

	mounts := cli.DockerCmd(c, "plugin", "inspect", "-f", "{{with $mount := index .Settings.Mounts 0}}{{$mount.Source}}{{end}}", name).Stdout()
	assert.Check(c, is.Contains(mounts, mntSrc))
	cli.DockerCmd(c, "plugin", "set", name, "pmount1.source=bar")

	mounts = cli.DockerCmd(c, "plugin", "inspect", "-f", "{{with $mount := index .Settings.Mounts 0}}{{$mount.Source}}{{end}}", name).Stdout()
	assert.Check(c, is.Contains(mounts, "bar"))
	out, _, err := dockerCmdWithError("plugin", "set", name, "pmount2.source=bar2")
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "plugin config has no mount source"))
	out, _, err = dockerCmdWithError("plugin", "set", name, "pdev2.path=/dev/bar2")
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "plugin config has no device path"))
}

func (ps *DockerPluginSuite) TestPluginInstallArgs(c *testing.T) {
	pluginName := path.Join(ps.registryHost(), "plugin", "testplugininstallwithargs")
	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 60*time.Second)
	defer cancel()

	plugin.CreateInRegistry(ctx, pluginName, nil, func(cfg *plugin.Config) {
		cfg.Env = []plugintypes.Env{{Name: "DEBUG", Settable: []string{"value"}}}
	})

	out := cli.DockerCmd(c, "plugin", "install", "--grant-all-permissions", "--disable", pluginName, "DEBUG=1").Stdout()
	assert.Check(c, is.Contains(out, pluginName))
	env := cli.DockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", pluginName).Stdout()
	assert.Check(c, is.Equal(strings.TrimSpace(env), "[DEBUG=1]"))
}

func (ps *DockerPluginSuite) TestPluginInstallImage(c *testing.T) {
	testRequires(c, IsAmd64)
	skip.If(c, GitHubActions, "FIXME: https://github.com/moby/moby/issues/43996")

	const imgRepo = privateRegistryURL + "/dockercli/busybox"
	// tag the image to upload it to the private registry
	cli.DockerCmd(c, "tag", "busybox", imgRepo)
	// push the image to the registry
	cli.DockerCmd(c, "push", imgRepo)

	out, _, err := dockerCmdWithError("plugin", "install", imgRepo)
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, `did not find plugin config for specified reference`))
}

func (ps *DockerPluginSuite) TestPluginEnableDisableNegative(c *testing.T) {
	pluginName := ps.getPluginRepoWithTag()

	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	out, _, err = dockerCmdWithError("plugin", "enable", pluginName)
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "already enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", pluginName)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "disable", pluginName)
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "already disabled"))
	_, _, err = dockerCmdWithError("plugin", "remove", pluginName)
	assert.NilError(c, err)
}

func (ps *DockerPluginSuite) TestPluginCreate(c *testing.T) {
	name := "foo/bar-driver"
	temp, err := os.MkdirTemp("", "foo")
	assert.NilError(c, err)
	defer os.RemoveAll(temp)

	data := `{"description": "foo plugin"}`
	err = os.WriteFile(filepath.Join(temp, "config.json"), []byte(data), 0o644)
	assert.NilError(c, err)

	err = os.MkdirAll(filepath.Join(temp, "rootfs"), 0o700)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "create", name, temp)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	out, _, err = dockerCmdWithError("plugin", "create", name, temp)
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "already exist"))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	// The output will consists of one HEADER line and one line of foo/bar-driver
	assert.Equal(c, len(strings.Split(strings.TrimSpace(out), "\n")), 2)
}

func (ps *DockerPluginSuite) TestPluginInspect(c *testing.T) {
	pluginName := ps.getPluginRepoWithTag()

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pluginName)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	assert.Check(c, is.Contains(out, "true"))
	// Find the ID first
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", pluginName)
	assert.NilError(c, err)
	id := strings.TrimSpace(out)
	assert.Assert(c, id != "")

	// Long form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id)
	assert.NilError(c, err)
	assert.Check(c, is.Equal(strings.TrimSpace(out), id))

	// Short form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id[:5])
	assert.NilError(c, err)
	assert.Check(c, is.Equal(strings.TrimSpace(out), id))

	// Name with tag form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Equal(strings.TrimSpace(out), id))

	// Name without tag form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", ps.getPluginRepo())
	assert.NilError(c, err)
	assert.Check(c, is.Equal(strings.TrimSpace(out), id))

	_, _, err = dockerCmdWithError("plugin", "disable", pluginName)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pluginName)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, pluginName))
	// After remove nothing should be found
	_, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id[:5])
	assert.ErrorContains(c, err, "")
}

// Test case for https://github.com/moby/moby/pull/29186#discussion_r91277345
func (s *DockerCLIPluginsSuite) TestPluginInspectOnWindows(c *testing.T) {
	// This test should work on Windows only
	testRequires(c, DaemonIsWindows)

	out, _, err := dockerCmdWithError("plugin", "inspect", "foobar")
	assert.ErrorContains(c, err, "")
	assert.Check(c, is.Contains(out, "plugins are not supported on this platform"))
	assert.ErrorContains(c, err, "plugins are not supported on this platform")
}

func (ps *DockerPluginSuite) TestPluginIDPrefix(c *testing.T) {
	name := "test"
	client := testEnv.APIClient()

	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 60*time.Second)
	initialValue := "0"
	err := plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Env = []plugintypes.Env{{Name: "DEBUG", Value: &initialValue, Settable: []string{"value"}}}
	})
	cancel()

	assert.Assert(c, err == nil, "failed to create test plugin")

	// Find ID first
	id, _, err := dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", name)
	id = strings.TrimSpace(id)
	assert.NilError(c, err)

	// List current state
	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	assert.Check(c, is.Contains(out, "false"))
	env := cli.DockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", id[:5]).Stdout()
	assert.Check(c, is.Equal(strings.TrimSpace(env), "[DEBUG=0]"))

	cli.DockerCmd(c, "plugin", "set", id[:5], "DEBUG=1")

	env = cli.DockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", id[:5]).Stdout()
	assert.Check(c, is.Equal(strings.TrimSpace(env), "[DEBUG=1]"))

	// Enable
	_, _, err = dockerCmdWithError("plugin", "enable", id[:5])
	assert.NilError(c, err)
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	assert.Check(c, is.Contains(out, "true"))
	// Disable
	_, _, err = dockerCmdWithError("plugin", "disable", id[:5])
	assert.NilError(c, err)
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, name))
	assert.Check(c, is.Contains(out, "false"))
	// Remove
	_, _, err = dockerCmdWithError("plugin", "remove", id[:5])
	assert.NilError(c, err)
	// List returns none
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(out, name))
}

func (ps *DockerPluginSuite) TestPluginListDefaultFormat(c *testing.T) {
	config, err := os.MkdirTemp("", "config-file-")
	assert.NilError(c, err)
	defer os.RemoveAll(config)

	err = os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"pluginsFormat": "raw"}`), 0o644)
	assert.NilError(c, err)

	name := "test:latest"
	client := testEnv.APIClient()

	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 60*time.Second)
	defer cancel()
	err = plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Description = "test plugin"
	})
	assert.Assert(c, err == nil, "failed to create test plugin")

	id := cli.DockerCmd(c, "plugin", "inspect", "--format", "{{.ID}}", name).Stdout()
	id = strings.TrimSpace(id)

	// We expect the format to be in `raw + --no-trunc`
	expectedOutput := fmt.Sprintf(`plugin_id: %s
name: %s
description: test plugin
enabled: false`, id, name)

	out := cli.DockerCmd(c, "--config", config, "plugin", "ls", "--no-trunc").Combined()
	assert.Check(c, is.Contains(out, expectedOutput))
}

func (s *DockerCLIPluginsSuite) TestPluginUpgrade(c *testing.T) {
	testRequires(c, DaemonIsLinux, Network, testEnv.IsLocalDaemon, IsAmd64, NotUserNamespace)
	const pluginName = "cpuguy83/docker-volume-driver-plugin-local:latest"
	const pluginV2 = "cpuguy83/docker-volume-driver-plugin-local:v2"

	cli.DockerCmd(c, "plugin", "install", "--grant-all-permissions", pluginName)
	cli.DockerCmd(c, "volume", "create", "--driver", pluginName, "bananas")
	cli.DockerCmd(c, "run", "--rm", "-v", "bananas:/apple", "busybox", "sh", "-c", "touch /apple/core")

	out, _, err := dockerCmdWithError("plugin", "upgrade", "--grant-all-permissions", pluginName, pluginV2)
	assert.ErrorContains(c, err, "", out)
	assert.Check(c, is.Contains(out, "disabled before upgrading"))
	id := cli.DockerCmd(c, "plugin", "inspect", "--format={{.ID}}", pluginName).Stdout()
	id = strings.TrimSpace(id)

	// make sure "v2" does not exists
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id, "rootfs", "v2"))
	assert.Assert(c, os.IsNotExist(err), out)

	cli.DockerCmd(c, "plugin", "disable", "-f", pluginName)
	cli.DockerCmd(c, "plugin", "upgrade", "--grant-all-permissions", "--skip-remote-check", pluginName, pluginV2)

	// make sure "v2" file exists
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id, "rootfs", "v2"))
	assert.NilError(c, err)

	cli.DockerCmd(c, "plugin", "enable", pluginName)
	cli.DockerCmd(c, "volume", "inspect", "bananas")
	cli.DockerCmd(c, "run", "--rm", "-v", "bananas:/apple", "busybox", "sh", "-c", "ls -lh /apple/core")
}

func (s *DockerCLIPluginsSuite) TestPluginMetricsCollector(c *testing.T) {
	testRequires(c, DaemonIsLinux, Network, testEnv.IsLocalDaemon, IsAmd64)
	d := daemon.New(c, dockerBinary, dockerdBinary)
	d.Start(c)
	defer d.Stop(c)

	name := "cpuguy83/docker-metrics-plugin-test:latest"
	r := cli.Docker(cli.Args("plugin", "install", "--grant-all-permissions", name), cli.Daemon(d))
	assert.Assert(c, r.Error == nil, r.Combined())

	// plugin listens on localhost:19393 and proxies the metrics
	resp, err := http.Get("http://localhost:19393/metrics")
	assert.NilError(c, err)
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	assert.NilError(c, err)
	// check that a known metric is there... don't expect this metric to change over time.. probably safe
	assert.Check(c, is.Contains(string(b), "container_actions"))
}

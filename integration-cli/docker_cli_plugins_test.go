package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/docker/testutil/fixtures/plugin"
	"gotest.tools/assert"
)

var (
	pluginProcessName = "sample-volume-plugin"
	pName             = "tiborvass/sample-volume-plugin"
	npName            = "tiborvass/test-docker-netplugin"
	pTag              = "latest"
	pNameWithTag      = pName + ":" + pTag
	npNameWithTag     = npName + ":" + pTag
)

func (ps *DockerPluginSuite) TestPluginBasicOps(c *testing.T) {
	plugin := ps.getPluginRepoWithTag()
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", plugin)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, plugin))
	assert.Assert(c, strings.Contains(out, "true"))
	id, _, err := dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", plugin)
	id = strings.TrimSpace(id)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", plugin)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "is enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", plugin)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", plugin)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, plugin))
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id))
	if !os.IsNotExist(err) {
		c.Fatal(err)
	}
}

func (ps *DockerPluginSuite) TestPluginForceRemove(c *testing.T) {
	pNameWithTag := ps.getPluginRepoWithTag()

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pNameWithTag)
	assert.NilError(c, err)

	out, _, _ := dockerCmdWithError("plugin", "remove", pNameWithTag)
	assert.Assert(c, strings.Contains(out, "is enabled"))
	out, _, err = dockerCmdWithError("plugin", "remove", "--force", pNameWithTag)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, pNameWithTag))
}

func (s *DockerSuite) TestPluginActive(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pNameWithTag)
	assert.NilError(c, err)

	_, _, err = dockerCmdWithError("volume", "create", "-d", pNameWithTag, "--name", "testvol1")
	assert.NilError(c, err)

	out, _, _ := dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.Assert(c, strings.Contains(out, "in use"))
	_, _, err = dockerCmdWithError("volume", "rm", "testvol1")
	assert.NilError(c, err)

	_, _, err = dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pNameWithTag)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, pNameWithTag))
}

func (s *DockerSuite) TestPluginActiveNetwork(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", npNameWithTag)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("network", "create", "-d", npNameWithTag, "test")
	assert.NilError(c, err)

	nID := strings.TrimSpace(out)

	out, _, _ = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.Assert(c, strings.Contains(out, "is in use"))
	_, _, err = dockerCmdWithError("network", "rm", nID)
	assert.NilError(c, err)

	out, _, _ = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.Assert(c, strings.Contains(out, "is enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", npNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", npNameWithTag)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, npNameWithTag))
}

func (ps *DockerPluginSuite) TestPluginInstallDisable(c *testing.T) {
	pName := ps.getPluginRepoWithTag()

	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", "--disable", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "false"))
	out, _, err = dockerCmdWithError("plugin", "enable", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	out, _, err = dockerCmdWithError("plugin", "disable", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	out, _, err = dockerCmdWithError("plugin", "remove", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
}

func (s *DockerSuite) TestPluginInstallDisableVolumeLs(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)
	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", "--disable", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	dockerCmd(c, "volume", "ls")
}

func (ps *DockerPluginSuite) TestPluginSet(c *testing.T) {
	client := testEnv.APIClient()

	name := "test"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	initialValue := "0"
	mntSrc := "foo"
	devPath := "/dev/bar"

	// Create a new plugin with extra settings
	err := plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Env = []types.PluginEnv{{Name: "DEBUG", Value: &initialValue, Settable: []string{"value"}}}
		cfg.Mounts = []types.PluginMount{
			{Name: "pmount1", Settable: []string{"source"}, Type: "none", Source: &mntSrc},
			{Name: "pmount2", Settable: []string{"source"}, Type: "none"}, // Mount without source is invalid.
		}
		cfg.Linux.Devices = []types.PluginDevice{
			{Name: "pdev1", Path: &devPath, Settable: []string{"path"}},
			{Name: "pdev2", Settable: []string{"path"}}, // Device without Path is invalid.
		}
	})
	assert.Assert(c, err == nil, "failed to create test plugin")

	env, _ := dockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", name)
	assert.Equal(c, strings.TrimSpace(env), "[DEBUG=0]")

	dockerCmd(c, "plugin", "set", name, "DEBUG=1")

	env, _ = dockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", name)
	assert.Equal(c, strings.TrimSpace(env), "[DEBUG=1]")

	env, _ = dockerCmd(c, "plugin", "inspect", "-f", "{{with $mount := index .Settings.Mounts 0}}{{$mount.Source}}{{end}}", name)
	assert.Assert(c, strings.Contains(strings.TrimSpace(env), mntSrc))
	dockerCmd(c, "plugin", "set", name, "pmount1.source=bar")

	env, _ = dockerCmd(c, "plugin", "inspect", "-f", "{{with $mount := index .Settings.Mounts 0}}{{$mount.Source}}{{end}}", name)
	assert.Assert(c, strings.Contains(strings.TrimSpace(env), "bar"))
	out, _, err := dockerCmdWithError("plugin", "set", name, "pmount2.source=bar2")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "Plugin config has no mount source"))
	out, _, err = dockerCmdWithError("plugin", "set", name, "pdev2.path=/dev/bar2")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "Plugin config has no device path"))
}

func (ps *DockerPluginSuite) TestPluginInstallArgs(c *testing.T) {
	pName := path.Join(ps.registryHost(), "plugin", "testplugininstallwithargs")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	plugin.CreateInRegistry(ctx, pName, nil, func(cfg *plugin.Config) {
		cfg.Env = []types.PluginEnv{{Name: "DEBUG", Settable: []string{"value"}}}
	})

	out, _ := dockerCmd(c, "plugin", "install", "--grant-all-permissions", "--disable", pName, "DEBUG=1")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	env, _ := dockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", pName)
	assert.Equal(c, strings.TrimSpace(env), "[DEBUG=1]")
}

func (ps *DockerPluginSuite) TestPluginInstallImage(c *testing.T) {
	testRequires(c, IsAmd64)

	repoName := fmt.Sprintf("%v/dockercli/busybox", privateRegistryURL)
	// tag the image to upload it to the private registry
	dockerCmd(c, "tag", "busybox", repoName)
	// push the image to the registry
	dockerCmd(c, "push", repoName)

	out, _, err := dockerCmdWithError("plugin", "install", repoName)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, `Encountered remote "application/vnd.docker.container.image.v1+json"(image) when fetching`))
}

func (ps *DockerPluginSuite) TestPluginEnableDisableNegative(c *testing.T) {
	pName := ps.getPluginRepoWithTag()

	out, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pName)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), pName))
	out, _, err = dockerCmdWithError("plugin", "enable", pName)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "already enabled"))
	_, _, err = dockerCmdWithError("plugin", "disable", pName)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "disable", pName)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "already disabled"))
	_, _, err = dockerCmdWithError("plugin", "remove", pName)
	assert.NilError(c, err)
}

func (ps *DockerPluginSuite) TestPluginCreate(c *testing.T) {
	name := "foo/bar-driver"
	temp, err := ioutil.TempDir("", "foo")
	assert.NilError(c, err)
	defer os.RemoveAll(temp)

	data := `{"description": "foo plugin"}`
	err = ioutil.WriteFile(filepath.Join(temp, "config.json"), []byte(data), 0644)
	assert.NilError(c, err)

	err = os.MkdirAll(filepath.Join(temp, "rootfs"), 0700)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "create", name, temp)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, name))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, name))
	out, _, err = dockerCmdWithError("plugin", "create", name, temp)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "already exist"))
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, name))
	// The output will consists of one HEADER line and one line of foo/bar-driver
	assert.Equal(c, len(strings.Split(strings.TrimSpace(out), "\n")), 2)
}

func (ps *DockerPluginSuite) TestPluginInspect(c *testing.T) {
	pNameWithTag := ps.getPluginRepoWithTag()

	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pNameWithTag)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, pNameWithTag))
	assert.Assert(c, strings.Contains(out, "true"))
	// Find the ID first
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", pNameWithTag)
	assert.NilError(c, err)
	id := strings.TrimSpace(out)
	assert.Assert(c, id != "")

	// Long form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), id)

	// Short form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id[:5])
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), id)

	// Name with tag form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", pNameWithTag)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), id)

	// Name without tag form
	out, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", ps.getPluginRepo())
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), id)

	_, _, err = dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pNameWithTag)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, pNameWithTag))
	// After remove nothing should be found
	_, _, err = dockerCmdWithError("plugin", "inspect", "-f", "{{.Id}}", id[:5])
	assert.ErrorContains(c, err, "")
}

// Test case for https://github.com/docker/docker/pull/29186#discussion_r91277345
func (s *DockerSuite) TestPluginInspectOnWindows(c *testing.T) {
	// This test should work on Windows only
	testRequires(c, DaemonIsWindows)

	out, _, err := dockerCmdWithError("plugin", "inspect", "foobar")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "plugins are not supported on this platform"))
	assert.ErrorContains(c, err, "plugins are not supported on this platform")
}

func (ps *DockerPluginSuite) TestPluginIDPrefix(c *testing.T) {
	name := "test"
	client := testEnv.APIClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	initialValue := "0"
	err := plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Env = []types.PluginEnv{{Name: "DEBUG", Value: &initialValue, Settable: []string{"value"}}}
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
	assert.Assert(c, strings.Contains(out, name))
	assert.Assert(c, strings.Contains(out, "false"))
	env, _ := dockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", id[:5])
	assert.Equal(c, strings.TrimSpace(env), "[DEBUG=0]")

	dockerCmd(c, "plugin", "set", id[:5], "DEBUG=1")

	env, _ = dockerCmd(c, "plugin", "inspect", "-f", "{{.Settings.Env}}", id[:5])
	assert.Equal(c, strings.TrimSpace(env), "[DEBUG=1]")

	// Enable
	_, _, err = dockerCmdWithError("plugin", "enable", id[:5])
	assert.NilError(c, err)
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, name))
	assert.Assert(c, strings.Contains(out, "true"))
	// Disable
	_, _, err = dockerCmdWithError("plugin", "disable", id[:5])
	assert.NilError(c, err)
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, name))
	assert.Assert(c, strings.Contains(out, "false"))
	// Remove
	_, _, err = dockerCmdWithError("plugin", "remove", id[:5])
	assert.NilError(c, err)
	// List returns none
	out, _, err = dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(out, name))
}

func (ps *DockerPluginSuite) TestPluginListDefaultFormat(c *testing.T) {
	config, err := ioutil.TempDir("", "config-file-")
	assert.NilError(c, err)
	defer os.RemoveAll(config)

	err = ioutil.WriteFile(filepath.Join(config, "config.json"), []byte(`{"pluginsFormat": "raw"}`), 0644)
	assert.NilError(c, err)

	name := "test:latest"
	client := testEnv.APIClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = plugin.Create(ctx, client, name, func(cfg *plugin.Config) {
		cfg.Description = "test plugin"
	})
	assert.Assert(c, err == nil, "failed to create test plugin")

	out, _ := dockerCmd(c, "plugin", "inspect", "--format", "{{.ID}}", name)
	id := strings.TrimSpace(out)

	// We expect the format to be in `raw + --no-trunc`
	expectedOutput := fmt.Sprintf(`plugin_id: %s
name: %s
description: test plugin
enabled: false`, id, name)

	out, _ = dockerCmd(c, "--config", config, "plugin", "ls", "--no-trunc")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), expectedOutput))
}

func (s *DockerSuite) TestPluginUpgrade(c *testing.T) {
	testRequires(c, DaemonIsLinux, Network, testEnv.IsLocalDaemon, IsAmd64, NotUserNamespace)
	plugin := "cpuguy83/docker-volume-driver-plugin-local:latest"
	pluginV2 := "cpuguy83/docker-volume-driver-plugin-local:v2"

	dockerCmd(c, "plugin", "install", "--grant-all-permissions", plugin)
	dockerCmd(c, "volume", "create", "--driver", plugin, "bananas")
	dockerCmd(c, "run", "--rm", "-v", "bananas:/apple", "busybox", "sh", "-c", "touch /apple/core")

	out, _, err := dockerCmdWithError("plugin", "upgrade", "--grant-all-permissions", plugin, pluginV2)
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "disabled before upgrading"))
	out, _ = dockerCmd(c, "plugin", "inspect", "--format={{.ID}}", plugin)
	id := strings.TrimSpace(out)

	// make sure "v2" does not exists
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id, "rootfs", "v2"))
	assert.Assert(c, os.IsNotExist(err), "%s", out)

	dockerCmd(c, "plugin", "disable", "-f", plugin)
	dockerCmd(c, "plugin", "upgrade", "--grant-all-permissions", "--skip-remote-check", plugin, pluginV2)

	// make sure "v2" file exists
	_, err = os.Stat(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "plugins", id, "rootfs", "v2"))
	assert.NilError(c, err)

	dockerCmd(c, "plugin", "enable", plugin)
	dockerCmd(c, "volume", "inspect", "bananas")
	dockerCmd(c, "run", "--rm", "-v", "bananas:/apple", "busybox", "sh", "-c", "ls -lh /apple/core")
}

func (s *DockerSuite) TestPluginMetricsCollector(c *testing.T) {
	testRequires(c, DaemonIsLinux, Network, testEnv.IsLocalDaemon, IsAmd64)
	d := daemon.New(c, dockerBinary, dockerdBinary)
	d.Start(c)
	defer d.Stop(c)

	name := "cpuguy83/docker-metrics-plugin-test:latest"
	r := cli.Docker(cli.Args("plugin", "install", "--grant-all-permissions", name), cli.Daemon(d))
	assert.Assert(c, r.Error == nil, r.Combined())

	// plugin lisens on localhost:19393 and proxies the metrics
	resp, err := http.Get("http://localhost:19393/metrics")
	assert.NilError(c, err)
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	assert.NilError(c, err)
	// check that a known metric is there... don't expect this metric to change over time.. probably safe
	assert.Assert(c, strings.Contains(string(b), "container_actions"))
}

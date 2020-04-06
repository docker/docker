package volumes

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/testutil/daemon"
	"github.com/docker/docker/testutil/fixtures/plugin"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

// TestPluginWithDevMounts tests very specific regression caused by mounts ordering
// (sorted in the daemon). See #36698
func TestPluginWithDevMounts(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	t.Parallel()

	d := daemon.New(t)
	d.Start(t, "--iptables=false")
	defer d.Stop(t)

	c := d.NewClientT(t)
	ctx := context.Background()

	testDir, err := ioutil.TempDir("", "test-dir")
	assert.NilError(t, err)
	defer os.RemoveAll(testDir)

	createPlugin(t, c, "test", "dummy", asVolumeDriver, func(c *plugin.Config) {
		root := "/"
		dev := "/dev"
		mounts := []types.PluginMount{
			{Type: "bind", Source: &root, Destination: "/host", Options: []string{"rbind"}},
			{Type: "bind", Source: &dev, Destination: "/dev", Options: []string{"rbind"}},
			{Type: "bind", Source: &testDir, Destination: "/etc/foo", Options: []string{"rbind"}},
		}
		c.PluginConfig.Mounts = append(c.PluginConfig.Mounts, mounts...)
		c.PropagatedMount = "/propagated"
		c.Network = types.PluginConfigNetwork{Type: "host"}
		c.IpcHost = true
	})

	err = c.PluginEnable(ctx, "test", types.PluginEnableOptions{Timeout: 30})
	assert.NilError(t, err)
	defer func() {
		err := c.PluginRemove(ctx, "test", types.PluginRemoveOptions{Force: true})
		assert.Check(t, err)
	}()

	p, _, err := c.PluginInspectWithRaw(ctx, "test")
	assert.NilError(t, err)
	assert.Assert(t, p.Enabled)
}

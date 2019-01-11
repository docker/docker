package volumes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/internal/test/fixtures/plugin"
	"github.com/docker/docker/pkg/locker"
	"github.com/pkg/errors"
)

var pluginBuildLock = locker.New()

// ensurePlugin makes the that a plugin binary has been installed on the system.
// Plugins that have not been installed are built from `cmd/<name>`.
func ensurePlugin(t *testing.T, name string) string {
	pluginBuildLock.Lock(name)
	defer pluginBuildLock.Unlock(name)

	goPath := os.Getenv("GOPATH")
	if goPath == "" {
		goPath = "/go"
	}
	installPath := filepath.Join(goPath, "bin", name)
	if _, err := os.Stat(installPath); err == nil {
		return installPath
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(goBin, "build", "-tags", "netgo", "-o", installPath, "./"+filepath.Join("cmd", name))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(errors.Wrapf(err, "error building basic plugin bin: %s", string(out)))
	}

	return installPath
}

func withSockPath(name string) func(*plugin.Config) {
	return func(cfg *plugin.Config) {
		cfg.Interface.Socket = name
	}
}

func createPlugin(t *testing.T, client plugin.CreateClient, alias, bin string, opts ...plugin.CreateOpt) {
	pluginBin := ensurePlugin(t, bin)

	opts = append(opts, withSockPath("plugin.sock"))
	opts = append(opts, plugin.WithBinary(pluginBin))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	err := plugin.Create(ctx, client, alias, opts...)
	cancel()

	if err != nil {
		t.Fatal(err)
	}
}

func asVolumeDriver(cfg *plugin.Config) {
	cfg.Interface.Types = []types.PluginInterfaceType{
		{Capability: "volumedriver", Prefix: "docker", Version: "1.0"},
	}
}

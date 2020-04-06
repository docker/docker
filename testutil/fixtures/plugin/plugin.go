package plugin // import "github.com/docker/docker/testutil/fixtures/plugin"

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/plugin"
	"github.com/docker/docker/registry"
	"github.com/pkg/errors"
)

// CreateOpt is passed used to change the default plugin config before
// creating it
type CreateOpt func(*Config)

// Config wraps types.PluginConfig to provide some extra state for options
// extra customizations on the plugin details, such as using a custom binary to
// create the plugin with.
type Config struct {
	*types.PluginConfig
	binPath string
}

// WithBinary is a CreateOpt to set an custom binary to create the plugin with.
// This binary must be statically compiled.
func WithBinary(bin string) CreateOpt {
	return func(cfg *Config) {
		cfg.binPath = bin
	}
}

// CreateClient is the interface used for `BuildPlugin` to interact with the
// daemon.
type CreateClient interface {
	PluginCreate(context.Context, io.Reader, types.PluginCreateOptions) error
}

// Create creates a new plugin with the specified name
func Create(ctx context.Context, c CreateClient, name string, opts ...CreateOpt) error {
	tmpDir, err := ioutil.TempDir("", "create-test-plugin")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tar, err := makePluginBundle(tmpDir, opts...)
	if err != nil {
		return err
	}
	defer tar.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return c.PluginCreate(ctx, tar, types.PluginCreateOptions{RepoName: name})
}

// CreateInRegistry makes a plugin (locally) and pushes it to a registry.
// This does not use a dockerd instance to create or push the plugin.
// If you just want to create a plugin in some daemon, use `Create`.
//
// This can be useful when testing plugins on swarm where you don't really want
// the plugin to exist on any of the daemons (immediately) and there needs to be
// some way to distribute the plugin.
func CreateInRegistry(ctx context.Context, repo string, auth *types.AuthConfig, opts ...CreateOpt) error {
	tmpDir, err := ioutil.TempDir("", "create-test-plugin-local")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	inPath := filepath.Join(tmpDir, "plugin")
	if err := os.MkdirAll(inPath, 0755); err != nil {
		return errors.Wrap(err, "error creating plugin root")
	}

	tar, err := makePluginBundle(inPath, opts...)
	if err != nil {
		return err
	}
	defer tar.Close()

	dummyExec := func(m *plugin.Manager) (plugin.Executor, error) {
		return nil, nil
	}

	regService, err := registry.NewService(registry.ServiceOptions{})
	if err != nil {
		return err
	}

	managerConfig := plugin.ManagerConfig{
		Store:           plugin.NewStore(),
		RegistryService: regService,
		Root:            filepath.Join(tmpDir, "root"),
		ExecRoot:        "/run/docker", // manager init fails if not set
		CreateExecutor:  dummyExec,
		LogPluginEvent:  func(id, name, action string) {}, // panics when not set
	}
	manager, err := plugin.NewManager(managerConfig)
	if err != nil {
		return errors.Wrap(err, "error creating plugin manager")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := manager.CreateFromContext(ctx, tar, &types.PluginCreateOptions{RepoName: repo}); err != nil {
		return err
	}

	if auth == nil {
		auth = &types.AuthConfig{}
	}
	err = manager.Push(ctx, repo, nil, auth, ioutil.Discard)
	return errors.Wrap(err, "error pushing plugin")
}

func makePluginBundle(inPath string, opts ...CreateOpt) (io.ReadCloser, error) {
	p := &types.PluginConfig{
		Interface: types.PluginConfigInterface{
			Socket: "basic.sock",
			Types:  []types.PluginInterfaceType{{Capability: "docker.dummy/1.0"}},
		},
		Entrypoint: []string{"/basic"},
	}
	cfg := &Config{
		PluginConfig: p,
	}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.binPath == "" {
		binPath, err := ensureBasicPluginBin()
		if err != nil {
			return nil, err
		}
		cfg.binPath = binPath
	}

	configJSON, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if err := ioutil.WriteFile(filepath.Join(inPath, "config.json"), configJSON, 0644); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(inPath, "rootfs", filepath.Dir(p.Entrypoint[0])), 0755); err != nil {
		return nil, errors.Wrap(err, "error creating plugin rootfs dir")
	}

	// Ensure the mount target paths exist
	for _, m := range p.Mounts {
		var stat os.FileInfo
		if m.Source != nil {
			stat, err = os.Stat(*m.Source)
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}

		if stat == nil || stat.IsDir() {
			var mode os.FileMode = 0755
			if stat != nil {
				mode = stat.Mode()
			}
			if err := os.MkdirAll(filepath.Join(inPath, "rootfs", m.Destination), mode); err != nil {
				return nil, errors.Wrap(err, "error preparing plugin mount destination path")
			}
		} else {
			if err := os.MkdirAll(filepath.Join(inPath, "rootfs", filepath.Dir(m.Destination)), 0755); err != nil {
				return nil, errors.Wrap(err, "error preparing plugin mount destination dir")
			}
			f, err := os.Create(filepath.Join(inPath, "rootfs", m.Destination))
			if err != nil && !os.IsExist(err) {
				return nil, errors.Wrap(err, "error preparing plugin mount destination file")
			}
			if f != nil {
				f.Close()
			}
		}
	}
	if err := archive.NewDefaultArchiver().CopyFileWithTar(cfg.binPath, filepath.Join(inPath, "rootfs", p.Entrypoint[0])); err != nil {
		return nil, errors.Wrap(err, "error copying plugin binary to rootfs path")
	}
	tar, err := archive.Tar(inPath, archive.Uncompressed)
	return tar, errors.Wrap(err, "error making plugin archive")
}

func ensureBasicPluginBin() (string, error) {
	name := "docker-basic-plugin"
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}
	installPath := filepath.Join(os.Getenv("GOPATH"), "bin", name)
	sourcePath := filepath.Join("github.com", "docker", "docker", "testutil", "fixtures", "plugin", "basic")
	cmd := exec.Command(goBin, "build", "-o", installPath, sourcePath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.Wrapf(err, "error building basic plugin bin: %s", string(out))
	}
	return installPath, nil
}

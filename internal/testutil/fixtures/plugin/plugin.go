package plugin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/moby/go-archive"
	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/api/types/events"
	plugintypes "github.com/moby/moby/api/types/plugin"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/pkg/plugin"
	registrypkg "github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/pkg/errors"
)

// CreateOpt is passed used to change the default plugin config before
// creating it
type CreateOpt func(*Config)

// Config wraps [pluginapi.Config] to provide some extra state for options
// extra customizations on the plugin details, such as using a custom binary to
// create the plugin with.
type Config struct {
	*plugintypes.Config
	binPath        string
	RegistryConfig registrypkg.ServiceOptions
}

// WithInsecureRegistry specifies that the given registry can skip host-key checking as well as fall back to plain http
func WithInsecureRegistry(url string) CreateOpt {
	return func(cfg *Config) {
		cfg.RegistryConfig.InsecureRegistries = append(cfg.RegistryConfig.InsecureRegistries, url)
	}
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
	PluginCreate(context.Context, io.Reader, client.PluginCreateOptions) (client.PluginCreateResult, error)
}

// Create creates a new plugin with the specified name
func Create(ctx context.Context, c CreateClient, name string, opts ...CreateOpt) error {
	tmpDir, err := os.MkdirTemp("", "create-test-plugin")
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

	_, err = c.PluginCreate(ctx, tar, client.PluginCreateOptions{RepoName: name})
	return err
}

// CreateInRegistry makes a plugin (locally) and pushes it to a registry.
// This does not use a dockerd instance to create or push the plugin.
// If you just want to create a plugin in some daemon, use `Create`.
//
// This can be useful when testing plugins on swarm where you don't really want
// the plugin to exist on any of the daemons (immediately) and there needs to be
// some way to distribute the plugin.
func CreateInRegistry(ctx context.Context, repo string, auth *registry.AuthConfig, opts ...CreateOpt) error {
	tmpDir, err := os.MkdirTemp("", "create-test-plugin-local")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	inPath := filepath.Join(tmpDir, "plugin")
	if err := os.MkdirAll(inPath, 0o755); err != nil {
		return errors.Wrap(err, "error creating plugin root")
	}

	cfg := Config{
		Config: &plugintypes.Config{},
	}
	for _, o := range opts {
		o(&cfg)
	}

	tar, err := makePluginBundle(inPath, opts...)
	if err != nil {
		return err
	}
	defer tar.Close()

	dummyExec := func(m *plugin.Manager) (plugin.Executor, error) {
		return nil, nil
	}

	regService, err := registrypkg.NewService(cfg.RegistryConfig)
	if err != nil {
		return err
	}

	manager, err := plugin.NewManager(plugin.ManagerConfig{
		Store:           plugin.NewStore(),
		RegistryService: regService,
		Root:            filepath.Join(tmpDir, "root"),
		ExecRoot:        "/run/docker", // manager init fails if not set
		CreateExecutor:  dummyExec,
		LogPluginEvent:  func(id, name string, action events.Action) {}, // panics when not set
	})
	if err != nil {
		return errors.Wrap(err, "error creating plugin manager")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := manager.CreateFromContext(ctx, tar, &backend.PluginCreateConfig{RepoName: repo}); err != nil {
		return err
	}

	if auth == nil {
		auth = &registry.AuthConfig{}
	}
	err = manager.Push(ctx, repo, nil, auth, io.Discard)
	return errors.Wrap(err, "error pushing plugin")
}

func makePluginBundle(inPath string, opts ...CreateOpt) (io.ReadCloser, error) {
	p := &plugintypes.Config{
		Interface: plugintypes.Interface{
			Socket: "basic.sock",
			Types:  []plugintypes.CapabilityID{{Prefix: "docker", Capability: "dummy", Version: "1.0"}},
		},
		Entrypoint: []string{"/basic"},
	}
	cfg := &Config{
		Config: p,
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
	if err := os.WriteFile(filepath.Join(inPath, "config.json"), configJSON, 0o644); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(inPath, "rootfs", filepath.Dir(p.Entrypoint[0])), 0o755); err != nil {
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
			var mode os.FileMode = 0o755
			if stat != nil {
				mode = stat.Mode()
			}
			if err := os.MkdirAll(filepath.Join(inPath, "rootfs", m.Destination), mode); err != nil {
				return nil, errors.Wrap(err, "error preparing plugin mount destination path")
			}
		} else {
			if err := os.MkdirAll(filepath.Join(inPath, "rootfs", filepath.Dir(m.Destination)), 0o755); err != nil {
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
	tar, err := archive.Tar(inPath, compression.None)
	return tar, errors.Wrap(err, "error making plugin archive")
}

var ensureBasicPluginBin = sync.OnceValues(buildBasicPluginBin)

func buildBasicPluginBin() (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", err
	}

	tmpDir := filepath.Join(os.TempDir(), "moby-plugin-fixtures")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", errors.Wrap(err, "error creating plugin fixtures dir")
	}
	installPath := filepath.Join(tmpDir, "docker-basic-plugin")
	cmd := exec.Command(goBin, "build", "-buildvcs=false", "-o", installPath, "github.com/moby/moby/v2/internal/testutil/fixtures/plugin/basic")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=auto")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.Wrapf(err, "error building basic plugin bin: %s", string(out))
	}
	return installPath, nil
}

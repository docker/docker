package plugin // import "github.com/docker/docker/plugin"

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	reference "github.com/containerd/containerd/reference/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/authorization"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/pubsub"
	"github.com/docker/docker/pkg/system"
	v2 "github.com/docker/docker/plugin/v2"
	"github.com/docker/docker/registry"
	"github.com/moby/sys/mount"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const configFileName = "config.json"
const rootFSFileName = "rootfs"

var validFullID = regexp.MustCompile(`^([a-f0-9]{64})$`)

// Executor is the interface that the plugin manager uses to interact with for starting/stopping plugins
type Executor interface {
	Create(id string, spec specs.Spec, stdout, stderr io.WriteCloser) error
	IsRunning(id string) (bool, error)
	Restore(id string, stdout, stderr io.WriteCloser) (alive bool, err error)
	Signal(id string, signal int) error
}

func (pm *Manager) restorePlugin(p *v2.Plugin, c *controller) error {
	if p.IsEnabled() {
		return pm.restore(p, c)
	}
	return nil
}

type eventLogger func(id, name, action string)

// ManagerConfig defines configuration needed to start new manager.
type ManagerConfig struct {
	Store              *Store // remove
	RegistryService    registry.Service
	LiveRestoreEnabled bool // TODO: remove
	LogPluginEvent     eventLogger
	Root               string
	ExecRoot           string
	CreateExecutor     ExecutorCreator
	AuthzMiddleware    *authorization.Middleware
}

// ExecutorCreator is used in the manager config to pass in an `Executor`
type ExecutorCreator func(*Manager) (Executor, error)

// Manager controls the plugin subsystem.
type Manager struct {
	config    ManagerConfig
	mu        sync.RWMutex // protects cMap
	muGC      sync.RWMutex // protects blobstore deletions
	cMap      map[*v2.Plugin]*controller
	blobStore content.Store
	publisher *pubsub.Publisher
	executor  Executor
}

// controller represents the manager's control on a plugin.
type controller struct {
	restart       bool
	exitChan      chan bool
	timeoutInSecs int
}

// pluginRegistryService ensures that all resolved repositories
// are of the plugin class.
type pluginRegistryService struct {
	registry.Service
}

func (s pluginRegistryService) ResolveRepository(name reference.Named) (repoInfo *registry.RepositoryInfo, err error) {
	repoInfo, err = s.Service.ResolveRepository(name)
	if repoInfo != nil {
		repoInfo.Class = "plugin"
	}
	return
}

// NewManager returns a new plugin manager.
func NewManager(config ManagerConfig) (*Manager, error) {
	if config.RegistryService != nil {
		config.RegistryService = pluginRegistryService{config.RegistryService}
	}
	manager := &Manager{
		config: config,
	}
	for _, dirName := range []string{manager.config.Root, manager.config.ExecRoot, manager.tmpDir()} {
		if err := os.MkdirAll(dirName, 0700); err != nil {
			return nil, errors.Wrapf(err, "failed to mkdir %v", dirName)
		}
	}
	var err error
	manager.executor, err = config.CreateExecutor(manager)
	if err != nil {
		return nil, err
	}

	manager.blobStore, err = local.NewStore(filepath.Join(manager.config.Root, "storage"))
	if err != nil {
		return nil, errors.Wrap(err, "error creating plugin blob store")
	}

	manager.cMap = make(map[*v2.Plugin]*controller)
	if err := manager.reload(); err != nil {
		return nil, errors.Wrap(err, "failed to restore plugins")
	}

	manager.publisher = pubsub.NewPublisher(0, 0)
	return manager, nil
}

func (pm *Manager) tmpDir() string {
	return filepath.Join(pm.config.Root, "tmp")
}

// HandleExitEvent is called when the executor receives the exit event
// In the future we may change this, but for now all we care about is the exit event.
func (pm *Manager) HandleExitEvent(id string) error {
	p, err := pm.config.Store.GetV2Plugin(id)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(filepath.Join(pm.config.ExecRoot, id)); err != nil && !os.IsNotExist(err) {
		logrus.WithError(err).WithField("id", id).Error("Could not remove plugin bundle dir")
	}

	pm.mu.RLock()
	c := pm.cMap[p]
	if c.exitChan != nil {
		close(c.exitChan)
		c.exitChan = nil // ignore duplicate events (containerd issue #2299)
	}
	restart := c.restart
	pm.mu.RUnlock()

	if restart {
		pm.enable(p, c, true)
	} else {
		if err := mount.RecursiveUnmount(filepath.Join(pm.config.Root, id)); err != nil {
			return errors.Wrap(err, "error cleaning up plugin mounts")
		}
	}
	return nil
}

func handleLoadError(err error, id string) {
	if err == nil {
		return
	}
	logger := logrus.WithError(err).WithField("id", id)
	if errors.Is(err, os.ErrNotExist) {
		// Likely some error while removing on an older version of docker
		logger.Warn("missing plugin config, skipping: this may be caused due to a failed remove and requires manual cleanup.")
		return
	}
	logger.Error("error loading plugin, skipping")
}

func (pm *Manager) reload() error { // todo: restore
	dir, err := ioutil.ReadDir(pm.config.Root)
	if err != nil {
		return errors.Wrapf(err, "failed to read %v", pm.config.Root)
	}
	plugins := make(map[string]*v2.Plugin)
	for _, v := range dir {
		if validFullID.MatchString(v.Name()) {
			p, err := pm.loadPlugin(v.Name())
			if err != nil {
				handleLoadError(err, v.Name())
				continue
			}
			plugins[p.GetID()] = p
		} else {
			if validFullID.MatchString(strings.TrimSuffix(v.Name(), "-removing")) {
				// There was likely some error while removing this plugin, let's try to remove again here
				if err := system.EnsureRemoveAll(v.Name()); err != nil {
					logrus.WithError(err).WithField("id", v.Name()).Warn("error while attempting to clean up previously removed plugin")
				}
			}
		}
	}

	pm.config.Store.SetAll(plugins)

	var wg sync.WaitGroup
	wg.Add(len(plugins))
	for _, p := range plugins {
		c := &controller{exitChan: make(chan bool)}
		pm.mu.Lock()
		pm.cMap[p] = c
		pm.mu.Unlock()

		go func(p *v2.Plugin) {
			defer wg.Done()
			if err := pm.restorePlugin(p, c); err != nil {
				logrus.WithError(err).WithField("id", p.GetID()).Error("Failed to restore plugin")
				return
			}

			if p.Rootfs != "" {
				p.Rootfs = filepath.Join(pm.config.Root, p.PluginObj.ID, "rootfs")
			}

			// We should only enable rootfs propagation for certain plugin types that need it.
			for _, typ := range p.PluginObj.Config.Interface.Types {
				if (typ.Capability == "volumedriver" || typ.Capability == "graphdriver") && typ.Prefix == "docker" && strings.HasPrefix(typ.Version, "1.") {
					if p.PluginObj.Config.PropagatedMount != "" {
						propRoot := filepath.Join(filepath.Dir(p.Rootfs), "propagated-mount")

						// check if we need to migrate an older propagated mount from before
						// these mounts were stored outside the plugin rootfs
						if _, err := os.Stat(propRoot); os.IsNotExist(err) {
							rootfsProp := filepath.Join(p.Rootfs, p.PluginObj.Config.PropagatedMount)
							if _, err := os.Stat(rootfsProp); err == nil {
								if err := os.Rename(rootfsProp, propRoot); err != nil {
									logrus.WithError(err).WithField("dir", propRoot).Error("error migrating propagated mount storage")
								}
							}
						}

						if err := os.MkdirAll(propRoot, 0755); err != nil {
							logrus.Errorf("failed to create PropagatedMount directory at %s: %v", propRoot, err)
						}
					}
				}
			}

			pm.save(p)
			requiresManualRestore := !pm.config.LiveRestoreEnabled && p.IsEnabled()

			if requiresManualRestore {
				// if liveRestore is not enabled, the plugin will be stopped now so we should enable it
				if err := pm.enable(p, c, true); err != nil {
					logrus.WithError(err).WithField("id", p.GetID()).Error("failed to enable plugin")
				}
			}
		}(p)
	}
	wg.Wait()
	return nil
}

// Get looks up the requested plugin in the store.
func (pm *Manager) Get(idOrName string) (*v2.Plugin, error) {
	return pm.config.Store.GetV2Plugin(idOrName)
}

func (pm *Manager) loadPlugin(id string) (*v2.Plugin, error) {
	p := filepath.Join(pm.config.Root, id, configFileName)
	dt, err := ioutil.ReadFile(p)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading %v", p)
	}
	var plugin v2.Plugin
	if err := json.Unmarshal(dt, &plugin); err != nil {
		return nil, errors.Wrapf(err, "error decoding %v", p)
	}
	return &plugin, nil
}

func (pm *Manager) save(p *v2.Plugin) error {
	pluginJSON, err := json.Marshal(p)
	if err != nil {
		return errors.Wrap(err, "failed to marshal plugin json")
	}
	if err := ioutils.AtomicWriteFile(filepath.Join(pm.config.Root, p.GetID(), configFileName), pluginJSON, 0600); err != nil {
		return errors.Wrap(err, "failed to write atomically plugin json")
	}
	return nil
}

// GC cleans up unreferenced blobs. This is recommended to run in a goroutine
func (pm *Manager) GC() {
	pm.muGC.Lock()
	defer pm.muGC.Unlock()

	whitelist := make(map[digest.Digest]struct{})
	for _, p := range pm.config.Store.GetAll() {
		whitelist[p.Config] = struct{}{}
		for _, b := range p.Blobsums {
			whitelist[b] = struct{}{}
		}
	}

	ctx := context.TODO()
	pm.blobStore.Walk(ctx, func(info content.Info) error {
		_, ok := whitelist[info.Digest]
		if ok {
			return nil
		}

		return pm.blobStore.Delete(ctx, info.Digest)
	})
}

type logHook struct{ id string }

func (logHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (l logHook) Fire(entry *logrus.Entry) error {
	entry.Data = logrus.Fields{"plugin": l.id}
	return nil
}

func makeLoggerStreams(id string) (stdout, stderr io.WriteCloser) {
	logger := logrus.New()
	logger.Hooks.Add(logHook{id})
	return logger.WriterLevel(logrus.InfoLevel), logger.WriterLevel(logrus.ErrorLevel)
}

func validatePrivileges(requiredPrivileges, privileges types.PluginPrivileges) error {
	if !isEqual(requiredPrivileges, privileges, isEqualPrivilege) {
		return errors.New("incorrect privileges")
	}

	return nil
}

func isEqual(arrOne, arrOther types.PluginPrivileges, compare func(x, y types.PluginPrivilege) bool) bool {
	if len(arrOne) != len(arrOther) {
		return false
	}

	sort.Sort(arrOne)
	sort.Sort(arrOther)

	for i := 1; i < arrOne.Len(); i++ {
		if !compare(arrOne[i], arrOther[i]) {
			return false
		}
	}

	return true
}

func isEqualPrivilege(a, b types.PluginPrivilege) bool {
	if a.Name != b.Name {
		return false
	}

	return reflect.DeepEqual(a.Value, b.Value)
}

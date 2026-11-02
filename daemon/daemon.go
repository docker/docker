// Package daemon exposes the functions that occur on the host server
// that the Docker daemon is running.
//
// In implementing the various functions of the daemon, there is often
// a method-specific struct for configuring the runtime behavior.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	dist "github.com/docker/distribution"
	"github.com/docker/go-units"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/locker"
	containertypes "github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	registrytypes "github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/daemon/internal/nri"
	"github.com/moby/sys/user"
	"github.com/moby/sys/userns"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"resenje.org/singleflight"
	"tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/moby/extensions/host"
	"github.com/moby/moby/v2/daemon/builder"
	executorpkg "github.com/moby/moby/v2/daemon/cluster/executor"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	ctrd "github.com/moby/moby/v2/daemon/containerd"
	"github.com/moby/moby/v2/daemon/containerd/identitycache"
	"github.com/moby/moby/v2/daemon/containerd/migration"
	"github.com/moby/moby/v2/daemon/events"
	_ "github.com/moby/moby/v2/daemon/graphdriver/register" // register graph drivers
	"github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/distribution"
	dmetadata "github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/idtools"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/libcontainerd"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	pluginexec "github.com/moby/moby/v2/daemon/internal/plugin/executor/containerd"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/libnetwork/cluster"
	nwconfig "github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/ipbits"
	"github.com/moby/moby/v2/daemon/libnetwork/kvstore/boltdb"
	dlogger "github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/pkg/plugin"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/snapshotter"
	"github.com/moby/moby/v2/daemon/stats"
	volumesservice "github.com/moby/moby/v2/daemon/volume/service"
	"github.com/moby/moby/v2/dockerversion"
	"github.com/moby/moby/v2/pkg/authorization"
	"github.com/moby/moby/v2/pkg/meminfo"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"github.com/moby/moby/v2/pkg/sysinfo"
	policyverifier "github.com/moby/policy-helpers"
)

type configStore struct {
	config.Config

	Runtimes runtimes
}

// ContainerdDialer dials the in-process containerd over an in-memory pipe.
//
// It is set in embedded mode, so the daemon's own client can skip socket
// syscalls, and is nil otherwise. The signature matches grpc.WithContextDialer,
// and the address argument is ignored.
type ContainerdDialer = func(ctx context.Context, addr string) (net.Conn, error)

// Daemon holds information about the Docker daemon.
type Daemon struct {
	id                string
	repository        string
	containers        container.Store
	containersReplica *container.ViewDB
	execCommands      *container.ExecStore
	imageService      ImageService
	configStore       atomic.Pointer[configStore]
	configReload      sync.Mutex
	statsCollector    *stats.Collector
	defaultLogConfig  containertypes.LogConfig
	registryService   *registry.Service
	EventsService     *events.Events
	netController     *libnetwork.Controller
	netStore          *boltdb.BoltDB
	ociHooks          *ocihook.Server
	volumes           *volumesservice.VolumesService
	root              string
	sysInfoOnce       sync.Once
	sysInfo           *sysinfo.SysInfo
	sysInfoErr        error
	shutdown          bool
	idMapping         user.IdentityMapping
	PluginStore       *plugin.Store // TODO: remove
	nri               *nri.NRI
	pluginManager     *plugin.Manager
	extensionHost     *host.Host
	linkIndex         *linkIndex
	containerdClient  *containerd.Client
	containerd        libcontainerdtypes.Client
	defaultIsolation  containertypes.Isolation // Default isolation mode on Windows
	clusterProvider   cluster.Provider
	cluster           Cluster
	genericResources  []swarm.GenericResource
	ReferenceStore    refstore.Store

	machineMemory uint64

	seccompProfile      []byte
	seccompProfilePath  string
	appArmorProfile     *template.Template
	appArmorProfilePath string

	usageContainers singleflight.Group[bool, *backend.ContainerDiskUsage]
	usageImages     singleflight.Group[bool, *backend.ImageDiskUsage]
	usageVolumes    singleflight.Group[bool, *backend.VolumeDiskUsage]
	usageLayer      singleflight.Group[struct{}, int64]

	pruneRunning atomic.Bool
	hosts        map[string]bool // hosts stores the addresses the daemon is listening on
	startupDone  chan struct{}

	attachmentStore       network.AttachmentStore
	attachableNetworkLock *locker.Locker

	// This is used for Windows which doesn't currently support running on containerd
	// It stores metadata for the content store (used for manifest caching)
	// This needs to be closed on daemon exit
	mdDB *bolt.DB

	usesSnapshotter bool

	CDICache *cdi.Cache
}

// ID returns the daemon id
func (daemon *Daemon) ID() string {
	return daemon.id
}

// StoreHosts stores the addresses the daemon is listening on
func (daemon *Daemon) StoreHosts(hosts []string) {
	if daemon.hosts == nil {
		daemon.hosts = make(map[string]bool)
	}
	for _, h := range hosts {
		daemon.hosts[h] = true
	}
}

// config returns an immutable snapshot of the current daemon configuration.
// Multiple calls to this function will return the same pointer until the
// configuration is reloaded so callers must take care not to modify the
// returned value.
//
// To ensure that the configuration used remains consistent throughout the
// lifetime of an operation, the configuration pointer should be passed down the
// call stack, like one would a [context.Context] value. Only the entrypoints
// for operations, the outermost functions, should call this function.
func (daemon *Daemon) config() *configStore {
	cfg := daemon.configStore.Load()
	if cfg == nil {
		return &configStore{}
	}
	return cfg
}

// Config returns daemon's config.
func (daemon *Daemon) Config() config.Config {
	return daemon.config().Config
}

// HasExperimental returns whether the experimental features of the daemon are enabled or not
func (daemon *Daemon) HasExperimental() bool {
	return daemon.config().Experimental
}

// Features returns the features map from configStore
func (daemon *Daemon) Features() map[string]bool {
	return daemon.config().Features
}

// UsesSnapshotter returns true if feature flag to use containerd snapshotter is enabled
func (daemon *Daemon) UsesSnapshotter() bool {
	return daemon.usesSnapshotter
}

// DefaultIsolation returns the default isolation mode for the daemon to run in (only applicable on Windows).
func (daemon *Daemon) DefaultIsolation() containertypes.Isolation {
	return daemon.defaultIsolation
}

func (daemon *Daemon) loadContainers(ctx context.Context) (map[string]map[string]*container.Container, error) {
	var mapLock sync.Mutex
	driverContainers := make(map[string]map[string]*container.Container)

	log.G(ctx).Info("Loading containers: start.")

	dir, err := os.ReadDir(daemon.repository)
	if err != nil {
		return nil, err
	}

	// parallelLimit is the maximum number of parallel startup jobs that we
	// allow (this is the limited used for all startup semaphores). The multiplier
	// (128) was chosen after some fairly significant benchmarking -- don't change
	// it unless you've tested it significantly (this value is adjusted if
	// RLIMIT_NOFILE is small to avoid EMFILE).
	parallelLimit := adjustParallelLimit(len(dir), 128*runtime.NumCPU())

	// Re-used for all parallel startup jobs.
	var group sync.WaitGroup
	sem := semaphore.NewWeighted(int64(parallelLimit))

	for _, v := range dir {
		if err := ctx.Err(); err != nil {
			// Wait for already-started loads before returning.
			group.Wait()
			return nil, err
		}

		id := v.Name()
		group.Go(func() {
			if err := sem.Acquire(ctx, 1); err != nil {
				// ctx is done.
				return
			}
			defer sem.Release(1)

			c, err := daemon.load(id)
			if err != nil {
				log.G(ctx).WithFields(log.Fields{"error": err, "container": id}).Error("Failed to load container")
				return
			}
			if c.ProcessLabel != "" {
				if err := selinux.ReserveLabelV2(c.ProcessLabel); err != nil {
					// Don't treat this as a fatal error to preserve existing
					// behavior, and because this is restoring existing state,
					// so there's no practical way to resolve this.
					log.G(ctx).WithFields(log.Fields{"error": err, "container": id}).Error("Failed to reserve SELinux label during load")
				}
			}

			mapLock.Lock()
			if containers, ok := driverContainers[c.Driver]; !ok {
				driverContainers[c.Driver] = map[string]*container.Container{
					c.ID: c,
				}
			} else {
				containers[c.ID] = c
			}
			mapLock.Unlock()
		})
	}
	group.Wait()

	// A canceled acquire may have skipped one or more container loads.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return driverContainers, nil
}

func (daemon *Daemon) restore(ctx context.Context, cfg *configStore, containers map[string]*container.Container) error {
	var mapLock sync.Mutex

	log.G(ctx).Info("Restoring containers: start.")

	// parallelLimit is the maximum number of parallel startup jobs that we
	// allow (this is the limited used for all startup semaphores). The multiplier
	// (128) was chosen after some fairly significant benchmarking -- don't change
	// it unless you've tested it significantly (this value is adjusted if
	// RLIMIT_NOFILE is small to avoid EMFILE).
	parallelLimit := adjustParallelLimit(len(containers), 128*runtime.NumCPU())

	// Re-used for all parallel startup jobs.
	var group sync.WaitGroup
	sem := semaphore.NewWeighted(int64(parallelLimit))

	removeContainers := make(map[string]*container.Container)
	restartContainers := make(map[*container.Container]chan struct{})
	activeSandboxes := make(map[string]any)

	for _, c := range containers {
		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			logger := log.G(ctx).WithField("container", c.ID)

			rwlayer, err := daemon.imageService.GetLayerByID(c.ID)
			if err != nil {
				// A container without a rwlayer is in a bad state, but we must register that container to let users
				// remove it. So, log the error but do not early-return.
				logger.WithError(err).Error("failed to load container mount")
			} else {
				c.RWLayer = rwlayer
			}
			logger.WithFields(log.Fields{
				"running": c.State.IsRunning(),
				"paused":  c.State.IsPaused(),
			}).Debug("loaded container")

			if err := daemon.registerName(c); err != nil {
				logger.WithError(err).Errorf("failed to register container name: %s", c.Name)
				mapLock.Lock()
				delete(containers, c.ID)
				mapLock.Unlock()
				return
			}
			if err := daemon.register(ctx, c); err != nil {
				logger.WithError(err).Error("failed to register container")
				mapLock.Lock()
				delete(containers, c.ID)
				mapLock.Unlock()
				return
			}
		})
	}
	group.Wait()

	for _, c := range containers {
		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			baseLogger := log.G(ctx).WithField("container", c.ID)

			// Fill in missing platform information with platform from image for older containers
			// Remove this in a future release
			if c.ImagePlatform.Architecture == "" {
				migration := daemonPlatformReader{
					imageService: daemon.imageService,
				}
				if daemon.containerdClient != nil {
					migration.content = daemon.containerdClient.ContentStore()
				}
				migrateContainerOS(ctx, migration, c)
			}

			if c.HostConfig != nil {
				// Migrate containers that don't have the default ("no") restart-policy set.
				// The RestartPolicy.Name field may be empty for containers that were
				// created with versions before v25.0.0.
				//
				// We also need to set the MaximumRetryCount to 0, to prevent
				// validation from failing (MaximumRetryCount is not allowed if
				// no restart-policy ("none") is set).
				if c.HostConfig.RestartPolicy.Name == "" {
					baseLogger.Debug("migrated restart-policy")
					c.HostConfig.RestartPolicy.Name = containertypes.RestartPolicyDisabled
					c.HostConfig.RestartPolicy.MaximumRetryCount = 0
				}

				// Normalize the "default" network mode into the network mode
				// it aliases ("bridge on Linux and "nat" on Windows). This is
				// also done by the container router, for new containers. But
				// we need to do it here too to handle containers that were
				// created prior to v26.0.
				//
				// TODO(aker): remove this migration code once the next LTM version of MCR is released.
				if c.HostConfig.NetworkMode.IsDefault() {
					c.HostConfig.NetworkMode = network.DefaultNetwork
					if nw, ok := c.NetworkSettings.Networks[networktypes.NetworkDefault]; ok {
						c.NetworkSettings.Networks[c.HostConfig.NetworkMode.NetworkName()] = nw
						delete(c.NetworkSettings.Networks, networktypes.NetworkDefault)
					}
				}

				// The logger option 'fluentd-async-connect' has been
				// deprecated in v20.10 in favor of 'fluentd-async', and
				// removed in v28.0.
				// TODO(aker): remove this migration once the next LTS version of MCR is released.
				if v, ok := c.HostConfig.LogConfig.Config["fluentd-async-connect"]; ok {
					if _, ok := c.HostConfig.LogConfig.Config["fluentd-async"]; !ok {
						c.HostConfig.LogConfig.Config["fluentd-async"] = v
					}
					delete(c.HostConfig.LogConfig.Config, "fluentd-async-connect")
				}
				if len(c.HostConfig.ExtraHosts) > 0 {
					// Daemon versions before v29.0.0 were more permissive when handling whitespace in the IP-address:
					//
					// See https://github.com/moby/moby/issues/52274
					// See https://github.com/moby/moby/pull/50956
					//
					// TODO(thaJeztah): remove this migration when we no longer need migration for docker < v29.0.0
					for i, h := range c.HostConfig.ExtraHosts {
						if host, ip, ok := strings.Cut(h, ":"); ok {
							c.HostConfig.ExtraHosts[i] = strings.TrimSpace(host) + ":" + strings.TrimSpace(ip)
						}
					}
				}
			}

			if err := daemon.checkpointAndSave(c); err != nil {
				baseLogger.WithError(err).Error("failed to save migrated container config to disk")
			}

			daemon.setStateCounter(c)

			logger := func(c *container.Container) *log.Entry {
				return baseLogger.WithFields(log.Fields{
					"running":    c.State.IsRunning(),
					"paused":     c.State.IsPaused(),
					"restarting": c.State.IsRestarting(),
				})
			}

			logger(c).Debug("restoring container")

			var es *containerd.ExitStatus

			if err := c.RestoreTask(context.Background(), daemon.containerd); err != nil && !cerrdefs.IsNotFound(err) {
				logger(c).WithError(err).Error("failed to restore container with containerd")
				return
			}

			alive := false
			status := containerd.Unknown
			if tsk, ok := c.State.Task(); ok {
				s, err := tsk.Status(context.Background())
				if err != nil {
					logger(c).WithError(err).Error("failed to get task status")
				} else {
					status = s.Status
					alive = status != containerd.Stopped
					if !alive {
						logger(c).Debug("cleaning up dead container process")
						es, err = tsk.Delete(context.Background())
						if err != nil && !cerrdefs.IsNotFound(err) {
							logger(c).WithError(err).Error("failed to delete task from containerd")
							return
						}
					} else if !cfg.LiveRestoreEnabled {
						logger(c).Debug("shutting down container considered alive by containerd")
						if err := daemon.shutdownContainer(c); err != nil && !cerrdefs.IsNotFound(err) {
							baseLogger.WithError(err).Error("error shutting down container")
							return
						}
						status = containerd.Stopped
						alive = false
						c.ResetRestartManager(false)
					}
				}
			}
			// If the containerd task for the container was not found, docker's view of the
			// container state will be updated accordingly via SetStopped further down.

			if c.State.IsRunning() || c.State.IsPaused() {
				logger(c).Debug("syncing container on disk state with real state")

				c.RestartManager().Cancel() // manually start containers because some need to wait for swarm networking

				switch {
				case c.State.IsPaused() && alive:
					logger(c).WithField("state", status).Info("restored container paused")
					switch status {
					case containerd.Paused, containerd.Pausing:
						// nothing to do
					case containerd.Unknown, containerd.Stopped, "":
						baseLogger.WithField("status", status).Error("unexpected status for paused container during restore")
					default:
						// running
						c.Lock()
						c.State.SetPaused(false)
						daemon.setStateCounter(c)
						daemon.initHealthMonitor(c)
						if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
							baseLogger.WithError(err).Error("failed to update paused container state")
						}
						c.Unlock()
					}
				case !c.State.IsPaused() && alive:
					logger(c).Debug("restoring healthcheck")
					c.Lock()
					daemon.initHealthMonitor(c)
					c.Unlock()
				}

				if !alive {
					logger(c).Debug("setting stopped state")
					c.Lock()
					var ces container.ExitStatus
					if es != nil {
						ces.ExitCode = int(es.ExitCode())
						ces.ExitedAt = es.ExitTime()
					} else {
						ces.ExitCode = 255
					}
					c.State.SetStopped(&ces)
					daemon.Cleanup(context.TODO(), c)
					if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
						baseLogger.WithError(err).Error("failed to update stopped container state")
					}
					c.Unlock()
					logger(c).Debug("set stopped state")
				}

				// we call Mount and then Unmount to get BaseFs of the container
				if err := daemon.Mount(c); err != nil {
					// The mount is unlikely to fail. However, in case mount fails
					// the container should be allowed to restore here. Some functionalities
					// (like docker exec -u user) might be missing but container is able to be
					// stopped/restarted/removed.
					// See #29365 for related information.
					// The error is only logged here.
					logger(c).WithError(err).Warn("failed to mount container to get BaseFs path")
				} else {
					if err := daemon.Unmount(c); err != nil {
						logger(c).WithError(err).Warn("failed to umount container to get BaseFs path")
					}
				}

				c.ResetRestartManager(false)
				if !c.HostConfig.NetworkMode.IsContainer() && c.State.IsRunning() {
					mapLock.Lock()
					activeSandboxes[c.NetworkSettings.SandboxID] = libnetwork.ActiveSandboxOptionBuilder(func(controller *libnetwork.Controller) []libnetwork.SandboxOption {
						setHostGatewayIP(controller, &cfg.Config)
						options, err := buildSandboxOptions(&cfg.Config, c)
						if err != nil {
							logger(c).WithError(err).Warn("failed to build sandbox option to restore container")
						}
						return options
					})
					mapLock.Unlock()
				}
			}

			// get list of containers we need to restart

			// Do not autostart containers which
			// has endpoints in a swarm scope
			// network yet since the cluster is
			// not initialized yet. We will start
			// it after the cluster is
			// initialized.
			if cfg.AutoRestart && c.ShouldRestart() && !c.NetworkSettings.HasSwarmEndpoint && c.HasBeenStartedBefore {
				mapLock.Lock()
				restartContainers[c] = make(chan struct{})
				mapLock.Unlock()
			} else if c.HostConfig != nil && c.HostConfig.AutoRemove {
				// Remove the container if live-restore is disabled or if the container has already exited.
				if !cfg.LiveRestoreEnabled || !alive {
					mapLock.Lock()
					removeContainers[c.ID] = c
					mapLock.Unlock()
				}
			}

			c.Lock()
			// TODO(thaJeztah): we no longer persist RemovalInProgress on disk, so this code is likely redundant; see https://github.com/moby/moby/pull/49968
			if c.State.RemovalInProgress {
				// We probably crashed in the middle of a removal, reset
				// the flag.
				//
				// We DO NOT remove the container here as we do not
				// know if the user had requested for either the
				// associated volumes, network links or both to also
				// be removed. So we put the container in the "dead"
				// state and leave further processing up to them.
				c.State.RemovalInProgress = false
				c.State.SetDead()
				if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
					baseLogger.WithError(err).Error("failed to update RemovalInProgress container state")
				} else {
					baseLogger.Debugf("reset RemovalInProgress state for container")
				}
			}
			c.Unlock()
			logger(c).Debug("done restoring container")
		})
	}
	group.Wait()

	// Initialize the network controller and configure network settings.
	//
	// Note that we cannot initialize the network controller earlier, as it
	// needs to know if there's active sandboxes (running containers).
	if err := daemon.initNetworkController(&cfg.Config, activeSandboxes); err != nil {
		return fmt.Errorf("Error initializing network controller: %v", err)
	}

	for _, c := range containers {
		sb, err := daemon.netController.SandboxByID(c.NetworkSettings.SandboxID)
		if err != nil {
			// If this container's sandbox doesn't exist anymore, that's because
			// the netController gc'ed it during its initialization. In that case,
			// we need to clear all the network-related state carried by that
			// container.
			daemon.releaseNetwork(context.WithoutCancel(context.TODO()), c)
			continue
		}
		// If port-mapping failed during live-restore of a container, perhaps because
		// a host port that was previously mapped to a container is now in-use by some
		// other process - ports will not be mapped for the restored container, but it
		// will be running. Replace the restored mappings in NetworkSettings with the
		// current state so that the problem is visible in 'inspect'.
		c.NetworkSettings.Ports = getPortMapInfo(sb)
	}

	// Now that all the containers are registered, register the links
	for _, c := range containers {
		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			if err := daemon.registerLinks(c); err != nil {
				log.G(ctx).WithField("container", c.ID).WithError(err).Error("failed to register link for container")
			}
		})
	}
	group.Wait()

	for c, notifyChan := range restartContainers {
		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			logger := log.G(ctx).WithField("container", c.ID)

			logger.Debug("starting container")

			// ignore errors here as this is a best effort to wait for children
			// (legacy links or container network) to be running before we try to start the container
			if children := daemon.GetDependentContainers(c); len(children) > 0 {
				timeout := time.NewTimer(5 * time.Second)
				defer timeout.Stop()

				for _, child := range children {
					if notifier, exists := restartContainers[child]; exists {
						select {
						case <-notifier:
						case <-timeout.C:
						}
					}
				}
			}

			if err := daemon.prepareMountPoints(c); err != nil {
				logger.WithError(err).Error("failed to prepare mount points for container")
			}
			if err := daemon.containerStart(context.Background(), cfg, c, "", "", true); err != nil {
				logger.WithError(err).Error("failed to start container")
			}
			close(notifyChan)
		})
	}
	group.Wait()

	for cid, c := range removeContainers {
		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			if c.State.IsDead() {
				if err := daemon.cleanupContainer(c, backend.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
					log.G(ctx).WithField("container", cid).WithError(err).Error("failed to remove dead container")
				}
				return
			}

			if err := daemon.containerRm(&cfg.Config, cid, &backend.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
				log.G(ctx).WithField("container", cid).WithError(err).Error("failed to remove container")
			}
		})
	}
	group.Wait()

	// any containers that were started above would already have had this done,
	// however we need to now prepare the mountpoints for the rest of the containers as well.
	// This shouldn't cause any issue running on the containers that already had this run.
	// This must be run after any containers with a restart policy so that containerized plugins
	// can have a chance to be running before we try to initialize them.
	for _, c := range containers {
		// if the container has restart policy, do not
		// prepare the mountpoints since it has been done on restarting.
		// This is to speed up the daemon start when a restart container
		// has a volume and the volume driver is not available.
		if _, ok := restartContainers[c]; ok {
			continue
		} else if _, ok := removeContainers[c.ID]; ok {
			// container is automatically removed, skip it.
			continue
		}

		group.Go(func() {
			_ = sem.Acquire(context.WithoutCancel(ctx), 1)
			defer sem.Release(1)

			if err := daemon.prepareMountPoints(c); err != nil {
				log.G(ctx).WithField("container", c.ID).WithError(err).Error("failed to prepare mountpoints for container")
			}
		})
	}
	group.Wait()

	log.G(ctx).Info("Loading containers: done.")

	return nil
}

// RestartSwarmContainers restarts any autostart container which has a
// swarm endpoint.
func (daemon *Daemon) RestartSwarmContainers(ctx context.Context) error {
	return daemon.restartSwarmContainers(ctx, daemon.config())
}

func (daemon *Daemon) restartSwarmContainers(ctx context.Context, cfg *configStore) error {
	// parallelLimit is the maximum number of parallel startup jobs that we
	// allow (this is the limited used for all startup semaphores). The multiplier
	// (128) was chosen after some fairly significant benchmarking -- don't change
	// it unless you've tested it significantly (this value is adjusted if
	// RLIMIT_NOFILE is small to avoid EMFILE).
	parallelLimit := adjustParallelLimit(len(daemon.List()), 128*runtime.NumCPU())

	var group sync.WaitGroup
	sem := semaphore.NewWeighted(int64(parallelLimit))

	for _, c := range daemon.List() {
		if err := ctx.Err(); err != nil {
			group.Wait()
			return err
		}

		if c.State.IsRunning() || c.State.IsPaused() {
			continue
		}

		// Autostart all the containers which has a
		// swarm endpoint now that the cluster is
		// initialized.
		if cfg.AutoRestart && c.ShouldRestart() && c.NetworkSettings.HasSwarmEndpoint && c.HasBeenStartedBefore {
			group.Go(func() {
				if err := sem.Acquire(ctx, 1); err != nil {
					return
				}
				defer sem.Release(1)

				if err := daemon.containerStart(ctx, cfg, c, "", "", true); err != nil {
					log.G(ctx).WithField("container", c.ID).WithError(err).Error("failed to start swarm container")
				}
			})
		}
	}
	group.Wait()

	// A canceled acquire may have skipped one or more container starts.
	if err := ctx.Err(); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) registerLink(parent, child *container.Container, alias string) error {
	fullName := path.Join(parent.Name, alias)
	if err := daemon.containersReplica.ReserveName(fullName, child.ID); err != nil {
		if cerrdefs.IsConflict(err) {
			log.G(context.TODO()).Warnf("error registering link for %s, to %s, as alias %s, ignoring: %v", parent.ID, child.ID, alias, err)
			return nil
		}
		return err
	}
	daemon.linkIndex.link(parent, child, fullName)
	return nil
}

// DaemonJoinsCluster informs the daemon has joined the cluster and provides
// the handler to query the cluster component
func (daemon *Daemon) DaemonJoinsCluster(clusterProvider cluster.Provider) {
	daemon.setClusterProvider(clusterProvider)
}

// DaemonLeavesCluster informs the daemon has left the cluster
func (daemon *Daemon) DaemonLeavesCluster() {
	// Daemon is in charge of removing the attachable networks with
	// connected containers when the node leaves the swarm
	daemon.clearAttachableNetworks()
	// We no longer need the cluster provider, stop it now so that
	// the network agent will stop listening to cluster events.
	daemon.setClusterProvider(nil)
	// Wait for the networking cluster agent to stop
	daemon.netController.AgentStopWait()
	// Daemon is in charge of removing the ingress network when the
	// node leaves the swarm. Wait for job to be done or timeout.
	// This is called also on graceful daemon shutdown. We need to
	// wait, because the ingress release has to happen before the
	// network controller is stopped.

	if done, err := daemon.ReleaseIngress(); err == nil {
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()

		select {
		case <-done:
		case <-timeout.C:
			log.G(context.TODO()).Warn("timeout while waiting for ingress network removal")
		}
	} else {
		log.G(context.TODO()).Warnf("failed to initiate ingress network removal: %v", err)
	}

	daemon.attachmentStore.ClearAttachments()
}

// setClusterProvider sets a component for querying the current cluster state.
func (daemon *Daemon) setClusterProvider(clusterProvider cluster.Provider) {
	daemon.clusterProvider = clusterProvider
	daemon.netController.SetClusterProvider(clusterProvider)
	daemon.attachableNetworkLock = locker.New()
}

// IsSwarmCompatible verifies if the current daemon
// configuration is compatible with the swarm mode
func (daemon *Daemon) IsSwarmCompatible() error {
	return daemon.config().IsSwarmCompatible()
}

var checkSystemOnce = sync.OnceValue(checkSystem)

// CheckSystem verifies that the system meets the platform-specific requirements
// for running the Docker daemon.
func CheckSystem() error {
	if os.Getenv("TEST_SYSTEM_REQUIREMENTS_FAILURE") != "" {
		return errors.New("fake system requirements not met error")
	}
	return checkSystemOnce()
}

// NewDaemon sets up everything for the daemon to be able to service
// requests from the webserver.
func NewDaemon(ctx context.Context, config *config.Config, pluginStore *plugin.Store, authzMiddleware *authorization.Middleware, containerdDialer ContainerdDialer) (_ *Daemon, retErr error) {
	registryService, err := registry.NewService(config.ServiceOptions)
	if err != nil {
		return nil, err
	}

	// Ensure that we have a correct root key limit for launching containers.
	if err := modifyRootKeyLimit(); err != nil {
		log.G(ctx).Warnf("unable to modify root key limit, number of containers could be limited by this quota: %v", err)
	}

	// Ensure we have compatible and valid configuration options
	if err := verifyDaemonSettings(config); err != nil {
		return nil, err
	}

	// Do we have a disabled network?
	config.DisableBridge = isBridgeNetworkDisabled(config)

	// Setup the resolv.conf
	setupResolvConf(config)

	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return nil, err
	}
	uid, gid := idMapping.RootPair()

	// set up the tmpDir to use a canonical path
	tmp, err := prepareTempDir(config.Root)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the TempDir under %s: %s", config.Root, err)
	}
	realTmp, err := resolveSymlinkedDirectory(tmp)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the full path to the TempDir (%s): %s", tmp, err)
	}
	if runtime.GOOS == "windows" {
		if err := os.MkdirAll(realTmp, 0); err != nil {
			return nil, fmt.Errorf("Unable to create the TempDir (%s): %s", realTmp, err)
		}
		_ = os.Setenv("TEMP", realTmp)
		_ = os.Setenv("TMP", realTmp)
		// Set the SystemTemp environment variable, because for system processes
		// GetTempPath2() uses it rather than TEMP/TMP:
		// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-gettemppath2w
		_ = os.Setenv("SystemTemp", realTmp)
	} else {
		_ = os.Setenv("TMPDIR", realTmp)
	}

	if err := initRuntimesDir(config); err != nil {
		return nil, err
	}
	rts, err := setupRuntimes(config)
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		PluginStore: pluginStore,
		startupDone: make(chan struct{}),
	}
	cfgStore := &configStore{
		Config:   *config,
		Runtimes: rts,
	}
	d.configStore.Store(cfgStore)

	imgStoreChoice, err := determineImageStoreChoice(config, determineImageStoreChoiceOptions{})
	if err != nil {
		return nil, err
	}

	migrationThreshold := int64(-1)
	if config.Features["containerd-migration"] {
		migrationThreshold = 0
		if ts := os.Getenv("DOCKER_MIGRATE_SNAPSHOTTER_THRESHOLD"); ts != "" {
			v, err := units.FromHumanSize(ts)
			if err != nil {
				return nil, fmt.Errorf("invalid migration threshold value (DOCKER_MIGRATE_SNAPSHOTTER_THRESHOLD=%s): %w", ts, err)
			}
			if v < 0 {
				return nil, fmt.Errorf("invalid migration threshold value (DOCKER_MIGRATE_SNAPSHOTTER_THRESHOLD=%s): value must not be negative", ts)
			}
			migrationThreshold = v
		}
		if migrationThreshold > 0 {
			log.G(ctx).WithField("max_size", migrationThreshold).Info("(Experimental) Migration to containerd is enabled, driver will be switched to snapshotter after migration is complete")
		} else {
			log.G(ctx).Info("Migration to containerd is enabled, driver will be switched to snapshotter if there are no images or containers")
		}
	}

	// Ensure the daemon is properly shutdown if there is a failure during
	// initialization
	defer func() {
		if retErr != nil {
			// Use a fresh context here. Passed context could be cancelled.
			if err := d.Shutdown(context.Background()); err != nil {
				log.G(ctx).Error(err)
			}
		}
	}()

	// Build the host after installing the cleanup defer: this starts extension
	// processes and must be covered if initialization fails.
	d.extensionHost, err = d.newExtensionHost(ctx)
	if err != nil {
		return nil, err
	}

	if err := d.setGenericResources(&cfgStore.Config); err != nil {
		return nil, err
	}
	// set up SIGUSR1 handler on Unix-like systems, or a Win32 global event
	// on Windows to dump Go routine stacks
	stackDumpDir := cfgStore.Root
	if execRoot := cfgStore.GetExecRoot(); execRoot != "" {
		stackDumpDir = execRoot
	}
	d.setupDumpStackTrap(stackDumpDir)

	if err := d.setupSeccompProfile(&cfgStore.Config); err != nil {
		return nil, err
	}
	if err := d.setupAppArmorProfile(&cfgStore.Config); err != nil {
		return nil, err
	}

	// Set the default isolation mode (only applicable on Windows)
	if err := d.setDefaultIsolation(&cfgStore.Config); err != nil {
		return nil, fmt.Errorf("error setting default isolation mode: %v", err)
	}

	if err := configureMaxThreads(ctx); err != nil {
		log.G(ctx).Warnf("Failed to configure golang's threads limit: %v", err)
	}

	// Always install the default AppArmor profile at startup to pick up
	// any changes to the profile template from a daemon upgrade.
	if err := d.installDefaultAppArmorProfile(); err != nil {
		log.G(ctx).WithError(err).Error("Failed to load default apparmor profile")
	}

	daemonRepo := filepath.Join(cfgStore.Root, "containers")
	if err := user.MkdirAllAndChown(daemonRepo, 0o710, os.Getuid(), gid); err != nil {
		return nil, err
	}

	if runtime.GOOS == "windows" {
		// Note that permissions (0o700) are ignored on Windows; passing them to
		// show intent only. We could consider using idtools.MkdirAndChown here
		// to apply an ACL.
		if err = os.Mkdir(filepath.Join(cfgStore.Root, "credentialspecs"), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}

	d.registryService = registryService
	dlogger.RegisterPluginGetter(d.PluginStore)

	if err := metrics.RegisterPlugin(d.PluginStore, filepath.Join(cfgStore.ExecRoot, "metrics.sock")); err != nil {
		return nil, err
	}

	if err := d.initOCIHooks(&cfgStore.Config); err != nil {
		return nil, err
	}

	const connTimeout = 60 * time.Second

	gopts := []grpc.DialOption{
		grpc.WithStatsHandler(tracing.ClientStatsHandler(otelgrpc.WithTracerProvider(otel.GetTracerProvider()))),
		grpc.WithUnaryInterceptor(grpcerrors.UnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpcerrors.StreamClientInterceptor),
	}
	if containerdDialer != nil {
		// Keep ContainerdAddr as the gRPC target (so containerd.New still
		// installs its namespace interceptors) but override the connection to
		// use the in-memory pipe.
		gopts = append(gopts, grpc.WithContextDialer(containerdDialer))
	}

	if cfgStore.ContainerdAddr != "" {
		log.G(ctx).WithFields(log.Fields{
			"address": cfgStore.ContainerdAddr,
			"timeout": connTimeout,
		}).Info("Creating a containerd client")
		d.containerdClient, err = containerd.New(
			cfgStore.ContainerdAddr,
			containerd.WithDefaultNamespace(cfgStore.ContainerdNamespace),
			containerd.WithExtraDialOpts(gopts),
			containerd.WithTimeout(connTimeout),
		)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to dial %q", cfgStore.ContainerdAddr)
		}
	}

	createPluginExec := func(m *plugin.Manager) (plugin.Executor, error) {
		var pluginCli *containerd.Client

		if cfgStore.ContainerdAddr != "" {
			pluginCli, err = containerd.New(
				cfgStore.ContainerdAddr,
				containerd.WithDefaultNamespace(cfgStore.ContainerdPluginNamespace),
				containerd.WithExtraDialOpts(gopts),
				containerd.WithTimeout(connTimeout),
			)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to dial %q", cfgStore.ContainerdAddr)
			}
		}

		var (
			shim     string
			shimOpts any
		)
		if runtime.GOOS != "windows" {
			shim, shimOpts, err = rts.Get("")
			if err != nil {
				return nil, err
			}
		}
		return pluginexec.New(ctx, getPluginExecRoot(&cfgStore.Config), pluginCli, cfgStore.ContainerdPluginNamespace, m, shim, shimOpts)
	}

	// Plugin system initialization should happen before restore. Do not change order.
	d.pluginManager, err = plugin.NewManager(plugin.ManagerConfig{
		Root:               filepath.Join(cfgStore.Root, "plugins"),
		ExecRoot:           getPluginExecRoot(&cfgStore.Config),
		Store:              d.PluginStore,
		CreateExecutor:     createPluginExec,
		RegistryService:    registryService,
		LiveRestoreEnabled: cfgStore.LiveRestoreEnabled,
		LogPluginEvent:     d.LogPluginEvent, // todo: make private
		AuthzMiddleware:    authzMiddleware,
	})
	if err != nil {
		return nil, errors.Wrap(err, "couldn't create plugin manager")
	}

	d.defaultLogConfig, err = defaultLogConfig(&cfgStore.Config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to set log opts")
	}
	log.G(ctx).Debugf("Using default logging driver %s", d.defaultLogConfig.Type)

	d.volumes, err = volumesservice.NewVolumeService(cfgStore.Root, d.PluginStore, idtools.Identity{UID: uid, GID: gid}, d)
	if err != nil {
		return nil, err
	}

	// Check if Devices cgroup is mounted, it is hard requirement for container security,
	// on Linux.
	//
	// Important: we call getSysInfo() directly here, without storing the results,
	// as networking has not yet been set up, so we only have partial system info
	// at this point.
	//
	// TODO(thaJeztah) add a utility to only collect the CgroupDevicesEnabled information
	if runtime.GOOS == "linux" && !userns.RunningInUserNS() && !getSysInfo(&cfgStore.Config).CgroupDevicesEnabled {
		return nil, errors.New("Devices cgroup isn't mounted")
	}

	d.id, err = LoadOrCreateID(cfgStore.Root)
	if err != nil {
		return nil, err
	}
	d.repository = daemonRepo
	d.containers = container.NewMemoryStore()
	if d.containersReplica, err = container.NewViewDB(); err != nil {
		return nil, err
	}
	d.execCommands = container.NewExecStore()
	if runtime.GOOS == "linux" {
		if mi, err := meminfo.Read(); err == nil && mi.MemTotal > 0 {
			d.machineMemory = uint64(mi.MemTotal)
		}
	}
	d.statsCollector = d.newStatsCollector(1 * time.Second)

	d.EventsService = events.New()
	d.root = cfgStore.Root
	d.idMapping = idMapping

	d.linkIndex = newLinkIndex()

	containers, err := d.loadContainers(ctx)
	if err != nil {
		return nil, err
	}

	d.nri, err = nri.NewNRI(ctx, nri.Config{
		DaemonConfig:    config.NRIOpts,
		ContainerLister: d.containers,
	})
	if err != nil {
		return nil, err
	}

	driverName := getDriverOverride(ctx, cfgStore.GraphDriver, imgStoreChoice)

	var migrationConfig migration.Config
	if imgStoreChoice.IsGraphDriver() {
		layerStore, err := layer.NewStoreFromOptions(layer.StoreOptions{
			Root:               cfgStore.Root,
			GraphDriver:        driverName,
			GraphDriverOptions: cfgStore.GraphOptions,
			IDMapping:          idMapping,
		})
		if err != nil {
			return nil, err
		}

		// NewStoreFromOptions will determine the driver if driverName is empty
		// so we need to update the driverName to match the driver used.
		driverName = layerStore.DriverName()

		// Configure and validate the kernels security support. Note this is a Linux/FreeBSD
		// operation only, so it is safe to pass *just* the runtime OS graphdriver.
		if err := configureKernelSecuritySupport(&cfgStore.Config, layerStore.DriverName()); err != nil {
			return nil, err
		}

		imageRoot := filepath.Join(cfgStore.Root, "image", layerStore.DriverName())
		ifs, err := image.NewFSStoreBackend(filepath.Join(imageRoot, "imagedb"))
		if err != nil {
			return nil, err
		}

		// We have a single tag/reference store for the daemon globally. However, it's
		// stored under the graphdriver. On host platforms which only support a single
		// container OS, but multiple selectable graphdrivers, this means depending on which
		// graphdriver is chosen, the global reference store is under there. For
		// platforms which support multiple container operating systems, this is slightly
		// more problematic as where does the global ref store get located? Fortunately,
		// for Windows, which is currently the only daemon supporting multiple container
		// operating systems, the list of graphdrivers available isn't user configurable.
		// For backwards compatibility, we just put it under the windowsfilter
		// directory regardless.
		refStoreLocation := filepath.Join(imageRoot, `repositories.json`)
		rs, err := refstore.NewReferenceStore(refStoreLocation)
		if err != nil {
			return nil, fmt.Errorf("Couldn't create reference store repository: %s", err)
		}
		d.ReferenceStore = rs

		imageStore, err := image.NewImageStore(ifs, layerStore)
		if err != nil {
			return nil, err
		}

		distributionMetadataStore, err := dmetadata.NewFSMetadataStore(filepath.Join(imageRoot, "distribution"))
		if err != nil {
			return nil, err
		}

		imgSvcConfig := images.ImageServiceConfig{
			ContainerStore:            d.containers,
			DistributionMetadataStore: distributionMetadataStore,
			EventsService:             d.EventsService,
			ImageStore:                imageStore,
			LayerStore:                layerStore,
			MaxConcurrentDownloads:    config.MaxConcurrentDownloads,
			MaxConcurrentUploads:      config.MaxConcurrentUploads,
			MaxDownloadAttempts:       config.MaxDownloadAttempts,
			ReferenceStore:            rs,
			RegistryService:           registryService,
			ContentNamespace:          config.ContainerdNamespace,
		}

		// containerd is not currently supported with Windows.
		// So sometimes d.containerdCli will be nil
		// In that case we'll create a local content store... but otherwise we'll use containerd
		if d.containerdClient != nil {
			imgSvcConfig.Leases = d.containerdClient.LeasesService()
			imgSvcConfig.ContentStore = d.containerdClient.ContentStore()
		} else {
			imgSvcConfig.ContentStore, imgSvcConfig.Leases, err = d.configureLocalContentStore(config.ContainerdNamespace)
			if err != nil {
				return nil, err
			}
		}

		// If no containers are present, check whether can migrate image service
		if drv := layerStore.DriverName(); len(containers[drv]) == 0 && migrationThreshold >= 0 {
			switch drv {
			case "overlay2":
				driverName = "overlayfs"
			case "windowsfilter":
				driverName = "windows"
			case "vfs":
				driverName = "native"
			default:
				migrationThreshold = -1
				log.G(ctx).Infof("Not migrating to containerd snapshotter, no migration defined for graph driver %q", drv)
			}

			var totalSize int64
			ic := imgSvcConfig.ImageStore.Len()
			if migrationThreshold >= 0 && ic > 0 {
				for _, img := range imgSvcConfig.ImageStore.Map() {
					if layerID := img.RootFS.ChainID(); layerID != "" {
						l, err := imgSvcConfig.LayerStore.Get(layerID)
						if err != nil {
							if errors.Is(err, layer.ErrLayerDoesNotExist) {
								continue
							}
							return nil, err
						}

						// Just look at layer size for considering maximum size
						totalSize += l.Size()
						layer.ReleaseAndLog(imgSvcConfig.LayerStore, l)
					}
				}
			}

			if totalSize <= migrationThreshold {
				log.G(ctx).WithField("total", totalSize).Infof("Enabling containerd snapshotter because migration set with no containers and %d images in graph driver", ic)
				migrationConfig = migration.Config{
					ImageCount:       ic,
					LayerStore:       imgSvcConfig.LayerStore,
					DockerImageStore: imgSvcConfig.ImageStore,
					ReferenceStore:   imgSvcConfig.ReferenceStore,
				}
			} else if migrationThreshold >= 0 {
				log.G(ctx).WithField("total", totalSize).Warnf("Not migrating to containerd snapshotter because still have %d images in graph driver", ic)
				d.imageService = images.NewImageService(ctx, imgSvcConfig)
			} else {
				d.imageService = images.NewImageService(ctx, imgSvcConfig)
			}
		} else {
			log.G(ctx).Debugf("Not attempting migration with %d containers and %d image threshold", len(containers[drv]), migrationThreshold)
			d.imageService = images.NewImageService(ctx, imgSvcConfig)
		}
	}

	if d.imageService == nil {
		if d.containerdClient == nil {
			return nil, errors.New("containerd snapshotter is enabled but containerd is not configured")
		}
		log.G(ctx).Info("Starting daemon with containerd snapshotter integration enabled")

		resp, err := d.containerdClient.IntrospectionService().Plugins(ctx, `type=="io.containerd.snapshotter.v1"`)
		if err != nil {
			return nil, fmt.Errorf("failed to get containerd plugins: %w", err)
		}
		if resp == nil || len(resp.Plugins) == 0 {
			return nil, fmt.Errorf("failed to get containerd plugins response: %w", cerrdefs.ErrUnavailable)
		}
		availableDrivers := map[string]struct{}{}
		for _, p := range resp.Plugins {
			if p == nil || p.Type != "io.containerd.snapshotter.v1" {
				continue
			}
			if p.InitErr == nil {
				availableDrivers[p.ID] = struct{}{}
			} else if (p.ID == driverName) || (driverName == "" && p.ID == defaults.DefaultSnapshotter) {
				log.G(ctx).WithField("message", p.InitErr.Message).Warn("Preferred snapshotter not available in containerd")
			}
		}

		if driverName == "" {
			if _, ok := availableDrivers[defaults.DefaultSnapshotter]; ok {
				driverName = defaults.DefaultSnapshotter
			} else if _, ok := availableDrivers["native"]; ok {
				driverName = "native"
			} else {
				log.G(ctx).WithField("available", maps.Keys(availableDrivers)).Debug("Preferred snapshotter not available in containerd")
				return nil, fmt.Errorf("snapshotter selection failed, no drivers available: %w", cerrdefs.ErrUnavailable)
			}
		} else if _, ok := availableDrivers[driverName]; !ok {
			return nil, fmt.Errorf("configured driver %q not available: %w", driverName, cerrdefs.ErrUnavailable)
		}

		// Configure and validate the kernels security support. Note this is a Linux/FreeBSD
		// operation only, so it is safe to pass *just* the runtime OS graphdriver.
		if err := configureKernelSecuritySupport(&cfgStore.Config, driverName); err != nil {
			return nil, err
		}
		identityCacheBackend, err := identitycache.NewBoltDBBackend(config.Root)
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to initialize image identity bbolt cache backend")
			identityCacheBackend = identitycache.NewNopBackend()
		}
		d.usesSnapshotter = true
		d.imageService = ctrd.NewService(ctrd.ImageServiceConfig{
			Client:                 d.containerdClient,
			Containers:             d.containers,
			Snapshotter:            driverName,
			IdentityCacheBackend:   identityCacheBackend,
			RegistryHosts:          d.RegistryHosts,
			Registry:               d.registryService,
			EventsService:          d.EventsService,
			IDMapping:              idMapping,
			RefCountMounter:        snapshotter.NewMounter(config.Root, driverName, idMapping),
			PolicyVerifierProvider: verifierProvider(cfgStore.Root),
			MaxConcurrentDownloads: config.MaxConcurrentDownloads,
			MaxConcurrentUploads:   config.MaxConcurrentUploads,
			Features:               d,
		})

		if migrationConfig.ImageCount > 0 {
			if d.imageService.CountImages(ctx) > 0 {
				log.G(ctx).WithField("image_count", migrationConfig.ImageCount).Warnf("Images not migrated because images already exist in containerd %q", migrationConfig.LayerStore.DriverName())
			} else {
				migrationConfig.Leases = d.containerdClient.LeasesService()
				migrationConfig.Content = d.containerdClient.ContentStore()
				migrationConfig.ImageStore = d.containerdClient.ImageService()
				m := migration.NewLayerMigrator(migrationConfig)
				err := m.MigrateTocontainerd(ctx, driverName, d.containerdClient.SnapshotService(driverName))
				if err != nil {
					log.G(ctx).WithError(err).Errorf("Failed to migrate images to containerd, images in graph driver %q are no longer visible", migrationConfig.LayerStore.DriverName())
				} else {
					log.G(ctx).WithField("image_count", migrationConfig.ImageCount).Infof("Successfully migrated images from %q to containerd", migrationConfig.LayerStore.DriverName())
				}
			}
		}
	}

	go d.execCommandGC(ctx)

	d.containerd, err = libcontainerd.NewClient(ctx, d.containerdClient, filepath.Join(config.ExecRoot, "containerd"), config.ContainerdNamespace, d)
	if err != nil {
		return nil, err
	}

	if driverName == "" {
		return nil, errors.New("driverName is empty. Please report it as a bug! As a workaround, please set the storage driver explicitly")
	}

	driverContainers, ok := containers[driverName]
	// Log containers which are not loaded with current driver
	if (!ok && len(containers) > 0) || len(containers) > 1 {
		for driver, all := range containers {
			if driver == driverName {
				continue
			}
			for id := range all {
				log.G(ctx).WithFields(log.Fields{
					"container":      id,
					"driver":         driver,
					"current_driver": driverName,
				}).Debugf("not restoring container because it was created with another storage driver (%s)", driver)
			}
		}
	}
	if err := d.restore(ctx, cfgStore, driverContainers); err != nil {
		return nil, err
	}
	// Wait for migration to complete
	close(d.startupDone)

	info, err := d.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range info.Warnings {
		log.G(ctx).Warn(w)
	}

	metrics.EngineInfo.WithValues(
		dockerversion.Version,
		dockerversion.GitCommit,
		info.Architecture,
		info.Driver,
		info.KernelVersion,
		info.OperatingSystem,
		info.OSType,
		info.OSVersion,
		info.ID,
	).Set(1)
	metrics.EngineCPUs.Set(float64(info.NCPU))
	metrics.EngineMemory.Set(float64(info.MemTotal))

	log.G(ctx).WithFields(log.Fields{
		"version":                dockerversion.Version,
		"commit":                 dockerversion.GitCommit,
		"storage-driver":         d.ImageService().StorageDriver(),
		"containerd-snapshotter": d.UsesSnapshotter(),
	}).Info("Docker daemon")

	return d, nil
}

func verifierProvider(root string) func() (*policyverifier.Verifier, error) {
	var verifier *policyverifier.Verifier
	var mu sync.Mutex

	return func() (*policyverifier.Verifier, error) {
		mu.Lock()
		defer mu.Unlock()

		if verifier != nil {
			return verifier, nil
		}

		confDir := filepath.Join(root, "policy")
		if err := os.MkdirAll(confDir, 0o700); err != nil {
			return nil, errors.Wrapf(err, "failed to create policy verifier config dir")
		}

		v, err := policyverifier.NewVerifier(policyverifier.Config{
			StateDir: confDir,
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create policy verifier")
		}
		verifier = v
		return verifier, nil
	}
}

// DistributionServices returns services controlling daemon storage
func (daemon *Daemon) DistributionServices() images.DistributionServices {
	return daemon.imageService.DistributionServices()
}

func (daemon *Daemon) waitForStartupDone() {
	<-daemon.startupDone
}

func (daemon *Daemon) shutdownContainer(c *container.Container) error {
	ctx := context.WithoutCancel(context.TODO())

	// If container failed to exit in stopTimeout seconds of SIGTERM, then using the force
	if err := daemon.containerStop(ctx, c, backend.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("Failed to stop container %s with error: %v", c.ID, err)
	}

	// Wait without timeout for the container to exit.
	// Ignore the result.
	<-c.State.Wait(ctx, containertypes.WaitConditionNotRunning)
	return nil
}

// ShutdownTimeout returns the timeout (in seconds) before containers are forcibly
// killed during shutdown. The default timeout can be configured both on the daemon
// and per container, and the longest timeout will be used. A grace-period of
// 5 seconds is added to the configured timeout.
//
// A negative (-1) timeout means "indefinitely", which means that containers
// are not forcibly killed, and the daemon shuts down after all containers exit.
func (daemon *Daemon) ShutdownTimeout() int {
	return daemon.shutdownTimeout(&daemon.config().Config)
}

func (daemon *Daemon) shutdownTimeout(cfg *config.Config) int {
	shutdownTimeout := cfg.ShutdownTimeout
	if shutdownTimeout < 0 {
		return -1
	}
	if daemon.containers == nil {
		return shutdownTimeout
	}

	graceTimeout := 5
	for _, c := range daemon.containers.List() {
		stopTimeout := cfg.DefaultStopTimeout
		if c.Config.StopTimeout != nil {
			stopTimeout = *c.Config.StopTimeout
		}
		if stopTimeout < 0 {
			return -1
		}
		if stopTimeout+graceTimeout > shutdownTimeout {
			shutdownTimeout = stopTimeout + graceTimeout
		}
	}
	return shutdownTimeout
}

// Shutdown stops the daemon.
func (daemon *Daemon) Shutdown(ctx context.Context) error {
	daemon.shutdown = true
	// Extensions are daemon-scoped regardless of whether they are built-in or
	// executable, so shut them down before the live-restore early return.
	if daemon.extensionHost != nil {
		if err := daemon.extensionHost.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shut down extensions")
		}
	}

	// Keep mounts and networking running on daemon shutdown if
	// we are to keep containers running and restore them.

	cfg := &daemon.config().Config
	if cfg.LiveRestoreEnabled && daemon.containers != nil {
		// check if there are any running containers, if none we should do some cleanup
		if ls, err := daemon.Containers(ctx, &backend.ContainerListOptions{}); len(ls) != 0 || err != nil {
			// metrics plugins still need some cleanup
			metrics.CleanupPlugin(daemon.PluginStore)
			return err
		}
	}

	if daemon.containers != nil {
		log.G(ctx).Debugf("daemon configured with a %d seconds minimum shutdown timeout", cfg.ShutdownTimeout)
		log.G(ctx).Debugf("start clean shutdown of all containers with a %d seconds timeout...", daemon.shutdownTimeout(cfg))
		daemon.containers.ApplyAll(func(c *container.Container) {
			if !c.State.IsRunning() {
				return
			}
			logger := log.G(ctx).WithField("container", c.ID)
			logger.Debug("shutting down container")
			if err := daemon.shutdownContainer(c); err != nil {
				logger.WithError(err).Error("failed to shut down container")
				return
			}
			if mountid, err := daemon.imageService.GetLayerMountID(c.ID); err == nil {
				daemon.cleanupMountsByID(mountid)
			}
			logger.Debugf("shut down container")
		})
	}

	if daemon.volumes != nil {
		if err := daemon.volumes.Shutdown(); err != nil {
			log.G(ctx).Errorf("Error shutting down volume store: %v", err)
		}
	}

	if daemon.imageService != nil {
		if err := daemon.imageService.Cleanup(); err != nil {
			log.G(ctx).Error(err)
		}
	}

	// If we are part of a cluster, clean up cluster's stuff
	if daemon.clusterProvider != nil {
		log.G(ctx).Debugf("start clean shutdown of cluster resources...")
		daemon.DaemonLeavesCluster()
	}

	metrics.CleanupPlugin(daemon.PluginStore)

	// Shutdown plugins after containers and layerstore. Don't change the order.
	daemon.pluginShutdown()

	if daemon.nri != nil {
		daemon.nri.Shutdown(ctx)
	}

	// Hook callbacks may use the network controller, so wait for them to
	// return before closing its store.
	if daemon.ociHooks != nil {
		if err := daemon.ociHooks.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("error shutting down OCI hook server")
		}
	}

	if daemon.netStore != nil {
		daemon.netStore.Close()
	}

	if daemon.containerdClient != nil {
		daemon.containerdClient.Close()
	}

	if daemon.mdDB != nil {
		daemon.mdDB.Close()
	}

	// At this point, everything has been shut down and no containers are
	// running anymore. If there are still some open connections to the
	// '/events' endpoint, closing the EventsService should tear them down
	// immediately.
	if daemon.EventsService != nil {
		daemon.EventsService.Close()
	}

	return daemon.cleanupMounts(cfg)
}

// Mount sets container.BaseFS
func (daemon *Daemon) Mount(container *container.Container) error {
	ctx := context.TODO()

	if container.RWLayer == nil {
		return errors.New("RWLayer of container " + container.ID + " is unexpectedly nil")
	}
	dir, err := container.RWLayer.Mount(container.GetMountLabel())
	if err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{"container": container.ID, "root": dir, "storage-driver": container.Driver}).Debug("container mounted via layerStore")

	if container.BaseFS != "" && container.BaseFS != dir {
		// The mount path reported by the graph driver should always be trusted on Windows, since the
		// volume path for a given mounted layer may change over time.  This should only be an error
		// on non-Windows operating systems.
		if runtime.GOOS != "windows" {
			daemon.Unmount(container)
			driver := daemon.ImageService().StorageDriver()
			return fmt.Errorf("driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
				driver, container.ID, container.BaseFS, dir)
		}
	}
	container.BaseFS = dir // TODO: combine these fields
	return nil
}

// Unmount unsets the container base filesystem
func (daemon *Daemon) Unmount(container *container.Container) error {
	ctx := context.TODO()
	if container.RWLayer == nil {
		return errors.New("RWLayer of container " + container.ID + " is unexpectedly nil")
	}
	if err := container.RWLayer.Unmount(); err != nil {
		log.G(ctx).WithField("container", container.ID).WithError(err).Error("error unmounting container")
		return err
	}

	return nil
}

// Subnets return the IPv4 and IPv6 subnets of networks that are manager by Docker.
func (daemon *Daemon) Subnets() ([]net.IPNet, []net.IPNet) {
	var v4Subnets []net.IPNet
	var v6Subnets []net.IPNet

	for _, managedNetwork := range daemon.netController.Networks(context.TODO()) {
		v4infos, v6infos := managedNetwork.IpamInfo()
		for _, info := range v4infos {
			if info.IPAMData.Pool != nil {
				v4Subnets = append(v4Subnets, *info.IPAMData.Pool)
			}
		}
		for _, info := range v6infos {
			if info.IPAMData.Pool != nil {
				v6Subnets = append(v6Subnets, *info.IPAMData.Pool)
			}
		}
	}

	return v4Subnets, v6Subnets
}

// prepareTempDir prepares and returns the default directory to use
// for temporary files.
// If it doesn't exist, it is created. If it exists, its content is removed.
func prepareTempDir(rootDir string) (string, error) {
	var tmpDir string
	if tmpDir = os.Getenv("DOCKER_TMPDIR"); tmpDir == "" {
		tmpDir = filepath.Join(rootDir, "tmp")
		newName := tmpDir + "-old"
		if err := os.Rename(tmpDir, newName); err == nil {
			go func() {
				if err := os.RemoveAll(newName); err != nil {
					log.G(context.TODO()).Warnf("failed to delete old tmp directory: %s", newName)
				}
			}()
		} else if !os.IsNotExist(err) {
			log.G(context.TODO()).Warnf("failed to rename %s for background deletion: %s. Deleting synchronously", tmpDir, err)
			if err := os.RemoveAll(tmpDir); err != nil {
				log.G(context.TODO()).Warnf("failed to delete old tmp directory: %s", tmpDir)
			}
		}
	}
	return tmpDir, user.MkdirAllAndChown(tmpDir, 0o700, os.Getuid(), os.Getegid())
}

func (daemon *Daemon) setGenericResources(conf *config.Config) error {
	genericResources, err := config.ParseGenericResources(conf.NodeGenericResources)
	if err != nil {
		return err
	}

	daemon.genericResources = genericResources

	return nil
}

// IsShuttingDown tells whether the daemon is shutting down or not
func (daemon *Daemon) IsShuttingDown() bool {
	return daemon.shutdown
}

func isBridgeNetworkDisabled(conf *config.Config) bool {
	return conf.BridgeConfig.Iface == config.DisableNetworkBridge
}

// newNetworkController creates the libnetwork controller, which keeps its
// state in the daemon's data root.
func (daemon *Daemon) newNetworkController(ctx context.Context, conf *config.Config, options []nwconfig.Option) error {
	store, err := boltdb.New(filepath.Join(conf.Root, config.LibnetDataPath, "local-kv.db"))
	if err != nil {
		return err
	}
	controller, err := libnetwork.New(ctx, append(options, nwconfig.WithKVStore(store))...)
	if err != nil {
		store.Close()
		return err
	}
	daemon.netController, daemon.netStore = controller, store
	return nil
}

func (daemon *Daemon) networkOptions(conf *config.Config, pg plugingetter.PluginGetter, hostID string, activeSandboxes map[string]any) ([]nwconfig.Option, error) {
	options := []nwconfig.Option{
		nwconfig.OptionDefaultDriver(network.DefaultNetwork),
		nwconfig.OptionDefaultNetwork(network.DefaultNetwork),
		nwconfig.OptionNetworkControlPlaneMTU(conf.NetworkControlPlaneMTU),
		nwconfig.OptionFirewallBackend(conf.FirewallBackend),
	}

	options = append(options, networkPlatformOptions(conf)...)

	defaultAddressPools := ipamutils.GetLocalScopeDefaultNetworks()
	if len(conf.NetworkConfig.DefaultAddressPools.Value()) > 0 {
		defaultAddressPools = conf.NetworkConfig.DefaultAddressPools.Value()
	}
	// If the Engine admin don't configure default-address-pools or if they
	// don't provide any IPv6 prefix, we derive a ULA prefix from the daemon's
	// hostID and add it to the pools. This makes dynamic IPv6 subnet
	// allocation possible out-of-the-box.
	if !slices.ContainsFunc(defaultAddressPools, func(nw *ipamutils.NetworkToSplit) bool {
		return nw.Base.Addr().Is6() && !nw.Base.Addr().Is4In6()
	}) {
		defaultAddressPools = append(defaultAddressPools, deriveULABaseNetwork(hostID))
	}
	options = append(options, nwconfig.OptionDefaultAddressPoolConfig(defaultAddressPools))

	if conf.LiveRestoreEnabled && len(activeSandboxes) != 0 {
		options = append(options, nwconfig.OptionActiveSandboxes(activeSandboxes))
	}
	if pg != nil {
		options = append(options, nwconfig.OptionPluginGetter(pg))
	}

	return options, nil
}

// deriveULABaseNetwork derives a Global ID from the provided hostID and
// appends it to the ULA prefix (with L bit set) to generate a ULA prefix
// unique to this host. The returned ipamutils.NetworkToSplit is stable over
// time if hostID doesn't change.
//
// This is loosely based on the algorithm described in https://datatracker.ietf.org/doc/html/rfc4193#section-3.2.2.
func deriveULABaseNetwork(hostID string) *ipamutils.NetworkToSplit {
	sha := sha256.Sum256([]byte(hostID))
	gid := binary.BigEndian.Uint64(sha[:]) & (1<<40 - 1) // Keep the 40 least significant bits.
	addr := ipbits.Add(netip.MustParseAddr("fd00::"), gid, 80)

	return &ipamutils.NetworkToSplit{
		Base: netip.PrefixFrom(addr, 48),
		Size: 64,
	}
}

// GetCluster returns the cluster
func (daemon *Daemon) GetCluster() Cluster {
	return daemon.cluster
}

// SetCluster sets the cluster
func (daemon *Daemon) SetCluster(cluster Cluster) {
	daemon.cluster = cluster
}

func (daemon *Daemon) pluginShutdown() {
	manager := daemon.pluginManager
	// Check for a valid manager object. In error conditions, daemon init can fail
	// and shutdown called, before plugin manager is initialized.
	if manager != nil {
		manager.Shutdown()
	}
}

// PluginManager returns current pluginManager associated with the daemon
func (daemon *Daemon) PluginManager() *plugin.Manager { // set up before daemon to avoid this method
	return daemon.pluginManager
}

// PluginGetter returns current pluginStore associated with the daemon
func (daemon *Daemon) PluginGetter() *plugin.Store {
	return daemon.PluginStore
}

// CreateDaemonRoot creates the root for the daemon
func CreateDaemonRoot(config *config.Config) error {
	// get the canonical path to the Docker root directory
	var realRoot string
	if _, err := os.Stat(config.Root); err != nil && os.IsNotExist(err) {
		realRoot = config.Root
	} else {
		realRoot, err = resolveSymlinkedDirectory(config.Root)
		if err != nil {
			return fmt.Errorf("unable to get the full path to root (%s): %s", config.Root, err)
		}
	}

	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return err
	}
	uid, gid := idMapping.RootPair()
	return setupDaemonRoot(config, realRoot, uid, gid)
}

// RemapContainerdNamespaces returns the right containerd namespaces to use:
// - if they are not already set in the config file
// -  and the daemon is running with user namespace remapping enabled
// Then it will return new namespace names, otherwise it will return the existing
// namespaces
func RemapContainerdNamespaces(config *config.Config) (ns string, pluginNs string, err error) {
	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return "", "", err
	}
	if idMapping.Empty() {
		return config.ContainerdNamespace, config.ContainerdPluginNamespace, nil
	}
	uid, gid := idMapping.RootPair()

	ns = config.ContainerdNamespace
	if _, ok := config.ValuesSet["containerd-namespace"]; !ok {
		ns = fmt.Sprintf("%s-%d.%d", config.ContainerdNamespace, uid, gid)
	}

	pluginNs = config.ContainerdPluginNamespace
	if _, ok := config.ValuesSet["containerd-plugin-namespace"]; !ok {
		pluginNs = fmt.Sprintf("%s-%d.%d", config.ContainerdPluginNamespace, uid, gid)
	}

	return ns, pluginNs, nil
}

// checkpointAndSave grabs a container lock to safely call container.CheckpointTo
func (daemon *Daemon) checkpointAndSave(container *container.Container) error {
	container.Lock()
	defer container.Unlock()
	if err := container.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
		return fmt.Errorf("Error saving container state: %v", err)
	}
	return nil
}

// because the CLI sends a -1 when it wants to unset the swappiness value
// we need to clear it on the server side
func fixMemorySwappiness(resources *containertypes.Resources) {
	if resources.MemorySwappiness != nil && *resources.MemorySwappiness == -1 {
		resources.MemorySwappiness = nil
	}
}

// GetAttachmentStore returns current attachment store associated with the daemon
func (daemon *Daemon) GetAttachmentStore() *network.AttachmentStore {
	return &daemon.attachmentStore
}

// IdentityMapping returns uid/gid mapping or a SID (in the case of Windows) for the builder
func (daemon *Daemon) IdentityMapping() user.IdentityMapping {
	return daemon.idMapping
}

// ImageService returns the Daemon's ImageService
func (daemon *Daemon) ImageService() ImageService {
	return daemon.imageService
}

// ImageBackend returns an image-backend for Swarm and the distribution router.
func (daemon *Daemon) ImageBackend() executorpkg.ImageBackend {
	return &imageBackend{
		ImageService:    daemon.imageService,
		registryService: daemon.registryService,
	}
}

// RegistryService returns the Daemon's RegistryService
func (daemon *Daemon) RegistryService() *registry.Service {
	return daemon.registryService
}

// BuilderBackend returns the backend used by builder
func (daemon *Daemon) BuilderBackend() builder.Backend {
	return struct {
		*Daemon
		ImageService
	}{daemon, daemon.imageService}
}

// RawSysInfo returns *sysinfo.SysInfo .
func (daemon *Daemon) RawSysInfo() (*sysinfo.SysInfo, error) {
	daemon.sysInfoOnce.Do(func() {
		// We check if sysInfo is not set here, to allow some test to
		// override the actual sysInfo.
		if daemon.sysInfo == nil {
			daemon.sysInfoErr = daemon.runInNetNS(func() error {
				daemon.sysInfo = getSysInfo(&daemon.config().Config)
				return nil
			})
		}
	})

	return daemon.sysInfo, daemon.sysInfoErr
}

// imageBackend is used to satisfy the [executorpkg.ImageBackend] and
// [github.com/moby/moby/v2/daemon/server/router/distribution.Backend]
// interfaces.
type imageBackend struct {
	ImageService
	registryService *registry.Service
}

// GetRepositories returns a list of repositories configured for the given
// reference. Multiple repositories can be returned if the reference is for
// the default (Docker Hub) registry and a mirror is configured, but it omits
// registries that were not reachable (pinging the /v2/ endpoint failed).
//
// It returns an error if it was unable to reach any of the registries for
// the given reference, or if the provided reference is invalid.
func (i *imageBackend) GetRepositories(ctx context.Context, ref reference.Named, authConfig *registrytypes.AuthConfig) ([]dist.Repository, error) {
	return distribution.GetRepositories(ctx, ref, &distribution.ImagePullConfig{
		Config: distribution.Config{
			AuthConfig:      authConfig,
			RegistryService: i.registryService,
		},
	})
}

// resolveSymlinkedDirectory returns the target directory of a symlink and
// checks if it resolves to a directory (not a file).
func resolveSymlinkedDirectory(path string) (realPath string, _ error) {
	var err error
	realPath, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("unable to get absolute path for %s: %w", path, err)
	}
	realPath, err = filepath.EvalSymlinks(realPath)
	if err != nil {
		return "", fmt.Errorf("failed to canonicalise path for %s: %w", path, err)
	}
	realPathInfo, err := os.Stat(realPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat target '%s' of '%s': %w", realPath, path, err)
	}
	if !realPathInfo.Mode().IsDir() {
		return "", fmt.Errorf("canonical path points to a file '%s'", realPath)
	}
	return realPath, nil
}

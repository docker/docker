package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/cio"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/docker/go-units"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	mounttypes "github.com/moby/moby/api/types/mount"
	networktypes "github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/daemon/internal/image"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/daemon/internal/restartmanager"
	"github.com/moby/moby/v2/daemon/internal/stream"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/jsonfilelog"
	"github.com/moby/moby/v2/daemon/logger/local"
	"github.com/moby/moby/v2/daemon/logger/loggerutils/cache"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/pkg/oci"
	"github.com/moby/moby/v2/daemon/volume"
	volumemounts "github.com/moby/moby/v2/daemon/volume/mounts"
	"github.com/moby/moby/v2/errdefs"
	agentexec "github.com/moby/swarmkit/v2/agent/exec"
	"github.com/moby/sys/atomicwriter"
	"github.com/moby/sys/signal"
	"github.com/moby/sys/symlink"
	"github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	configFileName     = "config.v2.json"
	hostConfigFileName = "hostconfig.json"

	// defaultStopSignal is the default syscall signal used to stop a container.
	defaultStopSignal = syscall.SIGTERM
)

// ExitStatus provides exit reasons for a container.
type ExitStatus struct {
	// The exit code with which the container exited.
	ExitCode int

	// Time at which the container died
	ExitedAt time.Time

	// Error, if not nil, replaces the container's recorded error.
	Error error
}

// Container holds the structure defining a container object.
type Container struct {
	StreamConfig *stream.Config
	// We embed [State] here so that Container supports states directly,
	// but marshal it as a struct in JSON.
	//
	// State also provides a [sync.Mutex] which is used as lock for both
	// the Container and State.
	*State          `json:"State"`
	Root            string  `json:"-"` // Path to the "home" of the container, including metadata.
	BaseFS          string  `json:"-"` // Path to the graphdriver mountpoint
	RWLayer         RWLayer `json:"-"`
	ID              string
	Created         time.Time
	Managed         bool
	Path            string
	Args            []string
	Config          *containertypes.Config
	ImageID         image.ID `json:"Image"`
	ImageManifest   *ocispec.Descriptor
	NetworkSettings *network.Settings
	LogPath         string
	Name            string
	Driver          string

	// Deprecated: use [ImagePlatform.OS] instead.
	// TODO: Remove, see https://github.com/moby/moby/issues/48892
	OS string

	ImagePlatform ocispec.Platform

	RestartCount             int
	HasBeenStartedBefore     bool
	HasBeenManuallyStopped   bool // used for unless-stopped restart policy
	HasBeenManuallyRestarted bool `json:"-"` // used to distinguish restart caused by restart policy from the manual one
	MountPoints              map[string]*volumemounts.MountPoint
	HostConfig               *containertypes.HostConfig `json:"-"` // do not serialize the host config in the json, otherwise we'll make the container unportable
	ExecCommands             *ExecStore                 `json:"-"`
	DependencyStore          agentexec.DependencyGetter `json:"-"`
	SecretReferences         []*swarmtypes.SecretReference
	ConfigReferences         []*swarmtypes.ConfigReference
	// logDriver for closing
	LogDriver      logger.Logger  `json:"-"`
	LogCopier      *logger.Copier `json:"-"`
	restartManager *restartmanager.RestartManager
	attachContext  *attachContext

	// Fields here are specific to Unix platforms
	SecurityOptions
	HostnamePath   string
	HostsPath      string
	ShmPath        string
	ResolvConfPath string

	// Fields here are specific to Windows
	NetworkSharedContainerID string   `json:"-"`
	SharedEndpointList       []string `json:"-"`
	LocalLogCacheMeta        localLogCacheMeta
}

type SecurityOptions struct {
	// MountLabel contains the options for the "mount" command.
	MountLabel      string
	ProcessLabel    string
	AppArmorProfile string
	SeccompProfile  string
	NoNewPrivileges bool
	WritableCgroups *bool
}

type localLogCacheMeta struct {
	HaveNotifyEnabled bool
}

// NewBaseContainer creates a new container with its
// basic configuration.
func NewBaseContainer(id, root string) *Container {
	return &Container{
		ID:            id,
		State:         &State{},
		ExecCommands:  NewExecStore(),
		Root:          root,
		MountPoints:   make(map[string]*volumemounts.MountPoint),
		StreamConfig:  stream.NewConfig(),
		attachContext: &attachContext{},
	}
}

// FromDisk loads the container configuration stored in the host.
func (container *Container) FromDisk() error {
	pth, err := container.ConfigPath()
	if err != nil {
		return err
	}

	jsonSource, err := os.Open(pth)
	if err != nil {
		return err
	}
	defer jsonSource.Close()

	dec := json.NewDecoder(jsonSource)

	// Load container settings
	if err := dec.Decode(container); err != nil {
		return err
	}

	if container.OS != "" {
		// OS was deprecated in favor of ImagePlatform
		// Make sure we migrate the OS to ImagePlatform.OS.
		if container.ImagePlatform.OS == "" {
			container.ImagePlatform.OS = container.OS //nolint:staticcheck // ignore SA1019: field is deprecated
		}
	} else {
		// Pre multiple-OS support containers have no OS set.
		// Assume it is the host platform.
		container.ImagePlatform = platforms.DefaultSpec()
		container.OS = container.ImagePlatform.OS //nolint:staticcheck // ignore SA1019: field is deprecated
	}

	return container.readHostConfig()
}

// toDisk writes the container's configuration (config.v2.json, hostconfig.json)
// to disk and returns a deep copy.
func (container *Container) toDisk() (*Container, error) {
	pth, err := container.ConfigPath()
	if err != nil {
		return nil, err
	}

	// Save container settings
	f, err := atomicwriter.New(pth, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := io.MultiWriter(&buf, f)
	if err := json.NewEncoder(w).Encode(container); err != nil {
		return nil, err
	}

	var deepCopy Container
	if err := json.NewDecoder(&buf).Decode(&deepCopy); err != nil {
		return nil, err
	}
	deepCopy.HostConfig, err = container.WriteHostConfig()
	if err != nil {
		return nil, err
	}
	return &deepCopy, nil
}

// CheckpointTo makes the Container's current state visible to queries, and persists state.
// Callers must hold a Container lock.
func (container *Container) CheckpointTo(ctx context.Context, store *ViewDB) error {
	ctx, span := otel.Tracer("").Start(ctx, "container.CheckpointTo", trace.WithAttributes(
		attribute.String("container.ID", container.ID),
		attribute.String("container.Name", container.Name)))
	defer span.End()

	deepCopy, err := container.toDisk()
	if err != nil {
		return err
	}
	return store.Save(deepCopy)
}

// readHostConfig reads the host configuration from disk for the container.
func (container *Container) readHostConfig() error {
	container.HostConfig = &containertypes.HostConfig{}
	// If the hostconfig file does not exist, do not read it.
	// (We still have to initialize container.HostConfig,
	// but that's OK, since we just did that above.)
	pth, err := container.HostConfigPath()
	if err != nil {
		return err
	}

	f, err := os.Open(pth)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&container.HostConfig); err != nil {
		return err
	}

	container.InitDNSHostConfig()
	container.BackfillEmptyPBs()

	return nil
}

// WriteHostConfig saves the host configuration on disk for the container,
// and returns a deep copy of the saved object. Callers must hold a Container lock.
func (container *Container) WriteHostConfig() (*containertypes.HostConfig, error) {
	var (
		buf      bytes.Buffer
		deepCopy containertypes.HostConfig
	)

	pth, err := container.HostConfigPath()
	if err != nil {
		return nil, err
	}

	f, err := atomicwriter.New(pth, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	w := io.MultiWriter(&buf, f)
	if err := json.NewEncoder(w).Encode(&container.HostConfig); err != nil {
		return nil, err
	}

	if err := json.NewDecoder(&buf).Decode(&deepCopy); err != nil {
		return nil, err
	}
	return &deepCopy, nil
}

// CommitInMemory makes the Container's current state visible to queries,
// but does not persist state.
//
// Callers must hold a Container lock.
func (container *Container) CommitInMemory(store *ViewDB) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(container); err != nil {
		return err
	}

	var deepCopy Container
	if err := json.NewDecoder(&buf).Decode(&deepCopy); err != nil {
		return err
	}

	buf.Reset()
	if err := json.NewEncoder(&buf).Encode(container.HostConfig); err != nil {
		return err
	}
	if err := json.NewDecoder(&buf).Decode(&deepCopy.HostConfig); err != nil {
		return err
	}

	return store.Save(&deepCopy)
}

// SetupWorkingDirectory sets up the container's working directory as set in container.Config.WorkingDir
func (container *Container) SetupWorkingDirectory(uid int, gid int) error {
	if container.Config.WorkingDir == "" {
		return nil
	}

	workdir := filepath.Clean(container.Config.WorkingDir)
	pth, err := container.GetResourcePath(workdir)
	if err != nil {
		return err
	}

	if err := user.MkdirAllAndChown(pth, 0o755, uid, gid, user.WithOnlyNew); err != nil {
		pthInfo, err2 := os.Stat(pth)
		if err2 == nil && pthInfo != nil && !pthInfo.IsDir() {
			return errors.Errorf("Cannot mkdir: %s is not a directory", container.Config.WorkingDir)
		}

		return err
	}

	return nil
}

// GetResourcePath evaluates `path` in the scope of the container's BaseFS, with proper path
// sanitization. Symlinks are all scoped to the BaseFS of the container, as
// though the container's BaseFS was `/`.
//
// The BaseFS of a container is the host-facing path which is bind-mounted as
// `/` inside the container. This method is essentially used to access a
// particular path inside the container as though you were a process in that
// container.
//
// # NOTE
// The returned path is *only* safely scoped inside the container's BaseFS
// if no component of the returned path changes (such as a component
// symlinking to a different path) between using this method and using the
// path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetResourcePath(path string) (string, error) {
	if container.BaseFS == "" {
		return "", errors.New("GetResourcePath: BaseFS of container " + container.ID + " is unexpectedly empty")
	}
	// IMPORTANT - These are paths on the OS where the daemon is running, hence
	// any filepath operations must be done in an OS-agnostic way.
	r, e := symlink.FollowSymlinkInScope(filepath.Join(container.BaseFS, cleanScopedPath(path)), container.BaseFS)

	// Log this here on the daemon side as there's otherwise no indication apart
	// from the error being propagated all the way back to the client. This makes
	// debugging significantly easier and clearly indicates the error comes from the daemon.
	if e != nil {
		log.G(context.TODO()).Errorf("Failed to ResolveScopedPath BaseFS %s path %s %s\n", container.BaseFS, path, e)
	}
	return r, e
}

// cleanScopedPath prepares the given path to be combined with a mount path or
// a drive-letter. On Windows, it removes any existing driveletter (e.g. "C:").
// The returned path is always prefixed with a [filepath.Separator].
func cleanScopedPath(path string) string {
	if len(path) >= 2 {
		if v := filepath.VolumeName(path); v != "" {
			path = path[len(v):]
		}
	}
	return filepath.Join(string(filepath.Separator), path)
}

// GetRootResourcePath evaluates `path` in the scope of the container's root, with proper path
// sanitization. Symlinks are all scoped to the root of the container, as
// though the container's root was `/`.
//
// The root of a container is the host-facing configuration metadata directory.
// Only use this method to safely access the container's `container.json` or
// other metadata files. If in doubt, use container.GetResourcePath.
//
// # NOTE
// The returned path is *only* safely scoped inside the container's root
// if no component of the returned path changes (such as a component
// symlinking to a different path) between using this method and using the
// path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetRootResourcePath(path string) (string, error) {
	// IMPORTANT - These are paths on the OS where the daemon is running, hence
	// any filepath operations must be done in an OS-agnostic way.
	cleanPath := filepath.Join(string(os.PathSeparator), path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.Root, cleanPath), container.Root)
}

// ExitOnNext signals to the monitor that it should not restart the container
// after we send the kill signal.
func (container *Container) ExitOnNext() {
	if !container.HostConfig.RestartPolicy.IsNone() {
		log.G(context.TODO()).WithField("container", container.ID).Info("stopping restart-manager")
	}
	// If the container does not have a restartmanager, one is created (through
	// [container.RestartManager]), which is needed to handle daemon shutdown.
	container.RestartManager().Cancel()
}

// HostConfigPath returns the path to the container's JSON hostconfig
func (container *Container) HostConfigPath() (string, error) {
	return container.GetRootResourcePath(hostConfigFileName)
}

// ConfigPath returns the path to the container's JSON config
func (container *Container) ConfigPath() (string, error) {
	return container.GetRootResourcePath(configFileName)
}

// CheckpointDir returns the directory checkpoints are stored in
func (container *Container) CheckpointDir() string {
	return filepath.Join(container.Root, "checkpoints")
}

// StartLogger starts a new logger driver for the container.
func (container *Container) StartLogger() (logger.Logger, error) {
	cfg := container.HostConfig.LogConfig
	initDriver, err := logger.GetLogDriver(cfg.Type)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get logging factory")
	}
	info := logger.Info{
		Config:              cfg.Config,
		ContainerID:         container.ID,
		ContainerName:       container.Name,
		ContainerEntrypoint: container.Path,
		ContainerArgs:       container.Args,
		ContainerImageID:    container.ImageID.String(),
		ContainerImageName:  container.Config.Image,
		ContainerCreated:    container.Created,
		ContainerEnv:        container.Config.Env,
		ContainerLabels:     container.Config.Labels,
		DaemonName:          "docker",
	}

	// Set logging file for "json-logger"
	// TODO(@cpuguy83): Setup here based on log driver is a little weird.
	switch cfg.Type {
	case jsonfilelog.Name:
		info.LogPath, err = container.GetRootResourcePath(container.ID + "-json.log")
		if err != nil {
			return nil, err
		}

		container.LogPath = info.LogPath
	case local.Name:
		// Do not set container.LogPath for the local driver
		// This would expose the value to the API, which should not be done as it means
		// that the log file implementation would become a stable API that cannot change.
		logDir, err := container.GetRootResourcePath("local-logs")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return nil, errdefs.System(errors.Wrap(err, "error creating local logs dir"))
		}
		info.LogPath = filepath.Join(logDir, "container.log")
	}

	l, err := initDriver(info)
	if err != nil {
		return nil, err
	}

	if containertypes.LogMode(cfg.Config["mode"]) == containertypes.LogModeNonBlock {
		bufferSize := int64(-1)
		if s, exists := cfg.Config["max-buffer-size"]; exists {
			bufferSize, err = units.RAMInBytes(s)
			if err != nil {
				return nil, err
			}
		}
		l = logger.NewRingLogger(l, info, bufferSize)
	}

	if _, ok := l.(logger.LogReader); !ok {
		if cache.ShouldUseCache(cfg.Config) {
			logPath, err := container.GetRootResourcePath("container-cached.log")
			if err != nil {
				return nil, err
			}

			if !container.LocalLogCacheMeta.HaveNotifyEnabled {
				log.G(context.TODO()).WithField("container", container.ID).WithField("driver", container.HostConfig.LogConfig.Type).Info("Configured log driver does not support reads, enabling local file cache for container logs")
				container.LocalLogCacheMeta.HaveNotifyEnabled = true
			}
			info.LogPath = logPath
			l, err = cache.WithLocalCache(l, info)
			if err != nil {
				return nil, errors.Wrap(err, "error setting up local container log cache")
			}
		}
	}
	return l, nil
}

// GetProcessLabel returns the process label for the container.
func (container *Container) GetProcessLabel() string {
	// even if we have a process label return "" if we are running
	// in privileged mode
	if container.HostConfig.Privileged {
		return ""
	}
	return container.ProcessLabel
}

// GetMountLabel returns the mounting label for the container.
// This label is empty if the container is privileged.
func (container *Container) GetMountLabel() string {
	return container.MountLabel
}

// GetExecIDs returns the list of exec commands running on the container.
func (container *Container) GetExecIDs() []string {
	return container.ExecCommands.List()
}

// ShouldRestart decides whether the daemon should restart the container or not.
// This is based on the container's restart policy.
func (container *Container) ShouldRestart() bool {
	shouldRestart, _, _ := container.RestartManager().ShouldRestart(
		uint32(container.State.ExitCode),
		container.HasBeenManuallyStopped,
		container.State.FinishedAt.Sub(container.State.StartedAt),
	)
	return shouldRestart
}

// AddMountPointWithVolume adds a new mount point configured with a volume to the container.
func (container *Container) AddMountPointWithVolume(destination string, vol volume.Volume, rw bool) {
	volumeParser := volumemounts.NewParser()
	container.MountPoints[destination] = &volumemounts.MountPoint{
		Type:        mounttypes.TypeVolume,
		Name:        vol.Name(),
		Driver:      vol.DriverName(),
		Destination: destination,
		RW:          rw,
		Volume:      vol,
		CopyData:    volumeParser.DefaultCopyMode(),
	}
}

// UnmountVolumes unmounts all volumes
func (container *Container) UnmountVolumes(ctx context.Context, volumeEventLog func(name string, action events.Action, attributes map[string]string)) error {
	var errs []string
	for _, volumeMount := range container.MountPoints {
		if volumeMount.Volume == nil {
			continue
		}

		if err := volumeMount.Cleanup(ctx); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		volumeEventLog(volumeMount.Volume.Name(), events.ActionUnmount, map[string]string{
			"driver":    volumeMount.Volume.DriverName(),
			"container": container.ID,
		})
	}
	if len(errs) > 0 {
		return fmt.Errorf("error while unmounting volumes for container %s: %s", container.ID, strings.Join(errs, "; "))
	}
	return nil
}

// IsDestinationMounted checks whether a path is mounted on the container or not.
func (container *Container) IsDestinationMounted(destination string) bool {
	return container.MountPoints[destination] != nil
}

// StopSignal returns the signal used to stop the container.
func (container *Container) StopSignal() syscall.Signal {
	stopSignal := defaultStopSignal
	if container.Config.StopSignal != "" {
		// signal.ParseSignal returns "-1" for invalid or unknown signals.
		sig, err := signal.ParseSignal(container.Config.StopSignal)
		if err == nil && sig > 0 {
			stopSignal = sig
		}
	}
	return stopSignal
}

// InitDNSHostConfig ensures that the dns fields are never nil.
// New containers don't ever have those fields nil,
// but pre created containers can still have those nil values.
// The non-recommended host configuration in the start api can
// make these fields nil again, this corrects that issue until
// we remove that behavior for good.
// See https://github.com/moby/moby/pull/17779
// for a more detailed explanation on why we don't want that.
func (container *Container) InitDNSHostConfig() {
	container.Lock()
	defer container.Unlock()
	if container.HostConfig.DNS == nil {
		container.HostConfig.DNS = make([]netip.Addr, 0)
	}

	if container.HostConfig.DNSSearch == nil {
		container.HostConfig.DNSSearch = make([]string, 0)
	}

	if container.HostConfig.DNSOptions == nil {
		container.HostConfig.DNSOptions = make([]string, 0)
	}
}

// BackfillEmptyPBs backfills empty PortBindings slices with a port binding
// with an unspecified HostIP and HostPort. This is required to ensure backward
// compatibility for containers created with older Engine versions.
//
// Prior to v29.0, the daemon was doing this backfilling automatically through
// github.com/docker/go-connections/nat on ContainerStart. Starting with that
// version, it is done by the API for older API versions (i.e. < 1.53). Newer
// API versions are just dropping entries with empty PortBindings slices.
//
// See https://github.com/moby/moby/pull/50710#discussion_r2315840899 for more
// context.
func (container *Container) BackfillEmptyPBs() {
	if container.HostConfig == nil {
		return
	}
	if container.HostConfig.PortBindings == nil {
		container.HostConfig.PortBindings = networktypes.PortMap{}
		return
	}
	for portProto, pb := range container.HostConfig.PortBindings {
		if len(pb) > 0 || pb == nil {
			continue
		}
		container.HostConfig.PortBindings[portProto] = []networktypes.PortBinding{
			{}, // Backfill an empty PortBinding
		}
	}
}

// UpdateMonitor updates monitor configure for running container
func (container *Container) UpdateMonitor(restartPolicy containertypes.RestartPolicy) {
	container.RestartManager().SetPolicy(restartPolicy)
}

// FullHostname returns hostname and optional domain appended to it.
func (container *Container) FullHostname() string {
	fullHostname := container.Config.Hostname
	if container.Config.Domainname != "" {
		fullHostname = fmt.Sprintf("%s.%s", fullHostname, container.Config.Domainname)
	}
	return fullHostname
}

// RestartManager returns the current restartmanager instance connected to container.
func (container *Container) RestartManager() *restartmanager.RestartManager {
	if container.restartManager == nil {
		container.restartManager = restartmanager.New(container.HostConfig.RestartPolicy, container.RestartCount)
	}
	return container.restartManager
}

// ResetRestartManager initializes new restartmanager based on container config
func (container *Container) ResetRestartManager(resetCount bool) {
	if container.restartManager != nil {
		container.restartManager.Cancel()
	}
	if resetCount {
		container.RestartCount = 0
	}
	container.restartManager = nil
}

// AttachContext returns the context for attach calls to track container liveness.
func (container *Container) AttachContext() context.Context {
	return container.attachContext.init()
}

// CancelAttachContext cancels attach context. All attach calls should detach
// after this call.
func (container *Container) CancelAttachContext() {
	container.attachContext.cancel()
}

func (container *Container) startLogging() error {
	if container.HostConfig.LogConfig.Type == "none" {
		return nil // do not start logging routines
	}

	l, err := container.StartLogger()
	if err != nil {
		return fmt.Errorf("failed to initialize logging driver: %v", err)
	}

	copier := logger.NewCopier(map[string]io.Reader{"stdout": container.StdoutPipe(), "stderr": container.StderrPipe()}, l)
	container.LogCopier = copier
	copier.Run()
	container.LogDriver = l

	return nil
}

// StdinPipe gets the stdin stream of the container
func (container *Container) StdinPipe() io.WriteCloser {
	return container.StreamConfig.StdinPipe()
}

// StdoutPipe gets the stdout stream of the container
func (container *Container) StdoutPipe() io.ReadCloser {
	return container.StreamConfig.StdoutPipe()
}

// StderrPipe gets the stderr stream of the container
func (container *Container) StderrPipe() io.ReadCloser {
	return container.StreamConfig.StderrPipe()
}

// CloseStreams closes the container's stdio streams
func (container *Container) CloseStreams() error {
	return container.StreamConfig.CloseStreams()
}

// InitializeStdio is called by libcontainerd to connect the stdio.
func (container *Container) InitializeStdio(iop *cio.DirectIO) (cio.IO, error) {
	if err := container.startLogging(); err != nil {
		container.Reset()
		return nil, err
	}

	container.StreamConfig.CopyToPipe(iop)

	if container.StreamConfig.Stdin() == nil && !container.Config.Tty {
		if iop.Stdin != nil {
			if err := iop.Stdin.Close(); err != nil {
				log.G(context.TODO()).Warnf("error closing stdin: %+v", err)
			}
		}
	}

	return &rio{IO: iop, sc: container.StreamConfig}, nil
}

// MountsResourcePath returns the path where mounts are stored for the given mount
func (container *Container) MountsResourcePath(mount string) (string, error) {
	return container.GetRootResourcePath(filepath.Join("mounts", mount))
}

// SecretMountPath returns the path of the secret mount for the container
func (container *Container) SecretMountPath() (string, error) {
	return container.MountsResourcePath("secrets")
}

// SecretFilePath returns the path to the location of a secret on the host.
func (container *Container) SecretFilePath(secretRef swarmtypes.SecretReference) (string, error) {
	secrets, err := container.SecretMountPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(secrets, secretRef.SecretID), nil
}

func getSecretTargetPath(r *swarmtypes.SecretReference) string {
	if filepath.IsAbs(r.File.Name) {
		return r.File.Name
	}

	return filepath.Join(containerSecretMountPath, r.File.Name)
}

// getConfigTargetPath makes sure that config paths inside the container are
// absolute, as required by the runtime spec, and enforced by runc >= 1.0.0-rc94.
// see https://github.com/opencontainers/runc/issues/2928
func getConfigTargetPath(r *swarmtypes.ConfigReference) string {
	if filepath.IsAbs(r.File.Name) {
		return r.File.Name
	}

	return filepath.Join(containerConfigMountPath, r.File.Name)
}

// CreateDaemonEnvironment creates a new environment variable slice for this container.
func (container *Container) CreateDaemonEnvironment(tty bool, linkedEnv []string) []string {
	// Setup environment
	ctrOS := container.ImagePlatform.OS
	if ctrOS == "" {
		ctrOS = runtime.GOOS
	}

	// Figure out what size slice we need so we can allocate this all at once.
	envSize := len(container.Config.Env)
	if runtime.GOOS != "windows" {
		envSize += 2 + len(linkedEnv)
	}
	if tty {
		envSize++
	}

	env := make([]string, 0, envSize)
	if runtime.GOOS != "windows" {
		env = append(env, "PATH="+oci.DefaultPathEnv(ctrOS))
		env = append(env, "HOSTNAME="+container.Config.Hostname)
		if tty {
			env = append(env, "TERM=xterm")
		}
		env = append(env, linkedEnv...)
	}

	// because the env on the container can override certain default values
	// we need to replace the 'env' keys where they match and append anything
	// else.
	env = ReplaceOrAppendEnvValues(env, container.Config.Env)
	return env
}

// RestoreTask restores the containerd container and task handles and reattaches
// the IO for the running task. Container state is not synced with containerd's
// state.
//
// An errdefs.NotFound error is returned if the container does not exist in
// containerd. However, a nil error is returned if the task does not exist in
// containerd.
func (container *Container) RestoreTask(ctx context.Context, client libcontainerdtypes.Client) error {
	container.Lock()
	defer container.Unlock()
	var err error
	container.State.ctr, err = client.LoadContainer(ctx, container.ID)
	if err != nil {
		return err
	}
	container.State.task, err = container.State.ctr.AttachTask(ctx, container.InitializeStdio)
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// GetRunningTask asserts that the container is running and returns the Task for
// the container. An errdefs.Conflict error is returned if the container is not
// in the Running state.
//
// A system error is returned if container is in a bad state: Running is true
// but has a nil Task.
//
// The container lock must be held when calling this method.
func (container *Container) GetRunningTask() (libcontainerdtypes.Task, error) {
	if !container.State.Running {
		return nil, errdefs.Conflict(fmt.Errorf("container %s is not running", container.ID))
	}
	tsk, ok := container.State.Task()
	if !ok {
		return nil, errdefs.System(errors.WithStack(fmt.Errorf("container %s is in Running state but has no containerd Task set", container.ID)))
	}
	return tsk, nil
}

type rio struct {
	cio.IO

	sc *stream.Config
}

func (i *rio) Close() error {
	i.IO.Close()

	return i.sc.CloseStreams()
}

func (i *rio) Wait() {
	i.sc.Wait(context.Background())

	i.IO.Wait()
}

type conflictingUpdateOptions string

func (e conflictingUpdateOptions) Error() string {
	return string(e)
}

func (e conflictingUpdateOptions) Conflict() {}

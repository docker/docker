package container // import "github.com/docker/docker/container"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/cio"
	containertypes "github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/container/stream"
	"github.com/docker/docker/daemon/exec"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/local"
	"github.com/docker/docker/daemon/logger/loggerutils/cache"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/restartmanager"
	"github.com/docker/docker/volume"
	volumemounts "github.com/docker/docker/volume/mounts"
	units "github.com/docker/go-units"
	agentexec "github.com/docker/swarmkit/agent/exec"
	"github.com/moby/sys/symlink"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	configFileName     = "config.v2.json"
	hostConfigFileName = "hostconfig.json"
)

// ExitStatus provides exit reasons for a container.
type ExitStatus struct {
	// The exit code with which the container exited.
	ExitCode int

	// Whether the container encountered an OOM.
	OOMKilled bool

	// Time at which the container died
	ExitedAt time.Time
}

// Container holds the structure defining a container object.
type Container struct {
	StreamConfig *stream.Config
	// embed for Container to support states directly.
	*State          `json:"State"`          // Needed for Engine API version <= 1.11
	Root            string                  `json:"-"` // Path to the "home" of the container, including metadata.
	BaseFS          containerfs.ContainerFS `json:"-"` // interface containing graphdriver mount
	RWLayer         layer.RWLayer           `json:"-"`
	ID              string
	Created         time.Time
	Managed         bool
	Path            string
	Args            []string
	Config          *containertypes.Config
	ImageID         image.ID `json:"Image"`
	NetworkSettings *network.Settings
	LogPath         string
	Name            string
	Driver          string
	OS              string
	// MountLabel contains the options for the 'mount' command
	MountLabel             string
	ProcessLabel           string
	RestartCount           int
	HasBeenStartedBefore   bool
	HasBeenManuallyStopped bool // used for unless-stopped restart policy
	MountPoints            map[string]*volumemounts.MountPoint
	HostConfig             *containertypes.HostConfig `json:"-"` // do not serialize the host config in the json, otherwise we'll make the container unportable
	ExecCommands           *exec.Store                `json:"-"`
	DependencyStore        agentexec.DependencyGetter `json:"-"`
	SecretReferences       []*swarmtypes.SecretReference
	ConfigReferences       []*swarmtypes.ConfigReference
	// logDriver for closing
	LogDriver      logger.Logger  `json:"-"`
	LogCopier      *logger.Copier `json:"-"`
	restartManager restartmanager.RestartManager
	attachContext  *attachContext

	// Fields here are specific to Unix platforms
	AppArmorProfile string
	HostnamePath    string
	HostsPath       string
	ShmPath         string
	ResolvConfPath  string
	SeccompProfile  string
	NoNewPrivileges bool

	// Fields here are specific to Windows
	NetworkSharedContainerID string            `json:"-"`
	SharedEndpointList       []string          `json:"-"`
	LocalLogCacheMeta        localLogCacheMeta `json:",omitempty"`
}

type localLogCacheMeta struct {
	HaveNotifyEnabled bool
}

// NewBaseContainer creates a new container with its
// basic configuration.
func NewBaseContainer(id, root string) *Container {
	return &Container{
		ID:            id,
		State:         NewState(),
		ExecCommands:  exec.NewStore(),
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

	// Ensure the operating system is set if blank. Assume it is the OS of the
	// host OS if not, to ensure containers created before multiple-OS
	// support are migrated
	if container.OS == "" {
		container.OS = runtime.GOOS
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
	f, err := ioutils.NewAtomicFileWriter(pth, 0600)
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
func (container *Container) CheckpointTo(store ViewDB) error {
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

	f, err := ioutils.NewAtomicFileWriter(pth, 0600)
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

// SetupWorkingDirectory sets up the container's working directory as set in container.Config.WorkingDir
func (container *Container) SetupWorkingDirectory(rootIdentity idtools.Identity) error {
	// TODO: LCOW Support. This will need revisiting.
	// We will need to do remote filesystem operations here.
	if container.OS != runtime.GOOS {
		return nil
	}

	if container.Config.WorkingDir == "" {
		return nil
	}

	container.Config.WorkingDir = filepath.Clean(container.Config.WorkingDir)
	pth, err := container.GetResourcePath(container.Config.WorkingDir)
	if err != nil {
		return err
	}

	if err := idtools.MkdirAllAndChownNew(pth, 0755, rootIdentity); err != nil {
		pthInfo, err2 := os.Stat(pth)
		if err2 == nil && pthInfo != nil && !pthInfo.IsDir() {
			return errors.Errorf("Cannot mkdir: %s is not a directory", container.Config.WorkingDir)
		}

		return err
	}

	return nil
}

// GetResourcePath evaluates `path` in the scope of the container's BaseFS, with proper path
// sanitisation. Symlinks are all scoped to the BaseFS of the container, as
// though the container's BaseFS was `/`.
//
// The BaseFS of a container is the host-facing path which is bind-mounted as
// `/` inside the container. This method is essentially used to access a
// particular path inside the container as though you were a process in that
// container.
//
// NOTE: The returned path is *only* safely scoped inside the container's BaseFS
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetResourcePath(path string) (string, error) {
	if container.BaseFS == nil {
		return "", errors.New("GetResourcePath: BaseFS of container " + container.ID + " is unexpectedly nil")
	}
	// IMPORTANT - These are paths on the OS where the daemon is running, hence
	// any filepath operations must be done in an OS agnostic way.
	r, e := container.BaseFS.ResolveScopedPath(path, false)

	// Log this here on the daemon side as there's otherwise no indication apart
	// from the error being propagated all the way back to the client. This makes
	// debugging significantly easier and clearly indicates the error comes from the daemon.
	if e != nil {
		logrus.Errorf("Failed to ResolveScopedPath BaseFS %s path %s %s\n", container.BaseFS.Path(), path, e)
	}
	return r, e
}

// GetRootResourcePath evaluates `path` in the scope of the container's root, with proper path
// sanitisation. Symlinks are all scoped to the root of the container, as
// though the container's root was `/`.
//
// The root of a container is the host-facing configuration metadata directory.
// Only use this method to safely access the container's `container.json` or
// other metadata files. If in doubt, use container.GetResourcePath.
//
// NOTE: The returned path is *only* safely scoped inside the container's root
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetRootResourcePath(path string) (string, error) {
	// IMPORTANT - These are paths on the OS where the daemon is running, hence
	// any filepath operations must be done in an OS agnostic way.
	cleanPath := filepath.Join(string(os.PathSeparator), path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.Root, cleanPath), container.Root)
}

// ExitOnNext signals to the monitor that it should not restart the container
// after we send the kill signal.
func (container *Container) ExitOnNext() {
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
		info.LogPath, err = container.GetRootResourcePath(fmt.Sprintf("%s-json.log", container.ID))
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
		if err := os.MkdirAll(logDir, 0700); err != nil {
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
				logrus.WithField("container", container.ID).WithField("driver", container.HostConfig.LogConfig.Type).Info("Configured log driver does not support reads, enabling local file cache for container logs")
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
	shouldRestart, _, _ := container.RestartManager().ShouldRestart(uint32(container.ExitCode()), container.HasBeenManuallyStopped, container.FinishedAt.Sub(container.StartedAt))
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
func (container *Container) UnmountVolumes(volumeEventLog func(name, action string, attributes map[string]string)) error {
	var errors []string
	for _, volumeMount := range container.MountPoints {
		if volumeMount.Volume == nil {
			continue
		}

		if err := volumeMount.Cleanup(); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		attributes := map[string]string{
			"driver":    volumeMount.Volume.DriverName(),
			"container": container.ID,
		}
		volumeEventLog(volumeMount.Volume.Name(), "unmount", attributes)
	}
	if len(errors) > 0 {
		return fmt.Errorf("error while unmounting volumes for container %s: %s", container.ID, strings.Join(errors, "; "))
	}
	return nil
}

// IsDestinationMounted checks whether a path is mounted on the container or not.
func (container *Container) IsDestinationMounted(destination string) bool {
	return container.MountPoints[destination] != nil
}

// StopSignal returns the signal used to stop the container.
func (container *Container) StopSignal() int {
	var stopSignal syscall.Signal
	if container.Config.StopSignal != "" {
		stopSignal, _ = signal.ParseSignal(container.Config.StopSignal)
	}

	if int(stopSignal) == 0 {
		stopSignal, _ = signal.ParseSignal(signal.DefaultStopSignal)
	}
	return int(stopSignal)
}

// StopTimeout returns the timeout (in seconds) used to stop the container.
func (container *Container) StopTimeout() int {
	if container.Config.StopTimeout != nil {
		return *container.Config.StopTimeout
	}
	return DefaultStopTimeout
}

// InitDNSHostConfig ensures that the dns fields are never nil.
// New containers don't ever have those fields nil,
// but pre created containers can still have those nil values.
// The non-recommended host configuration in the start api can
// make these fields nil again, this corrects that issue until
// we remove that behavior for good.
// See https://github.com/docker/docker/pull/17779
// for a more detailed explanation on why we don't want that.
func (container *Container) InitDNSHostConfig() {
	container.Lock()
	defer container.Unlock()
	if container.HostConfig.DNS == nil {
		container.HostConfig.DNS = make([]string, 0)
	}

	if container.HostConfig.DNSSearch == nil {
		container.HostConfig.DNSSearch = make([]string, 0)
	}

	if container.HostConfig.DNSOptions == nil {
		container.HostConfig.DNSOptions = make([]string, 0)
	}
}

// UpdateMonitor updates monitor configure for running container
func (container *Container) UpdateMonitor(restartPolicy containertypes.RestartPolicy) {
	type policySetter interface {
		SetPolicy(containertypes.RestartPolicy)
	}

	if rm, ok := container.RestartManager().(policySetter); ok {
		rm.SetPolicy(restartPolicy)
	}
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
func (container *Container) RestartManager() restartmanager.RestartManager {
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

type attachContext struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

// InitAttachContext initializes or returns existing context for attach calls to
// track container liveness.
func (container *Container) InitAttachContext() context.Context {
	container.attachContext.mu.Lock()
	defer container.attachContext.mu.Unlock()
	if container.attachContext.ctx == nil {
		container.attachContext.ctx, container.attachContext.cancel = context.WithCancel(context.Background())
	}
	return container.attachContext.ctx
}

// CancelAttachContext cancels attach context. All attach calls should detach
// after this call.
func (container *Container) CancelAttachContext() {
	container.attachContext.mu.Lock()
	if container.attachContext.ctx != nil {
		container.attachContext.cancel()
		container.attachContext.ctx = nil
	}
	container.attachContext.mu.Unlock()
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
		container.Reset(false)
		return nil, err
	}

	container.StreamConfig.CopyToPipe(iop)

	if container.StreamConfig.Stdin() == nil && !container.Config.Tty {
		if iop.Stdin != nil {
			if err := iop.Stdin.Close(); err != nil {
				logrus.Warnf("error closing stdin: %+v", err)
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
	os := container.OS
	if os == "" {
		os = runtime.GOOS
	}

	// Figure out what size slice we need so we can allocate this all at once.
	envSize := len(container.Config.Env)
	if runtime.GOOS != "windows" || (runtime.GOOS == "windows" && os == "linux") {
		envSize += 2 + len(linkedEnv)
	}
	if tty {
		envSize++
	}

	env := make([]string, 0, envSize)
	if runtime.GOOS != "windows" {
		env = append(env, "PATH="+system.DefaultPathEnv(os))
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

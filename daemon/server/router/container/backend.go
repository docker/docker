package container

import (
	"context"
	"io"

	"github.com/moby/moby/v2/daemon/internal/pruneprogress"

	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	containerpkg "github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/pkg/sysinfo"
)

// execBackend includes functions to implement to provide exec functionality.
type execBackend interface {
	ContainerExecCreate(name string, options *container.ExecCreateRequest) (string, error)
	ContainerExecInspect(id string) (*container.ExecInspectResponse, error)
	ContainerExecResize(ctx context.Context, name string, height, width uint32) error
	ContainerExecStart(ctx context.Context, name string, options backend.ExecStartConfig) error
	ExecExists(name string) (bool, error)
}

// copyBackend includes functions to implement to provide container copy functionality.
type copyBackend interface {
	ContainerArchivePath(name string, path string) (content io.ReadCloser, stat *container.PathStat, err error)
	ContainerExport(ctx context.Context, name string, out io.Writer) error
	ContainerExtractToDir(name, path string, copyUIDGID, allowOverwriteDirWithFile bool, content io.Reader) error
	ContainerStatPath(name string, path string) (stat *container.PathStat, err error)
}

// stateBackend includes functions to implement to provide container state lifecycle functionality.
type stateBackend interface {
	ContainerCreate(ctx context.Context, config backend.ContainerCreateConfig) (container.CreateResponse, error)
	ContainerKill(ctx context.Context, name string, signal string) error
	ContainerPause(name string) error
	ContainerRename(oldName, newName string) error
	ContainerResize(ctx context.Context, name string, height, width uint32) error
	ContainerRestart(ctx context.Context, name string, options backend.ContainerStopOptions) error
	ContainerRm(name string, config *backend.ContainerRmConfig) error
	ContainerStart(ctx context.Context, name string, checkpoint string, checkpointDir string) error
	ContainerStop(ctx context.Context, name string, options backend.ContainerStopOptions) error
	ContainerUnpause(name string) error
	ContainerUpdate(name string, hostConfig *container.HostConfig) (container.UpdateResponse, error)
	ContainerWait(ctx context.Context, name string, condition container.WaitCondition) (<-chan containerpkg.StateStatus, error)
}

// monitorBackend includes functions to implement to provide containers monitoring functionality.
type monitorBackend interface {
	ContainerChanges(ctx context.Context, name string) ([]archive.Change, error)
	ContainerInspect(ctx context.Context, name string, options backend.ContainerInspectOptions) (_ *container.InspectResponse, desiredMACAddress network.HardwareAddr, _ error)
	ContainerLogs(ctx context.Context, name string, config *backend.ContainerLogsOptions) (msgs <-chan *backend.LogMessage, tty bool, err error)
	ContainerStats(ctx context.Context, name string, config *backend.ContainerStatsConfig) error
	ContainerTop(name string, psArgs string) (*container.TopResponse, error)
	Containers(ctx context.Context, config *backend.ContainerListOptions) ([]container.Summary, error)
}

// attachBackend includes function to implement to provide container attaching functionality.
type attachBackend interface {
	ContainerAttach(name string, c *backend.ContainerAttachConfig) error
}

// systemBackend includes functions to implement to provide system wide containers functionality
type systemBackend interface {
	ContainerPrune(ctx context.Context, opts pruneprogress.Options) (*container.PruneReport, error)
}

type commitBackend interface {
	CreateImageFromContainer(ctx context.Context, name string, config *backend.CreateImageConfig) (imageID string, err error)
}

type sysInfoProvider interface {
	RawSysInfo() (*sysinfo.SysInfo, error)
}

// Backend is all the methods that need to be implemented to provide container specific functionality.
type Backend interface {
	commitBackend
	execBackend
	copyBackend
	stateBackend
	monitorBackend
	attachBackend
	systemBackend
	sysInfoProvider
}

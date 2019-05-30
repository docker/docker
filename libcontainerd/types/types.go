package types // import "github.com/docker/docker/libcontainerd/types"

import (
	"context"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// EventType represents a possible event from libcontainerd
type EventType string

// Event constants used when reporting events
const (
	EventUnknown     EventType = "unknown"
	EventExit        EventType = "exit"
	EventOOM         EventType = "oom"
	EventCreate      EventType = "create"
	EventStart       EventType = "start"
	EventExecAdded   EventType = "exec-added"
	EventExecStarted EventType = "exec-started"
	EventPaused      EventType = "paused"
	EventResumed     EventType = "resumed"
)

// EventInfo contains the event info
type EventInfo struct {
	ContainerID string
	ProcessID   string
	Pid         uint32
	ExitCode    uint32
	ExitedAt    time.Time
	OOMKilled   bool
	Error       error
}

// Backend defines callbacks that the client of the library needs to implement.
type Backend interface {
	ProcessEvent(containerID string, event EventType, ei EventInfo) error
}

// Process of a container
type Process interface {
	Delete(context.Context) (uint32, time.Time, error)
}

// Client provides access to containerd features.
type Client interface {
	Version(ctx context.Context) (containerd.Version, error)

	Restore(ctx context.Context, containerID string, attachStdio StdioCallback) (alive bool, pid int, p Process, err error)

	Create(ctx context.Context, containerID string, spec *specs.Spec, runtimeOptions interface{}) error
	Start(ctx context.Context, containerID, checkpointDir string, withStdin bool, attachStdio StdioCallback) (pid int, err error)
	SignalProcess(ctx context.Context, containerID, processID string, signal int) error
	Exec(ctx context.Context, containerID, processID string, spec *specs.Process, withStdin bool, attachStdio StdioCallback) (int, error)
	ResizeTerminal(ctx context.Context, containerID, processID string, width, height int) error
	CloseStdin(ctx context.Context, containerID, processID string) error
	Pause(ctx context.Context, containerID string) error
	Resume(ctx context.Context, containerID string) error
	Stats(ctx context.Context, containerID string) (*Stats, error)
	ListPids(ctx context.Context, containerID string) ([]uint32, error)
	Summary(ctx context.Context, containerID string) ([]Summary, error)
	DeleteTask(ctx context.Context, containerID string) (uint32, time.Time, error)
	Delete(ctx context.Context, containerID string) error
	Status(ctx context.Context, containerID string) (containerd.ProcessStatus, error)

	UpdateResources(ctx context.Context, containerID string, resources *Resources) error
	CreateCheckpoint(ctx context.Context, containerID, checkpointDir string, exit bool) error
}

// StdioCallback is called to connect a container or process stdio.
type StdioCallback func(io *cio.DirectIO) (cio.IO, error)

// InitProcessName is the name given to the first process of a container
const InitProcessName = "init"

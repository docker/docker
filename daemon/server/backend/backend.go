// Package backend includes types to send information to server backends.
package backend

import (
	"io"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/filters"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ContainerCreateConfig is the parameter set to ContainerCreate()
type ContainerCreateConfig struct {
	Name                        string
	Config                      *container.Config
	HostConfig                  *container.HostConfig
	NetworkingConfig            *network.NetworkingConfig
	Platform                    *ocispec.Platform
	DefaultReadOnlyNonRecursive bool
}

// ContainerRmConfig holds arguments for the container remove
// operation. This struct is used to tell the backend what operations
// to perform.
type ContainerRmConfig struct {
	ForceRemove, RemoveVolume, RemoveLink bool
}

// ContainerAttachConfig holds the streams to use when connecting to a container to view logs.
type ContainerAttachConfig struct {
	GetStreams func(multiplexed bool, cancel func()) (io.ReadCloser, io.Writer, io.Writer, error)
	UseStdin   bool
	UseStdout  bool
	UseStderr  bool
	Logs       bool
	Stream     bool
	DetachKeys string
	// Used to signify that streams must be multiplexed by producer as endpoint can't manage multiple streams.
	// This is typically set by HTTP endpoint, while websocket can transport raw streams
	MuxStreams bool
}

// PartialLogMetaData provides meta data for a partial log message. Messages
// exceeding a predefined size are split into chunks with this metadata. The
// expectation is for the logger endpoints to assemble the chunks using this
// metadata.
type PartialLogMetaData struct {
	Last    bool   // true if this message is last of a partial
	ID      string // identifies group of messages comprising a single record
	Ordinal int    // ordering of message in partial group
}

// LogMessage is datastructure that represents piece of output produced by some
// container.  The Line member is a slice of an array whose contents can be
// changed after a log driver's Log() method returns.
type LogMessage struct {
	Line         []byte
	Source       string
	Timestamp    time.Time
	Attrs        []LogAttr
	PLogMetaData *PartialLogMetaData

	// Err is an error associated with a message. Completeness of a message
	// with Err is not expected, tho it may be partially complete (fields may
	// be missing, gibberish, or nil)
	Err error
}

// LogAttr is used to hold the extra attributes available in the log message.
type LogAttr struct {
	Key   string
	Value string
}

// LogSelector is a list of services and tasks that should be returned as part
// of a log stream. It is similar to swarmapi.LogSelector, with the difference
// that the names don't have to be resolved to IDs; this is mostly to avoid
// accidents later where a swarmapi LogSelector might have been incorrectly
// used verbatim (and to avoid the handler having to import swarmapi types)
type LogSelector struct {
	Services []string
	Tasks    []string
}

// ContainerStatsConfig holds information for configuring the runtime
// behavior of a backend.ContainerStats() call.
type ContainerStatsConfig struct {
	Stream    bool
	OneShot   bool
	OutStream func() io.Writer
}

// ContainerInspectOptions defines options for the backend.ContainerInspect
// call.
type ContainerInspectOptions struct {
	// Size controls whether to propagate the container's size fields.
	Size bool
}

// ContainerListOptions holds parameters to list containers with.
type ContainerListOptions struct {
	Size    bool
	All     bool
	Limit   int
	Filters filters.Args
}

// ContainerLogsOptions holds parameters to filter logs with.
type ContainerLogsOptions struct {
	ShowStdout bool
	ShowStderr bool
	// ShowExecStdout selects the stdout stream of execs whose output was
	// captured in the container's logs (execs created with CaptureStdout /
	// CaptureStderr).
	ShowExecStdout bool
	// ShowExecStderr selects the stderr stream of execs whose output was
	// captured in the container's logs (execs created with CaptureStdout /
	// CaptureStderr).
	ShowExecStderr bool
	Since          time.Time
	Until          time.Time
	Timestamps     bool
	Follow         bool
	Tail           string
	Details        bool
}

// ContainerStopOptions holds the options to stop or restart a container.
type ContainerStopOptions struct {
	// Signal (optional) is the signal to send to the container to (gracefully)
	// stop it before forcibly terminating the container with SIGKILL after the
	// timeout expires. If not value is set, the default (SIGTERM) is used.
	Signal string `json:",omitempty"`

	// Timeout (optional) is the timeout (in seconds) to wait for the container
	// to stop gracefully before forcibly terminating it with SIGKILL.
	//
	// - Use nil to use the container's configured timeout, or the engine default
	//   if the container has no configured timeout.
	// - Use '-1' to wait indefinitely.
	// - Use '0' to not wait for the container to exit gracefully, and
	//   immediately proceeds to forcibly terminating the container.
	// - Other positive values are used as timeout (in seconds).
	Timeout *int `json:",omitempty"`
}

// ExecStartConfig holds the options to start container's exec.
type ExecStartConfig struct {
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	ConsoleSize *[2]uint `json:",omitempty"`
}

// CreateImageConfig is the configuration for creating an image from a
// container.
type CreateImageConfig struct {
	Tag     reference.NamedTagged
	NoPause bool
	Author  string
	Comment string
	Config  *container.Config
	Changes []string
}

// CommitConfig is the configuration for creating an image as part of a build.
type CommitConfig struct {
	Author              string
	Comment             string
	Config              *container.Config // TODO(thaJeztah); change this to [dockerspec.DockerOCIImageConfig]
	ContainerConfig     *container.Config
	ContainerID         string
	ContainerMountLabel string
	ContainerOS         string
	ParentImageID       string
}

// PluginCreateConfig hold all options to plugin create.
type PluginCreateConfig struct {
	RepoName string
}

// PluginRmConfig holds arguments for plugin remove.
type PluginRmConfig struct {
	ForceRemove bool
}

// PluginEnableConfig holds arguments for plugin enable
type PluginEnableConfig struct {
	Timeout int
}

// PluginDisableConfig holds arguments for plugin disable.
type PluginDisableConfig struct {
	ForceDisable bool
}

// NetworkListConfig stores the options available for listing networks
type NetworkListConfig struct {
	WithServices bool
	WithStatus   bool
}

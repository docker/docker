// Package backend includes types to send information to server backends.
package backend // import "github.com/docker/docker/api/types/backend"

import (
	"crypto/tls"
	"io"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
)

// ContainerAttachConfig is used to configure a container attach request
// The stdio streams of the container are written to the stream returned by the `GetStream` function.
// Reads/Writes will be performed using the defined framing.
type ContainerAttachConfig struct {
	GetStream  func() (io.ReadWriteCloser, error)
	Logs       bool
	Stream     bool
	DetachKeys string
	// The reason for this is so the process that's actually handling the attachment knows how to setup the framing.
	Framing AttachFraming
	// The API falls back to no framing when a TTY is used since there is only stdout anyway.
	// Since the API router doesn't have knowledge of the TTY, this is handled within the daemon object.
	AllowTTYNoFraming bool
	IncludeStdin      bool
	IncludeStdout     bool
	IncludeStderr     bool
}

// AttachFraming is used to define protocols used for a container stdio attachment request.
type AttachFraming int

// Framing protocols for container attachments
const (
	// No framing, this means that the underlying reader/writers are defined in such a way that it can receive the raw
	AttachFramingNone AttachFraming = 0
	// Defined in github.com/docker/docker/pkg/stdcopy
	AttachFramingStdcopy AttachFraming = 1
	// Websocket text framing (before API version 1.28)
	AttachFramingWebsocket AttachFraming = 2
	// Websocket binary framing
	AttachFramingWebsocketBinary AttachFraming = 3
)

type AttachWebsocketData struct {
	Header    http.Header
	Host      string
	Scheme    string
	URI       string
	TLSConfig *tls.Config
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
// changes to this struct need to be reflect in the reset method in
// daemon/logger/logger.go
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
	OutStream io.Writer
	Version   string
}

// ExecInspect holds information about a running process started
// with docker exec.
type ExecInspect struct {
	ID            string
	Running       bool
	ExitCode      *int
	ProcessConfig *ExecProcessConfig
	OpenStdin     bool
	OpenStderr    bool
	OpenStdout    bool
	CanRemove     bool
	ContainerID   string
	DetachKeys    []byte
	Pid           int
}

// ExecProcessConfig holds information about the exec process
// running on the host.
type ExecProcessConfig struct {
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
	Privileged *bool    `json:"privileged,omitempty"`
	User       string   `json:"user,omitempty"`
}

// CreateImageConfig is the configuration for creating an image from a
// container.
type CreateImageConfig struct {
	Repo    string
	Tag     string
	Pause   bool
	Author  string
	Comment string
	Config  *container.Config
	Changes []string
}

// CommitConfig is the configuration for creating an image as part of a build.
type CommitConfig struct {
	Author              string
	Comment             string
	Config              *container.Config
	ContainerConfig     *container.Config
	ContainerID         string
	ContainerMountLabel string
	ContainerOS         string
	ParentImageID       string
}

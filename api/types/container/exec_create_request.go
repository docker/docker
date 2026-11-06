package container

// ExecCreateRequest is a small subset of the Config struct that holds the configuration
// for the exec feature of docker.
type ExecCreateRequest struct {
	User         string   // User that will run the command
	Privileged   bool     // Is the container in privileged mode
	Tty          bool     // Attach standard streams to a tty.
	ConsoleSize  *[2]uint `json:",omitempty"` // Initial console size [height, width]
	AttachStdin  bool     // Attach the standard input, makes possible user interaction
	AttachStderr bool     // Attach the standard error
	AttachStdout bool     // Attach the standard output
	DetachKeys   string   // Escape keys for detach
	Env          []string // Environment variables
	WorkingDir   string   // Working directory
	Cmd          []string // Execution commands and args

	// CaptureStdout tees the exec process's stdout into the container's
	// logging driver, recorded on the dedicated "exec-stdout" stream.
	// Captured messages carry the exec's identity ("exec_id" and any
	// Labels) as per-message attributes, so log readers can tell exec
	// output apart from the container's main process output.
	CaptureStdout bool `json:",omitempty"`

	// CaptureStderr tees the exec process's stderr into the container's
	// logging driver, recorded on the dedicated "exec-stderr" stream.
	// Captured messages carry the exec's identity ("exec_id" and any
	// Labels) as per-message attributes, so log readers can tell exec
	// output apart from the container's main process output.
	CaptureStderr bool `json:",omitempty"`

	// Labels holds user-defined metadata for the exec instance. Labels are
	// reported by exec inspect, attached to the exec's lifecycle events,
	// and stamped on captured log messages when CaptureStdout or
	// CaptureStderr is set.
	Labels map[string]string `json:",omitempty"`
}

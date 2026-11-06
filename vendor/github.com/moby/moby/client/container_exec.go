package client

import (
	"context"
	"encoding/json"
	"net/http"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

// ExecCreateOptions is a small subset of the Config struct that holds the configuration
// for the exec feature of docker.
type ExecCreateOptions struct {
	User         string      // User that will run the command
	Privileged   bool        // Is the container in privileged mode
	TTY          bool        // Attach standard streams to a tty.
	ConsoleSize  ConsoleSize // Initial terminal size [height, width], unused if TTY == false
	AttachStdin  bool        // Attach the standard input, makes possible user interaction
	AttachStderr bool        // Attach the standard error
	AttachStdout bool        // Attach the standard output
	DetachKeys   string      // Escape keys for detach
	Env          []string    // Environment variables
	WorkingDir   string      // Working directory
	Cmd          []string    // Execution commands and args

	// CaptureStdout tees the exec process's stdout into the container's
	// logging driver, recorded on the dedicated "exec-stdout" stream and
	// stamped with the exec's identity ("exec_id" and any Labels).
	CaptureStdout bool

	// CaptureStderr tees the exec process's stderr into the container's
	// logging driver, recorded on the dedicated "exec-stderr" stream and
	// stamped with the exec's identity ("exec_id" and any Labels).
	CaptureStderr bool

	// Labels holds user-defined metadata for the exec instance.
	Labels map[string]string
}

// ExecCreateResult holds the result of creating a container exec.
type ExecCreateResult struct {
	ID string
}

// ExecCreate creates a new exec configuration to run an exec process.
func (cli *Client) ExecCreate(ctx context.Context, containerID string, options ExecCreateOptions) (ExecCreateResult, error) {
	containerID, err := trimID("container", containerID)
	if err != nil {
		return ExecCreateResult{}, err
	}

	consoleSize, err := getConsoleSize(options.TTY, options.ConsoleSize)
	if err != nil {
		return ExecCreateResult{}, err
	}

	resp, err := cli.post(ctx, "/containers/"+containerID+"/exec", nil, nil, container.ExecCreateRequest{
		User:          options.User,
		Privileged:    options.Privileged,
		Tty:           options.TTY,
		ConsoleSize:   consoleSize,
		AttachStdin:   options.AttachStdin,
		AttachStderr:  options.AttachStderr,
		AttachStdout:  options.AttachStdout,
		DetachKeys:    options.DetachKeys,
		Env:           options.Env,
		WorkingDir:    options.WorkingDir,
		Cmd:           options.Cmd,
		CaptureStdout: options.CaptureStdout,
		CaptureStderr: options.CaptureStderr,
		Labels:        options.Labels,
	})
	defer ensureReaderClosed(resp)
	if err != nil {
		return ExecCreateResult{}, err
	}

	var response container.ExecCreateResponse
	err = json.NewDecoder(resp.Body).Decode(&response)
	return ExecCreateResult{ID: response.ID}, err
}

type ConsoleSize struct {
	Height, Width uint
}

// ExecStartOptions holds options for starting a container exec.
type ExecStartOptions struct {
	// ExecStart will first check if it's detached
	Detach bool
	// Check if there's a tty
	TTY bool
	// Terminal size [height, width], unused if TTY == false
	ConsoleSize ConsoleSize
}

// ExecStartResult holds the result of starting a container exec.
type ExecStartResult struct{}

// ExecStart starts an exec process already created in the docker host.
func (cli *Client) ExecStart(ctx context.Context, execID string, options ExecStartOptions) (ExecStartResult, error) {
	consoleSize, err := getConsoleSize(options.TTY, options.ConsoleSize)
	if err != nil {
		return ExecStartResult{}, err
	}

	resp, err := cli.post(ctx, "/exec/"+execID+"/start", nil, nil, container.ExecStartRequest{
		Detach:      options.Detach,
		Tty:         options.TTY,
		ConsoleSize: consoleSize,
	})
	defer ensureReaderClosed(resp)
	return ExecStartResult{}, err
}

// ExecAttachOptions holds options for attaching to a container exec.
type ExecAttachOptions struct {
	// Check if there's a tty
	TTY bool
	// Terminal size [height, width], unused if TTY == false
	ConsoleSize ConsoleSize `json:",omitzero"`
}

// ExecAttachResult holds the result of attaching to a container exec.
type ExecAttachResult struct {
	HijackedResponse
}

// ExecAttach attaches a connection to an exec process in the server.
//
// It returns a [HijackedResponse] with the hijacked connection
// and a reader to get output. It's up to the caller to close
// the hijacked connection by calling [HijackedResponse.Close].
//
// The stream format on the response uses one of two formats:
//
//   - If the container is using a TTY, there is only a single stream (stdout)
//     and data is copied directly from the container output stream, no extra
//     multiplexing or headers.
//   - If the container is *not* using a TTY, streams for stdout and stderr are
//     multiplexed.
//
// You can use [stdcopy.StdCopy] to demultiplex this stream. Refer to
// [Client.ContainerAttach] for details about the multiplexed stream.
//
// [stdcopy.StdCopy]: https://pkg.go.dev/github.com/moby/moby/api/pkg/stdcopy#StdCopy
func (cli *Client) ExecAttach(ctx context.Context, execID string, options ExecAttachOptions) (ExecAttachResult, error) {
	consoleSize, err := getConsoleSize(options.TTY, options.ConsoleSize)
	if err != nil {
		return ExecAttachResult{}, err
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	response, err := cli.postHijacked(ctx, "/exec/"+execID+"/start", nil, headers, container.ExecStartRequest{
		Detach:      false,
		Tty:         options.TTY,
		ConsoleSize: consoleSize,
	})
	return ExecAttachResult{HijackedResponse: response}, err
}

func getConsoleSize(hasTTY bool, consoleSize ConsoleSize) (*[2]uint, error) {
	if consoleSize.Height != 0 || consoleSize.Width != 0 {
		if !hasTTY {
			return nil, cerrdefs.ErrInvalidArgument.WithMessage("console size is only supported when TTY is enabled")
		}
		return &[2]uint{consoleSize.Height, consoleSize.Width}, nil
	}
	return nil, nil
}

// ExecInspectOptions holds options for inspecting a container exec.
type ExecInspectOptions struct{}

// ExecInspectResult holds the result of inspecting a container exec.
//
// It provides a subset of the information included in [container.ExecInspectResponse].
//
// TODO(thaJeztah): include all fields of [container.ExecInspectResponse] ?
type ExecInspectResult struct {
	ID          string
	ContainerID string
	Running     bool
	ExitCode    int
	PID         int

	// Labels holds the user-defined metadata declared at exec create time.
	Labels map[string]string
}

// ExecInspect returns information about a specific exec process on the docker host.
func (cli *Client) ExecInspect(ctx context.Context, execID string, options ExecInspectOptions) (ExecInspectResult, error) {
	resp, err := cli.get(ctx, "/exec/"+execID+"/json", nil, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return ExecInspectResult{}, err
	}

	var response container.ExecInspectResponse
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return ExecInspectResult{}, err
	}

	var ec int
	if response.ExitCode != nil {
		ec = *response.ExitCode
	}

	return ExecInspectResult{
		ID:          response.ID,
		ContainerID: response.ContainerID,
		Running:     response.Running,
		ExitCode:    ec,
		PID:         response.Pid,
		Labels:      response.Labels,
	}, nil
}

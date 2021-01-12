package stdio

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/containerd/ttrpc"
	types "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"
)

var _ Attacher = &Client{}

type AttachConfig struct {
	Stdin         *os.File
	Stdout        *os.File
	Stderr        *os.File
	Framing       StreamFraming
	IncludeStdout bool
	IncludeStderr bool
	DetachKeys    []byte
}

// Attacher is the interface used abstract stream attachments for different implementations which can be backed by RPC or directly in process.
type Attacher interface {
	Attach(ctx context.Context, stdin, stdout, stderr *os.File) error
	AttachMultiplexed(ctx context.Context, f *os.File, framing StreamFraming, detachKeys []byte, includeStdin, includeStdout, includeStderr bool) error
	Close() error
}

// FdSender sends file descriptors.
// It is used as a backchannel to send files to the out of process stdio attacher since the RPC mechanism doesn't currently support it.
type FdSender interface {
	Sendfd(files ...*os.File) ([]int, error)
}

// Client wraps a StdioService to be used as an Attacher.
// It handles passing FD's to the StdioService as well as making the actual calls to the service.
type Client struct {
	c           StdioService
	fdSender    FdSender
	ttrpcClient *ttrpc.Client
}

// NewAttachClient creates a new client from the provided socket address.
// The passed in fdSender is responsible for all transport of file descriptors.
func NewAttachClient(ctx context.Context, fdSender FdSender, rpcAddr string) (*Client, error) {
	conn, err := dialRetry(ctx, rpcAddr, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("error dialing rpc server: %w", err)
	}
	ttrpcClient := ttrpc.NewClient(conn, ttrpc.WithOnClose(func() {
		conn.Close()
	}))
	return &Client{c: NewStdioClient(ttrpcClient), fdSender: fdSender, ttrpcClient: ttrpcClient}, nil
}

// CheckRunning checks if the address is accepting connections.
//
// This is used to determine if a new process should be spun up to handle stdio attachments.
func CheckRunning(addr string) bool {
	conn, err := dial(addr, time.Second)
	if err != nil {
		return false
	}

	conn.Close()
	return true
}

// Attach sends a request to attach the I/O streams to the container's stdio.
//
// On success, all passed in files are closed.
func (c *Client) Attach(ctx context.Context, stdin, stdout, stderr *os.File) (retErr error) {
	send := make([]*os.File, 0, 3)
	if stdin != nil {
		send = append(send, stdin)
	}
	if stdout != nil {
		send = append(send, stdout)
	}
	if stderr != nil {
		send = append(send, stderr)
	}

	var req AttachStreamsRequest

	fds, err := c.fdSender.Sendfd(send...)
	if err != nil {
		return fmt.Errorf("error sending file descriptors: %w", err)
	}

	defer func() {
		if stdin != nil {
			stdin.Close()
		}
		if stdout != nil {
			err := stdout.Close()
			logrus.WithError(err).Debug("Closing stdout write stream")
		}
		if stderr != nil {
			stderr.Close()
			logrus.WithError(err).Debug("Closing stderr write stream")
		}
		return
	}()

	if len(fds) != len(send) {
		return fmt.Errorf("expected sendfd to return %d fiile descriptors, got %d", len(send), len(fds))
	}

	var pos int
	if stdin != nil {
		req.Stdin = &FileDescriptor{Fileno: uint32(fds[pos]), Name: stdin.Name()}
		pos++
	}
	if stdout != nil {
		req.Stdout = &FileDescriptor{Fileno: uint32(fds[pos]), Name: stdout.Name()}
		pos++
	}
	if stderr != nil {
		req.Stderr = &FileDescriptor{Fileno: uint32(fds[pos]), Name: stderr.Name()}
	}

	_, err = c.c.AttachStreams(ctx, &req)
	return err
}

// AttachMultiplexed a request to attach the I/O streams to the container with the output streams multiplexed.
//
// On success, the passed in file is closed.
func (c *Client) AttachMultiplexed(ctx context.Context, f *os.File, framing StreamFraming, detachKeys []byte, includeStdin, includeStdout, includeStderr bool) error {
	fds, err := c.fdSender.Sendfd(f)
	if err != nil {
		return fmt.Errorf("error sending file descriptors: %w", err)
	}
	f.Close()

	if len(fds) != 1 {
		return fmt.Errorf("expected sendfd to return %d fiile descriptors, got %d", 1, len(fds))
	}

	req := &AttachStreamsMultiplexedRequest{
		Stream:        &FileDescriptor{Fileno: uint32(fds[0]), Name: f.Name()},
		Framing:       framing,
		IncludeStdin:  includeStdin,
		IncludeStdout: includeStdout,
		IncludeStderr: includeStderr,
	}

	_, err = c.c.AttachStreamsMultiplexed(ctx, req)
	return err
}

// Close sends a request to close the service.
//
// In practice this is used to exit the out of process stdio attacher since we don't always have a handle on the process itself.
func (c *Client) Close() error {
	if closer, ok := c.fdSender.(io.Closer); ok {
		closer.Close()
	}
	_, err := c.c.Shutdown(context.TODO(), &types.Empty{})
	c.ttrpcClient.Close()
	return err
}

func dialRetry(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	dur := 500 * time.Millisecond
	timer := time.NewTimer(dur)
	defer timer.Stop()

	if !timer.Stop() {
		<-timer.C
	}

	first := true
	for {
		conn, err := dial(addr, timeout)
		if conn != nil {
			return conn, nil
		}

		if !first {
			logrus.WithError(err).Debugf("Error attempting to dial %s", addr)
		}
		first = false

		timer.Reset(dur)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

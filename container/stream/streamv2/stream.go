package streamv2

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/containerd/containerd/cio"
	"github.com/docker/docker/container/stream/streamv2/stdio"
	"github.com/docker/docker/pkg/pools"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Streams struct {
	dir string
	io  cio.IO
	id  string

	mu sync.Mutex
	// Requests stores all the requests for attach that have occurred while a conttainer is not running.
	// When stdio is initialied for the container, the requests are forwarded to the I/O manmager.
	// At that point requests will be cleared.
	requests    []*attachRequest
	initialized bool
	client      stdio.Attacher
	closers     []io.Closer
}

// attachRequests are used to store attachments while the container is not running.
type attachRequest struct {
	stdin                                      *os.File
	stdout, stderr                             *os.File
	framing                                    *stdio.StreamFraming
	detachKeys                                 []byte
	stream                                     *os.File
	includeStdin, includeStdout, includeStderr bool
}

func (r *attachRequest) Close() {
	if r.stdin != nil {
		r.stdin.Close()
	}
	if r.stdout != nil {
		r.stdout.Close()
	}
	if r.stderr != nil {
		r.stderr.Close()
	}
	if r.stream != nil {
		r.stream.Close()
	}
}

func New(workDir string) *Streams {
	return &Streams{
		dir: workDir,
	}
}

func (c *Streams) CopyToPipe(dio *cio.DirectIO) (_ cio.IO, retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return c.io, nil
	}

	ctx := context.TODO()
	client, err := handleStdio(ctx, dio, c.dir)
	if err != nil {
		return nil, err
	}

	c.initialized = true
	c.client = client
	c.io = &cioWithClient{IO: dio, client: client}
	defer func() {
		if retErr != nil {
			if err := client.Close(); err != nil {
				logrus.WithError(err).Debug("Error shutting down container stdio service")
			}
			c.initialized = false
			c.client = nil
			c.io.Close()
			c.io = nil
		}
	}()

	for _, req := range c.requests {
		if req.framing != nil {
			err = c.client.AttachMultiplexed(ctx, req.stream, *req.framing, req.detachKeys, req.includeStdin, req.includeStdout, req.includeStderr)
		} else {
			err = c.client.Attach(ctx, req.stdin, req.stdout, req.stderr)
		}

		if err != nil {
			return nil, errors.Wrap(err, "error attaching stored attach requests")
		}
	}

	c.requests = nil

	return c.io, nil
}

func (c *Streams) CloseStreams() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	defer func() {
		for _, req := range c.requests {
			req.Close()
		}
		c.requests = nil
	}()

	if !c.initialized {
		return nil
	}

	c.initialized = false

	if c.io != nil {
		if err := c.io.Close(); err != nil {
			logrus.WithError(err).Error("Error closing container streams")
		}
		c.io = nil
	}

	c.client.Close()

	for _, c := range c.closers {
		c.Close()
	}
	c.closers = nil

	return nil
}

func (c *Streams) Wait(ctx context.Context) {
	c.mu.Lock()
	cio := c.io
	c.mu.Unlock()
	if cio != nil {
		cio.Wait()
	}
}

func getFile(i interface{}, ref string) (retFile *os.File) {
	logger := logrus.WithField("ref", ref)
	defer func() {
		if retFile == nil {
			logger.Debug("Could not get real file descriptor, falling back to os pipes")
		}
	}()
	switch f := i.(type) {
	case *os.File:
		logger.Debug("Using os file")
		// Nothing to do
		return f
	case fileNetConn:
		logger.Debug("Using file conn")
		ff, err := f.File()
		if err != nil {
			logger.WithError(err).Debug("Error getting file from conn")
			return nil
		}
		// `ff` is a duplicate descriptor here, so we need to close the original
		if closer, ok := i.(io.Closer); ok {
			closer.Close()
		}
		return ff
	case syscall.Conn:
		logger.Debug("Using syscall conn")
		ff, err := fromSyscallConn(f, ref)
		if err != nil {
			logger.WithError(err).Debug("Error getting file from syscall conn")
			return nil
		}
		// In `fromSyscallConn` we always get a duplicate fd.
		if closer, ok := i.(io.Closer); ok {
			closer.Close()
		}
		return ff
	}

	return nil
}

// AttachStreams attaches the passed in streams to the cotnainer's stdio
//
// When using a multiplex stream you MUST NOT pass in a stderr stream.
// This will take care of handling the multiplexing itself.
//
// Ideally callers pass in an `*os.File` so that we can hand of the actual file descriptor off to the stdio manager process.
//  Doing so means the daemon can shutdown but still keep the streams attached.
//  Alternatively, pass down types that can be converted to raw file descriptor number.
//  Supported types for this are:
//		- *os.File
//		- Types which implement `func File() (*os.File, error)`, e.g. many net.Conn implementations.
// 		- syscall.Conn
//	For other types, an `os.Pipe()` will be created and the attachment will be tied to the lifetime of the daemon process.
func (c *Streams) AttachStreams(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (retErr error) {
	var (
		stdinF, stdoutF, stderrF *os.File
	)

	if stdin != nil {
		stdinF = getFile(stdin, "TODO_STDIN_REQUEST")
		if stdinF == nil {
			r, w, err := os.Pipe()
			if err != nil {
				return errors.Wrap(err, "error opening stdin pipe")
			}
			defer func() {
				if retErr != nil {
					w.Close()
					r.Close()
				}
			}()
			stdinF = r
			go func() {
				pools.Copy(w, stdin)
				w.Close()
			}()
		}
	}

	if stdout != nil {
		stdoutF = getFile(stdout, "TODO_STDOUT_REQUEST")
		if stdoutF == nil {
			r, w, err := os.Pipe()
			if err != nil {
				return errors.Wrap(err, "error opening stdout pipe")
			}
			defer func() {
				if retErr != nil {
					w.Close()
					r.Close()
				}
			}()
			stdoutF = w
			go func() {
				pools.Copy(stdout, r)
				w.Close()
			}()
		}
	}

	if stderr != nil {
		stderrF = getFile(stderr, "TODO_STDERR_REQUEST")
		if stderrF == nil {
			r, w, err := os.Pipe()
			if err != nil {
				return errors.Wrap(err, "error opening stderr pipe")
			}
			defer func() {
				if retErr != nil {
					w.Close()
					r.Close()
				}
			}()
			stderrF = w
			go func() {
				pools.Copy(stderr, r)
				w.Close()
			}()
		}
	}

	c.mu.Lock()
	if !c.initialized {
		c.requests = append(c.requests, &attachRequest{stdin: stdinF, stdout: stdoutF, stderr: stderrF})
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	return c.client.Attach(ctx, stdinF, stdoutF, stderrF)
}

// AttachStreamsMultiplexed multiplexes all the requested stdio sttreams onto the single passed in stream.
// See `AtttachStreams` for how
func (c *Streams) AttachStreamsMultiplexed(ctx context.Context, stream io.ReadWriteCloser, framing *stdio.StreamFraming, detachKeys []byte, includeStdin, includeStdout, includeStderr bool) error {
	var stored bool

	streamF := getFile(stream, "TODO_ATTACH_MULTIPLEX")
	if streamF == nil {
		r, w, err := os.Pipe()
		if err != nil {
			return errors.Wrap(err, "error opening stream pipe")
		}

		streamF = w
		go func() {
			pools.Copy(stream, r)
			stream.Close()
			w.Close()
		}()
	} else {
		defer func() {
			if !stored {
				stream.Close()
			}
		}()
	}

	c.mu.Lock()
	if !c.initialized {
		stored = true
		c.requests = append(c.requests, &attachRequest{stream: streamF, detachKeys: detachKeys, framing: framing, includeStdin: includeStdin, includeStdout: includeStdout, includeStderr: includeStderr})
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	return c.client.AttachMultiplexed(ctx, streamF, *framing, detachKeys, includeStdin, includeStdout, includeStderr)
}

// LogPipes creates 2 pipes for stdout/stderr which are then attached to the
// container's stdio streams.
func (c *Streams) LogPipes(ctx context.Context) (_, _ io.ReadCloser, retErr error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, nil, errors.Wrap(err, "error creating stdout pipe")
	}
	defer func() {
		if retErr != nil {
			stdoutR.Close()
			stdoutW.Close()
		}
	}()

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return nil, nil, errors.Wrap(err, "error creating stderr pipe")
	}
	defer func() {
		if retErr != nil {
			stderrR.Close()
			stderrW.Close()
		}
	}()

	if err := c.AttachStreams(ctx, nil, stdoutW, stderrW); err != nil {
		return nil, nil, err
	}

	c.mu.Lock()
	c.closers = append(c.closers, stdoutW, stderrW, stdoutR, stderrR)
	c.mu.Unlock()

	return stdoutR, stderrR, nil
}

// common implementations of net.Conn (such as net.UnixConn, net.TCPConn, etc...) have this `File()` method that we can use
type fileNetConn interface {
	File() (*os.File, error)
	net.Conn
}

type cioWithClient struct {
	client stdio.Attacher
	cio.IO
}

func (c *cioWithClient) Cancel() {
	c.client.Close()
	c.IO.Cancel()
}

func (c *cioWithClient) Close() error {
	c.client.Close()
	return c.IO.Close()
}

package stdio

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

	"github.com/containerd/containerd/log"
	"github.com/docker/docker/pkg/broadcaster"
	"github.com/docker/docker/pkg/pools"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
)

var _ Attacher = &local{}

type local struct {
	stdin            io.WriteCloser
	stdoutB, stderrB *broadcaster.Unbuffered
	stdout, stderr   io.ReadCloser
}

// NewLocalAttacher creates an attacher from the passed in streams that attaches streams in process.
func NewLocalAttacher(stdin io.WriteCloser, stdout, stderr io.ReadCloser) Attacher {
	m := &local{stdin: stdin}

	if stdout != nil {
		m.stdout = stdout
		m.stdoutB = &broadcaster.Unbuffered{}
		go func() {
			pools.Copy(m.stdoutB, stdout)
			stdout.Close()
		}()
	}

	if stderr != nil {
		m.stderr = stderr
		m.stderrB = &broadcaster.Unbuffered{}
		go func() {
			pools.Copy(m.stderrB, stderr)
			stderr.Close()
		}()
	}

	return m
}

var errMissingStream = errors.New("missing required stream")

func (m *local) Attach(ctx context.Context, stdin, stdout, stderr *os.File) error {
	if m.stdin != nil && stdin != nil {
		go func() {
			_, err := pools.Copy(m.stdin, stdin)
			if err != nil {
				log.G(ctx).WithError(err).Error("error copying stdin stream")
			}
			stdin.Close()
		}()
	}

	if m.stdout != nil && stdout != nil {
		m.stdoutB.Add(stdout)
	}

	if m.stderr != nil && stderr != nil {
		m.stderrB.Add(stderr)
	}

	return nil
}

func (m *local) AttachMultiplexed(ctx context.Context, f *os.File, framing StreamFraming, detachKeys []byte, includeStdin, includeStdout, includeStderr bool) error {
	var rwc io.ReadWriteCloser = f

	// Convert this to a net.Conn if possible
	conn, err := net.FileConn(f)
	if err == nil {
		f.Close()
		rwc = conn
	}

	stdin, stdout, stderr := GetFramedStreams(rwc, framing, includeStdin, includeStdout, includeStderr)
	if stdin != nil && m.stdin != nil {
		var r io.Reader = stdin
		if len(detachKeys) > 0 {
			r = term.NewEscapeProxy(r, detachKeys)
		}
		go func() {
			n, err := pools.Copy(m.stdin, r)
			logrus.WithError(err).WithField("copied", n).Debug("Finished copying stdin data")
			if err := stdin.Close(); err != nil {
				logrus.WithError(err).Debug("Error closing stdin")
			}
			logrus.Debug("stdin done")
		}()
	}
	if stdout != nil && m.stdout != nil {
		m.stdoutB.Add(stdout)
	}
	if stderr != nil && m.stderr != nil {
		m.stderrB.Add(stderr)
	}
	return nil
}

func (m *local) Close() error {
	if m.stdin != nil {
		m.stdin.Close()
	}
	if m.stdout != nil {
		m.stdoutB.Clean()
		m.stdout.Close()
	}
	if m.stderr != nil {
		m.stderrB.Clean()
		m.stderr.Close()
	}

	return nil
}

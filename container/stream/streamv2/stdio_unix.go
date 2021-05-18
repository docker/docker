// +build !windows

package streamv2

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/cio"
	"github.com/containerd/fifo"
	"github.com/docker/docker/container/stream/streamv2/stdio"
	"github.com/docker/docker/pkg/pools"
	"github.com/docker/docker/pkg/reexec"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var errIsRunning = errors.New("stdio service already running")

func handleStdio(ctx context.Context, iop *cio.DirectIO, workDir string) (c stdio.Attacher, retErr error) {
	rpcAddr := filepath.Join(workDir, "rpc.sock")
	fdAddr := filepath.Join(workDir, "fd.sock")

	if err := os.MkdirAll(workDir, 0700); err != nil {
		return nil, errors.Wrap(err, "error creating docker-stdio work dir")
	}
	debugStderr, err := fifo.OpenFifo(ctx, filepath.Join(workDir, "debug-stderr"), unix.O_NONBLOCK|unix.O_CREAT|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.Wrap(err, "error opening debug stdout pipe for docker-stdio")
	}
	defer func() {
		if retErr != nil {
			debugStderr.Close()
		}
	}()
	go pools.Copy(logrus.StandardLogger().Out, debugStderr)

	if !stdio.CheckRunning(rpcAddr) {
		args := []string{"docker-stdio", "--rpc-addr", rpcAddr, "--fd-addr", fdAddr}
		if iop.Config().Stdin != "" {
			args = append(args, "--stdin="+iop.Config().Stdin)
		}
		if iop.Config().Stdout != "" {
			args = append(args, "--stdout="+iop.Config().Stdout)
		}
		if iop.Config().Stderr != "" {
			args = append(args, "--stderr="+iop.Config().Stderr)
		}

		cmd := reexec.Command(args...)
		cmd.Stderr = debugStderr
		cmd.SysProcAttr.Pdeathsig = 0 // detach from daemon lifecycle

		if err := cmd.Start(); err != nil {
			return nil, errors.Wrap(err, "error starting io manager")
		}
		go func() {
			if err := cmd.Wait(); err != nil {
				logrus.WithError(err).Debug("container stdio process exited unexpectedly")
			}
			debugStderr.Close()
		}()
		defer func() {
			if retErr != nil {
				cmd.Process.Kill()
			}
		}()
	}

	ctxT, cancel := context.WithTimeout(ctx, 30*time.Second)
	fdClient, err := stdio.NewFdClient(ctxT, fdAddr)
	cancel()
	if err != nil {
		return nil, err
	}

	ctxT, cancel = context.WithTimeout(ctx, 30*time.Second)
	client, err := stdio.NewAttachClient(ctxT, fdClient, rpcAddr)
	cancel()
	if err != nil {
		fdClient.Close()
		return nil, err
	}

	return &clientWithStderrPipe{Attacher: client, pipe: debugStderr}, nil
}

type clientWithStderrPipe struct {
	stdio.Attacher
	pipe io.Closer
}

func (c *clientWithStderrPipe) Close() error {
	err := c.Attacher.Close()
	// c.pipe.Close()
	return err
}

func fromSyscallConn(sc syscall.Conn, ref string) (f *os.File, retErr error) {
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}

	rc.Control(func(fd uintptr) {
		newFd, err := unix.Dup(int(fd))
		if err == nil {
			f = os.NewFile(uintptr(newFd), ref)
		}
		retErr = err
	})
	return f, retErr
}

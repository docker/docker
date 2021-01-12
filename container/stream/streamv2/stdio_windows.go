package streamv2

import (
	"context"
	"os"
	"syscall"

	"github.com/containerd/containerd/cio"
	"github.com/docker/docker/container/stream/streamv2/stdio"
	"golang.org/x/sys/windows"
)

func handleStdio(ctx context.Context, iop *cio.DirectIO, workDir string) (_ stdio.Attacher, retErr error) {
	return stdio.NewLocalAttacher(iop.Stdin, iop.Stdout, iop.Stderr), nil
}

func fromSyscallConn(sc syscall.Conn, ref string) (f *os.File, retErr error) {
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}

	rc.Control(func(fd uintptr) {
		p, err := windows.GetCurrentProcess()
		if err != nil {
			retErr = err
		}

		var target windows.Handle

		err = windows.DuplicateHandle(p, windows.Handle(fd), p, &target, 0, true, windows.DUPLICATE_SAME_ACCESS)
		if err == nil {
			f = os.NewFile(uintptr(target), ref)
		}
		retErr = err
	})
	return f, retErr
}

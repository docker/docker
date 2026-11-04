//go:build linux || freebsd

package logger

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/fifo"
	"golang.org/x/sys/unix"
)

func openPluginStream(a *pluginAdapter) (io.WriteCloser, error) {
	// Make sure to also open with read (in addition to write) to avoid broken pipe errors on plugin failure.
	// It is up to the plugin to keep track of pipes that it should re-attach to, however.
	// If the plugin doesn't open for reads, then the container will block once the pipe is full.
	f, err := fifo.OpenFifo(context.Background(), a.fifoPath, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0o700)
	if err != nil {
		return nil, fmt.Errorf("error creating i/o pipe for log plugin %q: %w", a.Name(), err)
	}
	return f, nil
}

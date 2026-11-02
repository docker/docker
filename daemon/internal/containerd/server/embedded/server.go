//go:build (linux || windows) && !no_embedded_containerd

package embedded

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/log"
	"github.com/docker/go-connections/sockets"

	// Cross-platform plugin registrations. Each blank import registers one or
	// more containerd plugins that the local bootstrap then loads. Platform-specific
	// snapshotters and differs are registered in embedded_linux.go /
	// embedded_windows.go.
	//
	// This is a trimmed subset of containerd's cmd/containerd/builtins: dockerd
	// does not need CRI, sandbox, streaming, transfer, NRI, or the restart
	// monitor.
	_ "github.com/containerd/containerd/v2/core/runtime/v2"
	_ "github.com/containerd/containerd/v2/plugins/content/local/plugin"
	_ "github.com/containerd/containerd/v2/plugins/events"
	_ "github.com/containerd/containerd/v2/plugins/gc"
	_ "github.com/containerd/containerd/v2/plugins/leases"
	_ "github.com/containerd/containerd/v2/plugins/metadata"
	_ "github.com/containerd/containerd/v2/plugins/mount"
	_ "github.com/containerd/containerd/v2/plugins/services/containers"
	_ "github.com/containerd/containerd/v2/plugins/services/content"
	_ "github.com/containerd/containerd/v2/plugins/services/diff"
	_ "github.com/containerd/containerd/v2/plugins/services/events"
	_ "github.com/containerd/containerd/v2/plugins/services/healthcheck"
	_ "github.com/containerd/containerd/v2/plugins/services/images"
	_ "github.com/containerd/containerd/v2/plugins/services/introspection"
	_ "github.com/containerd/containerd/v2/plugins/services/leases"
	_ "github.com/containerd/containerd/v2/plugins/services/namespaces"
	_ "github.com/containerd/containerd/v2/plugins/services/snapshots"
	_ "github.com/containerd/containerd/v2/plugins/services/tasks"
	_ "github.com/containerd/containerd/v2/plugins/services/version"
	_ "github.com/containerd/containerd/v2/plugins/services/warning"
)

// Start initializes and runs the embedded containerd server, using rootDir for
// persistent state (bolt DB, content store) and the daemon subdirectory under
// stateDir for runtime state.
// See the package doc for the transport layout.
// Plugins self-register via the blank imports above and in the platform-specific
// files.
func Start(ctx context.Context, rootDir, stateDir string) (*Daemon, error) {
	setContainerdVersion()

	address := defaultAddress(stateDir)
	cfg := &serverConfig{
		root:               rootDir,
		state:              filepath.Join(stateDir, "daemon"),
		grpcAddress:        address,
		ttrpcAddress:       address + ".ttrpc",
		maxRecvMessageSize: defaults.DefaultMaxRecvMsgSize,
		maxSendMessageSize: defaults.DefaultMaxSendMsgSize,
	}

	// Create the root and state directories with the permissions containerd
	// expects (e.g. the state dir at 0o711 for userns-remapped containers),
	// matching what containerd's own command does before initializing plugins.
	if err := createTopLevelDirectories(cfg); err != nil {
		return nil, err
	}

	log.G(ctx).WithField("address", address).Info("starting embedded containerd server")
	srv, err := newServer(ctx, cfg)
	if err != nil {
		return nil, err
	}

	socket, err := listen(cfg.grpcAddress)
	if err != nil {
		srv.Stop()
		return nil, err
	}
	// Shims (separate processes) publish task events back to containerd over
	// ttrpc, so it must be a real socket. The runtime plugin hands its address
	// (PropertyTTRPCAddress) to each shim.
	ttrpcL, err := listen(cfg.ttrpcAddress)
	if err != nil {
		srv.Stop()
		_ = socket.Close()
		return nil, err
	}
	// A single pending connection is typically sufficient, as gRPC reuses
	// transports. Use a small backlog to tolerate brief bursts of concurrent
	// dials during startup or reconnects.
	inMemory := sockets.NewInmemSocket("embedded-containerd", 16)

	e := &Daemon{
		srv:      srv,
		address:  address,
		inMemory: inMemory,
		ttrpcL:   ttrpcL,
		stopCh:   make(chan struct{}),
	}

	var wg sync.WaitGroup
	serve := func(name string, l net.Listener, fn func(net.Listener) error) {
		wg.Go(func() {
			// Serve returns an error once the server is stopped, which is
			// expected during Shutdown, so only log unexpected failures.
			if err := fn(l); err != nil && !e.stopping.Load() {
				log.G(ctx).WithError(err).Errorf("embedded containerd %s server exited", name)
			}
		})
	}
	serve("gRPC socket", socket, srv.ServeGRPC)
	serve("gRPC in-memory", inMemory, srv.ServeGRPC)
	serve("ttrpc", ttrpcL, srv.ServeTTRPC)
	go func() {
		wg.Wait()
		close(e.stopCh)
	}()

	// Tie the server to the daemon context: shut down when it is cancelled,
	// leaving running shims untouched. WithoutCancel so Shutdown can still wait
	// for the server to stop after ctx is already done.
	go func() {
		<-ctx.Done()
		if err := e.shutdown(context.WithoutCancel(ctx)); err != nil {
			log.G(ctx).WithError(err).Error("failed to shut down embedded containerd")
		}
	}()

	// Do not report successful startup until asynchronous plugin initialization
	// has completed.
	srv.Wait()
	return e, nil
}

// setContainerdVersion makes the embedded server report the vendored containerd
// version in "docker info" and "docker version" instead of the source default.
//
// The version comes from the build's module info.
// The git commit is not recorded there, so only the version is set.
func setContainerdVersion() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, dep := range bi.Deps {
		if dep.Path == "github.com/containerd/containerd/v2" && dep.Version != "" {
			version.Version = dep.Version
			break
		}
	}
}

// Daemon is an in-process containerd server.
type Daemon struct {
	srv      *containerdServer
	address  string
	inMemory *sockets.InmemSocket
	ttrpcL   net.Listener
	stopCh   chan struct{}
	stopping atomic.Bool
}

// Address returns the containerd gRPC address (unix socket path, or named
// pipe on Windows) external clients and tooling should dial.
func (e *Daemon) Address() string {
	return e.address
}

// Dial returns one end of an in-memory pipe connected to the gRPC server. The
// addr argument is ignored, and only present to satisfy grpc.WithContextDialer.
func (e *Daemon) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return e.inMemory.DialContext(ctx, "inmem", addr)
}

// WaitTimeout waits up to d for the server to stop after shutdown.
func (e *Daemon) WaitTimeout(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return errors.New("timeout waiting for embedded containerd to stop")
	case <-e.stopCh:
		return nil
	}
}

// shutdown gracefully stops the in-process server and waits for it to exit.
func (e *Daemon) shutdown(ctx context.Context) error {
	e.stopping.Store(true)
	// Stop closes both RPC servers and their listeners.
	e.srv.Stop()
	_ = e.inMemory.Close()
	select {
	case <-e.stopCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

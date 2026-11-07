//go:build (linux || windows) && !no_embedded_containerd

package embedded

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/sys"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

type grpcService interface {
	Register(*grpc.Server) error
}

type ttrpcService interface {
	RegisterTTRPC(*ttrpc.Server) error
}

type serverConfig struct {
	root               string
	state              string
	grpcAddress        string
	ttrpcAddress       string
	maxRecvMessageSize int
	maxSendMessageSize int

	// containerd is the configuration loaded from drop-in files.
	// Only its plugins, disabled_plugins, and required_plugins settings apply.
	containerd srvconfig.Config
}

type containerdServer struct {
	grpcServer  *grpc.Server
	ttrpcServer *ttrpc.Server
	serveCtx    context.Context
	cancelServe context.CancelFunc
	plugins     []*plugin.Plugin
	ready       sync.WaitGroup
	stopOnce    sync.Once
}

// newServer initializes the containerd plugin graph and the RPC servers needed
// by dockerd.
// Command-owned behavior such as process mutation, proxy plugins, TCP/TLS, and
// Prometheus registration is intentionally not part of the embedded server.
func newServer(ctx context.Context, cfg *serverConfig) (*containerdServer, error) {
	registrations := registry.Graph(srvconfig.V2DisabledFilter(cfg.containerd.DisabledPlugins))
	return newServerWithRegistrations(ctx, cfg, registrations)
}

// newServerWithRegistrations mirrors containerd's plugin lifecycle for the
// subset used by dockerd.
// It is based on containerd v2.3.3's cmd/containerd/server.New:
// https://github.com/containerd/containerd/blob/aad11006b869517fcd3009450b6f82da282e1a9b/cmd/containerd/server/server.go
func newServerWithRegistrations(ctx context.Context, cfg *serverConfig, registrations []plugin.Registration) (_ *containerdServer, retErr error) {
	// BuildKit registers collectors with the same names in dockerd, and the
	// embedded containerd metrics are not exposed, so omit gRPC metrics instead
	// of mutating the process-global Prometheus registerer.
	grpcOptions := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(streamNamespaceInterceptor),
		grpc.ChainUnaryInterceptor(unaryNamespaceInterceptor),
	}
	if cfg.maxRecvMessageSize > 0 {
		grpcOptions = append(grpcOptions, grpc.MaxRecvMsgSize(cfg.maxRecvMessageSize))
	}
	if cfg.maxSendMessageSize > 0 {
		grpcOptions = append(grpcOptions, grpc.MaxSendMsgSize(cfg.maxSendMessageSize))
	}

	ttrpcServer, err := newTTRPCServer()
	if err != nil {
		return nil, fmt.Errorf("creating containerd ttrpc server: %w", err)
	}

	serveCtx, cancelServe := context.WithCancel(ctx)
	srv := &containerdServer{
		grpcServer:  grpc.NewServer(grpcOptions...),
		ttrpcServer: ttrpcServer,
		serveCtx:    serveCtx,
		cancelServe: cancelServe,
	}
	defer func() {
		if retErr != nil {
			srv.Stop()
		}
	}()

	initialized := plugin.NewPluginSet()
	required := make(map[string]struct{}, len(cfg.containerd.RequiredPlugins))
	for _, id := range cfg.containerd.RequiredPlugins {
		required[id] = struct{}{}
	}

	var grpcServices []grpcService
	var ttrpcServices []ttrpcService
	for _, registration := range registrations {
		id := registration.URI()
		log.G(ctx).WithFields(log.Fields{"id": id, "type": registration.Type}).Info("loading plugin")

		initContext := plugin.NewContext(ctx, initialized, map[string]string{
			plugins.PropertyRootDir:      filepath.Join(cfg.root, id),
			plugins.PropertyStateDir:     filepath.Join(cfg.state, id),
			plugins.PropertyGRPCAddress:  cfg.grpcAddress,
			plugins.PropertyTTRPCAddress: cfg.ttrpcAddress,
		})

		if registration.Config != nil {
			pluginConfig, err := cfg.containerd.Decode(ctx, id, registration.Config)
			if err != nil {
				return nil, fmt.Errorf("decoding config for plugin %q: %w", id, err)
			}
			registration.Config = pluginConfig
		}

		// Disable cgroups Prometheus metrics for the embedded server to
		// avoid exposing them through dockerd's process-global Prometheus
		// registry.
		//
		// TODO(thaJeztah): Consider making containerd's cgroups Prometheus metrics configurable.
		initContext.Config = disableCgroupsPrometheus(registration)

		var mustSucceed atomic.Bool
		initContext.RegisterReadiness = func() func() {
			mustSucceed.Store(true)
			return srv.registerReadiness()
		}

		result := registration.Init(initContext)
		if err := initialized.Add(result); err != nil {
			return nil, fmt.Errorf("adding plugin %q to initialized set: %w", id, err)
		}

		instance, err := result.Instance()
		if err != nil {
			if mustSucceed.Load() {
				return nil, fmt.Errorf("plugin %q failed after registering readiness: %w", id, err)
			}
			if _, ok := required[id]; ok {
				return nil, fmt.Errorf("loading required plugin %q: %w", id, err)
			}
			fields := log.Fields{"error": err, "id": id, "type": registration.Type}
			if plugin.IsSkipPlugin(err) {
				log.G(ctx).WithFields(fields).Info("skip loading plugin")
			} else {
				log.G(ctx).WithFields(fields).Warn("failed to load plugin")
			}
			continue
		}
		delete(required, id)

		if service, ok := instance.(grpcService); ok {
			grpcServices = append(grpcServices, service)
		}
		if service, ok := instance.(ttrpcService); ok {
			ttrpcServices = append(ttrpcServices, service)
		}
		srv.plugins = append(srv.plugins, result)
	}
	if len(required) != 0 {
		return nil, fmt.Errorf("required plugins not loaded: %s", strings.Join(slices.Sorted(maps.Keys(required)), ", "))
	}

	for _, service := range grpcServices {
		if err := service.Register(srv.grpcServer); err != nil {
			return nil, fmt.Errorf("registering containerd grpc service: %w", err)
		}
	}
	for _, service := range ttrpcServices {
		if err := service.RegisterTTRPC(srv.ttrpcServer); err != nil {
			return nil, fmt.Errorf("registering containerd ttrpc service: %w", err)
		}
	}
	return srv, nil
}

func createTopLevelDirectories(cfg *serverConfig) error {
	switch {
	case cfg.root == "":
		return errors.New("containerd root must be specified")
	case cfg.state == "":
		return errors.New("containerd state must be specified")
	case cfg.root == cfg.state:
		return errors.New("containerd root and state must be different paths")
	}

	if err := sys.MkdirAllWithACL(cfg.root, 0o700); err != nil {
		return fmt.Errorf("creating containerd root directory: %w", err)
	}
	if err := os.Chmod(cfg.root, 0o700); err != nil {
		return fmt.Errorf("setting containerd root directory permissions: %w", err)
	}

	// State must be searchable by remapped users so they can reach plugin-owned
	// directories with more restrictive permissions below it.
	if err := sys.MkdirAllWithACL(cfg.state, 0o711); err != nil {
		return fmt.Errorf("creating containerd state directory: %w", err)
	}
	if cfg.state != defaults.DefaultStateDir {
		// Shim sockets and FIFOs still use the default state directory even when
		// containerd is configured with a different state directory.
		if err := sys.MkdirAllWithACL(defaults.DefaultStateDir, 0o711); err != nil {
			return fmt.Errorf("creating default containerd state directory: %w", err)
		}
	}
	return nil
}

func (s *containerdServer) ServeGRPC(listener net.Listener) error {
	return trapClosedConnErr(s.grpcServer.Serve(listener))
}

func (s *containerdServer) ServeTTRPC(listener net.Listener) error {
	return trapClosedConnErr(s.ttrpcServer.Serve(s.serveCtx, listener))
}

func (s *containerdServer) Stop() {
	s.stopOnce.Do(func() {
		s.cancelServe()
		s.grpcServer.Stop()
		_ = s.ttrpcServer.Close()
		s.closePlugins()
	})
}

func (s *containerdServer) registerReadiness() func() {
	s.ready.Add(1)
	return s.ready.Done
}

func (s *containerdServer) Wait() {
	s.ready.Wait()
}

func (s *containerdServer) closePlugins() {
	for i := len(s.plugins) - 1; i >= 0; i-- {
		initialized := s.plugins[i]
		instance, err := initialized.Instance()
		if err != nil {
			log.L.WithFields(log.Fields{
				"error": err,
				"id":    initialized.Registration.URI(),
			}).Error("could not get plugin instance")
			continue
		}
		closer, ok := instance.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			log.L.WithFields(log.Fields{
				"error": err,
				"id":    initialized.Registration.URI(),
			}).Error("failed to close plugin")
		}
	}
}

func unaryNamespaceInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if namespace, ok := namespaces.Namespace(ctx); ok {
		// Namespace reads incoming metadata; add it to outgoing metadata for
		// service handlers that call other containerd components.
		ctx = namespaces.WithNamespace(ctx, namespace)
	}
	return handler(ctx, req)
}

func streamNamespaceInterceptor(server any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := stream.Context()
	if namespace, ok := namespaces.Namespace(ctx); ok {
		// Namespace reads incoming metadata; add it to outgoing metadata for
		// service handlers that call other containerd components.
		ctx = namespaces.WithNamespace(ctx, namespace)
		stream = &serverStreamWithContext{ServerStream: stream, ctx: ctx}
	}
	return handler(server, stream)
}

type serverStreamWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serverStreamWithContext) Context() context.Context {
	return s.ctx
}

func trapClosedConnErr(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, ttrpc.ErrServerClosed) {
		return nil
	}
	return err
}

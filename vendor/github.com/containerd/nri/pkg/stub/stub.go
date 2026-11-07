/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package stub

import (
	"context"
	"errors"
	"fmt"
	stdnet "net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	nrilog "github.com/containerd/nri/pkg/log"
	"github.com/containerd/nri/pkg/net"
	"github.com/containerd/nri/pkg/net/multiplex"
	"github.com/containerd/nri/pkg/version"
	"github.com/containerd/ttrpc"
)

// Plugin can implement a number of interfaces related to Pod and Container
// lifecycle events. No any single such interface is mandatory, therefore the
// Plugin interface itself is empty. Plugins are required to implement at
// least one of these interfaces and this is verified during stub creation.
// Trying to create a stub for a plugin violating this requirement will fail
// with and error.
type Plugin interface{}

// ConfigureInterface handles Configure API request.
type ConfigureInterface interface {
	// Configure the plugin with the given NRI-supplied configuration.
	// If a non-zero EventMask is returned, the plugin will be subscribed
	// to the corresponding.
	Configure(ctx context.Context, config, runtime, version string) (api.EventMask, error)
}

// SynchronizeInterface handles Synchronize API requests.
type SynchronizeInterface interface {
	// Synchronize the state of the plugin with the runtime.
	// The plugin can request updates to containers in response.
	Synchronize(context.Context, []*api.PodSandbox, []*api.Container) ([]*api.ContainerUpdate, error)
}

// ShutdownInterface handles a Shutdown API request.
type ShutdownInterface interface {
	// Shutdown notifies the plugin about the runtime shutting down.
	Shutdown(context.Context)
}

// RunPodInterface handles RunPodSandbox API events.
type RunPodInterface interface {
	// RunPodSandbox relays a RunPodSandbox event to the plugin.
	RunPodSandbox(context.Context, *api.PodSandbox) error
}

// UpdatePodInterface handles UpdatePodSandbox API requests.
type UpdatePodInterface interface {
	// UpdatePodSandbox relays an UpdatePodSandbox request to the plugin.
	UpdatePodSandbox(context.Context, *api.PodSandbox, *api.LinuxResources, *api.LinuxResources) error
}

// StopPodInterface handles StopPodSandbox API events.
type StopPodInterface interface {
	// StopPodSandbox relays a StopPodSandbox event to the plugin.
	StopPodSandbox(context.Context, *api.PodSandbox) error
}

// RemovePodInterface handles RemovePodSandbox API events.
type RemovePodInterface interface {
	// RemovePodSandbox relays a RemovePodSandbox event to the plugin.
	RemovePodSandbox(context.Context, *api.PodSandbox) error
}

// PostUpdatePodInterface handles PostUpdatePodSandbox API events.
type PostUpdatePodInterface interface {
	// PostUpdatePodSandbox relays a PostUpdatePodSandbox event to the plugin.
	PostUpdatePodSandbox(context.Context, *api.PodSandbox) error
}

// CreateContainerInterface handles CreateContainer API requests.
type CreateContainerInterface interface {
	// CreateContainer relays a CreateContainer request to the plugin.
	// The plugin can request adjustments to the container being created
	// and updates to other unstopped containers in response.
	CreateContainer(context.Context, *api.PodSandbox, *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error)
}

// StartContainerInterface handles StartContainer API requests.
type StartContainerInterface interface {
	// StartContainer relays a StartContainer event to the plugin.
	StartContainer(context.Context, *api.PodSandbox, *api.Container) error
}

// UpdateContainerInterface handles UpdateContainer API requests.
type UpdateContainerInterface interface {
	// UpdateContainer relays an UpdateContainer request to the plugin.
	// The plugin can request updates both to the container being updated
	// (which then supersedes the original update) and to other unstopped
	// containers in response.
	UpdateContainer(context.Context, *api.PodSandbox, *api.Container, *api.LinuxResources) ([]*api.ContainerUpdate, error)
}

// StopContainerInterface handles StopContainer API requests.
type StopContainerInterface interface {
	// StopContainer relays a StopContainer request to the plugin.
	// The plugin can request updates to unstopped containers in response.
	StopContainer(context.Context, *api.PodSandbox, *api.Container) ([]*api.ContainerUpdate, error)
}

// RemoveContainerInterface handles RemoveContainer API events.
type RemoveContainerInterface interface {
	// RemoveContainer relays a RemoveContainer event to the plugin.
	RemoveContainer(context.Context, *api.PodSandbox, *api.Container) error
}

// PostCreateContainerInterface handles PostCreateContainer API events.
type PostCreateContainerInterface interface {
	// PostCreateContainer relays a PostCreateContainer event to the plugin.
	PostCreateContainer(context.Context, *api.PodSandbox, *api.Container) error
}

// PostStartContainerInterface handles PostStartContainer API events.
type PostStartContainerInterface interface {
	// PostStartContainer relays a PostStartContainer event to the plugin.
	PostStartContainer(context.Context, *api.PodSandbox, *api.Container) error
}

// PostUpdateContainerInterface handles PostUpdateContainer API events.
type PostUpdateContainerInterface interface {
	// PostUpdateContainer relays a PostUpdateContainer event to the plugin.
	PostUpdateContainer(context.Context, *api.PodSandbox, *api.Container) error
}

// ValidateContainerAdjustmentInterface handles container adjustment validation.
type ValidateContainerAdjustmentInterface interface {
	// ValidateContainerAdjustment validates the container adjustment.
	ValidateContainerAdjustment(context.Context, *api.ValidateContainerAdjustmentRequest) error
}

// Stub is the interface the stub provides for the plugin implementation.
type Stub interface {
	// Run starts the plugin then waits for the plugin service to exit, either due to a
	// critical error or an explicit call to Stop(). Once Run() returns, the plugin can be
	// restarted by calling Run() or Start() again.
	Run(context.Context) error
	// Start the plugin.
	Start(context.Context) error
	// Stop the plugin.
	Stop()
	// Wait for the plugin to stop.
	Wait()

	// UpdateContainer requests unsolicited updates to containers.
	UpdateContainers([]*api.ContainerUpdate) ([]*api.ContainerUpdate, error)

	// RegistrationTimeout returns the registration timeout for the stub.
	// This is the default timeout if the plugin has not been started or
	// the timeout received in the Configure request otherwise.
	RegistrationTimeout() time.Duration

	// RequestTimeout returns the request timeout for the stub.
	// This is the default timeout if the plugin has not been started or
	// the timeout received in the Configure request otherwise.
	RequestTimeout() time.Duration

	// Logger returns the logger used by the stub.
	Logger() nrilog.Logger

	// RuntimeNRIVersion returns the NRI version used in the runtime, if known.
	RuntimeNRIVersion() string
	// PluignNRIVersion returns the NRI version used in the plugin/stub.
	PluginNRIVersion() string
}

const (
	// DefaultRegistrationTimeout is the default plugin registration timeout.
	DefaultRegistrationTimeout = api.DefaultPluginRegistrationTimeout
	// DefaultRequestTimeout is the default plugin request processing timeout.
	DefaultRequestTimeout = api.DefaultPluginRequestTimeout
)

var (
	// Used instead of a nil Context in logging.
	noCtx = context.TODO()

	// ErrNoService indicates that the stub has no runtime service/connection,
	// for instance by UpdateContainers on a stub which has not been started.
	ErrNoService = errors.New("stub: no service/connection")
)

// EventMask holds a mask of events for plugin subscription.
type EventMask = api.EventMask

// Option to apply to a plugin during its creation.
type Option func(*stub) error

// WithOnClose sets a notification function to call if the ttRPC connection goes down.
func WithOnClose(onClose func()) Option {
	return func(s *stub) error {
		s.onClose = onClose
		return nil
	}
}

// WithPluginName sets the name to use in plugin registration.
func WithPluginName(name string) Option {
	return func(s *stub) error {
		if s.name != "" {
			s.logger.Infof(noCtx, "Plugin name overridden: %q (previously %q)",
				name, s.name)
		}
		s.name = name
		return nil
	}
}

// WithPluginIdx sets the index to use in plugin registration.
func WithPluginIdx(idx string) Option {
	return func(s *stub) error {
		if s.idx != "" {
			return fmt.Errorf("plugin ID already set (%q)", s.idx)
		}
		s.idx = idx
		return nil
	}
}

// WithSocketPath sets the NRI socket path to connect to.
func WithSocketPath(path string) Option {
	return func(s *stub) error {
		s.socketPath = path
		return nil
	}
}

// WithConnection sets an existing NRI connection to use.
func WithConnection(conn stdnet.Conn) Option {
	return func(s *stub) error {
		s.conn = conn
		return nil
	}
}

// WithDialer sets the dialer to use.
func WithDialer(d func(string) (stdnet.Conn, error)) Option {
	return func(s *stub) error {
		s.dialer = d
		return nil
	}
}

// WithTTRPCOptions sets extra client and server options to use for ttrpc .
func WithTTRPCOptions(clientOpts []ttrpc.ClientOpts, serverOpts []ttrpc.ServerOpt) Option {
	return func(s *stub) error {
		s.clientOpts = append(s.clientOpts, clientOpts...)
		s.serverOpts = append(s.serverOpts, serverOpts...)
		return nil
	}
}

// WithLogger sets the logger to be used by the stub.
func WithLogger(logger nrilog.Logger) Option {
	return func(s *stub) error {
		s.logger = logger
		return nil
	}
}

// WithFailurePolicy sets how the runtime handles a failure of the plugin to
// process a request. A failure is a request timeout, a lost connection or a
// protocol error. The policy has two independent fields. The default of each
// field is the behavior of NRI before failure policies existed:
//
//   - FailRequest: fail the request with an error. Default: the request
//     proceeds without the plugin.
//   - KeepConnected: keep the plugin registered when it exceeds the request
//     timeout. Default: the runtime disconnects the plugin. A plugin that
//     loses its connection is always disconnected.
//
// An error returned by the plugin always fails the request. A runtime with
// an NRI version without failure policy support ignores this option.
func WithFailurePolicy(policy *api.FailurePolicy) Option {
	return func(s *stub) error {
		if policy == nil {
			return fmt.Errorf("nil failure policy")
		}
		s.failurePolicy = policy
		return nil
	}
}

// stub implements Stub.
type stub struct {
	sync.Mutex
	plugin     interface{}
	handlers   handlers
	events     api.EventMask
	name       string
	idx        string
	socketPath string
	dialer     func(string) (stdnet.Conn, error)
	conn       stdnet.Conn
	onClose    func()
	serverOpts []ttrpc.ServerOpt
	clientOpts []ttrpc.ClientOpts
	rpcm       multiplex.Mux
	rpcl       stdnet.Listener
	rpcs       *ttrpc.Server
	rpcc       *ttrpc.Client
	runtime    api.RuntimeService
	started    bool
	doneC      chan struct{}
	srvErrC    chan error
	cfgErrC    chan error
	syncReq    *api.SynchronizeRequest

	registrationTimeout time.Duration
	requestTimeout      time.Duration
	failurePolicy       *api.FailurePolicy
	runtimeNRIVersion   string
	pluginNRIVersion    string
	logger              nrilog.Logger
}

// Handlers for NRI plugin event and request.
type handlers struct {
	Configure                   func(context.Context, string, string, string) (api.EventMask, error)
	Synchronize                 func(context.Context, []*api.PodSandbox, []*api.Container) ([]*api.ContainerUpdate, error)
	Shutdown                    func(context.Context)
	RunPodSandbox               func(context.Context, *api.PodSandbox) error
	UpdatePodSandbox            func(context.Context, *api.PodSandbox, *api.LinuxResources, *api.LinuxResources) error
	StopPodSandbox              func(context.Context, *api.PodSandbox) error
	RemovePodSandbox            func(context.Context, *api.PodSandbox) error
	PostUpdatePodSandbox        func(context.Context, *api.PodSandbox) error
	CreateContainer             func(context.Context, *api.PodSandbox, *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error)
	StartContainer              func(context.Context, *api.PodSandbox, *api.Container) error
	UpdateContainer             func(context.Context, *api.PodSandbox, *api.Container, *api.LinuxResources) ([]*api.ContainerUpdate, error)
	StopContainer               func(context.Context, *api.PodSandbox, *api.Container) ([]*api.ContainerUpdate, error)
	RemoveContainer             func(context.Context, *api.PodSandbox, *api.Container) error
	PostCreateContainer         func(context.Context, *api.PodSandbox, *api.Container) error
	PostStartContainer          func(context.Context, *api.PodSandbox, *api.Container) error
	PostUpdateContainer         func(context.Context, *api.PodSandbox, *api.Container) error
	ValidateContainerAdjustment func(context.Context, *api.ValidateContainerAdjustmentRequest) error
}

// New creates a stub with the given plugin and options.
func New(p interface{}, opts ...Option) (Stub, error) {
	stub := &stub{
		plugin:     p,
		name:       os.Getenv(api.PluginNameEnvVar),
		idx:        os.Getenv(api.PluginIdxEnvVar),
		socketPath: api.DefaultSocketPath,
		dialer:     func(p string) (stdnet.Conn, error) { return stdnet.Dial("unix", p) },

		registrationTimeout: DefaultRegistrationTimeout,
		requestTimeout:      DefaultRequestTimeout,
		logger:              nrilog.Get(),
		runtimeNRIVersion:   "unknown",
	}

	for _, o := range opts {
		if err := o(stub); err != nil {
			return nil, err
		}
	}

	if err := stub.setupHandlers(); err != nil {
		return nil, err
	}

	if err := stub.ensureIdentity(); err != nil {
		return nil, err
	}

	stub.logger.Infof(noCtx, "Created plugin %s (%s, handles %s)", stub.Name(),
		filepath.Base(os.Args[0]), stub.events.PrettyString())

	return stub, nil
}

// Start event processing, register to NRI and wait for getting configured.
func (stub *stub) Start(ctx context.Context) (retErr error) {
	stub.Lock()
	defer stub.Unlock()

	if stub.isStarted() {
		return fmt.Errorf("stub already started")
	}
	stub.doneC = make(chan struct{})

	err := stub.connect()
	if err != nil {
		return err
	}

	rpcm := multiplex.Multiplex(stub.conn)
	defer func() {
		if retErr != nil {
			rpcm.Close()
			stub.rpcm = nil
		}
	}()

	rpcl, err := rpcm.Listen(multiplex.PluginServiceConn)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			rpcl.Close()
			stub.rpcl = nil
		}
	}()

	rpcs, err := ttrpc.NewServer(stub.serverOpts...)
	if err != nil {
		return fmt.Errorf("failed to create ttrpc server: %w", err)
	}
	defer func() {
		if retErr != nil {
			rpcs.Close()
			stub.rpcs = nil
		}
	}()

	api.RegisterPluginService(rpcs, stub)

	conn, err := rpcm.Open(multiplex.RuntimeServiceConn)
	if err != nil {
		return fmt.Errorf("failed to multiplex ttrpc client connection: %w", err)
	}

	clientOpts := []ttrpc.ClientOpts{
		ttrpc.WithOnClose(func() {
			stub.connClosed()
		}),
	}
	rpcc := ttrpc.NewClient(conn, append(clientOpts, stub.clientOpts...)...)
	defer func() {
		if retErr != nil {
			rpcc.Close()
			stub.rpcc = nil
		}
	}()

	stub.srvErrC = make(chan error, 1)
	stub.cfgErrC = make(chan error, 1)

	go func(l stdnet.Listener, doneC chan struct{}, srvErrC chan error) {
		srvErrC <- rpcs.Serve(ctx, l)
		close(doneC)
	}(rpcl, stub.doneC, stub.srvErrC)

	stub.rpcm = rpcm
	stub.rpcl = rpcl
	stub.rpcs = rpcs
	stub.rpcc = rpcc

	stub.runtime = api.NewRuntimeClient(rpcc)

	if err = stub.register(ctx); err != nil {
		stub.close()
		return err
	}

	if err = <-stub.cfgErrC; err != nil {
		return err
	}

	stub.logger.Infof(ctx, "Started plugin %s...", stub.Name())

	stub.started = true
	return nil
}

// Stop the plugin.
func (stub *stub) Stop() {
	stub.logger.Infof(noCtx, "Stopping plugin %s...", stub.Name())

	stub.Lock()
	defer stub.Unlock()
	stub.close()
}

// IsStarted returns true if the plugin has been started either by Start() or by Run().
func (stub *stub) IsStarted() bool {
	stub.Lock()
	defer stub.Unlock()
	return stub.isStarted()
}

func (stub *stub) isStarted() bool {
	return stub.started
}

// reset stub to the status that can initiate a new
// NRI connection, the caller must hold lock.
func (stub *stub) close() {
	if !stub.isStarted() {
		return
	}

	if stub.rpcl != nil {
		stub.rpcl.Close()
	}
	if stub.rpcs != nil {
		stub.rpcs.Close()
	}
	if stub.rpcc != nil {
		stub.rpcc.Close()
	}
	if stub.rpcm != nil {
		stub.rpcm.Close()
	}
	if stub.srvErrC != nil {
		<-stub.doneC
	}

	stub.started = false
	stub.conn = nil
	stub.syncReq = nil
}

// Run the plugin. Start event processing then wait for an error or getting stopped.
func (stub *stub) Run(ctx context.Context) error {
	if err := stub.Start(ctx); err != nil {
		return err
	}

	select {
	case err := <-stub.srvErrC:
		if err == ttrpc.ErrServerClosed {
			stub.logger.Infof(noCtx, "ttrpc server closed %s : %v", stub.Name(), err)
		}
		return err
	case <-ctx.Done():
		stub.Stop()
		return ctx.Err()
	}
}

// Wait for the plugin to stop, should be called after Start() or Run().
func (stub *stub) Wait() {
	if stub.IsStarted() {
		<-stub.doneC
	}
}

// Name returns the full indexed name of the plugin.
func (stub *stub) Name() string {
	return stub.idx + "-" + stub.name
}

func (stub *stub) Logger() nrilog.Logger {
	return stub.logger
}

func (stub *stub) RegistrationTimeout() time.Duration {
	return stub.registrationTimeout
}

func (stub *stub) RequestTimeout() time.Duration {
	return stub.requestTimeout
}

func (stub *stub) RuntimeNRIVersion() string {
	return stub.runtimeNRIVersion
}

func (stub *stub) PluginNRIVersion() string {
	if stub.pluginNRIVersion == "" {
		stub.pluginNRIVersion = version.GetFromBuildInfo()
	}
	return stub.pluginNRIVersion
}

// Connect the plugin to NRI.
func (stub *stub) connect() error {
	if stub.conn != nil {
		stub.logger.Infof(noCtx, "Using given plugin connection...")
		return nil
	}

	if env := os.Getenv(api.PluginSocketEnvVar); env != "" {
		stub.logger.Infof(noCtx, "Using connection %q from environment...", env)

		fd, err := strconv.Atoi(env)
		if err != nil {
			return fmt.Errorf("invalid socket in environment (%s=%q): %w",
				api.PluginSocketEnvVar, env, err)
		}

		stub.conn, err = net.NewFdConn(fd)
		if err != nil {
			return fmt.Errorf("invalid socket (%d) in environment: %w", fd, err)
		}

		return nil
	}

	conn, err := stub.dialer(stub.socketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to NRI service: %w", err)
	}

	stub.conn = conn

	return nil
}

// Register the plugin with NRI.
func (stub *stub) register(ctx context.Context) error {
	nriVersion := stub.PluginNRIVersion()
	stub.logger.Infof(ctx, "Registering plugin %s using NRI version %s...",
		stub.Name(), nriVersion)

	ctx, cancel := context.WithTimeout(ctx, stub.registrationTimeout)
	defer cancel()

	req := &api.RegisterPluginRequest{
		PluginName: stub.name,
		PluginIdx:  stub.idx,
		NRIVersion: nriVersion,
	}
	if _, err := stub.runtime.RegisterPlugin(ctx, req); err != nil {
		return fmt.Errorf("failed to register with NRI/Runtime: %w", err)
	}

	return nil
}

// Handle a lost connection.
func (stub *stub) connClosed() {
	stub.Lock()
	stub.close()
	stub.Unlock()
	if stub.onClose != nil {
		stub.onClose()
		return
	}
}

//
// plugin event and request handlers
//

// UpdateContainers requests unsolicited updates to containers.
func (stub *stub) UpdateContainers(update []*api.ContainerUpdate) ([]*api.ContainerUpdate, error) {
	if stub.runtime == nil {
		return nil, ErrNoService
	}

	ctx := context.Background()
	req := &api.UpdateContainersRequest{
		Update: update,
	}
	rpl, err := stub.runtime.UpdateContainers(ctx, req)
	if rpl != nil {
		return rpl.Failed, err
	}
	return nil, err
}

// Configure the plugin.
func (stub *stub) Configure(ctx context.Context, req *api.ConfigureRequest) (rpl *api.ConfigureResponse, retErr error) {
	var (
		events api.EventMask
		err    error
	)

	stub.logger.Infof(ctx, "Configuring plugin %s for runtime %s/%s (NRI version %s)...",
		stub.Name(), req.RuntimeName, req.RuntimeVersion, req.NRIVersion)

	switch req.NRIVersion {
	case "", api.DevelVersion, api.UnknownVersion:
		inferred, err := api.InferVersionFromRuntime(req.RuntimeName, req.RuntimeVersion)
		if err != nil {
			stub.logger.Warnf(ctx, "failed to infer runtime NRI version: %v", err)
		} else {
			stub.logger.Infof(ctx, "inferred runtime NRI version: %s", inferred)
			req.NRIVersion = inferred
		}
	}

	stub.registrationTimeout = time.Duration(req.RegistrationTimeout * int64(time.Millisecond))
	stub.requestTimeout = time.Duration(req.RequestTimeout * int64(time.Millisecond))
	stub.runtimeNRIVersion = req.NRIVersion

	defer func() {
		stub.cfgErrC <- retErr
	}()

	if handler := stub.handlers.Configure; handler == nil {
		events = stub.events
	} else {
		events, err = handler(ctx, req.Config, req.RuntimeName, req.RuntimeVersion)
		if err != nil {
			stub.logger.Errorf(ctx, "Plugin configuration failed: %v", err)
			return nil, err
		}

		if events == 0 {
			events = stub.events
		}

		// Only allow plugins to subscribe to events they can handle.
		if extra := events & ^stub.events; extra != 0 {
			stub.logger.Errorf(ctx, "Plugin subscribed for unhandled events %s (0x%x)",
				extra.PrettyString(), extra)
			return nil, fmt.Errorf("internal error: unhandled events %s (0x%x)",
				extra.PrettyString(), extra)
		}

		stub.logger.Infof(ctx, "Subscribing plugin %s (%s) for events %s", stub.Name(),
			filepath.Base(os.Args[0]), events.PrettyString())
	}

	return &api.ConfigureResponse{
		Events:        int32(events),
		FailurePolicy: stub.failurePolicy,
	}, nil
}

// Synchronize the state of the plugin with the runtime.
func (stub *stub) Synchronize(ctx context.Context, req *api.SynchronizeRequest) (*api.SynchronizeResponse, error) {
	handler := stub.handlers.Synchronize
	if handler == nil {
		return &api.SynchronizeResponse{More: req.More}, nil
	}

	if req.More {
		return stub.collectSync(req)
	}

	return stub.deliverSync(ctx, req)
}

func (stub *stub) collectSync(req *api.SynchronizeRequest) (*api.SynchronizeResponse, error) {
	stub.Lock()
	defer stub.Unlock()

	stub.logger.Debugf(noCtx, "collecting sync req with %d pods, %d containers...",
		len(req.Pods), len(req.Containers))

	if stub.syncReq == nil {
		stub.syncReq = req
	} else {
		stub.syncReq.Pods = append(stub.syncReq.Pods, req.Pods...)
		stub.syncReq.Containers = append(stub.syncReq.Containers, req.Containers...)
	}

	return &api.SynchronizeResponse{More: req.More}, nil
}

func (stub *stub) deliverSync(ctx context.Context, req *api.SynchronizeRequest) (*api.SynchronizeResponse, error) {
	stub.Lock()
	syncReq := stub.syncReq
	stub.syncReq = nil
	stub.Unlock()

	if syncReq == nil {
		syncReq = req
	} else {
		syncReq.Pods = append(syncReq.Pods, req.Pods...)
		syncReq.Containers = append(syncReq.Containers, req.Containers...)
	}

	update, err := stub.handlers.Synchronize(ctx, syncReq.Pods, syncReq.Containers)
	return &api.SynchronizeResponse{
		Update: update,
		More:   false,
	}, err
}

// Shutdown the plugin.
func (stub *stub) Shutdown(ctx context.Context, _ *api.ShutdownRequest) (*api.ShutdownResponse, error) {
	handler := stub.handlers.Shutdown
	if handler != nil {
		handler(ctx)
	}
	return &api.ShutdownResponse{}, nil
}

// RunPodSandbox request handler.
func (stub *stub) RunPodSandbox(ctx context.Context, req *api.RunPodSandboxRequest) (*api.RunPodSandboxResponse, error) {
	handler := stub.handlers.RunPodSandbox
	if handler == nil {
		return &api.RunPodSandboxResponse{}, nil
	}
	err := handler(ctx, req.GetPod())
	return &api.RunPodSandboxResponse{}, err
}

// UpdatePodSandbox request handler.
func (stub *stub) UpdatePodSandbox(ctx context.Context, req *api.UpdatePodSandboxRequest) (*api.UpdatePodSandboxResponse, error) {
	handler := stub.handlers.UpdatePodSandbox
	if handler == nil {
		return &api.UpdatePodSandboxResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetOverheadLinuxResources(), req.GetLinuxResources())
	return &api.UpdatePodSandboxResponse{}, err
}

// PostUpdatePodSandbox request handler.
func (stub *stub) PostUpdatePodSandbox(ctx context.Context, req *api.PostUpdatePodSandboxRequest) (*api.PostUpdatePodSandboxResponse, error) {
	handler := stub.handlers.PostUpdatePodSandbox
	if handler == nil {
		return &api.PostUpdatePodSandboxResponse{}, nil
	}
	err := handler(ctx, req.GetPod())
	return &api.PostUpdatePodSandboxResponse{}, err
}

// StopPodSandbox request handler.
func (stub *stub) StopPodSandbox(ctx context.Context, req *api.StopPodSandboxRequest) (*api.StopPodSandboxResponse, error) {
	handler := stub.handlers.StopPodSandbox
	if handler == nil {
		return &api.StopPodSandboxResponse{}, nil
	}
	err := handler(ctx, req.GetPod())
	return &api.StopPodSandboxResponse{}, err
}

// RemovePodSandbox request handler.
func (stub *stub) RemovePodSandbox(ctx context.Context, req *api.RemovePodSandboxRequest) (*api.RemovePodSandboxResponse, error) {
	handler := stub.handlers.RemovePodSandbox
	if handler == nil {
		return &api.RemovePodSandboxResponse{}, nil
	}
	err := handler(ctx, req.GetPod())
	return &api.RemovePodSandboxResponse{}, err
}

// CreateContainer request handler.
func (stub *stub) CreateContainer(ctx context.Context, req *api.CreateContainerRequest) (*api.CreateContainerResponse, error) {
	handler := stub.handlers.CreateContainer
	if handler == nil {
		return &api.CreateContainerResponse{}, nil
	}
	adjust, update, err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.CreateContainerResponse{
		Adjust: adjust,
		Update: update,
	}, err
}

// PostCreateContainer request handler.
func (stub *stub) PostCreateContainer(ctx context.Context, req *api.PostCreateContainerRequest) (*api.PostCreateContainerResponse, error) {
	handler := stub.handlers.PostCreateContainer
	if handler == nil {
		return &api.PostCreateContainerResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.PostCreateContainerResponse{}, err
}

// StartContainer request handler.
func (stub *stub) StartContainer(ctx context.Context, req *api.StartContainerRequest) (*api.StartContainerResponse, error) {
	handler := stub.handlers.StartContainer
	if handler == nil {
		return &api.StartContainerResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.StartContainerResponse{}, err
}

// PostStartContainer request handler.
func (stub *stub) PostStartContainer(ctx context.Context, req *api.PostStartContainerRequest) (*api.PostStartContainerResponse, error) {
	handler := stub.handlers.PostStartContainer
	if handler == nil {
		return &api.PostStartContainerResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.PostStartContainerResponse{}, err
}

// UpdateContainer request handler.
func (stub *stub) UpdateContainer(ctx context.Context, req *api.UpdateContainerRequest) (*api.UpdateContainerResponse, error) {
	handler := stub.handlers.UpdateContainer
	if handler == nil {
		return &api.UpdateContainerResponse{}, nil
	}
	update, err := handler(ctx, req.GetPod(), req.GetContainer(), req.GetLinuxResources())
	return &api.UpdateContainerResponse{
		Update: update,
	}, err
}

// PostUpdateContainer request handler.
func (stub *stub) PostUpdateContainer(ctx context.Context, req *api.PostUpdateContainerRequest) (*api.PostUpdateContainerResponse, error) {
	handler := stub.handlers.PostUpdateContainer
	if handler == nil {
		return &api.PostUpdateContainerResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.PostUpdateContainerResponse{}, err
}

// StopContainer request handler.
func (stub *stub) StopContainer(ctx context.Context, req *api.StopContainerRequest) (*api.StopContainerResponse, error) {
	handler := stub.handlers.StopContainer
	if handler == nil {
		return &api.StopContainerResponse{}, nil
	}
	update, err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.StopContainerResponse{
		Update: update,
	}, err
}

// RemoveContainer request handler.
func (stub *stub) RemoveContainer(ctx context.Context, req *api.RemoveContainerRequest) (*api.RemoveContainerResponse, error) {
	handler := stub.handlers.RemoveContainer
	if handler == nil {
		return &api.RemoveContainerResponse{}, nil
	}
	err := handler(ctx, req.GetPod(), req.GetContainer())
	return &api.RemoveContainerResponse{}, err
}

// StateChange event handler.
func (stub *stub) StateChange(ctx context.Context, evt *api.StateChangeEvent) (*api.Empty, error) {
	var err error
	switch evt.Event {
	case api.Event_RUN_POD_SANDBOX:
		if handler := stub.handlers.RunPodSandbox; handler != nil {
			err = handler(ctx, evt.GetPod())
		}
	case api.Event_POST_UPDATE_POD_SANDBOX:
		if handler := stub.handlers.PostUpdatePodSandbox; handler != nil {
			err = handler(ctx, evt.GetPod())
		}
	case api.Event_STOP_POD_SANDBOX:
		if handler := stub.handlers.StopPodSandbox; handler != nil {
			err = handler(ctx, evt.GetPod())
		}
	case api.Event_REMOVE_POD_SANDBOX:
		if handler := stub.handlers.RemovePodSandbox; handler != nil {
			err = handler(ctx, evt.GetPod())
		}
	case api.Event_POST_CREATE_CONTAINER:
		if handler := stub.handlers.PostCreateContainer; handler != nil {
			err = handler(ctx, evt.GetPod(), evt.GetContainer())
		}
	case api.Event_START_CONTAINER:
		if handler := stub.handlers.StartContainer; handler != nil {
			err = handler(ctx, evt.GetPod(), evt.GetContainer())
		}
	case api.Event_POST_START_CONTAINER:
		if handler := stub.handlers.PostStartContainer; handler != nil {
			err = handler(ctx, evt.GetPod(), evt.GetContainer())
		}
	case api.Event_POST_UPDATE_CONTAINER:
		if handler := stub.handlers.PostUpdateContainer; handler != nil {
			err = handler(ctx, evt.GetPod(), evt.GetContainer())
		}
	case api.Event_REMOVE_CONTAINER:
		if handler := stub.handlers.RemoveContainer; handler != nil {
			err = handler(ctx, evt.GetPod(), evt.GetContainer())
		}
	}

	return &api.StateChangeResponse{}, err
}

func (stub *stub) ValidateContainerAdjustment(ctx context.Context, req *api.ValidateContainerAdjustmentRequest) (*api.ValidateContainerAdjustmentResponse, error) {
	handler := stub.handlers.ValidateContainerAdjustment
	if handler == nil {
		return &api.ValidateContainerAdjustmentResponse{}, nil
	}

	if err := handler(ctx, req); err != nil {
		return &api.ValidateContainerAdjustmentResponse{
			Reject: true,
			Reason: err.Error(),
		}, nil
	}

	return &api.ValidateContainerAdjustmentResponse{}, nil
}

// ensureIdentity sets plugin index and name from the binary if those are unset.
func (stub *stub) ensureIdentity() error {
	if stub.idx != "" && stub.name != "" {
		return nil
	}

	if stub.idx != "" {
		stub.name = filepath.Base(os.Args[0])
		return nil
	}

	idx, name, err := api.ParsePluginName(filepath.Base(os.Args[0]))
	if err != nil {
		return err
	}

	stub.name = name
	stub.idx = idx

	return nil
}

// Set up event handlers and the subscription mask for the plugin.
func (stub *stub) setupHandlers() error {
	if plugin, ok := stub.plugin.(ConfigureInterface); ok {
		stub.handlers.Configure = plugin.Configure
	}
	if plugin, ok := stub.plugin.(SynchronizeInterface); ok {
		stub.handlers.Synchronize = plugin.Synchronize
	}
	if plugin, ok := stub.plugin.(ShutdownInterface); ok {
		stub.handlers.Shutdown = plugin.Shutdown
	}

	if plugin, ok := stub.plugin.(RunPodInterface); ok {
		stub.handlers.RunPodSandbox = plugin.RunPodSandbox
		stub.events.Set(api.Event_RUN_POD_SANDBOX)
	}
	if plugin, ok := stub.plugin.(UpdatePodInterface); ok {
		stub.handlers.UpdatePodSandbox = plugin.UpdatePodSandbox
		stub.events.Set(api.Event_UPDATE_POD_SANDBOX)
	}
	if plugin, ok := stub.plugin.(PostUpdatePodInterface); ok {
		stub.handlers.PostUpdatePodSandbox = plugin.PostUpdatePodSandbox
		stub.events.Set(api.Event_POST_UPDATE_POD_SANDBOX)
	}
	if plugin, ok := stub.plugin.(StopPodInterface); ok {
		stub.handlers.StopPodSandbox = plugin.StopPodSandbox
		stub.events.Set(api.Event_STOP_POD_SANDBOX)
	}
	if plugin, ok := stub.plugin.(RemovePodInterface); ok {
		stub.handlers.RemovePodSandbox = plugin.RemovePodSandbox
		stub.events.Set(api.Event_REMOVE_POD_SANDBOX)
	}
	if plugin, ok := stub.plugin.(CreateContainerInterface); ok {
		stub.handlers.CreateContainer = plugin.CreateContainer
		stub.events.Set(api.Event_CREATE_CONTAINER)
	}
	if plugin, ok := stub.plugin.(StartContainerInterface); ok {
		stub.handlers.StartContainer = plugin.StartContainer
		stub.events.Set(api.Event_START_CONTAINER)
	}
	if plugin, ok := stub.plugin.(UpdateContainerInterface); ok {
		stub.handlers.UpdateContainer = plugin.UpdateContainer
		stub.events.Set(api.Event_UPDATE_CONTAINER)
	}
	if plugin, ok := stub.plugin.(StopContainerInterface); ok {
		stub.handlers.StopContainer = plugin.StopContainer
		stub.events.Set(api.Event_STOP_CONTAINER)
	}
	if plugin, ok := stub.plugin.(RemoveContainerInterface); ok {
		stub.handlers.RemoveContainer = plugin.RemoveContainer
		stub.events.Set(api.Event_REMOVE_CONTAINER)
	}
	if plugin, ok := stub.plugin.(PostCreateContainerInterface); ok {
		stub.handlers.PostCreateContainer = plugin.PostCreateContainer
		stub.events.Set(api.Event_POST_CREATE_CONTAINER)
	}
	if plugin, ok := stub.plugin.(PostStartContainerInterface); ok {
		stub.handlers.PostStartContainer = plugin.PostStartContainer
		stub.events.Set(api.Event_POST_START_CONTAINER)
	}
	if plugin, ok := stub.plugin.(PostUpdateContainerInterface); ok {
		stub.handlers.PostUpdateContainer = plugin.PostUpdateContainer
		stub.events.Set(api.Event_POST_UPDATE_CONTAINER)
	}
	if plugin, ok := stub.plugin.(ValidateContainerAdjustmentInterface); ok {
		stub.handlers.ValidateContainerAdjustment = plugin.ValidateContainerAdjustment
		stub.events.Set(api.Event_VALIDATE_CONTAINER_ADJUSTMENT)
	}

	if stub.events == 0 {
		return fmt.Errorf("internal error: plugin %T does not implement any NRI request handlers",
			stub.plugin)
	}

	return nil
}

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

package adaptation

import (
	"context"
	"errors"
	"fmt"
	stdnet "net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/adaptation/builtin"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/log"
	"github.com/containerd/nri/pkg/net"
	"github.com/containerd/nri/pkg/net/multiplex"
	"github.com/containerd/ttrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// DefaultPluginRegistrationTimeout is the default timeout for plugin registration.
	DefaultPluginRegistrationTimeout = api.DefaultPluginRegistrationTimeout
	// DefaultPluginRequestTimeout is the default timeout for plugins to handle a request.
	DefaultPluginRequestTimeout = api.DefaultPluginRequestTimeout
)

var (
	pluginRegistrationTimeout = DefaultPluginRegistrationTimeout
	pluginRequestTimeout      = DefaultPluginRequestTimeout
	timeoutCfgLock            sync.RWMutex
)

type plugin struct {
	sync.Mutex
	idx    string
	base   string
	cfg    string
	pid    int
	cmd    *exec.Cmd
	mux    multiplex.Mux
	rpcc   *ttrpc.Client
	rpcl   stdnet.Listener
	rpcs   *ttrpc.Server
	events EventMask
	policy *FailurePolicy
	closed bool
	regC   chan error
	closeC chan struct{}
	r      *Adaptation
	impl   *pluginType
}

// SetPluginRegistrationTimeout sets the timeout for plugin registration.
func SetPluginRegistrationTimeout(t time.Duration) {
	timeoutCfgLock.Lock()
	defer timeoutCfgLock.Unlock()
	pluginRegistrationTimeout = t
}

func getPluginRegistrationTimeout() time.Duration {
	timeoutCfgLock.RLock()
	defer timeoutCfgLock.RUnlock()
	return pluginRegistrationTimeout
}

// SetPluginRequestTimeout sets the timeout for plugins to handle a request.
func SetPluginRequestTimeout(t time.Duration) {
	timeoutCfgLock.Lock()
	defer timeoutCfgLock.Unlock()
	pluginRequestTimeout = t
}

func getPluginRequestTimeout() time.Duration {
	timeoutCfgLock.RLock()
	defer timeoutCfgLock.RUnlock()
	return pluginRequestTimeout
}

// newLaunchedPlugin launches a pre-installed plugin with a pre-connected socketpair.
// If the plugin is a wasm binary, then it will use the internal wasm service
// to setup the plugin.
func (r *Adaptation) newLaunchedPlugin(dir, idx, base, cfg string) (p *plugin, retErr error) {
	name := idx + "-" + base
	fullPath := filepath.Join(dir, name)

	if isWasm(fullPath) {
		if r.wasmService == nil {
			return nil, fmt.Errorf("can't load WASM plugin %s: no WASM support", fullPath)
		}

		log.Infof(noCtx, "Found WASM plugin: %s", fullPath)
		wasm, err := r.wasmService.Load(context.Background(), fullPath, wasmHostFunctions{})
		if err != nil {
			return nil, fmt.Errorf("load WASM plugin %s: %w", fullPath, err)
		}
		return &plugin{
			cfg:  cfg,
			idx:  idx,
			base: base,
			r:    r,
			impl: &pluginType{wasmImpl: wasm},
		}, nil
	}

	sockets, err := net.NewSocketPair()
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin connection for plugin %q: %w", name, err)
	}
	defer sockets.Close()

	conn, err := sockets.LocalConn()
	if err != nil {
		return nil, fmt.Errorf("failed to set up local connection for plugin %q: %w", name, err)
	}

	peerFile := sockets.PeerFile()
	defer func() {
		peerFile.Close()
		if retErr != nil {
			conn.Close()
		}
	}()

	cmd := exec.Command(fullPath)
	cmd.ExtraFiles = []*os.File{peerFile}
	cmd.Env = []string{
		api.PluginNameEnvVar + "=" + base,
		api.PluginIdxEnvVar + "=" + idx,
		api.PluginSocketEnvVar + "=3",
	}

	p = &plugin{
		cfg:    cfg,
		cmd:    cmd,
		idx:    idx,
		base:   base,
		regC:   make(chan error, 1),
		closeC: make(chan struct{}),
		r:      r,
	}

	if err = p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to launch plugin %q: %w", p.name(), err)
	}

	if err = p.connect(conn); err != nil {
		return nil, err
	}

	return p, nil
}

func (r *Adaptation) newBuiltinPlugin(b *builtin.BuiltinPlugin) (*plugin, error) {
	if b.Base == "" || b.Index == "" {
		return nil, fmt.Errorf("builtin plugin without index or name (%q, %q)", b.Index, b.Base)
	}

	return &plugin{
		idx:    b.Index,
		base:   b.Base,
		closeC: make(chan struct{}),
		r:      r,
		impl:   &pluginType{builtinImpl: b},
	}, nil
}

func isWasm(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		log.Errorf(noCtx, "Unable to open file %s: %v", path, err)
		return false
	}
	defer file.Close()

	const headerLen = 8
	buf := make([]byte, headerLen)
	if _, err := file.Read(buf); err != nil {
		log.Errorf(noCtx, "Unable to read file %s: %v", path, err)
		return false
	}

	// WASM has starts with `\0asm`, followed by the version.
	// http://webassembly.github.io/spec/core/binary/modules.html#binary-magic
	return len(buf) >= headerLen &&
		buf[0] == 0x00 && buf[1] == 0x61 &&
		buf[2] == 0x73 && buf[3] == 0x6D &&
		buf[4] == 0x01 && buf[5] == 0x00 &&
		buf[6] == 0x00 && buf[7] == 0x00
}

// Create a plugin (stub) for an accepted external plugin connection.
func (r *Adaptation) newExternalPlugin(conn stdnet.Conn) (p *plugin, retErr error) {
	p = &plugin{
		regC:   make(chan error, 1),
		closeC: make(chan struct{}),
		r:      r,
	}
	if err := p.connect(conn); err != nil {
		return nil, err
	}

	return p, nil
}

// Get plugin-specific configuration for an NRI-launched plugin.
func (r *Adaptation) getPluginConfig(id, base string) (string, error) {
	name := id + "-" + base
	dropIns := []string{
		filepath.Join(r.dropinPath, name+".conf"),
		filepath.Join(r.dropinPath, base+".conf"),
	}

	for _, path := range dropIns {
		buf, err := os.ReadFile(path)
		if err == nil {
			return string(buf), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to read configuration for plugin %q: %w", name, err)
		}
	}

	return "", nil
}

// Check if the plugin is external (was not launched by us).
func (p *plugin) isExternal() bool {
	return p.cmd == nil
}

// Check if the plugin is a container adjustment validator.
func (p *plugin) isContainerAdjustmentValidator() bool {
	return p.events.IsSet(Event_VALIDATE_CONTAINER_ADJUSTMENT)
}

// 'connect' a plugin, setting up multiplexing on its socket.
func (p *plugin) connect(conn stdnet.Conn) (retErr error) {
	mux := multiplex.Multiplex(conn, multiplex.WithBlockedRead())
	defer func() {
		if retErr != nil {
			mux.Close()
		}
	}()

	pconn, err := mux.Open(multiplex.PluginServiceConn)
	if err != nil {
		return fmt.Errorf("failed to mux plugin connection for plugin %q: %w", p.name(), err)
	}

	clientOpts := []ttrpc.ClientOpts{
		ttrpc.WithOnClose(
			func() {
				log.Infof(noCtx, "connection to plugin %q closed", p.name())
				close(p.closeC)
				p.close()
			}),
	}
	rpcc := ttrpc.NewClient(pconn, append(clientOpts, p.r.clientOpts...)...)
	defer func() {
		if retErr != nil {
			rpcc.Close()
		}
	}()

	rpcs, err := ttrpc.NewServer(p.r.serverOpts...)
	if err != nil {
		return fmt.Errorf("failed to create ttrpc server for plugin %q: %w", p.name(), err)
	}
	defer func() {
		if retErr != nil {
			rpcs.Close()
		}
	}()

	rpcl, err := mux.Listen(multiplex.RuntimeServiceConn)
	if err != nil {
		return fmt.Errorf("failed to create mux runtime listener for plugin %q: %w", p.name(), err)
	}

	p.mux = mux
	p.rpcc = rpcc
	p.rpcl = rpcl
	p.rpcs = rpcs
	p.impl = &pluginType{ttrpcImpl: api.NewPluginClient(rpcc)}

	p.pid, err = getPeerPid(p.mux.Trunk())
	if err != nil {
		log.Warnf(noCtx, "failed to determine plugin pid: %v", err)
	}

	api.RegisterRuntimeService(p.rpcs, p)

	return nil
}

// Start Runtime service, wait for plugin to register, then configure it.
func (p *plugin) start(name, version string) (err error) {
	// skip start for WASM and builtin plugins and head right to the registration for
	// events config
	if p.impl.isTtrpc() {
		var (
			err     error
			timeout = getPluginRegistrationTimeout()
		)

		go func() {
			err := p.rpcs.Serve(context.Background(), p.rpcl)
			if err != ttrpc.ErrServerClosed {
				log.Infof(noCtx, "ttrpc server for plugin %q closed (%v)", p.name(), err)
			}
			p.close()
		}()

		p.mux.Unblock()

		select {
		case err = <-p.regC:
			if err != nil {
				return fmt.Errorf("failed to register plugin: %w", err)
			}
		case <-p.closeC:
			return fmt.Errorf("failed to register plugin, connection closed")
		case <-time.After(timeout):
			p.close()
			p.stop()
			return errors.New("plugin registration timed out")
		}
	}

	err = p.configure(context.Background(), name, version, p.cfg)
	if err != nil {
		p.close()
		p.stop()
		return err
	}

	return nil
}

// close a plugin shutting down its multiplexed ttrpc connections.
func (p *plugin) close() {
	if p.impl.isWasm() || p.impl.isBuiltin() {
		p.closed = true
		return
	}

	p.Lock()
	defer p.Unlock()
	if p.closed {
		return
	}

	p.closed = true
	p.mux.Close()
	p.rpcc.Close()
	p.rpcs.Close()
	p.rpcl.Close()
}

func (p *plugin) isClosed() bool {
	p.Lock()
	defer p.Unlock()
	return p.closed
}

// stop a plugin (if it was launched by us)
func (p *plugin) stop() error {
	if p.isExternal() || p.cmd.Process == nil || p.impl.isWasm() || p.impl.isBuiltin() {
		return nil
	}

	// TODO(klihub):
	//   We should attempt a graceful shutdown of the process here...
	//     - send it SIGINT
	//     - give the it some slack waiting with a timeout
	//     - butcher it with SIGKILL after the timeout

	p.cmd.Process.Kill()
	p.cmd.Process.Wait()
	p.cmd.Process.Release()

	return nil
}

// Name returns a string indentication for the plugin.
func (p *plugin) name() string {
	return p.idx + "-" + p.base
}

func (p *plugin) qualifiedName() string {
	var kind, idx, base, pid string
	if p.impl.isBuiltin() {
		kind = "builtin"
	} else {
		if p.isExternal() {
			kind = "external"
		} else {
			kind = "pre-connected"
		}
		if p.impl.isWasm() {
			kind += "-wasm"
		} else {
			pid = "[" + strconv.Itoa(p.pid) + "]"
		}
	}
	if idx = p.idx; idx == "" {
		idx = "??"
	}
	if base = p.base; base == "" {
		base = "plugin"
	}
	return kind + ":" + idx + "-" + base + pid
}

// RegisterPlugin handles the plugin's registration request.
func (p *plugin) RegisterPlugin(ctx context.Context, req *RegisterPluginRequest) (*RegisterPluginResponse, error) {
	if p.isExternal() {
		if err := api.CheckPluginName(req.PluginName); err != nil {
			p.regC <- fmt.Errorf("plugin registered with an invalid name: %w", err)
			return &RegisterPluginResponse{}, fmt.Errorf("invalid plugin name: %w", err)
		}
		if err := api.CheckPluginIndex(req.PluginIdx); err != nil {
			p.regC <- fmt.Errorf("plugin %q registered with an invalid index: %w", req.PluginName, err)
			return &RegisterPluginResponse{}, fmt.Errorf("invalid plugin index: %w", err)
		}
		p.base = req.PluginName
		p.idx = req.PluginIdx
	}

	nriVersion := "unknown"
	if req.NRIVersion != "" {
		nriVersion = req.NRIVersion
	}
	log.Infof(ctx, "plugin %q registered as %q (with NRI version %s)",
		p.qualifiedName(), p.name(), nriVersion)

	p.regC <- nil
	return &RegisterPluginResponse{}, nil
}

// UpdateContainers relays container update request to the runtime.
func (p *plugin) UpdateContainers(ctx context.Context, req *UpdateContainersRequest) (*UpdateContainersResponse, error) {
	log.Infof(ctx, "plugin %q requested container updates", p.name())

	failed, err := p.r.updateContainers(ctx, req.Update)
	return &UpdateContainersResponse{
		Failed: failed,
	}, err
}

// configure the plugin and subscribe it for the events it requested.
func (p *plugin) configure(ctx context.Context, name, version, config string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	req := &ConfigureRequest{
		Config:              config,
		RuntimeName:         name,
		RuntimeVersion:      version,
		RegistrationTimeout: getPluginRegistrationTimeout().Milliseconds(),
		RequestTimeout:      getPluginRequestTimeout().Milliseconds(),
		NRIVersion:          p.r.nriVersion,
	}

	rpl, err := p.impl.Configure(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to configure plugin: %w", err)
	}

	events := EventMask(rpl.Events)
	if events != 0 {
		if extra := events &^ ValidEvents; extra != 0 {
			return fmt.Errorf("invalid plugin events: 0x%x", extra)
		}
	} else {
		events = ValidEvents
	}
	p.events = events

	// A nil policy means all defaults. The proto getters accept a nil receiver.
	p.policy = rpl.FailurePolicy

	return nil
}

// synchronize the plugin with the current state of the runtime.
func (p *plugin) synchronize(ctx context.Context, pods []*PodSandbox, containers []*Container) ([]*ContainerUpdate, error) {
	log.Infof(ctx, "synchronizing plugin %s", p.name())

	ctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	var (
		podsToSend = pods
		ctrsToSend = containers
		podsPerMsg = len(pods)
		ctrsPerMsg = len(containers)

		rpl *SynchronizeResponse
		err error
	)

	for {
		req := &SynchronizeRequest{
			Pods:       podsToSend[:podsPerMsg],
			Containers: ctrsToSend[:ctrsPerMsg],
			More:       len(podsToSend) > podsPerMsg || len(ctrsToSend) > ctrsPerMsg,
		}

		log.Debugf(ctx, "sending sync message, %d/%d, %d/%d (more: %v)",
			len(req.Pods), len(podsToSend), len(req.Containers), len(ctrsToSend), req.More)

		start := time.Now()
		rpl, err = p.impl.Synchronize(ctx, req)
		p.recordInvocation("Synchronize", start, err)
		if err == nil {
			if rpl != nil {
				p.recordAdjustments("Synchronize", nil, len(rpl.Update), 0)
			}
			if !req.More {
				break
			}

			if len(rpl.Update) > 0 || rpl.More != req.More {
				p.close()
				return nil, fmt.Errorf("plugin does not handle split sync requests")
			}

			podsToSend = podsToSend[podsPerMsg:]
			ctrsToSend = ctrsToSend[ctrsPerMsg:]

			if podsPerMsg > len(podsToSend) {
				podsPerMsg = len(podsToSend)
			}
			if ctrsPerMsg > len(ctrsToSend) {
				ctrsPerMsg = len(ctrsToSend)
			}
		} else {
			podsPerMsg, ctrsPerMsg, err = recalcObjsPerSyncMsg(podsPerMsg, ctrsPerMsg, err)
			if err != nil {
				p.close()
				return nil, err
			}

			log.Debugf(ctx, "oversized message, retrying in smaller chunks")
		}
	}

	return rpl.Update, nil
}

func recalcObjsPerSyncMsg(pods, ctrs int, err error) (int, int, error) {
	const (
		minObjsPerMsg = 8
	)

	if status.Code(err) != codes.ResourceExhausted {
		return pods, ctrs, err
	}

	if pods+ctrs <= minObjsPerMsg {
		return pods, ctrs, fmt.Errorf("failed to synchronize plugin with split messages")
	}

	var e *ttrpc.OversizedMessageErr
	if !errors.As(err, &e) {
		return pods, ctrs, fmt.Errorf("failed to synchronize plugin with split messages")
	}

	maxLen := e.MaximumLength()
	msgLen := e.RejectedLength()

	if msgLen == 0 || maxLen == 0 || msgLen <= maxLen {
		return pods, ctrs, fmt.Errorf("failed to synchronize plugin with split messages")
	}

	factor := float64(maxLen) / float64(msgLen)
	if factor > 0.9 {
		factor = 0.9
	}

	pods = int(float64(pods) * factor)
	ctrs = int(float64(ctrs) * factor)

	if pods+ctrs < minObjsPerMsg {
		pods = minObjsPerMsg / 2
		ctrs = minObjsPerMsg / 2
	}

	return pods, ctrs, nil
}

// Relay RunPodSandbox request to plugin.
func (p *plugin) runPodSandbox(ctx context.Context, req *RunPodSandboxRequest) (*RunPodSandboxResponse, error) {
	event := Event_RUN_POD_SANDBOX
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	rpl, err := p.impl.RunPodSandbox(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return rpl, nil
}

// Relay UpdatePodSandbox request to plugin.
func (p *plugin) updatePodSandbox(ctx context.Context, req *UpdatePodSandboxRequest) (*UpdatePodSandboxResponse, error) {
	event := Event_UPDATE_POD_SANDBOX
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.UpdatePodSandbox(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}

	return &UpdatePodSandboxResponse{}, nil
}

// Relay PostUpdatePodSandbox request to plugin.
func (p *plugin) postUpdatePodSandbox(ctx context.Context, req *PostUpdatePodSandboxRequest) (*PostUpdatePodSandboxResponse, error) {
	event := Event_POST_UPDATE_POD_SANDBOX
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.PostUpdatePodSandbox(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &PostUpdatePodSandboxResponse{}, nil
}

// Relay StopPodSandbox request to plugin.
func (p *plugin) stopPodSandbox(ctx context.Context, req *StopPodSandboxRequest) (*StopPodSandboxResponse, error) {
	event := Event_STOP_POD_SANDBOX
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.StopPodSandbox(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &StopPodSandboxResponse{}, nil
}

// Relay RemovePodSandbox request to plugin.
func (p *plugin) removePodSandbox(ctx context.Context, req *RemovePodSandboxRequest) (*RemovePodSandboxResponse, error) {
	event := Event_REMOVE_POD_SANDBOX
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.RemovePodSandbox(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &RemovePodSandboxResponse{}, nil
}

// Relay CreateContainer request to plugin.
func (p *plugin) createContainer(ctx context.Context, req *CreateContainerRequest) (*CreateContainerResponse, error) {
	event := Event_CREATE_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	rpl, err := p.impl.CreateContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	if rpl != nil {
		p.recordAdjustments(event.PrettyName(), rpl.Adjust, len(rpl.Update), len(rpl.Evict))
	}

	return rpl, nil
}

// Relay PostCreateContainer request to plugin.
func (p *plugin) postCreateContainer(ctx context.Context, req *PostCreateContainerRequest) (*PostCreateContainerResponse, error) {
	event := Event_POST_CREATE_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.PostCreateContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &PostCreateContainerResponse{}, nil
}

// Relay StartContainer request to plugin.
func (p *plugin) startContainer(ctx context.Context, req *StartContainerRequest) (*StartContainerResponse, error) {
	event := Event_START_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.StartContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &StartContainerResponse{}, nil
}

// Relay PostStartContainer request to plugin.
func (p *plugin) postStartContainer(ctx context.Context, req *PostStartContainerRequest) (*PostStartContainerResponse, error) {
	event := Event_POST_START_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.PostStartContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &PostStartContainerResponse{}, nil
}

// Relay UpdateContainer request to plugin.
func (p *plugin) updateContainer(ctx context.Context, req *UpdateContainerRequest) (*UpdateContainerResponse, error) {
	event := Event_UPDATE_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	rpl, err := p.impl.UpdateContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	if rpl != nil {
		p.recordAdjustments(event.PrettyName(), nil, len(rpl.Update), len(rpl.Evict))
	}

	return rpl, nil
}

// Relay PostUpdateContainer request to plugin.
func (p *plugin) postUpdateContainer(ctx context.Context, req *PostUpdateContainerRequest) (*PostUpdateContainerResponse, error) {
	event := Event_POST_UPDATE_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.PostUpdateContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &PostUpdateContainerResponse{}, nil
}

// Relay StopContainer request to the plugin.
func (p *plugin) stopContainer(ctx context.Context, req *StopContainerRequest) (rpl *StopContainerResponse, err error) {
	event := Event_STOP_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	rpl, err = p.impl.StopContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	if rpl != nil {
		p.recordAdjustments(event.PrettyName(), nil, len(rpl.Update), 0)
	}

	return rpl, nil
}

// Relay RemoveContainer request to plugin.
func (p *plugin) removeContainer(ctx context.Context, req *RemoveContainerRequest) (*RemoveContainerResponse, error) {
	event := Event_REMOVE_CONTAINER
	if !p.events.IsSet(event) {
		return nil, nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	_, err := p.impl.RemoveContainer(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		return nil, p.handleRequestError(ctx, event, err)
	}
	p.warnDeprecatedEvent(ctx, event)

	return &RemoveContainerResponse{}, nil
}

// Relay ValidateContainerAdjustment request to plugin.
func (p *plugin) ValidateContainerAdjustment(ctx context.Context, req *ValidateContainerAdjustmentRequest) error {
	event := Event_VALIDATE_CONTAINER_ADJUSTMENT
	if !p.events.IsSet(event) {
		return nil
	}

	rctx, cancel := context.WithTimeout(ctx, getPluginRequestTimeout())
	defer cancel()

	start := time.Now()
	rpl, err := p.impl.ValidateContainerAdjustment(rctx, req)
	p.recordInvocation(event.PrettyName(), start, err)
	if err != nil {
		if ctx.Err() == nil && p.dropsPlugin(err) {
			log.Errorf(ctx, "closing plugin %s, failed to handle %s request: %v",
				p.name(), event.PrettyName(), err)
			p.close()
		}
		return fmt.Errorf("validator plugin %s failed: %w", p.name(), err)
	}

	return rpl.ValidationResult(p.name())
}

// Record metrics for a plugin invocation.
func (p *plugin) recordInvocation(op string, since time.Time, err error) {
	p.r.metrics.RecordPluginLatency(p.name(), op, time.Since(since))
	p.r.metrics.RecordPluginInvocation(p.name(), op, err)
}

// Record metrics for adjustments requested by a plugin.
func (p *plugin) recordAdjustments(op string, adjust *ContainerAdjustment, updates, evicts int) {
	p.r.metrics.RecordPluginAdjustments(p.name(), op, adjust, updates, evicts)
}

// Warn about a plugins using deprecated StateChange for event handling.
func (p *plugin) warnDeprecatedEvent(ctx context.Context, event Event) {
	if !p.impl.deprecated[event] || p.impl.warned[event] {
		return
	}

	if p.r.deprecation != nil {
		p.r.deprecation.PluginWarning(ctx, DeprecatedStateChange, p.name(),
			fmt.Sprintf("does not implement a dedicated %s RPC call", event.PrettyName()))
	} else {
		log.Warnf(ctx, "plugin %s uses deprecated StateChange instead of a dedicated %s RPC call",
			p.name(), event.PrettyName())
		log.Warnf(ctx, "please update %s with a newer version of NRI for future compatibility",
			p.name())
	}

	p.impl.warned[event] = true
}

// handleRequestError handles an error from a relayed request. ctx is the
// context of the runtime. It is not the context with the request timeout.
//
// Two kinds of errors are not plugin failures. The function returns them
// unchanged and the request fails:
//
//   - an error returned by the plugin,
//   - any error after the runtime cancelled ctx or its deadline expired.
//     The plugin is not responsible for that, even if the request timeout
//     expired too.
//
// A fatal error is a plugin failure: the plugin did not answer the request.
// The failure policy of the plugin applies. The function takes two
// independent decisions. It drops the plugin unless the policy keeps it
// (see dropsPlugin). It fails the request only if the policy says so.
// Otherwise the request proceeds without the plugin. A returned error
// names the plugin and the request, because the runtime reports it.
func (p *plugin) handleRequestError(ctx context.Context, event Event, err error) error {
	if ctx.Err() != nil || !isFatalError(err) {
		return err
	}

	if p.dropsPlugin(err) {
		log.Errorf(ctx, "closing plugin %s, failed to handle %s request: %v",
			p.name(), event.PrettyName(), err)
		p.close()
	}

	if !p.policy.GetFailRequest() {
		log.Warnf(ctx, "plugin %s failed to handle %s request, proceeding without it: %v",
			p.name(), event.PrettyName(), err)
		return nil
	}

	return fmt.Errorf("plugin %s failed to handle %s request: %w",
		p.name(), event.PrettyName(), err)
}

// dropsPlugin returns true if the plugin must be closed and unregistered
// for the error. A fatal error drops the plugin, with one exception: a
// request timeout does not drop a plugin with KeepConnected set. The timeout
// only means that the plugin did not handle this request in time. A lost
// connection or a protocol error always drops the plugin.
func (p *plugin) dropsPlugin(err error) bool {
	if !isFatalError(err) {
		return false
	}
	return !errors.Is(err, context.DeadlineExceeded) || !p.policy.GetKeepConnected()
}

// isFatalError returns true if the error is fatal and the plugin connection should be closed.
func isFatalError(err error) bool {
	switch {
	case errors.Is(err, ttrpc.ErrClosed):
		return true
	case errors.Is(err, ttrpc.ErrServerClosed):
		return true
	case errors.Is(err, ttrpc.ErrProtocol):
		return true
	case errors.Is(err, context.DeadlineExceeded):
		return true
	}
	return false
}

// wasmHostFunctions implements the webassembly host functions
type wasmHostFunctions struct{}

func (wasmHostFunctions) Log(ctx context.Context, request *api.LogRequest) (*api.Empty, error) {
	switch request.GetLevel() {
	case api.LogRequest_LEVEL_INFO:
		log.Infof(ctx, request.GetMsg())
	case api.LogRequest_LEVEL_WARN:
		log.Warnf(ctx, request.GetMsg())
	case api.LogRequest_LEVEL_ERROR:
		log.Errorf(ctx, request.GetMsg())
	default:
		log.Debugf(ctx, request.GetMsg())
	}

	return &api.Empty{}, nil
}

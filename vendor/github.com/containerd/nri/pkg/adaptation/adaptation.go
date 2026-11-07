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
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/containerd/nri/pkg/adaptation/builtin"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/log"
	nriver "github.com/containerd/nri/pkg/version"
	validator "github.com/containerd/nri/plugins/default-validator/builtin"
	"github.com/containerd/ttrpc"

	"google.golang.org/protobuf/proto"
)

const (
	// DefaultPluginPath is the default path to search for NRI plugins.
	DefaultPluginPath = "/opt/nri/plugins"
	// DefaultSocketPath is the default socket path for external plugins.
	DefaultSocketPath = api.DefaultSocketPath
	// DefaultPluginConfigPath is the drop-in directory for NRI-launched plugin configuration.
	DefaultPluginConfigPath = "/etc/nri/conf.d"
)

// SyncFn is a container runtime function for state synchronization.
type SyncFn func(context.Context, SyncCB) error

// SyncCB is an NRI function used to synchronize plugins with the runtime.
type SyncCB func(context.Context, []*PodSandbox, []*Container) ([]*ContainerUpdate, error)

// UpdateFn is a container runtime function for unsolicited container updates.
type UpdateFn func(context.Context, []*ContainerUpdate) ([]*ContainerUpdate, error)

// Adaptation is the NRI abstraction for container runtime NRI adaptation/integration.
type Adaptation struct {
	sync.Mutex
	name        string
	version     string
	nriVersion  string
	dropinPath  string
	pluginPath  string
	socketPath  string
	dontListen  bool
	syncFn      SyncFn
	updateFn    UpdateFn
	clientOpts  []ttrpc.ClientOpts
	serverOpts  []ttrpc.ServerOpt
	listener    net.Listener
	plugins     []*plugin
	validators  []*plugin
	builtin     []*builtin.BuiltinPlugin
	syncLock    sync.RWMutex
	wasmService *api.PluginPlugin
	metrics     Metrics
	deprecation DeprecationRecorder
}

var (
	// Used instead of nil Context in logging.
	noCtx = context.TODO()

	// ErrWasmDisabled is returned for WASM initialization if WASM support is disabled.
	ErrWasmDisabled = errors.New("WASM support is disabled (at build time)")
)

// Option to apply to the NRI runtime.
type Option func(*Adaptation) error

// WithPluginPath returns an option to override the default NRI plugin path.
func WithPluginPath(path string) Option {
	return func(r *Adaptation) error {
		r.pluginPath = path
		return nil
	}
}

// WithPluginConfigPath returns an option to override the default NRI plugin config path.
func WithPluginConfigPath(path string) Option {
	return func(r *Adaptation) error {
		r.dropinPath = path
		return nil
	}
}

// WithSocketPath returns an option to override the default NRI socket path.
func WithSocketPath(path string) Option {
	return func(r *Adaptation) error {
		r.socketPath = path
		return nil
	}
}

// WithDisabledExternalConnections returns an options to disable accepting plugin connections.
func WithDisabledExternalConnections() Option {
	return func(r *Adaptation) error {
		r.dontListen = true
		return nil
	}
}

// WithTTRPCOptions sets extra client and server options to use for ttrpc.
func WithTTRPCOptions(clientOpts []ttrpc.ClientOpts, serverOpts []ttrpc.ServerOpt) Option {
	return func(r *Adaptation) error {
		r.clientOpts = append(r.clientOpts, clientOpts...)
		r.serverOpts = append(r.serverOpts, serverOpts...)
		return nil
	}
}

// WithBuiltinPlugins sets extra builtin plugins to register.
func WithBuiltinPlugins(plugins ...*builtin.BuiltinPlugin) Option {
	return func(r *Adaptation) error {
		r.builtin = append(r.builtin, plugins...)
		return nil
	}
}

// WithMetrics allows consumers to register an implementation of the Metrics interface.
func WithMetrics(m Metrics) Option {
	return func(r *Adaptation) error {
		if m != nil {
			r.metrics = m
		}
		return nil
	}
}

// WithDefaultValidator sets up builtin validator plugin if it is configured.
func WithDefaultValidator(cfg *validator.DefaultValidatorConfig) Option {
	return func(r *Adaptation) error {
		if plugin := validator.GetDefaultValidator(cfg); plugin != nil {
			r.builtin = append([]*builtin.BuiltinPlugin{plugin}, r.builtin...)
		}
		return nil
	}
}

// WithDeprecationRecorder sets up functions to record usage of deprecated
// NRI interfaces or features.
func WithDeprecationRecorder(d DeprecationRecorder) Option {
	return func(r *Adaptation) error {
		r.deprecation = d
		return nil
	}
}

// New creates a new NRI Runtime.
func New(name, version string, syncFn SyncFn, updateFn UpdateFn, opts ...Option) (*Adaptation, error) {
	var err error

	if syncFn == nil {
		return nil, fmt.Errorf("failed to create NRI adaptation, nil SyncFn")
	}
	if updateFn == nil {
		return nil, fmt.Errorf("failed to create NRI adaptation, nil UpdateFn")
	}

	wasmService, err := getWasmService()
	if err != nil {
		log.Errorf(noCtx, "failed to initialize WASM support: %v", err)
		if !errors.Is(err, ErrWasmDisabled) {
			return nil, err
		}
	}

	r := &Adaptation{
		name:        name,
		version:     version,
		nriVersion:  nriver.GetFromBuildInfo(),
		syncFn:      syncFn,
		updateFn:    updateFn,
		pluginPath:  DefaultPluginPath,
		dropinPath:  DefaultPluginConfigPath,
		socketPath:  DefaultSocketPath,
		syncLock:    sync.RWMutex{},
		wasmService: wasmService,
		metrics:     &noopMetrics{},
	}

	for _, o := range opts {
		if err = o(r); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	log.Infof(noCtx, "runtime interface created")

	return r, nil
}

// Start up the NRI runtime.
func (r *Adaptation) Start() error {
	log.Infof(noCtx, "runtime interface starting up...")

	r.Lock()
	defer r.Unlock()

	if err := r.startPlugins(); err != nil {
		return err
	}

	if err := r.startListener(); err != nil {
		return err
	}

	return nil
}

// Stop the NRI runtime.
func (r *Adaptation) Stop() {
	log.Infof(noCtx, "runtime interface shutting down...")

	r.Lock()
	defer r.Unlock()

	r.stopListener()
	r.stopPlugins()
}

// RunPodSandbox relays the corresponding CRI request to plugins.
func (r *Adaptation) RunPodSandbox(ctx context.Context, req *RunPodSandboxRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		if _, err := plugin.runPodSandbox(ctx, req); err != nil {
			return err
		}
	}

	return nil
}

// UpdatePodSandbox relays the corresponding CRI request to plugins.
func (r *Adaptation) UpdatePodSandbox(ctx context.Context, req *UpdatePodSandboxRequest) (*UpdatePodSandboxResponse, error) {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.updatePodSandbox(ctx, req)
		if err != nil {
			return nil, err
		}
	}

	return &UpdatePodSandboxResponse{}, nil
}

// PostUpdatePodSandbox relays the corresponding CRI request to plugins.
func (r *Adaptation) PostUpdatePodSandbox(ctx context.Context, req *PostUpdatePodSandboxRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.postUpdatePodSandbox(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// StopPodSandbox relays the corresponding CRI request to plugins.
func (r *Adaptation) StopPodSandbox(ctx context.Context, req *StopPodSandboxRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.stopPodSandbox(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// RemovePodSandbox relays the corresponding CRI request to plugins.
func (r *Adaptation) RemovePodSandbox(ctx context.Context, req *RemovePodSandboxRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.removePodSandbox(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// CreateContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) CreateContainer(ctx context.Context, req *CreateContainerRequest) (*CreateContainerResponse, error) {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	var (
		result   = collectCreateContainerResult(req)
		validate *ValidateContainerAdjustmentRequest
	)

	if r.hasValidators() {
		validate = &ValidateContainerAdjustmentRequest{
			Pod:       req.Pod,
			Container: proto.Clone(req.Container).(*Container),
		}
	}

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		if validate != nil {
			validate.AddPlugin(plugin.base, plugin.idx)
		}
		rpl, err := plugin.createContainer(ctx, req)
		if err != nil {
			return nil, err
		}
		err = result.apply(rpl, plugin.name())
		if err != nil {
			return nil, err
		}
	}

	return r.validateContainerAdjustment(ctx, validate, result)
}

// PostCreateContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) PostCreateContainer(ctx context.Context, req *PostCreateContainerRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.postCreateContainer(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// StartContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) StartContainer(ctx context.Context, req *StartContainerRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.startContainer(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// PostStartContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) PostStartContainer(ctx context.Context, req *PostStartContainerRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.postStartContainer(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) UpdateContainer(ctx context.Context, req *UpdateContainerRequest) (*UpdateContainerResponse, error) {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	result := collectUpdateContainerResult(req)
	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		rpl, err := plugin.updateContainer(ctx, req)
		if err != nil {
			return nil, err
		}
		err = result.apply(rpl, plugin.name())
		if err != nil {
			return nil, err
		}
	}

	return result.updateContainerResponse(), nil
}

// PostUpdateContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) PostUpdateContainer(ctx context.Context, req *PostUpdateContainerRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.postUpdateContainer(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// StopContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) StopContainer(ctx context.Context, req *StopContainerRequest) (*StopContainerResponse, error) {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	result := collectStopContainerResult()
	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		rpl, err := plugin.stopContainer(ctx, req)
		if err != nil {
			return nil, err
		}
		err = result.apply(rpl, plugin.name())
		if err != nil {
			return nil, err
		}
	}

	return result.stopContainerResponse(), nil
}

// RemoveContainer relays the corresponding CRI request to plugins.
func (r *Adaptation) RemoveContainer(ctx context.Context, req *RemoveContainerRequest) error {
	r.Lock()
	defer r.Unlock()
	defer r.removeClosedPlugins()

	for _, plugin := range r.plugins {
		if plugin.isClosed() {
			continue
		}
		_, err := plugin.removeContainer(ctx, req)
		if err != nil {
			return err
		}
	}

	return nil
}

// Perform a set of unsolicited container updates requested by a plugin.
func (r *Adaptation) updateContainers(ctx context.Context, req []*ContainerUpdate) ([]*ContainerUpdate, error) {
	return r.updateFn(ctx, req)
}

// Validate requested container adjustments.
func (r *Adaptation) validateContainerAdjustment(ctx context.Context, req *ValidateContainerAdjustmentRequest, result *result) (*CreateContainerResponse, error) {
	rpl := result.createContainerResponse()

	if req == nil || len(r.validators) == 0 {
		return rpl, nil
	}

	req.AddResponse(rpl)
	req.AddOwners(result.owners)

	errors := make(chan error, len(r.validators))
	wg := sync.WaitGroup{}

	for _, p := range r.validators {
		if p.isClosed() {
			continue
		}
		wg.Add(1)
		go func(p *plugin) {
			defer wg.Done()
			errors <- p.ValidateContainerAdjustment(ctx, req)
		}(p)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			return nil, err
		}
	}

	return rpl, nil
}

// Start up pre-installed plugins.
func (r *Adaptation) startPlugins() (retErr error) {
	var plugins []*plugin

	log.Infof(noCtx, "starting plugins...")

	ids, names, configs, err := r.discoverPlugins()
	if err != nil {
		return err
	}

	defer func() {
		if retErr != nil {
			for _, p := range plugins {
				p.stop()
			}
		}
	}()

	for _, b := range r.builtin {
		log.Infof(noCtx, "starting builtin NRI plugin %q...", b.Index+"-"+b.Base)
		p, err := r.newBuiltinPlugin(b)
		if err != nil {
			return fmt.Errorf("failed to initialize builtin NRI plugin %q: %v", b.Base, err)
		}

		if err := p.start(r.name, r.version); err != nil {
			return fmt.Errorf("failed to start builtin NRI plugin %q: %v", b.Base, err)
		}

		plugins = append(plugins, p)
	}

	for i, name := range names {
		log.Infof(noCtx, "starting pre-installed NRI plugin %q...", name)

		id := ids[i]
		p, err := r.newLaunchedPlugin(r.pluginPath, id, name, configs[i])
		if err != nil {
			log.Warnf(noCtx, "failed to initialize pre-installed NRI plugin %q: %v", name, err)
			continue
		}

		if err := p.start(r.name, r.version); err != nil {
			log.Warnf(noCtx, "failed to start pre-installed NRI plugin %q: %v", name, err)
			continue
		}

		plugins = append(plugins, p)
	}

	// Although the error returned by syncPlugins may not be nil, r.syncFn could still ignores this error and returns a nil error.
	// We need to make sure that the plugins are successfully synchronized in the `plugins`
	syncPlugins := func(ctx context.Context, pods []*PodSandbox, containers []*Container) (updates []*ContainerUpdate, err error) {
		startedPlugins := plugins
		plugins = make([]*plugin, 0, len(plugins))
		for _, plugin := range startedPlugins {
			us, err := plugin.synchronize(ctx, pods, containers)
			if err != nil {
				plugin.stop()
				log.Warnf(noCtx, "failed to synchronize pre-installed NRI plugin %q: %v", plugin.name(), err)
				continue
			}

			plugins = append(plugins, plugin)
			updates = append(updates, us...)
			log.Infof(noCtx, "pre-installed NRI plugin %q synchronization success", plugin.name())
		}
		return updates, nil
	}
	if err := r.syncFn(noCtx, syncPlugins); err != nil {
		return fmt.Errorf("failed to synchronize pre-installed NRI Plugins: %w", err)
	}

	r.plugins = plugins
	r.sortPlugins()
	return nil
}

// Stop plugins.
func (r *Adaptation) stopPlugins() {
	log.Infof(noCtx, "stopping plugins...")

	for _, p := range r.plugins {
		p.stop()
	}
	r.plugins = nil
}

// removeClosedPlugins removes closed plugins from the registry. The caller
// must hold the lock. A plugin is closed when a request drops it, or when
// its connection closes while no request is in progress. It is removed at
// the end of the next request. Until then, requests skip it: a request
// relayed to it fails with ttrpc.ErrClosed, and a fail_request policy
// would fail the request for a plugin that is already gone.
func (r *Adaptation) removeClosedPlugins() {
	var active, closed, validators []*plugin
	for _, p := range r.plugins {
		if p.isClosed() {
			closed = append(closed, p)
		} else {
			active = append(active, p)
			if p.isContainerAdjustmentValidator() {
				validators = append(validators, p)
			}
		}
	}

	if len(closed) != 0 {
		go func() {
			for _, plugin := range closed {
				plugin.stop()
			}
		}()
	}

	r.plugins = active
	r.validators = validators
	r.metrics.UpdatePluginCount(len(r.plugins))
}

func (r *Adaptation) startListener() error {
	if r.dontListen {
		log.Infof(noCtx, "connection from external plugins disabled")
		return nil
	}

	os.Remove(r.socketPath)
	if err := os.MkdirAll(filepath.Dir(r.socketPath), 0700); err != nil {
		return fmt.Errorf("failed to create socket %q: %w", r.socketPath, err)
	}

	l, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: r.socketPath,
		Net:  "unix",
	})
	if err != nil {
		return fmt.Errorf("failed to create socket %q: %w", r.socketPath, err)
	}

	r.acceptPluginConnections(l)

	return nil
}

func (r *Adaptation) stopListener() {
	if r.listener != nil {
		r.listener.Close()
	}
}

func (r *Adaptation) acceptPluginConnections(l net.Listener) error {
	r.listener = l

	ctx := context.Background()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				log.Infof(ctx, "stopped accepting plugin connections (%v)", err)
				return
			}

			p, err := r.newExternalPlugin(conn)
			if err != nil {
				log.Errorf(ctx, "failed to create external plugin: %v", err)
				continue
			}

			if err := p.start(r.name, r.version); err != nil {
				log.Errorf(ctx, "failed to start external plugin: %v", err)
				continue
			}

			r.requestPluginSync()
			err = r.syncFn(ctx, p.synchronize)
			if err != nil {
				log.Errorf(ctx, "failed to synchronize plugin: %v", err)
				p.close()
			} else {
				r.Lock()
				r.plugins = append(r.plugins, p)
				if p.isContainerAdjustmentValidator() {
					r.validators = append(r.validators, p)
				}
				r.sortPlugins()
				count := len(r.plugins)
				r.Unlock()
				r.metrics.UpdatePluginCount(count)
				log.Infof(ctx, "plugin %q connected and synchronized", p.name())
			}
			r.finishedPluginSync()
		}
	}()

	return nil
}

func (r *Adaptation) discoverPlugins() ([]string, []string, []string, error) {
	var (
		plugins []string
		indices []string
		configs []string
		entries []os.DirEntry
		info    fs.FileInfo
		err     error
	)

	if entries, err = os.ReadDir(r.pluginPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("failed to discover plugins in %s: %w",
			r.pluginPath, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err = e.Info(); err != nil {
			continue
		}
		if info.Mode()&fs.FileMode(0o111) == 0 {
			continue
		}

		name := e.Name()
		idx, base, err := api.ParsePluginName(name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to discover plugins in %s: %w",
				r.pluginPath, err)
		}

		cfg, err := r.getPluginConfig(idx, base)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to discover plugins in %s: %w",
				r.pluginPath, err)
		}

		log.Infof(noCtx, "discovered plugin %s", name)

		indices = append(indices, idx)
		plugins = append(plugins, base)
		configs = append(configs, cfg)
	}

	return indices, plugins, configs, nil
}

func (r *Adaptation) sortPlugins() {
	r.removeClosedPlugins()
	sort.Slice(r.plugins, func(i, j int) bool {
		return r.plugins[i].idx < r.plugins[j].idx
	})
	sort.Slice(r.validators, func(i, j int) bool {
		return r.validators[i].idx < r.validators[j].idx
	})
	if len(r.plugins) > 0 {
		log.Infof(noCtx, "plugin invocation order")
		for i, p := range r.plugins {
			log.Infof(noCtx, "  #%d: %q (%s)", i+1, p.name(), p.qualifiedName())
		}
	}
	if len(r.validators) > 0 {
		log.Infof(noCtx, "validator plugins")
		for _, p := range r.validators {
			log.Infof(noCtx, "  %q (%s)", p.name(), p.qualifiedName())
		}
	}
}

func (r *Adaptation) hasValidators() bool {
	return len(r.validators) > 0
}

func (r *Adaptation) requestPluginSync() {
	r.syncLock.Lock()
}

func (r *Adaptation) finishedPluginSync() {
	r.syncLock.Unlock()
}

// PluginSyncBlock is a handle for blocking plugin synchronization.
type PluginSyncBlock struct {
	r *Adaptation
}

// BlockPluginSync blocks plugins from being synchronized/fully registered.
func (r *Adaptation) BlockPluginSync() *PluginSyncBlock {
	r.syncLock.RLock()
	return &PluginSyncBlock{r: r}
}

// Unblock a plugin sync. block put in place by BlockPluginSync. Safe to call
// multiple times but only from a single goroutine.
func (b *PluginSyncBlock) Unblock() {
	if b != nil && b.r != nil {
		b.r.syncLock.RUnlock()
		b.r = nil
	}
}

//go:build linux

package runcexecutor

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/moby/buildkit/util/bklog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/containerd/containerd/v2/core/mount"
	containerdoci "github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/continuity/fs"
	runc "github.com/containerd/go-runc"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/executor/resources"
	resourcestypes "github.com/moby/buildkit/executor/resources/types"
	gatewayapi "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/network"
	rootlessspecconv "github.com/moby/buildkit/util/rootless/specconv"
	"github.com/moby/buildkit/util/stack"
	"github.com/moby/sys/user"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

const runcStartTimeout = 15 * time.Second

type Opt struct {
	// root directory
	Root              string
	CommandCandidates []string
	// without root privileges (has nothing to do with Opt.Root directory)
	Rootless bool
	// DefaultCgroupParent is the cgroup-parent name for executor
	DefaultCgroupParent string
	// ProcessMode
	ProcessMode     oci.ProcessMode
	IdentityMapping *user.IdentityMapping
	// runc run --no-pivot (unrecommended)
	NoPivot         bool
	DNS             *oci.DNSConfig
	OOMScoreAdj     *int
	ApparmorProfile string
	SELinux         bool
	TracingSocket   string
	ResourceMonitor *resources.Monitor
	CDIManager      *cdidevices.Manager
	ProxyProvider   network.ProxyProvider
}

var defaultCommandCandidates = []string{"buildkit-runc", "runc"}

type runcExecutor struct {
	runc             *runc.Runc
	root             string
	cgroupParent     string
	rootless         bool
	networkProviders map[pb.NetMode]network.Provider
	proxyProvider    network.ProxyProvider
	processMode      oci.ProcessMode
	idmap            *user.IdentityMapping
	noPivot          bool
	dns              *oci.DNSConfig
	oomScoreAdj      *int
	running          map[string]chan error
	mu               sync.Mutex
	apparmorProfile  string
	selinux          bool
	tracingSocket    string
	resmon           *resources.Monitor
	cdiManager       *cdidevices.Manager
}

func New(opt Opt, networkProviders map[pb.NetMode]network.Provider) (executor.Executor, error) {
	cmds := opt.CommandCandidates
	if cmds == nil {
		cmds = defaultCommandCandidates
	}

	var cmd string
	var found bool
	for _, cmd = range cmds {
		if _, err := exec.LookPath(cmd); err == nil {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.Errorf("failed to find %s binary", cmd)
	}

	root := opt.Root

	if err := os.MkdirAll(root, 0o711); err != nil {
		return nil, errors.Wrapf(err, "failed to create %s", root)
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}

	// clean up old hosts/resolv.conf file. ignore errors
	os.RemoveAll(filepath.Join(root, "hosts"))
	os.RemoveAll(filepath.Join(root, "resolv.conf"))

	runtime := &runc.Runc{
		Command:   cmd,
		Log:       filepath.Join(root, "runc-log.json"),
		LogFormat: runc.JSON,
		Setpgid:   true,
		// we don't execute runc with --rootless=(true|false) explicitly,
		// so as to support non-runc runtimes
	}

	updateRuncFieldsForHostOS(runtime)

	w := &runcExecutor{
		runc:             runtime,
		root:             root,
		cgroupParent:     opt.DefaultCgroupParent,
		rootless:         opt.Rootless,
		networkProviders: networkProviders,
		proxyProvider:    opt.ProxyProvider,
		processMode:      opt.ProcessMode,
		idmap:            opt.IdentityMapping,
		noPivot:          opt.NoPivot,
		dns:              opt.DNS,
		oomScoreAdj:      opt.OOMScoreAdj,
		running:          make(map[string]chan error),
		apparmorProfile:  opt.ApparmorProfile,
		selinux:          opt.SELinux,
		tracingSocket:    opt.TracingSocket,
		resmon:           opt.ResourceMonitor,
		cdiManager:       opt.CDIManager,
	}
	return w, nil
}

func (w *runcExecutor) Run(ctx context.Context, id string, root executor.Mount, mounts []executor.Mount, process executor.ProcessInfo, started chan<- struct{}) (rec resourcestypes.Recorder, err error) {
	if id == "" {
		id = identity.NewID()
	}
	if err := executor.ValidContainerID(id); err != nil {
		return nil, err
	}

	startedOnce := sync.Once{}
	done := make(chan error, 1)
	w.mu.Lock()
	w.running[id] = done
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.running, id)
		w.mu.Unlock()
		done <- err
		close(done)
		if started != nil {
			startedOnce.Do(func() {
				close(started)
			})
		}
	}()

	meta := process.Meta
	if meta.NetMode == pb.NetMode_HOST {
		bklog.G(ctx).Info("enabling HostNetworking")
	}

	proxyConfig := meta.Proxy
	var provider network.Provider
	if proxyConfig == nil {
		var ok bool
		provider, ok = w.networkProviders[meta.NetMode]
		if !ok {
			return nil, errors.Errorf("unknown network mode %s", meta.NetMode)
		}
	} else if w.proxyProvider == nil {
		return nil, errors.New("proxy network provider is not available")
	} else {
		proxyConfig = &network.ProxyConfig{
			Policy:     proxyConfig.Policy,
			Capture:    proxyConfig.Capture,
			EgressMode: meta.NetMode,
		}
	}
	var namespace network.Namespace
	if proxyConfig != nil {
		namespace, err = w.proxyProvider.NewProxy(ctx, proxyConfig)
	} else {
		namespace, err = provider.New(ctx, meta.Hostname, network.NamespaceOptions{})
	}
	if err != nil {
		return nil, err
	}
	if proxyNS, ok := namespace.(network.ProxyNamespace); ok {
		meta.Env = executor.ReplaceEnv(meta.Env, proxyNS.ProxyEnv())
	}
	doReleaseNetwork := true
	defer func() {
		if doReleaseNetwork {
			namespace.Close()
		}
	}()

	stateDirRoot, err := os.OpenRoot(w.root)
	if err != nil {
		return nil, err
	}
	defer stateDirRoot.Close()

	resolvConfName, err := oci.GetResolvConf(ctx, stateDirRoot, w.idmap, w.dns, meta.NetMode)
	if err != nil {
		return nil, err
	}
	resolvConf := filepath.Join(w.root, resolvConfName)

	hostsName, clean, err := oci.GetHostsFile(ctx, stateDirRoot, meta.ExtraHosts, w.idmap, meta.Hostname)
	if err != nil {
		return nil, err
	}
	hostsFile := filepath.Join(w.root, hostsName)
	if clean != nil {
		defer clean()
	}

	mountable, err := root.Src.Mount(ctx, false)
	if err != nil {
		return nil, err
	}

	rootMount, release, err := mountable.Mount()
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}

	bundle := filepath.Join(w.root, id)

	if err := os.Mkdir(bundle, 0o711); err != nil {
		return nil, errors.WithStack(err)
	}
	defer os.RemoveAll(bundle)

	var rootUID, rootGID int
	if w.idmap != nil {
		rootUID, rootGID = w.idmap.RootPair()
	}

	rootFSPath := filepath.Join(bundle, "rootfs")
	if err := user.MkdirAllAndChown(rootFSPath, 0o700, rootUID, rootGID); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := mount.All(rootMount, rootFSPath); err != nil {
		return nil, errors.WithStack(err)
	}
	defer mount.Unmount(rootFSPath, 0)

	defer executor.MountStubsCleaner(context.WithoutCancel(ctx), rootFSPath, mounts, meta.RemoveMountStubsRecursive)()
	if proxyNS, ok := namespace.(network.ProxyNamespace); ok {
		cleanProxyCA, injectErr := executor.InjectProxyCA(rootFSPath, proxyNS.ProxyCACert())
		if injectErr != nil {
			return nil, injectErr
		}
		defer func() {
			if cleanupErr := cleanProxyCA(); cleanupErr != nil {
				err = errors.WithStack(stderrors.Join(
					err,
					errors.Wrap(cleanupErr, "failed to clean up proxy CA"),
				))
			}
		}()
	}

	uid, gid, sgids, err := oci.GetUser(rootFSPath, meta.User)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	f, err := os.Create(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	opts := []containerdoci.SpecOpts{oci.WithUIDGID(uid, gid, sgids)}

	if meta.ReadonlyRootFS {
		opts = append(opts, containerdoci.WithRootFSReadonly())
	}

	rootUID, rootGID = int(uid), int(gid)
	if w.idmap != nil {
		rootUID, rootGID, err = w.idmap.ToHost(rootUID, rootGID)
		if err != nil {
			return nil, err
		}
	}

	spec, cleanup, err := oci.GenerateSpec(ctx, meta, mounts, id, resolvConf, hostsFile, namespace, w.cgroupParent, w.processMode, w.idmap, w.apparmorProfile, w.selinux, w.tracingSocket, w.cdiManager, opts...)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	spec.Root.Path = rootFSPath
	if root.Readonly {
		spec.Root.Readonly = true
	}

	newp, err := fs.RootPath(rootFSPath, meta.Cwd)
	if err != nil {
		return nil, errors.Wrapf(err, "working dir %s points to invalid target", newp)
	}
	if _, err := os.Stat(newp); err != nil {
		if err := user.MkdirAllAndChown(newp, 0o755, rootUID, rootGID); err != nil {
			return nil, errors.Wrapf(err, "failed to create working directory %s", newp)
		}
	}

	spec.Process.Terminal = meta.Tty
	spec.Process.OOMScoreAdj = w.oomScoreAdj
	if w.rootless {
		var removedMounts []string
		removedMounts, err = rootlessspecconv.ToRootless(spec)
		if err != nil {
			return nil, err
		}
		// The runtime no longer sets these mounts up, but a rootful build still gets
		// their mount points left in the rootfs. Recreate them once the container is
		// gone: creating them up front would turn a mount point that the image does
		// not ship into a directory the build can write to. moby/buildkit#6686
		var stubUID, stubGID int
		if w.idmap != nil {
			stubUID, stubGID = w.idmap.RootPair()
		}
		defer func() {
			if err == nil {
				err = executor.CreateMountStubs(rootFSPath, removedMounts, stubUID, stubGID)
			}
		}()
	}

	if err := json.NewEncoder(f).Encode(spec); err != nil {
		return nil, errors.WithStack(err)
	}

	bklog.G(ctx).Debugf("> creating %s %v", id, meta.Args)

	cgroupPath := spec.Linux.CgroupsPath
	if cgroupPath != "" {
		rec, err = w.resmon.RecordNamespace(cgroupPath, resources.RecordOpt{
			NetworkSampler: namespace,
		})
		if err != nil {
			return nil, err
		}
	}

	trace.SpanFromContext(ctx).AddEvent("Container created")
	err = w.run(ctx, id, bundle, process, func() {
		startedOnce.Do(func() {
			trace.SpanFromContext(ctx).AddEvent("Container started")
			if started != nil {
				close(started)
			}
			if rec != nil {
				rec.Start()
			}
		})
	}, true)

	releaseContainer := func(ctx context.Context) error {
		err := w.runc.Delete(ctx, id, &runc.DeleteOpts{})
		err1 := namespace.Close()
		if err == nil {
			err = err1
		}
		return err
	}
	doReleaseNetwork = false

	err = exitError(ctx, cgroupPath, err, process.Meta.ValidExitCodes)
	if err != nil {
		if rec != nil {
			rec.Close()
		}
		releaseContainer(context.TODO())
		return nil, err
	}

	if rec == nil {
		return nil, releaseContainer(context.TODO())
	}

	return rec, rec.CloseAsync(releaseContainer)
}

func exitError(ctx context.Context, cgroupPath string, err error, validExitCodes []int) error {
	exitErr := &gatewayapi.ExitError{ExitCode: uint32(gatewayapi.UnknownExitStatus), Err: err}

	if err == nil {
		exitErr.ExitCode = 0
	} else {
		var runcExitError *runc.ExitError
		if errors.As(err, &runcExitError) {
			exitErr = &gatewayapi.ExitError{ExitCode: uint32(runcExitError.Status)}
		}

		detectOOM(ctx, cgroupPath, exitErr)
	}

	trace.SpanFromContext(ctx).AddEvent(
		"Container exited",
		trace.WithAttributes(attribute.Int("exit.code", int(exitErr.ExitCode))),
	)

	if validExitCodes == nil {
		// no exit codes specified, so only 0 is allowed
		if exitErr.ExitCode == 0 {
			return nil
		}
	} else {
		// exit code in allowed list, so exit cleanly
		if slices.Contains(validExitCodes, int(exitErr.ExitCode)) {
			return nil
		}
	}

	select {
	case <-ctx.Done():
		exitErr.Err = errors.Wrap(context.Cause(ctx), exitErr.Error())
		return exitErr
	default:
		return stack.Enable(exitErr)
	}
}

func (w *runcExecutor) Exec(ctx context.Context, id string, process executor.ProcessInfo) (err error) {
	meta := process.Meta
	// first verify the container is running, if we get an error assume the container
	// is in the process of being created and check again every 100ms or until
	// context is canceled.
	var state *runc.Container
	for {
		w.mu.Lock()
		done, ok := w.running[id]
		w.mu.Unlock()
		if !ok {
			return errors.Errorf("container %s not found", id)
		}

		state, _ = w.runc.State(ctx, id)
		if state != nil && state.Status == "running" {
			break
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case err, ok := <-done:
			if !ok || err == nil {
				return errors.Errorf("container %s has stopped", id)
			}
			return errors.Wrapf(err, "container %s has exited with error", id)
		case <-time.After(100 * time.Millisecond):
		}
	}

	// load default process spec (for Env, Cwd etc) from bundle
	f, err := os.Open(filepath.Join(state.Bundle, "config.json"))
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	spec := &specs.Spec{}
	dec := json.NewDecoder(f)
	if err := dec.Decode(spec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON spec object")
	}
	if meta.Proxy != nil && len(meta.Env) > 0 {
		meta.Env = executor.ReplaceEnv(meta.Env, network.FilterProxyEnv(spec.Process.Env))
		process.Meta = meta
	}

	if meta.User != "" {
		uid, gid, sgids, err := oci.GetUser(state.Rootfs, meta.User)
		if err != nil {
			return err
		}
		spec.Process.User = specs.User{
			UID:            uid,
			GID:            gid,
			AdditionalGids: sgids,
		}
	}

	spec.Process.Terminal = meta.Tty
	spec.Process.Args = meta.Args
	if meta.Cwd != "" {
		spec.Process.Cwd = meta.Cwd
	}

	if len(meta.Env) > 0 {
		spec.Process.Env = meta.Env
	}

	err = w.exec(ctx, id, spec.Process, process, nil)
	return exitError(ctx, "", err, process.Meta.ValidExitCodes)
}

type forwardIO struct {
	stdin          io.ReadCloser
	stdout, stderr io.WriteCloser
}

func (s *forwardIO) Close() error {
	return nil
}

func (s *forwardIO) Set(cmd *exec.Cmd) {
	cmd.Stdin = s.stdin
	cmd.Stdout = s.stdout
	cmd.Stderr = s.stderr
}

func (s *forwardIO) Stdin() io.WriteCloser {
	return nil
}

func (s *forwardIO) Stdout() io.ReadCloser {
	return nil
}

func (s *forwardIO) Stderr() io.ReadCloser {
	return nil
}

// newRunProcKiller returns an abstraction for sending SIGKILL to the
// process inside the container initiated from `runc run`.
func newRunProcKiller(runC *runc.Runc, id string) procKiller {
	return procKiller{runC: runC, id: id}
}

// newExecProcKiller returns an abstraction for sending SIGKILL to the
// process inside the container initiated from `runc exec`.
func newExecProcKiller(runC *runc.Runc, id string) (procKiller, error) {
	// for `runc exec` we need to create a pidfile and read it later to kill
	// the process
	tdir, err := os.MkdirTemp("", "runc")
	if err != nil {
		return procKiller{}, errors.Wrap(err, "failed to create directory for runc pidfile")
	}

	return procKiller{
		runC:    runC,
		id:      id,
		pidfile: filepath.Join(tdir, "pidfile"),
		cleanup: func() {
			os.RemoveAll(tdir)
		},
	}, nil
}

type procKiller struct {
	runC    *runc.Runc
	id      string
	pidfile string
	cleanup func()
}

// Cleanup will delete any tmp files created for the pidfile allocation
// if this killer was for a `runc exec` process.
func (k procKiller) Cleanup() {
	if k.cleanup != nil {
		k.cleanup()
	}
}

// Kill will send SIGKILL to the process running inside the container.
// If the process was created by `runc run` then we will use `runc kill`,
// otherwise for `runc exec` we will read the pid from a pidfile and then
// send the signal directly that process.
func (k procKiller) Kill(ctx context.Context) (err error) {
	bklog.G(ctx).Debugf("sending sigkill to process in container %s", k.id)
	defer func() {
		if err != nil {
			bklog.G(ctx).Errorf("failed to kill process in container id %s: %+v", k.id, err)
		}
	}()

	// this timeout is generally a no-op, the Kill ctx should already have a
	// shorter timeout but here as a fail-safe for future refactoring.
	ctx, cancel := context.WithCancelCause(ctx)
	ctx, _ = context.WithTimeoutCause(ctx, 10*time.Second, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
	defer func() { cancel(errors.WithStack(context.Canceled)) }()

	if k.pidfile == "" {
		// for `runc run` process we use `runc kill` to terminate the process
		return k.runC.Kill(ctx, k.id, int(syscall.SIGKILL), nil)
	}

	// `runc exec` will write the pidfile a few milliseconds after we
	// get the runc pid via the startedCh, so we might need to retry until
	// it appears in the edge case where we want to kill a process
	// immediately after it was created.
	var pidData []byte
	for {
		pidData, err = os.ReadFile(k.pidfile)
		if err != nil {
			if os.IsNotExist(err) {
				select {
				case <-ctx.Done():
					return errors.New("context cancelled before runc wrote pidfile")
				case <-time.After(10 * time.Millisecond):
					continue
				}
			}
			return errors.Wrap(err, "failed to read pidfile from runc")
		}
		break
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		return errors.Wrap(err, "read invalid pid from pidfile")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		// error only possible on non-unix hosts
		return errors.Wrapf(err, "failed to find process for pid %d from pidfile", pid)
	}
	defer process.Release()
	return process.Signal(syscall.SIGKILL)
}

// procHandle is to track the process so we can send signals to it
// and handle graceful shutdown.
type procHandle struct {
	// this is for the runc process (not the process in-container)
	monitorProcess *os.Process
	ready          chan struct{}
	ended          chan struct{}
	shutdown       func(error)
	// this only used when the request context is canceled and we need
	// to kill the in-container process.
	killer procKiller
}

// runcProcessHandle will create a procHandle that will be monitored, where
// on ctx.Done the in-container process will receive a SIGKILL.  The returned
// context should be used for the go-runc.(Run|Exec) invocations.  The returned
// context will only be canceled in the case where the request context is
// canceled and we are unable to send the SIGKILL to the in-container process.
// The goal is to allow for runc to gracefully shutdown when the request context
// is cancelled.
func runcProcessHandle(ctx context.Context, killer procKiller) (*procHandle, context.Context) {
	runcCtx, cancel := context.WithCancelCause(context.Background())
	p := &procHandle{
		ready:    make(chan struct{}),
		ended:    make(chan struct{}),
		shutdown: cancel,
		killer:   killer,
	}
	// preserve the logger on the context used for the runc process handling
	runcCtx = bklog.WithLogger(runcCtx, bklog.G(ctx))

	go func() {
		// Wait for pid
		select {
		case <-p.ended:
			return // nothing to kill
		case <-p.ready:
			select {
			case <-p.ended:
				return
			default:
			}
		}

		select {
		case <-ctx.Done():
			if err := doKillProc(ctx, p); err != nil {
				cancel(err)
			}
		case <-p.ended:
		}
	}()

	return p, runcCtx
}

// Release will free resources with a procHandle.
func (p *procHandle) Release() {
	close(p.ended)
	if p.monitorProcess != nil {
		p.monitorProcess.Release()
	}
}

// Shutdown should be called after the runc process has exited. This will allow
// the signal handling and tty resize loops to exit, terminating the
// goroutines.
func (p *procHandle) Shutdown() {
	if p.shutdown != nil {
		p.shutdown(errors.WithStack(context.Canceled))
	}
}

// WaitForReady will wait until we have received the runc pid via the go-runc
// Started channel, or until the request context is canceled.  This should
// return without errors before attempting to send signals to the runc process.
func (p *procHandle) WaitForReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-p.ready:
		return nil
	}
}

// WaitForStart will record the runc pid reported by go-runc via the channel.
// We wait for up to 10s for the runc pid to be reported.  If the started
// callback is non-nil it will be called after receiving the pid.
func (p *procHandle) WaitForStart(ctx context.Context, startedCh <-chan int, started func()) error {
	ctx, cancel := context.WithCancelCause(ctx)
	ctx, _ = context.WithTimeoutCause(ctx, runcStartTimeout, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
	defer func() { cancel(errors.WithStack(context.Canceled)) }()
	select {
	case <-ctx.Done():
		return errors.New("go-runc started message never received")
	case runcPid, ok := <-startedCh:
		if !ok {
			return errors.New("go-runc failed to send pid")
		}
		if started != nil {
			started()
		}
		var err error
		p.monitorProcess, err = os.FindProcess(runcPid)
		if err != nil {
			// error only possible on non-unix hosts
			return errors.Wrapf(err, "failed to find runc process %d", runcPid)
		}
		close(p.ready)
	}
	return nil
}

// handleSignals will wait until the procHandle is ready then will
// send each signal received on the channel to the runc process (not directly
// to the in-container process)
func handleSignals(ctx context.Context, runcProcess *procHandle, signals <-chan syscall.Signal) error {
	if signals == nil {
		return nil
	}
	err := runcProcess.WaitForReady(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-signals:
			if sig == syscall.SIGKILL {
				// never send SIGKILL directly to runc, it needs to go to the
				// process in-container
				if err := runcProcess.killer.Kill(ctx); err != nil {
					return err
				}
				continue
			}
			if err := runcProcess.monitorProcess.Signal(sig); err != nil {
				bklog.G(ctx).Errorf("failed to signal %s to process: %s", sig, err)
				return err
			}
		}
	}
}

const killWaitDelay = 50 * time.Millisecond

func doKillProc(ctx context.Context, p *procHandle) error {
	// Attempt to kill the process normally.
	for {
		didSucceed := true

		killCtx, timeout := context.WithTimeoutCause(context.WithoutCancel(ctx), 7*time.Second, errors.WithStack(context.DeadlineExceeded))
		if err := p.killer.Kill(killCtx); err != nil {
			if contextErr := context.Cause(ctx); contextErr != nil {
				timeout()
				return contextErr
			}
			didSucceed = false
		}
		timeout()

		if didSucceed {
			// The kill was successful so exit this for loop.
			break
		}

		// The kill did not succeed and the context wasn't canceled.
		// Either wait for the ended signal or try again in 50 milliseconds.
		select {
		case <-p.ended:
			return nil
		case <-time.After(killWaitDelay):
		}
	}

	// Wait for the process to end. Track how long it takes.
	start := time.Now()
	select {
	case <-p.ended:
		return nil
	case <-time.After(killWaitDelay):
		bklog.G(ctx).Warnf("container id %s is taking a long time to exit after kill", p.killer.id)
	}

	// We have warned about the slow exit. Just wait for it to exit instead of spamming the logs.
	<-p.ended
	bklog.G(ctx).Warnf("container id %s took %s to exit", p.killer.id, time.Since(start))
	return nil
}

package daemon // import "github.com/docker/docker/testutil/daemon"

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/tailfile"
	"github.com/docker/docker/testutil/request"
	"github.com/docker/go-connections/sockets"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/pkg/errors"
	"gotest.tools/v3/assert"
)

// LogT is the subset of the testing.TB interface used by the daemon.
type LogT interface {
	Logf(string, ...interface{})
}

// nopLog is a no-op implementation of LogT that is used in daemons created by
// NewDaemon (where no testing.TB is available).
type nopLog struct{}

func (nopLog) Logf(string, ...interface{}) {}

const (
	defaultDockerdBinary         = "dockerd"
	defaultContainerdSocket      = "/var/run/docker/containerd/containerd.sock"
	defaultDockerdRootlessBinary = "dockerd-rootless.sh"
)

var errDaemonNotStarted = errors.New("daemon not started")

// SockRoot holds the path of the default docker integration daemon socket
var SockRoot = filepath.Join(os.TempDir(), "docker-integration")

type clientConfig struct {
	transport *http.Transport
	scheme    string
	addr      string
}

// Daemon represents a Docker daemon for the testing framework
type Daemon struct {
	Root              string
	Folder            string
	Wait              chan error
	UseDefaultHost    bool
	UseDefaultTLSHost bool

	id                         string
	logFile                    *os.File
	cmd                        *exec.Cmd
	storageDriver              string
	userlandProxy              bool
	defaultCgroupNamespaceMode string
	execRoot                   string
	experimental               bool
	init                       bool
	dockerdBinary              string
	log                        LogT
	pidFile                    string
	args                       []string
	containerdSocket           string
	rootlessUser               *user.User
	rootlessXDGRuntimeDir      string

	// swarm related field
	swarmListenAddr string
	SwarmPort       int // FIXME(vdemeester) should probably not be exported
	DefaultAddrPool []string
	SubnetSize      uint32
	DataPathPort    uint32
	OOMScoreAdjust  int
	// cached information
	CachedInfo types.Info
}

// NewDaemon returns a Daemon instance to be used for testing.
// The daemon will not automatically start.
// The daemon will modify and create files under workingDir.
func NewDaemon(workingDir string, ops ...Option) (*Daemon, error) {
	storageDriver := os.Getenv("DOCKER_GRAPHDRIVER")

	if err := os.MkdirAll(SockRoot, 0700); err != nil {
		return nil, errors.Wrapf(err, "failed to create daemon socket root %q", SockRoot)
	}

	id := fmt.Sprintf("d%s", stringid.TruncateID(stringid.GenerateRandomID()))
	dir := filepath.Join(workingDir, id)
	daemonFolder, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	daemonRoot := filepath.Join(daemonFolder, "root")
	if err := os.MkdirAll(daemonRoot, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create daemon root %q", daemonRoot)
	}

	userlandProxy := true
	if env := os.Getenv("DOCKER_USERLANDPROXY"); env != "" {
		if val, err := strconv.ParseBool(env); err != nil {
			userlandProxy = val
		}
	}
	d := &Daemon{
		id:            id,
		Folder:        daemonFolder,
		Root:          daemonRoot,
		storageDriver: storageDriver,
		userlandProxy: userlandProxy,
		// dxr stands for docker-execroot (shortened for avoiding unix(7) path length limitation)
		execRoot:         filepath.Join(os.TempDir(), "dxr", id),
		dockerdBinary:    defaultDockerdBinary,
		swarmListenAddr:  defaultSwarmListenAddr,
		SwarmPort:        DefaultSwarmPort,
		log:              nopLog{},
		containerdSocket: defaultContainerdSocket,
	}

	for _, op := range ops {
		op(d)
	}

	if d.rootlessUser != nil {
		if err := os.Chmod(SockRoot, 0777); err != nil {
			return nil, err
		}
		uid, err := strconv.Atoi(d.rootlessUser.Uid)
		if err != nil {
			return nil, err
		}
		gid, err := strconv.Atoi(d.rootlessUser.Gid)
		if err != nil {
			return nil, err
		}
		if err := os.Chown(d.Folder, uid, gid); err != nil {
			return nil, err
		}
		if err := os.Chown(d.Root, uid, gid); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(d.execRoot), 0700); err != nil {
			return nil, err
		}
		if err := os.Chown(filepath.Dir(d.execRoot), uid, gid); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(d.execRoot, 0700); err != nil {
			return nil, err
		}
		if err := os.Chown(d.execRoot, uid, gid); err != nil {
			return nil, err
		}
		d.rootlessXDGRuntimeDir = filepath.Join(d.Folder, "xdgrun")
		if err := os.MkdirAll(d.rootlessXDGRuntimeDir, 0700); err != nil {
			return nil, err
		}
		if err := os.Chown(d.rootlessXDGRuntimeDir, uid, gid); err != nil {
			return nil, err
		}
		d.containerdSocket = ""
	}

	return d, nil
}

// New returns a Daemon instance to be used for testing.
// This will create a directory such as d123456789 in the folder specified by
// $DOCKER_INTEGRATION_DAEMON_DEST or $DEST.
// The daemon will not automatically start.
func New(t testing.TB, ops ...Option) *Daemon {
	t.Helper()
	dest := os.Getenv("DOCKER_INTEGRATION_DAEMON_DEST")
	if dest == "" {
		dest = os.Getenv("DEST")
	}
	dest = filepath.Join(dest, t.Name())

	assert.Check(t, dest != "", "Please set the DOCKER_INTEGRATION_DAEMON_DEST or the DEST environment variable")

	if os.Getenv("DOCKER_ROOTLESS") != "" {
		if os.Getenv("DOCKER_REMAP_ROOT") != "" {
			t.Skip("DOCKER_ROOTLESS doesn't support DOCKER_REMAP_ROOT currently")
		}
		if env := os.Getenv("DOCKER_USERLANDPROXY"); env != "" {
			if val, err := strconv.ParseBool(env); err == nil && !val {
				t.Skip("DOCKER_ROOTLESS doesn't support DOCKER_USERLANDPROXY=false")
			}
		}
		ops = append(ops, WithRootlessUser("unprivilegeduser"))
	}
	ops = append(ops, WithOOMScoreAdjust(-500))

	d, err := NewDaemon(dest, ops...)
	assert.NilError(t, err, "could not create daemon at %q", dest)
	if d.rootlessUser != nil && d.dockerdBinary != defaultDockerdBinary {
		t.Skipf("DOCKER_ROOTLESS doesn't support specifying non-default dockerd binary path %q", d.dockerdBinary)
	}

	return d
}

// BinaryPath returns the binary and its arguments.
func (d *Daemon) BinaryPath() (string, error) {
	dockerdBinary, err := exec.LookPath(d.dockerdBinary)
	if err != nil {
		return "", errors.Wrapf(err, "[%s] could not find docker binary in $PATH", d.id)
	}
	return dockerdBinary, nil
}

// ContainersNamespace returns the containerd namespace used for containers.
func (d *Daemon) ContainersNamespace() string {
	return d.id
}

// RootDir returns the root directory of the daemon.
func (d *Daemon) RootDir() string {
	return d.Root
}

// ID returns the generated id of the daemon
func (d *Daemon) ID() string {
	return d.id
}

// StorageDriver returns the configured storage driver of the daemon
func (d *Daemon) StorageDriver() string {
	return d.storageDriver
}

// Sock returns the socket path of the daemon
func (d *Daemon) Sock() string {
	return fmt.Sprintf("unix://" + d.sockPath())
}

func (d *Daemon) sockPath() string {
	return filepath.Join(SockRoot, d.id+".sock")
}

// LogFileName returns the path the daemon's log file
func (d *Daemon) LogFileName() string {
	return d.logFile.Name()
}

// ReadLogFile returns the content of the daemon log file
func (d *Daemon) ReadLogFile() ([]byte, error) {
	return ioutil.ReadFile(d.logFile.Name())
}

// NewClientT creates new client based on daemon's socket path
func (d *Daemon) NewClientT(t testing.TB, extraOpts ...client.Opt) *client.Client {
	t.Helper()

	c, err := d.NewClient(extraOpts...)
	assert.NilError(t, err, "[%s] could not create daemon client", d.id)
	return c
}

// NewClient creates new client based on daemon's socket path
func (d *Daemon) NewClient(extraOpts ...client.Opt) (*client.Client, error) {
	clientOpts := []client.Opt{
		client.FromEnv,
		client.WithHost(d.Sock()),
	}
	clientOpts = append(clientOpts, extraOpts...)

	return client.NewClientWithOpts(clientOpts...)
}

// Cleanup cleans the daemon files : exec root (network namespaces, ...), swarmkit files
func (d *Daemon) Cleanup(t testing.TB) {
	t.Helper()
	cleanupMount(t, d)
	cleanupRaftDir(t, d)
	cleanupNetworkNamespace(t, d)
}

// TailLogsT attempts to tail N lines from the daemon logs.
// If there is an error the error is only logged, it does not cause an error with the test.
func (d *Daemon) TailLogsT(t LogT, n int) {
	lines, err := d.TailLogs(n)
	if err != nil {
		t.Logf("[%s] %v", d.id, err)
		return
	}
	for _, l := range lines {
		t.Logf("[%s] %s", d.id, string(l))
	}
}

// TailLogs tails N lines from the daemon logs
func (d *Daemon) TailLogs(n int) ([][]byte, error) {
	logF, err := os.Open(d.logFile.Name())
	if err != nil {
		return nil, errors.Wrap(err, "error opening daemon log file after failed start")
	}

	defer logF.Close()
	lines, err := tailfile.TailFile(logF, n)
	if err != nil {
		return nil, errors.Wrap(err, "error tailing log daemon logs")
	}

	return lines, nil

}

// Start starts the daemon and return once it is ready to receive requests.
func (d *Daemon) Start(t testing.TB, args ...string) {
	t.Helper()
	if err := d.StartWithError(args...); err != nil {
		d.TailLogsT(t, 20)
		d.DumpStackAndQuit() // in case the daemon is stuck
		t.Fatalf("[%s] failed to start daemon with arguments %v : %v", d.id, d.args, err)
	}
}

// StartWithError starts the daemon and return once it is ready to receive requests.
// It returns an error in case it couldn't start.
func (d *Daemon) StartWithError(args ...string) error {
	logFile, err := os.OpenFile(filepath.Join(d.Folder, "docker.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return errors.Wrapf(err, "[%s] failed to create logfile", d.id)
	}

	return d.StartWithLogFile(logFile, args...)
}

// StartWithLogFile will start the daemon and attach its streams to a given file.
func (d *Daemon) StartWithLogFile(out *os.File, providedArgs ...string) error {
	d.handleUserns()
	dockerdBinary, err := d.BinaryPath()
	if err != nil {
		return err
	}

	if d.pidFile == "" {
		d.pidFile = filepath.Join(d.Folder, "docker.pid")
	}

	d.args = []string{}
	if d.rootlessUser != nil {
		if d.dockerdBinary != defaultDockerdBinary {
			return errors.Errorf("[%s] DOCKER_ROOTLESS doesn't support non-default dockerd binary path %q", d.id, d.dockerdBinary)
		}
		dockerdBinary = "sudo"
		d.args = append(d.args,
			"-u", d.rootlessUser.Username,
			"-E", "XDG_RUNTIME_DIR="+d.rootlessXDGRuntimeDir,
			"-E", "HOME="+d.rootlessUser.HomeDir,
			"-E", "PATH="+os.Getenv("PATH"),
			"--",
			defaultDockerdRootlessBinary,
		)
	}

	d.args = append(d.args,
		"--data-root", d.Root,
		"--exec-root", d.execRoot,
		"--pidfile", d.pidFile,
		fmt.Sprintf("--userland-proxy=%t", d.userlandProxy),
		"--containerd-namespace", d.id,
		"--containerd-plugins-namespace", d.id+"p",
	)
	if d.containerdSocket != "" {
		d.args = append(d.args, "--containerd", d.containerdSocket)
	}

	if d.defaultCgroupNamespaceMode != "" {
		d.args = append(d.args, "--default-cgroupns-mode", d.defaultCgroupNamespaceMode)
	}
	if d.experimental {
		d.args = append(d.args, "--experimental")
	}
	if d.init {
		d.args = append(d.args, "--init")
	}
	if !(d.UseDefaultHost || d.UseDefaultTLSHost) {
		d.args = append(d.args, "--host", d.Sock())
	}
	if root := os.Getenv("DOCKER_REMAP_ROOT"); root != "" {
		d.args = append(d.args, "--userns-remap", root)
	}

	// If we don't explicitly set the log-level or debug flag(-D) then
	// turn on debug mode
	foundLog := false
	foundSd := false
	for _, a := range providedArgs {
		if strings.Contains(a, "--log-level") || strings.Contains(a, "-D") || strings.Contains(a, "--debug") {
			foundLog = true
		}
		if strings.Contains(a, "--storage-driver") {
			foundSd = true
		}
	}
	if !foundLog {
		d.args = append(d.args, "--debug")
	}
	if d.storageDriver != "" && !foundSd {
		d.args = append(d.args, "--storage-driver", d.storageDriver)
	}

	d.args = append(d.args, providedArgs...)
	d.cmd = exec.Command(dockerdBinary, d.args...)
	d.cmd.Env = append(os.Environ(), "DOCKER_SERVICE_PREFER_OFFLINE_IMAGE=1")
	d.cmd.Stdout = out
	d.cmd.Stderr = out
	d.logFile = out
	if d.rootlessUser != nil {
		// sudo requires this for propagating signals
		setsid(d.cmd)
	}

	if err := d.cmd.Start(); err != nil {
		return errors.Wrapf(err, "[%s] could not start daemon container", d.id)
	}

	wait := make(chan error, 1)

	go func() {
		ret := d.cmd.Wait()
		d.log.Logf("[%s] exiting daemon", d.id)
		// If we send before logging, we might accidentally log _after_ the test is done.
		// As of Go 1.12, this incurs a panic instead of silently being dropped.
		wait <- ret
		close(wait)
	}()

	d.Wait = wait

	clientConfig, err := d.getClientConfig()
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: clientConfig.transport,
	}

	req, err := http.NewRequest(http.MethodGet, "/_ping", nil)
	if err != nil {
		return errors.Wrapf(err, "[%s] could not create new request", d.id)
	}
	req.URL.Host = clientConfig.addr
	req.URL.Scheme = clientConfig.scheme

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// make sure daemon is ready to receive requests
	for i := 0; ; i++ {
		d.log.Logf("[%s] waiting for daemon to start", d.id)

		select {
		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), "[%s] daemon exited and never started", d.id)
		case err := <-d.Wait:
			return errors.Wrapf(err, "[%s] daemon exited during startup", d.id)
		default:
			rctx, rcancel := context.WithTimeout(context.TODO(), 2*time.Second)
			defer rcancel()

			resp, err := client.Do(req.WithContext(rctx))
			if err != nil {
				if i > 2 { // don't log the first couple, this ends up just being noise
					d.log.Logf("[%s] error pinging daemon on start: %v", d.id, err)
				}

				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}

			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				d.log.Logf("[%s] received status != 200 OK: %s\n", d.id, resp.Status)
			}
			d.log.Logf("[%s] daemon started\n", d.id)
			d.Root, err = d.queryRootDir()
			if err != nil {
				return errors.Wrapf(err, "[%s] error querying daemon for root directory", d.id)
			}
			return nil
		}
	}
}

// StartWithBusybox will first start the daemon with Daemon.Start()
// then save the busybox image from the main daemon and load it into this Daemon instance.
func (d *Daemon) StartWithBusybox(t testing.TB, arg ...string) {
	t.Helper()
	d.Start(t, arg...)
	d.LoadBusybox(t)
}

// Kill will send a SIGKILL to the daemon
func (d *Daemon) Kill() error {
	if d.cmd == nil || d.Wait == nil {
		return errDaemonNotStarted
	}

	defer func() {
		d.logFile.Close()
		d.cmd = nil
	}()

	if err := d.cmd.Process.Kill(); err != nil {
		return err
	}

	if d.pidFile != "" {
		_ = os.Remove(d.pidFile)
	}
	return nil
}

// Pid returns the pid of the daemon
func (d *Daemon) Pid() int {
	return d.cmd.Process.Pid
}

// Interrupt stops the daemon by sending it an Interrupt signal
func (d *Daemon) Interrupt() error {
	return d.Signal(os.Interrupt)
}

// Signal sends the specified signal to the daemon if running
func (d *Daemon) Signal(signal os.Signal) error {
	if d.cmd == nil || d.Wait == nil {
		return errDaemonNotStarted
	}
	return d.cmd.Process.Signal(signal)
}

// DumpStackAndQuit sends SIGQUIT to the daemon, which triggers it to dump its
// stack to its log file and exit
// This is used primarily for gathering debug information on test timeout
func (d *Daemon) DumpStackAndQuit() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	SignalDaemonDump(d.cmd.Process.Pid)
}

// Stop will send a SIGINT every second and wait for the daemon to stop.
// If it times out, a SIGKILL is sent.
// Stop will not delete the daemon directory. If a purged daemon is needed,
// instantiate a new one with NewDaemon.
// If an error occurs while starting the daemon, the test will fail.
func (d *Daemon) Stop(t testing.TB) {
	t.Helper()
	err := d.StopWithError()
	if err != nil {
		if err != errDaemonNotStarted {
			t.Fatalf("[%s] error while stopping the daemon: %v", d.id, err)
		} else {
			t.Logf("[%s] daemon is not started", d.id)
		}
	}
}

// StopWithError will send a SIGINT every second and wait for the daemon to stop.
// If it timeouts, a SIGKILL is sent.
// Stop will not delete the daemon directory. If a purged daemon is needed,
// instantiate a new one with NewDaemon.
func (d *Daemon) StopWithError() (err error) {
	if d.cmd == nil || d.Wait == nil {
		return errDaemonNotStarted
	}
	defer func() {
		if err != nil {
			d.log.Logf("[%s] error while stopping daemon: %v", d.id, err)
		} else {
			d.log.Logf("[%s] daemon stopped", d.id)
			if d.pidFile != "" {
				_ = os.Remove(d.pidFile)
			}
		}
		if err := d.logFile.Close(); err != nil {
			d.log.Logf("[%s] failed to close daemon logfile: %v", d.id, err)
		}
		d.cmd = nil
	}()

	i := 1
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	tick := ticker.C

	d.log.Logf("[%s] stopping daemon", d.id)

	if err := d.cmd.Process.Signal(os.Interrupt); err != nil {
		if strings.Contains(err.Error(), "os: process already finished") {
			return errDaemonNotStarted
		}
		return errors.Wrapf(err, "[%s] could not send signal", d.id)
	}

out1:
	for {
		select {
		case err := <-d.Wait:
			return err
		case <-time.After(20 * time.Second):
			// time for stopping jobs and run onShutdown hooks
			d.log.Logf("[%s] daemon stop timed out after 20 seconds", d.id)
			break out1
		}
	}

out2:
	for {
		select {
		case err := <-d.Wait:
			return err
		case <-tick:
			i++
			if i > 5 {
				d.log.Logf("[%s] tried to interrupt daemon for %d times, now try to kill it", d.id, i)
				break out2
			}
			d.log.Logf("[%d] attempt #%d/5: daemon is still running with pid %d", i, d.cmd.Process.Pid)
			if err := d.cmd.Process.Signal(os.Interrupt); err != nil {
				return errors.Wrapf(err, "[%s] attempt #%d/5 could not send signal", d.id, i)
			}
		}
	}

	if err := d.cmd.Process.Kill(); err != nil {
		d.log.Logf("[%s] failed to kill daemon: %v", d.id, err)
		return err
	}

	return nil
}

// Restart will restart the daemon by first stopping it and the starting it.
// If an error occurs while starting the daemon, the test will fail.
func (d *Daemon) Restart(t testing.TB, args ...string) {
	t.Helper()
	d.Stop(t)
	d.Start(t, args...)
}

// RestartWithError will restart the daemon by first stopping it and then starting it.
func (d *Daemon) RestartWithError(arg ...string) error {
	if err := d.StopWithError(); err != nil {
		return err
	}
	return d.StartWithError(arg...)
}

func (d *Daemon) handleUserns() {
	// in the case of tests running a user namespace-enabled daemon, we have resolved
	// d.Root to be the actual final path of the graph dir after the "uid.gid" of
	// remapped root is added--we need to subtract it from the path before calling
	// start or else we will continue making subdirectories rather than truly restarting
	// with the same location/root:
	if root := os.Getenv("DOCKER_REMAP_ROOT"); root != "" {
		d.Root = filepath.Dir(d.Root)
	}
}

// ReloadConfig asks the daemon to reload its configuration
func (d *Daemon) ReloadConfig() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return errors.New("daemon is not running")
	}

	errCh := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		_, body, err := request.Get("/events", request.Host(d.Sock()))
		close(started)
		if err != nil {
			errCh <- err
			return
		}
		defer body.Close()
		dec := json.NewDecoder(body)
		for {
			var e events.Message
			if err := dec.Decode(&e); err != nil {
				errCh <- err
				return
			}
			if e.Type != events.DaemonEventType {
				continue
			}
			if e.Action != "reload" {
				continue
			}
			close(errCh) // notify that we are done
			return
		}
	}()

	<-started
	if err := signalDaemonReload(d.cmd.Process.Pid); err != nil {
		return errors.Wrapf(err, "[%s] error signaling daemon reload", d.id)
	}
	select {
	case err := <-errCh:
		if err != nil {
			return errors.Wrapf(err, "[%s] error waiting for daemon reload event", d.id)
		}
	case <-time.After(30 * time.Second):
		return errors.Errorf("[%s] daemon reload event timed out after 30 seconds", d.id)
	}
	return nil
}

// LoadBusybox image into the daemon
func (d *Daemon) LoadBusybox(t testing.TB) {
	t.Helper()
	clientHost, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(t, err, "[%s] failed to create client", d.id)
	defer clientHost.Close()

	ctx := context.Background()
	reader, err := clientHost.ImageSave(ctx, []string{"busybox:latest"})
	assert.NilError(t, err, "[%s] failed to download busybox", d.id)
	defer reader.Close()

	c := d.NewClientT(t)
	defer c.Close()

	resp, err := c.ImageLoad(ctx, reader, true)
	assert.NilError(t, err, "[%s] failed to load busybox", d.id)
	defer resp.Body.Close()
}

func (d *Daemon) getClientConfig() (*clientConfig, error) {
	var (
		transport *http.Transport
		scheme    string
		addr      string
		proto     string
	)
	if d.UseDefaultTLSHost {
		option := &tlsconfig.Options{
			CAFile:   "fixtures/https/ca.pem",
			CertFile: "fixtures/https/client-cert.pem",
			KeyFile:  "fixtures/https/client-key.pem",
		}
		tlsConfig, err := tlsconfig.Client(*option)
		if err != nil {
			return nil, err
		}
		transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		addr = fmt.Sprintf("%s:%d", opts.DefaultHTTPHost, opts.DefaultTLSHTTPPort)
		scheme = "https"
		proto = "tcp"
	} else if d.UseDefaultHost {
		addr = opts.DefaultUnixSocket
		proto = "unix"
		scheme = "http"
		transport = &http.Transport{}
	} else {
		addr = d.sockPath()
		proto = "unix"
		scheme = "http"
		transport = &http.Transport{}
	}

	if err := sockets.ConfigureTransport(transport, proto, addr); err != nil {
		return nil, err
	}
	transport.DisableKeepAlives = true
	if proto == "unix" {
		addr = filepath.Base(addr)
	}
	return &clientConfig{
		transport: transport,
		scheme:    scheme,
		addr:      addr,
	}, nil
}

func (d *Daemon) queryRootDir() (string, error) {
	// update daemon root by asking /info endpoint (to support user
	// namespaced daemon with root remapped uid.gid directory)
	clientConfig, err := d.getClientConfig()
	if err != nil {
		return "", err
	}

	c := &http.Client{
		Transport: clientConfig.transport,
	}

	req, err := http.NewRequest(http.MethodGet, "/info", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.URL.Host = clientConfig.addr
	req.URL.Scheme = clientConfig.scheme

	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	body := ioutils.NewReadCloserWrapper(resp.Body, func() error {
		return resp.Body.Close()
	})

	type Info struct {
		DockerRootDir string
	}
	var b []byte
	var i Info
	b, err = request.ReadBody(body)
	if err == nil && resp.StatusCode == http.StatusOK {
		// read the docker root dir
		if err = json.Unmarshal(b, &i); err == nil {
			return i.DockerRootDir, nil
		}
	}
	return "", err
}

// Info returns the info struct for this daemon
func (d *Daemon) Info(t testing.TB) types.Info {
	t.Helper()
	c := d.NewClientT(t)
	info, err := c.Info(context.Background())
	assert.NilError(t, err)
	assert.NilError(t, c.Close())
	return info
}

// cleanupRaftDir removes swarmkit wal files if present
func cleanupRaftDir(t testing.TB, d *Daemon) {
	t.Helper()
	for _, p := range []string{"wal", "wal-v3-encrypted", "snap-v3-encrypted"} {
		dir := filepath.Join(d.Root, "swarm/raft", p)
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("[%s] error removing %v: %v", d.id, dir, err)
		}
	}
}

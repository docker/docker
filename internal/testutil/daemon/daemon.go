package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/go-connections/sockets"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/moby/moby/client/pkg/stringid"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/internal/testutil/request"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/moby/moby/v2/pkg/pidfile"
	"github.com/moby/moby/v2/pkg/tailfile"
	"github.com/pkg/errors"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
)

// LogT is the subset of the testing.TB interface used by the daemon.
type LogT interface {
	Logf(string, ...any)
}

// nopLog is a no-op implementation of LogT that is used in daemons created by
// NewDaemon (where no testing.TB is available).
type nopLog struct{}

func (nopLog) Logf(string, ...any) {}

const (
	// DefaultDockerdBinary is the dockerd binary used by test daemons unless
	// [WithDockerdBinary] or [Daemon.UseBinary] is used to select another one.
	DefaultDockerdBinary = "dockerd"

	defaultTLSHost = "localhost:2376"
)

var errDaemonNotStarted = errors.New("daemon not started")

// containerdEmbeddedFromEnv reports whether test daemons should run containerd
// in-process, via the embedded-containerd feature. It is controlled by
// TEST_INTEGRATION_CONTAINERD_EMBEDDED.
func containerdEmbeddedFromEnv() bool {
	return os.Getenv("TEST_INTEGRATION_CONTAINERD_EMBEDDED") != ""
}

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
	extraEnv                   []string
	containerdSocket           string
	containerdEmbedded         bool
	usernsRemap                string
	rootlessUser               *user.User
	rootlessXDGRuntimeDir      string
	resolvConfContent          string
	ResolvConfPathOverride     string // Path to a replacement for "/etc/resolv.conf", or empty.

	// swarm related field
	swarmListenAddr   string
	swarmWithIptables bool
	SwarmPort         int // FIXME(vdemeester) should probably not be exported
	DefaultAddrPool   []netip.Prefix
	SubnetSize        uint32
	DataPathPort      uint32
	OOMScoreAdjust    int
	// cached information
	CachedInfo system.Info
}

// NewDaemon returns a Daemon instance to be used for testing.
// The daemon will not automatically start.
// The daemon will modify and create files under workingDir.
func NewDaemon(workingDir string, ops ...Option) (*Daemon, error) {
	storageDriver := os.Getenv("DOCKER_GRAPHDRIVER")

	if err := os.MkdirAll(SockRoot, 0o700); err != nil {
		return nil, errors.Wrapf(err, "failed to create daemon socket root %q", SockRoot)
	}

	id := "d" + stringid.TruncateID(stringid.GenerateRandomID())
	dir := filepath.Join(workingDir, id)
	daemonFolder, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	daemonRoot := filepath.Join(daemonFolder, "root")
	if err := os.MkdirAll(daemonRoot, 0o755); err != nil { // #nosec G703 -- ignore "path traversal" for test code.
		return nil, errors.Wrapf(err, "failed to create daemon root %q", daemonRoot)
	}

	userlandProxy := true
	if env := os.Getenv("DOCKER_USERLANDPROXY"); env != "" {
		val, err := strconv.ParseBool(env)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse DOCKER_USERLANDPROXY")
		}
		userlandProxy = val
	}
	d := &Daemon{
		id:            id,
		Folder:        daemonFolder,
		Root:          daemonRoot,
		storageDriver: storageDriver,
		userlandProxy: userlandProxy,
		// dxr stands for docker-execroot (shortened for avoiding unix(7) path length limitation)
		execRoot:           filepath.Join(os.TempDir(), "dxr", id),
		dockerdBinary:      DefaultDockerdBinary,
		swarmListenAddr:    defaultSwarmListenAddr,
		SwarmPort:          DefaultSwarmPort,
		log:                nopLog{},
		containerdSocket:   defaultContainerdSocket,
		containerdEmbedded: containerdEmbeddedFromEnv(),
	}

	for _, op := range ops {
		op(d)
	}

	if d.resolvConfContent != "" {
		path := filepath.Join(d.Folder, "resolv.conf")
		if err := os.WriteFile(path, []byte(d.resolvConfContent), 0o644); err != nil { // #nosec G703 -- ignore "path traversal" for test code.
			return nil, fmt.Errorf("failed to write docker resolv.conf to %q: %v", path, err)
		}
		d.extraEnv = append(d.extraEnv, "DOCKER_TEST_RESOLV_CONF_PATH="+path)
		d.ResolvConfPathOverride = path
	}

	if d.rootlessUser != nil {
		if err := os.Chmod(SockRoot, 0o777); err != nil {
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
		if err := os.Chown(d.Folder, uid, gid); err != nil { // #nosec G703 -- ignore "path traversal" for test code.
			return nil, err
		}
		if err := os.Chown(d.Root, uid, gid); err != nil { // #nosec G703 -- ignore "path traversal" for test code.
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(d.execRoot), 0o700); err != nil {
			return nil, err
		}
		if err := os.Chown(filepath.Dir(d.execRoot), uid, gid); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(d.execRoot, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chown(d.execRoot, uid, gid); err != nil {
			return nil, err
		}
		// $XDG_RUNTIME_DIR mustn't be too long, as ${XDG_RUNTIME_DIR/dockerd-rootless
		// contains Unix sockets
		d.rootlessXDGRuntimeDir = filepath.Join(os.TempDir(), "xdgrun-"+id)
		if err := os.MkdirAll(d.rootlessXDGRuntimeDir, 0o700); err != nil {
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
	assert.Assert(t, dest != "", "Please set the DOCKER_INTEGRATION_DAEMON_DEST or the DEST environment variable")
	dest = filepath.Join(dest, sanitizedTestName(t))

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
	if d.rootlessUser != nil && d.dockerdBinary != DefaultDockerdBinary {
		t.Skipf("DOCKER_ROOTLESS doesn't support specifying non-default dockerd binary path %q", d.dockerdBinary)
	}

	return d
}

// sanitizedTestName removes special characters (like double or single quotes)
// from name to prevent creating filesystem paths that can cause
// errors when parsing through scripts.
//
// Test-names (t.Name()) for sub-tests allow free-form names to be used,
// for example:
//
//	t.Run("engine restart shouldn't kill alive containers", func(t *testing.T) {
//
// Which would result in the quote (') to be included in the filename,
// which is valid, but can cause issues when processing with shell scripts;
//
//	find bundles -path '*/root/*overlay2' -prune -o -type f \( -name '*-report.json' -o -name '*.log' -o -name '*.out' -o -name '*.prof' -o -name '*-report.xml' \) -print | xargs sudo tar -czf /tmp/reports.tar.gz
//	[...]
//	xargs: unmatched single quote; by default quotes are special to xargs unless you use the -0 option
//
// sanitizedTestName replaces special characters with an underscore to prevent
// such issues.
func sanitizedTestName(t testing.TB) string {
	t.Helper()
	parts := strings.Split(t.Name(), "/")
	for i := range parts {
		parts[i] = sanitizePathComponent(parts[i])
	}
	return filepath.Join(parts...)
}

func sanitizePathComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	out := strings.TrimLeft(strings.TrimSpace(b.String()), "-")
	if out == "" {
		return "_"
	}
	return out
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
	return "unix://" + d.sockPath()
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
	_ = d.logFile.Sync()
	return os.ReadFile(d.logFile.Name())
}

// NewClientT creates new client based on daemon's socket path
func (d *Daemon) NewClientT(t testing.TB, extraOpts ...client.Opt) *client.Client {
	t.Helper()

	c, err := d.NewClient(extraOpts...)
	assert.NilError(t, err, "[%s] could not create daemon client", d.id)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// NewClient creates new client based on daemon's socket path
func (d *Daemon) NewClient(extraOpts ...client.Opt) (*client.Client, error) {
	clientOpts := []client.Opt{
		client.FromEnv,
		client.WithHost(d.Sock()),
	}
	clientOpts = append(clientOpts, extraOpts...)

	return client.New(clientOpts...)
}

// Cleanup cleans the daemon files : exec root (network namespaces, ...), swarmkit files
func (d *Daemon) Cleanup(t testing.TB) {
	t.Helper()
	cleanupMount(t, d)
	cleanupRaftDir(t, d)
	cleanupDaemonStorage(t, d)
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

// PollCheckLogs is a poll.Check that checks the daemon logs using the passed in match function.
func (d *Daemon) PollCheckLogs(ctx context.Context, match func(s string) bool) poll.Check {
	return func(t poll.LogT) poll.Result {
		ok, _, err := d.ScanLogs(ctx, match)
		if err != nil {
			return poll.Error(err)
		}
		if !ok {
			return poll.Continue("waiting for daemon logs match")
		}
		return poll.Success()
	}
}

// ScanLogsMatchString returns a function that can be used to scan the daemon logs for the passed in string (`contains`).
func ScanLogsMatchString(contains string) func(string) bool {
	return func(line string) bool {
		return strings.Contains(line, contains)
	}
}

// ScanLogsMatchCount returns a function that can be used to scan the daemon logs until the passed in matcher function matches `count` times
func ScanLogsMatchCount(f func(string) bool, count int) func(string) bool {
	matched := 0
	return func(line string) bool {
		if f(line) {
			matched++
		}
		return matched == count
	}
}

// ScanLogsMatchAll returns a function that can be used to scan the daemon logs until *all* the passed in strings are matched
func ScanLogsMatchAll(contains ...string) func(string) bool {
	matched := make(map[string]bool)
	return func(line string) bool {
		for _, c := range contains {
			if strings.Contains(line, c) {
				matched[c] = true
			}
		}
		return len(matched) == len(contains)
	}
}

// ScanLogsT uses `ScanLogs` to match the daemon logs using the passed in match function.
// If there is an error or the match fails, the test will fail.
func (d *Daemon) ScanLogsT(ctx context.Context, t testing.TB, match func(s string) bool) (bool, string) {
	t.Helper()
	ok, line, err := d.ScanLogs(ctx, match)
	assert.NilError(t, err)
	return ok, line
}

// ScanLogs scans the daemon logs and passes each line to the match function.
func (d *Daemon) ScanLogs(ctx context.Context, match func(s string) bool) (bool, string, error) {
	stat, err := d.logFile.Stat()
	if err != nil {
		return false, "", err
	}
	rdr := io.NewSectionReader(d.logFile, 0, stat.Size())

	scanner := bufio.NewScanner(rdr)
	for scanner.Scan() {
		if match(scanner.Text()) {
			return true, scanner.Text(), nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		default:
		}
	}
	return false, "", scanner.Err()
}

// TailLogs tails N lines from the daemon logs
func (d *Daemon) TailLogs(n int) ([][]byte, error) {
	if d.logFile == nil {
		return nil, errors.New("d.logFile is nil")
	}
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
	logFile, err := os.OpenFile(filepath.Join(d.Folder, "docker.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
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

	dockerdBinary, args, err := d.rootlessCommand(dockerdBinary)
	if err != nil {
		return err
	}
	d.args = args

	d.args = append(d.args,
		// Make sure we don't use the environment-provided global config file.
		"--config-file", os.DevNull,
		"--data-root", d.Root,
		"--exec-root", d.execRoot,
		"--pidfile", d.pidFile,
		"--containerd-namespace", d.id,
		"--containerd-plugins-namespace", d.id+"p",
	)
	d.args = append(d.args, d.platformArgs()...)

	if d.containerdEmbedded {
		d.args = append(d.args, "--feature", "embedded-containerd")
	} else if d.containerdSocket != "" {
		d.args = append(d.args, "--containerd", d.containerdSocket)
	}

	if d.usernsRemap != "" {
		d.args = append(d.args, "--userns-remap", d.usernsRemap)
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
	if !d.UseDefaultHost && !d.UseDefaultTLSHost {
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

	hasFwBackendArg := slices.ContainsFunc(providedArgs, func(s string) bool {
		return strings.HasPrefix(s, "--firewall-backend")
	})
	if !hasFwBackendArg {
		if fw := os.Getenv("DOCKER_FIREWALL_BACKEND"); fw != "" {
			d.args = append(d.args, "--firewall-backend="+fw)
		}
	}

	d.args = append(d.args, providedArgs...)
	cmd := exec.Command(dockerdBinary, d.args...)
	cmd.Env = append(os.Environ(), "DOCKER_SERVICE_PREFER_OFFLINE_IMAGE=1")
	cmd.Env = append(cmd.Env, d.extraEnv...)
	cmd.Env = append(cmd.Env, "OTEL_SERVICE_NAME=dockerd-"+d.id)
	cmd.Stdout = out
	cmd.Stderr = out
	d.logFile = out
	if d.rootlessUser != nil {
		// sudo requires this for propagating signals
		setsid(cmd)
	}

	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "[%s] could not start daemon container", d.id)
	}

	wait := make(chan error, 1)
	d.cmd = cmd
	d.Wait = wait

	go func() {
		ret := cmd.Wait()
		d.log.Logf("[%s] exiting daemon", d.id)
		// If we send before logging, we might accidentally log _after_ the test is done.
		// As of Go 1.12, this incurs a panic instead of silently being dropped.
		wait <- ret
		close(wait)
	}()

	clientCfg, err := d.getClientConfig()
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Transport: clientCfg.transport,
	}

	req, err := http.NewRequest(http.MethodGet, "/_ping", http.NoBody)
	if err != nil {
		return errors.Wrapf(err, "[%s] could not create new request", d.id)
	}
	req.URL.Host = clientCfg.addr
	req.URL.Scheme = clientCfg.scheme

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

			resp, err := httpClient.Do(req.WithContext(rctx))
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

			_ = resp.Body.Close()
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
func (d *Daemon) StartWithBusybox(ctx context.Context, t testing.TB, arg ...string) {
	t.Helper()
	d.Start(t, arg...)
	d.LoadBusybox(ctx, t)
}

// Kill will send a SIGKILL to the daemon
func (d *Daemon) Kill() error {
	if d.cmd == nil || d.Wait == nil {
		return errDaemonNotStarted
	}

	defer func() {
		_ = d.logFile.Close()
		d.cmd = nil
	}()

	if err := d.cmd.Process.Kill(); err != nil {
		return err
	}

	_, err := d.cmd.Process.Wait()
	if err != nil && !errors.Is(err, syscall.ECHILD) {
		return err
	}

	if d.pidFile != "" {
		_ = os.Remove(d.pidFile)
	}
	return nil
}

// Pid returns the pid of the daemon's direct child process. In rootless
// mode, this is the outermost process started by the test framework (e.g.
// sudo), not dockerd itself.
func (d *Daemon) Pid() int {
	return d.cmd.Process.Pid
}

// DockerdPid returns the pid of the dockerd process itself, as recorded by
// dockerd in its pidfile. Unlike Pid, this is accurate in rootless mode.
func (d *Daemon) DockerdPid(t testing.TB) int {
	t.Helper()

	pid, err := pidfile.Read(d.pidFile)
	assert.NilError(t, err)
	assert.Assert(t, pid != 0, "no running process found in %s", d.pidFile)

	return pid
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
	if err != nil && !errors.Is(err, errDaemonNotStarted) {
		t.Fatalf("[%s] error while stopping the daemon: %v", d.id, err)
	}
}

// StopWithError will send a SIGINT every second and wait for the daemon to stop.
// If it timeouts, a SIGKILL is sent.
// Stop will not delete the daemon directory. If a purged daemon is needed,
// instantiate a new one with NewDaemon.
func (d *Daemon) StopWithError() (retErr error) {
	if d.cmd == nil || d.Wait == nil {
		return errDaemonNotStarted
	}
	defer func() {
		if retErr != nil {
			d.log.Logf("[%s] error while stopping daemon: %v", d.id, retErr)
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
		if errors.Is(err, os.ErrProcessDone) {
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
			d.log.Logf("[%s] attempt #%d/5: daemon is still running with pid %d", d.id, i, d.cmd.Process.Pid)
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
		_, body, err := request.Get(context.TODO(), "/events", request.Host(d.Sock()))
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
			if e.Action != events.ActionReload {
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

// SetEnvVar updates the set of extra env variables for the daemon, to take
// effect on the next start/restart.
func (d *Daemon) SetEnvVar(name, val string) {
	prefix := name + "="
	if idx := slices.IndexFunc(d.extraEnv, func(ev string) bool { return strings.HasPrefix(ev, prefix) }); idx > 0 {
		d.extraEnv[idx] = prefix + val
		return
	}
	d.extraEnv = append(d.extraEnv, prefix+val)
}

// UseBinary sets the dockerd binary to start the daemon with. The daemon must
// be stopped when calling it. It allows a test to restart a daemon's state
// (data-root, exec-root and containerd namespaces are unchanged) with another
// version of dockerd, for example to verify that state written by a released
// daemon can be used by the daemon under test.
func (d *Daemon) UseBinary(t testing.TB, dockerdBinary string) {
	t.Helper()
	assert.Assert(t, d.cmd == nil, "daemon must be stopped before changing the dockerd binary")
	d.dockerdBinary = dockerdBinary
}

// LoadBusybox image into the daemon
func (d *Daemon) LoadBusybox(ctx context.Context, t testing.TB) {
	d.LoadImage(ctx, t, "busybox:latest")
}

func (d *Daemon) LoadImage(ctx context.Context, t testing.TB, img string) {
	t.Helper()
	clientHost, err := client.New(client.FromEnv)
	assert.NilError(t, err, "[%s] failed to create client", d.id)
	defer clientHost.Close()

	reader, err := clientHost.ImageSave(ctx, []string{img})
	assert.NilError(t, err, "[%s] failed to download %s", d.id, img)
	defer func() { _ = reader.Close() }()

	c := d.NewClientT(t)
	defer c.Close()

	resp, err := c.ImageLoad(ctx, reader, client.ImageLoadWithQuiet(true))
	assert.NilError(t, err, "[%s] failed to load %s", d.id, img)
	defer func() { _ = resp.Close() }()

	assert.NilError(t, jsonmessage.DisplayStream(resp, io.Discard), "[%s] failed to read load response for %s", d.id, img)
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
		addr = defaultTLSHost
		scheme = "https"
		proto = "tcp"
	} else if d.UseDefaultHost {
		transport, scheme, proto, addr = defaultHostConfig()
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

	req, err := http.NewRequest(http.MethodGet, "/info", http.NoBody)
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
func (d *Daemon) Info(t testing.TB) system.Info {
	t.Helper()
	c := d.NewClientT(t)
	result, err := c.Info(t.Context(), client.InfoOptions{})
	assert.NilError(t, err)
	info := result.Info
	assert.NilError(t, c.Close())
	return info
}

func (d *Daemon) FirewallBackendDriver(t testing.TB) string {
	t.Helper()
	info := d.Info(t)
	assert.Assert(t, info.FirewallBackend != nil, "no firewall backend reported")
	return info.FirewallBackend.Driver
}

// FirewallReloadedAt fetches the daemon's Info and, if it contains a firewall
// reload time, returns that time.
func (d *Daemon) FirewallReloadedAt(t testing.TB) string {
	t.Helper()
	info := d.Info(t)
	if info.FirewallBackend == nil {
		return ""
	}
	for _, kv := range info.FirewallBackend.Info {
		if kv[0] == "ReloadedAt" {
			return kv[1]
		}
	}
	return ""
}

// TamperWithContainerConfig modifies the on-disk config of a container.
func (d *Daemon) TamperWithContainerConfig(t testing.TB, containerID string, tamper func(*container.Container)) {
	t.Helper()

	configPath := filepath.Join(d.Root, "containers", containerID, "config.v2.json")
	configBytes, err := os.ReadFile(configPath)
	assert.NilError(t, err)

	hostConfigPath := filepath.Join(d.Root, "containers", containerID, "hostconfig.json")
	hostConfigBytes, err := os.ReadFile(hostConfigPath)
	assert.NilError(t, err)

	var c container.Container
	assert.NilError(t, json.Unmarshal(configBytes, &c))
	assert.NilError(t, json.Unmarshal(hostConfigBytes, &c.HostConfig))

	c.State = &container.State{}
	tamper(&c)

	configBytes, err = json.Marshal(&c)
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(configPath, configBytes, 0o600))

	hostConfigBytes, err = json.Marshal(&c.HostConfig)
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(hostConfigPath, hostConfigBytes, 0o600))
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

// cleanupDaemonStorage removes the daemon's storage directory.
//
// Note that we don't delete the whole directory, as some files (e.g. daemon
// logs) are collected for inclusion in the "bundles" that are stored as Jenkins
// artifacts.
//
// We currently do not include container logs in the bundles, so this also
// removes the "containers" subdirectory.
func cleanupDaemonStorage(t testing.TB, d *Daemon) {
	t.Helper()
	dirs := []string{
		"builder",
		"buildkit",
		"containers",
		"image",
		"network",
		"plugins",
		"tmp",
		"trust",
		"volumes",
		// note: this assumes storage-driver name matches the subdirectory,
		// which is currently true, but not guaranteed.
		d.storageDriver,
	}

	for _, p := range dirs {
		dir := filepath.Join(d.Root, p)
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("[%s] error removing %v: %v", d.id, dir, err)
		}
	}
}

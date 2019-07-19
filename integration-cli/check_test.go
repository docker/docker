package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/docker/integration-cli/environment"
	"github.com/docker/docker/internal/test/suite"
	"github.com/docker/docker/pkg/reexec"
	testdaemon "github.com/docker/docker/testutil/daemon"
	ienv "github.com/docker/docker/testutil/environment"
	"github.com/docker/docker/testutil/fakestorage"
	"github.com/docker/docker/testutil/fixtures/plugin"
	"github.com/docker/docker/testutil/registry"
	"gotest.tools/assert"
)

const (
	// the private registry to use for tests
	privateRegistryURL = registry.DefaultURL

	// path to containerd's ctr binary
	ctrBinary = "ctr"

	// the docker daemon binary to use
	dockerdBinary = "dockerd"
)

var (
	testEnv *environment.Execution

	// the docker client binary to use
	dockerBinary = ""
)

func init() {
	var err error

	reexec.Init() // This is required for external graphdriver tests

	testEnv, err = environment.New()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()

	// Global set up
	dockerBinary = testEnv.DockerBinary()
	err := ienv.EnsureFrozenImagesLinux(&testEnv.Execution)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	testEnv.Print()
	os.Exit(m.Run())
}

func Test(t *testing.T) {
	cli.SetTestEnvironment(testEnv)
	fakestorage.SetTestEnvironment(&testEnv.Execution)
	ienv.ProtectAll(t, &testEnv.Execution)
	suite.Run(t, &DockerSuite{})
	suite.Run(t, &DockerRegistrySuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerSchema1RegistrySuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerRegistryAuthHtpasswdSuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerRegistryAuthTokenSuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerDaemonSuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerSwarmSuite{ds: &DockerSuite{}})
	suite.Run(t, &DockerPluginSuite{ds: &DockerSuite{}})
	if runtime.GOOS != "windows" {
		suite.Run(t, &DockerExternalVolumeSuite{ds: &DockerSuite{}})
		suite.Run(t, &DockerNetworkSuite{ds: &DockerSuite{}})
		// FIXME. Temporarily turning this off for Windows as GH16039 was breaking
		// Windows to Linux CI @icecrime
		suite.Run(t, newDockerHubPullSuite())
	}
}

type DockerSuite struct {
}

func (s *DockerSuite) OnTimeout(c *testing.T) {
	if testEnv.IsRemoteDaemon() {
		return
	}
	path := filepath.Join(os.Getenv("DEST"), "docker.pid")
	b, err := ioutil.ReadFile(path)
	if err != nil {
		c.Fatalf("Failed to get daemon PID from %s\n", path)
	}

	rawPid, err := strconv.ParseInt(string(b), 10, 32)
	if err != nil {
		c.Fatalf("Failed to parse pid from %s: %s\n", path, err)
	}

	daemonPid := int(rawPid)
	if daemonPid > 0 {
		testdaemon.SignalDaemonDump(daemonPid)
	}
}

func (s *DockerSuite) TearDownTest(c *testing.T) {
	testEnv.Clean(c)
}

type DockerRegistrySuite struct {
	ds  *DockerSuite
	reg *registry.V2
	d   *daemon.Daemon
}

func (s *DockerRegistrySuite) OnTimeout(c *testing.T) {
	s.d.DumpStackAndQuit()
}

func (s *DockerRegistrySuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, RegistryHosting, testEnv.IsLocalDaemon)
	s.reg = registry.NewV2(c)
	s.reg.WaitReady(c)
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerRegistrySuite) TearDownTest(c *testing.T) {
	if s.reg != nil {
		s.reg.Close()
	}
	if s.d != nil {
		s.d.Stop(c)
	}
	s.ds.TearDownTest(c)
}

type DockerSchema1RegistrySuite struct {
	ds  *DockerSuite
	reg *registry.V2
	d   *daemon.Daemon
}

func (s *DockerSchema1RegistrySuite) OnTimeout(c *testing.T) {
	s.d.DumpStackAndQuit()
}

func (s *DockerSchema1RegistrySuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, RegistryHosting, NotArm64, testEnv.IsLocalDaemon)
	s.reg = registry.NewV2(c, registry.Schema1)
	s.reg.WaitReady(c)
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerSchema1RegistrySuite) TearDownTest(c *testing.T) {
	if s.reg != nil {
		s.reg.Close()
	}
	if s.d != nil {
		s.d.Stop(c)
	}
	s.ds.TearDownTest(c)
}

type DockerRegistryAuthHtpasswdSuite struct {
	ds  *DockerSuite
	reg *registry.V2
	d   *daemon.Daemon
}

func (s *DockerRegistryAuthHtpasswdSuite) OnTimeout(c *testing.T) {
	s.d.DumpStackAndQuit()
}

func (s *DockerRegistryAuthHtpasswdSuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, RegistryHosting, testEnv.IsLocalDaemon)
	s.reg = registry.NewV2(c, registry.Htpasswd)
	s.reg.WaitReady(c)
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerRegistryAuthHtpasswdSuite) TearDownTest(c *testing.T) {
	if s.reg != nil {
		out, err := s.d.Cmd("logout", privateRegistryURL)
		assert.NilError(c, err, out)
		s.reg.Close()
	}
	if s.d != nil {
		s.d.Stop(c)
	}
	s.ds.TearDownTest(c)
}

type DockerRegistryAuthTokenSuite struct {
	ds  *DockerSuite
	reg *registry.V2
	d   *daemon.Daemon
}

func (s *DockerRegistryAuthTokenSuite) OnTimeout(c *testing.T) {
	s.d.DumpStackAndQuit()
}

func (s *DockerRegistryAuthTokenSuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, RegistryHosting, testEnv.IsLocalDaemon)
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerRegistryAuthTokenSuite) TearDownTest(c *testing.T) {
	if s.reg != nil {
		out, err := s.d.Cmd("logout", privateRegistryURL)
		assert.NilError(c, err, out)
		s.reg.Close()
	}
	if s.d != nil {
		s.d.Stop(c)
	}
	s.ds.TearDownTest(c)
}

func (s *DockerRegistryAuthTokenSuite) setupRegistryWithTokenService(c *testing.T, tokenURL string) {
	if s == nil {
		c.Fatal("registry suite isn't initialized")
	}
	s.reg = registry.NewV2(c, registry.Token(tokenURL))
	s.reg.WaitReady(c)
}

type DockerDaemonSuite struct {
	ds *DockerSuite
	d  *daemon.Daemon
}

func (s *DockerDaemonSuite) OnTimeout(c *testing.T) {
	s.d.DumpStackAndQuit()
}

func (s *DockerDaemonSuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerDaemonSuite) TearDownTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	if s.d != nil {
		s.d.Stop(c)
	}
	s.ds.TearDownTest(c)

	// Some tests in this Suite make modifications to the "docker0" bridge.
	// Remove the bridge afterwards to force the next test to re-create it
	deleteInterface(c, "docker0")
}

func (s *DockerDaemonSuite) TearDownSuite(c *testing.T) {
	filepath.Walk(testdaemon.SockRoot, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			// ignore errors here
			// not cleaning up sockets is not really an error
			return nil
		}
		if fi.Mode() == os.ModeSocket {
			syscall.Unlink(path)
		}
		return nil
	})
	os.RemoveAll(testdaemon.SockRoot)
}

const defaultSwarmPort = 2477

type DockerSwarmSuite struct {
	server      *httptest.Server
	ds          *DockerSuite
	daemonsLock sync.Mutex // protect access to daemons and portIndex
	daemons     []*daemon.Daemon
	portIndex   int
}

func (s *DockerSwarmSuite) OnTimeout(c *testing.T) {
	s.daemonsLock.Lock()
	defer s.daemonsLock.Unlock()
	for _, d := range s.daemons {
		d.DumpStackAndQuit()
	}
}

func (s *DockerSwarmSuite) SetUpTest(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
}

func (s *DockerSwarmSuite) AddDaemon(c *testing.T, joinSwarm, manager bool) *daemon.Daemon {
	d := daemon.New(c, dockerBinary, dockerdBinary,
		testdaemon.WithEnvironment(testEnv.Execution),
		testdaemon.WithSwarmPort(defaultSwarmPort+s.portIndex),
	)
	if joinSwarm {
		if len(s.daemons) > 0 {
			d.StartAndSwarmJoin(c, s.daemons[0].Daemon, manager)
		} else {
			d.StartAndSwarmInit(c)
		}
	} else {
		d.StartNodeWithBusybox(c)
	}

	s.daemonsLock.Lock()
	s.portIndex++
	s.daemons = append(s.daemons, d)
	s.daemonsLock.Unlock()

	return d
}

func (s *DockerSwarmSuite) TearDownTest(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	s.daemonsLock.Lock()
	for _, d := range s.daemons {
		if d != nil {
			d.Stop(c)
			d.Cleanup(c)
		}
	}
	s.daemons = nil
	s.portIndex = 0
	s.daemonsLock.Unlock()
	s.ds.TearDownTest(c)
}

type DockerPluginSuite struct {
	ds       *DockerSuite
	registry *registry.V2
}

func (ps *DockerPluginSuite) registryHost() string {
	return privateRegistryURL
}

func (ps *DockerPluginSuite) getPluginRepo() string {
	return path.Join(ps.registryHost(), "plugin", "basic")
}
func (ps *DockerPluginSuite) getPluginRepoWithTag() string {
	return ps.getPluginRepo() + ":" + "latest"
}

func (ps *DockerPluginSuite) SetUpSuite(c *testing.T) {
	testRequires(c, DaemonIsLinux, RegistryHosting)
	ps.registry = registry.NewV2(c)
	ps.registry.WaitReady(c)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := plugin.CreateInRegistry(ctx, ps.getPluginRepo(), nil)
	assert.NilError(c, err, "failed to create plugin")
}

func (ps *DockerPluginSuite) TearDownSuite(c *testing.T) {
	if ps.registry != nil {
		ps.registry.Close()
	}
}

func (ps *DockerPluginSuite) TearDownTest(c *testing.T) {
	ps.ds.TearDownTest(c)
}

func (ps *DockerPluginSuite) OnTimeout(c *testing.T) {
	ps.ds.OnTimeout(c)
}

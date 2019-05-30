// +build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/helpers"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/ipamapi"
	remoteipam "github.com/docker/libnetwork/ipams/remote/api"
	"github.com/docker/swarmkit/ca/keyutils"
	"github.com/go-check/check"
	"github.com/vishvananda/netlink"
	"gotest.tools/assert"
	"gotest.tools/fs"
	"gotest.tools/icmd"
)

func (s *DockerSwarmSuite) TestSwarmUpdate(c *check.C) {
	d := s.AddDaemon(c, true, true)

	getSpec := func() swarm.Spec {
		sw := d.GetSwarm(c)
		return sw.Spec
	}

	out, err := d.Cmd("swarm", "update", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s")
	assert.NilError(c, err, out)

	spec := getSpec()
	c.Assert(spec.CAConfig.NodeCertExpiry, checker.Equals, 30*time.Hour)
	c.Assert(spec.Dispatcher.HeartbeatPeriod, checker.Equals, 11*time.Second)

	// setting anything under 30m for cert-expiry is not allowed
	out, err = d.Cmd("swarm", "update", "--cert-expiry", "15m")
	assert.ErrorContains(c, err, "")
	c.Assert(out, checker.Contains, "minimum certificate expiry time")
	spec = getSpec()
	c.Assert(spec.CAConfig.NodeCertExpiry, checker.Equals, 30*time.Hour)

	// passing an external CA (this is without starting a root rotation) does not fail
	cli.Docker(cli.Args("swarm", "update", "--external-ca", "protocol=cfssl,url=https://something.org",
		"--external-ca", "protocol=cfssl,url=https://somethingelse.org,cacert=fixtures/https/ca.pem"),
		cli.Daemon(d)).Assert(c, icmd.Success)

	expected, err := ioutil.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)

	spec = getSpec()
	c.Assert(spec.CAConfig.ExternalCAs, checker.HasLen, 2)
	c.Assert(spec.CAConfig.ExternalCAs[0].CACert, checker.Equals, "")
	c.Assert(spec.CAConfig.ExternalCAs[1].CACert, checker.Equals, string(expected))

	// passing an invalid external CA fails
	tempFile := fs.NewFile(c, "testfile", fs.WithContent("fakecert"))
	defer tempFile.Remove()

	result := cli.Docker(cli.Args("swarm", "update",
		"--external-ca", fmt.Sprintf("protocol=cfssl,url=https://something.org,cacert=%s", tempFile.Path())),
		cli.Daemon(d))
	result.Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "must be in PEM format",
	})
}

func (s *DockerSwarmSuite) TestSwarmInit(c *check.C) {
	d := s.AddDaemon(c, false, false)

	getSpec := func() swarm.Spec {
		sw := d.GetSwarm(c)
		return sw.Spec
	}

	// passing an invalid external CA fails
	tempFile := fs.NewFile(c, "testfile", fs.WithContent("fakecert"))
	defer tempFile.Remove()

	result := cli.Docker(cli.Args("swarm", "init", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s",
		"--external-ca", fmt.Sprintf("protocol=cfssl,url=https://somethingelse.org,cacert=%s", tempFile.Path())),
		cli.Daemon(d))
	result.Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "must be in PEM format",
	})

	cli.Docker(cli.Args("swarm", "init", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s",
		"--external-ca", "protocol=cfssl,url=https://something.org",
		"--external-ca", "protocol=cfssl,url=https://somethingelse.org,cacert=fixtures/https/ca.pem"),
		cli.Daemon(d)).Assert(c, icmd.Success)

	expected, err := ioutil.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)

	spec := getSpec()
	c.Assert(spec.CAConfig.NodeCertExpiry, checker.Equals, 30*time.Hour)
	c.Assert(spec.Dispatcher.HeartbeatPeriod, checker.Equals, 11*time.Second)
	c.Assert(spec.CAConfig.ExternalCAs, checker.HasLen, 2)
	c.Assert(spec.CAConfig.ExternalCAs[0].CACert, checker.Equals, "")
	c.Assert(spec.CAConfig.ExternalCAs[1].CACert, checker.Equals, string(expected))

	c.Assert(d.SwarmLeave(c, true), checker.IsNil)
	cli.Docker(cli.Args("swarm", "init"), cli.Daemon(d)).Assert(c, icmd.Success)

	spec = getSpec()
	c.Assert(spec.CAConfig.NodeCertExpiry, checker.Equals, 90*24*time.Hour)
	c.Assert(spec.Dispatcher.HeartbeatPeriod, checker.Equals, 5*time.Second)
}

func (s *DockerSwarmSuite) TestSwarmInitIPv6(c *check.C) {
	testRequires(c, IPv6)
	d1 := s.AddDaemon(c, false, false)
	cli.Docker(cli.Args("swarm", "init", "--listen-add", "::1"), cli.Daemon(d1)).Assert(c, icmd.Success)

	d2 := s.AddDaemon(c, false, false)
	cli.Docker(cli.Args("swarm", "join", "::1"), cli.Daemon(d2)).Assert(c, icmd.Success)

	out := cli.Docker(cli.Args("info"), cli.Daemon(d2)).Assert(c, icmd.Success).Combined()
	c.Assert(out, checker.Contains, "Swarm: active")
}

func (s *DockerSwarmSuite) TestSwarmInitUnspecifiedAdvertiseAddr(c *check.C) {
	d := s.AddDaemon(c, false, false)
	out, err := d.Cmd("swarm", "init", "--advertise-addr", "0.0.0.0")
	assert.ErrorContains(c, err, "")
	c.Assert(out, checker.Contains, "advertise address must be a non-zero IP address")
}

func (s *DockerSwarmSuite) TestSwarmIncompatibleDaemon(c *check.C) {
	// init swarm mode and stop a daemon
	d := s.AddDaemon(c, true, true)
	info := d.SwarmInfo(c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	d.Stop(c)

	// start a daemon with --cluster-store and --cluster-advertise
	err := d.StartWithError("--cluster-store=consul://consuladdr:consulport/some/path", "--cluster-advertise=1.1.1.1:2375")
	assert.ErrorContains(c, err, "")
	content, err := d.ReadLogFile()
	assert.NilError(c, err)
	c.Assert(string(content), checker.Contains, "--cluster-store and --cluster-advertise daemon configurations are incompatible with swarm mode")

	// start a daemon with --live-restore
	err = d.StartWithError("--live-restore")
	assert.ErrorContains(c, err, "")
	content, err = d.ReadLogFile()
	assert.NilError(c, err)
	c.Assert(string(content), checker.Contains, "--live-restore daemon configuration is incompatible with swarm mode")
	// restart for teardown
	d.StartNode(c)
}

func (s *DockerSwarmSuite) TestSwarmServiceTemplatingHostname(c *check.C) {
	d := s.AddDaemon(c, true, true)
	hostname, err := d.Cmd("node", "inspect", "--format", "{{.Description.Hostname}}", "self")
	c.Assert(err, checker.IsNil, check.Commentf("%s", hostname))

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "test", "--hostname", "{{.Service.Name}}-{{.Task.Slot}}-{{.Node.Hostname}}", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	containers := d.ActiveContainers(c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.Config.Hostname}}", containers[0])
	assert.NilError(c, err, out)
	c.Assert(strings.Split(out, "\n")[0], checker.Equals, "test-1-"+strings.Split(hostname, "\n")[0], check.Commentf("hostname with templating invalid"))
}

// Test case for #24270
func (s *DockerSwarmSuite) TestSwarmServiceListFilter(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name1 := "redis-cluster-md5"
	name2 := "redis-cluster"
	name3 := "other-cluster"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name1, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name2, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name3, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	filter1 := "name=redis-cluster-md5"
	filter2 := "name=redis-cluster"

	// We search checker.Contains with `name+" "` to prevent prefix only.
	out, err = d.Cmd("service", "ls", "--filter", filter1)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name1+" ")
	c.Assert(out, checker.Not(checker.Contains), name2+" ")
	c.Assert(out, checker.Not(checker.Contains), name3+" ")

	out, err = d.Cmd("service", "ls", "--filter", filter2)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name1+" ")
	c.Assert(out, checker.Contains, name2+" ")
	c.Assert(out, checker.Not(checker.Contains), name3+" ")

	out, err = d.Cmd("service", "ls")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name1+" ")
	c.Assert(out, checker.Contains, name2+" ")
	c.Assert(out, checker.Contains, name3+" ")
}

func (s *DockerSwarmSuite) TestSwarmNodeListFilter(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("node", "inspect", "--format", "{{ .Description.Hostname }}", "self")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
	name := strings.TrimSpace(out)

	filter := "name=" + name[:4]

	out, err = d.Cmd("node", "ls", "--filter", filter)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name)

	out, err = d.Cmd("node", "ls", "--filter", "name=none")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Not(checker.Contains), name)
}

func (s *DockerSwarmSuite) TestSwarmNodeTaskListFilter(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "redis-cluster-md5"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--replicas=3", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 3)

	filter := "name=redis-cluster"

	out, err = d.Cmd("node", "ps", "--filter", filter, "self")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name+".1")
	c.Assert(out, checker.Contains, name+".2")
	c.Assert(out, checker.Contains, name+".3")

	out, err = d.Cmd("node", "ps", "--filter", "name=none", "self")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Not(checker.Contains), name+".1")
	c.Assert(out, checker.Not(checker.Contains), name+".2")
	c.Assert(out, checker.Not(checker.Contains), name+".3")
}

// Test case for #25375
func (s *DockerSwarmSuite) TestSwarmPublishAdd(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--label", "x=y", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "update", "--detach", "--publish-add", "80:80", name)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "update", "--detach", "--publish-add", "80:80", name)
	assert.NilError(c, err, out)

	_, err = d.Cmd("service", "update", "--detach", "--publish-add", "80:80", "--publish-add", "80:20", name)
	assert.ErrorContains(c, err, "")

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.EndpointSpec.Ports }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "[{ tcp 80 80 ingress}]")
}

func (s *DockerSwarmSuite) TestSwarmServiceWithGroup(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--user", "root:root", "--group", "wheel", "--group", "audio", "--group", "staff", "--group", "777", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	container := strings.TrimSpace(out)

	out, err = d.Cmd("exec", container, "id")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "uid=0(root) gid=0(root) groups=10(wheel),29(audio),50(staff),777")
}

func (s *DockerSwarmSuite) TestSwarmContainerAutoStart(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("run", "-id", "--restart=always", "--net=foo", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	d.RestartNode(c)

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
}

func (s *DockerSwarmSuite) TestSwarmContainerEndpointOptions(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("run", "-d", "--net=foo", "--name=first", "--net-alias=first-alias", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("run", "-d", "--net=foo", "--name=second", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("run", "-d", "--net=foo", "--net-alias=third-alias", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	// ping first container and its alias, also ping third and anonymous container by its alias
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err, out)
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "first-alias")
	assert.NilError(c, err, out)
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "third-alias")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmContainerAttachByNetworkId(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "testnet")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
	networkID := strings.TrimSpace(out)

	out, err = d.Cmd("run", "-d", "--net", networkID, "busybox", "top")
	assert.NilError(c, err, out)
	cID := strings.TrimSpace(out)
	d.WaitRun(cID)

	out, err = d.Cmd("rm", "-f", cID)
	assert.NilError(c, err, out)

	out, err = d.Cmd("network", "rm", "testnet")
	assert.NilError(c, err, out)

	checkNetwork := func(*check.C) (interface{}, check.CommentInterface) {
		out, err := d.Cmd("network", "ls")
		assert.NilError(c, err)
		return out, nil
	}

	waitAndAssert(c, 3*time.Second, checkNetwork, checker.Not(checker.Contains), "testnet")
}

func (s *DockerSwarmSuite) TestOverlayAttachable(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", "ovnet")
	assert.NilError(c, err, out)

	// validate attachable
	out, err = d.Cmd("network", "inspect", "--format", "{{json .Attachable}}", "ovnet")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")

	// validate containers can attach to this overlay network
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c1", "busybox", "top")
	assert.NilError(c, err, out)

	// redo validation, there was a bug that the value of attachable changes after
	// containers attach to the network
	out, err = d.Cmd("network", "inspect", "--format", "{{json .Attachable}}", "ovnet")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestOverlayAttachableOnSwarmLeave(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create an attachable swarm network
	nwName := "attovl"
	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", nwName)
	assert.NilError(c, err, out)

	// Connect a container to the network
	out, err = d.Cmd("run", "-d", "--network", nwName, "--name", "c1", "busybox", "top")
	assert.NilError(c, err, out)

	// Leave the swarm
	c.Assert(d.SwarmLeave(c, true), checker.IsNil)

	// Check the container is disconnected
	out, err = d.Cmd("inspect", "c1", "--format", "{{.NetworkSettings.Networks."+nwName+"}}")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "<no value>")

	// Check the network is gone
	out, err = d.Cmd("network", "ls", "--format", "{{.Name}}")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Not(checker.Contains), nwName)
}

func (s *DockerSwarmSuite) TestOverlayAttachableReleaseResourcesOnFailure(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create attachable network
	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", "--subnet", "10.10.9.0/24", "ovnet")
	assert.NilError(c, err, out)

	// Attach a container with specific IP
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c1", "--ip", "10.10.9.33", "busybox", "top")
	assert.NilError(c, err, out)

	// Attempt to attach another container with same IP, must fail
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c2", "--ip", "10.10.9.33", "busybox", "top")
	assert.ErrorContains(c, err, "", out)

	// Remove first container
	out, err = d.Cmd("rm", "-f", "c1")
	assert.NilError(c, err, out)

	// Verify the network can be removed, no phantom network attachment task left over
	out, err = d.Cmd("network", "rm", "ovnet")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmIngressNetwork(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Ingress network can be removed
	removeNetwork := func(name string) *icmd.Result {
		return cli.Docker(
			cli.Args("-H", d.Sock(), "network", "rm", name),
			cli.WithStdin(strings.NewReader("Y")))
	}

	result := removeNetwork("ingress")
	result.Assert(c, icmd.Success)

	// And recreated
	out, err := d.Cmd("network", "create", "-d", "overlay", "--ingress", "new-ingress")
	assert.NilError(c, err, out)

	// But only one is allowed
	out, err = d.Cmd("network", "create", "-d", "overlay", "--ingress", "another-ingress")
	assert.ErrorContains(c, err, "")
	c.Assert(strings.TrimSpace(out), checker.Contains, "is already present")

	// It cannot be removed if it is being used
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv1", "-p", "9000:8000", "busybox", "top")
	assert.NilError(c, err, out)

	result = removeNetwork("new-ingress")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "ingress network cannot be removed because service",
	})

	// But it can be removed once no more services depend on it
	out, err = d.Cmd("service", "update", "--detach", "--publish-rm", "9000:8000", "srv1")
	assert.NilError(c, err, out)

	result = removeNetwork("new-ingress")
	result.Assert(c, icmd.Success)

	// A service which needs the ingress network cannot be created if no ingress is present
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv2", "-p", "500:500", "busybox", "top")
	assert.ErrorContains(c, err, "")
	c.Assert(strings.TrimSpace(out), checker.Contains, "no ingress network is present")

	// An existing service cannot be updated to use the ingress nw if the nw is not present
	out, err = d.Cmd("service", "update", "--detach", "--publish-add", "9000:8000", "srv1")
	assert.ErrorContains(c, err, "")
	c.Assert(strings.TrimSpace(out), checker.Contains, "no ingress network is present")

	// But services which do not need routing mesh can be created regardless
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv3", "--endpoint-mode", "dnsrr", "busybox", "top")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmCreateServiceWithNoIngressNetwork(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Remove ingress network
	result := cli.Docker(
		cli.Args("-H", d.Sock(), "network", "rm", "ingress"),
		cli.WithStdin(strings.NewReader("Y")))
	result.Assert(c, icmd.Success)

	// Create a overlay network and launch a service on it
	// Make sure nothing panics because ingress network is missing
	out, err := d.Cmd("network", "create", "-d", "overlay", "another-network")
	assert.NilError(c, err, out)
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv4", "--network", "another-network", "busybox", "top")
	assert.NilError(c, err, out)
}

// Test case for #24108, also the case from:
// https://github.com/docker/docker/pull/24620#issuecomment-233715656
func (s *DockerSwarmSuite) TestSwarmTaskListFilter(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "redis-cluster-md5"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--replicas=3", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	filter := "name=redis-cluster"

	checkNumTasks := func(*check.C) (interface{}, check.CommentInterface) {
		out, err := d.Cmd("service", "ps", "--filter", filter, name)
		assert.NilError(c, err, out)
		return len(strings.Split(out, "\n")) - 2, nil // includes header and nl in last line
	}

	// wait until all tasks have been created
	waitAndAssert(c, defaultReconciliationTimeout, checkNumTasks, checker.Equals, 3)

	out, err = d.Cmd("service", "ps", "--filter", filter, name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name+".1")
	c.Assert(out, checker.Contains, name+".2")
	c.Assert(out, checker.Contains, name+".3")

	out, err = d.Cmd("service", "ps", "--filter", "name="+name+".1", name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name+".1")
	c.Assert(out, checker.Not(checker.Contains), name+".2")
	c.Assert(out, checker.Not(checker.Contains), name+".3")

	out, err = d.Cmd("service", "ps", "--filter", "name=none", name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Not(checker.Contains), name+".1")
	c.Assert(out, checker.Not(checker.Contains), name+".2")
	c.Assert(out, checker.Not(checker.Contains), name+".3")

	name = "redis-cluster-sha1"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--mode=global", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	waitAndAssert(c, defaultReconciliationTimeout, checkNumTasks, checker.Equals, 1)

	filter = "name=redis-cluster"
	out, err = d.Cmd("service", "ps", "--filter", filter, name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name)

	out, err = d.Cmd("service", "ps", "--filter", "name="+name, name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, name)

	out, err = d.Cmd("service", "ps", "--filter", "name=none", name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Not(checker.Contains), name)
}

func (s *DockerSwarmSuite) TestPsListContainersFilterIsTask(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a bare container
	out, err := d.Cmd("run", "-d", "--name=bare-container", "busybox", "top")
	assert.NilError(c, err, out)
	bareID := strings.TrimSpace(out)[:12]
	// Create a service
	name := "busybox-top"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckServiceRunningTasks(name), checker.Equals, 1)

	// Filter non-tasks
	out, err = d.Cmd("ps", "-a", "-q", "--filter=is-task=false")
	assert.NilError(c, err, out)
	psOut := strings.TrimSpace(out)
	c.Assert(psOut, checker.Equals, bareID, check.Commentf("Expected id %s, got %s for is-task label, output %q", bareID, psOut, out))

	// Filter tasks
	out, err = d.Cmd("ps", "-a", "-q", "--filter=is-task=true")
	assert.NilError(c, err, out)
	lines := strings.Split(strings.Trim(out, "\n "), "\n")
	assert.Equal(c, len(lines), 1)
	c.Assert(lines[0], checker.Not(checker.Equals), bareID, check.Commentf("Expected not %s, but got it for is-task label, output %q", bareID, out))
}

const globalNetworkPlugin = "global-network-plugin"
const globalIPAMPlugin = "global-ipam-plugin"

func setupRemoteGlobalNetworkPlugin(c *check.C, mux *http.ServeMux, url, netDrv, ipamDrv string) {

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"Implements": ["%s", "%s"]}`, driverapi.NetworkPluginEndpointType, ipamapi.PluginEndpointType)
	})

	// Network driver implementation
	mux.HandleFunc(fmt.Sprintf("/%s.GetCapabilities", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"Scope":"global"}`)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.AllocateNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&remoteDriverNetworkRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, "null")
	})

	mux.HandleFunc(fmt.Sprintf("/%s.FreeNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, "null")
	})

	mux.HandleFunc(fmt.Sprintf("/%s.CreateNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&remoteDriverNetworkRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, "null")
	})

	mux.HandleFunc(fmt.Sprintf("/%s.DeleteNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, "null")
	})

	mux.HandleFunc(fmt.Sprintf("/%s.CreateEndpoint", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"Interface":{"MacAddress":"a0:b1:c2:d3:e4:f5"}}`)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.Join", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")

		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: "randomIfName", TxQLen: 0}, PeerName: "cnt0"}
		if err := netlink.LinkAdd(veth); err != nil {
			fmt.Fprintf(w, `{"Error":"failed to add veth pair: `+err.Error()+`"}`)
		} else {
			fmt.Fprintf(w, `{"InterfaceName":{ "SrcName":"cnt0", "DstPrefix":"veth"}}`)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.Leave", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, "null")
	})

	mux.HandleFunc(fmt.Sprintf("/%s.DeleteEndpoint", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		if link, err := netlink.LinkByName("cnt0"); err == nil {
			netlink.LinkDel(link)
		}
		fmt.Fprintf(w, "null")
	})

	// IPAM Driver implementation
	var (
		poolRequest       remoteipam.RequestPoolRequest
		poolReleaseReq    remoteipam.ReleasePoolRequest
		addressRequest    remoteipam.RequestAddressRequest
		addressReleaseReq remoteipam.ReleaseAddressRequest
		lAS               = "localAS"
		gAS               = "globalAS"
		pool              = "172.28.0.0/16"
		poolID            = lAS + "/" + pool
		gw                = "172.28.255.254/16"
	)

	mux.HandleFunc(fmt.Sprintf("/%s.GetDefaultAddressSpaces", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"LocalDefaultAddressSpace":"`+lAS+`", "GlobalDefaultAddressSpace": "`+gAS+`"}`)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.RequestPool", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&poolRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		if poolRequest.AddressSpace != lAS && poolRequest.AddressSpace != gAS {
			fmt.Fprintf(w, `{"Error":"Unknown address space in pool request: `+poolRequest.AddressSpace+`"}`)
		} else if poolRequest.Pool != "" && poolRequest.Pool != pool {
			fmt.Fprintf(w, `{"Error":"Cannot handle explicit pool requests yet"}`)
		} else {
			fmt.Fprintf(w, `{"PoolID":"`+poolID+`", "Pool":"`+pool+`"}`)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.RequestAddress", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&addressRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		// make sure libnetwork is now querying on the expected pool id
		if addressRequest.PoolID != poolID {
			fmt.Fprintf(w, `{"Error":"unknown pool id"}`)
		} else if addressRequest.Address != "" {
			fmt.Fprintf(w, `{"Error":"Cannot handle explicit address requests yet"}`)
		} else {
			fmt.Fprintf(w, `{"Address":"`+gw+`"}`)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.ReleaseAddress", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&addressReleaseReq)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		// make sure libnetwork is now asking to release the expected address from the expected poolid
		if addressRequest.PoolID != poolID {
			fmt.Fprintf(w, `{"Error":"unknown pool id"}`)
		} else if addressReleaseReq.Address != gw {
			fmt.Fprintf(w, `{"Error":"unknown address"}`)
		} else {
			fmt.Fprintf(w, "null")
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.ReleasePool", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&poolReleaseReq)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		// make sure libnetwork is now asking to release the expected poolid
		if addressRequest.PoolID != poolID {
			fmt.Fprintf(w, `{"Error":"unknown pool id"}`)
		} else {
			fmt.Fprintf(w, "null")
		}
	})

	err := os.MkdirAll("/etc/docker/plugins", 0755)
	assert.NilError(c, err)

	fileName := fmt.Sprintf("/etc/docker/plugins/%s.spec", netDrv)
	err = ioutil.WriteFile(fileName, []byte(url), 0644)
	assert.NilError(c, err)

	ipamFileName := fmt.Sprintf("/etc/docker/plugins/%s.spec", ipamDrv)
	err = ioutil.WriteFile(ipamFileName, []byte(url), 0644)
	assert.NilError(c, err)
}

func (s *DockerSwarmSuite) TestSwarmNetworkPlugin(c *check.C) {
	mux := http.NewServeMux()
	s.server = httptest.NewServer(mux)
	c.Assert(s.server, check.NotNil) // check that HTTP server has started
	setupRemoteGlobalNetworkPlugin(c, mux, s.server.URL, globalNetworkPlugin, globalIPAMPlugin)
	defer func() {
		s.server.Close()
		err := os.RemoveAll("/etc/docker/plugins")
		assert.NilError(c, err)
	}()

	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "-d", globalNetworkPlugin, "foo")
	assert.ErrorContains(c, err, "", out)
	c.Assert(out, checker.Contains, "not supported in swarm mode")
}

// Test case for #24712
func (s *DockerSwarmSuite) TestSwarmServiceEnvFile(c *check.C) {
	d := s.AddDaemon(c, true, true)

	path := filepath.Join(d.Folder, "env.txt")
	err := ioutil.WriteFile(path, []byte("VAR1=A\nVAR2=A\n"), 0644)
	assert.NilError(c, err)

	name := "worker"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--env-file", path, "--env", "VAR1=B", "--env", "VAR1=C", "--env", "VAR2=", "--env", "VAR2", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// The complete env is [VAR1=A VAR2=A VAR1=B VAR1=C VAR2= VAR2] and duplicates will be removed => [VAR1=C VAR2]
	out, err = d.Cmd("inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.Env }}", name)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, "[VAR1=C VAR2]")
}

func (s *DockerSwarmSuite) TestSwarmServiceTTY(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "top"

	ttyCheck := "if [ -t 0 ]; then echo TTY > /status && top; else echo none > /status && top; fi"

	// Without --tty
	expectedOutput := "none"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "sh", "-c", ttyCheck)
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// We need to get the container id.
	out, err = d.Cmd("ps", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	out, err = d.Cmd("exec", id, "cat", "/status")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, expectedOutput, check.Commentf("Expected '%s', but got %q", expectedOutput, out))

	// Remove service
	out, err = d.Cmd("service", "rm", name)
	assert.NilError(c, err, out)
	// Make sure container has been destroyed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 0)

	// With --tty
	expectedOutput = "TTY"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--tty", "busybox", "sh", "-c", ttyCheck)
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// We need to get the container id.
	out, err = d.Cmd("ps", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id = strings.TrimSpace(out)

	out, err = d.Cmd("exec", id, "cat", "/status")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, expectedOutput, check.Commentf("Expected '%s', but got %q", expectedOutput, out))
}

func (s *DockerSwarmSuite) TestSwarmServiceTTYUpdate(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.TTY }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "false")

	out, err = d.Cmd("service", "update", "--detach", "--tty", name)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.TTY }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestSwarmServiceNetworkUpdate(c *check.C) {
	d := s.AddDaemon(c, true, true)

	result := icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "foo"))
	result.Assert(c, icmd.Success)
	fooNetwork := strings.TrimSpace(string(result.Combined()))

	result = icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "bar"))
	result.Assert(c, icmd.Success)
	barNetwork := strings.TrimSpace(string(result.Combined()))

	result = icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "baz"))
	result.Assert(c, icmd.Success)
	bazNetwork := strings.TrimSpace(string(result.Combined()))

	// Create a service
	name := "top"
	result = icmd.RunCmd(d.Command("service", "create", "--detach", "--no-resolve-image", "--network", "foo", "--network", "bar", "--name", name, "busybox", "top"))
	result.Assert(c, icmd.Success)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskNetworks, checker.DeepEquals,
		map[string]int{fooNetwork: 1, barNetwork: 1})

	// Remove a network
	result = icmd.RunCmd(d.Command("service", "update", "--detach", "--network-rm", "foo", name))
	result.Assert(c, icmd.Success)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskNetworks, checker.DeepEquals,
		map[string]int{barNetwork: 1})

	// Add a network
	result = icmd.RunCmd(d.Command("service", "update", "--detach", "--network-add", "baz", name))
	result.Assert(c, icmd.Success)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskNetworks, checker.DeepEquals,
		map[string]int{barNetwork: 1, bazNetwork: 1})
}

func (s *DockerSwarmSuite) TestDNSConfig(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--dns=1.2.3.4", "--dns-search=example.com", "--dns-option=timeout:3", "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// We need to get the container id.
	out, err = d.Cmd("ps", "-a", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// Compare against expected output.
	expectedOutput1 := "nameserver 1.2.3.4"
	expectedOutput2 := "search example.com"
	expectedOutput3 := "options timeout:3"
	out, err = d.Cmd("exec", id, "cat", "/etc/resolv.conf")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, expectedOutput1, check.Commentf("Expected '%s', but got %q", expectedOutput1, out))
	c.Assert(out, checker.Contains, expectedOutput2, check.Commentf("Expected '%s', but got %q", expectedOutput2, out))
	c.Assert(out, checker.Contains, expectedOutput3, check.Commentf("Expected '%s', but got %q", expectedOutput3, out))
}

func (s *DockerSwarmSuite) TestDNSConfigUpdate(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("service", "update", "--detach", "--dns-add=1.2.3.4", "--dns-search-add=example.com", "--dns-option-add=timeout:3", name)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.DNSConfig }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "{[1.2.3.4] [example.com] [timeout:3]}")
}

func getNodeStatus(c *check.C, d *daemon.Daemon) swarm.LocalNodeState {
	info := d.SwarmInfo(c)
	return info.LocalNodeState
}

func checkKeyIsEncrypted(d *daemon.Daemon) func(*check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		keyBytes, err := ioutil.ReadFile(filepath.Join(d.Folder, "root", "swarm", "certificates", "swarm-node.key"))
		if err != nil {
			return fmt.Errorf("error reading key: %v", err), nil
		}

		keyBlock, _ := pem.Decode(keyBytes)
		if keyBlock == nil {
			return fmt.Errorf("invalid PEM-encoded private key"), nil
		}

		return keyutils.IsEncryptedPEMBlock(keyBlock), nil
	}
}

func checkSwarmLockedToUnlocked(c *check.C, d *daemon.Daemon) {
	// Wait for the PEM file to become unencrypted
	waitAndAssert(c, defaultReconciliationTimeout, checkKeyIsEncrypted(d), checker.Equals, false)

	d.RestartNode(c)
	waitAndAssert(c, time.Second, d.CheckLocalNodeState, checker.Equals, swarm.LocalNodeStateActive)
}

func checkSwarmUnlockedToLocked(c *check.C, d *daemon.Daemon) {
	// Wait for the PEM file to become encrypted
	waitAndAssert(c, defaultReconciliationTimeout, checkKeyIsEncrypted(d), checker.Equals, true)

	d.RestartNode(c)
	waitAndAssert(c, time.Second, d.CheckLocalNodeState, checker.Equals, swarm.LocalNodeStateLocked)
}

func (s *DockerSwarmSuite) TestUnlockEngineAndUnlockedSwarm(c *check.C) {
	d := s.AddDaemon(c, false, false)

	// unlocking a normal engine should return an error - it does not even ask for the key
	cmd := d.Command("swarm", "unlock")
	result := icmd.RunCmd(cmd)
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	c.Assert(result.Combined(), checker.Contains, "Error: This node is not part of a swarm")
	c.Assert(result.Combined(), checker.Not(checker.Contains), "Please enter unlock key")

	out, err := d.Cmd("swarm", "init")
	assert.NilError(c, err, out)

	// unlocking an unlocked swarm should return an error - it does not even ask for the key
	cmd = d.Command("swarm", "unlock")
	result = icmd.RunCmd(cmd)
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	c.Assert(result.Combined(), checker.Contains, "Error: swarm is not locked")
	c.Assert(result.Combined(), checker.Not(checker.Contains), "Please enter unlock key")
}

func (s *DockerSwarmSuite) TestSwarmInitLocked(c *check.C) {
	d := s.AddDaemon(c, false, false)

	outs, err := d.Cmd("swarm", "init", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
	unlockKey := getUnlockKey(d, c, outs)

	c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)

	// It starts off locked
	d.RestartNode(c)
	c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateLocked)

	cmd := d.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString("wrong-secret-key")
	icmd.RunCmd(cmd).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "invalid key",
	})

	c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateLocked)

	cmd = d.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString(unlockKey)
	icmd.RunCmd(cmd).Assert(c, icmd.Success)

	c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)

	outs, err = d.Cmd("node", "ls")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
	c.Assert(outs, checker.Not(checker.Contains), "Swarm is encrypted and needs to be unlocked")

	outs, err = d.Cmd("swarm", "update", "--autolock=false")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))

	checkSwarmLockedToUnlocked(c, d)

	outs, err = d.Cmd("node", "ls")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
	c.Assert(outs, checker.Not(checker.Contains), "Swarm is encrypted and needs to be unlocked")
}

func (s *DockerSwarmSuite) TestSwarmLeaveLocked(c *check.C) {
	d := s.AddDaemon(c, false, false)

	outs, err := d.Cmd("swarm", "init", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))

	// It starts off locked
	d.RestartNode(c)

	info := d.SwarmInfo(c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateLocked)

	outs, _ = d.Cmd("node", "ls")
	c.Assert(outs, checker.Contains, "Swarm is encrypted and needs to be unlocked")

	// `docker swarm leave` a locked swarm without --force will return an error
	outs, _ = d.Cmd("swarm", "leave")
	c.Assert(outs, checker.Contains, "Swarm is encrypted and locked.")

	// It is OK for user to leave a locked swarm with --force
	outs, err = d.Cmd("swarm", "leave", "--force")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))

	info = d.SwarmInfo(c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	outs, err = d.Cmd("swarm", "init")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))

	info = d.SwarmInfo(c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
}

func (s *DockerSwarmSuite) TestSwarmLockUnlockCluster(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, true)
	d3 := s.AddDaemon(c, true, true)

	// they start off unlocked
	d2.RestartNode(c)
	c.Assert(getNodeStatus(c, d2), checker.Equals, swarm.LocalNodeStateActive)

	// stop this one so it does not get autolock info
	d2.Stop(c)

	// enable autolock
	outs, err := d1.Cmd("swarm", "update", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
	unlockKey := getUnlockKey(d1, c, outs)

	// The ones that got the cluster update should be set to locked
	for _, d := range []*daemon.Daemon{d1, d3} {
		checkSwarmUnlockedToLocked(c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)
		c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)
	}

	// d2 never got the cluster update, so it is still set to unlocked
	d2.StartNode(c)
	c.Assert(getNodeStatus(c, d2), checker.Equals, swarm.LocalNodeStateActive)

	// d2 is now set to lock
	checkSwarmUnlockedToLocked(c, d2)

	// leave it locked, and set the cluster to no longer autolock
	outs, err = d1.Cmd("swarm", "update", "--autolock=false")
	c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))

	// the ones that got the update are now set to unlocked
	for _, d := range []*daemon.Daemon{d1, d3} {
		checkSwarmLockedToUnlocked(c, d)
	}

	// d2 still locked
	c.Assert(getNodeStatus(c, d2), checker.Equals, swarm.LocalNodeStateLocked)

	// unlock it
	cmd := d2.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString(unlockKey)
	icmd.RunCmd(cmd).Assert(c, icmd.Success)
	c.Assert(getNodeStatus(c, d2), checker.Equals, swarm.LocalNodeStateActive)

	// once it's caught up, d2 is set to not be locked
	checkSwarmLockedToUnlocked(c, d2)

	// managers who join now are never set to locked in the first place
	d4 := s.AddDaemon(c, true, true)
	d4.RestartNode(c)
	c.Assert(getNodeStatus(c, d4), checker.Equals, swarm.LocalNodeStateActive)
}

func (s *DockerSwarmSuite) TestSwarmJoinPromoteLocked(c *check.C) {
	d1 := s.AddDaemon(c, true, true)

	// enable autolock
	outs, err := d1.Cmd("swarm", "update", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))
	unlockKey := getUnlockKey(d1, c, outs)

	// joined workers start off unlocked
	d2 := s.AddDaemon(c, true, false)
	d2.RestartNode(c)
	waitAndAssert(c, time.Second, d2.CheckLocalNodeState, checker.Equals, swarm.LocalNodeStateActive)

	// promote worker
	outs, err = d1.Cmd("node", "promote", d2.NodeID())
	assert.NilError(c, err)
	c.Assert(outs, checker.Contains, "promoted to a manager in the swarm")

	// join new manager node
	d3 := s.AddDaemon(c, true, true)

	// both new nodes are locked
	for _, d := range []*daemon.Daemon{d2, d3} {
		checkSwarmUnlockedToLocked(c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)
		c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)
	}

	// demote manager back to worker - workers are not locked
	outs, err = d1.Cmd("node", "demote", d3.NodeID())
	assert.NilError(c, err)
	c.Assert(outs, checker.Contains, "demoted in the swarm")

	// Wait for it to actually be demoted, for the key and cert to be replaced.
	// Then restart and assert that the node is not locked.  If we don't wait for the cert
	// to be replaced, then the node still has the manager TLS key which is still locked
	// (because we never want a manager TLS key to be on disk unencrypted if the cluster
	// is set to autolock)
	waitAndAssert(c, defaultReconciliationTimeout, d3.CheckControlAvailable, checker.False)
	waitAndAssert(c, defaultReconciliationTimeout, func(c *check.C) (interface{}, check.CommentInterface) {
		certBytes, err := ioutil.ReadFile(filepath.Join(d3.Folder, "root", "swarm", "certificates", "swarm-node.crt"))
		if err != nil {
			return "", check.Commentf("error: %v", err)
		}
		certs, err := helpers.ParseCertificatesPEM(certBytes)
		if err == nil && len(certs) > 0 && len(certs[0].Subject.OrganizationalUnit) > 0 {
			return certs[0].Subject.OrganizationalUnit[0], nil
		}
		return "", check.Commentf("could not get organizational unit from certificate")
	}, checker.Equals, "swarm-worker")

	// by now, it should *never* be locked on restart
	d3.RestartNode(c)
	waitAndAssert(c, time.Second, d3.CheckLocalNodeState, checker.Equals, swarm.LocalNodeStateActive)
}

func (s *DockerSwarmSuite) TestSwarmRotateUnlockKey(c *check.C) {
	d := s.AddDaemon(c, true, true)

	outs, err := d.Cmd("swarm", "update", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))
	unlockKey := getUnlockKey(d, c, outs)

	// Rotate multiple times
	for i := 0; i != 3; i++ {
		outs, err = d.Cmd("swarm", "unlock-key", "-q", "--rotate")
		c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))
		// Strip \n
		newUnlockKey := outs[:len(outs)-1]
		c.Assert(newUnlockKey, checker.Not(checker.Equals), "")
		c.Assert(newUnlockKey, checker.Not(checker.Equals), unlockKey)

		d.RestartNode(c)
		c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateLocked)

		outs, _ = d.Cmd("node", "ls")
		c.Assert(outs, checker.Contains, "Swarm is encrypted and needs to be unlocked")

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		result := icmd.RunCmd(cmd)

		if result.Error == nil {
			// On occasion, the daemon may not have finished
			// rotating the KEK before restarting. The test is
			// intentionally written to explore this behavior.
			// When this happens, unlocking with the old key will
			// succeed. If we wait for the rotation to happen and
			// restart again, the new key should be required this
			// time.

			time.Sleep(3 * time.Second)

			d.RestartNode(c)

			cmd = d.Command("swarm", "unlock")
			cmd.Stdin = bytes.NewBufferString(unlockKey)
			result = icmd.RunCmd(cmd)
		}
		result.Assert(c, icmd.Expected{
			ExitCode: 1,
			Err:      "invalid key",
		})

		outs, _ = d.Cmd("node", "ls")
		c.Assert(outs, checker.Contains, "Swarm is encrypted and needs to be unlocked")

		cmd = d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(newUnlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)

		c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)

		outs, err = d.Cmd("node", "ls")
		assert.NilError(c, err)
		c.Assert(outs, checker.Not(checker.Contains), "Swarm is encrypted and needs to be unlocked")

		unlockKey = newUnlockKey
	}
}

// This differs from `TestSwarmRotateUnlockKey` because that one rotates a single node, which is the leader.
// This one keeps the leader up, and asserts that other manager nodes in the cluster also have their unlock
// key rotated.
func (s *DockerSwarmSuite) TestSwarmClusterRotateUnlockKey(c *check.C) {
	if runtime.GOARCH == "s390x" {
		c.Skip("Disabled on s390x")
	}
	if runtime.GOARCH == "ppc64le" {
		c.Skip("Disabled on  ppc64le")
	}

	d1 := s.AddDaemon(c, true, true) // leader - don't restart this one, we don't want leader election delays
	d2 := s.AddDaemon(c, true, true)
	d3 := s.AddDaemon(c, true, true)

	outs, err := d1.Cmd("swarm", "update", "--autolock")
	c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
	unlockKey := getUnlockKey(d1, c, outs)

	// Rotate multiple times
	for i := 0; i != 3; i++ {
		outs, err = d1.Cmd("swarm", "unlock-key", "-q", "--rotate")
		c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
		// Strip \n
		newUnlockKey := outs[:len(outs)-1]
		c.Assert(newUnlockKey, checker.Not(checker.Equals), "")
		c.Assert(newUnlockKey, checker.Not(checker.Equals), unlockKey)

		d2.RestartNode(c)
		d3.RestartNode(c)

		for _, d := range []*daemon.Daemon{d2, d3} {
			c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateLocked)

			outs, _ := d.Cmd("node", "ls")
			c.Assert(outs, checker.Contains, "Swarm is encrypted and needs to be unlocked")

			cmd := d.Command("swarm", "unlock")
			cmd.Stdin = bytes.NewBufferString(unlockKey)
			result := icmd.RunCmd(cmd)

			if result.Error == nil {
				// On occasion, the daemon may not have finished
				// rotating the KEK before restarting. The test is
				// intentionally written to explore this behavior.
				// When this happens, unlocking with the old key will
				// succeed. If we wait for the rotation to happen and
				// restart again, the new key should be required this
				// time.

				time.Sleep(3 * time.Second)

				d.RestartNode(c)

				cmd = d.Command("swarm", "unlock")
				cmd.Stdin = bytes.NewBufferString(unlockKey)
				result = icmd.RunCmd(cmd)
			}
			result.Assert(c, icmd.Expected{
				ExitCode: 1,
				Err:      "invalid key",
			})

			outs, _ = d.Cmd("node", "ls")
			c.Assert(outs, checker.Contains, "Swarm is encrypted and needs to be unlocked")

			cmd = d.Command("swarm", "unlock")
			cmd.Stdin = bytes.NewBufferString(newUnlockKey)
			icmd.RunCmd(cmd).Assert(c, icmd.Success)

			c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)

			outs, err = d.Cmd("node", "ls")
			c.Assert(err, checker.IsNil, check.Commentf("%s", outs))
			c.Assert(outs, checker.Not(checker.Contains), "Swarm is encrypted and needs to be unlocked")
		}

		unlockKey = newUnlockKey
	}
}

func (s *DockerSwarmSuite) TestSwarmAlternateLockUnlock(c *check.C) {
	d := s.AddDaemon(c, true, true)

	for i := 0; i < 2; i++ {
		// set to lock
		outs, err := d.Cmd("swarm", "update", "--autolock")
		c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))
		c.Assert(outs, checker.Contains, "docker swarm unlock")
		unlockKey := getUnlockKey(d, c, outs)

		checkSwarmUnlockedToLocked(c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)

		c.Assert(getNodeStatus(c, d), checker.Equals, swarm.LocalNodeStateActive)

		outs, err = d.Cmd("swarm", "update", "--autolock=false")
		c.Assert(err, checker.IsNil, check.Commentf("out: %v", outs))

		checkSwarmLockedToUnlocked(c, d)
	}
}

func (s *DockerSwarmSuite) TestExtraHosts(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--host=example.com:1.2.3.4", "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// We need to get the container id.
	out, err = d.Cmd("ps", "-a", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// Compare against expected output.
	expectedOutput := "1.2.3.4\texample.com"
	out, err = d.Cmd("exec", id, "cat", "/etc/hosts")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, expectedOutput, check.Commentf("Expected '%s', but got %q", expectedOutput, out))
}

func (s *DockerSwarmSuite) TestSwarmManagerAddress(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, false)
	d3 := s.AddDaemon(c, true, false)

	// Manager Addresses will always show Node 1's address
	expectedOutput := fmt.Sprintf("Manager Addresses:\n  127.0.0.1:%d\n", d1.SwarmPort)

	out, err := d1.Cmd("info")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput))

	out, err = d2.Cmd("info")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput))

	out, err = d3.Cmd("info")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput))
}

func (s *DockerSwarmSuite) TestSwarmNetworkIPAMOptions(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("network", "create", "-d", "overlay", "--ipam-opt", "foo=bar", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("network", "inspect", "--format", "{{.IPAM.Options}}", "foo")
	assert.NilError(c, err, out)
	c.Assert(strings.TrimSpace(out), checker.Contains, "foo:bar")
	c.Assert(strings.TrimSpace(out), checker.Contains, "com.docker.network.ipam.serial:true")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--network=foo", "--name", "top", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("network", "inspect", "--format", "{{.IPAM.Options}}", "foo")
	assert.NilError(c, err, out)
	c.Assert(strings.TrimSpace(out), checker.Contains, "foo:bar")
	c.Assert(strings.TrimSpace(out), checker.Contains, "com.docker.network.ipam.serial:true")
}

// Test case for issue #27866, which did not allow NW name that is the prefix of a swarm NW ID.
// e.g. if the ingress ID starts with "n1", it was impossible to create a NW named "n1".
func (s *DockerSwarmSuite) TestSwarmNetworkCreateIssue27866(c *check.C) {
	d := s.AddDaemon(c, true, true)
	out, err := d.Cmd("network", "inspect", "-f", "{{.Id}}", "ingress")
	assert.NilError(c, err, "out: %v", out)
	ingressID := strings.TrimSpace(out)
	c.Assert(ingressID, checker.Not(checker.Equals), "")

	// create a network of which name is the prefix of the ID of an overlay network
	// (ingressID in this case)
	newNetName := ingressID[0:2]
	out, err = d.Cmd("network", "create", "--driver", "overlay", newNetName)
	// In #27866, it was failing because of "network with name %s already exists"
	assert.NilError(c, err, "out: %v", out)
	out, err = d.Cmd("network", "rm", newNetName)
	assert.NilError(c, err, "out: %v", out)
}

// Test case for https://github.com/docker/docker/pull/27938#issuecomment-265768303
// This test creates two networks with the same name sequentially, with various drivers.
// Since the operations in this test are done sequentially, the 2nd call should fail with
// "network with name FOO already exists".
// Note that it is to ok have multiple networks with the same name if the operations are done
// in parallel. (#18864)
func (s *DockerSwarmSuite) TestSwarmNetworkCreateDup(c *check.C) {
	d := s.AddDaemon(c, true, true)
	drivers := []string{"bridge", "overlay"}
	for i, driver1 := range drivers {
		nwName := fmt.Sprintf("network-test-%d", i)
		for _, driver2 := range drivers {
			c.Logf("Creating a network named %q with %q, then %q",
				nwName, driver1, driver2)
			out, err := d.Cmd("network", "create", "--driver", driver1, nwName)
			assert.NilError(c, err, "out: %v", out)
			out, err = d.Cmd("network", "create", "--driver", driver2, nwName)
			c.Assert(out, checker.Contains,
				fmt.Sprintf("network with name %s already exists", nwName))
			assert.ErrorContains(c, err, "")
			c.Logf("As expected, the attempt to network %q with %q failed: %s",
				nwName, driver2, out)
			out, err = d.Cmd("network", "rm", nwName)
			assert.NilError(c, err, "out: %v", out)
		}
	}
}

func (s *DockerSwarmSuite) TestSwarmPublishDuplicatePorts(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--publish", "5005:80", "--publish", "5006:80", "--publish", "80", "--publish", "80", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// Total len = 4, with 2 dynamic ports and 2 non-dynamic ports
	// Dynamic ports are likely to be 30000 and 30001 but doesn't matter
	out, err = d.Cmd("service", "inspect", "--format", "{{.Endpoint.Ports}} len={{len .Endpoint.Ports}}", id)
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, "len=4")
	c.Assert(out, checker.Contains, "{ tcp 80 5005 ingress}")
	c.Assert(out, checker.Contains, "{ tcp 80 5006 ingress}")
}

func (s *DockerSwarmSuite) TestSwarmJoinWithDrain(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("node", "ls")
	assert.NilError(c, err)
	c.Assert(out, checker.Not(checker.Contains), "Drain")

	out, err = d.Cmd("swarm", "join-token", "-q", "manager")
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	token := strings.TrimSpace(out)

	d1 := s.AddDaemon(c, false, false)

	out, err = d1.Cmd("swarm", "join", "--availability=drain", "--token", token, d.SwarmListenAddr())
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("node", "ls")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Drain")

	out, err = d1.Cmd("node", "ls")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Drain")
}

func (s *DockerSwarmSuite) TestSwarmInitWithDrain(c *check.C) {
	d := s.AddDaemon(c, false, false)

	out, err := d.Cmd("swarm", "init", "--availability", "drain")
	assert.NilError(c, err, "out: %v", out)

	out, err = d.Cmd("node", "ls")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Drain")
}

func (s *DockerSwarmSuite) TestSwarmReadonlyRootfs(c *check.C) {
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top", "--read-only", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.ReadOnly }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")

	containers := d.ActiveContainers(c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.HostConfig.ReadonlyRootfs}}", containers[0])
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestNetworkInspectWithDuplicateNames(c *check.C) {
	d := s.AddDaemon(c, true, true)

	name := "foo"
	options := types.NetworkCreate{
		CheckDuplicate: false,
		Driver:         "bridge",
	}

	cli := d.NewClientT(c)
	defer cli.Close()

	n1, err := cli.NetworkCreate(context.Background(), name, options)
	assert.NilError(c, err)

	// Full ID always works
	out, err := d.Cmd("network", "inspect", "--format", "{{.ID}}", n1.ID)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n1.ID)

	// Name works if it is unique
	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n1.ID)

	n2, err := cli.NetworkCreate(context.Background(), name, options)
	assert.NilError(c, err)
	// Full ID always works
	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", n1.ID)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n1.ID)

	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", n2.ID)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n2.ID)

	// Name with duplicates
	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", name)
	assert.ErrorContains(c, err, "", out)
	c.Assert(out, checker.Contains, "2 matches found based on name")

	out, err = d.Cmd("network", "rm", n2.ID)
	assert.NilError(c, err, out)

	// Duplicates with name but with different driver
	options.Driver = "overlay"

	n2, err = cli.NetworkCreate(context.Background(), name, options)
	assert.NilError(c, err)

	// Full ID always works
	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", n1.ID)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n1.ID)

	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", n2.ID)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), n2.ID)

	// Name with duplicates
	out, err = d.Cmd("network", "inspect", "--format", "{{.ID}}", name)
	assert.ErrorContains(c, err, "", out)
	c.Assert(out, checker.Contains, "2 matches found based on name")
}

func (s *DockerSwarmSuite) TestSwarmStopSignal(c *check.C) {
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top", "--stop-signal=SIGHUP", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.StopSignal }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGHUP")

	containers := d.ActiveContainers(c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.Config.StopSignal}}", containers[0])
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGHUP")

	out, err = d.Cmd("service", "update", "--detach", "--stop-signal=SIGUSR1", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.StopSignal }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGUSR1")
}

func (s *DockerSwarmSuite) TestSwarmServiceLsFilterMode(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top1", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top2", "--mode=global", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 2)

	out, err = d.Cmd("service", "ls")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, "top1")
	c.Assert(out, checker.Contains, "top2")
	c.Assert(out, checker.Not(checker.Contains), "localnet")

	out, err = d.Cmd("service", "ls", "--filter", "mode=global")
	c.Assert(out, checker.Not(checker.Contains), "top1")
	c.Assert(out, checker.Contains, "top2")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "ls", "--filter", "mode=replicated")
	assert.NilError(c, err, out)
	c.Assert(out, checker.Contains, "top1")
	c.Assert(out, checker.Not(checker.Contains), "top2")
}

func (s *DockerSwarmSuite) TestSwarmInitUnspecifiedDataPathAddr(c *check.C) {
	d := s.AddDaemon(c, false, false)

	out, err := d.Cmd("swarm", "init", "--data-path-addr", "0.0.0.0")
	assert.ErrorContains(c, err, "")
	c.Assert(out, checker.Contains, "data path address must be a non-zero IP")

	out, err = d.Cmd("swarm", "init", "--data-path-addr", "0.0.0.0:2000")
	assert.ErrorContains(c, err, "")
	c.Assert(out, checker.Contains, "data path address must be a non-zero IP")
}

func (s *DockerSwarmSuite) TestSwarmJoinLeave(c *check.C) {
	d := s.AddDaemon(c, true, true)

	out, err := d.Cmd("swarm", "join-token", "-q", "worker")
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	token := strings.TrimSpace(out)

	// Verify that back to back join/leave does not cause panics
	d1 := s.AddDaemon(c, false, false)
	for i := 0; i < 10; i++ {
		out, err = d1.Cmd("swarm", "join", "--token", token, d.SwarmListenAddr())
		assert.NilError(c, err)
		assert.Assert(c, strings.TrimSpace(out) != "")

		_, err = d1.Cmd("swarm", "leave")
		assert.NilError(c, err)
	}
}

const defaultRetryCount = 10

func waitForEvent(c *check.C, d *daemon.Daemon, since string, filter string, event string, retry int) string {
	if retry < 1 {
		c.Fatalf("retry count %d is invalid. It should be no less than 1", retry)
		return ""
	}
	var out string
	for i := 0; i < retry; i++ {
		until := daemonUnixTime(c)
		var err error
		if len(filter) > 0 {
			out, err = d.Cmd("events", "--since", since, "--until", until, filter)
		} else {
			out, err = d.Cmd("events", "--since", since, "--until", until)
		}
		assert.NilError(c, err, out)
		if strings.Contains(out, event) {
			return strings.TrimSpace(out)
		}
		// no need to sleep after last retry
		if i < retry-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	c.Fatalf("docker events output '%s' doesn't contain event '%s'", out, event)
	return ""
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsSource(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, true)
	d3 := s.AddDaemon(c, true, false)

	// create a network
	out, err := d1.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)
	c.Assert(networkID, checker.Not(checker.Equals), "")

	// d1, d2 are managers that can get swarm events
	waitForEvent(c, d1, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)
	waitForEvent(c, d2, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)

	// d3 is a worker, not able to get cluster events
	out = waitForEvent(c, d3, "0", "-f scope=swarm", "", 1)
	c.Assert(out, checker.Not(checker.Contains), "network create ")
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsScope(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// scope swarm filters cluster events
	out = waitForEvent(c, d, "0", "-f scope=swarm", "service create "+serviceID, defaultRetryCount)
	c.Assert(out, checker.Not(checker.Contains), "container create ")

	// all events are returned if scope is not specified
	waitForEvent(c, d, "0", "", "service create "+serviceID, 1)
	waitForEvent(c, d, "0", "", "container create ", defaultRetryCount)

	// scope local only shows non-cluster events
	out = waitForEvent(c, d, "0", "-f scope=local", "container create ", 1)
	c.Assert(out, checker.Not(checker.Contains), "service create ")
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsType(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// create a network
	out, err = d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)
	c.Assert(networkID, checker.Not(checker.Equals), "")

	// filter by service
	out = waitForEvent(c, d, "0", "-f type=service", "service create "+serviceID, defaultRetryCount)
	c.Assert(out, checker.Not(checker.Contains), "network create")

	// filter by network
	out = waitForEvent(c, d, "0", "-f type=network", "network create "+networkID, defaultRetryCount)
	c.Assert(out, checker.Not(checker.Contains), "service create")
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsService(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// validate service create event
	waitForEvent(c, d, "0", "-f scope=swarm", "service create "+serviceID, defaultRetryCount)

	t1 := daemonUnixTime(c)
	out, err = d.Cmd("service", "update", "--force", "--detach=false", "test")
	assert.NilError(c, err, out)

	// wait for service update start
	out = waitForEvent(c, d, t1, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	c.Assert(out, checker.Contains, "updatestate.new=updating")

	// allow service update complete. This is a service with 1 instance
	time.Sleep(400 * time.Millisecond)
	out = waitForEvent(c, d, t1, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	c.Assert(out, checker.Contains, "updatestate.new=completed, updatestate.old=updating")

	// scale service
	t2 := daemonUnixTime(c)
	out, err = d.Cmd("service", "scale", "test=3")
	assert.NilError(c, err, out)

	out = waitForEvent(c, d, t2, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	c.Assert(out, checker.Contains, "replicas.new=3, replicas.old=1")

	// remove service
	t3 := daemonUnixTime(c)
	out, err = d.Cmd("service", "rm", "test")
	assert.NilError(c, err, out)

	waitForEvent(c, d, t3, "-f scope=swarm", "service remove "+serviceID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsNode(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	s.AddDaemon(c, true, true)
	d3 := s.AddDaemon(c, true, true)

	d3ID := d3.NodeID()
	waitForEvent(c, d1, "0", "-f scope=swarm", "node create "+d3ID, defaultRetryCount)

	t1 := daemonUnixTime(c)
	out, err := d1.Cmd("node", "update", "--availability=pause", d3ID)
	assert.NilError(c, err, out)

	// filter by type
	out = waitForEvent(c, d1, t1, "-f type=node", "node update "+d3ID, defaultRetryCount)
	c.Assert(out, checker.Contains, "availability.new=pause, availability.old=active")

	t2 := daemonUnixTime(c)
	out, err = d1.Cmd("node", "demote", d3ID)
	assert.NilError(c, err, out)

	waitForEvent(c, d1, t2, "-f type=node", "node update "+d3ID, defaultRetryCount)

	t3 := daemonUnixTime(c)
	out, err = d1.Cmd("node", "rm", "-f", d3ID)
	assert.NilError(c, err, out)

	// filter by scope
	waitForEvent(c, d1, t3, "-f scope=swarm", "node remove "+d3ID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsNetwork(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// create a network
	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)

	waitForEvent(c, d, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)

	// remove network
	t1 := daemonUnixTime(c)
	out, err = d.Cmd("network", "rm", "foo")
	assert.NilError(c, err, out)

	// filtered by network
	waitForEvent(c, d, t1, "-f type=network", "network remove "+networkID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsSecret(c *check.C) {
	d := s.AddDaemon(c, true, true)

	testName := "test_secret"
	id := d.CreateSecret(c, swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	c.Assert(id, checker.Not(checker.Equals), "", check.Commentf("secrets: %s", id))

	waitForEvent(c, d, "0", "-f scope=swarm", "secret create "+id, defaultRetryCount)

	t1 := daemonUnixTime(c)
	d.DeleteSecret(c, id)
	// filtered by secret
	waitForEvent(c, d, t1, "-f type=secret", "secret remove "+id, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsConfig(c *check.C) {
	d := s.AddDaemon(c, true, true)

	testName := "test_config"
	id := d.CreateConfig(c, swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	c.Assert(id, checker.Not(checker.Equals), "", check.Commentf("configs: %s", id))

	waitForEvent(c, d, "0", "-f scope=swarm", "config create "+id, defaultRetryCount)

	t1 := daemonUnixTime(c)
	d.DeleteConfig(c, id)
	// filtered by config
	waitForEvent(c, d, t1, "-f type=config", "config remove "+id, defaultRetryCount)
}

func getUnlockKey(d *daemon.Daemon, c *check.C, autolockOutput string) string {
	unlockKey, err := d.Cmd("swarm", "unlock-key", "-q")
	c.Assert(err, checker.IsNil, check.Commentf("%s", unlockKey))
	unlockKey = strings.TrimSuffix(unlockKey, "\n")

	// Check that "docker swarm init --autolock" or "docker swarm update --autolock"
	// contains all the expected strings, including the unlock key
	c.Assert(autolockOutput, checker.Contains, "docker swarm unlock")
	c.Assert(autolockOutput, checker.Contains, unlockKey)

	return unlockKey
}

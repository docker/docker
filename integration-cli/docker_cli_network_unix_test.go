// +build !windows

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/versions/v1p20"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/runconfig"
	testdaemon "github.com/docker/docker/testutil/daemon"
	"github.com/docker/libnetwork/driverapi"
	remoteapi "github.com/docker/libnetwork/drivers/remote/api"
	"github.com/docker/libnetwork/ipamapi"
	remoteipam "github.com/docker/libnetwork/ipams/remote/api"
	"github.com/docker/libnetwork/netlabel"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

const dummyNetworkDriver = "dummy-network-driver"
const dummyIPAMDriver = "dummy-ipam-driver"

var remoteDriverNetworkRequest remoteapi.CreateNetworkRequest

func (s *DockerNetworkSuite) SetUpTest(c *testing.T) {
	s.d = daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
}

func (s *DockerNetworkSuite) TearDownTest(c *testing.T) {
	if s.d != nil {
		s.d.Stop(c)
		s.ds.TearDownTest(c)
	}
}

func (s *DockerNetworkSuite) SetUpSuite(c *testing.T) {
	mux := http.NewServeMux()
	s.server = httptest.NewServer(mux)
	assert.Assert(c, s.server != nil, "Failed to start an HTTP Server")
	setupRemoteNetworkDrivers(c, mux, s.server.URL, dummyNetworkDriver, dummyIPAMDriver)
}

func setupRemoteNetworkDrivers(c *testing.T, mux *http.ServeMux, url, netDrv, ipamDrv string) {

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"Implements": ["%s", "%s"]}`, driverapi.NetworkPluginEndpointType, ipamapi.PluginEndpointType)
	})

	// Network driver implementation
	mux.HandleFunc(fmt.Sprintf("/%s.GetCapabilities", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		fmt.Fprintf(w, `{"Scope":"local"}`)
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

func (s *DockerNetworkSuite) TearDownSuite(c *testing.T) {
	if s.server == nil {
		return
	}

	s.server.Close()

	err := os.RemoveAll("/etc/docker/plugins")
	assert.NilError(c, err)
}

func assertNwIsAvailable(c *testing.T, name string) {
	if !isNwPresent(c, name) {
		c.Fatalf("Network %s not found in network ls o/p", name)
	}
}

func assertNwNotAvailable(c *testing.T, name string) {
	if isNwPresent(c, name) {
		c.Fatalf("Found network %s in network ls o/p", name)
	}
}

func isNwPresent(c *testing.T, name string) bool {
	out, _ := dockerCmd(c, "network", "ls")
	lines := strings.Split(out, "\n")
	for i := 1; i < len(lines)-1; i++ {
		netFields := strings.Fields(lines[i])
		if netFields[1] == name {
			return true
		}
	}
	return false
}

// assertNwList checks network list retrieved with ls command
// equals to expected network list
// note: out should be `network ls [option]` result
func assertNwList(c *testing.T, out string, expectNws []string) {
	lines := strings.Split(out, "\n")
	var nwList []string
	for _, line := range lines[1 : len(lines)-1] {
		netFields := strings.Fields(line)
		// wrap all network name in nwList
		nwList = append(nwList, netFields[1])
	}

	// network ls should contains all expected networks
	assert.DeepEqual(c, nwList, expectNws)
}

func getNwResource(c *testing.T, name string) *types.NetworkResource {
	out, _ := dockerCmd(c, "network", "inspect", name)
	var nr []types.NetworkResource
	err := json.Unmarshal([]byte(out), &nr)
	assert.NilError(c, err)
	return &nr[0]
}

func (s *DockerNetworkSuite) TestDockerNetworkLsDefault(c *testing.T) {
	defaults := []string{"bridge", "host", "none"}
	for _, nn := range defaults {
		assertNwIsAvailable(c, nn)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkCreatePredefined(c *testing.T) {
	predefined := []string{"bridge", "host", "none", "default"}
	for _, net := range predefined {
		// predefined networks can't be created again
		out, _, err := dockerCmdWithError("network", "create", net)
		assert.ErrorContains(c, err, "", out)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkCreateHostBind(c *testing.T) {
	dockerCmd(c, "network", "create", "--subnet=192.168.10.0/24", "--gateway=192.168.10.1", "-o", "com.docker.network.bridge.host_binding_ipv4=192.168.10.1", "testbind")
	assertNwIsAvailable(c, "testbind")

	out := runSleepingContainer(c, "--net=testbind", "-p", "5000:5000")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))
	out, _ = dockerCmd(c, "ps")
	assert.Assert(c, strings.Contains(out, "192.168.10.1:5000->5000/tcp"))
}

func (s *DockerNetworkSuite) TestDockerNetworkRmPredefined(c *testing.T) {
	predefined := []string{"bridge", "host", "none", "default"}
	for _, net := range predefined {
		// predefined networks can't be removed
		out, _, err := dockerCmdWithError("network", "rm", net)
		assert.ErrorContains(c, err, "", out)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkLsFilter(c *testing.T) {
	testRequires(c, OnlyDefaultNetworks)
	testNet := "testnet1"
	testLabel := "foo"
	testValue := "bar"
	out, _ := dockerCmd(c, "network", "create", "dev")
	defer func() {
		dockerCmd(c, "network", "rm", "dev")
		dockerCmd(c, "network", "rm", testNet)
	}()
	networkID := strings.TrimSpace(out)

	// filter with partial ID
	// only show 'dev' network
	out, _ = dockerCmd(c, "network", "ls", "-f", "id="+networkID[0:5])
	assertNwList(c, out, []string{"dev"})

	out, _ = dockerCmd(c, "network", "ls", "-f", "name=dge")
	assertNwList(c, out, []string{"bridge"})

	// only show built-in network (bridge, none, host)
	out, _ = dockerCmd(c, "network", "ls", "-f", "type=builtin")
	assertNwList(c, out, []string{"bridge", "host", "none"})

	// only show custom networks (dev)
	out, _ = dockerCmd(c, "network", "ls", "-f", "type=custom")
	assertNwList(c, out, []string{"dev"})

	// show all networks with filter
	// it should be equivalent of ls without option
	out, _ = dockerCmd(c, "network", "ls", "-f", "type=custom", "-f", "type=builtin")
	assertNwList(c, out, []string{"bridge", "dev", "host", "none"})

	dockerCmd(c, "network", "create", "--label", testLabel+"="+testValue, testNet)
	assertNwIsAvailable(c, testNet)

	out, _ = dockerCmd(c, "network", "ls", "-f", "label="+testLabel)
	assertNwList(c, out, []string{testNet})

	out, _ = dockerCmd(c, "network", "ls", "-f", "label="+testLabel+"="+testValue)
	assertNwList(c, out, []string{testNet})

	out, _ = dockerCmd(c, "network", "ls", "-f", "label=nonexistent")
	outArr := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(c, len(outArr), 1, fmt.Sprintf("%s\n", out))

	out, _ = dockerCmd(c, "network", "ls", "-f", "driver=null")
	assertNwList(c, out, []string{"none"})

	out, _ = dockerCmd(c, "network", "ls", "-f", "driver=host")
	assertNwList(c, out, []string{"host"})

	out, _ = dockerCmd(c, "network", "ls", "-f", "driver=bridge")
	assertNwList(c, out, []string{"bridge", "dev", testNet})
}

func (s *DockerNetworkSuite) TestDockerNetworkCreateDelete(c *testing.T) {
	dockerCmd(c, "network", "create", "test")
	assertNwIsAvailable(c, "test")

	dockerCmd(c, "network", "rm", "test")
	assertNwNotAvailable(c, "test")
}

func (s *DockerNetworkSuite) TestDockerNetworkCreateLabel(c *testing.T) {
	testNet := "testnetcreatelabel"
	testLabel := "foo"
	testValue := "bar"

	dockerCmd(c, "network", "create", "--label", testLabel+"="+testValue, testNet)
	assertNwIsAvailable(c, testNet)

	out, _, err := dockerCmdWithError("network", "inspect", "--format={{ .Labels."+testLabel+" }}", testNet)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), testValue)

	dockerCmd(c, "network", "rm", testNet)
	assertNwNotAvailable(c, testNet)
}

func (s *DockerSuite) TestDockerNetworkDeleteNotExists(c *testing.T) {
	out, _, err := dockerCmdWithError("network", "rm", "test")
	assert.ErrorContains(c, err, "", out)
}

func (s *DockerSuite) TestDockerNetworkDeleteMultiple(c *testing.T) {
	dockerCmd(c, "network", "create", "testDelMulti0")
	assertNwIsAvailable(c, "testDelMulti0")
	dockerCmd(c, "network", "create", "testDelMulti1")
	assertNwIsAvailable(c, "testDelMulti1")
	dockerCmd(c, "network", "create", "testDelMulti2")
	assertNwIsAvailable(c, "testDelMulti2")
	out, _ := dockerCmd(c, "run", "-d", "--net", "testDelMulti2", "busybox", "top")
	containerID := strings.TrimSpace(out)
	waitRun(containerID)

	// delete three networks at the same time, since testDelMulti2
	// contains active container, its deletion should fail.
	out, _, err := dockerCmdWithError("network", "rm", "testDelMulti0", "testDelMulti1", "testDelMulti2")
	// err should not be nil due to deleting testDelMulti2 failed.
	assert.Assert(c, err != nil, "out: %s", out)
	// testDelMulti2 should fail due to network has active endpoints
	assert.Assert(c, strings.Contains(out, "has active endpoints"))
	assertNwNotAvailable(c, "testDelMulti0")
	assertNwNotAvailable(c, "testDelMulti1")
	// testDelMulti2 can't be deleted, so it should exist
	assertNwIsAvailable(c, "testDelMulti2")
}

func (s *DockerSuite) TestDockerNetworkInspect(c *testing.T) {
	out, _ := dockerCmd(c, "network", "inspect", "host")
	var networkResources []types.NetworkResource
	err := json.Unmarshal([]byte(out), &networkResources)
	assert.NilError(c, err)
	assert.Equal(c, len(networkResources), 1)

	out, _ = dockerCmd(c, "network", "inspect", "--format={{ .Name }}", "host")
	assert.Equal(c, strings.TrimSpace(out), "host")
}

func (s *DockerSuite) TestDockerNetworkInspectWithID(c *testing.T) {
	out, _ := dockerCmd(c, "network", "create", "test2")
	networkID := strings.TrimSpace(out)
	assertNwIsAvailable(c, "test2")
	out, _ = dockerCmd(c, "network", "inspect", "--format={{ .Id }}", "test2")
	assert.Equal(c, strings.TrimSpace(out), networkID)

	out, _ = dockerCmd(c, "network", "inspect", "--format={{ .ID }}", "test2")
	assert.Equal(c, strings.TrimSpace(out), networkID)
}

func (s *DockerSuite) TestDockerInspectMultipleNetwork(c *testing.T) {
	result := dockerCmdWithResult("network", "inspect", "host", "none")
	result.Assert(c, icmd.Success)

	var networkResources []types.NetworkResource
	err := json.Unmarshal([]byte(result.Stdout()), &networkResources)
	assert.NilError(c, err)
	assert.Equal(c, len(networkResources), 2)
}

func (s *DockerSuite) TestDockerInspectMultipleNetworksIncludingNonexistent(c *testing.T) {
	// non-existent network was not at the beginning of the inspect list
	// This should print an error, return an exitCode 1 and print the host network
	result := dockerCmdWithResult("network", "inspect", "host", "nonexistent")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "Error: No such network: nonexistent",
		Out:      "host",
	})

	var networkResources []types.NetworkResource
	err := json.Unmarshal([]byte(result.Stdout()), &networkResources)
	assert.NilError(c, err)
	assert.Equal(c, len(networkResources), 1)

	// Only one non-existent network to inspect
	// Should print an error and return an exitCode, nothing else
	result = dockerCmdWithResult("network", "inspect", "nonexistent")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "Error: No such network: nonexistent",
		Out:      "[]",
	})

	// non-existent network was at the beginning of the inspect list
	// Should not fail fast, and still print host network but print an error
	result = dockerCmdWithResult("network", "inspect", "nonexistent", "host")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "Error: No such network: nonexistent",
		Out:      "host",
	})

	networkResources = []types.NetworkResource{}
	err = json.Unmarshal([]byte(result.Stdout()), &networkResources)
	assert.NilError(c, err)
	assert.Equal(c, len(networkResources), 1)
}

func (s *DockerSuite) TestDockerInspectNetworkWithContainerName(c *testing.T) {
	dockerCmd(c, "network", "create", "brNetForInspect")
	assertNwIsAvailable(c, "brNetForInspect")
	defer func() {
		dockerCmd(c, "network", "rm", "brNetForInspect")
		assertNwNotAvailable(c, "brNetForInspect")
	}()

	out, _ := dockerCmd(c, "run", "-d", "--name", "testNetInspect1", "--net", "brNetForInspect", "busybox", "top")
	assert.Assert(c, waitRun("testNetInspect1") == nil)
	containerID := strings.TrimSpace(out)
	defer func() {
		// we don't stop container by name, because we'll rename it later
		dockerCmd(c, "stop", containerID)
	}()

	out, _ = dockerCmd(c, "network", "inspect", "brNetForInspect")
	var networkResources []types.NetworkResource
	err := json.Unmarshal([]byte(out), &networkResources)
	assert.NilError(c, err)
	assert.Equal(c, len(networkResources), 1)
	container, ok := networkResources[0].Containers[containerID]
	assert.Assert(c, ok)
	assert.Equal(c, container.Name, "testNetInspect1")

	// rename container and check docker inspect output update
	newName := "HappyNewName"
	dockerCmd(c, "rename", "testNetInspect1", newName)

	// check whether network inspect works properly
	out, _ = dockerCmd(c, "network", "inspect", "brNetForInspect")
	var newNetRes []types.NetworkResource
	err = json.Unmarshal([]byte(out), &newNetRes)
	assert.NilError(c, err)
	assert.Equal(c, len(newNetRes), 1)
	container1, ok := newNetRes[0].Containers[containerID]
	assert.Assert(c, ok)
	assert.Equal(c, container1.Name, newName)
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectDisconnect(c *testing.T) {
	dockerCmd(c, "network", "create", "test")
	assertNwIsAvailable(c, "test")
	nr := getNwResource(c, "test")

	assert.Equal(c, nr.Name, "test")
	assert.Equal(c, len(nr.Containers), 0)

	// run a container
	out, _ := dockerCmd(c, "run", "-d", "--name", "test", "busybox", "top")
	assert.Assert(c, waitRun("test") == nil)
	containerID := strings.TrimSpace(out)

	// connect the container to the test network
	dockerCmd(c, "network", "connect", "test", containerID)

	// inspect the network to make sure container is connected
	nr = getNetworkResource(c, nr.ID)
	assert.Equal(c, len(nr.Containers), 1)

	// check if container IP matches network inspect
	ip, _, err := net.ParseCIDR(nr.Containers[containerID].IPv4Address)
	assert.NilError(c, err)
	containerIP := findContainerIP(c, "test", "test")
	assert.Equal(c, ip.String(), containerIP)

	// disconnect container from the network
	dockerCmd(c, "network", "disconnect", "test", containerID)
	nr = getNwResource(c, "test")
	assert.Equal(c, nr.Name, "test")
	assert.Equal(c, len(nr.Containers), 0)

	// run another container
	out, _ = dockerCmd(c, "run", "-d", "--net", "test", "--name", "test2", "busybox", "top")
	assert.Assert(c, waitRun("test2") == nil)
	containerID = strings.TrimSpace(out)

	nr = getNwResource(c, "test")
	assert.Equal(c, nr.Name, "test")
	assert.Equal(c, len(nr.Containers), 1)

	// force disconnect the container to the test network
	dockerCmd(c, "network", "disconnect", "-f", "test", containerID)

	nr = getNwResource(c, "test")
	assert.Equal(c, nr.Name, "test")
	assert.Equal(c, len(nr.Containers), 0)

	dockerCmd(c, "network", "rm", "test")
	assertNwNotAvailable(c, "test")
}

func (s *DockerNetworkSuite) TestDockerNetworkIPAMMultipleNetworks(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// test0 bridge network
	dockerCmd(c, "network", "create", "--subnet=192.168.0.0/16", "test1")
	assertNwIsAvailable(c, "test1")

	// test2 bridge network does not overlap
	dockerCmd(c, "network", "create", "--subnet=192.169.0.0/16", "test2")
	assertNwIsAvailable(c, "test2")

	// for networks w/o ipam specified, docker will choose proper non-overlapping subnets
	dockerCmd(c, "network", "create", "test3")
	assertNwIsAvailable(c, "test3")
	dockerCmd(c, "network", "create", "test4")
	assertNwIsAvailable(c, "test4")
	dockerCmd(c, "network", "create", "test5")
	assertNwIsAvailable(c, "test5")

	// test network with multiple subnets
	// bridge network doesn't support multiple subnets. hence, use a dummy driver that supports

	dockerCmd(c, "network", "create", "-d", dummyNetworkDriver, "--subnet=192.170.0.0/16", "--subnet=192.171.0.0/16", "test6")
	assertNwIsAvailable(c, "test6")

	// test network with multiple subnets with valid ipam combinations
	// also check same subnet across networks when the driver supports it.
	dockerCmd(c, "network", "create", "-d", dummyNetworkDriver,
		"--subnet=192.172.0.0/16", "--subnet=192.173.0.0/16",
		"--gateway=192.172.0.100", "--gateway=192.173.0.100",
		"--ip-range=192.172.1.0/24",
		"--aux-address", "a=192.172.1.5", "--aux-address", "b=192.172.1.6",
		"--aux-address", "c=192.173.1.5", "--aux-address", "d=192.173.1.6",
		"test7")
	assertNwIsAvailable(c, "test7")

	// cleanup
	for i := 1; i < 8; i++ {
		dockerCmd(c, "network", "rm", fmt.Sprintf("test%d", i))
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkCustomIPAM(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Create a bridge network using custom ipam driver
	dockerCmd(c, "network", "create", "--ipam-driver", dummyIPAMDriver, "br0")
	assertNwIsAvailable(c, "br0")

	// Verify expected network ipam fields are there
	nr := getNetworkResource(c, "br0")
	assert.Equal(c, nr.Driver, "bridge")
	assert.Equal(c, nr.IPAM.Driver, dummyIPAMDriver)

	// remove network and exercise remote ipam driver
	dockerCmd(c, "network", "rm", "br0")
	assertNwNotAvailable(c, "br0")
}

func (s *DockerNetworkSuite) TestDockerNetworkIPAMOptions(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Create a bridge network using custom ipam driver and options
	dockerCmd(c, "network", "create", "--ipam-driver", dummyIPAMDriver, "--ipam-opt", "opt1=drv1", "--ipam-opt", "opt2=drv2", "br0")
	assertNwIsAvailable(c, "br0")

	// Verify expected network ipam options
	nr := getNetworkResource(c, "br0")
	opts := nr.IPAM.Options
	assert.Equal(c, opts["opt1"], "drv1")
	assert.Equal(c, opts["opt2"], "drv2")
}

func (s *DockerNetworkSuite) TestDockerNetworkNullIPAMDriver(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Create a network with null ipam driver
	_, _, err := dockerCmdWithError("network", "create", "-d", dummyNetworkDriver, "--ipam-driver", "null", "test000")
	assert.NilError(c, err)
	assertNwIsAvailable(c, "test000")

	// Verify the inspect data contains the default subnet provided by the null
	// ipam driver and no gateway, as the null ipam driver does not provide one
	nr := getNetworkResource(c, "test000")
	assert.Equal(c, nr.IPAM.Driver, "null")
	assert.Equal(c, len(nr.IPAM.Config), 1)
	assert.Equal(c, nr.IPAM.Config[0].Subnet, "0.0.0.0/0")
	assert.Equal(c, nr.IPAM.Config[0].Gateway, "")
}

func (s *DockerNetworkSuite) TestDockerNetworkInspectDefault(c *testing.T) {
	nr := getNetworkResource(c, "none")
	assert.Equal(c, nr.Driver, "null")
	assert.Equal(c, nr.Scope, "local")
	assert.Equal(c, nr.Internal, false)
	assert.Equal(c, nr.EnableIPv6, false)
	assert.Equal(c, nr.IPAM.Driver, "default")
	assert.Equal(c, len(nr.IPAM.Config), 0)

	nr = getNetworkResource(c, "host")
	assert.Equal(c, nr.Driver, "host")
	assert.Equal(c, nr.Scope, "local")
	assert.Equal(c, nr.Internal, false)
	assert.Equal(c, nr.EnableIPv6, false)
	assert.Equal(c, nr.IPAM.Driver, "default")
	assert.Equal(c, len(nr.IPAM.Config), 0)

	nr = getNetworkResource(c, "bridge")
	assert.Equal(c, nr.Driver, "bridge")
	assert.Equal(c, nr.Scope, "local")
	assert.Equal(c, nr.Internal, false)
	assert.Equal(c, nr.EnableIPv6, false)
	assert.Equal(c, nr.IPAM.Driver, "default")
	assert.Equal(c, len(nr.IPAM.Config), 1)
}

func (s *DockerNetworkSuite) TestDockerNetworkInspectCustomUnspecified(c *testing.T) {
	// if unspecified, network subnet will be selected from inside preferred pool
	dockerCmd(c, "network", "create", "test01")
	assertNwIsAvailable(c, "test01")

	nr := getNetworkResource(c, "test01")
	assert.Equal(c, nr.Driver, "bridge")
	assert.Equal(c, nr.Scope, "local")
	assert.Equal(c, nr.Internal, false)
	assert.Equal(c, nr.EnableIPv6, false)
	assert.Equal(c, nr.IPAM.Driver, "default")
	assert.Equal(c, len(nr.IPAM.Config), 1)

	dockerCmd(c, "network", "rm", "test01")
	assertNwNotAvailable(c, "test01")
}

func (s *DockerNetworkSuite) TestDockerNetworkInspectCustomSpecified(c *testing.T) {
	dockerCmd(c, "network", "create", "--driver=bridge", "--ipv6", "--subnet=fd80:24e2:f998:72d6::/64", "--subnet=172.28.0.0/16", "--ip-range=172.28.5.0/24", "--gateway=172.28.5.254", "br0")
	assertNwIsAvailable(c, "br0")

	nr := getNetworkResource(c, "br0")
	assert.Equal(c, nr.Driver, "bridge")
	assert.Equal(c, nr.Scope, "local")
	assert.Equal(c, nr.Internal, false)
	assert.Equal(c, nr.EnableIPv6, true)
	assert.Equal(c, nr.IPAM.Driver, "default")
	assert.Equal(c, len(nr.IPAM.Config), 2)
	assert.Equal(c, nr.IPAM.Config[0].Subnet, "172.28.0.0/16")
	assert.Equal(c, nr.IPAM.Config[0].IPRange, "172.28.5.0/24")
	assert.Equal(c, nr.IPAM.Config[0].Gateway, "172.28.5.254")
	assert.Equal(c, nr.Internal, false)
	dockerCmd(c, "network", "rm", "br0")
	assertNwNotAvailable(c, "br0")
}

func (s *DockerNetworkSuite) TestDockerNetworkIPAMInvalidCombinations(c *testing.T) {
	// network with ip-range out of subnet range
	_, _, err := dockerCmdWithError("network", "create", "--subnet=192.168.0.0/16", "--ip-range=192.170.0.0/16", "test")
	assert.ErrorContains(c, err, "")

	// network with multiple gateways for a single subnet
	_, _, err = dockerCmdWithError("network", "create", "--subnet=192.168.0.0/16", "--gateway=192.168.0.1", "--gateway=192.168.0.2", "test")
	assert.ErrorContains(c, err, "")

	// Multiple overlapping subnets in the same network must fail
	_, _, err = dockerCmdWithError("network", "create", "--subnet=192.168.0.0/16", "--subnet=192.168.1.0/16", "test")
	assert.ErrorContains(c, err, "")

	// overlapping subnets across networks must fail
	// create a valid test0 network
	dockerCmd(c, "network", "create", "--subnet=192.168.0.0/16", "test0")
	assertNwIsAvailable(c, "test0")
	// create an overlapping test1 network
	_, _, err = dockerCmdWithError("network", "create", "--subnet=192.168.128.0/17", "test1")
	assert.ErrorContains(c, err, "")
	dockerCmd(c, "network", "rm", "test0")
	assertNwNotAvailable(c, "test0")
}

func (s *DockerNetworkSuite) TestDockerNetworkDriverOptions(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	dockerCmd(c, "network", "create", "-d", dummyNetworkDriver, "-o", "opt1=drv1", "-o", "opt2=drv2", "testopt")
	assertNwIsAvailable(c, "testopt")
	gopts := remoteDriverNetworkRequest.Options[netlabel.GenericData]
	assert.Assert(c, gopts != nil)
	opts, ok := gopts.(map[string]interface{})
	assert.Equal(c, ok, true)
	assert.Equal(c, opts["opt1"], "drv1")
	assert.Equal(c, opts["opt2"], "drv2")
	dockerCmd(c, "network", "rm", "testopt")
	assertNwNotAvailable(c, "testopt")

}

func (s *DockerNetworkSuite) TestDockerPluginV2NetworkDriver(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)

	var (
		npName        = "tiborvass/test-docker-netplugin"
		npTag         = "latest"
		npNameWithTag = npName + ":" + npTag
	)
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", npNameWithTag)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("plugin", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, npName))
	assert.Assert(c, strings.Contains(out, npTag))
	assert.Assert(c, strings.Contains(out, "true"))
	dockerCmd(c, "network", "create", "-d", npNameWithTag, "v2net")
	assertNwIsAvailable(c, "v2net")
	dockerCmd(c, "network", "rm", "v2net")
	assertNwNotAvailable(c, "v2net")

}

func (s *DockerDaemonSuite) TestDockerNetworkNoDiscoveryDefaultBridgeNetwork(c *testing.T) {
	// On default bridge network built-in service discovery should not happen
	hostsFile := "/etc/hosts"
	bridgeName := "external-bridge"
	bridgeIP := "192.169.255.254/24"
	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	s.d.StartWithBusybox(c, "--bridge", bridgeName)
	defer s.d.Restart(c)

	// run two containers and store first container's etc/hosts content
	out, err := s.d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err)
	cid1 := strings.TrimSpace(out)
	defer s.d.Cmd("stop", cid1)

	hosts, err := s.d.Cmd("exec", cid1, "cat", hostsFile)
	assert.NilError(c, err)

	out, err = s.d.Cmd("run", "-d", "--name", "container2", "busybox", "top")
	assert.NilError(c, err)
	cid2 := strings.TrimSpace(out)

	// verify first container's etc/hosts file has not changed after spawning the second named container
	hostsPost, err := s.d.Cmd("exec", cid1, "cat", hostsFile)
	assert.NilError(c, err)
	assert.Equal(c, hosts, hostsPost, fmt.Sprintf("Unexpected %s change on second container creation", hostsFile))
	// stop container 2 and verify first container's etc/hosts has not changed
	_, err = s.d.Cmd("stop", cid2)
	assert.NilError(c, err)

	hostsPost, err = s.d.Cmd("exec", cid1, "cat", hostsFile)
	assert.NilError(c, err)
	assert.Equal(c, hosts, hostsPost, fmt.Sprintf("Unexpected %s change on second container creation", hostsFile))
	// but discovery is on when connecting to non default bridge network
	network := "anotherbridge"
	out, err = s.d.Cmd("network", "create", network)
	assert.NilError(c, err, out)
	defer s.d.Cmd("network", "rm", network)

	out, err = s.d.Cmd("network", "connect", network, cid1)
	assert.NilError(c, err, out)

	hosts, err = s.d.Cmd("exec", cid1, "cat", hostsFile)
	assert.NilError(c, err)

	hostsPost, err = s.d.Cmd("exec", cid1, "cat", hostsFile)
	assert.NilError(c, err)
	assert.Equal(c, hosts, hostsPost, fmt.Sprintf("Unexpected %s change on second network connection", hostsFile))
}

func (s *DockerNetworkSuite) TestDockerNetworkAnonymousEndpoint(c *testing.T) {
	testRequires(c, NotArm)
	hostsFile := "/etc/hosts"
	cstmBridgeNw := "custom-bridge-nw"
	cstmBridgeNw1 := "custom-bridge-nw1"

	dockerCmd(c, "network", "create", "-d", "bridge", cstmBridgeNw)
	assertNwIsAvailable(c, cstmBridgeNw)

	// run two anonymous containers and store their etc/hosts content
	out, _ := dockerCmd(c, "run", "-d", "--net", cstmBridgeNw, "busybox", "top")
	cid1 := strings.TrimSpace(out)

	hosts1 := readContainerFileWithExec(c, cid1, hostsFile)

	out, _ = dockerCmd(c, "run", "-d", "--net", cstmBridgeNw, "busybox", "top")
	cid2 := strings.TrimSpace(out)

	// verify first container etc/hosts file has not changed
	hosts1post := readContainerFileWithExec(c, cid1, hostsFile)
	assert.Equal(c, string(hosts1), string(hosts1post), fmt.Sprintf("Unexpected %s change on anonymous container creation", hostsFile))
	// Connect the 2nd container to a new network and verify the
	// first container /etc/hosts file still hasn't changed.
	dockerCmd(c, "network", "create", "-d", "bridge", cstmBridgeNw1)
	assertNwIsAvailable(c, cstmBridgeNw1)

	dockerCmd(c, "network", "connect", cstmBridgeNw1, cid2)

	hosts2 := readContainerFileWithExec(c, cid2, hostsFile)
	hosts1post = readContainerFileWithExec(c, cid1, hostsFile)
	assert.Equal(c, string(hosts1), string(hosts1post), fmt.Sprintf("Unexpected %s change on container connect", hostsFile))
	// start a named container
	cName := "AnyName"
	out, _ = dockerCmd(c, "run", "-d", "--net", cstmBridgeNw, "--name", cName, "busybox", "top")
	cid3 := strings.TrimSpace(out)

	// verify that container 1 and 2 can ping the named container
	dockerCmd(c, "exec", cid1, "ping", "-c", "1", cName)
	dockerCmd(c, "exec", cid2, "ping", "-c", "1", cName)

	// Stop named container and verify first two containers' etc/hosts file hasn't changed
	dockerCmd(c, "stop", cid3)
	hosts1post = readContainerFileWithExec(c, cid1, hostsFile)
	assert.Equal(c, string(hosts1), string(hosts1post), fmt.Sprintf("Unexpected %s change on name container creation", hostsFile))
	hosts2post := readContainerFileWithExec(c, cid2, hostsFile)
	assert.Equal(c, string(hosts2), string(hosts2post), fmt.Sprintf("Unexpected %s change on name container creation", hostsFile))
	// verify that container 1 and 2 can't ping the named container now
	_, _, err := dockerCmdWithError("exec", cid1, "ping", "-c", "1", cName)
	assert.ErrorContains(c, err, "")
	_, _, err = dockerCmdWithError("exec", cid2, "ping", "-c", "1", cName)
	assert.ErrorContains(c, err, "")
}

func (s *DockerNetworkSuite) TestDockerNetworkLinkOnDefaultNetworkOnly(c *testing.T) {
	// Legacy Link feature must work only on default network, and not across networks
	cnt1 := "container1"
	cnt2 := "container2"
	network := "anotherbridge"

	// Run first container on default network
	dockerCmd(c, "run", "-d", "--name", cnt1, "busybox", "top")

	// Create another network and run the second container on it
	dockerCmd(c, "network", "create", network)
	assertNwIsAvailable(c, network)
	dockerCmd(c, "run", "-d", "--net", network, "--name", cnt2, "busybox", "top")

	// Try launching a container on default network, linking to the first container. Must succeed
	dockerCmd(c, "run", "-d", "--link", fmt.Sprintf("%s:%s", cnt1, cnt1), "busybox", "top")

	// Try launching a container on default network, linking to the second container. Must fail
	_, _, err := dockerCmdWithError("run", "-d", "--link", fmt.Sprintf("%s:%s", cnt2, cnt2), "busybox", "top")
	assert.ErrorContains(c, err, "")

	// Connect second container to default network. Now a container on default network can link to it
	dockerCmd(c, "network", "connect", "bridge", cnt2)
	dockerCmd(c, "run", "-d", "--link", fmt.Sprintf("%s:%s", cnt2, cnt2), "busybox", "top")
}

func (s *DockerNetworkSuite) TestDockerNetworkOverlayPortMapping(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Verify exposed ports are present in ps output when running a container on
	// a network managed by a driver which does not provide the default gateway
	// for the container
	nwn := "ov"
	ctn := "bb"
	port1 := 80
	port2 := 443
	expose1 := fmt.Sprintf("--expose=%d", port1)
	expose2 := fmt.Sprintf("--expose=%d", port2)

	dockerCmd(c, "network", "create", "-d", dummyNetworkDriver, nwn)
	assertNwIsAvailable(c, nwn)

	dockerCmd(c, "run", "-d", "--net", nwn, "--name", ctn, expose1, expose2, "busybox", "top")

	// Check docker ps o/p for last created container reports the unpublished ports
	unpPort1 := fmt.Sprintf("%d/tcp", port1)
	unpPort2 := fmt.Sprintf("%d/tcp", port2)
	out, _ := dockerCmd(c, "ps", "-n=1")
	// Missing unpublished ports in docker ps output
	assert.Assert(c, strings.Contains(out, unpPort1))
	// Missing unpublished ports in docker ps output
	assert.Assert(c, strings.Contains(out, unpPort2))
}

func (s *DockerNetworkSuite) TestDockerNetworkDriverUngracefulRestart(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace, testEnv.IsLocalDaemon)
	dnd := "dnd"
	did := "did"

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	setupRemoteNetworkDrivers(c, mux, server.URL, dnd, did)

	s.d.StartWithBusybox(c)
	_, err := s.d.Cmd("network", "create", "-d", dnd, "--subnet", "1.1.1.0/24", "net1")
	assert.NilError(c, err)

	_, err = s.d.Cmd("run", "-d", "--net", "net1", "--name", "foo", "--ip", "1.1.1.10", "busybox", "top")
	assert.NilError(c, err)

	// Kill daemon and restart
	assert.Assert(c, s.d.Kill() == nil)

	server.Close()

	startTime := time.Now().Unix()
	s.d.Restart(c)
	lapse := time.Now().Unix() - startTime
	if lapse > 60 {
		// In normal scenarios, daemon restart takes ~1 second.
		// Plugin retry mechanism can delay the daemon start. systemd may not like it.
		// Avoid accessing plugins during daemon bootup
		c.Logf("daemon restart took too long : %d seconds", lapse)
	}

	// Restart the custom dummy plugin
	mux = http.NewServeMux()
	server = httptest.NewServer(mux)
	setupRemoteNetworkDrivers(c, mux, server.URL, dnd, did)

	// trying to reuse the same ip must succeed
	_, err = s.d.Cmd("run", "-d", "--net", "net1", "--name", "bar", "--ip", "1.1.1.10", "busybox", "top")
	assert.NilError(c, err)
}

func (s *DockerNetworkSuite) TestDockerNetworkMacInspect(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Verify endpoint MAC address is correctly populated in container's network settings
	nwn := "ov"
	ctn := "bb"

	dockerCmd(c, "network", "create", "-d", dummyNetworkDriver, nwn)
	assertNwIsAvailable(c, nwn)

	dockerCmd(c, "run", "-d", "--net", nwn, "--name", ctn, "busybox", "top")

	mac := inspectField(c, ctn, "NetworkSettings.Networks."+nwn+".MacAddress")
	assert.Equal(c, mac, "a0:b1:c2:d3:e4:f5")
}

func (s *DockerSuite) TestInspectAPIMultipleNetworks(c *testing.T) {
	dockerCmd(c, "network", "create", "mybridge1")
	dockerCmd(c, "network", "create", "mybridge2")
	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	dockerCmd(c, "network", "connect", "mybridge1", id)
	dockerCmd(c, "network", "connect", "mybridge2", id)

	body := getInspectBody(c, "v1.20", id)
	var inspect120 v1p20.ContainerJSON
	err := json.Unmarshal(body, &inspect120)
	assert.NilError(c, err)

	versionedIP := inspect120.NetworkSettings.IPAddress

	body = getInspectBody(c, "v1.21", id)
	var inspect121 types.ContainerJSON
	err = json.Unmarshal(body, &inspect121)
	assert.NilError(c, err)
	assert.Equal(c, len(inspect121.NetworkSettings.Networks), 3)

	bridge := inspect121.NetworkSettings.Networks["bridge"]
	assert.Equal(c, bridge.IPAddress, versionedIP)
	assert.Equal(c, bridge.IPAddress, inspect121.NetworkSettings.IPAddress)
}

func connectContainerToNetworks(c *testing.T, d *daemon.Daemon, cName string, nws []string) {
	// Run a container on the default network
	out, err := d.Cmd("run", "-d", "--name", cName, "busybox", "top")
	assert.NilError(c, err, out)

	// Attach the container to other networks
	for _, nw := range nws {
		out, err = d.Cmd("network", "create", nw)
		assert.NilError(c, err, out)
		out, err = d.Cmd("network", "connect", nw, cName)
		assert.NilError(c, err, out)
	}
}

func verifyContainerIsConnectedToNetworks(c *testing.T, d *daemon.Daemon, cName string, nws []string) {
	// Verify container is connected to all the networks
	for _, nw := range nws {
		out, err := d.Cmd("inspect", "-f", fmt.Sprintf("{{.NetworkSettings.Networks.%s}}", nw), cName)
		assert.NilError(c, err, out)
		assert.Assert(c, out != "<no value>\n")
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkMultipleNetworksGracefulDaemonRestart(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	cName := "bb"
	nwList := []string{"nw1", "nw2", "nw3"}

	s.d.StartWithBusybox(c)

	connectContainerToNetworks(c, s.d, cName, nwList)
	verifyContainerIsConnectedToNetworks(c, s.d, cName, nwList)

	// Reload daemon
	s.d.Restart(c)

	_, err := s.d.Cmd("start", cName)
	assert.NilError(c, err)

	verifyContainerIsConnectedToNetworks(c, s.d, cName, nwList)
}

func (s *DockerNetworkSuite) TestDockerNetworkMultipleNetworksUngracefulDaemonRestart(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	cName := "cc"
	nwList := []string{"nw1", "nw2", "nw3"}

	s.d.StartWithBusybox(c)

	connectContainerToNetworks(c, s.d, cName, nwList)
	verifyContainerIsConnectedToNetworks(c, s.d, cName, nwList)

	// Kill daemon and restart
	assert.Assert(c, s.d.Kill() == nil)
	s.d.Restart(c)

	// Restart container
	_, err := s.d.Cmd("start", cName)
	assert.NilError(c, err)

	verifyContainerIsConnectedToNetworks(c, s.d, cName, nwList)
}

func (s *DockerNetworkSuite) TestDockerNetworkRunNetByID(c *testing.T) {
	out, _ := dockerCmd(c, "network", "create", "one")
	containerOut, _, err := dockerCmdWithError("run", "-d", "--net", strings.TrimSpace(out), "busybox", "top")
	assert.Assert(c, err == nil, containerOut)
}

func (s *DockerNetworkSuite) TestDockerNetworkHostModeUngracefulDaemonRestart(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(c)

	// Run a few containers on host network
	for i := 0; i < 10; i++ {
		cName := fmt.Sprintf("hostc-%d", i)
		out, err := s.d.Cmd("run", "-d", "--name", cName, "--net=host", "--restart=always", "busybox", "top")
		assert.NilError(c, err, out)

		// verify container has finished starting before killing daemon
		err = s.d.WaitRun(cName)
		assert.NilError(c, err)
	}

	// Kill daemon ungracefully and restart
	assert.Assert(c, s.d.Kill() == nil)
	s.d.Restart(c)

	// make sure all the containers are up and running
	for i := 0; i < 10; i++ {
		err := s.d.WaitRun(fmt.Sprintf("hostc-%d", i))
		assert.NilError(c, err)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectToHostFromOtherNetwork(c *testing.T) {
	dockerCmd(c, "run", "-d", "--name", "container1", "busybox", "top")
	assert.Assert(c, waitRun("container1") == nil)
	dockerCmd(c, "network", "disconnect", "bridge", "container1")
	out, _, err := dockerCmdWithError("network", "connect", "host", "container1")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrConflictHostNetwork.Error()))
}

func (s *DockerNetworkSuite) TestDockerNetworkDisconnectFromHost(c *testing.T) {
	dockerCmd(c, "run", "-d", "--name", "container1", "--net=host", "busybox", "top")
	assert.Assert(c, waitRun("container1") == nil)
	out, _, err := dockerCmdWithError("network", "disconnect", "host", "container1")
	assert.Assert(c, err != nil, "Should err out disconnect from host")
	assert.Assert(c, strings.Contains(out, runconfig.ErrConflictHostNetwork.Error()))
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectWithPortMapping(c *testing.T) {
	testRequires(c, NotArm)
	dockerCmd(c, "network", "create", "test1")
	dockerCmd(c, "run", "-d", "--name", "c1", "-p", "5000:5000", "busybox", "top")
	assert.Assert(c, waitRun("c1") == nil)
	dockerCmd(c, "network", "connect", "test1", "c1")
}

func verifyPortMap(c *testing.T, container, port, originalMapping string, mustBeEqual bool) {
	currentMapping, _ := dockerCmd(c, "port", container, port)
	if mustBeEqual {
		assert.Equal(c, currentMapping, originalMapping)
	} else {
		assert.Assert(c, currentMapping != originalMapping)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectDisconnectWithPortMapping(c *testing.T) {
	// Connect and disconnect a container with explicit and non-explicit
	// host port mapping to/from networks which do cause and do not cause
	// the container default gateway to change, and verify docker port cmd
	// returns congruent information
	testRequires(c, NotArm)
	cnt := "c1"
	dockerCmd(c, "network", "create", "aaa")
	dockerCmd(c, "network", "create", "ccc")

	dockerCmd(c, "run", "-d", "--name", cnt, "-p", "9000:90", "-p", "70", "busybox", "top")
	assert.Assert(c, waitRun(cnt) == nil)
	curPortMap, _ := dockerCmd(c, "port", cnt, "70")
	curExplPortMap, _ := dockerCmd(c, "port", cnt, "90")

	// Connect to a network which causes the container's default gw switch
	dockerCmd(c, "network", "connect", "aaa", cnt)
	verifyPortMap(c, cnt, "70", curPortMap, false)
	verifyPortMap(c, cnt, "90", curExplPortMap, true)

	// Read current mapping
	curPortMap, _ = dockerCmd(c, "port", cnt, "70")

	// Disconnect from a network which causes the container's default gw switch
	dockerCmd(c, "network", "disconnect", "aaa", cnt)
	verifyPortMap(c, cnt, "70", curPortMap, false)
	verifyPortMap(c, cnt, "90", curExplPortMap, true)

	// Read current mapping
	curPortMap, _ = dockerCmd(c, "port", cnt, "70")

	// Connect to a network which does not cause the container's default gw switch
	dockerCmd(c, "network", "connect", "ccc", cnt)
	verifyPortMap(c, cnt, "70", curPortMap, true)
	verifyPortMap(c, cnt, "90", curExplPortMap, true)
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectWithMac(c *testing.T) {
	macAddress := "02:42:ac:11:00:02"
	dockerCmd(c, "network", "create", "mynetwork")
	dockerCmd(c, "run", "--name=test", "-d", "--mac-address", macAddress, "busybox", "top")
	assert.Assert(c, waitRun("test") == nil)
	mac1 := inspectField(c, "test", "NetworkSettings.Networks.bridge.MacAddress")
	assert.Equal(c, strings.TrimSpace(mac1), macAddress)
	dockerCmd(c, "network", "connect", "mynetwork", "test")
	mac2 := inspectField(c, "test", "NetworkSettings.Networks.mynetwork.MacAddress")
	assert.Assert(c, strings.TrimSpace(mac2) != strings.TrimSpace(mac1))
}

func (s *DockerNetworkSuite) TestDockerNetworkInspectCreatedContainer(c *testing.T) {
	dockerCmd(c, "create", "--name", "test", "busybox")
	networks := inspectField(c, "test", "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, "bridge"), "Should return 'bridge' network")
}

func (s *DockerNetworkSuite) TestDockerNetworkRestartWithMultipleNetworks(c *testing.T) {
	dockerCmd(c, "network", "create", "test")
	dockerCmd(c, "run", "--name=foo", "-d", "busybox", "top")
	assert.Assert(c, waitRun("foo") == nil)
	dockerCmd(c, "network", "connect", "test", "foo")
	dockerCmd(c, "restart", "foo")
	networks := inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, "bridge"), "Should contain 'bridge' network")
	assert.Assert(c, strings.Contains(networks, "test"), "Should contain 'test' network")
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectDisconnectToStoppedContainer(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	dockerCmd(c, "network", "create", "test")
	dockerCmd(c, "create", "--name=foo", "busybox", "top")
	dockerCmd(c, "network", "connect", "test", "foo")
	networks := inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, "test"), "Should contain 'test' network")
	// Restart docker daemon to test the config has persisted to disk
	s.d.Restart(c)
	networks = inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, "test"), "Should contain 'test' network")
	// start the container and test if we can ping it from another container in the same network
	dockerCmd(c, "start", "foo")
	assert.Assert(c, waitRun("foo") == nil)
	ip := inspectField(c, "foo", "NetworkSettings.Networks.test.IPAddress")
	ip = strings.TrimSpace(ip)
	dockerCmd(c, "run", "--net=test", "busybox", "sh", "-c", fmt.Sprintf("ping -c 1 %s", ip))

	dockerCmd(c, "stop", "foo")

	// Test disconnect
	dockerCmd(c, "network", "disconnect", "test", "foo")
	networks = inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, !strings.Contains(networks, "test"), "Should not contain 'test' network")
	// Restart docker daemon to test the config has persisted to disk
	s.d.Restart(c)
	networks = inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, !strings.Contains(networks, "test"), "Should not contain 'test' network")
}

func (s *DockerNetworkSuite) TestDockerNetworkDisconnectContainerNonexistingNetwork(c *testing.T) {
	dockerCmd(c, "network", "create", "test")
	dockerCmd(c, "run", "--net=test", "-d", "--name=foo", "busybox", "top")
	networks := inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, "test"), "Should contain 'test' network")
	// Stop container and remove network
	dockerCmd(c, "stop", "foo")
	dockerCmd(c, "network", "rm", "test")

	// Test disconnecting stopped container from nonexisting network
	dockerCmd(c, "network", "disconnect", "-f", "test", "foo")
	networks = inspectField(c, "foo", "NetworkSettings.Networks")
	assert.Assert(c, !strings.Contains(networks, "test"), "Should not contain 'test' network")
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectPreferredIP(c *testing.T) {
	// create two networks
	dockerCmd(c, "network", "create", "--ipv6", "--subnet=172.28.0.0/16", "--subnet=2001:db8:1234::/64", "n0")
	assertNwIsAvailable(c, "n0")

	dockerCmd(c, "network", "create", "--ipv6", "--subnet=172.30.0.0/16", "--ip-range=172.30.5.0/24", "--subnet=2001:db8:abcd::/64", "--ip-range=2001:db8:abcd::/80", "n1")
	assertNwIsAvailable(c, "n1")

	// run a container on first network specifying the ip addresses
	dockerCmd(c, "run", "-d", "--name", "c0", "--net=n0", "--ip", "172.28.99.88", "--ip6", "2001:db8:1234::9988", "busybox", "top")
	assert.Assert(c, waitRun("c0") == nil)
	verifyIPAddressConfig(c, "c0", "n0", "172.28.99.88", "2001:db8:1234::9988")
	verifyIPAddresses(c, "c0", "n0", "172.28.99.88", "2001:db8:1234::9988")

	// connect the container to the second network specifying an ip addresses
	dockerCmd(c, "network", "connect", "--ip", "172.30.55.44", "--ip6", "2001:db8:abcd::5544", "n1", "c0")
	verifyIPAddressConfig(c, "c0", "n1", "172.30.55.44", "2001:db8:abcd::5544")
	verifyIPAddresses(c, "c0", "n1", "172.30.55.44", "2001:db8:abcd::5544")

	// Stop and restart the container
	dockerCmd(c, "stop", "c0")
	dockerCmd(c, "start", "c0")

	// verify requested addresses are applied and configs are still there
	verifyIPAddressConfig(c, "c0", "n0", "172.28.99.88", "2001:db8:1234::9988")
	verifyIPAddresses(c, "c0", "n0", "172.28.99.88", "2001:db8:1234::9988")
	verifyIPAddressConfig(c, "c0", "n1", "172.30.55.44", "2001:db8:abcd::5544")
	verifyIPAddresses(c, "c0", "n1", "172.30.55.44", "2001:db8:abcd::5544")

	// Still it should fail to connect to the default network with a specified IP (whatever ip)
	out, _, err := dockerCmdWithError("network", "connect", "--ip", "172.21.55.44", "bridge", "c0")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkAndIP.Error()))
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectPreferredIPStoppedContainer(c *testing.T) {
	// create a container
	dockerCmd(c, "create", "--name", "c0", "busybox", "top")

	// create a network
	dockerCmd(c, "network", "create", "--ipv6", "--subnet=172.30.0.0/16", "--subnet=2001:db8:abcd::/64", "n0")
	assertNwIsAvailable(c, "n0")

	// connect the container to the network specifying an ip addresses
	dockerCmd(c, "network", "connect", "--ip", "172.30.55.44", "--ip6", "2001:db8:abcd::5544", "n0", "c0")
	verifyIPAddressConfig(c, "c0", "n0", "172.30.55.44", "2001:db8:abcd::5544")

	// start the container, verify config has not changed and ip addresses are assigned
	dockerCmd(c, "start", "c0")
	assert.Assert(c, waitRun("c0") == nil)
	verifyIPAddressConfig(c, "c0", "n0", "172.30.55.44", "2001:db8:abcd::5544")
	verifyIPAddresses(c, "c0", "n0", "172.30.55.44", "2001:db8:abcd::5544")

	// stop the container and check ip config has not changed
	dockerCmd(c, "stop", "c0")
	verifyIPAddressConfig(c, "c0", "n0", "172.30.55.44", "2001:db8:abcd::5544")
}

func (s *DockerNetworkSuite) TestDockerNetworkUnsupportedRequiredIP(c *testing.T) {
	// requested IP is not supported on predefined networks
	for _, mode := range []string{"none", "host", "bridge", "default"} {
		checkUnsupportedNetworkAndIP(c, mode)
	}

	// requested IP is not supported on networks with no user defined subnets
	dockerCmd(c, "network", "create", "n0")
	assertNwIsAvailable(c, "n0")

	out, _, err := dockerCmdWithError("run", "-d", "--ip", "172.28.99.88", "--net", "n0", "busybox", "top")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkNoSubnetAndIP.Error()))
	out, _, err = dockerCmdWithError("run", "-d", "--ip6", "2001:db8:1234::9988", "--net", "n0", "busybox", "top")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkNoSubnetAndIP.Error()))
	dockerCmd(c, "network", "rm", "n0")
	assertNwNotAvailable(c, "n0")
}

func checkUnsupportedNetworkAndIP(c *testing.T, nwMode string) {
	out, _, err := dockerCmdWithError("run", "-d", "--net", nwMode, "--ip", "172.28.99.88", "--ip6", "2001:db8:1234::9988", "busybox", "top")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkAndIP.Error()))
}

func verifyIPAddressConfig(c *testing.T, cName, nwname, ipv4, ipv6 string) {
	if ipv4 != "" {
		out := inspectField(c, cName, fmt.Sprintf("NetworkSettings.Networks.%s.IPAMConfig.IPv4Address", nwname))
		assert.Equal(c, strings.TrimSpace(out), ipv4)
	}

	if ipv6 != "" {
		out := inspectField(c, cName, fmt.Sprintf("NetworkSettings.Networks.%s.IPAMConfig.IPv6Address", nwname))
		assert.Equal(c, strings.TrimSpace(out), ipv6)
	}
}

func verifyIPAddresses(c *testing.T, cName, nwname, ipv4, ipv6 string) {
	out := inspectField(c, cName, fmt.Sprintf("NetworkSettings.Networks.%s.IPAddress", nwname))
	assert.Equal(c, strings.TrimSpace(out), ipv4)

	out = inspectField(c, cName, fmt.Sprintf("NetworkSettings.Networks.%s.GlobalIPv6Address", nwname))
	assert.Equal(c, strings.TrimSpace(out), ipv6)
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectLinkLocalIP(c *testing.T) {
	// create one test network
	dockerCmd(c, "network", "create", "--ipv6", "--subnet=2001:db8:1234::/64", "n0")
	assertNwIsAvailable(c, "n0")

	// run a container with incorrect link-local address
	_, _, err := dockerCmdWithError("run", "--link-local-ip", "169.253.5.5", "busybox", "top")
	assert.ErrorContains(c, err, "")
	_, _, err = dockerCmdWithError("run", "--link-local-ip", "2001:db8::89", "busybox", "top")
	assert.ErrorContains(c, err, "")

	// run two containers with link-local ip on the test network
	dockerCmd(c, "run", "-d", "--name", "c0", "--net=n0", "--link-local-ip", "169.254.7.7", "--link-local-ip", "fe80::254:77", "busybox", "top")
	assert.Assert(c, waitRun("c0") == nil)
	dockerCmd(c, "run", "-d", "--name", "c1", "--net=n0", "--link-local-ip", "169.254.8.8", "--link-local-ip", "fe80::254:88", "busybox", "top")
	assert.Assert(c, waitRun("c1") == nil)

	// run a container on the default network and connect it to the test network specifying a link-local address
	dockerCmd(c, "run", "-d", "--name", "c2", "busybox", "top")
	assert.Assert(c, waitRun("c2") == nil)
	dockerCmd(c, "network", "connect", "--link-local-ip", "169.254.9.9", "n0", "c2")

	// verify the three containers can ping each other via the link-local addresses
	_, _, err = dockerCmdWithError("exec", "c0", "ping", "-c", "1", "169.254.8.8")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "c1", "ping", "-c", "1", "169.254.9.9")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "c2", "ping", "-c", "1", "169.254.7.7")
	assert.NilError(c, err)

	// Stop and restart the three containers
	dockerCmd(c, "stop", "c0")
	dockerCmd(c, "stop", "c1")
	dockerCmd(c, "stop", "c2")
	dockerCmd(c, "start", "c0")
	dockerCmd(c, "start", "c1")
	dockerCmd(c, "start", "c2")

	// verify the ping again
	_, _, err = dockerCmdWithError("exec", "c0", "ping", "-c", "1", "169.254.8.8")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "c1", "ping", "-c", "1", "169.254.9.9")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "c2", "ping", "-c", "1", "169.254.7.7")
	assert.NilError(c, err)
}

func (s *DockerSuite) TestUserDefinedNetworkConnectDisconnectLink(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace, NotArm)
	dockerCmd(c, "network", "create", "-d", "bridge", "foo1")
	dockerCmd(c, "network", "create", "-d", "bridge", "foo2")

	dockerCmd(c, "run", "-d", "--net=foo1", "--name=first", "busybox", "top")
	assert.Assert(c, waitRun("first") == nil)

	// run a container in a user-defined network with a link for an existing container
	// and a link for a container that doesn't exist
	dockerCmd(c, "run", "-d", "--net=foo1", "--name=second", "--link=first:FirstInFoo1",
		"--link=third:bar", "busybox", "top")
	assert.Assert(c, waitRun("second") == nil)

	// ping to first and its alias FirstInFoo1 must succeed
	_, _, err := dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "FirstInFoo1")
	assert.NilError(c, err)

	// connect first container to foo2 network
	dockerCmd(c, "network", "connect", "foo2", "first")
	// connect second container to foo2 network with a different alias for first container
	dockerCmd(c, "network", "connect", "--link=first:FirstInFoo2", "foo2", "second")

	// ping the new alias in network foo2
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "FirstInFoo2")
	assert.NilError(c, err)

	// disconnect first container from foo1 network
	dockerCmd(c, "network", "disconnect", "foo1", "first")

	// link in foo1 network must fail
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "FirstInFoo1")
	assert.ErrorContains(c, err, "")

	// link in foo2 network must succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "FirstInFoo2")
	assert.NilError(c, err)
}

func (s *DockerNetworkSuite) TestDockerNetworkDisconnectDefault(c *testing.T) {
	netWorkName1 := "test1"
	netWorkName2 := "test2"
	containerName := "foo"

	dockerCmd(c, "network", "create", netWorkName1)
	dockerCmd(c, "network", "create", netWorkName2)
	dockerCmd(c, "create", "--name", containerName, "busybox", "top")
	dockerCmd(c, "network", "connect", netWorkName1, containerName)
	dockerCmd(c, "network", "connect", netWorkName2, containerName)
	dockerCmd(c, "network", "disconnect", "bridge", containerName)

	dockerCmd(c, "start", containerName)
	assert.Assert(c, waitRun(containerName) == nil)
	networks := inspectField(c, containerName, "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, netWorkName1), fmt.Sprintf("Should contain '%s' network", netWorkName1))
	assert.Assert(c, strings.Contains(networks, netWorkName2), fmt.Sprintf("Should contain '%s' network", netWorkName2))
	assert.Assert(c, !strings.Contains(networks, "bridge"), "Should not contain 'bridge' network")
}

func (s *DockerNetworkSuite) TestDockerNetworkConnectWithAliasOnDefaultNetworks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace, NotArm)

	defaults := []string{"bridge", "host", "none"}
	out, _ := dockerCmd(c, "run", "-d", "--net=none", "busybox", "top")
	containerID := strings.TrimSpace(out)
	for _, net := range defaults {
		res, _, err := dockerCmdWithError("network", "connect", "--alias", "alias"+net, net, containerID)
		assert.ErrorContains(c, err, "")
		assert.Assert(c, strings.Contains(res, runconfig.ErrUnsupportedNetworkAndAlias.Error()))
	}
}

func (s *DockerSuite) TestUserDefinedNetworkConnectDisconnectAlias(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace, NotArm)
	dockerCmd(c, "network", "create", "-d", "bridge", "net1")
	dockerCmd(c, "network", "create", "-d", "bridge", "net2")

	cid, _ := dockerCmd(c, "run", "-d", "--net=net1", "--name=first", "--net-alias=foo", "busybox:glibc", "top")
	assert.Assert(c, waitRun("first") == nil)

	dockerCmd(c, "run", "-d", "--net=net1", "--name=second", "busybox:glibc", "top")
	assert.Assert(c, waitRun("second") == nil)

	// ping first container and its alias
	_, _, err := dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)

	// ping first container's short-id alias
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", stringid.TruncateID(cid))
	assert.NilError(c, err)

	// connect first container to net2 network
	dockerCmd(c, "network", "connect", "--alias=bar", "net2", "first")
	// connect second container to foo2 network with a different alias for first container
	dockerCmd(c, "network", "connect", "net2", "second")

	// ping the new alias in network foo2
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "bar")
	assert.NilError(c, err)

	// disconnect first container from net1 network
	dockerCmd(c, "network", "disconnect", "net1", "first")

	// ping to net1 scoped alias "foo" must fail
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.ErrorContains(c, err, "")

	// ping to net2 scoped alias "bar" must still succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "bar")
	assert.NilError(c, err)
	// ping to net2 scoped alias short-id must still succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", stringid.TruncateID(cid))
	assert.NilError(c, err)

	// verify the alias option is rejected when running on predefined network
	out, _, err := dockerCmdWithError("run", "--rm", "--name=any", "--net-alias=any", "busybox:glibc", "top")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkAndAlias.Error()))
	// verify the alias option is rejected when connecting to predefined network
	out, _, err = dockerCmdWithError("network", "connect", "--alias=any", "bridge", "first")
	assert.Assert(c, err != nil, "out: %s", out)
	assert.Assert(c, strings.Contains(out, runconfig.ErrUnsupportedNetworkAndAlias.Error()))
}

func (s *DockerSuite) TestUserDefinedNetworkConnectivity(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	dockerCmd(c, "network", "create", "-d", "bridge", "br.net1")

	dockerCmd(c, "run", "-d", "--net=br.net1", "--name=c1.net1", "busybox:glibc", "top")
	assert.Assert(c, waitRun("c1.net1") == nil)

	dockerCmd(c, "run", "-d", "--net=br.net1", "--name=c2.net1", "busybox:glibc", "top")
	assert.Assert(c, waitRun("c2.net1") == nil)

	// ping first container by its unqualified name
	_, _, err := dockerCmdWithError("exec", "c2.net1", "ping", "-c", "1", "c1.net1")
	assert.NilError(c, err)

	// ping first container by its qualified name
	_, _, err = dockerCmdWithError("exec", "c2.net1", "ping", "-c", "1", "c1.net1.br.net1")
	assert.NilError(c, err)

	// ping with first qualified name masked by an additional domain. should fail
	_, _, err = dockerCmdWithError("exec", "c2.net1", "ping", "-c", "1", "c1.net1.br.net1.google.com")
	assert.ErrorContains(c, err, "")
}

func (s *DockerSuite) TestEmbeddedDNSInvalidInput(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	dockerCmd(c, "network", "create", "-d", "bridge", "nw1")

	// Sending garbage to embedded DNS shouldn't crash the daemon
	dockerCmd(c, "run", "-i", "--net=nw1", "--name=c1", "debian:jessie", "bash", "-c", "echo InvalidQuery > /dev/udp/127.0.0.11/53")
}

func (s *DockerSuite) TestDockerNetworkConnectFailsNoInspectChange(c *testing.T) {
	dockerCmd(c, "run", "-d", "--name=bb", "busybox", "top")
	assert.Assert(c, waitRun("bb") == nil)
	defer dockerCmd(c, "stop", "bb")

	ns0 := inspectField(c, "bb", "NetworkSettings.Networks.bridge")

	// A failing redundant network connect should not alter current container's endpoint settings
	_, _, err := dockerCmdWithError("network", "connect", "bridge", "bb")
	assert.ErrorContains(c, err, "")

	ns1 := inspectField(c, "bb", "NetworkSettings.Networks.bridge")
	assert.Equal(c, ns1, ns0)
}

func (s *DockerSuite) TestDockerNetworkInternalMode(c *testing.T) {
	dockerCmd(c, "network", "create", "--driver=bridge", "--internal", "internal")
	assertNwIsAvailable(c, "internal")
	nr := getNetworkResource(c, "internal")
	assert.Assert(c, nr.Internal)

	dockerCmd(c, "run", "-d", "--net=internal", "--name=first", "busybox:glibc", "top")
	assert.Assert(c, waitRun("first") == nil)
	dockerCmd(c, "run", "-d", "--net=internal", "--name=second", "busybox:glibc", "top")
	assert.Assert(c, waitRun("second") == nil)
	out, _, err := dockerCmdWithError("exec", "first", "ping", "-W", "4", "-c", "1", "8.8.8.8")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "100% packet loss"))
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
}

// Test for #21401
func (s *DockerNetworkSuite) TestDockerNetworkCreateDeleteSpecialCharacters(c *testing.T) {
	dockerCmd(c, "network", "create", "test@#$")
	assertNwIsAvailable(c, "test@#$")
	dockerCmd(c, "network", "rm", "test@#$")
	assertNwNotAvailable(c, "test@#$")

	dockerCmd(c, "network", "create", "kiwl$%^")
	assertNwIsAvailable(c, "kiwl$%^")
	dockerCmd(c, "network", "rm", "kiwl$%^")
	assertNwNotAvailable(c, "kiwl$%^")
}

func (s *DockerDaemonSuite) TestDaemonRestartRestoreBridgeNetwork(t *testing.T) {
	s.d.StartWithBusybox(t, "--live-restore")
	defer s.d.Stop(t)
	oldCon := "old"

	_, err := s.d.Cmd("run", "-d", "--name", oldCon, "-p", "80:80", "busybox", "top")
	if err != nil {
		t.Fatal(err)
	}
	oldContainerIP, err := s.d.Cmd("inspect", "-f", "{{ .NetworkSettings.Networks.bridge.IPAddress }}", oldCon)
	if err != nil {
		t.Fatal(err)
	}
	// Kill the daemon
	if err := s.d.Kill(); err != nil {
		t.Fatal(err)
	}

	// restart the daemon
	s.d.Start(t, "--live-restore")

	// start a new container, the new container's ip should not be the same with
	// old running container.
	newCon := "new"
	_, err = s.d.Cmd("run", "-d", "--name", newCon, "busybox", "top")
	if err != nil {
		t.Fatal(err)
	}
	newContainerIP, err := s.d.Cmd("inspect", "-f", "{{ .NetworkSettings.Networks.bridge.IPAddress }}", newCon)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Compare(strings.TrimSpace(oldContainerIP), strings.TrimSpace(newContainerIP)) == 0 {
		t.Fatalf("new container ip should not equal to old running container  ip")
	}

	// start a new container, the new container should ping old running container
	_, err = s.d.Cmd("run", "-t", "busybox", "ping", "-c", "1", oldContainerIP)
	if err != nil {
		t.Fatal(err)
	}

	// start a new container, trying to publish port 80:80 should fail
	out, err := s.d.Cmd("run", "-p", "80:80", "-d", "busybox", "top")
	if err == nil || !strings.Contains(out, "Bind for 0.0.0.0:80 failed: port is already allocated") {
		t.Fatalf("80 port is allocated to old running container, it should failed on allocating to new container")
	}

	// kill old running container and try to allocate again
	_, err = s.d.Cmd("kill", oldCon)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.d.Cmd("run", "-p", "80:80", "-d", "busybox", "top")
	if err != nil {
		t.Fatal(err)
	}

	// Cleanup because these containers will not be shut down by daemon
	out, err = s.d.Cmd("stop", newCon)
	if err != nil {
		t.Fatalf("err: %v %v", err, out)
	}
	_, err = s.d.Cmd("stop", strings.TrimSpace(id))
	if err != nil {
		t.Fatal(err)
	}
}

func (s *DockerNetworkSuite) TestDockerNetworkFlagAlias(c *testing.T) {
	dockerCmd(c, "network", "create", "user")
	output, status := dockerCmd(c, "run", "--rm", "--network=user", "--network-alias=foo", "busybox", "true")
	assert.Equal(c, status, 0, fmt.Sprintf("unexpected status code %d (%s)", status, output))

	output, status, _ = dockerCmdWithError("run", "--rm", "--net=user", "--network=user", "busybox", "true")
	assert.Equal(c, status, 0, fmt.Sprintf("unexpected status code %d (%s)", status, output))

	output, status, _ = dockerCmdWithError("run", "--rm", "--network=user", "--net-alias=foo", "--network-alias=bar", "busybox", "true")
	assert.Equal(c, status, 0, fmt.Sprintf("unexpected status code %d (%s)", status, output))
}

func (s *DockerNetworkSuite) TestDockerNetworkValidateIP(c *testing.T) {
	_, _, err := dockerCmdWithError("network", "create", "--ipv6", "--subnet=172.28.0.0/16", "--subnet=2001:db8:1234::/64", "mynet")
	assert.NilError(c, err)
	assertNwIsAvailable(c, "mynet")

	_, _, err = dockerCmdWithError("run", "-d", "--name", "mynet0", "--net=mynet", "--ip", "172.28.99.88", "--ip6", "2001:db8:1234::9988", "busybox", "top")
	assert.NilError(c, err)
	assert.Assert(c, waitRun("mynet0") == nil)
	verifyIPAddressConfig(c, "mynet0", "mynet", "172.28.99.88", "2001:db8:1234::9988")
	verifyIPAddresses(c, "mynet0", "mynet", "172.28.99.88", "2001:db8:1234::9988")

	_, _, err = dockerCmdWithError("run", "--net=mynet", "--ip", "mynet_ip", "--ip6", "2001:db8:1234::9999", "busybox", "top")
	assert.ErrorContains(c, err, "invalid IPv4 address")
	_, _, err = dockerCmdWithError("run", "--net=mynet", "--ip", "172.28.99.99", "--ip6", "mynet_ip6", "busybox", "top")
	assert.ErrorContains(c, err, "invalid IPv6 address")

	// This is a case of IPv4 address to `--ip6`
	_, _, err = dockerCmdWithError("run", "--net=mynet", "--ip6", "172.28.99.99", "busybox", "top")
	assert.ErrorContains(c, err, "invalid IPv6 address")
	// This is a special case of an IPv4-mapped IPv6 address
	_, _, err = dockerCmdWithError("run", "--net=mynet", "--ip6", "::ffff:172.28.99.99", "busybox", "top")
	assert.ErrorContains(c, err, "invalid IPv6 address")
}

// Test case for 26220
func (s *DockerNetworkSuite) TestDockerNetworkDisconnectFromBridge(c *testing.T) {
	out, _ := dockerCmd(c, "network", "inspect", "--format", "{{.Id}}", "bridge")

	network := strings.TrimSpace(out)

	name := "test"
	dockerCmd(c, "create", "--name", name, "busybox", "top")

	_, _, err := dockerCmdWithError("network", "disconnect", network, name)
	assert.NilError(c, err)
}

// TestConntrackFlowsLeak covers the failure scenario of ticket: https://github.com/docker/docker/issues/8795
// Validates that conntrack is correctly cleaned once a container is destroyed
func (s *DockerNetworkSuite) TestConntrackFlowsLeak(c *testing.T) {
	testRequires(c, IsAmd64, DaemonIsLinux, Network, testEnv.IsLocalDaemon)

	// Create a new network
	cli.DockerCmd(c, "network", "create", "--subnet=192.168.10.0/24", "--gateway=192.168.10.1", "-o", "com.docker.network.bridge.host_binding_ipv4=192.168.10.1", "testbind")
	assertNwIsAvailable(c, "testbind")

	// Launch the server, this will remain listening on an exposed port and reply to any request in a ping/pong fashion
	cmd := "while true; do echo hello | nc -w 1 -lu 8080; done"
	cli.DockerCmd(c, "run", "-d", "--name", "server", "--net", "testbind", "-p", "8080:8080/udp", "appropriate/nc", "sh", "-c", cmd)

	// Launch a container client, here the objective is to create a flow that is natted in order to expose the bug
	cmd = "echo world | nc -q 1 -u 192.168.10.1 8080"
	cli.DockerCmd(c, "run", "-d", "--name", "client", "--net=host", "appropriate/nc", "sh", "-c", cmd)

	// Get all the flows using netlink
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET)
	assert.NilError(c, err)
	var flowMatch int
	for _, flow := range flows {
		// count only the flows that we are interested in, skipping others that can be laying around the host
		if flow.Forward.Protocol == unix.IPPROTO_UDP &&
			flow.Forward.DstIP.Equal(net.ParseIP("192.168.10.1")) &&
			flow.Forward.DstPort == 8080 {
			flowMatch++
		}
	}
	// The client should have created only 1 flow
	assert.Equal(c, flowMatch, 1)

	// Now delete the server, this will trigger the conntrack cleanup
	cli.DockerCmd(c, "rm", "-fv", "server")

	// Fetch again all the flows and validate that there is no server flow in the conntrack laying around
	flows, err = netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET)
	assert.NilError(c, err)
	flowMatch = 0
	for _, flow := range flows {
		if flow.Forward.Protocol == unix.IPPROTO_UDP &&
			flow.Forward.DstIP.Equal(net.ParseIP("192.168.10.1")) &&
			flow.Forward.DstPort == 8080 {
			flowMatch++
		}
	}
	// All the flows have to be gone
	assert.Equal(c, flowMatch, 0)
}

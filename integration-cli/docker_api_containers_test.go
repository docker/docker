package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/internal/test/request"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/volume"
	"github.com/docker/go-connections/nat"
	"github.com/go-check/check"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
)

func (s *DockerSuite) TestContainerAPIGetAll(c *check.C) {
	startCount := getContainerCount(c)
	name := "getall"
	dockerCmd(c, "run", "--name", name, "busybox", "true")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	options := types.ContainerListOptions{
		All: true,
	}
	containers, err := cli.ContainerList(context.Background(), options)
	assert.NilError(c, err)
	assert.Equal(c, len(containers), startCount+1)
	actual := containers[0].Names[0]
	c.Assert(actual, checker.Equals, "/"+name)
}

// regression test for empty json field being omitted #13691
func (s *DockerSuite) TestContainerAPIGetJSONNoFieldsOmitted(c *check.C) {
	startCount := getContainerCount(c)
	dockerCmd(c, "run", "busybox", "true")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	options := types.ContainerListOptions{
		All: true,
	}
	containers, err := cli.ContainerList(context.Background(), options)
	assert.NilError(c, err)
	assert.Equal(c, len(containers), startCount+1)
	actual := fmt.Sprintf("%+v", containers[0])

	// empty Labels field triggered this bug, make sense to check for everything
	// cause even Ports for instance can trigger this bug
	// better safe than sorry..
	fields := []string{
		"ID",
		"Names",
		"Image",
		"Command",
		"Created",
		"Ports",
		"Labels",
		"Status",
		"NetworkSettings",
	}

	// decoding into types.Container do not work since it eventually unmarshal
	// and empty field to an empty go map, so we just check for a string
	for _, f := range fields {
		if !strings.Contains(actual, f) {
			c.Fatalf("Field %s is missing and it shouldn't", f)
		}
	}
}

type containerPs struct {
	Names []string
	Ports []types.Port
}

// regression test for non-empty fields from #13901
func (s *DockerSuite) TestContainerAPIPsOmitFields(c *check.C) {
	// Problematic for Windows porting due to networking not yet being passed back
	testRequires(c, DaemonIsLinux)
	name := "pstest"
	port := 80
	runSleepingContainer(c, "--name", name, "--expose", strconv.Itoa(port))

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	options := types.ContainerListOptions{
		All: true,
	}
	containers, err := cli.ContainerList(context.Background(), options)
	assert.NilError(c, err)
	var foundContainer containerPs
	for _, c := range containers {
		for _, testName := range c.Names {
			if "/"+name == testName {
				foundContainer.Names = c.Names
				foundContainer.Ports = c.Ports
				break
			}
		}
	}

	c.Assert(foundContainer.Ports, checker.HasLen, 1)
	c.Assert(foundContainer.Ports[0].PrivatePort, checker.Equals, uint16(port))
	c.Assert(foundContainer.Ports[0].PublicPort, checker.NotNil)
	c.Assert(foundContainer.Ports[0].IP, checker.NotNil)
}

func (s *DockerSuite) TestContainerAPIGetExport(c *check.C) {
	// Not supported on Windows as Windows does not support docker export
	testRequires(c, DaemonIsLinux)
	name := "exportcontainer"
	dockerCmd(c, "run", "--name", name, "busybox", "touch", "/test")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	body, err := cli.ContainerExport(context.Background(), name)
	assert.NilError(c, err)
	defer body.Close()
	found := false
	for tarReader := tar.NewReader(body); ; {
		h, err := tarReader.Next()
		if err != nil && err == io.EOF {
			break
		}
		if h.Name == "test" {
			found = true
			break
		}
	}
	c.Assert(found, checker.True, check.Commentf("The created test file has not been found in the exported image"))
}

func (s *DockerSuite) TestContainerAPIGetChanges(c *check.C) {
	// Not supported on Windows as Windows does not support docker diff (/containers/name/changes)
	testRequires(c, DaemonIsLinux)
	name := "changescontainer"
	dockerCmd(c, "run", "--name", name, "busybox", "rm", "/etc/passwd")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	changes, err := cli.ContainerDiff(context.Background(), name)
	assert.NilError(c, err)

	// Check the changelog for removal of /etc/passwd
	success := false
	for _, elem := range changes {
		if elem.Path == "/etc/passwd" && elem.Kind == 2 {
			success = true
		}
	}
	c.Assert(success, checker.True, check.Commentf("/etc/passwd has been removed but is not present in the diff"))
}

func (s *DockerSuite) TestGetContainerStats(c *check.C) {
	var (
		name = "statscontainer"
	)
	runSleepingContainer(c, "--name", name)

	type b struct {
		stats types.ContainerStats
		err   error
	}

	bc := make(chan b, 1)
	go func() {
		cli, err := client.NewClientWithOpts(client.FromEnv)
		assert.NilError(c, err)
		defer cli.Close()

		stats, err := cli.ContainerStats(context.Background(), name, true)
		assert.NilError(c, err)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	dockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		dec := json.NewDecoder(sr.stats.Body)
		defer sr.stats.Body.Close()
		var s *types.Stats
		// decode only one object from the stream
		assert.NilError(c, dec.Decode(&s))
	}
}

func (s *DockerSuite) TestGetContainerStatsRmRunning(c *check.C) {
	out := runSleepingContainer(c)
	id := strings.TrimSpace(out)

	buf := &ChannelBuffer{C: make(chan []byte, 1)}
	defer buf.Close()

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	stats, err := cli.ContainerStats(context.Background(), id, true)
	assert.NilError(c, err)
	defer stats.Body.Close()

	chErr := make(chan error, 1)
	go func() {
		_, err = io.Copy(buf, stats.Body)
		chErr <- err
	}()

	b := make([]byte, 32)
	// make sure we've got some stats
	_, err = buf.ReadTimeout(b, 2*time.Second)
	assert.NilError(c, err)

	// Now remove without `-f` and make sure we are still pulling stats
	_, _, err = dockerCmdWithError("rm", id)
	c.Assert(err, checker.Not(checker.IsNil), check.Commentf("rm should have failed but didn't"))
	_, err = buf.ReadTimeout(b, 2*time.Second)
	assert.NilError(c, err)

	dockerCmd(c, "rm", "-f", id)
	c.Assert(<-chErr, checker.IsNil)
}

// ChannelBuffer holds a chan of byte array that can be populate in a goroutine.
type ChannelBuffer struct {
	C chan []byte
}

// Write implements Writer.
func (c *ChannelBuffer) Write(b []byte) (int, error) {
	c.C <- b
	return len(b), nil
}

// Close closes the go channel.
func (c *ChannelBuffer) Close() error {
	close(c.C)
	return nil
}

// ReadTimeout reads the content of the channel in the specified byte array with
// the specified duration as timeout.
func (c *ChannelBuffer) ReadTimeout(p []byte, n time.Duration) (int, error) {
	select {
	case b := <-c.C:
		return copy(p[0:], b), nil
	case <-time.After(n):
		return -1, fmt.Errorf("timeout reading from channel")
	}
}

// regression test for gh13421
// previous test was just checking one stat entry so it didn't fail (stats with
// stream false always return one stat)
func (s *DockerSuite) TestGetContainerStatsStream(c *check.C) {
	name := "statscontainer"
	runSleepingContainer(c, "--name", name)

	type b struct {
		stats types.ContainerStats
		err   error
	}

	bc := make(chan b, 1)
	go func() {
		cli, err := client.NewClientWithOpts(client.FromEnv)
		assert.NilError(c, err)
		defer cli.Close()

		stats, err := cli.ContainerStats(context.Background(), name, true)
		assert.NilError(c, err)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	dockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		b, err := ioutil.ReadAll(sr.stats.Body)
		defer sr.stats.Body.Close()
		assert.NilError(c, err)
		s := string(b)
		// count occurrences of "read" of types.Stats
		if l := strings.Count(s, "read"); l < 2 {
			c.Fatalf("Expected more than one stat streamed, got %d", l)
		}
	}
}

func (s *DockerSuite) TestGetContainerStatsNoStream(c *check.C) {
	name := "statscontainer"
	runSleepingContainer(c, "--name", name)

	type b struct {
		stats types.ContainerStats
		err   error
	}

	bc := make(chan b, 1)

	go func() {
		cli, err := client.NewClientWithOpts(client.FromEnv)
		assert.NilError(c, err)
		defer cli.Close()

		stats, err := cli.ContainerStats(context.Background(), name, false)
		assert.NilError(c, err)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	dockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		b, err := ioutil.ReadAll(sr.stats.Body)
		defer sr.stats.Body.Close()
		assert.NilError(c, err)
		s := string(b)
		// count occurrences of `"read"` of types.Stats
		assert.Assert(c, strings.Count(s, `"read"`) == 1, "Expected only one stat streamed, got %d", strings.Count(s, `"read"`))
	}
}

func (s *DockerSuite) TestGetStoppedContainerStats(c *check.C) {
	name := "statscontainer"
	dockerCmd(c, "create", "--name", name, "busybox", "ps")

	chResp := make(chan error)

	// We expect an immediate response, but if it's not immediate, the test would hang, so put it in a goroutine
	// below we'll check this on a timeout.
	go func() {
		cli, err := client.NewClientWithOpts(client.FromEnv)
		assert.NilError(c, err)
		defer cli.Close()

		resp, err := cli.ContainerStats(context.Background(), name, false)
		defer resp.Body.Close()
		chResp <- err
	}()

	select {
	case err := <-chResp:
		assert.NilError(c, err)
	case <-time.After(10 * time.Second):
		c.Fatal("timeout waiting for stats response for stopped container")
	}
}

func (s *DockerSuite) TestContainerAPIPause(c *check.C) {
	// Problematic on Windows as Windows does not support pause
	testRequires(c, DaemonIsLinux)

	getPaused := func(c *check.C) []string {
		return strings.Fields(cli.DockerCmd(c, "ps", "-f", "status=paused", "-q", "-a").Combined())
	}

	out := cli.DockerCmd(c, "run", "-d", "busybox", "sleep", "30").Combined()
	ContainerID := strings.TrimSpace(out)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerPause(context.Background(), ContainerID)
	assert.NilError(c, err)

	pausedContainers := getPaused(c)

	if len(pausedContainers) != 1 || stringid.TruncateID(ContainerID) != pausedContainers[0] {
		c.Fatalf("there should be one paused container and not %d", len(pausedContainers))
	}

	err = cli.ContainerUnpause(context.Background(), ContainerID)
	assert.NilError(c, err)

	pausedContainers = getPaused(c)
	c.Assert(pausedContainers, checker.HasLen, 0, check.Commentf("There should be no paused container."))
}

func (s *DockerSuite) TestContainerAPITop(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "top")
	id := strings.TrimSpace(string(out))
	assert.NilError(c, waitRun(id))

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	// sort by comm[andline] to make sure order stays the same in case of PID rollover
	top, err := cli.ContainerTop(context.Background(), id, []string{"aux", "--sort=comm"})
	assert.NilError(c, err)
	c.Assert(top.Titles, checker.HasLen, 11, check.Commentf("expected 11 titles, found %d: %v", len(top.Titles), top.Titles))

	if top.Titles[0] != "USER" || top.Titles[10] != "COMMAND" {
		c.Fatalf("expected `USER` at `Titles[0]` and `COMMAND` at Titles[10]: %v", top.Titles)
	}
	c.Assert(top.Processes, checker.HasLen, 2, check.Commentf("expected 2 processes, found %d: %v", len(top.Processes), top.Processes))
	c.Assert(top.Processes[0][10], checker.Equals, "/bin/sh -c top")
	c.Assert(top.Processes[1][10], checker.Equals, "top")
}

func (s *DockerSuite) TestContainerAPITopWindows(c *check.C) {
	testRequires(c, DaemonIsWindows)
	out := runSleepingContainer(c, "-d")
	id := strings.TrimSpace(string(out))
	assert.NilError(c, waitRun(id))

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	top, err := cli.ContainerTop(context.Background(), id, nil)
	assert.NilError(c, err)
	assert.Equal(c, len(top.Titles), 4, "expected 4 titles, found %d: %v", len(top.Titles), top.Titles)

	if top.Titles[0] != "Name" || top.Titles[3] != "Private Working Set" {
		c.Fatalf("expected `Name` at `Titles[0]` and `Private Working Set` at Titles[3]: %v", top.Titles)
	}
	assert.Assert(c, len(top.Processes) >= 2, "expected at least 2 processes, found %d: %v", len(top.Processes), top.Processes)

	foundProcess := false
	expectedProcess := "busybox.exe"
	for _, process := range top.Processes {
		if process[0] == expectedProcess {
			foundProcess = true
			break
		}
	}

	assert.Assert(c, foundProcess, "expected to find %s: %v", expectedProcess, top.Processes)
}

func (s *DockerSuite) TestContainerAPICommit(c *check.C) {
	cName := "testapicommit"
	dockerCmd(c, "run", "--name="+cName, "busybox", "/bin/sh", "-c", "touch /test")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	options := types.ContainerCommitOptions{
		Reference: "testcontainerapicommit:testtag",
	}

	img, err := cli.ContainerCommit(context.Background(), cName, options)
	assert.NilError(c, err)

	cmd := inspectField(c, img.ID, "Config.Cmd")
	c.Assert(cmd, checker.Equals, "[/bin/sh -c touch /test]", check.Commentf("got wrong Cmd from commit: %q", cmd))

	// sanity check, make sure the image is what we think it is
	dockerCmd(c, "run", img.ID, "ls", "/test")
}

func (s *DockerSuite) TestContainerAPICommitWithLabelInConfig(c *check.C) {
	cName := "testapicommitwithconfig"
	dockerCmd(c, "run", "--name="+cName, "busybox", "/bin/sh", "-c", "touch /test")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	config := containertypes.Config{
		Labels: map[string]string{"key1": "value1", "key2": "value2"}}

	options := types.ContainerCommitOptions{
		Reference: "testcontainerapicommitwithconfig",
		Config:    &config,
	}

	img, err := cli.ContainerCommit(context.Background(), cName, options)
	assert.NilError(c, err)

	label1 := inspectFieldMap(c, img.ID, "Config.Labels", "key1")
	c.Assert(label1, checker.Equals, "value1")

	label2 := inspectFieldMap(c, img.ID, "Config.Labels", "key2")
	c.Assert(label2, checker.Equals, "value2")

	cmd := inspectField(c, img.ID, "Config.Cmd")
	c.Assert(cmd, checker.Equals, "[/bin/sh -c touch /test]", check.Commentf("got wrong Cmd from commit: %q", cmd))

	// sanity check, make sure the image is what we think it is
	dockerCmd(c, "run", img.ID, "ls", "/test")
}

func (s *DockerSuite) TestContainerAPIBadPort(c *check.C) {
	// TODO Windows to Windows CI - Port this test
	testRequires(c, DaemonIsLinux)

	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "echo test"},
	}

	hostConfig := containertypes.HostConfig{
		PortBindings: nat.PortMap{
			"8080/tcp": []nat.PortBinding{
				{
					HostIP:   "",
					HostPort: "aa80"},
			},
		},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "")
	assert.ErrorContains(c, err, `invalid port specification: "aa80"`)
}

func (s *DockerSuite) TestContainerAPICreate(c *check.C) {
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "touch /test && ls /test"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	out, _ := dockerCmd(c, "start", "-a", container.ID)
	assert.Equal(c, strings.TrimSpace(out), "/test")
}

func (s *DockerSuite) TestContainerAPICreateEmptyConfig(c *check.C) {

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &containertypes.Config{}, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")

	expected := "No command specified"
	assert.ErrorContains(c, err, expected)
}

func (s *DockerSuite) TestContainerAPICreateMultipleNetworksConfig(c *check.C) {
	// Container creation must fail if client specified configurations for more than one network
	config := containertypes.Config{
		Image: "busybox",
	}

	networkingConfig := networktypes.NetworkingConfig{
		EndpointsConfig: map[string]*networktypes.EndpointSettings{
			"net1": {},
			"net2": {},
			"net3": {},
		},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networkingConfig, "")
	msg := err.Error()
	// network name order in error message is not deterministic
	c.Assert(msg, checker.Contains, "Container cannot be connected to network endpoints")
	c.Assert(msg, checker.Contains, "net1")
	c.Assert(msg, checker.Contains, "net2")
	c.Assert(msg, checker.Contains, "net3")
}

func (s *DockerSuite) TestContainerAPICreateBridgeNetworkMode(c *check.C) {
	// Windows does not support bridge
	testRequires(c, DaemonIsLinux)
	UtilCreateNetworkMode(c, "bridge")
}

func (s *DockerSuite) TestContainerAPICreateOtherNetworkModes(c *check.C) {
	// Windows does not support these network modes
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	UtilCreateNetworkMode(c, "host")
	UtilCreateNetworkMode(c, "container:web1")
}

func UtilCreateNetworkMode(c *check.C, networkMode containertypes.NetworkMode) {
	config := containertypes.Config{
		Image: "busybox",
	}

	hostConfig := containertypes.HostConfig{
		NetworkMode: networkMode,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	c.Assert(containerJSON.HostConfig.NetworkMode, checker.Equals, containertypes.NetworkMode(networkMode), check.Commentf("Mismatched NetworkMode"))
}

func (s *DockerSuite) TestContainerAPICreateWithCpuSharesCpuset(c *check.C) {
	// TODO Windows to Windows CI. The CpuShares part could be ported.
	testRequires(c, DaemonIsLinux)
	config := containertypes.Config{
		Image: "busybox",
	}

	hostConfig := containertypes.HostConfig{
		Resources: containertypes.Resources{
			CPUShares:  512,
			CpusetCpus: "0",
		},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	out := inspectField(c, containerJSON.ID, "HostConfig.CpuShares")
	assert.Equal(c, out, "512")

	outCpuset := inspectField(c, containerJSON.ID, "HostConfig.CpusetCpus")
	c.Assert(outCpuset, checker.Equals, "0")
}

func (s *DockerSuite) TestContainerAPIVerifyHeader(c *check.C) {
	config := map[string]interface{}{
		"Image": "busybox",
	}

	create := func(ct string) (*http.Response, io.ReadCloser, error) {
		jsonData := bytes.NewBuffer(nil)
		c.Assert(json.NewEncoder(jsonData).Encode(config), checker.IsNil)
		return request.Post("/containers/create", request.RawContent(ioutil.NopCloser(jsonData)), request.ContentType(ct))
	}

	// Try with no content-type
	res, body, err := create("")
	assert.NilError(c, err)
	// todo: we need to figure out a better way to compare between dockerd versions
	// comparing between daemon API version is not precise.
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}
	body.Close()

	// Try with wrong content-type
	res, body, err = create("application/xml")
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}
	body.Close()

	// now application/json
	res, body, err = create("application/json")
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusCreated)
	body.Close()
}

//Issue 14230. daemon should return 500 for invalid port syntax
func (s *DockerSuite) TestContainerAPIInvalidPortSyntax(c *check.C) {
	config := `{
				  "Image": "busybox",
				  "HostConfig": {
					"NetworkMode": "default",
					"PortBindings": {
					  "19039;1230": [
						{}
					  ]
					}
				  }
				}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b[:]), checker.Contains, "invalid port")
}

func (s *DockerSuite) TestContainerAPIRestartPolicyInvalidPolicyName(c *check.C) {
	config := `{
		"Image": "busybox",
		"HostConfig": {
			"RestartPolicy": {
				"Name": "something",
				"MaximumRetryCount": 0
			}
		}
	}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b[:]), checker.Contains, "invalid restart policy")
}

func (s *DockerSuite) TestContainerAPIRestartPolicyRetryMismatch(c *check.C) {
	config := `{
		"Image": "busybox",
		"HostConfig": {
			"RestartPolicy": {
				"Name": "always",
				"MaximumRetryCount": 2
			}
		}
	}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b[:]), checker.Contains, "maximum retry count cannot be used with restart policy")
}

func (s *DockerSuite) TestContainerAPIRestartPolicyNegativeRetryCount(c *check.C) {
	config := `{
		"Image": "busybox",
		"HostConfig": {
			"RestartPolicy": {
				"Name": "on-failure",
				"MaximumRetryCount": -2
			}
		}
	}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b[:]), checker.Contains, "maximum retry count cannot be negative")
}

func (s *DockerSuite) TestContainerAPIRestartPolicyDefaultRetryCount(c *check.C) {
	config := `{
		"Image": "busybox",
		"HostConfig": {
			"RestartPolicy": {
				"Name": "on-failure",
				"MaximumRetryCount": 0
			}
		}
	}`

	res, _, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusCreated)
}

// Issue 7941 - test to make sure a "null" in JSON is just ignored.
// W/o this fix a null in JSON would be parsed into a string var as "null"
func (s *DockerSuite) TestContainerAPIPostCreateNull(c *check.C) {
	config := `{
		"Hostname":"",
		"Domainname":"",
		"Memory":0,
		"MemorySwap":0,
		"CpuShares":0,
		"Cpuset":null,
		"AttachStdin":true,
		"AttachStdout":true,
		"AttachStderr":true,
		"ExposedPorts":{},
		"Tty":true,
		"OpenStdin":true,
		"StdinOnce":true,
		"Env":[],
		"Cmd":"ls",
		"Image":"busybox",
		"Volumes":{},
		"WorkingDir":"",
		"Entrypoint":null,
		"NetworkDisabled":false,
		"OnBuild":null}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusCreated)

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	type createResp struct {
		ID string
	}
	var container createResp
	c.Assert(json.Unmarshal(b, &container), checker.IsNil)
	out := inspectField(c, container.ID, "HostConfig.CpusetCpus")
	assert.Equal(c, out, "")

	outMemory := inspectField(c, container.ID, "HostConfig.Memory")
	c.Assert(outMemory, checker.Equals, "0")
	outMemorySwap := inspectField(c, container.ID, "HostConfig.MemorySwap")
	c.Assert(outMemorySwap, checker.Equals, "0")
}

func (s *DockerSuite) TestCreateWithTooLowMemoryLimit(c *check.C) {
	// TODO Windows: Port once memory is supported
	testRequires(c, DaemonIsLinux)
	config := `{
		"Image":     "busybox",
		"Cmd":       "ls",
		"OpenStdin": true,
		"CpuShares": 100,
		"Memory":    524287
	}`

	res, body, err := request.Post("/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	b, err2 := request.ReadBody(body)
	c.Assert(err2, checker.IsNil)

	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}
	c.Assert(string(b), checker.Contains, "Minimum memory limit allowed is 4MB")
}

func (s *DockerSuite) TestContainerAPIRename(c *check.C) {
	out, _ := dockerCmd(c, "run", "--name", "TestContainerAPIRename", "-d", "busybox", "sh")

	containerID := strings.TrimSpace(out)
	newName := "TestContainerAPIRenameNew"

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRename(context.Background(), containerID, newName)
	assert.NilError(c, err)

	name := inspectField(c, containerID, "Name")
	c.Assert(name, checker.Equals, "/"+newName, check.Commentf("Failed to rename container"))
}

func (s *DockerSuite) TestContainerAPIKill(c *check.C) {
	name := "test-api-kill"
	runSleepingContainer(c, "-i", "--name", name)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerKill(context.Background(), name, "SIGKILL")
	assert.NilError(c, err)

	state := inspectField(c, name, "State.Running")
	c.Assert(state, checker.Equals, "false", check.Commentf("got wrong State from container %s: %q", name, state))
}

func (s *DockerSuite) TestContainerAPIRestart(c *check.C) {
	name := "test-api-restart"
	runSleepingContainer(c, "-di", "--name", name)
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	timeout := 1 * time.Second
	err = cli.ContainerRestart(context.Background(), name, &timeout)
	assert.NilError(c, err)

	c.Assert(waitInspect(name, "{{ .State.Restarting  }} {{ .State.Running  }}", "false true", 15*time.Second), checker.IsNil)
}

func (s *DockerSuite) TestContainerAPIRestartNotimeoutParam(c *check.C) {
	name := "test-api-restart-no-timeout-param"
	out := runSleepingContainer(c, "-di", "--name", name)
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRestart(context.Background(), name, nil)
	assert.NilError(c, err)

	c.Assert(waitInspect(name, "{{ .State.Restarting  }} {{ .State.Running  }}", "false true", 15*time.Second), checker.IsNil)
}

func (s *DockerSuite) TestContainerAPIStart(c *check.C) {
	name := "testing-start"
	config := containertypes.Config{
		Image:     "busybox",
		Cmd:       append([]string{"/bin/sh", "-c"}, sleepCommandForDaemonPlatform()...),
		OpenStdin: true,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, name)
	assert.NilError(c, err)

	err = cli.ContainerStart(context.Background(), name, types.ContainerStartOptions{})
	assert.NilError(c, err)

	// second call to start should give 304
	// maybe add ContainerStartWithRaw to test it
	err = cli.ContainerStart(context.Background(), name, types.ContainerStartOptions{})
	assert.NilError(c, err)

	// TODO(tibor): figure out why this doesn't work on windows
}

func (s *DockerSuite) TestContainerAPIStop(c *check.C) {
	name := "test-api-stop"
	runSleepingContainer(c, "-i", "--name", name)
	timeout := 30 * time.Second

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerStop(context.Background(), name, &timeout)
	assert.NilError(c, err)
	c.Assert(waitInspect(name, "{{ .State.Running  }}", "false", 60*time.Second), checker.IsNil)

	// second call to start should give 304
	// maybe add ContainerStartWithRaw to test it
	err = cli.ContainerStop(context.Background(), name, &timeout)
	assert.NilError(c, err)
}

func (s *DockerSuite) TestContainerAPIWait(c *check.C) {
	name := "test-api-wait"

	sleepCmd := "/bin/sleep"
	if testEnv.OSType == "windows" {
		sleepCmd = "sleep"
	}
	dockerCmd(c, "run", "--name", name, "busybox", sleepCmd, "2")

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	waitresC, errC := cli.ContainerWait(context.Background(), name, "")

	select {
	case err = <-errC:
		assert.NilError(c, err)
	case waitres := <-waitresC:
		c.Assert(waitres.StatusCode, checker.Equals, int64(0))
	}
}

func (s *DockerSuite) TestContainerAPICopyNotExistsAnyMore(c *check.C) {
	name := "test-container-api-copy"
	dockerCmd(c, "run", "--name", name, "busybox", "touch", "/test.txt")

	postData := types.CopyConfig{
		Resource: "/test.txt",
	}
	// no copy in client/
	res, _, err := request.Post("/containers/"+name+"/copy", request.JSONBody(postData))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusNotFound)
}

func (s *DockerSuite) TestContainerAPICopyPre124(c *check.C) {
	testRequires(c, DaemonIsLinux) // Windows only supports 1.25 or later
	name := "test-container-api-copy"
	dockerCmd(c, "run", "--name", name, "busybox", "touch", "/test.txt")

	postData := types.CopyConfig{
		Resource: "/test.txt",
	}

	res, body, err := request.Post("/v1.23/containers/"+name+"/copy", request.JSONBody(postData))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusOK)

	found := false
	for tarReader := tar.NewReader(body); ; {
		h, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			c.Fatal(err)
		}
		if h.Name == "test.txt" {
			found = true
			break
		}
	}
	c.Assert(found, checker.True)
}

func (s *DockerSuite) TestContainerAPICopyResourcePathEmptyPre124(c *check.C) {
	testRequires(c, DaemonIsLinux) // Windows only supports 1.25 or later
	name := "test-container-api-copy-resource-empty"
	dockerCmd(c, "run", "--name", name, "busybox", "touch", "/test.txt")

	postData := types.CopyConfig{
		Resource: "",
	}

	res, body, err := request.Post("/v1.23/containers/"+name+"/copy", request.JSONBody(postData))
	assert.NilError(c, err)
	if versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	} else {
		c.Assert(res.StatusCode, checker.Not(checker.Equals), http.StatusOK)
	}
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b), checker.Matches, "Path cannot be empty\n")
}

func (s *DockerSuite) TestContainerAPICopyResourcePathNotFoundPre124(c *check.C) {
	testRequires(c, DaemonIsLinux) // Windows only supports 1.25 or later
	name := "test-container-api-copy-resource-not-found"
	dockerCmd(c, "run", "--name", name, "busybox")

	postData := types.CopyConfig{
		Resource: "/notexist",
	}

	res, body, err := request.Post("/v1.23/containers/"+name+"/copy", request.JSONBody(postData))
	assert.NilError(c, err)
	if versions.LessThan(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, res.StatusCode, http.StatusInternalServerError)
	} else {
		assert.Equal(c, res.StatusCode, http.StatusNotFound)
	}
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	c.Assert(string(b), checker.Matches, "Could not find the file /notexist in container "+name+"\n")
}

func (s *DockerSuite) TestContainerAPICopyContainerNotFoundPr124(c *check.C) {
	testRequires(c, DaemonIsLinux) // Windows only supports 1.25 or later
	postData := types.CopyConfig{
		Resource: "/something",
	}

	res, _, err := request.Post("/v1.23/containers/notexists/copy", request.JSONBody(postData))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusNotFound)
}

func (s *DockerSuite) TestContainerAPIDelete(c *check.C) {
	out := runSleepingContainer(c)

	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	dockerCmd(c, "stop", id)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{})
	assert.NilError(c, err)
}

func (s *DockerSuite) TestContainerAPIDeleteNotExist(c *check.C) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), "doesnotexist", types.ContainerRemoveOptions{})
	assert.ErrorContains(c, err, "No such container: doesnotexist")
}

func (s *DockerSuite) TestContainerAPIDeleteForce(c *check.C) {
	out := runSleepingContainer(c)
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	removeOptions := types.ContainerRemoveOptions{
		Force: true,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), id, removeOptions)
	assert.NilError(c, err)
}

func (s *DockerSuite) TestContainerAPIDeleteRemoveLinks(c *check.C) {
	// Windows does not support links
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "--name", "tlink1", "busybox", "top")

	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	out, _ = dockerCmd(c, "run", "--link", "tlink1:tlink1", "--name", "tlink2", "-d", "busybox", "top")

	id2 := strings.TrimSpace(out)
	c.Assert(waitRun(id2), checker.IsNil)

	links := inspectFieldJSON(c, id2, "HostConfig.Links")
	c.Assert(links, checker.Equals, "[\"/tlink1:/tlink2/tlink1\"]", check.Commentf("expected to have links between containers"))

	removeOptions := types.ContainerRemoveOptions{
		RemoveLinks: true,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), "tlink2/tlink1", removeOptions)
	assert.NilError(c, err)

	linksPostRm := inspectFieldJSON(c, id2, "HostConfig.Links")
	c.Assert(linksPostRm, checker.Equals, "null", check.Commentf("call to api deleteContainer links should have removed the specified links"))
}

func (s *DockerSuite) TestContainerAPIDeleteConflict(c *check.C) {
	out := runSleepingContainer(c)

	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{})
	expected := "cannot remove a running container"
	assert.ErrorContains(c, err, expected)
}

func (s *DockerSuite) TestContainerAPIDeleteRemoveVolume(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon)

	vol := "/testvolume"
	if testEnv.OSType == "windows" {
		vol = `c:\testvolume`
	}

	out := runSleepingContainer(c, "-v", vol)

	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	source, err := inspectMountSourceField(id, vol)
	assert.NilError(c, err)
	_, err = os.Stat(source)
	assert.NilError(c, err)

	removeOptions := types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), id, removeOptions)
	assert.NilError(c, err)

	_, err = os.Stat(source)
	c.Assert(os.IsNotExist(err), checker.True, check.Commentf("expected to get ErrNotExist error, got %v", err))
}

// Regression test for https://github.com/docker/docker/issues/6231
func (s *DockerSuite) TestContainerAPIChunkedEncoding(c *check.C) {

	config := map[string]interface{}{
		"Image":     "busybox",
		"Cmd":       append([]string{"/bin/sh", "-c"}, sleepCommandForDaemonPlatform()...),
		"OpenStdin": true,
	}

	resp, _, err := request.Post("/containers/create", request.JSONBody(config), request.With(func(req *http.Request) error {
		// This is a cheat to make the http request do chunked encoding
		// Otherwise (just setting the Content-Encoding to chunked) net/http will overwrite
		// https://golang.org/src/pkg/net/http/request.go?s=11980:12172
		req.ContentLength = -1
		return nil
	}))
	c.Assert(err, checker.IsNil, check.Commentf("error creating container with chunked encoding"))
	defer resp.Body.Close()
	assert.Equal(c, resp.StatusCode, http.StatusCreated)
}

func (s *DockerSuite) TestContainerAPIPostContainerStop(c *check.C) {
	out := runSleepingContainer(c)

	containerID := strings.TrimSpace(out)
	c.Assert(waitRun(containerID), checker.IsNil)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerStop(context.Background(), containerID, nil)
	assert.NilError(c, err)
	c.Assert(waitInspect(containerID, "{{ .State.Running  }}", "false", 60*time.Second), checker.IsNil)
}

// #14170
func (s *DockerSuite) TestPostContainerAPICreateWithStringOrSliceEntrypoint(c *check.C) {
	config := containertypes.Config{
		Image:      "busybox",
		Entrypoint: []string{"echo"},
		Cmd:        []string{"hello", "world"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "echotest")
	assert.NilError(c, err)
	out, _ := dockerCmd(c, "start", "-a", "echotest")
	assert.Equal(c, strings.TrimSpace(out), "hello world")

	config2 := struct {
		Image      string
		Entrypoint string
		Cmd        []string
	}{"busybox", "echo", []string{"hello", "world"}}
	_, _, err = request.Post("/containers/create?name=echotest2", request.JSONBody(config2))
	assert.NilError(c, err)
	out, _ = dockerCmd(c, "start", "-a", "echotest2")
	assert.Equal(c, strings.TrimSpace(out), "hello world")
}

// #14170
func (s *DockerSuite) TestPostContainersCreateWithStringOrSliceCmd(c *check.C) {
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"echo", "hello", "world"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "echotest")
	assert.NilError(c, err)
	out, _ := dockerCmd(c, "start", "-a", "echotest")
	assert.Equal(c, strings.TrimSpace(out), "hello world")

	config2 := struct {
		Image      string
		Entrypoint string
		Cmd        string
	}{"busybox", "echo", "hello world"}
	_, _, err = request.Post("/containers/create?name=echotest2", request.JSONBody(config2))
	assert.NilError(c, err)
	out, _ = dockerCmd(c, "start", "-a", "echotest2")
	assert.Equal(c, strings.TrimSpace(out), "hello world")
}

// regression #14318
// for backward compatibility testing with and without CAP_ prefix
// and with upper and lowercase
func (s *DockerSuite) TestPostContainersCreateWithStringOrSliceCapAddDrop(c *check.C) {
	// Windows doesn't support CapAdd/CapDrop
	testRequires(c, DaemonIsLinux)
	config := struct {
		Image   string
		CapAdd  string
		CapDrop string
	}{"busybox", "NET_ADMIN", "cap_sys_admin"}
	res, _, err := request.Post("/containers/create?name=capaddtest0", request.JSONBody(config))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusCreated)

	config2 := containertypes.Config{
		Image: "busybox",
	}
	hostConfig := containertypes.HostConfig{
		CapAdd:  []string{"net_admin", "SYS_ADMIN"},
		CapDrop: []string{"SETGID", "CAP_SETPCAP"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config2, &hostConfig, &networktypes.NetworkingConfig{}, "capaddtest1")
	assert.NilError(c, err)
}

// #14915
func (s *DockerSuite) TestContainerAPICreateNoHostConfig118(c *check.C) {
	testRequires(c, DaemonIsLinux) // Windows only support 1.25 or later
	config := containertypes.Config{
		Image: "busybox",
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("v1.18"))
	assert.NilError(c, err)

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)
}

// Ensure an error occurs when you have a container read-only rootfs but you
// extract an archive to a symlink in a writable volume which points to a
// directory outside of the volume.
func (s *DockerSuite) TestPutContainerArchiveErrSymlinkInVolumeToReadOnlyRootfs(c *check.C) {
	// Windows does not support read-only rootfs
	// Requires local volume mount bind.
	// --read-only + userns has remount issues
	testRequires(c, testEnv.IsLocalDaemon, NotUserNamespace, DaemonIsLinux)

	testVol := getTestDir(c, "test-put-container-archive-err-symlink-in-volume-to-read-only-rootfs-")
	defer os.RemoveAll(testVol)

	makeTestContentInDir(c, testVol)

	cID := makeTestContainer(c, testContainerOptions{
		readOnly: true,
		volumes:  defaultVolumes(testVol), // Our bind mount is at /vol2
	})

	// Attempt to extract to a symlink in the volume which points to a
	// directory outside the volume. This should cause an error because the
	// rootfs is read-only.
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("v1.20"))
	assert.NilError(c, err)

	err = cli.CopyToContainer(context.Background(), cID, "/vol2/symlinkToAbsDir", nil, types.CopyToContainerOptions{})
	assert.ErrorContains(c, err, "container rootfs is marked read-only")
}

func (s *DockerSuite) TestPostContainersCreateWithWrongCpusetValues(c *check.C) {
	// Not supported on Windows
	testRequires(c, DaemonIsLinux)

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	config := containertypes.Config{
		Image: "busybox",
	}
	hostConfig1 := containertypes.HostConfig{
		Resources: containertypes.Resources{
			CpusetCpus: "1-42,,",
		},
	}
	name := "wrong-cpuset-cpus"

	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig1, &networktypes.NetworkingConfig{}, name)
	expected := "Invalid value 1-42,, for cpuset cpus"
	assert.ErrorContains(c, err, expected)

	hostConfig2 := containertypes.HostConfig{
		Resources: containertypes.Resources{
			CpusetMems: "42-3,1--",
		},
	}
	name = "wrong-cpuset-mems"
	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig2, &networktypes.NetworkingConfig{}, name)
	expected = "Invalid value 42-3,1-- for cpuset mems"
	assert.ErrorContains(c, err, expected)
}

func (s *DockerSuite) TestPostContainersCreateShmSizeNegative(c *check.C) {
	// ShmSize is not supported on Windows
	testRequires(c, DaemonIsLinux)
	config := containertypes.Config{
		Image: "busybox",
	}
	hostConfig := containertypes.HostConfig{
		ShmSize: -1,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "")
	assert.ErrorContains(c, err, "SHM size can not be less than 0")
}

func (s *DockerSuite) TestPostContainersCreateShmSizeHostConfigOmitted(c *check.C) {
	// ShmSize is not supported on Windows
	testRequires(c, DaemonIsLinux)
	var defaultSHMSize int64 = 67108864
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"mount"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	c.Assert(containerJSON.HostConfig.ShmSize, check.Equals, defaultSHMSize)

	out, _ := dockerCmd(c, "start", "-i", containerJSON.ID)
	shmRegexp := regexp.MustCompile(`shm on /dev/shm type tmpfs(.*)size=65536k`)
	if !shmRegexp.MatchString(out) {
		c.Fatalf("Expected shm of 64MB in mount command, got %v", out)
	}
}

func (s *DockerSuite) TestPostContainersCreateShmSizeOmitted(c *check.C) {
	// ShmSize is not supported on Windows
	testRequires(c, DaemonIsLinux)
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"mount"},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	c.Assert(containerJSON.HostConfig.ShmSize, check.Equals, int64(67108864))

	out, _ := dockerCmd(c, "start", "-i", containerJSON.ID)
	shmRegexp := regexp.MustCompile(`shm on /dev/shm type tmpfs(.*)size=65536k`)
	if !shmRegexp.MatchString(out) {
		c.Fatalf("Expected shm of 64MB in mount command, got %v", out)
	}
}

func (s *DockerSuite) TestPostContainersCreateWithShmSize(c *check.C) {
	// ShmSize is not supported on Windows
	testRequires(c, DaemonIsLinux)
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"mount"},
	}

	hostConfig := containertypes.HostConfig{
		ShmSize: 1073741824,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	c.Assert(containerJSON.HostConfig.ShmSize, check.Equals, int64(1073741824))

	out, _ := dockerCmd(c, "start", "-i", containerJSON.ID)
	shmRegex := regexp.MustCompile(`shm on /dev/shm type tmpfs(.*)size=1048576k`)
	if !shmRegex.MatchString(out) {
		c.Fatalf("Expected shm of 1GB in mount command, got %v", out)
	}
}

func (s *DockerSuite) TestPostContainersCreateMemorySwappinessHostConfigOmitted(c *check.C) {
	// Swappiness is not supported on Windows
	testRequires(c, DaemonIsLinux)
	config := containertypes.Config{
		Image: "busybox",
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	container, err := cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, "")
	assert.NilError(c, err)

	containerJSON, err := cli.ContainerInspect(context.Background(), container.ID)
	assert.NilError(c, err)

	if versions.LessThan(testEnv.DaemonAPIVersion(), "1.31") {
		c.Assert(*containerJSON.HostConfig.MemorySwappiness, check.Equals, int64(-1))
	} else {
		c.Assert(containerJSON.HostConfig.MemorySwappiness, check.IsNil)
	}
}

// check validation is done daemon side and not only in cli
func (s *DockerSuite) TestPostContainersCreateWithOomScoreAdjInvalidRange(c *check.C) {
	// OomScoreAdj is not supported on Windows
	testRequires(c, DaemonIsLinux)

	config := containertypes.Config{
		Image: "busybox",
	}

	hostConfig := containertypes.HostConfig{
		OomScoreAdj: 1001,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	name := "oomscoreadj-over"
	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, name)

	expected := "Invalid value 1001, range for oom score adj is [-1000, 1000]"
	assert.ErrorContains(c, err, expected)

	hostConfig = containertypes.HostConfig{
		OomScoreAdj: -1001,
	}

	name = "oomscoreadj-low"
	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, name)

	expected = "Invalid value -1001, range for oom score adj is [-1000, 1000]"
	assert.ErrorContains(c, err, expected)
}

// test case for #22210 where an empty container name caused panic.
func (s *DockerSuite) TestContainerAPIDeleteWithEmptyName(c *check.C) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	err = cli.ContainerRemove(context.Background(), "", types.ContainerRemoveOptions{})
	assert.ErrorContains(c, err, "No such container")
}

func (s *DockerSuite) TestContainerAPIStatsWithNetworkDisabled(c *check.C) {
	// Problematic on Windows as Windows does not support stats
	testRequires(c, DaemonIsLinux)

	name := "testing-network-disabled"

	config := containertypes.Config{
		Image:           "busybox",
		Cmd:             []string{"top"},
		NetworkDisabled: true,
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &containertypes.HostConfig{}, &networktypes.NetworkingConfig{}, name)
	assert.NilError(c, err)

	err = cli.ContainerStart(context.Background(), name, types.ContainerStartOptions{})
	assert.NilError(c, err)

	c.Assert(waitRun(name), check.IsNil)

	type b struct {
		stats types.ContainerStats
		err   error
	}
	bc := make(chan b, 1)
	go func() {
		stats, err := cli.ContainerStats(context.Background(), name, false)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	dockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		c.Assert(sr.err, checker.IsNil)
		sr.stats.Body.Close()
	}
}

func (s *DockerSuite) TestContainersAPICreateMountsValidation(c *check.C) {
	type testCase struct {
		config     containertypes.Config
		hostConfig containertypes.HostConfig
		msg        string
	}

	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	destPath := prefix + slash + "foo"
	notExistPath := prefix + slash + "notexist"

	cases := []testCase{
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type:   "notreal",
					Target: destPath,
				},
				},
			},

			msg: "mount type unknown",
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type: "bind"}}},
			msg: "Target must not be empty",
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type:   "bind",
					Target: destPath}}},
			msg: "Source must not be empty",
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type:   "bind",
					Source: notExistPath,
					Target: destPath}}},
			msg: "source path does not exist",
			// FIXME(vdemeester) fails into e2e, migrate to integration/container anyway
			// msg: "source path does not exist: " + notExistPath,
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type: "volume"}}},
			msg: "Target must not be empty",
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type:   "volume",
					Source: "hello",
					Target: destPath}}},
			msg: "",
		},
		{
			config: containertypes.Config{
				Image: "busybox",
			},
			hostConfig: containertypes.HostConfig{
				Mounts: []mounttypes.Mount{{
					Type:   "volume",
					Source: "hello2",
					Target: destPath,
					VolumeOptions: &mounttypes.VolumeOptions{
						DriverConfig: &mounttypes.Driver{
							Name: "local"}}}}},
			msg: "",
		},
	}

	if testEnv.IsLocalDaemon() {
		tmpDir, err := ioutils.TempDir("", "test-mounts-api")
		assert.NilError(c, err)
		defer os.RemoveAll(tmpDir)
		cases = append(cases, []testCase{
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{{
						Type:   "bind",
						Source: tmpDir,
						Target: destPath}}},
				msg: "",
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{{
						Type:          "bind",
						Source:        tmpDir,
						Target:        destPath,
						VolumeOptions: &mounttypes.VolumeOptions{}}}},
				msg: "VolumeOptions must not be specified",
			},
		}...)
	}

	if DaemonIsWindows() {
		cases = append(cases, []testCase{
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{
						{
							Type:   "volume",
							Source: "not-supported-on-windows",
							Target: destPath,
							VolumeOptions: &mounttypes.VolumeOptions{
								DriverConfig: &mounttypes.Driver{
									Name:    "local",
									Options: map[string]string{"type": "tmpfs"},
								},
							},
						},
					},
				},
				msg: `options are not supported on this platform`,
			},
		}...)
	}

	if DaemonIsLinux() {
		cases = append(cases, []testCase{
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{
						{
							Type:   "volume",
							Source: "missing-device-opt",
							Target: destPath,
							VolumeOptions: &mounttypes.VolumeOptions{
								DriverConfig: &mounttypes.Driver{
									Name:    "local",
									Options: map[string]string{"foobar": "foobaz"},
								},
							},
						},
					},
				},
				msg: `invalid option: "foobar"`,
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{
						{
							Type:   "volume",
							Source: "missing-device-opt",
							Target: destPath,
							VolumeOptions: &mounttypes.VolumeOptions{
								DriverConfig: &mounttypes.Driver{
									Name:    "local",
									Options: map[string]string{"type": "tmpfs"},
								},
							},
						},
					},
				},
				msg: `missing required option: "device"`,
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{
						{
							Type:   "volume",
							Source: "missing-type-opt",
							Target: destPath,
							VolumeOptions: &mounttypes.VolumeOptions{
								DriverConfig: &mounttypes.Driver{
									Name:    "local",
									Options: map[string]string{"device": "tmpfs"},
								},
							},
						},
					},
				},
				msg: `missing required option: "type"`,
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{
						{
							Type:   "volume",
							Source: "hello4",
							Target: destPath,
							VolumeOptions: &mounttypes.VolumeOptions{
								DriverConfig: &mounttypes.Driver{
									Name:    "local",
									Options: map[string]string{"o": "size=1", "type": "tmpfs", "device": "tmpfs"},
								},
							},
						},
					},
				},
				msg: "",
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{{
						Type:   "tmpfs",
						Target: destPath}}},
				msg: "",
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{{
						Type:   "tmpfs",
						Target: destPath,
						TmpfsOptions: &mounttypes.TmpfsOptions{
							SizeBytes: 4096 * 1024,
							Mode:      0700,
						}}}},
				msg: "",
			},
			{
				config: containertypes.Config{
					Image: "busybox",
				},
				hostConfig: containertypes.HostConfig{
					Mounts: []mounttypes.Mount{{
						Type:   "tmpfs",
						Source: "/shouldnotbespecified",
						Target: destPath}}},
				msg: "Source must not be specified",
			},
		}...)

	}
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	// TODO add checks for statuscode returned by API
	for i, x := range cases {
		c.Logf("case %d", i)
		_, err = cli.ContainerCreate(context.Background(), &x.config, &x.hostConfig, &networktypes.NetworkingConfig{}, "")
		if len(x.msg) > 0 {
			assert.ErrorContains(c, err, x.msg, "%v", cases[i].config)
		} else {
			assert.NilError(c, err)
		}
	}
}

func (s *DockerSuite) TestContainerAPICreateMountsBindRead(c *check.C) {
	testRequires(c, NotUserNamespace, testEnv.IsLocalDaemon)
	// also with data in the host side
	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	destPath := prefix + slash + "foo"
	tmpDir, err := ioutil.TempDir("", "test-mounts-api-bind")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)
	err = ioutil.WriteFile(filepath.Join(tmpDir, "bar"), []byte("hello"), 666)
	assert.NilError(c, err)
	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "cat /foo/bar"},
	}
	hostConfig := containertypes.HostConfig{
		Mounts: []mounttypes.Mount{
			{Type: "bind", Source: tmpDir, Target: destPath},
		},
	}
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, "test")
	assert.NilError(c, err)

	out, _ := dockerCmd(c, "start", "-a", "test")
	assert.Equal(c, out, "hello")
}

// Test Mounts comes out as expected for the MountPoint
func (s *DockerSuite) TestContainersAPICreateMountsCreate(c *check.C) {
	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	destPath := prefix + slash + "foo"

	var (
		testImg string
	)
	if testEnv.OSType != "windows" {
		testImg = "test-mount-config"
		buildImageSuccessfully(c, testImg, build.WithDockerfile(`
	FROM busybox
	RUN mkdir `+destPath+` && touch `+destPath+slash+`bar
	CMD cat `+destPath+slash+`bar
	`))
	} else {
		testImg = "busybox"
	}

	type testCase struct {
		spec     mounttypes.Mount
		expected types.MountPoint
	}

	var selinuxSharedLabel string
	// this test label was added after a bug fix in 1.32, thus add requirements min API >= 1.32
	// for the sake of making test pass in earlier versions
	// bug fixed in https://github.com/moby/moby/pull/34684
	if !versions.LessThan(testEnv.DaemonAPIVersion(), "1.32") {
		if runtime.GOOS == "linux" {
			selinuxSharedLabel = "z"
		}
	}

	cases := []testCase{
		// use literal strings here for `Type` instead of the defined constants in the volume package to keep this honest
		// Validation of the actual `Mount` struct is done in another test is not needed here
		{
			spec:     mounttypes.Mount{Type: "volume", Target: destPath},
			expected: types.MountPoint{Driver: volume.DefaultDriverName, Type: "volume", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
		},
		{
			spec:     mounttypes.Mount{Type: "volume", Target: destPath + slash},
			expected: types.MountPoint{Driver: volume.DefaultDriverName, Type: "volume", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
		},
		{
			spec:     mounttypes.Mount{Type: "volume", Target: destPath, Source: "test1"},
			expected: types.MountPoint{Type: "volume", Name: "test1", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
		},
		{
			spec:     mounttypes.Mount{Type: "volume", Target: destPath, ReadOnly: true, Source: "test2"},
			expected: types.MountPoint{Type: "volume", Name: "test2", RW: false, Destination: destPath, Mode: selinuxSharedLabel},
		},
		{
			spec:     mounttypes.Mount{Type: "volume", Target: destPath, Source: "test3", VolumeOptions: &mounttypes.VolumeOptions{DriverConfig: &mounttypes.Driver{Name: volume.DefaultDriverName}}},
			expected: types.MountPoint{Driver: volume.DefaultDriverName, Type: "volume", Name: "test3", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
		},
	}

	if testEnv.IsLocalDaemon() {
		// setup temp dir for testing binds
		tmpDir1, err := ioutil.TempDir("", "test-mounts-api-1")
		assert.NilError(c, err)
		defer os.RemoveAll(tmpDir1)
		cases = append(cases, []testCase{
			{
				spec: mounttypes.Mount{
					Type:   "bind",
					Source: tmpDir1,
					Target: destPath,
				},
				expected: types.MountPoint{
					Type:        "bind",
					RW:          true,
					Destination: destPath,
					Source:      tmpDir1,
				},
			},
			{
				spec:     mounttypes.Mount{Type: "bind", Source: tmpDir1, Target: destPath, ReadOnly: true},
				expected: types.MountPoint{Type: "bind", RW: false, Destination: destPath, Source: tmpDir1},
			},
		}...)

		// for modes only supported on Linux
		if DaemonIsLinux() {
			tmpDir3, err := ioutils.TempDir("", "test-mounts-api-3")
			assert.NilError(c, err)
			defer os.RemoveAll(tmpDir3)

			c.Assert(mount.Mount(tmpDir3, tmpDir3, "none", "bind,shared"), checker.IsNil)

			cases = append(cases, []testCase{
				{
					spec:     mounttypes.Mount{Type: "bind", Source: tmpDir3, Target: destPath},
					expected: types.MountPoint{Type: "bind", RW: true, Destination: destPath, Source: tmpDir3},
				},
				{
					spec:     mounttypes.Mount{Type: "bind", Source: tmpDir3, Target: destPath, ReadOnly: true},
					expected: types.MountPoint{Type: "bind", RW: false, Destination: destPath, Source: tmpDir3},
				},
				{
					spec:     mounttypes.Mount{Type: "bind", Source: tmpDir3, Target: destPath, ReadOnly: true, BindOptions: &mounttypes.BindOptions{Propagation: "shared"}},
					expected: types.MountPoint{Type: "bind", RW: false, Destination: destPath, Source: tmpDir3, Propagation: "shared"},
				},
			}...)
		}
	}

	if testEnv.OSType != "windows" { // Windows does not support volume populate
		cases = append(cases, []testCase{
			{
				spec:     mounttypes.Mount{Type: "volume", Target: destPath, VolumeOptions: &mounttypes.VolumeOptions{NoCopy: true}},
				expected: types.MountPoint{Driver: volume.DefaultDriverName, Type: "volume", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				spec:     mounttypes.Mount{Type: "volume", Target: destPath + slash, VolumeOptions: &mounttypes.VolumeOptions{NoCopy: true}},
				expected: types.MountPoint{Driver: volume.DefaultDriverName, Type: "volume", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				spec:     mounttypes.Mount{Type: "volume", Target: destPath, Source: "test4", VolumeOptions: &mounttypes.VolumeOptions{NoCopy: true}},
				expected: types.MountPoint{Type: "volume", Name: "test4", RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				spec:     mounttypes.Mount{Type: "volume", Target: destPath, Source: "test5", ReadOnly: true, VolumeOptions: &mounttypes.VolumeOptions{NoCopy: true}},
				expected: types.MountPoint{Type: "volume", Name: "test5", RW: false, Destination: destPath, Mode: selinuxSharedLabel},
			},
		}...)
	}

	type wrapper struct {
		containertypes.Config
		HostConfig containertypes.HostConfig
	}
	type createResp struct {
		ID string `json:"Id"`
	}

	ctx := context.Background()
	apiclient := testEnv.APIClient()
	for i, x := range cases {
		c.Logf("case %d - config: %v", i, x.spec)
		container, err := apiclient.ContainerCreate(
			ctx,
			&containertypes.Config{Image: testImg},
			&containertypes.HostConfig{Mounts: []mounttypes.Mount{x.spec}},
			&networktypes.NetworkingConfig{},
			"")
		assert.NilError(c, err)

		containerInspect, err := apiclient.ContainerInspect(ctx, container.ID)
		assert.NilError(c, err)
		mps := containerInspect.Mounts
		assert.Assert(c, is.Len(mps, 1))
		mountPoint := mps[0]

		if x.expected.Source != "" {
			assert.Check(c, is.Equal(x.expected.Source, mountPoint.Source))
		}
		if x.expected.Name != "" {
			assert.Check(c, is.Equal(x.expected.Name, mountPoint.Name))
		}
		if x.expected.Driver != "" {
			assert.Check(c, is.Equal(x.expected.Driver, mountPoint.Driver))
		}
		if x.expected.Propagation != "" {
			assert.Check(c, is.Equal(x.expected.Propagation, mountPoint.Propagation))
		}
		assert.Check(c, is.Equal(x.expected.RW, mountPoint.RW))
		assert.Check(c, is.Equal(x.expected.Type, mountPoint.Type))
		assert.Check(c, is.Equal(x.expected.Mode, mountPoint.Mode))
		assert.Check(c, is.Equal(x.expected.Destination, mountPoint.Destination))

		err = apiclient.ContainerStart(ctx, container.ID, types.ContainerStartOptions{})
		assert.NilError(c, err)
		poll.WaitOn(c, containerExit(apiclient, container.ID), poll.WithDelay(time.Second))

		err = apiclient.ContainerRemove(ctx, container.ID, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
		assert.NilError(c, err)

		switch {

		// Named volumes still exist after the container is removed
		case x.spec.Type == "volume" && len(x.spec.Source) > 0:
			_, err := apiclient.VolumeInspect(ctx, mountPoint.Name)
			assert.NilError(c, err)

		// Bind mounts are never removed with the container
		case x.spec.Type == "bind":

		// anonymous volumes are removed
		default:
			_, err := apiclient.VolumeInspect(ctx, mountPoint.Name)
			assert.Check(c, client.IsErrNotFound(err))
		}
	}
}

func containerExit(apiclient client.APIClient, name string) func(poll.LogT) poll.Result {
	return func(logT poll.LogT) poll.Result {
		container, err := apiclient.ContainerInspect(context.Background(), name)
		if err != nil {
			return poll.Error(err)
		}
		switch container.State.Status {
		case "created", "running":
			return poll.Continue("container %s is %s, waiting for exit", name, container.State.Status)
		}
		return poll.Success()
	}
}

func (s *DockerSuite) TestContainersAPICreateMountsTmpfs(c *check.C) {
	testRequires(c, DaemonIsLinux)
	type testCase struct {
		cfg             mounttypes.Mount
		expectedOptions []string
	}
	target := "/foo"
	cases := []testCase{
		{
			cfg: mounttypes.Mount{
				Type:   "tmpfs",
				Target: target},
			expectedOptions: []string{"rw", "nosuid", "nodev", "noexec", "relatime"},
		},
		{
			cfg: mounttypes.Mount{
				Type:   "tmpfs",
				Target: target,
				TmpfsOptions: &mounttypes.TmpfsOptions{
					SizeBytes: 4096 * 1024, Mode: 0700}},
			expectedOptions: []string{"rw", "nosuid", "nodev", "noexec", "relatime", "size=4096k", "mode=700"},
		},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	config := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", fmt.Sprintf("mount | grep 'tmpfs on %s'", target)},
	}
	for i, x := range cases {
		cName := fmt.Sprintf("test-tmpfs-%d", i)
		hostConfig := containertypes.HostConfig{
			Mounts: []mounttypes.Mount{x.cfg},
		}

		_, err = cli.ContainerCreate(context.Background(), &config, &hostConfig, &networktypes.NetworkingConfig{}, cName)
		assert.NilError(c, err)
		out, _ := dockerCmd(c, "start", "-a", cName)
		for _, option := range x.expectedOptions {
			c.Assert(out, checker.Contains, option)
		}
	}
}

// Regression test for #33334
// Makes sure that when a container which has a custom stop signal + restart=always
// gets killed (with SIGKILL) by the kill API, that the restart policy is cancelled.
func (s *DockerSuite) TestContainerKillCustomStopSignal(c *check.C) {
	id := strings.TrimSpace(runSleepingContainer(c, "--stop-signal=SIGTERM", "--restart=always"))
	res, _, err := request.Post("/containers/" + id + "/kill")
	assert.NilError(c, err)
	defer res.Body.Close()

	b, err := ioutil.ReadAll(res.Body)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusNoContent, string(b))
	err = waitInspect(id, "{{.State.Running}} {{.State.Restarting}}", "false false", 30*time.Second)
	assert.NilError(c, err)
}

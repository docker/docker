package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func (s *DockerAPISuite) TestContainerAPIGetAll(c *testing.T) {
	startCount := getContainerCount(c)
	const name = "getall"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "true")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	ctx := testutil.GetContext(c)
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	assert.NilError(c, err)
	assert.Equal(c, len(list.Items), startCount+1)
	actual := list.Items[0].Names[0]
	assert.Equal(c, actual, "/"+name)
}

// regression test for empty json field being omitted #13691
func (s *DockerAPISuite) TestContainerAPIGetJSONNoFieldsOmitted(c *testing.T) {
	startCount := getContainerCount(c)
	cli.DockerCmd(c, "run", "busybox", "true")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	options := client.ContainerListOptions{
		All: true,
	}
	ctx := testutil.GetContext(c)
	list, err := apiClient.ContainerList(ctx, options)
	assert.NilError(c, err)
	assert.Equal(c, len(list.Items), startCount+1)
	actual := fmt.Sprintf("%+v", list.Items[0])

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

func (s *DockerAPISuite) TestContainerAPIGetExport(c *testing.T) {
	// Not supported on Windows as Windows does not support docker export
	testRequires(c, DaemonIsLinux)
	const name = "exportcontainer"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "touch", "/test")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	res, err := apiClient.ContainerExport(testutil.GetContext(c), name, client.ContainerExportOptions{})
	assert.NilError(c, err)
	defer res.Close()
	found := false
	for tarReader := tar.NewReader(res); ; {
		h, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if h != nil && h.Name == "test" {
			found = true
			break
		}
	}
	assert.Assert(c, found, "The created test file has not been found in the exported image")
}

func (s *DockerAPISuite) TestContainerAPIGetChanges(c *testing.T) {
	// Not supported on Windows as Windows does not support docker diff (/containers/name/changes)
	testRequires(c, DaemonIsLinux)
	const name = "changescontainer"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "rm", "/etc/passwd")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	result, err := apiClient.ContainerDiff(testutil.GetContext(c), name, client.ContainerDiffOptions{})
	assert.NilError(c, err)

	// Check the changelog for removal of /etc/passwd
	success := false
	for _, elem := range result.Changes {
		if elem.Path == "/etc/passwd" && elem.Kind == 2 {
			success = true
		}
	}
	assert.Assert(c, success, "/etc/passwd has been removed but is not present in the diff")
}

func (s *DockerAPISuite) TestGetContainerStats(c *testing.T) {
	const name = "statscontainer"
	runSleepingContainer(c, "--name", name)

	type b struct {
		stats client.ContainerStatsResult
		err   error
	}

	bc := make(chan b, 1)
	go func() {
		apiClient, err := client.New(client.FromEnv)
		assert.NilError(c, err)
		defer apiClient.Close()

		stats, err := apiClient.ContainerStats(testutil.GetContext(c), name, client.ContainerStatsOptions{
			Stream: true,
		})
		assert.NilError(c, err)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	cli.DockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		dec := json.NewDecoder(sr.stats.Body)
		var s container.StatsResponse
		// decode only one object from the stream
		err := dec.Decode(&s)
		_ = sr.stats.Body.Close()
		assert.NilError(c, err)
	}
}

func (s *DockerAPISuite) TestGetContainerStatsRmRunning(c *testing.T) {
	id := runSleepingContainer(c)

	buf := &ChannelBuffer{C: make(chan []byte, 1)}
	defer buf.Close()

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	stats, err := apiClient.ContainerStats(testutil.GetContext(c), id, client.ContainerStatsOptions{
		Stream: true,
	})
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
	assert.Assert(c, err != nil, "rm should have failed but didn't")
	_, err = buf.ReadTimeout(b, 2*time.Second)
	assert.NilError(c, err)

	cli.DockerCmd(c, "rm", "-f", id)
	assert.Assert(c, is.Nil(<-chErr))
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
		return -1, errors.New("timeout reading from channel")
	}
}

// regression test for gh13421
// previous test was just checking one stat entry so it didn't fail (stats with
// stream false always return one stat)
func (s *DockerAPISuite) TestGetContainerStatsStream(c *testing.T) {
	const name = "statscontainer"
	runSleepingContainer(c, "--name", name)

	type b struct {
		stats client.ContainerStatsResult
		err   error
	}

	bc := make(chan b, 1)
	go func() {
		apiClient, err := client.New(client.FromEnv)
		assert.NilError(c, err)
		defer apiClient.Close()

		stats, err := apiClient.ContainerStats(testutil.GetContext(c), name, client.ContainerStatsOptions{
			Stream: true,
		})
		assert.NilError(c, err)
		bc <- b{stats, err}
	}()

	// allow some time to stream the stats from the container
	time.Sleep(4 * time.Second)
	cli.DockerCmd(c, "rm", "-f", name)

	// collect the results from the stats stream or timeout and fail
	// if the stream was not disconnected.
	select {
	case <-time.After(2 * time.Second):
		c.Fatal("stream was not closed after container was removed")
	case sr := <-bc:
		b, err := io.ReadAll(sr.stats.Body)
		defer sr.stats.Body.Close()
		assert.NilError(c, err)
		s := string(b)
		// count occurrences of "read" of types.Stats
		if l := strings.Count(s, "read"); l < 2 {
			c.Fatalf("Expected more than one stat streamed, got %d", l)
		}
	}
}

func (s *DockerAPISuite) TestGetContainerStatsNoStream(c *testing.T) {
	const name = "statscontainer2"
	cID := runSleepingContainer(c, "--name", name)
	defer cli.DockerCmd(c, "rm", "-f", cID)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	// We expect an immediate response, but if it's not immediate, the test would hang.
	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 10*time.Second)
	defer cancel()

	stats, err := apiClient.ContainerStats(ctx, cID, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	assert.NilError(c, err)
	defer func() { _ = stats.Body.Close() }()

	var v container.StatsResponse
	dec := json.NewDecoder(stats.Body)
	assert.NilError(c, dec.Decode(&v))
	assert.Check(c, is.Equal(v.ID, cID))
	err = dec.Decode(&v)
	assert.Check(c, is.ErrorIs(err, io.EOF), "expected only a single result")
}

func (s *DockerAPISuite) TestGetStoppedContainerStats(c *testing.T) {
	const name = "statscontainer3"
	cli.DockerCmd(c, "create", "--name", name, "busybox", "ps")
	defer cli.DockerCmd(c, "rm", "-f", name)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	// We expect an immediate response, but if it's not immediate, the test would hang.
	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 10*time.Second)
	defer cancel()

	stats, err := apiClient.ContainerStats(ctx, name, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	assert.NilError(c, err)
	defer func() { _ = stats.Body.Close() }()

	var v container.StatsResponse
	dec := json.NewDecoder(stats.Body)
	assert.NilError(c, dec.Decode(&v))
	assert.Check(c, v.ID != "", "expected non-empty stats response for stopped container: %+v", v)
	err = dec.Decode(&v)
	assert.Check(c, is.ErrorIs(err, io.EOF), "expected only a single result")
}

func (s *DockerAPISuite) TestContainerAPIPause(c *testing.T) {
	// Problematic on Windows as Windows does not support pause
	testRequires(c, DaemonIsLinux)

	getPaused := func(t *testing.T) []string {
		return strings.Fields(cli.DockerCmd(t, "ps", "-f", "status=paused", "-q", "-a").Combined())
	}

	out := cli.DockerCmd(c, "run", "-d", "busybox", "sleep", "30").Combined()
	ContainerID := strings.TrimSpace(out)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerPause(testutil.GetContext(c), ContainerID, client.ContainerPauseOptions{})
	assert.NilError(c, err)

	pausedContainers := getPaused(c)

	if len(pausedContainers) != 1 || stringid.TruncateID(ContainerID) != pausedContainers[0] {
		c.Fatalf("there should be one paused container and not %d", len(pausedContainers))
	}

	_, err = apiClient.ContainerUnpause(testutil.GetContext(c), ContainerID, client.ContainerUnpauseOptions{})
	assert.NilError(c, err)

	pausedContainers = getPaused(c)
	assert.Equal(c, len(pausedContainers), 0, "There should be no paused container.")
}

func (s *DockerAPISuite) TestContainerAPITop(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "top && true").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	// sort by comm[andline] to make sure order stays the same in case of PID rollover
	top, err := apiClient.ContainerTop(testutil.GetContext(c), id, client.ContainerTopOptions{Arguments: []string{"aux", "--sort=comm"}})
	assert.NilError(c, err)
	assert.Equal(c, len(top.Titles), 11, fmt.Sprintf("expected 11 titles, found %d: %v", len(top.Titles), top.Titles))

	if top.Titles[0] != "USER" || top.Titles[10] != "COMMAND" {
		c.Fatalf("expected `USER` at `Titles[0]` and `COMMAND` at Titles[10]: %v", top.Titles)
	}
	assert.Equal(c, len(top.Processes), 2, fmt.Sprintf("expected 2 processes, found %d: %v", len(top.Processes), top.Processes))
	assert.Equal(c, top.Processes[0][10], "/bin/sh -c top && true")
	assert.Equal(c, top.Processes[1][10], "top")
}

func (s *DockerAPISuite) TestContainerAPITopWindows(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	id := runSleepingContainer(c, "-d")
	cli.WaitRun(c, id)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	top, err := apiClient.ContainerTop(testutil.GetContext(c), id, client.ContainerTopOptions{})
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

func (s *DockerAPISuite) TestContainerAPICommit(c *testing.T) {
	const cName = "testapicommit"
	cli.DockerCmd(c, "run", "--name="+cName, "busybox", "/bin/sh", "-c", "touch /test")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	options := client.ContainerCommitOptions{
		Reference: "testcontainerapicommit:testtag",
	}

	img, err := apiClient.ContainerCommit(testutil.GetContext(c), cName, options)
	assert.NilError(c, err)

	cmd := inspectField(c, img.ID, "Config.Cmd")
	assert.Equal(c, cmd, "[/bin/sh -c touch /test]", fmt.Sprintf("got wrong Cmd from commit: %q", cmd))

	// sanity check, make sure the image is what we think it is
	cli.DockerCmd(c, "run", img.ID, "ls", "/test")
}

func (s *DockerAPISuite) TestContainerAPICommitWithLabelInConfig(c *testing.T) {
	const cName = "testapicommitwithconfig"
	cli.DockerCmd(c, "run", "--name="+cName, "busybox", "/bin/sh", "-c", "touch /test")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	config := container.Config{
		Labels: map[string]string{"key1": "value1", "key2": "value2"},
	}

	options := client.ContainerCommitOptions{
		Reference: "testcontainerapicommitwithconfig",
		Config:    &config,
	}

	img, err := apiClient.ContainerCommit(testutil.GetContext(c), cName, options)
	assert.NilError(c, err)

	imgInspect, err := apiClient.ImageInspect(testutil.GetContext(c), img.ID)
	assert.NilError(c, err)
	assert.Check(c, is.Equal(imgInspect.Config.Labels["key1"], "value1"))
	assert.Check(c, is.Equal(imgInspect.Config.Labels["key2"], "value2"))

	expected := []string{"/bin/sh", "-c", "touch /test"}
	assert.Check(c, is.DeepEqual(imgInspect.Config.Cmd, expected))

	// sanity check, make sure the image is what we think it is
	cli.DockerCmd(c, "run", img.ID, "ls", "/test")
}

func (s *DockerAPISuite) TestContainerAPIBadPort(c *testing.T) {
	// TODO Windows to Windows CI - Port this test
	testRequires(c, DaemonIsLinux)

	config := container.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "echo test"},
	}

	hostConfig := container.HostConfig{
		PortBindings: network.PortMap{
			network.MustParsePort("8080/tcp"): []network.PortBinding{
				{
					HostPort: "aa80",
				},
			},
		},
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.ErrorContains(c, err, `invalid port specification: "aa80"`)
}

func (s *DockerAPISuite) TestContainerAPICreate(c *testing.T) {
	config := container.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "touch /test && ls /test"},
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	ctr, err := apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(c, err)

	out := cli.DockerCmd(c, "start", "-a", ctr.ID).Stdout()
	assert.Equal(c, strings.TrimSpace(out), "/test")
}

func (s *DockerAPISuite) TestContainerAPICreateEmptyConfig(c *testing.T) {
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &container.Config{},
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
	})

	assert.ErrorContains(c, err, "config.Image or Image is required")
}

func (s *DockerAPISuite) TestContainerAPICreateBridgeNetworkMode(c *testing.T) {
	// Windows does not support bridge
	testRequires(c, DaemonIsLinux)
	UtilCreateNetworkMode(c, "bridge")
}

func (s *DockerAPISuite) TestContainerAPICreateOtherNetworkModes(c *testing.T) {
	// Windows does not support these network modes
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	UtilCreateNetworkMode(c, "host")
	UtilCreateNetworkMode(c, "container:web1")
}

func UtilCreateNetworkMode(t *testing.T, networkMode container.NetworkMode) {
	config := container.Config{
		Image: "busybox",
	}

	hostConfig := container.HostConfig{
		NetworkMode: networkMode,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(t, err)
	defer apiClient.Close()

	ctr, err := apiClient.ContainerCreate(testutil.GetContext(t), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(t, err)

	res, err := apiClient.ContainerInspect(testutil.GetContext(t), ctr.ID, client.ContainerInspectOptions{})
	assert.NilError(t, err)

	assert.Equal(t, res.Container.HostConfig.NetworkMode, networkMode, "Mismatched NetworkMode")
}

func (s *DockerAPISuite) TestContainerAPICreateWithCpuSharesCpuset(c *testing.T) {
	// TODO Windows to Windows CI. The CpuShares part could be ported.
	testRequires(c, DaemonIsLinux)
	config := container.Config{
		Image: "busybox",
	}

	hostConfig := container.HostConfig{
		Resources: container.Resources{
			CPUShares:  512,
			CpusetCpus: "0",
		},
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	ctr, err := apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(c, err)

	res, err := apiClient.ContainerInspect(testutil.GetContext(c), ctr.ID, client.ContainerInspectOptions{})
	assert.NilError(c, err)

	out := inspectField(c, res.Container.ID, "HostConfig.CpuShares")
	assert.Equal(c, out, "512")

	outCpuset := inspectField(c, res.Container.ID, "HostConfig.CpusetCpus")
	assert.Equal(c, outCpuset, "0")
}

// Issue 7941 - test to make sure a "null" in JSON is just ignored.
// W/o this fix a null in JSON would be parsed into a string var as "null"
func (s *DockerAPISuite) TestContainerAPIPostCreateNull(c *testing.T) {
	const config = `{
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
		"Cmd":["ls"],
		"Image":"busybox",
		"Volumes":{},
		"WorkingDir":"",
		"Entrypoint":null,
		"NetworkDisabled":false,
		"OnBuild":null}`

	res, body, err := request.Post(testutil.GetContext(c), "/containers/create", request.RawString(config), request.JSON)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusCreated)

	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	type createResp struct {
		ID string
	}
	var ctr createResp
	assert.NilError(c, json.Unmarshal(b, &ctr))
	out := inspectField(c, ctr.ID, "HostConfig.CpusetCpus")
	assert.Equal(c, out, "")

	outMemory := inspectField(c, ctr.ID, "HostConfig.Memory")
	assert.Equal(c, outMemory, "0")
	outMemorySwap := inspectField(c, ctr.ID, "HostConfig.MemorySwap")
	assert.Equal(c, outMemorySwap, "0")
}

func (s *DockerAPISuite) TestContainerAPIRestart(c *testing.T) {
	const name = "test-api-restart"
	runSleepingContainer(c, "-di", "--name", name)
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	timeout := 1
	_, err = apiClient.ContainerRestart(testutil.GetContext(c), name, client.ContainerRestartOptions{
		Timeout: &timeout,
	})
	assert.NilError(c, err)

	assert.NilError(c, waitInspect(name, "{{ .State.Restarting  }} {{ .State.Running  }}", "false true", 15*time.Second))
}

func (s *DockerAPISuite) TestContainerAPIRestartNotimeoutParam(c *testing.T) {
	const name = "test-api-restart-no-timeout-param"
	id := runSleepingContainer(c, "-di", "--name", name)
	cli.WaitRun(c, id)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRestart(testutil.GetContext(c), name, client.ContainerRestartOptions{})
	assert.NilError(c, err)

	assert.NilError(c, waitInspect(name, "{{ .State.Restarting  }} {{ .State.Running  }}", "false true", 15*time.Second))
}

func (s *DockerAPISuite) TestContainerAPIStart(c *testing.T) {
	const name = "testing-start"
	config := container.Config{
		Image:     "busybox",
		Cmd:       append([]string{"/bin/sh", "-c"}, sleepCommandForDaemonPlatform()...),
		OpenStdin: true,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name,
	})
	assert.NilError(c, err)

	_, err = apiClient.ContainerStart(testutil.GetContext(c), name, client.ContainerStartOptions{})
	assert.NilError(c, err)

	// second call to start should give 304
	// maybe add ContainerStartWithRaw to test it
	_, err = apiClient.ContainerStart(testutil.GetContext(c), name, client.ContainerStartOptions{})
	assert.NilError(c, err)

	// TODO(tibor): figure out why this doesn't work on windows
}

func (s *DockerAPISuite) TestContainerAPIWait(c *testing.T) {
	const name = "test-api-wait"

	sleepCmd := "/bin/sleep"
	if testEnv.DaemonInfo.OSType == "windows" {
		sleepCmd = "sleep"
	}
	cli.DockerCmd(c, "run", "--name", name, "busybox", sleepCmd, "2")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	wait := apiClient.ContainerWait(testutil.GetContext(c), name, client.ContainerWaitOptions{})

	select {
	case err = <-wait.Error:
		assert.NilError(c, err)
	case waitRes := <-wait.Result:
		assert.Equal(c, waitRes.StatusCode, int64(0))
	}
}

func (s *DockerAPISuite) TestContainerAPIDelete(c *testing.T) {
	id := runSleepingContainer(c)
	cli.WaitRun(c, id)
	cli.DockerCmd(c, "stop", id)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), id, client.ContainerRemoveOptions{})
	assert.NilError(c, err)
}

func (s *DockerAPISuite) TestContainerAPIDeleteNotExist(c *testing.T) {
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), "doesnotexist", client.ContainerRemoveOptions{})
	assert.ErrorContains(c, err, "No such container: doesnotexist")
}

func (s *DockerAPISuite) TestContainerAPIDeleteForce(c *testing.T) {
	id := runSleepingContainer(c)
	cli.WaitRun(c, id)

	removeOptions := client.ContainerRemoveOptions{
		Force: true,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), id, removeOptions)
	assert.NilError(c, err)
}

func (s *DockerAPISuite) TestContainerAPIDeleteRemoveLinks(c *testing.T) {
	// Windows does not support links
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-d", "--name", "tlink1", "busybox", "top").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)

	out = cli.DockerCmd(c, "run", "--link", "tlink1:tlink1", "--name", "tlink2", "-d", "busybox", "top").Stdout()
	id2 := strings.TrimSpace(out)
	cli.WaitRun(c, id2)

	links := inspectFieldJSON(c, id2, "HostConfig.Links")
	assert.Equal(c, links, `["/tlink1:/tlink2/tlink1"]`, "expected to have links between containers")

	removeOptions := client.ContainerRemoveOptions{
		RemoveLinks: true,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), "tlink2/tlink1", removeOptions)
	assert.NilError(c, err)

	linksPostRm := inspectFieldJSON(c, id2, "HostConfig.Links")
	assert.Equal(c, linksPostRm, "null", "call to api deleteContainer links should have removed the specified links")
}

func (s *DockerAPISuite) TestContainerAPIDeleteRemoveVolume(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	testVol := "/testvolume"
	if testEnv.DaemonInfo.OSType == "windows" {
		testVol = `c:\testvolume`
	}

	id := runSleepingContainer(c, "-v", testVol)
	cli.WaitRun(c, id)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	res, err := apiClient.ContainerInspect(testutil.GetContext(c), id, client.ContainerInspectOptions{})
	assert.NilError(c, err)
	assert.Assert(c, is.Len(res.Container.Mounts, 1), "expected to have 1 mount")
	mnt := res.Container.Mounts[0]
	assert.Equal(c, mnt.Destination, testVol)

	_, err = os.Stat(mnt.Source)
	assert.NilError(c, err)

	removeOptions := client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	}

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), id, removeOptions)
	assert.NilError(c, err)

	_, err = os.Stat(mnt.Source)
	assert.Assert(c, os.IsNotExist(err), "expected to get ErrNotExist error, got %v", err)
}

// Regression test for https://github.com/moby/moby/issues/6231
func (s *DockerAPISuite) TestContainerAPIChunkedEncoding(c *testing.T) {
	config := map[string]any{
		"Image":     "busybox",
		"Cmd":       append([]string{"/bin/sh", "-c"}, sleepCommandForDaemonPlatform()...),
		"OpenStdin": true,
	}

	resp, _, err := request.Post(testutil.GetContext(c), "/containers/create", request.JSONBody(config), request.With(func(req *http.Request) error {
		// This is a cheat to make the http request do chunked encoding
		// Otherwise (just setting the Content-Encoding to chunked) net/http will overwrite
		// https://golang.org/src/pkg/net/http/request.go?s=11980:12172
		req.ContentLength = -1
		return nil
	}))
	assert.NilError(c, err, "error creating container with chunked encoding")
	defer resp.Body.Close()
	assert.Equal(c, resp.StatusCode, http.StatusCreated)
}

// Ensure an error occurs when you have a container read-only rootfs but you
// extract an archive to a symlink in a writable volume which points to a
// directory outside of the volume.
func (s *DockerAPISuite) TestPutContainerArchiveErrSymlinkInVolumeToReadOnlyRootfs(c *testing.T) {
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
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)

	_, err = apiClient.CopyToContainer(testutil.GetContext(c), cID, client.CopyToContainerOptions{DestinationPath: "/vol2/symlinkToAbsDir"})
	assert.ErrorContains(c, err, "container rootfs is marked read-only")
}

func (s *DockerAPISuite) TestPostContainersCreateWithWrongCpusetValues(c *testing.T) {
	// Not supported on Windows
	testRequires(c, DaemonIsLinux)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	config := container.Config{
		Image: "busybox",
	}
	hostConfig1 := container.HostConfig{
		Resources: container.Resources{
			CpusetCpus: "1-42,,",
		},
	}
	const name = "wrong-cpuset-cpus"

	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig1,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name,
	})
	expected := "Invalid value 1-42,, for cpuset cpus"
	assert.ErrorContains(c, err, expected)

	hostConfig2 := container.HostConfig{
		Resources: container.Resources{
			CpusetMems: "42-3,1--",
		},
	}
	const name2 = "wrong-cpuset-mems"
	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig2,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name2,
	})
	expected = "Invalid value 42-3,1-- for cpuset mems"
	assert.ErrorContains(c, err, expected)
}

func (s *DockerAPISuite) TestPostContainersCreateMemorySwappinessHostConfigOmitted(c *testing.T) {
	// Swappiness is not supported on Windows
	testRequires(c, DaemonIsLinux)
	config := container.Config{
		Image: "busybox",
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	ctr, err := apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(c, err)

	res, err := apiClient.ContainerInspect(testutil.GetContext(c), ctr.ID, client.ContainerInspectOptions{})
	assert.NilError(c, err)

	assert.Assert(c, is.Nil(res.Container.HostConfig.MemorySwappiness))
}

// check validation is done daemon side and not only in cli
func (s *DockerAPISuite) TestPostContainersCreateWithOomScoreAdjInvalidRange(c *testing.T) {
	// OomScoreAdj is not supported on Windows
	testRequires(c, DaemonIsLinux)

	config := container.Config{
		Image: "busybox",
	}

	hostConfig := container.HostConfig{
		OomScoreAdj: 1001,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	const name = "oomscoreadj-over"
	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name,
	})

	expected := "Invalid value 1001, range for oom score adj is [-1000, 1000]"
	assert.ErrorContains(c, err, expected)

	hostConfig = container.HostConfig{
		OomScoreAdj: -1001,
	}

	const name2 = "oomscoreadj-low"
	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name2,
	})

	expected = "Invalid value -1001, range for oom score adj is [-1000, 1000]"
	assert.ErrorContains(c, err, expected)
}

// test case for #22210 where an empty container name caused panic.
func (s *DockerAPISuite) TestContainerAPIDeleteWithEmptyName(c *testing.T) {
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerRemove(testutil.GetContext(c), "", client.ContainerRemoveOptions{})
	assert.Check(c, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(c, is.ErrorContains(err, "value is empty"))
}

func (s *DockerAPISuite) TestContainerAPIStatsWithNetworkDisabled(c *testing.T) {
	// Problematic on Windows as Windows does not support stats
	testRequires(c, DaemonIsLinux)

	const name = "testing-network-disabled"

	config := container.Config{
		Image:           "busybox",
		Cmd:             []string{"top"},
		NetworkDisabled: true,
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	ctr, err := apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             name,
	})
	assert.NilError(c, err)
	defer cli.DockerCmd(c, "rm", "-f", name)

	_, err = apiClient.ContainerStart(testutil.GetContext(c), ctr.ID, client.ContainerStartOptions{})
	assert.NilError(c, err)
	cli.WaitRun(c, ctr.ID)

	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 10*time.Second)
	defer cancel()

	stats, err := apiClient.ContainerStats(ctx, name, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true,
	})
	assert.NilError(c, err)
	defer func() { _ = stats.Body.Close() }()

	var v container.StatsResponse
	dec := json.NewDecoder(stats.Body)
	assert.NilError(c, dec.Decode(&v))
	assert.Check(c, is.Equal(v.ID, ctr.ID))
	err = dec.Decode(&v)
	assert.Check(c, is.ErrorIs(err, io.EOF), "expected only a single result")
}

func (s *DockerAPISuite) TestContainersAPICreateMountsValidation(c *testing.T) {
	type testCase struct {
		config     container.Config
		hostConfig container.HostConfig
		msg        string
	}

	destPath := dPath("/foo")
	notExistPath := dPath("/notexist")

	tests := []testCase{
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type:   "notreal",
					Target: destPath,
				}},
			},

			msg: "mount type unknown",
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type: "bind",
				}},
			},
			msg: "Target must not be empty",
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type:   "bind",
					Target: destPath,
				}},
			},
			msg: "Source must not be empty",
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type:   "bind",
					Source: notExistPath,
					Target: destPath,
				}},
			},
			msg: "source path does not exist",
			// FIXME(vdemeester) fails into e2e, migrate to integration/container anyway
			// msg: "source path does not exist: " + notExistPath,
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type: "volume",
				}},
			},
			msg: "Target must not be empty",
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type:   "volume",
					Source: "hello",
					Target: destPath,
				}},
			},
			msg: "",
		},
		{
			config: container.Config{
				Image: "busybox",
			},
			hostConfig: container.HostConfig{
				Mounts: []mount.Mount{{
					Type:   "volume",
					Source: "hello2",
					Target: destPath,
					VolumeOptions: &mount.VolumeOptions{
						DriverConfig: &mount.Driver{
							Name: "local",
						},
					},
				}},
			},
			msg: "",
		},
	}

	if testEnv.IsLocalDaemon() {
		tmpDir, err := os.MkdirTemp("", "test-mounts-api")
		assert.NilError(c, err)
		defer os.RemoveAll(tmpDir)
		tests = append(tests, []testCase{
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{{
						Type:   "bind",
						Source: tmpDir,
						Target: destPath,
					}},
				},
				msg: "",
			},
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{{
						Type:          "bind",
						Source:        tmpDir,
						Target:        destPath,
						VolumeOptions: &mount.VolumeOptions{},
					}},
				},
				msg: "VolumeOptions must not be specified",
			},
		}...)
	}

	if DaemonIsWindows() {
		tests = append(tests, []testCase{
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{
						{
							Type:   "volume",
							Source: "not-supported-on-windows",
							Target: destPath,
							VolumeOptions: &mount.VolumeOptions{
								DriverConfig: &mount.Driver{
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
		tests = append(tests, []testCase{
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{
						{
							Type:   "volume",
							Source: "missing-device-opt",
							Target: destPath,
							VolumeOptions: &mount.VolumeOptions{
								DriverConfig: &mount.Driver{
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
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{
						{
							Type:   "volume",
							Source: "missing-device-opt",
							Target: destPath,
							VolumeOptions: &mount.VolumeOptions{
								DriverConfig: &mount.Driver{
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
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{
						{
							Type:   "volume",
							Source: "missing-type-opt",
							Target: destPath,
							VolumeOptions: &mount.VolumeOptions{
								DriverConfig: &mount.Driver{
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
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{
						{
							Type:   "volume",
							Source: "hello4",
							Target: destPath,
							VolumeOptions: &mount.VolumeOptions{
								DriverConfig: &mount.Driver{
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
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{{
						Type:   "tmpfs",
						Target: destPath,
					}},
				},
				msg: "",
			},
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{{
						Type:   "tmpfs",
						Target: destPath,
						TmpfsOptions: &mount.TmpfsOptions{
							SizeBytes: 4096 * 1024,
							Mode:      0o700,
						},
					}},
				},
				msg: "",
			},
			{
				config: container.Config{
					Image: "busybox",
				},
				hostConfig: container.HostConfig{
					Mounts: []mount.Mount{{
						Type:   "tmpfs",
						Source: "/shouldnotbespecified",
						Target: destPath,
					}},
				},
				msg: "Source must not be specified",
			},
		}...)
	}
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	// TODO add checks for statuscode returned by API
	for i, tc := range tests {
		c.Run(fmt.Sprintf("case %d", i), func(c *testing.T) {
			_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
				Config:           &tc.config,
				HostConfig:       &tc.hostConfig,
				NetworkingConfig: &network.NetworkingConfig{},
			})
			if tc.msg != "" {
				assert.ErrorContains(c, err, tc.msg, "%v", tests[i].config)
			} else {
				assert.NilError(c, err)
			}
		})
	}
}

func (s *DockerAPISuite) TestContainerAPICreateMountsBindRead(c *testing.T) {
	testRequires(c, NotUserNamespace, testEnv.IsLocalDaemon)
	// also with data in the host side
	tmpDir, err := os.MkdirTemp("", "test-mounts-api-bind")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)
	err = os.WriteFile(filepath.Join(tmpDir, "bar"), []byte("hello"), 0o666)
	assert.NilError(c, err)
	config := container.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", "cat /foo/bar"},
	}
	hostConfig := container.HostConfig{
		Mounts: []mount.Mount{
			{Type: "bind", Source: tmpDir, Target: dPath("/foo")},
		},
	}
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             "test",
	})
	assert.NilError(c, err)

	out := cli.DockerCmd(c, "start", "-a", "test").Combined()
	assert.Equal(c, out, "hello")
}

func (s *DockerAPISuite) TestContainersAPICreateMountsTmpfs(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	type testCase struct {
		cfg             mount.Mount
		expectedOptions []string
	}
	target := "/foo"
	cases := []testCase{
		{
			cfg: mount.Mount{
				Type:   "tmpfs",
				Target: target,
			},
			expectedOptions: []string{"rw", "nosuid", "nodev", "noexec", "relatime"},
		},
		{
			cfg: mount.Mount{
				Type:   "tmpfs",
				Target: target,
				TmpfsOptions: &mount.TmpfsOptions{
					SizeBytes: 4096 * 1024, Mode: 0o700,
				},
			},
			expectedOptions: []string{"rw", "nosuid", "nodev", "noexec", "relatime", "size=4096k", "mode=700"},
		},
	}

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	config := container.Config{
		Image: "busybox",
		Cmd:   []string{"/bin/sh", "-c", fmt.Sprintf("mount | grep 'tmpfs on %s'", target)},
	}
	for i, x := range cases {
		cName := fmt.Sprintf("test-tmpfs-%d", i)
		hostConfig := container.HostConfig{
			Mounts: []mount.Mount{x.cfg},
		}

		_, err = apiClient.ContainerCreate(testutil.GetContext(c), client.ContainerCreateOptions{
			Config:           &config,
			HostConfig:       &hostConfig,
			NetworkingConfig: &network.NetworkingConfig{},
			Name:             cName,
		})
		assert.NilError(c, err)
		out := cli.DockerCmd(c, "start", "-a", cName).Combined()
		for _, option := range x.expectedOptions {
			assert.Assert(c, is.Contains(out, option))
		}
	}
}

// Regression test for #33334
// Makes sure that when a container which has a custom stop signal + restart=always
// gets killed (with SIGKILL) by the kill API, that the restart policy is cancelled.
func (s *DockerAPISuite) TestContainerKillCustomStopSignal(c *testing.T) {
	id := strings.TrimSpace(runSleepingContainer(c, "--stop-signal=SIGTERM", "--restart=always"))
	res, _, err := request.Post(testutil.GetContext(c), "/containers/"+id+"/kill")
	assert.NilError(c, err)
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusNoContent, string(b))
	err = waitInspect(id, "{{.State.Running}} {{.State.Restarting}}", "false false", 30*time.Second)
	assert.NilError(c, err)
}

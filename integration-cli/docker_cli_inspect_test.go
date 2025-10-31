package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
)

type DockerCLIInspectSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIInspectSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIInspectSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (s *DockerCLIInspectSuite) TestInspectImage(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	imageTest := loadSpecialImage(c, specialimage.EmptyFS)
	// It is important that this ID remain stable. If a code change causes
	// it to be different, this is equivalent to a cache bust when pulling
	// a legacy-format manifest. If the check at the end of this function
	// fails, fix the difference in the image serialization instead of
	// updating this hash.
	imageTestID := "sha256:11f64303f0f7ffdc71f001788132bca5346831939a956e3e975c93267d89a16d"
	if containerdSnapshotterEnabled() {
		// Under containerd ID of the image is the digest of the manifest list.
		imageTestID = "sha256:e43ca824363c5c56016f6ede3a9035afe0e9bd43333215e0b0bde6193969725d"
	}

	id := inspectField(c, imageTest, "Id")

	assert.Equal(c, id, imageTestID)
}

func (s *DockerCLIInspectSuite) TestInspectInt64(c *testing.T) {
	cli.DockerCmd(c, "run", "-d", "-m=300M", "--name", "inspectTest", "busybox", "true")
	inspectOut := inspectField(c, "inspectTest", "HostConfig.Memory")
	assert.Equal(c, inspectOut, "314572800")
}

func (s *DockerCLIInspectSuite) TestInspectDefault(c *testing.T) {
	// Both the container and image are named busybox. docker inspect will fetch the container JSON.
	// If the container JSON is not available, it will go for the image JSON.

	out := cli.DockerCmd(c, "run", "--name=busybox", "-d", "busybox", "true").Stdout()
	containerID := strings.TrimSpace(out)

	inspectOut := inspectField(c, "busybox", "Id")
	assert.Equal(c, strings.TrimSpace(inspectOut), containerID)
}

func (s *DockerCLIInspectSuite) TestInspectStatus(c *testing.T) {
	id := runSleepingContainer(c, "-d")

	inspectOut := inspectField(c, id, "State.Status")
	assert.Equal(c, inspectOut, "running")

	// Windows does not support pause/unpause on Windows Server Containers.
	// (RS1 does for Hyper-V Containers, but production CI is not setup for that)
	if testEnv.DaemonInfo.OSType != "windows" {
		cli.DockerCmd(c, "pause", id)
		inspectOut = inspectField(c, id, "State.Status")
		assert.Equal(c, inspectOut, "paused")

		cli.DockerCmd(c, "unpause", id)
		inspectOut = inspectField(c, id, "State.Status")
		assert.Equal(c, inspectOut, "running")
	}

	cli.DockerCmd(c, "stop", id)
	inspectOut = inspectField(c, id, "State.Status")
	assert.Equal(c, inspectOut, "exited")
}

func (s *DockerCLIInspectSuite) TestInspectTypeFlagContainer(c *testing.T) {
	// Both the container and image are named busybox. docker inspect will fetch container
	// JSON State.Running field. If the field is true, it's a container.
	runSleepingContainer(c, "--name=busybox", "-d")

	formatStr := "--format={{.State.Running}}"
	out := cli.DockerCmd(c, "inspect", "--type=container", formatStr, "busybox").Stdout()
	assert.Equal(c, out, "true\n") // not a container JSON
}

func (s *DockerCLIInspectSuite) TestInspectTypeFlagWithNoContainer(c *testing.T) {
	// Run this test on an image named busybox. docker inspect will try to fetch container
	// JSON. Since there is no container named busybox and --type=container, docker inspect will
	// not try to get the image JSON. It will throw an error.

	cli.DockerCmd(c, "run", "-d", "busybox", "true")

	_, _, err := dockerCmdWithError("inspect", "--type=container", "busybox")
	// docker inspect should fail, as there is no container named busybox
	assert.ErrorContains(c, err, "")
}

func (s *DockerCLIInspectSuite) TestInspectTypeFlagWithImage(c *testing.T) {
	// Both the container and image are named busybox. docker inspect will fetch image
	// JSON as --type=image. if there is no image with name busybox, docker inspect
	// will throw an error.

	cli.DockerCmd(c, "run", "--name=busybox", "-d", "busybox", "true")

	out := cli.DockerCmd(c, "inspect", "--type=image", "busybox").Stdout()
	// not an image JSON
	assert.Assert(c, !strings.Contains(out, "State"))
}

func (s *DockerCLIInspectSuite) TestInspectTypeFlagWithInvalidValue(c *testing.T) {
	result := cli.Docker(cli.Args("inspect", "--type=foobar", "busybox"))
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	oneOf := []string{
		`unknown type: "foobar"`,
		"not a valid value for --type",
	}
	out := result.Combined()
	if !strings.Contains(out, oneOf[0]) && !strings.Contains(out, oneOf[1]) {
		c.Errorf("exoected one of %v: %s", oneOf, out)
	}
}

func (s *DockerCLIInspectSuite) TestInspectImageFilterInt(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	imageTest := loadSpecialImage(c, specialimage.EmptyFS)
	out := cli.DockerCmd(c, "inspect", "--format", "{{.Size}}", imageTest).Stdout()
	size, err := strconv.Atoi(strings.TrimSpace(out))
	assert.NilError(c, err)

	// now see if the size turns out to be the same
	formatStr := fmt.Sprintf("--format={{eq .Size %d}}", size)
	out = cli.DockerCmd(c, "inspect", formatStr, imageTest).Stdout()
	result, err := strconv.ParseBool(strings.TrimSuffix(out, "\n"))
	assert.NilError(c, err)
	assert.Equal(c, result, true)
}

func (s *DockerCLIInspectSuite) TestInspectContainerFilterInt(c *testing.T) {
	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-i", "-a", "stdin", "busybox", "cat"},
		Stdin:   strings.NewReader("blahblah"),
	})
	result.Assert(c, icmd.Success)
	out := result.Stdout()
	id := strings.TrimSpace(out)

	out = inspectField(c, id, "State.ExitCode")

	exitCode, err := strconv.Atoi(out)
	assert.Assert(c, err == nil, "failed to inspect exitcode of the container: %s, %v", out, err)

	// now get the exit code to verify
	formatStr := fmt.Sprintf("--format={{eq .State.ExitCode %d}}", exitCode)
	out = cli.DockerCmd(c, "inspect", formatStr, id).Stdout()
	inspectResult, err := strconv.ParseBool(strings.TrimSuffix(out, "\n"))
	assert.NilError(c, err)
	assert.Equal(c, inspectResult, true)
}

func (s *DockerCLIInspectSuite) TestInspectBindMountPoint(c *testing.T) {
	mode := ""
	if testEnv.DaemonInfo.OSType == "windows" {
		// Linux creates the host directory if it doesn't exist. Windows does not.
		os.Mkdir(`c:\data`, os.ModeDir)
	} else {
		mode = "z" // Relabel
	}

	var modifier string
	if mode != "" {
		modifier += ":" + mode
	}

	cli.DockerCmd(c, "run", "-d", "--name", "test", "-v", dPath("/data")+":"+dPath("/data")+modifier, "busybox", "cat")

	vol := inspectFieldJSON(c, "test", "Mounts")

	var mp []container.MountPoint
	err := json.Unmarshal([]byte(vol), &mp)
	assert.NilError(c, err)

	// check that there is only one mountpoint
	assert.Equal(c, len(mp), 1)

	m := mp[0]

	assert.Equal(c, m.Name, "")
	assert.Equal(c, m.Driver, "")
	assert.Equal(c, m.Source, dPath("/data"))
	assert.Equal(c, m.Destination, dPath("/data"))
	assert.Equal(c, m.Mode, mode)
	assert.Equal(c, m.RW, true)
}

func (s *DockerCLIInspectSuite) TestInspectNamedMountPoint(c *testing.T) {
	cli.DockerCmd(c, "run", "-d", "--name", "test", "-v", "data:"+dPath("/data"), "busybox", "cat")

	vol := inspectFieldJSON(c, "test", "Mounts")

	var mp []container.MountPoint
	err := json.Unmarshal([]byte(vol), &mp)
	assert.NilError(c, err)

	// check that there is only one mountpoint
	assert.Equal(c, len(mp), 1)

	m := mp[0]

	assert.Equal(c, m.Name, "data")
	assert.Equal(c, m.Driver, "local")
	assert.Assert(c, m.Source != "")
	assert.Equal(c, m.Destination, dPath("/data"))
	assert.Equal(c, m.RW, true)
}

// #14947
func (s *DockerCLIInspectSuite) TestInspectTimesAsRFC3339Nano(c *testing.T) {
	out := cli.DockerCmd(c, "run", "-d", "busybox", "true").Stdout()
	id := strings.TrimSpace(out)
	startedAt := inspectField(c, id, "State.StartedAt")
	finishedAt := inspectField(c, id, "State.FinishedAt")
	created := inspectField(c, id, "Created")

	_, err := time.Parse(time.RFC3339Nano, startedAt)
	assert.NilError(c, err)
	_, err = time.Parse(time.RFC3339Nano, finishedAt)
	assert.NilError(c, err)
	_, err = time.Parse(time.RFC3339Nano, created)
	assert.NilError(c, err)

	created = inspectField(c, "busybox", "Created")

	_, err = time.Parse(time.RFC3339Nano, created)
	assert.NilError(c, err)
}

// #15633
func (s *DockerCLIInspectSuite) TestInspectLogConfigNoType(c *testing.T) {
	cli.DockerCmd(c, "create", "--name=test", "--log-opt", "max-file=42", "busybox")
	var logConfig container.LogConfig

	out := inspectFieldJSON(c, "test", "HostConfig.LogConfig")

	err := json.NewDecoder(strings.NewReader(out)).Decode(&logConfig)
	assert.Assert(c, err == nil, "%v", out)

	assert.Equal(c, logConfig.Type, "json-file")
	assert.Equal(c, logConfig.Config["max-file"], "42", fmt.Sprint(logConfig))
}

func (s *DockerCLIInspectSuite) TestInspectNoSizeFlagContainer(c *testing.T) {
	// Both the container and image are named busybox. docker inspect will fetch container
	// JSON SizeRw and SizeRootFs field. If there is no flag --size/-s, there are no size fields.

	runSleepingContainer(c, "--name=busybox", "-d")

	formatStr := "--format={{.SizeRw}},{{.SizeRootFs}}"
	out := cli.DockerCmd(c, "inspect", "--type=container", formatStr, "busybox").Stdout()
	assert.Equal(c, strings.TrimSpace(out), "<nil>,<nil>", "Expected not to display size info: "+out)
}

func (s *DockerCLIInspectSuite) TestInspectSizeFlagContainer(c *testing.T) {
	runSleepingContainer(c, "--name=busybox", "-d")

	formatStr := "--format='{{.SizeRw}},{{.SizeRootFs}}'"
	out := cli.DockerCmd(c, "inspect", "-s", "--type=container", formatStr, "busybox").Stdout()
	sz := strings.Split(out, ",")

	assert.Assert(c, strings.TrimSpace(sz[0]) != "<nil>")
	assert.Assert(c, strings.TrimSpace(sz[1]) != "<nil>")
}

func (s *DockerCLIInspectSuite) TestInspectTemplateError(c *testing.T) {
	// Template parsing error for both the container and image.

	runSleepingContainer(c, "--name=container1", "-d")

	out, _, err := dockerCmdWithError("inspect", "--type=container", "--format='Format container: {{.ThisDoesNotExist}}'", "container1")
	assert.Assert(c, err != nil)
	assert.Assert(c, is.Contains(out, "template parsing error: template"))
	out, _, err = dockerCmdWithError("inspect", "--type=image", "--format='Format container: {{.ThisDoesNotExist}}'", "busybox")
	assert.Assert(c, err != nil)
	assert.Assert(c, is.Contains(out, "template parsing error"))
}

func (s *DockerCLIInspectSuite) TestInspectJSONFields(c *testing.T) {
	ctrName := "inspectjsonfields"
	runSleepingContainer(c, "--name", ctrName, "-d")
	out := cli.DockerCmd(c, "container", "inspect", "--format", "{{json .HostConfig.Dns}}", ctrName).Stdout()
	out = strings.TrimSpace(out)

	// Current versions use omit-empty, old versions returned an empty slice ({})
	oneOf := []string{"[]", "null"}
	var found bool
	for _, v := range oneOf {
		if out == v {
			found = true
			break
		}
	}
	assert.Assert(c, found)
}

func (s *DockerCLIInspectSuite) TestInspectByPrefix(c *testing.T) {
	id := inspectField(c, "busybox", "Id")
	assert.Assert(c, strings.HasPrefix(id, "sha256:"))

	id2 := inspectField(c, id[:12], "Id")
	assert.Equal(c, id, id2)

	id3 := inspectField(c, strings.TrimPrefix(id, "sha256:")[:12], "Id")
	assert.Equal(c, id, id3)
}

func (s *DockerCLIInspectSuite) TestInspectStopWhenNotFound(c *testing.T) {
	runSleepingContainer(c, "--name=busybox1", "-d")
	runSleepingContainer(c, "--name=busybox2", "-d")
	result := cli.Docker(cli.Args("inspect", "--type=container", "--format='{{.Name}}'", "busybox1", "busybox2", "missing"))

	assert.Assert(c, result.Error != nil)
	assert.Assert(c, is.Contains(result.Stdout(), "busybox1"))
	assert.Assert(c, is.Contains(result.Stdout(), "busybox2"))
	assert.Assert(c, is.Contains(result.Stderr(), "No such container: missing"))
	// test inspect would not fast fail
	result = cli.Docker(cli.Args("inspect", "--type=container", "--format='{{.Name}}'", "missing", "busybox1", "busybox2"))

	assert.Assert(c, result.Error != nil)
	assert.Assert(c, is.Contains(result.Stdout(), "busybox1"))
	assert.Assert(c, is.Contains(result.Stdout(), "busybox2"))
	assert.Assert(c, is.Contains(result.Stderr(), "No such container: missing"))
}

func (s *DockerCLIInspectSuite) TestInspectHistory(c *testing.T) {
	cli.DockerCmd(c, "run", "--name=testcont", "busybox", "echo", "hello")
	cli.DockerCmd(c, "commit", "-m", "test comment", "testcont", "testimg")
	out, _, err := dockerCmdWithError("inspect", "--format='{{.Comment}}'", "testimg")
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "test comment"))
}

func (s *DockerCLIInspectSuite) TestInspectContainerNetworkDefault(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	contName := "test1"
	cli.DockerCmd(c, "run", "--name", contName, "-d", "busybox", "top")
	netOut := cli.DockerCmd(c, "network", "inspect", "--format={{.ID}}", "bridge").Stdout()
	out := inspectField(c, contName, "NetworkSettings.Networks")
	assert.Assert(c, is.Contains(out, "bridge"))
	out = inspectField(c, contName, "NetworkSettings.Networks.bridge.NetworkID")
	assert.Equal(c, strings.TrimSpace(out), strings.TrimSpace(netOut))
}

func (s *DockerCLIInspectSuite) TestInspectContainerNetworkCustom(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	netOut := cli.DockerCmd(c, "network", "create", "net1").Stdout()
	cli.DockerCmd(c, "run", "--name=container1", "--net=net1", "-d", "busybox", "top")
	out := inspectField(c, "container1", "NetworkSettings.Networks")
	assert.Assert(c, is.Contains(out, "net1"))
	out = inspectField(c, "container1", "NetworkSettings.Networks.net1.NetworkID")
	assert.Equal(c, strings.TrimSpace(out), strings.TrimSpace(netOut))
}

func (s *DockerCLIInspectSuite) TestInspectRootFS(c *testing.T) {
	out, _, err := dockerCmdWithError("inspect", "busybox")
	assert.NilError(c, err)

	var imageJSON []image.InspectResponse
	err = json.Unmarshal([]byte(out), &imageJSON)
	assert.NilError(c, err)
	assert.Assert(c, len(imageJSON[0].RootFS.Layers) >= 1)
}

func (s *DockerCLIInspectSuite) TestInspectAmpersand(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	name := "test"
	out := cli.DockerCmd(c, "run", "--name", name, "--env", `TEST_ENV="soanni&rtr"`, "busybox", "env").Stdout()
	assert.Assert(c, is.Contains(out, `soanni&rtr`))
	out = cli.DockerCmd(c, "inspect", name).Stdout()
	assert.Assert(c, is.Contains(out, `soanni&rtr`))
}

func (s *DockerCLIInspectSuite) TestInspectPlugin(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, Network)
	_, _, err := dockerCmdWithError("plugin", "install", "--grant-all-permissions", pNameWithTag)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("inspect", "--type", "plugin", "--format", "{{.Name}}", pNameWithTag)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), pNameWithTag)

	out, _, err = dockerCmdWithError("inspect", "--format", "{{.Name}}", pNameWithTag)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), pNameWithTag)

	// Even without tag the inspect still work
	out, _, err = dockerCmdWithError("inspect", "--type", "plugin", "--format", "{{.Name}}", pNameWithTag)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), pNameWithTag)

	out, _, err = dockerCmdWithError("inspect", "--format", "{{.Name}}", pNameWithTag)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), pNameWithTag)

	_, _, err = dockerCmdWithError("plugin", "disable", pNameWithTag)
	assert.NilError(c, err)

	out, _, err = dockerCmdWithError("plugin", "remove", pNameWithTag)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, pNameWithTag))
}

// Test case for 29185
func (s *DockerCLIInspectSuite) TestInspectUnknownObject(c *testing.T) {
	// This test should work on both Windows and Linux
	res := cli.Docker(cli.Args("inspect", "foobar"))
	res.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	expected := "no such object: foobar"
	assert.Assert(c, is.Contains(strings.ToLower(res.Stderr()), expected))
}

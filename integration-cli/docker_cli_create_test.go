package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/testutil/fakecontext"
	"github.com/docker/go-connections/nat"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

// Make sure we can create a simple container with some args
func (s *DockerSuite) TestCreateArgs(c *testing.T) {
	// Intentionally clear entrypoint, as the Windows busybox image needs an entrypoint, which breaks this test
	out, _ := dockerCmd(c, "create", "--entrypoint=", "busybox", "command", "arg1", "arg2", "arg with space", "-c", "flags")

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "inspect", cleanedContainerID)

	var containers []struct {
		ID      string
		Created time.Time
		Path    string
		Args    []string
		Image   string
	}

	err := json.Unmarshal([]byte(out), &containers)
	assert.Assert(c, err == nil, "Error inspecting the container: %s", err)
	assert.Equal(c, len(containers), 1)

	cont := containers[0]
	assert.Equal(c, cont.Path, "command", fmt.Sprintf("Unexpected container path. Expected command, received: %s", cont.Path))

	b := false
	expected := []string{"arg1", "arg2", "arg with space", "-c", "flags"}
	for i, arg := range expected {
		if arg != cont.Args[i] {
			b = true
			break
		}
	}
	if len(cont.Args) != len(expected) || b {
		c.Fatalf("Unexpected args. Expected %v, received: %v", expected, cont.Args)
	}

}

// Make sure we can grow the container's rootfs at creation time.
func (s *DockerSuite) TestCreateGrowRootfs(c *testing.T) {
	// Windows and Devicemapper support growing the rootfs
	if testEnv.OSType != "windows" {
		testRequires(c, Devicemapper)
	}
	out, _ := dockerCmd(c, "create", "--storage-opt", "size=120G", "busybox")

	cleanedContainerID := strings.TrimSpace(out)

	inspectOut := inspectField(c, cleanedContainerID, "HostConfig.StorageOpt")
	assert.Equal(c, inspectOut, "map[size:120G]")
}

// Make sure we cannot shrink the container's rootfs at creation time.
func (s *DockerSuite) TestCreateShrinkRootfs(c *testing.T) {
	testRequires(c, Devicemapper)

	// Ensure this fails because of the defaultBaseFsSize is 10G
	out, _, err := dockerCmdWithError("create", "--storage-opt", "size=5G", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Container size cannot be smaller than"))
}

// Make sure we can set hostconfig options too
func (s *DockerSuite) TestCreateHostConfig(c *testing.T) {
	out, _ := dockerCmd(c, "create", "-P", "busybox", "echo")

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "inspect", cleanedContainerID)

	var containers []struct {
		HostConfig *struct {
			PublishAllPorts bool
		}
	}

	err := json.Unmarshal([]byte(out), &containers)
	assert.Assert(c, err == nil, "Error inspecting the container: %s", err)
	assert.Equal(c, len(containers), 1)

	cont := containers[0]
	assert.Assert(c, cont.HostConfig != nil, "Expected HostConfig, got none")
	assert.Assert(c, cont.HostConfig.PublishAllPorts, "Expected PublishAllPorts, got false")
}

func (s *DockerSuite) TestCreateWithPortRange(c *testing.T) {
	out, _ := dockerCmd(c, "create", "-p", "3300-3303:3300-3303/tcp", "busybox", "echo")

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "inspect", cleanedContainerID)

	var containers []struct {
		HostConfig *struct {
			PortBindings map[nat.Port][]nat.PortBinding
		}
	}
	err := json.Unmarshal([]byte(out), &containers)
	assert.Assert(c, err == nil, "Error inspecting the container: %s", err)
	assert.Equal(c, len(containers), 1)

	cont := containers[0]

	assert.Assert(c, cont.HostConfig != nil, "Expected HostConfig, got none")
	assert.Equal(c, len(cont.HostConfig.PortBindings), 4, fmt.Sprintf("Expected 4 ports bindings, got %d", len(cont.HostConfig.PortBindings)))

	for k, v := range cont.HostConfig.PortBindings {
		assert.Equal(c, len(v), 1, fmt.Sprintf("Expected 1 ports binding, for the port  %s but found %s", k, v))
		assert.Equal(c, k.Port(), v[0].HostPort, fmt.Sprintf("Expected host port %s to match published port %s", k.Port(), v[0].HostPort))

	}

}

func (s *DockerSuite) TestCreateWithLargePortRange(c *testing.T) {
	out, _ := dockerCmd(c, "create", "-p", "1-65535:1-65535/tcp", "busybox", "echo")

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "inspect", cleanedContainerID)

	var containers []struct {
		HostConfig *struct {
			PortBindings map[nat.Port][]nat.PortBinding
		}
	}

	err := json.Unmarshal([]byte(out), &containers)
	assert.Assert(c, err == nil, "Error inspecting the container: %s", err)
	assert.Equal(c, len(containers), 1)

	cont := containers[0]
	assert.Assert(c, cont.HostConfig != nil, "Expected HostConfig, got none")
	assert.Equal(c, len(cont.HostConfig.PortBindings), 65535)

	for k, v := range cont.HostConfig.PortBindings {
		assert.Equal(c, len(v), 1)
		assert.Equal(c, k.Port(), v[0].HostPort, fmt.Sprintf("Expected host port %s to match published port %s", k.Port(), v[0].HostPort))
	}

}

// "test123" should be printed by docker create + start
func (s *DockerSuite) TestCreateEchoStdout(c *testing.T) {
	out, _ := dockerCmd(c, "create", "busybox", "echo", "test123")

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "start", "-ai", cleanedContainerID)
	assert.Equal(c, out, "test123\n", "container should've printed 'test123', got %q", out)
}

func (s *DockerSuite) TestCreateVolumesCreated(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	prefix, slash := getPrefixAndSlashFromDaemonPlatform()

	name := "test_create_volume"
	dockerCmd(c, "create", "--name", name, "-v", prefix+slash+"foo", "busybox")

	dir, err := inspectMountSourceField(name, prefix+slash+"foo")
	assert.Assert(c, err == nil, "Error getting volume host path: %q", err)

	if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
		c.Fatalf("Volume was not created")
	}
	if err != nil {
		c.Fatalf("Error statting volume host path: %q", err)
	}

}

func (s *DockerSuite) TestCreateLabels(c *testing.T) {
	name := "test_create_labels"
	expected := map[string]string{"k1": "v1", "k2": "v2"}
	dockerCmd(c, "create", "--name", name, "-l", "k1=v1", "--label", "k2=v2", "busybox")

	actual := make(map[string]string)
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &actual)

	if !reflect.DeepEqual(expected, actual) {
		c.Fatalf("Expected %s got %s", expected, actual)
	}
}

func (s *DockerSuite) TestCreateLabelFromImage(c *testing.T) {
	imageName := "testcreatebuildlabel"
	buildImageSuccessfully(c, imageName, build.WithDockerfile(`FROM busybox
		LABEL k1=v1 k2=v2`))

	name := "test_create_labels_from_image"
	expected := map[string]string{"k2": "x", "k3": "v3", "k1": "v1"}
	dockerCmd(c, "create", "--name", name, "-l", "k2=x", "--label", "k3=v3", imageName)

	actual := make(map[string]string)
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &actual)

	if !reflect.DeepEqual(expected, actual) {
		c.Fatalf("Expected %s got %s", expected, actual)
	}
}

func (s *DockerSuite) TestCreateHostnameWithNumber(c *testing.T) {
	image := "busybox"
	// Busybox on Windows does not implement hostname command
	if testEnv.OSType == "windows" {
		image = testEnv.PlatformDefaults.BaseImage
	}
	out, _ := dockerCmd(c, "run", "-h", "web.0", image, "hostname")
	assert.Equal(c, strings.TrimSpace(out), "web.0", "hostname not set, expected `web.0`, got: %s", out)
}

func (s *DockerSuite) TestCreateRM(c *testing.T) {
	// Test to make sure we can 'rm' a new container that is in
	// "Created" state, and has ever been run. Test "rm -f" too.

	// create a container
	out, _ := dockerCmd(c, "create", "busybox")
	cID := strings.TrimSpace(out)

	dockerCmd(c, "rm", cID)

	// Now do it again so we can "rm -f" this time
	out, _ = dockerCmd(c, "create", "busybox")

	cID = strings.TrimSpace(out)
	dockerCmd(c, "rm", "-f", cID)
}

func (s *DockerSuite) TestCreateModeIpcContainer(c *testing.T) {
	// Uses Linux specific functionality (--ipc)
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)

	out, _ := dockerCmd(c, "create", "busybox")
	id := strings.TrimSpace(out)

	dockerCmd(c, "create", fmt.Sprintf("--ipc=container:%s", id), "busybox")
}

func (s *DockerSuite) TestCreateByImageID(c *testing.T) {
	imageName := "testcreatebyimageid"
	buildImageSuccessfully(c, imageName, build.WithDockerfile(`FROM busybox
		MAINTAINER dockerio`))
	imageID := getIDByName(c, imageName)
	truncatedImageID := stringid.TruncateID(imageID)

	dockerCmd(c, "create", imageID)
	dockerCmd(c, "create", truncatedImageID)

	// Ensure this fails
	out, exit, _ := dockerCmdWithError("create", fmt.Sprintf("%s:%s", imageName, imageID))
	if exit == 0 {
		c.Fatalf("expected non-zero exit code; received %d", exit)
	}

	if expected := "invalid reference format"; !strings.Contains(out, expected) {
		c.Fatalf(`Expected %q in output; got: %s`, expected, out)
	}

	if i := strings.IndexRune(imageID, ':'); i >= 0 {
		imageID = imageID[i+1:]
	}
	out, exit, _ = dockerCmdWithError("create", fmt.Sprintf("%s:%s", "wrongimage", imageID))
	if exit == 0 {
		c.Fatalf("expected non-zero exit code; received %d", exit)
	}

	if expected := "Unable to find image"; !strings.Contains(out, expected) {
		c.Fatalf(`Expected %q in output; got: %s`, expected, out)
	}
}

func (s *DockerSuite) TestCreateStopSignal(c *testing.T) {
	name := "test_create_stop_signal"
	dockerCmd(c, "create", "--name", name, "--stop-signal", "9", "busybox")

	res := inspectFieldJSON(c, name, "Config.StopSignal")
	assert.Assert(c, strings.Contains(res, "9"))
}

func (s *DockerSuite) TestCreateWithWorkdir(c *testing.T) {
	name := "foo"

	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	dir := prefix + slash + "home" + slash + "foo" + slash + "bar"

	dockerCmd(c, "create", "--name", name, "-w", dir, "busybox")
	// Windows does not create the workdir until the container is started
	if testEnv.OSType == "windows" {
		dockerCmd(c, "start", name)
		if IsolationIsHyperv() {
			// Hyper-V isolated containers do not allow file-operations on a
			// running container. This test currently uses `docker cp` to verify
			// that the WORKDIR was automatically created, which cannot be done
			// while the container is running.
			dockerCmd(c, "stop", name)
		}
	}
	// TODO: rewrite this test to not use `docker cp` for verifying that the WORKDIR was created
	dockerCmd(c, "cp", fmt.Sprintf("%s:%s", name, dir), prefix+slash+"tmp")
}

func (s *DockerSuite) TestCreateWithInvalidLogOpts(c *testing.T) {
	name := "test-invalidate-log-opts"
	out, _, err := dockerCmdWithError("create", "--name", name, "--log-opt", "invalid=true", "busybox")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "unknown log opt"))
	assert.Assert(c, is.Contains(out, "unknown log opt"))

	out, _ = dockerCmd(c, "ps", "-a")
	assert.Assert(c, !strings.Contains(out, name))
}

// #20972
func (s *DockerSuite) TestCreate64ByteHexID(c *testing.T) {
	out := inspectField(c, "busybox", "Id")
	imageID := strings.TrimPrefix(strings.TrimSpace(out), "sha256:")

	dockerCmd(c, "create", imageID)
}

// Test case for #23498
func (s *DockerSuite) TestCreateUnsetEntrypoint(c *testing.T) {
	name := "test-entrypoint"
	dockerfile := `FROM busybox
ADD entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
CMD echo foobar`

	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"entrypoint.sh": `#!/bin/sh
echo "I am an entrypoint"
exec "$@"`,
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "create", "--entrypoint=", name, "echo", "foo").Combined()
	id := strings.TrimSpace(out)
	assert.Assert(c, id != "")
	out = cli.DockerCmd(c, "start", "-a", id).Combined()
	assert.Equal(c, strings.TrimSpace(out), "foo")
}

// #22471
func (s *DockerSuite) TestCreateStopTimeout(c *testing.T) {
	name1 := "test_create_stop_timeout_1"
	dockerCmd(c, "create", "--name", name1, "--stop-timeout", "15", "busybox")

	res := inspectFieldJSON(c, name1, "Config.StopTimeout")
	assert.Assert(c, strings.Contains(res, "15"))
	name2 := "test_create_stop_timeout_2"
	dockerCmd(c, "create", "--name", name2, "busybox")

	res = inspectFieldJSON(c, name2, "Config.StopTimeout")
	assert.Assert(c, strings.Contains(res, "null"))
}

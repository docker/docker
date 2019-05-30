package container // import "github.com/docker/docker/integration/container"

import (
	"bufio"
	"context"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/daemon"
	"github.com/docker/docker/internal/test/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/fs"
	"gotest.tools/skip"
)

// testIpcCheckDevExists checks whether a given mount (identified by its
// major:minor pair from /proc/self/mountinfo) exists on the host system.
//
// The format of /proc/self/mountinfo is like:
//
// 29 23 0:24 / /dev/shm rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
//       ^^^^\
//            - this is the minor:major we look for
func testIpcCheckDevExists(mm string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 7 {
			continue
		}
		if fields[2] == mm {
			return true, nil
		}
	}

	return false, s.Err()
}

// testIpcNonePrivateShareable is a helper function to test "none",
// "private" and "shareable" modes.
func testIpcNonePrivateShareable(t *testing.T, mode string, mustBeMounted bool, mustBeShared bool) {
	defer setupTest(t)()

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IpcMode(mode),
	}
	client := testEnv.APIClient()
	ctx := context.Background()

	resp, err := client.ContainerCreate(ctx, &cfg, &hostCfg, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))

	err = client.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// get major:minor pair for /dev/shm from container's /proc/self/mountinfo
	cmd := "awk '($5 == \"/dev/shm\") {printf $3}' /proc/self/mountinfo"
	result, err := container.Exec(ctx, client, resp.ID, []string{"sh", "-c", cmd})
	assert.NilError(t, err)
	mm := result.Combined()
	if !mustBeMounted {
		assert.Check(t, is.Equal(mm, ""))
		// no more checks to perform
		return
	}
	assert.Check(t, is.Equal(true, regexp.MustCompile("^[0-9]+:[0-9]+$").MatchString(mm)))

	shared, err := testIpcCheckDevExists(mm)
	assert.NilError(t, err)
	t.Logf("[testIpcPrivateShareable] ipcmode: %v, ipcdev: %v, shared: %v, mustBeShared: %v\n", mode, mm, shared, mustBeShared)
	assert.Check(t, is.Equal(shared, mustBeShared))
}

// TestIpcModeNone checks the container "none" IPC mode
// (--ipc none) works as expected. It makes sure there is no
// /dev/shm mount inside the container.
func TestIpcModeNone(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcNonePrivateShareable(t, "none", false, false)
}

// TestAPIIpcModePrivate checks the container private IPC mode
// (--ipc private) works as expected. It gets the minor:major pair
// of /dev/shm mount from the container, and makes sure there is no
// such pair on the host.
func TestIpcModePrivate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcNonePrivateShareable(t, "private", true, false)
}

// TestAPIIpcModeShareable checks the container shareable IPC mode
// (--ipc shareable) works as expected. It gets the minor:major pair
// of /dev/shm mount from the container, and makes sure such pair
// also exists on the host.
func TestIpcModeShareable(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcNonePrivateShareable(t, "shareable", true, true)
}

// testIpcContainer is a helper function to test --ipc container:NNN mode in various scenarios
func testIpcContainer(t *testing.T, donorMode string, mustWork bool) {
	t.Helper()

	defer setupTest(t)()

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IpcMode(donorMode),
	}
	ctx := context.Background()
	client := testEnv.APIClient()

	// create and start the "donor" container
	resp, err := client.ContainerCreate(ctx, &cfg, &hostCfg, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name1 := resp.ID

	err = client.ContainerStart(ctx, name1, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// create and start the second container
	hostCfg.IpcMode = containertypes.IpcMode("container:" + name1)
	resp, err = client.ContainerCreate(ctx, &cfg, &hostCfg, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name2 := resp.ID

	err = client.ContainerStart(ctx, name2, types.ContainerStartOptions{})
	if !mustWork {
		// start should fail with a specific error
		assert.Check(t, is.ErrorContains(err, "non-shareable IPC"))
		// no more checks to perform here
		return
	}

	// start should succeed
	assert.NilError(t, err)

	// check that IPC is shared
	// 1. create a file in the first container
	_, err = container.Exec(ctx, client, name1, []string{"sh", "-c", "printf covfefe > /dev/shm/bar"})
	assert.NilError(t, err)
	// 2. check it's the same file in the second one
	result, err := container.Exec(ctx, client, name2, []string{"cat", "/dev/shm/bar"})
	assert.NilError(t, err)
	out := result.Combined()
	assert.Check(t, is.Equal(true, regexp.MustCompile("^covfefe$").MatchString(out)))
}

// TestAPIIpcModeShareableAndPrivate checks that
// 1) a container created with --ipc container:ID can use IPC of another shareable container.
// 2) a container created with --ipc container:ID can NOT use IPC of another private container.
func TestAPIIpcModeShareableAndContainer(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcContainer(t, "shareable", true)

	testIpcContainer(t, "private", false)
}

/* TestAPIIpcModeHost checks that a container created with --ipc host
 * can use IPC of the host system.
 */
func TestAPIIpcModeHost(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsUserNamespace)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IpcMode("host"),
	}
	ctx := context.Background()

	client := testEnv.APIClient()
	resp, err := client.ContainerCreate(ctx, &cfg, &hostCfg, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name := resp.ID

	err = client.ContainerStart(ctx, name, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// check that IPC is shared
	// 1. create a file inside container
	_, err = container.Exec(ctx, client, name, []string{"sh", "-c", "printf covfefe > /dev/shm/." + name})
	assert.NilError(t, err)
	// 2. check it's the same on the host
	bytes, err := ioutil.ReadFile("/dev/shm/." + name)
	assert.NilError(t, err)
	assert.Check(t, is.Equal("covfefe", string(bytes)))
	// 3. clean up
	_, err = container.Exec(ctx, client, name, []string{"rm", "-f", "/dev/shm/." + name})
	assert.NilError(t, err)
}

// testDaemonIpcPrivateShareable is a helper function to test "private" and "shareable" daemon default ipc modes.
func testDaemonIpcPrivateShareable(t *testing.T, mustBeShared bool, arg ...string) {
	defer setupTest(t)()

	d := daemon.New(t)
	d.StartWithBusybox(t, arg...)
	defer d.Stop(t)

	c := d.NewClientT(t)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	ctx := context.Background()

	resp, err := c.ContainerCreate(ctx, &cfg, &containertypes.HostConfig{}, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))

	err = c.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// get major:minor pair for /dev/shm from container's /proc/self/mountinfo
	cmd := "awk '($5 == \"/dev/shm\") {printf $3}' /proc/self/mountinfo"
	result, err := container.Exec(ctx, c, resp.ID, []string{"sh", "-c", cmd})
	assert.NilError(t, err)
	mm := result.Combined()
	assert.Check(t, is.Equal(true, regexp.MustCompile("^[0-9]+:[0-9]+$").MatchString(mm)))

	shared, err := testIpcCheckDevExists(mm)
	assert.NilError(t, err)
	t.Logf("[testDaemonIpcPrivateShareable] ipcdev: %v, shared: %v, mustBeShared: %v\n", mm, shared, mustBeShared)
	assert.Check(t, is.Equal(shared, mustBeShared))
}

// TestDaemonIpcModeShareable checks that --default-ipc-mode shareable works as intended.
func TestDaemonIpcModeShareable(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcPrivateShareable(t, true, "--default-ipc-mode", "shareable")
}

// TestDaemonIpcModePrivate checks that --default-ipc-mode private works as intended.
func TestDaemonIpcModePrivate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcPrivateShareable(t, false, "--default-ipc-mode", "private")
}

// used to check if an IpcMode given in config works as intended
func testDaemonIpcFromConfig(t *testing.T, mode string, mustExist bool) {
	config := `{"default-ipc-mode": "` + mode + `"}`
	file := fs.NewFile(t, "test-daemon-ipc-config", fs.WithContent(config))
	defer file.Remove()

	testDaemonIpcPrivateShareable(t, mustExist, "--config-file", file.Path())
}

// TestDaemonIpcModePrivateFromConfig checks that "default-ipc-mode: private" config works as intended.
func TestDaemonIpcModePrivateFromConfig(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcFromConfig(t, "private", false)
}

// TestDaemonIpcModeShareableFromConfig checks that "default-ipc-mode: shareable" config works as intended.
func TestDaemonIpcModeShareableFromConfig(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcFromConfig(t, "shareable", true)
}

// TestIpcModeOlderClient checks that older client gets shareable IPC mode
// by default, even when the daemon default is private.
func TestIpcModeOlderClient(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "requires a daemon with DefaultIpcMode: private")
	c := testEnv.APIClient()
	skip.If(t, versions.LessThan(c.ClientVersion(), "1.40"), "requires client API >= 1.40")

	t.Parallel()

	ctx := context.Background()

	// pre-check: default ipc mode in daemon is private
	cID := container.Create(t, ctx, c, container.WithAutoRemove)

	inspect, err := c.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(inspect.HostConfig.IpcMode), "private"))

	// main check: using older client creates "shareable" container
	c = request.NewAPIClient(t, client.WithVersion("1.39"))
	cID = container.Create(t, ctx, c, container.WithAutoRemove)

	inspect, err = c.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(inspect.HostConfig.IpcMode), "shareable"))
}

package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/integration/internal/requirement"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// Bring up a daemon with the specified default cgroup namespace mode, and then create a container with the container options
func testRunWithCgroupNs(t *testing.T, daemonNsMode string, containerOpts ...func(*container.TestContainerConfig)) (string, string) {
	t.Helper()
	d := daemon.New(t, daemon.WithDefaultCgroupNamespaceMode(daemonNsMode))
	client := d.NewClientT(t)
	ctx := context.Background()

	d.StartWithBusybox(t)
	defer d.Stop(t)

	cID := container.Run(ctx, t, client, containerOpts...)
	poll.WaitOn(t, container.IsInState(ctx, client, cID, "running"), poll.WithDelay(100*time.Millisecond))

	daemonCgroup := d.CgroupNamespace(t)
	containerCgroup := container.GetContainerNS(ctx, t, client, cID, "cgroup")
	return containerCgroup, daemonCgroup
}

// Bring up a daemon with the specified default cgroup namespace mode. Create a container with the container options,
// expecting an error with the specified string
func testCreateFailureWithCgroupNs(t *testing.T, daemonNsMode string, errStr string, containerOpts ...func(*container.TestContainerConfig)) {
	d := daemon.New(t, daemon.WithDefaultCgroupNamespaceMode(daemonNsMode))
	client := d.NewClientT(t)
	ctx := context.Background()

	d.StartWithBusybox(t)
	defer d.Stop(t)
	container.CreateExpectingErr(ctx, t, client, errStr, containerOpts...)
}

func TestCgroupNamespacesRun(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to private cgroup namespaces, containers launched
	// should be in their own private cgroup namespace by default
	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "private")
	assert.Assert(t, daemonCgroup != containerCgroup)
}

func TestCgroupNamespacesRunPrivileged(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())
	skip.If(t, testEnv.DaemonInfo.CgroupVersion == "2", "on cgroup v2, privileged containers use private cgroupns")
	skip.If(t, testEnv.IsUserNamespace, "privileged is not supported with user namespaces")

	// When the daemon defaults to private cgroup namespaces, privileged containers
	// launched should not be inside their own cgroup namespaces
	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "private", container.WithPrivileged(true))
	assert.Assert(t, daemonCgroup == containerCgroup)
}

func TestCgroupNamespacesRunDaemonHostMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to host cgroup namespaces, containers
	// launched should not be inside their own cgroup namespaces
	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "host")
	assert.Assert(t, daemonCgroup == containerCgroup)
}

func TestCgroupNamespacesRunHostMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to private cgroup namespaces, containers launched
	// with a cgroup ns mode of "host" should not be inside their own cgroup namespaces
	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "private", container.WithCgroupnsMode("host"))
	assert.Assert(t, daemonCgroup == containerCgroup)
}

func TestCgroupNamespacesRunPrivateMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to private cgroup namespaces, containers launched
	// with a cgroup ns mode of "private" should be inside their own cgroup namespaces
	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "private", container.WithCgroupnsMode("private"))
	assert.Assert(t, daemonCgroup != containerCgroup)
}

func TestCgroupNamespacesRunPrivilegedAndPrivate(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())
	skip.If(t, testEnv.IsUserNamespace, "privileged is not supported with user namespaces")

	containerCgroup, daemonCgroup := testRunWithCgroupNs(t, "private", container.WithPrivileged(true), container.WithCgroupnsMode("private"))
	assert.Assert(t, daemonCgroup != containerCgroup)
}

func TestCgroupNamespacesRunInvalidMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// An invalid cgroup namespace mode should return an error on container creation
	errStr := "invalid cgroup namespace mode: invalid"
	testCreateFailureWithCgroupNs(t, "private", errStr, container.WithCgroupnsMode("invalid"))
}

// Clients before 1.40 expect containers to be created in the host cgroup namespace,
// regardless of the default setting of the daemon, unless running with cgroup v2
func TestCgroupNamespacesRunOlderClient(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	d := daemon.New(t, daemon.WithDefaultCgroupNamespaceMode("private"))
	client := d.NewClientT(t, client.WithVersion("1.39"))

	ctx := context.Background()
	d.StartWithBusybox(t)
	defer d.Stop(t)

	cID := container.Run(ctx, t, client)
	poll.WaitOn(t, container.IsInState(ctx, client, cID, "running"), poll.WithDelay(100*time.Millisecond))

	daemonCgroup := d.CgroupNamespace(t)
	containerCgroup := container.GetContainerNS(ctx, t, client, cID, "cgroup")
	if testEnv.DaemonInfo.CgroupVersion != "2" {
		assert.Assert(t, daemonCgroup == containerCgroup)
	} else {
		assert.Assert(t, daemonCgroup != containerCgroup)
	}
}

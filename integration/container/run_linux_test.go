package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"strings"
	"testing"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/integration/internal/container"
	net "github.com/docker/docker/integration/internal/network"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestNISDomainname(t *testing.T) {
	// Older versions of the daemon would concatenate hostname and domainname,
	// so hostname "foobar" and domainname "baz.cyphar.com" would produce
	// `foobar.baz.cyphar.com` as hostname.
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "skip test from new feature")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsUserNamespace, "setting domain is not supported with user namespaces: https://github.com/docker/for-linux/issues/743")

	// Rootless supports custom Hostname but doesn't support custom Domainname
	//  OCI runtime create failed: container_linux.go:349: starting container process caused "process_linux.go:449: container init caused \
	//  "write sysctl key kernel.domainname: open /proc/sys/kernel/domainname: permission denied\"": unknown.
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support setting Domainname (TODO: https://github.com/moby/moby/issues/40632)")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	const (
		hostname   = "foobar"
		domainname = "baz.cyphar.com"
	)

	cID := container.Run(ctx, t, client, func(c *container.TestContainerConfig) {
		c.Config.Hostname = hostname
		c.Config.Domainname = domainname
	})

	poll.WaitOn(t, container.IsInState(ctx, client, cID, "running"), poll.WithDelay(100*time.Millisecond))

	inspect, err := client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(hostname, inspect.Config.Hostname))
	assert.Check(t, is.Equal(domainname, inspect.Config.Domainname))

	// Check hostname.
	res, err := container.Exec(ctx, client, cID,
		[]string{"cat", "/proc/sys/kernel/hostname"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(hostname, strings.TrimSpace(res.Stdout())))

	// Check domainname.
	res, err = container.Exec(ctx, client, cID,
		[]string{"cat", "/proc/sys/kernel/domainname"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(domainname, strings.TrimSpace(res.Stdout())))
}

func TestHostnameDnsResolution(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	const (
		hostname = "foobar"
	)

	// using user defined network as we want to use internal DNS
	netName := "foobar-net"
	net.CreateNoError(context.Background(), t, client, netName, net.WithDriver("bridge"))

	cID := container.Run(ctx, t, client, func(c *container.TestContainerConfig) {
		c.Config.Hostname = hostname
		c.HostConfig.NetworkMode = containertypes.NetworkMode(netName)
	})

	poll.WaitOn(t, container.IsInState(ctx, client, cID, "running"), poll.WithDelay(100*time.Millisecond))

	inspect, err := client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(hostname, inspect.Config.Hostname))

	// Clear hosts file so ping will use DNS for hostname resolution
	res, err := container.Exec(ctx, client, cID,
		[]string{"sh", "-c", "echo 127.0.0.1 localhost | tee /etc/hosts && ping -c 1 foobar"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal("", res.Stderr()))
	assert.Equal(t, 0, res.ExitCode)
}

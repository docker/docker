package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestUpdateMemory(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, !testEnv.DaemonInfo.MemoryLimit)
	skip.If(t, !testEnv.DaemonInfo.SwapLimit)

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	cID := container.Run(t, ctx, client, func(c *container.TestContainerConfig) {
		c.HostConfig.Resources = containertypes.Resources{
			Memory: 200 * 1024 * 1024,
		}
	})

	poll.WaitOn(t, container.IsInState(ctx, client, cID, "running"), poll.WithDelay(100*time.Millisecond))

	const (
		setMemory     int64 = 314572800
		setMemorySwap int64 = 524288000
	)

	_, err := client.ContainerUpdate(ctx, cID, containertypes.UpdateConfig{
		Resources: containertypes.Resources{
			Memory:     setMemory,
			MemorySwap: setMemorySwap,
		},
	})
	assert.NilError(t, err)

	inspect, err := client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(setMemory, inspect.HostConfig.Memory))
	assert.Check(t, is.Equal(setMemorySwap, inspect.HostConfig.MemorySwap))

	res, err := container.Exec(ctx, client, cID,
		[]string{"cat", "/sys/fs/cgroup/memory/memory.limit_in_bytes"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(strconv.FormatInt(setMemory, 10), strings.TrimSpace(res.Stdout())))

	res, err = container.Exec(ctx, client, cID,
		[]string{"cat", "/sys/fs/cgroup/memory/memory.memsw.limit_in_bytes"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(strconv.FormatInt(setMemorySwap, 10), strings.TrimSpace(res.Stdout())))
}

func TestUpdateCPUQuota(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	cID := container.Run(t, ctx, client)

	for _, test := range []struct {
		desc   string
		update int64
	}{
		{desc: "some random value", update: 15000},
		{desc: "a higher value", update: 20000},
		{desc: "a lower value", update: 10000},
		{desc: "unset value", update: -1},
	} {
		_, err := client.ContainerUpdate(ctx, cID, containertypes.UpdateConfig{
			Resources: containertypes.Resources{
				CPUQuota: test.update,
			},
		})
		assert.NilError(t, err)

		inspect, err := client.ContainerInspect(ctx, cID)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(test.update, inspect.HostConfig.CPUQuota))

		res, err := container.Exec(ctx, client, cID,
			[]string{"/bin/cat", "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(res.Stderr(), 0))
		assert.Equal(t, 0, res.ExitCode)

		assert.Check(t, is.Equal(strconv.FormatInt(test.update, 10), strings.TrimSpace(res.Stdout())))
	}
}

func TestUpdatePidsLimit(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, !testEnv.DaemonInfo.PidsLimit)

	defer setupTest(t)()
	apiClient := testEnv.APIClient()
	oldAPIclient := request.NewAPIClient(t, client.WithVersion("1.24"))
	ctx := context.Background()

	intPtr := func(i int64) *int64 {
		return &i
	}

	for _, test := range []struct {
		desc     string
		oldAPI   bool
		initial  *int64
		update   *int64
		expect   int64
		expectCg string
	}{
		{desc: "update from none", update: intPtr(32), expect: 32, expectCg: "32"},
		{desc: "no change", initial: intPtr(32), expect: 32, expectCg: "32"},
		{desc: "update lower", initial: intPtr(32), update: intPtr(16), expect: 16, expectCg: "16"},
		{desc: "update on old api ignores value", oldAPI: true, initial: intPtr(32), update: intPtr(16), expect: 32, expectCg: "32"},
		{desc: "unset limit with zero", initial: intPtr(32), update: intPtr(0), expect: 0, expectCg: "max"},
		{desc: "unset limit with minus one", initial: intPtr(32), update: intPtr(-1), expect: 0, expectCg: "max"},
		{desc: "unset limit with minus two", initial: intPtr(32), update: intPtr(-2), expect: 0, expectCg: "max"},
	} {
		c := apiClient
		if test.oldAPI {
			c = oldAPIclient
		}

		t.Run(test.desc, func(t *testing.T) {
			// Using "network=host" to speed up creation (13.96s vs 6.54s)
			cID := container.Run(t, ctx, apiClient, container.WithPidsLimit(test.initial), container.WithNetworkMode("host"))

			_, err := c.ContainerUpdate(ctx, cID, containertypes.UpdateConfig{
				Resources: containertypes.Resources{
					PidsLimit: test.update,
				},
			})
			assert.NilError(t, err)

			inspect, err := c.ContainerInspect(ctx, cID)
			assert.NilError(t, err)
			assert.Assert(t, inspect.HostConfig.Resources.PidsLimit != nil)
			assert.Equal(t, *inspect.HostConfig.Resources.PidsLimit, test.expect)

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			res, err := container.Exec(ctx, c, cID, []string{"cat", "/sys/fs/cgroup/pids/pids.max"})
			assert.NilError(t, err)
			assert.Assert(t, is.Len(res.Stderr(), 0))

			out := strings.TrimSpace(res.Stdout())
			assert.Equal(t, out, test.expectCg)
		})
	}
}

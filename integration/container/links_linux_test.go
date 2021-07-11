package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/integration/internal/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestLinksEtcHostsContentMatch(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode has different view of /etc/hosts")
	skip.If(t, testEnv.IsUserNamespace, "host network mode is not supported with userns enabled")

	hosts, err := ioutil.ReadFile("/etc/hosts")
	skip.If(t, os.IsNotExist(err))

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	cID := container.Run(ctx, t, client, container.WithNetworkMode("host"))
	res, err := container.Exec(ctx, client, cID, []string{"cat", "/etc/hosts"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)

	assert.Check(t, is.Equal(string(hosts), res.Stdout()))
}

func TestLinksContainerNames(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	containerA := "first_" + t.Name()
	containerB := "second_" + t.Name()
	container.Run(ctx, t, client, container.WithName(containerA))
	container.Run(ctx, t, client, container.WithName(containerB), container.WithLinks(containerA+":"+containerA))

	f := filters.NewArgs(filters.Arg("name", containerA))

	containers, err := client.ContainerList(ctx, types.ContainerListOptions{
		Filters: f,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(1, len(containers)))
	assert.Check(t, is.DeepEqual([]string{"/" + containerA, "/" + containerB + "/" + containerA}, containers[0].Names))
}

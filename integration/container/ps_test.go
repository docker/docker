package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/integration/internal/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestPsFilter(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	prev := container.Create(ctx, t, client)
	top := container.Create(ctx, t, client)
	next := container.Create(ctx, t, client)

	containerIDs := func(containers []types.Container) []string {
		var entries []string
		for _, container := range containers {
			entries = append(entries, container.ID)
		}
		return entries
	}

	f1 := filters.NewArgs()
	f1.Add("since", top)
	q1, err := client.ContainerList(ctx, types.ContainerListOptions{
		All:     true,
		Filters: f1,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Contains(containerIDs(q1), next))

	f2 := filters.NewArgs()
	f2.Add("before", top)
	q2, err := client.ContainerList(ctx, types.ContainerListOptions{
		All:     true,
		Filters: f2,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Contains(containerIDs(q2), prev))
}

// TestPsPlatform verifies that containers have a platform set
func TestPsPlatform(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	container.Create(ctx, t, client)

	containers, err := client.ContainerList(ctx, types.ContainerListOptions{
		All: true,
	})
	assert.NilError(t, err)
	assert.Check(t, len(containers) > 0)

	ctr := containers[0]
	if assert.Check(t, ctr.Platform != nil) {
		// Check that at least OS and Architecture have a value. Other values
		// depend on the platform on which we're running the test.
		assert.Equal(t, ctr.Platform.OS, runtime.GOOS)
		assert.Check(t, ctr.Platform.Architecture != "")
	}
}

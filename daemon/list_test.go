package daemon

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/container"
	"github.com/docker/docker/image"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

var root string

func TestMain(m *testing.M) {
	var err error
	root, err = ioutil.TempDir("", "docker-container-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	os.Exit(m.Run())
}

// This sets up a container with a name so that name filters
// work against it. It takes in a pointer to Daemon so that
// minor operations are not repeated by the caller
func setupContainerWithName(t *testing.T, name string, daemon *Daemon) *container.Container {
	t.Helper()
	var (
		id              = uuid.New().String()
		computedImageID = digest.FromString(id)
		cRoot           = filepath.Join(root, id)
	)
	if err := os.MkdirAll(cRoot, 0755); err != nil {
		t.Fatal(err)
	}

	c := container.NewBaseContainer(id, cRoot)
	// these are for passing includeContainerInList
	if name[0] != '/' {
		name = "/" + name
	}
	c.Name = name
	c.Running = true
	c.HostConfig = &containertypes.HostConfig{}

	// these are for passing the refreshImage reducer
	c.ImageID = image.IDFromDigest(computedImageID)
	c.Config = &containertypes.Config{
		Image: computedImageID.String(),
	}

	// this is done here to avoid requiring these
	// operations n x number of containers in the
	// calling function
	daemon.containersReplica.Save(c)
	daemon.reserveName(id, name)

	return c
}

func containerListContainsName(containers []*types.Container, name string) bool {
	for _, container := range containers {
		for _, containerName := range container.Names {
			if containerName == name {
				return true
			}
		}
	}

	return false
}

func TestListInvalidFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.Assert(t, err == nil)
	d := &Daemon{
		containersReplica: db,
	}

	f := filters.NewArgs(filters.Arg("invalid", "foo"))

	_, err = d.Containers(context.Background(), &types.ContainerListOptions{
		Filters: f,
	})
	assert.Assert(t, is.Error(err, "Invalid filter 'invalid'"))
}

func TestNameFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.Assert(t, err == nil)
	d := &Daemon{
		containersReplica: db,
	}

	var (
		one   = setupContainerWithName(t, "a1", d)
		two   = setupContainerWithName(t, "a2", d)
		three = setupContainerWithName(t, "b1", d)
	)

	// moby/moby #37453 - ^ regex not working due to prefix slash
	// not being stripped
	containerList, err := d.Containers(context.Background(), &types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^a")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerList, 2))
	assert.Assert(t, containerListContainsName(containerList, one.Name))
	assert.Assert(t, containerListContainsName(containerList, two.Name))

	// Same as above but with slash prefix should produce the same result
	containerListWithPrefix, err := d.Containers(context.Background(), &types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^/a")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerListWithPrefix, 2))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, one.Name))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, two.Name))

	// Same as above but make sure it works for exact names
	containerList, err = d.Containers(context.Background(), &types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "b1")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerList, 1))
	assert.Assert(t, containerListContainsName(containerList, three.Name))

	// Same as above but with slash prefix should produce the same result
	containerListWithPrefix, err = d.Containers(context.Background(), &types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "/b1")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerListWithPrefix, 1))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, three.Name))
}

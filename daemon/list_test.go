package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

var root string

func TestMain(m *testing.M) {
	var err error
	root, err = os.MkdirTemp("", "docker-container-test-")
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
		computedImageID = image.ID(digest.FromString(id))
		cRoot           = filepath.Join(root, id)
	)
	if err := os.MkdirAll(cRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	c := container.NewBaseContainer(id, cRoot)
	// these are for passing includeContainerInList
	if name[0] != '/' {
		name = "/" + name
	}
	c.Name = name
	c.State.Running = true
	c.HostConfig = &containertypes.HostConfig{}
	c.Created = time.Now()

	// these are for passing the refreshImage reducer
	c.ImageID = computedImageID
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

func containerListContainsName(containers []containertypes.Summary, name string) bool {
	for _, ctr := range containers {
		if slices.Contains(ctr.Names, name) {
			return true
		}
	}

	return false
}

func TestContainerList(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	// test list with different number of containers
	for _, num := range []int{0, 1, 2, 4, 8, 16, 32, 64, 100} {
		t.Run(fmt.Sprintf("%d containers", num), func(t *testing.T) {
			db, err := container.NewViewDB() // new DB to ignore prior containers
			assert.NilError(t, err)
			d = &Daemon{
				containersReplica: db,
			}

			// create the containers
			containers := make([]*container.Container, num)
			for i := range num {
				name := fmt.Sprintf("cont-%d", i)
				containers[i] = setupContainerWithName(t, name, d)
				// ensure container timestamps are separated enough so the
				// sort used by d.Containers() can deterministically sort them.
				if i > 0 {
					containers[i].Created = containers[i-1].Created.Add(time.Millisecond)
				}
			}

			// list them and verify correctness
			containerList, err := d.Containers(t.Context(), &backend.ContainerListOptions{All: true})
			assert.NilError(t, err)
			assert.Assert(t, is.Len(containerList, num))

			for i := range num {
				// container list should be ordered in descending creation order
				assert.Assert(t, is.Equal(containerList[i].Names[0], containers[num-1-i].Name))
			}
		})
	}
}

func TestContainerList_InvalidFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	_, err = d.Containers(t.Context(), &backend.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("invalid", "foo")),
	})
	assert.Assert(t, is.Error(err, "invalid filter 'invalid'"))
}

func TestContainerList_NameFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
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
	containerList, err := d.Containers(t.Context(), &backend.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^a")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerList, 2))
	assert.Assert(t, containerListContainsName(containerList, one.Name))
	assert.Assert(t, containerListContainsName(containerList, two.Name))

	// Same as above but with slash prefix should produce the same result
	containerListWithPrefix, err := d.Containers(t.Context(), &backend.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^/a")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerListWithPrefix, 2))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, one.Name))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, two.Name))

	// Same as above but make sure it works for exact names
	containerList, err = d.Containers(t.Context(), &backend.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "b1")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerList, 1))
	assert.Assert(t, containerListContainsName(containerList, three.Name))

	// Same as above but with slash prefix should produce the same result
	containerListWithPrefix, err = d.Containers(t.Context(), &backend.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "/b1")),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containerListWithPrefix, 1))
	assert.Assert(t, containerListContainsName(containerListWithPrefix, three.Name))
}

func TestContainerList_AnnotationFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	one := setupContainerWithName(t, "a1", d)
	one.HostConfig.Annotations = map[string]string{"annotation1": "value1"}
	assert.NilError(t, db.Save(one))

	two := setupContainerWithName(t, "a2", d)
	two.HostConfig.Annotations = map[string]string{"annotation1": "value2", "annotation2": "value1"}
	assert.NilError(t, db.Save(two))

	// no annotations
	three := setupContainerWithName(t, "b1", d)

	tests := []struct {
		doc      string
		filters  []filters.KeyValuePair
		expected []string
	}{
		{
			doc:      "by key",
			filters:  []filters.KeyValuePair{filters.Arg("annotation", "annotation1")},
			expected: []string{one.Name, two.Name},
		},
		{
			doc:      "by key and value",
			filters:  []filters.KeyValuePair{filters.Arg("annotation", "annotation1=value1")},
			expected: []string{one.Name},
		},
		{
			doc: "multiple annotations",
			filters: []filters.KeyValuePair{
				filters.Arg("annotation", "annotation1"),
				filters.Arg("annotation", "annotation2=value1"),
			},
			expected: []string{two.Name},
		},
		{
			doc:      "no match",
			filters:  []filters.KeyValuePair{filters.Arg("annotation", "annotation1=other")},
			expected: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			containerList, err := d.Containers(t.Context(), &backend.ContainerListOptions{
				All:     true,
				Filters: filters.NewArgs(tc.filters...),
			})
			assert.NilError(t, err)
			assert.Assert(t, is.Len(containerList, len(tc.expected)))
			for _, name := range tc.expected {
				assert.Assert(t, containerListContainsName(containerList, name))
			}
			assert.Assert(t, !containerListContainsName(containerList, three.Name))
		})
	}
}

func TestContainerList_ExcludeFilters(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	// Containers are listed newest first; space the timestamps so that the
	// order is deterministic: created, exited, unhealthy, healthy.
	now := time.Now()

	healthy := setupContainerWithName(t, "healthy", d)
	healthy.Created = now
	healthy.State.Health = &container.Health{Health: containertypes.Health{Status: containertypes.Healthy}}
	assert.NilError(t, db.Save(healthy))

	unhealthy := setupContainerWithName(t, "unhealthy", d)
	unhealthy.Created = now.Add(time.Second)
	unhealthy.State.Health = &container.Health{Health: containertypes.Health{Status: containertypes.Unhealthy}}
	assert.NilError(t, db.Save(unhealthy))

	exited := setupContainerWithName(t, "exited", d)
	exited.Created = now.Add(2 * time.Second)
	exited.State.Running = false
	exited.State.StartedAt = now
	assert.NilError(t, db.Save(exited))

	created := setupContainerWithName(t, "created", d)
	created.Created = now.Add(3 * time.Second)
	created.State.Running = false
	assert.NilError(t, db.Save(created))

	tests := []struct {
		doc      string
		all      bool
		limit    int
		filters  []filters.KeyValuePair
		expected []string
	}{
		{
			doc:      "status! includes stopped containers",
			filters:  []filters.KeyValuePair{filters.Arg("status!", "exited")},
			expected: []string{created.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc:      "status! running",
			filters:  []filters.KeyValuePair{filters.Arg("status!", "running")},
			expected: []string{created.Name, exited.Name},
		},
		{
			doc: "status and status! are combined",
			filters: []filters.KeyValuePair{
				filters.Arg("status", "running"),
				filters.Arg("status!", "exited"),
			},
			expected: []string{unhealthy.Name, healthy.Name},
		},
		{
			doc: "contradicting status and status!",
			filters: []filters.KeyValuePair{
				filters.Arg("status", "running"),
				filters.Arg("status!", "running"),
			},
			expected: []string{},
		},
		{
			doc: "multiple status! values",
			filters: []filters.KeyValuePair{
				filters.Arg("status!", "running"),
				filters.Arg("status!", "exited"),
			},
			expected: []string{created.Name},
		},
		{
			doc:      "status! with limit",
			limit:    1,
			filters:  []filters.KeyValuePair{filters.Arg("status!", "created")},
			expected: []string{exited.Name},
		},
		{
			doc:      "health! does not include stopped containers",
			filters:  []filters.KeyValuePair{filters.Arg("health!", "healthy")},
			expected: []string{unhealthy.Name},
		},
		{
			doc:      "health! with all",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("health!", "healthy")},
			expected: []string{created.Name, exited.Name, unhealthy.Name},
		},
		{
			doc:      "name! without slash",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("name!", "healthy")},
			expected: []string{created.Name, exited.Name},
		},
		{
			doc:      "name! with slash",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("name!", "/exited")},
			expected: []string{created.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc:      "name! anchored without slash",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("name!", "^exited$")},
			expected: []string{created.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc:      "name! anchored with slash",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("name!", "^/exited$")},
			expected: []string{created.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc: "name and name! are combined",
			all: true,
			filters: []filters.KeyValuePair{
				filters.Arg("name", "healthy"),
				filters.Arg("name!", "^unhealthy$"),
			},
			expected: []string{healthy.Name},
		},
		{
			doc:      "name! with invalid regular expression",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("name!", "[")},
			expected: []string{created.Name, exited.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc:      "id!",
			all:      true,
			filters:  []filters.KeyValuePair{filters.Arg("id!", exited.ID)},
			expected: []string{created.Name, unhealthy.Name, healthy.Name},
		},
		{
			doc: "contradicting id and id!",
			all: true,
			filters: []filters.KeyValuePair{
				filters.Arg("id", exited.ID),
				filters.Arg("id!", exited.ID),
			},
			expected: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			containerList, err := d.Containers(t.Context(), &backend.ContainerListOptions{
				All:     tc.all,
				Limit:   tc.limit,
				Filters: filters.NewArgs(tc.filters...),
			})
			assert.NilError(t, err)

			names := make([]string, 0, len(containerList))
			for _, ctr := range containerList {
				names = append(names, ctr.Names...)
			}
			assert.Check(t, is.DeepEqual(names, tc.expected))
		})
	}
}

func TestContainerList_ExcludeFilters_Invalid(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	tests := []struct {
		filter   filters.KeyValuePair
		expected string
	}{
		{filter: filters.Arg("status!", "bogus"), expected: "invalid filter 'status!=bogus'"},
		{filter: filters.Arg("health!", "bogus"), expected: "invalid filter 'health!=bogus'"},
		{filter: filters.Arg("label!", "foo"), expected: "invalid filter 'label!'"},
		{filter: filters.Arg("ancestor!", "foo"), expected: "invalid filter 'ancestor!'"},
	}

	for _, tc := range tests {
		t.Run(tc.filter.Key, func(t *testing.T) {
			_, err := d.Containers(t.Context(), &backend.ContainerListOptions{
				Filters: filters.NewArgs(tc.filter),
			})
			assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
			assert.Check(t, is.ErrorContains(err, tc.expected))
		})
	}
}

func TestContainerList_LimitFilter(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	d := &Daemon{
		containersReplica: db,
	}

	// start containers
	num := 32
	for i := range num {
		name := fmt.Sprintf("cont-%d", i)
		setupContainerWithName(t, name, d)
	}

	containers, err := db.Snapshot().All()
	assert.NilError(t, err)
	assert.Assert(t, is.Len(containers, num))

	tests := []struct {
		limit int
		doc   string
	}{
		{limit: 0, doc: "no limit"},
		{limit: -1, doc: "negative limit doesn't limit"},
		{limit: 1, doc: "limit 1 container"},
		{limit: 20, doc: "limit less than num containers"},
		{limit: 32, doc: "limit equal num containers"},
		{limit: 40, doc: "limit greater than num containers"},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			containerList, err := d.Containers(t.Context(), &backend.ContainerListOptions{Limit: tc.limit})
			assert.NilError(t, err)
			expectedListLen := num
			if tc.limit > 0 {
				expectedListLen = min(num, tc.limit)
			}
			assert.Assert(t, is.Len(containerList, expectedListLen))
		})
	}
}

// layerSizeErrImageService fails GetContainerLayerSize for the containers
// listed in failIDs and reports a fixed size for all others.
type layerSizeErrImageService struct {
	ImageService
	failIDs map[string]struct{}
}

func (s *layerSizeErrImageService) GetContainerLayerSize(_ context.Context, id string) (int64, int64, error) {
	if _, ok := s.failIDs[id]; ok {
		return 0, 0, errors.New("snapshotter.Usage failed: lstat: no such file or directory")
	}
	return 10, 20, nil
}

// A container whose size cannot be computed must not fail the whole listing.
func TestContainerList_SizeError(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	imgSvc := &layerSizeErrImageService{failIDs: map[string]struct{}{}}
	d := &Daemon{
		containersReplica: db,
		imageService:      imgSvc,
	}

	setupContainerWithName(t, "ok", d)
	bad := setupContainerWithName(t, "bad", d)
	imgSvc.failIDs[bad.ID] = struct{}{}

	list, err := d.Containers(t.Context(), &backend.ContainerListOptions{All: true, Size: true})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(list, 2))
	for _, c := range list {
		if c.ID == bad.ID {
			assert.Check(t, is.Equal(c.SizeRw, int64(-1)))
			assert.Check(t, is.Equal(c.SizeRootFs, int64(-1)))
		} else {
			assert.Check(t, is.Equal(c.SizeRw, int64(10)))
			assert.Check(t, is.Equal(c.SizeRootFs, int64(20)))
		}
	}
}

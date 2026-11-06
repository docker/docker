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

func (s *layerSizeErrImageService) GetContainerLayerSize(ctx context.Context, id string) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
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

func TestContainerList_SizeContextCancellation(t *testing.T) {
	db, err := container.NewViewDB()
	assert.NilError(t, err)
	imgSvc := &layerSizeErrImageService{failIDs: map[string]struct{}{}}
	d := &Daemon{
		containersReplica: db,
		imageService:      imgSvc,
	}

	setupContainerWithName(t, "ok-cancel", d)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = d.Containers(ctx, &backend.ContainerListOptions{All: true, Size: true})
	assert.Check(t, is.ErrorIs(err, context.Canceled))
}

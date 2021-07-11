package service

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/volume"
	volumedrivers "github.com/docker/docker/volume/drivers"
	"github.com/docker/docker/volume/service/opts"
	"github.com/docker/docker/volume/testutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestServiceCreate(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))

	ctx := context.Background()
	service, cleanup := newTestService(t, ds)
	defer cleanup()

	_, err := service.Create(ctx, "v1", "notexist")
	assert.Assert(t, errdefs.IsNotFound(err), err)

	v, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)

	vCopy, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(v, vCopy))

	_, err = service.Create(ctx, "v1", "d2")
	assert.Check(t, IsNameConflict(err), err)
	assert.Check(t, errdefs.IsConflict(err), err)

	assert.Assert(t, service.Remove(ctx, "v1"))
	_, err = service.Create(ctx, "v1", "d2")
	assert.NilError(t, err)
	_, err = service.Create(ctx, "v1", "d2")
	assert.NilError(t, err)

}

type spyEventLogger struct{}

var logCount = 0

func (l spyEventLogger) LogVolumeEvent(_, _ string, _ map[string]string) {
	logCount++
}

func TestServiceCreateLogs(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))

	ctx := context.Background()
	service, cleanup := newTestService(t, ds)
	service.eventLogger = spyEventLogger{}
	defer cleanup()

	v, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)

	vCopy, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(v, vCopy))

	assert.Assert(t, logCount == 1)
}

func TestServiceList(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()

	ctx := context.Background()

	_, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)
	_, err = service.Create(ctx, "v2", "d1")
	assert.NilError(t, err)
	_, err = service.Create(ctx, "v3", "d2")
	assert.NilError(t, err)

	ls, _, err := service.List(ctx, filters.NewArgs(filters.Arg("driver", "d1")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 2))

	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("driver", "d2")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 1))

	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("driver", "notexist")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 0))

	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 3))
	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "false")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 0))

	_, err = service.Get(ctx, "v1", opts.WithGetReference("foo"))
	assert.NilError(t, err)
	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 2))
	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "false")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 1))

	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "false"), filters.Arg("driver", "d2")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 0))
	ls, _, err = service.List(ctx, filters.NewArgs(filters.Arg("dangling", "true"), filters.Arg("driver", "d2")))
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 1))
}

func TestServiceRemove(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()
	ctx := context.Background()

	_, err := service.Create(ctx, "test", "d1")
	assert.NilError(t, err)

	assert.Assert(t, service.Remove(ctx, "test"))
	assert.Assert(t, service.Remove(ctx, "test", opts.WithPurgeOnError(true)))
}

func TestServiceGet(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()
	ctx := context.Background()

	v, err := service.Get(ctx, "notexist")
	assert.Assert(t, IsNotExist(err))
	assert.Check(t, v == nil)

	created, err := service.Create(ctx, "test", "d1")
	assert.NilError(t, err)
	assert.Assert(t, created != nil)

	v, err = service.Get(ctx, "test")
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(created, v))

	v, err = service.Get(ctx, "test", opts.WithGetResolveStatus)
	assert.NilError(t, err)
	assert.Assert(t, is.Len(v.Status, 1), v.Status)

	_, err = service.Get(ctx, "test", opts.WithGetDriver("notarealdriver"))
	assert.Assert(t, errdefs.IsConflict(err), err)
	v, err = service.Get(ctx, "test", opts.WithGetDriver("d1"))
	assert.Assert(t, err == nil)
	assert.Assert(t, is.DeepEqual(created, v))

	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))
	_, err = service.Get(ctx, "test", opts.WithGetDriver("d2"))
	assert.Assert(t, errdefs.IsConflict(err), err)
}

func TestServicePrune(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver(volume.DefaultDriverName), volume.DefaultDriverName))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("other"), "other"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()
	ctx := context.Background()

	_, err := service.Create(ctx, "test", volume.DefaultDriverName)
	assert.NilError(t, err)
	_, err = service.Create(ctx, "test2", "other")
	assert.NilError(t, err)

	pr, err := service.Prune(ctx, filters.NewArgs(filters.Arg("label", "banana")))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))

	pr, err = service.Prune(ctx, filters.NewArgs())
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))

	_, err = service.Get(ctx, "test")
	assert.Assert(t, IsNotExist(err), err)

	v, err := service.Get(ctx, "test2")
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(v.Driver, "other"))

	_, err = service.Create(ctx, "test", volume.DefaultDriverName)
	assert.NilError(t, err)

	pr, err = service.Prune(ctx, filters.NewArgs(filters.Arg("label!", "banana")))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))
	v, err = service.Get(ctx, "test2")
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(v.Driver, "other"))

	_, err = service.Create(ctx, "test", volume.DefaultDriverName, opts.WithCreateLabels(map[string]string{"banana": ""}))
	assert.NilError(t, err)
	pr, err = service.Prune(ctx, filters.NewArgs(filters.Arg("label!", "banana")))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))

	_, err = service.Create(ctx, "test3", volume.DefaultDriverName, opts.WithCreateLabels(map[string]string{"banana": "split"}))
	assert.NilError(t, err)
	pr, err = service.Prune(ctx, filters.NewArgs(filters.Arg("label!", "banana=split")))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))

	pr, err = service.Prune(ctx, filters.NewArgs(filters.Arg("label", "banana=split")))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test3"))

	v, err = service.Create(ctx, "test", volume.DefaultDriverName, opts.WithCreateReference(t.Name()))
	assert.NilError(t, err)

	pr, err = service.Prune(ctx, filters.NewArgs())
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))
	assert.Assert(t, service.Release(ctx, v.Name, t.Name()))

	pr, err = service.Prune(ctx, filters.NewArgs())
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))
}

func newTestService(t *testing.T, ds *volumedrivers.Store) (*VolumesService, func()) {
	t.Helper()

	dir, err := ioutil.TempDir("", t.Name())
	assert.NilError(t, err)

	store, err := NewStore(dir, ds)
	assert.NilError(t, err)
	s := &VolumesService{vs: store, eventLogger: dummyEventLogger{}}
	return s, func() {
		assert.Check(t, s.Shutdown())
		assert.Check(t, os.RemoveAll(dir))
	}
}

type dummyEventLogger struct{}

func (dummyEventLogger) LogVolumeEvent(_, _ string, _ map[string]string) {}

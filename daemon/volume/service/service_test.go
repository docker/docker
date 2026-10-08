package service

import (
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/prune"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/pruneprogress"
	"github.com/moby/moby/v2/daemon/volume"
	volumedrivers "github.com/moby/moby/v2/daemon/volume/drivers"
	"github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/moby/moby/v2/daemon/volume/testutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestServiceCreate(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))

	ctx := t.Context()
	service, cleanup := newTestService(t, ds)
	defer cleanup()

	_, err := service.Create(ctx, "v1", "notexist")
	assert.Assert(t, cerrdefs.IsNotFound(err), err)

	v, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)

	vCopy, err := service.Create(ctx, "v1", "d1")
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(v, vCopy))

	_, err = service.Create(ctx, "v1", "d2")
	assert.Check(t, IsNameConflict(err), err)
	assert.Check(t, cerrdefs.IsConflict(err), err)

	assert.Assert(t, service.Remove(ctx, "v1"))
	_, err = service.Create(ctx, "v1", "d2")
	assert.NilError(t, err)
	_, err = service.Create(ctx, "v1", "d2")
	assert.NilError(t, err)
}

func TestServiceList(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d1"), "d1"))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()

	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

	v, err := service.Get(ctx, "notexist")
	assert.Assert(t, IsNotExist(err))
	assert.Check(t, is.Nil(v))

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
	assert.Assert(t, cerrdefs.IsConflict(err), err)
	v, err = service.Get(ctx, "test", opts.WithGetDriver("d1"))
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(created, v))

	assert.Assert(t, ds.Register(testutils.NewFakeDriver("d2"), "d2"))
	_, err = service.Get(ctx, "test", opts.WithGetDriver("d2"))
	assert.Assert(t, cerrdefs.IsConflict(err), err)
}

func TestServicePrune(t *testing.T) {
	t.Parallel()

	ds := volumedrivers.NewStore(nil)
	assert.Assert(t, ds.Register(testutils.NewFakeDriver(volume.DefaultDriverName), volume.DefaultDriverName))
	assert.Assert(t, ds.Register(testutils.NewFakeDriver("other"), "other"))

	service, cleanup := newTestService(t, ds)
	defer cleanup()
	var progress []prune.Object
	ctx := t.Context()
	progressSink := func(message prune.Object) {
		progress = append(progress, message)
	}

	_, err := service.Create(ctx, "test", volume.DefaultDriverName)
	assert.NilError(t, err)
	_, err = service.Create(ctx, "test2", "other")
	assert.NilError(t, err)

	pr, err := service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("label", "banana"), filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))
	assert.Assert(t, is.Len(progress, 0), "filtered volumes must not be reported as deleted")

	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))
	assert.DeepEqual(t, progress, []prune.Object{{ID: "test", Action: prune.Deleted}})

	_, err = service.Get(ctx, "test")
	assert.Assert(t, IsNotExist(err), err)

	v, err := service.Get(ctx, "test2")
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(v.Driver, "other"))

	_, err = service.Create(ctx, "test", volume.DefaultDriverName)
	assert.NilError(t, err)

	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("label!", "banana"), filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))
	v, err = service.Get(ctx, "test2")
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(v.Driver, "other"))

	_, err = service.Create(ctx, "test", volume.DefaultDriverName, opts.WithCreateLabels(map[string]string{"banana": ""}))
	assert.NilError(t, err)
	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("label!", "banana")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))

	_, err = service.Create(ctx, "test3", volume.DefaultDriverName, opts.WithCreateLabels(map[string]string{"banana": "split"}))
	assert.NilError(t, err)
	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("label!", "banana=split"), filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))

	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("label", "banana=split"), filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test3"))

	v, err = service.Create(ctx, "test", volume.DefaultDriverName, opts.WithCreateReference(t.Name()))
	assert.NilError(t, err)

	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 0))
	assert.Assert(t, service.Release(ctx, v.Name, t.Name()))

	pr, err = service.Prune(ctx, pruneprogress.Options{Filters: filters.NewArgs(filters.Arg("all", "true")), Progress: progressSink})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(pr.VolumesDeleted, 1))
	assert.Assert(t, is.Equal(pr.VolumesDeleted[0], "test"))
}

func newTestService(t *testing.T, ds *volumedrivers.Store) (*VolumesService, func()) {
	t.Helper()

	dir := t.TempDir()

	store, err := NewStore(dir, ds)
	assert.NilError(t, err)
	s := &VolumesService{vs: store, eventLogger: dummyEventLogger{}}
	return s, func() {
		assert.Check(t, s.Shutdown())
	}
}

type dummyEventLogger struct{}

func (dummyEventLogger) LogVolumeEvent(_ string, _ events.Action, _ map[string]string) {}

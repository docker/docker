package containerd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/defaults"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/go-archive"
	"github.com/moby/moby/v2/daemon/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type viewRecordingSnapshotter struct {
	snapshots.Snapshotter
	viewErr error
	views   []string
	events  *[]string
}

func (s *viewRecordingSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	return snapshots.Info{Name: key, Parent: "parent"}, nil
}

func (s *viewRecordingSnapshotter) View(_ context.Context, key, _ string, _ ...snapshots.Opt) ([]mount.Mount, error) {
	if s.viewErr != nil {
		return nil, s.viewErr
	}
	s.views = append(s.views, key)
	return nil, nil
}

func (s *viewRecordingSnapshotter) Remove(_ context.Context, key string) error {
	*s.events = append(*s.events, "remove view")
	return nil
}

type fakeRWLayer struct {
	root   string
	events *[]string
}

func (l *fakeRWLayer) Mount(string) (string, error) { return l.root, nil }

func (l *fakeRWLayer) Unmount() error {
	*l.events = append(*l.events, "unmount rwlayer")
	return nil
}

func (l *fakeRWLayer) Metadata() (map[string]string, error) { return nil, nil }

func TestChanges(t *testing.T) {
	errView := errors.New("view failed")
	errActivate := errors.New("activate failed")
	errDeactivate := errors.New("deactivate failed")

	tests := []struct {
		name          string
		viewErr       error
		activateErr   error
		deactivateErr error
		compareFails  bool
		cancelCtx     bool
		wantErrs      []error
		wantEvents    []string
	}{
		{
			name:       "success",
			wantEvents: []string{"deactivate", "unmount rwlayer", "remove view"},
		},
		{
			name:     "view fails",
			viewErr:  errView,
			wantErrs: []error{errView},
		},
		{
			name:        "activate fails",
			activateErr: errActivate,
			wantErrs:    []error{errActivate},
			wantEvents:  []string{"unmount rwlayer", "remove view"},
		},
		{
			name:        "mount manager not implemented",
			activateErr: cerrdefs.ErrNotImplemented,
			wantEvents:  []string{"unmount rwlayer", "remove view"},
		},
		{
			name:          "deactivate fails",
			deactivateErr: errDeactivate,
			wantErrs:      []error{errDeactivate},
			wantEvents:    []string{"deactivate", "unmount rwlayer", "remove view"},
		},
		{
			name:          "compare and deactivate fail",
			compareFails:  true,
			deactivateErr: errDeactivate,
			wantErrs:      []error{os.ErrNotExist, errDeactivate},
			wantEvents:    []string{"deactivate", "unmount rwlayer", "remove view"},
		},
		{
			name:       "cancelled context",
			cancelCtx:  true,
			wantEvents: []string{"deactivate", "unmount rwlayer", "remove view"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			var events []string
			sn := &viewRecordingSnapshotter{viewErr: tc.viewErr, events: &events}
			mm := &fakeMountManager{activateErr: tc.activateErr, deactivateErr: tc.deactivateErr, events: &events}
			c8dCli, err := containerd.New("", containerd.WithServices(
				containerd.WithSnapshotters(map[string]snapshots.Snapshotter{defaults.DefaultSnapshotter: sn}),
				containerd.WithMountManager(mm),
				containerd.WithLeasesService(noopLeasesManager{}),
			))
			assert.NilError(t, err)
			i := &ImageService{client: c8dCli, snapshotter: defaults.DefaultSnapshotter}

			root := t.TempDir()
			assert.NilError(t, os.WriteFile(filepath.Join(root, "added"), nil, 0o644))
			if tc.compareFails {
				root = filepath.Join(root, "missing")
			}
			ctr := &container.Container{
				ID:      "ctr",
				Driver:  defaults.DefaultSnapshotter,
				RWLayer: &fakeRWLayer{root: root, events: &events},
			}

			changes, err := i.Changes(ctx, ctr)
			if len(tc.wantErrs) == 0 {
				assert.NilError(t, err)
				assert.Check(t, is.DeepEqual(changes, []archive.Change{{Path: filepath.FromSlash("/added"), Kind: archive.ChangeAdd}}))
			}
			for _, want := range tc.wantErrs {
				assert.Check(t, is.ErrorIs(err, want))
			}
			assert.Check(t, is.DeepEqual(events, tc.wantEvents))
			assert.Check(t, is.DeepEqual(mm.deactivated, mm.activated))
			if len(mm.deactivated) > 0 {
				assert.Check(t, is.DeepEqual(mm.deactivated, sn.views))
				assert.Check(t, mm.deactivateCtx.Err() == nil, "Deactivate must not use a cancelled context")
			}
		})
	}
}

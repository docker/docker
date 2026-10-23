package containerd

import (
	"context"
	"errors"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/defaults"
	cerrdefs "github.com/containerd/errdefs"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type fakeMountManager struct {
	mount.Manager
	activateErr   error
	deactivateErr error
	activated     []string
	deactivated   []string
	deactivateCtx context.Context
}

func (m *fakeMountManager) Activate(_ context.Context, name string, _ []mount.Mount, _ ...mount.ActivateOpt) (mount.ActivationInfo, error) {
	if m.activateErr != nil {
		return mount.ActivationInfo{}, m.activateErr
	}
	m.activated = append(m.activated, name)
	return mount.ActivationInfo{Name: name}, nil
}

func (m *fakeMountManager) Deactivate(ctx context.Context, name string) error {
	m.deactivated = append(m.deactivated, name)
	m.deactivateCtx = ctx
	return m.deactivateErr
}

func TestSetupInitLayer(t *testing.T) {
	errSetup := errors.New("setup failed")
	errDeactivate := errors.New("deactivate failed")

	tests := []struct {
		name           string
		activateErr    error
		setupErr       error
		deactivateErr  error
		wantErrs       []error
		wantDeactivate bool
	}{
		{
			name:           "success",
			wantDeactivate: true,
		},
		{
			name:           "setup fails",
			setupErr:       errSetup,
			wantErrs:       []error{errSetup},
			wantDeactivate: true,
		},
		{
			name:           "deactivate fails",
			deactivateErr:  errDeactivate,
			wantErrs:       []error{errDeactivate},
			wantDeactivate: true,
		},
		{
			name:           "setup and deactivate fail",
			setupErr:       errSetup,
			deactivateErr:  errDeactivate,
			wantErrs:       []error{errSetup, errDeactivate},
			wantDeactivate: true,
		},
		{
			name:        "mount manager not implemented",
			activateErr: cerrdefs.ErrNotImplemented,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			mm := &fakeMountManager{activateErr: tc.activateErr, deactivateErr: tc.deactivateErr}

			var setupCalled bool
			err := setupInitLayer(ctx, mm, nil, func(string) error {
				setupCalled = true
				// Deactivation must still run with a cancelled parent context.
				cancel()
				return tc.setupErr
			})

			assert.Check(t, setupCalled)
			if len(tc.wantErrs) == 0 {
				assert.NilError(t, err)
			}
			for _, want := range tc.wantErrs {
				assert.Check(t, is.ErrorIs(err, want))
			}

			if tc.wantDeactivate {
				assert.Check(t, is.DeepEqual(mm.deactivated, mm.activated))
				assert.Check(t, is.Len(mm.deactivated, 1))
				assert.Check(t, mm.deactivateCtx.Err() == nil, "Deactivate must not use the cancelled context")
			} else {
				assert.Check(t, is.Len(mm.deactivated, 0))
			}
		})
	}
}

type commitRecordingSnapshotter struct {
	snapshots.Snapshotter
	committed []string
}

func (s *commitRecordingSnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (s *commitRecordingSnapshotter) Commit(_ context.Context, name, _ string, _ ...snapshots.Opt) error {
	s.committed = append(s.committed, name)
	return nil
}

func TestPrepareInitLayerDeactivate(t *testing.T) {
	errDeactivate := errors.New("deactivate failed")

	tests := []struct {
		name          string
		deactivateErr error
		wantCommitted []string
	}{
		{
			name:          "deactivate succeeds",
			wantCommitted: []string{"ctr-init"},
		},
		{
			name:          "deactivate fails",
			deactivateErr: errDeactivate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			sn := &commitRecordingSnapshotter{}
			mm := &fakeMountManager{deactivateErr: tc.deactivateErr}

			c8dCli, err := containerd.New("", containerd.WithServices(
				containerd.WithSnapshotters(map[string]snapshots.Snapshotter{defaults.DefaultSnapshotter: sn}),
				containerd.WithMountManager(mm),
			))
			assert.NilError(t, err)
			i := &ImageService{client: c8dCli, snapshotter: defaults.DefaultSnapshotter}

			err = i.prepareInitLayer(ctx, "ctr", "parent", func(string) error { return nil })
			if tc.deactivateErr != nil {
				assert.Check(t, is.ErrorIs(err, tc.deactivateErr))
			} else {
				assert.NilError(t, err)
			}
			assert.Check(t, is.DeepEqual(sn.committed, tc.wantCommitted))
		})
	}
}

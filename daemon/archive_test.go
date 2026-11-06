package daemon

import (
	"bytes"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// TestContainerExtractToDirRemovedContainer verifies that extracting an archive
// into a container that is being removed (or already dead) fails with a
// Conflict error instead of the internal "RWLayer ... is unexpectedly nil"
// error. This happens when a copy races with removal of the container, for
// example when a container created with "--rm" exits while content is being
// copied into it; see https://github.com/moby/moby/issues/53886.
func TestContainerExtractToDirRemovedContainer(t *testing.T) {
	testCases := []struct {
		name     string
		mutate   func(*container.Container)
		expected string
	}{
		{
			name:     "removal in progress",
			mutate:   func(c *container.Container) { c.State.SetRemovalInProgress() },
			expected: "is being removed",
		},
		{
			name:     "dead",
			mutate:   func(c *container.Container) { c.State.Dead = true },
			expected: "is dead",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctr := &container.Container{ID: "test-container", State: &container.State{}}
			tc.mutate(ctr)
			store := container.NewMemoryStore()
			store.Add(ctr.ID, ctr)
			d := &Daemon{containers: store}

			err := d.ContainerExtractToDir(ctr.ID, "/", false, true, bytes.NewReader(nil))
			// The error must be typed as a Conflict; had it been passed through
			// errdefs.System, it would surface as a server error instead.
			assert.Assert(t, cerrdefs.IsConflict(err), "expected conflict error, got: %v", err)
			assert.Check(t, is.ErrorContains(err, tc.expected))
		})
	}
}

// TestContainerExtractToDirNilRWLayer verifies that a container that is not
// being removed still reports the internal error, so that a genuine bug
// (an RW layer going missing for an unrelated reason) is not silently masked.
func TestContainerExtractToDirNilRWLayer(t *testing.T) {
	ctr := &container.Container{ID: "test-container", State: &container.State{}}
	store := container.NewMemoryStore()
	store.Add(ctr.ID, ctr)
	d := &Daemon{containers: store}

	err := d.ContainerExtractToDir(ctr.ID, "/", false, true, bytes.NewReader(nil))
	assert.Assert(t, !cerrdefs.IsConflict(err), "expected an internal error, got: %v", err)
	assert.Check(t, is.ErrorContains(err, "RWLayer of container test-container is unexpectedly nil"))
}

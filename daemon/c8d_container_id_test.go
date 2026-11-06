package daemon

import (
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/container"
	"gotest.tools/v3/assert"
)

func TestC8dContainerID(t *testing.T) {
	t.Parallel()

	containerID := strings.Repeat("a", dockerContainerIDLength)
	ctr := container.NewBaseContainer(containerID, t.TempDir())

	assert.Equal(t, ctr.C8dContainerID(), containerID)

	ctr.State.RunID = 1
	first := ctr.C8dContainerID()
	assert.Equal(t, first, containerID[:c8dContainerIDPrefixLength]+"00000000000000000000000000000001")

	ctr.State.RunID++
	second := ctr.C8dContainerID()
	assert.Equal(t, len(second), dockerContainerIDLength)
	assert.Equal(t, second[:c8dContainerIDPrefixLength], containerID[:c8dContainerIDPrefixLength])
	assert.Assert(t, first < second)
}

func TestGetContainerByC8dContainerID(t *testing.T) {
	t.Parallel()

	containerID := strings.Repeat("a", dockerContainerIDLength)
	ctr := container.NewBaseContainer(containerID, t.TempDir())
	ctr.State.RunID = 1
	c8dContainerID := ctr.C8dContainerID()
	otherID := strings.Repeat("b", dockerContainerIDLength)
	other := container.NewBaseContainer(otherID, t.TempDir())

	store := container.NewMemoryStore()
	store.Add(containerID, ctr)
	store.Add(otherID, other)
	viewDB, err := container.NewViewDB()
	assert.NilError(t, err)
	assert.NilError(t, viewDB.Save(&container.Container{ID: containerID}))
	assert.NilError(t, viewDB.Save(&container.Container{ID: otherID}))
	daemon := &Daemon{
		containers:        store,
		containersReplica: viewDB,
	}

	got, err := daemon.getContainerByC8dContainerID(c8dContainerID)
	assert.NilError(t, err)
	assert.Assert(t, got == ctr)

	got, err = daemon.getContainerByC8dContainerID(containerID)
	assert.NilError(t, err)
	assert.Assert(t, got == ctr)

	_, err = daemon.getContainerByC8dContainerID("invalid")
	assert.ErrorContains(t, err, "No such container")

	ambiguousPrefix := strings.Repeat("c", c8dContainerIDPrefixLength)
	assert.NilError(t, viewDB.Save(&container.Container{ID: ambiguousPrefix + strings.Repeat("d", c8dContainerIDPrefixLength)}))
	assert.NilError(t, viewDB.Save(&container.Container{ID: ambiguousPrefix + strings.Repeat("e", c8dContainerIDPrefixLength)}))

	_, err = daemon.getContainerByC8dContainerID(ambiguousPrefix + strings.Repeat("f", c8dContainerIDPrefixLength))
	assert.Assert(t, cerrdefs.IsInvalidArgument(err))
}

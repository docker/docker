//go:build linux

package quota

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// 10MB
const testQuotaSize = 10 * 1024 * 1024

func TestBlockDev(t *testing.T) {
	if msg, ok := CanTestQuota(); !ok {
		t.Skip(msg)
	}

	// get sparse xfs test image
	imageFileName, err := PrepareQuotaTestImage(t)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(imageFileName)

	t.Run("testBlockDevQuotaDisabled", WrapMountTest(imageFileName, false, testBlockDevQuotaDisabled))
	t.Run("testBlockDevQuotaEnabled", WrapMountTest(imageFileName, true, testBlockDevQuotaEnabled))
	t.Run("testSmallerThanQuota", WrapMountTest(imageFileName, true, WrapQuotaTest(testSmallerThanQuota)))
	t.Run("testBiggerThanQuota", WrapMountTest(imageFileName, true, WrapQuotaTest(testBiggerThanQuota)))
	t.Run("testRetrieveQuota", WrapMountTest(imageFileName, true, WrapQuotaTest(testRetrieveQuota)))
	t.Run("testRemoveQuota", WrapMountTest(imageFileName, true, WrapQuotaTest(testRemoveQuota)))
	t.Run("testFreeListReuse", WrapMountTest(imageFileName, true, WrapQuotaTest(testFreeListReuse)))
	t.Run("testConcurrentQuota", WrapMountTest(imageFileName, true, WrapQuotaTest(testConcurrentQuota)))
	t.Run("testGapRecovery", WrapMountTest(imageFileName, true, WrapQuotaTest(testGapRecovery)))
}

func testBlockDevQuotaDisabled(t *testing.T, mountPoint, backingFsDev, testDir string) {
	hasSupport, err := hasQuotaSupport(backingFsDev)
	assert.NilError(t, err)
	assert.Check(t, !hasSupport)
}

func testBlockDevQuotaEnabled(t *testing.T, mountPoint, backingFsDev, testDir string) {
	hasSupport, err := hasQuotaSupport(backingFsDev)
	assert.NilError(t, err)
	assert.Check(t, hasSupport)
}

func testSmallerThanQuota(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	assert.NilError(t, ctrl.SetQuota(testSubDir, Quota{testQuotaSize}))
	smallerThanQuotaFile := filepath.Join(testSubDir, "smaller-than-quota")
	assert.NilError(t, os.WriteFile(smallerThanQuotaFile, make([]byte, testQuotaSize/2), 0o644))
	assert.NilError(t, os.Remove(smallerThanQuotaFile))
}

func testBiggerThanQuota(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	// Make sure the quota is being enforced
	// TODO: When we implement this under EXT4, we need to shed CAP_SYS_RESOURCE, otherwise
	// we're able to violate quota without issue
	assert.NilError(t, ctrl.SetQuota(testSubDir, Quota{testQuotaSize}))

	biggerThanQuotaFile := filepath.Join(testSubDir, "bigger-than-quota")
	err := os.WriteFile(biggerThanQuotaFile, make([]byte, testQuotaSize+1), 0o644)
	assert.ErrorContains(t, err, "")
	if errors.Is(err, io.ErrShortWrite) {
		assert.NilError(t, os.Remove(biggerThanQuotaFile))
	}
}

func testRetrieveQuota(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	// Validate that we can retrieve quota
	assert.NilError(t, ctrl.SetQuota(testSubDir, Quota{testQuotaSize}))

	var q Quota
	assert.NilError(t, ctrl.GetQuota(testSubDir, &q))
	assert.Check(t, is.Equal(uint64(testQuotaSize), q.Size))
}

func testRemoveQuota(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	// Set a quota and verify it is in place
	assert.NilError(t, ctrl.SetQuota(testSubDir, Quota{testQuotaSize}))

	var q Quota
	assert.NilError(t, ctrl.GetQuota(testSubDir, &q))
	assert.Check(t, is.Equal(uint64(testQuotaSize), q.Size))

	// Remove the directory first (as the driver does in Remove),
	// then remove quota (pure in-memory bookkeeping)
	assert.NilError(t, os.RemoveAll(testSubDir))
	assert.NilError(t, ctrl.RemoveQuota(testSubDir))

	// GetQuota should now fail because the path is no longer tracked
	err := ctrl.GetQuota(testSubDir, &q)
	assert.Assert(t, is.ErrorContains(err, "quota not found"))

	// RemoveQuota on a path with no quota should be a no-op (no error)
	assert.NilError(t, ctrl.RemoveQuota(testSubDir))
}

func testFreeListReuse(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	// Set a quota on testSubDir to get a project ID
	assert.NilError(t, ctrl.SetQuota(testSubDir, Quota{testQuotaSize}))

	// Record the project ID assigned to testSubDir
	ctrl.Lock()
	oldID := ctrl.quotas[testSubDir]
	ctrl.Unlock()

	// Remove the directory first (as the driver does in Remove),
	// then remove quota (pure in-memory bookkeeping)
	assert.NilError(t, os.RemoveAll(testSubDir))
	assert.NilError(t, ctrl.RemoveQuota(testSubDir))

	// Create a new directory and set a quota on it
	reuseDir, err := os.MkdirTemp(testDir, "reuse-test")
	assert.NilError(t, err)

	assert.NilError(t, ctrl.SetQuota(reuseDir, Quota{testQuotaSize}))

	// Verify the freed project ID was reused (not a new allocation)
	ctrl.Lock()
	newID := ctrl.quotas[reuseDir]
	ctrl.Unlock()
	assert.Check(t, is.Equal(oldID, newID))

	// The free list should now be empty
	state := getPquotaState()
	state.Lock()
	assert.Check(t, is.Equal(0, len(state.freeProjectIDs)))
	state.Unlock()
}

func testConcurrentQuota(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	const numDirs = 20
	const numWorkers = 4

	dirs := make([]string, numDirs)
	for i := range dirs {
		dir, err := os.MkdirTemp(testDir, "concurrent-test")
		assert.NilError(t, err)
		dirs[i] = dir
	}

	// Concurrently set quotas on all directories.
	// Use t.Errorf (not assert) inside goroutines — t.Fatal is unsafe
	// from non-test goroutines.
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := workerID; i < numDirs; i += numWorkers {
				if err := ctrl.SetQuota(dirs[i], Quota{testQuotaSize}); err != nil {
					t.Errorf("SetQuota failed for dir %d: %v", i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify all directories have unique project IDs
	ctrl.RLock()
	seen := make(map[uint32]bool)
	for _, dir := range dirs {
		id := ctrl.quotas[dir]
		assert.Assert(t, !seen[id], "duplicate project ID %d assigned to multiple directories", id)
		seen[id] = true
	}
	ctrl.RUnlock()

	// Concurrently remove directories and quotas
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := workerID; i < numDirs; i += numWorkers {
				if err := os.RemoveAll(dirs[i]); err != nil {
					t.Errorf("RemoveAll failed for dir %d: %v", i, err)
					return
				}
				if err := ctrl.RemoveQuota(dirs[i]); err != nil {
					t.Errorf("RemoveQuota failed for dir %d: %v", i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// All project IDs should be in the free list and removed from in-use
	state := getPquotaState()
	state.Lock()
	for id := range seen {
		assert.Assert(t, !state.projectIDsInUse[id], "project ID %d should not be in-use after RemoveQuota", id)
	}
	assert.Check(t, is.Equal(numDirs, len(state.freeProjectIDs)))
	state.Unlock()
}

func testGapRecovery(t *testing.T, ctrl *Control, homeDir, testDir, testSubDir string) {
	const numDirs = 5
	dirs := make([]string, numDirs)
	for i := range dirs {
		dir, err := os.MkdirTemp(testDir, "gap-test")
		assert.NilError(t, err)
		assert.NilError(t, ctrl.SetQuota(dir, Quota{testQuotaSize}))
		dirs[i] = dir
	}

	// Record the project IDs assigned to each directory
	ctrl.RLock()
	ids := make([]uint32, numDirs)
	for i, dir := range dirs {
		ids[i] = ctrl.quotas[dir]
	}
	ctrl.RUnlock()

	// Remove some directories from the filesystem WITHOUT calling
	// RemoveQuota — simulating a daemon crash where in-memory state
	// is lost but directories were already deleted.
	removedIndices := []int{1, 3}
	for _, i := range removedIndices {
		assert.NilError(t, os.RemoveAll(dirs[i]))
	}

	// Simulate daemon restart: reset global pquotaState to a clean state
	state := getPquotaState()
	state.Lock()
	state.nextProjectID = 1
	state.freeProjectIDs = state.freeProjectIDs[:0]
	state.projectIDsInUse = make(map[uint32]bool)
	state.Unlock()

	// Create a new Control — findNextProjectID will scan the directory,
	// find remaining layers, and collect gaps into the free list.
	ctrl2, err := NewControl(testDir)
	assert.NilError(t, err)

	// The removed directories' project IDs should be in the free list
	state.Lock()
	freeSet := make(map[uint32]bool)
	for _, id := range state.freeProjectIDs {
		freeSet[id] = true
	}
	state.Unlock()

	for _, i := range removedIndices {
		assert.Check(t, freeSet[ids[i]], "gap project ID %d should be in free list after restart", ids[i])
	}

	// The remaining directories should be tracked by the new Control
	remainingIndices := []int{0, 2, 4}
	for _, i := range remainingIndices {
		var q Quota
		assert.NilError(t, ctrl2.GetQuota(dirs[i], &q))
		assert.Check(t, is.Equal(uint64(testQuotaSize), q.Size))
	}
}

package images

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type fakeContainerStore struct{}

func (fakeContainerStore) First(container.StoreFilter) *container.Container { return nil }
func (fakeContainerStore) List() []*container.Container                     { return nil }
func (fakeContainerStore) Get(string) *container.Container                  { return nil }

type countingImageStore struct {
	image.Store
	calls   atomic.Int32
	started chan struct{}
	wait    chan struct{}
}

func (c *countingImageStore) Map() map[image.ID]*image.Image {
	c.calls.Add(1)
	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.wait != nil {
		<-c.wait
	}
	return c.Store.Map()
}

func (c *countingImageStore) Heads() map[image.ID]*image.Image {
	c.calls.Add(1)
	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.wait != nil {
		<-c.wait
	}
	return c.Store.Heads()
}

func TestImageListSingleflight(t *testing.T) {
	ctx, _, images, cleanup := setupTestStores(t)
	defer cleanup(t)

	imgID, err := images.Create([]byte(`{"rootFS": {}}`))
	assert.NilError(t, err)

	refStore, err := refstore.NewReferenceStore(filepath.Join(t.TempDir(), "repositories.json"))
	assert.NilError(t, err)

	countingStore := &countingImageStore{
		Store:   images,
		started: make(chan struct{}, 1),
		wait:    make(chan struct{}),
	}

	service := &ImageService{
		imageStore:     countingStore,
		referenceStore: refStore,
		containers:     fakeContainerStore{},
	}

	const concurrency = 8
	var wg sync.WaitGroup
	wg.Add(concurrency)

	results := make([][]imagetypes.Summary, concurrency)
	errs := make([]error, concurrency)

	for i := range concurrency {
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = service.Images(ctx, imagebackend.ListOptions{})
		}(i)
	}

	select {
	case <-countingStore.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for store.Map to be called")
	}

	time.Sleep(50 * time.Millisecond)

	close(countingStore.wait)
	wg.Wait()

	for i := range concurrency {
		assert.NilError(t, errs[i])
		assert.Check(t, is.Equal(len(results[i]), 1))
		assert.Check(t, is.Equal(results[i][0].ID, imgID.String()))
	}

	// Verify that concurrent identical queries collapsed into exactly 1 underlying Map call.
	assert.Check(t, is.Equal(countingStore.calls.Load(), int32(1)))

	// Verify isolation: mutating the result from one caller must not affect other callers.
	results[0][0].RepoTags = append(results[0][0].RepoTags, "mutated:tag")
	for i := 1; i < concurrency; i++ {
		assert.Check(t, !slices.Contains(results[i][0].RepoTags, "mutated:tag"))
	}

	// Verify distinct query parameters trigger separate singleflights.
	countingStore.started = nil
	countingStore.wait = nil
	_, err = service.Images(ctx, imagebackend.ListOptions{All: true})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(countingStore.calls.Load(), int32(2)))

	// Verify context cancellation on one caller does not abort concurrent callers.
	started2 := make(chan struct{}, 1)
	wait2 := make(chan struct{})
	countingStore.started = started2
	countingStore.wait = wait2

	cancelingCtx, cancel := context.WithCancel(ctx)
	errCh1 := make(chan error, 1)
	errCh2 := make(chan error, 1)

	go func() {
		_, e := service.Images(cancelingCtx, imagebackend.ListOptions{})
		errCh1 <- e
	}()

	select {
	case <-started2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second in-flight flight")
	}

	go func() {
		_, e := service.Images(ctx, imagebackend.ListOptions{})
		errCh2 <- e
	}()

	time.Sleep(50 * time.Millisecond)

	// Cancel the first caller's context while in flight.
	cancel()

	e1 := <-errCh1
	assert.Check(t, is.ErrorIs(e1, context.Canceled))

	// Release the underlying store wait to let the shared operation finish for caller 2.
	close(wait2)

	e2 := <-errCh2
	assert.NilError(t, e2)
}

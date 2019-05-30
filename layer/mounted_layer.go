package layer // import "github.com/docker/docker/layer"

import (
	"io"
	"sync"

	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/containerfs"
)

type mountedLayer struct {
	name       string
	mountID    string
	initID     string
	parent     *roLayer
	path       string
	layerStore *layerStore

	sync.Mutex
	references map[RWLayer]*referencedRWLayer
}

func (ml *mountedLayer) cacheParent() string {
	if ml.initID != "" {
		return ml.initID
	}
	if ml.parent != nil {
		return ml.parent.cacheID
	}
	return ""
}

func (ml *mountedLayer) TarStream() (io.ReadCloser, error) {
	return ml.layerStore.driver.Diff(ml.mountID, ml.cacheParent())
}

func (ml *mountedLayer) Name() string {
	return ml.name
}

func (ml *mountedLayer) Parent() Layer {
	if ml.parent != nil {
		return ml.parent
	}

	// Return a nil interface instead of an interface wrapping a nil
	// pointer.
	return nil
}

func (ml *mountedLayer) Size() (int64, error) {
	return ml.layerStore.driver.DiffSize(ml.mountID, ml.cacheParent())
}

func (ml *mountedLayer) Changes() ([]archive.Change, error) {
	return ml.layerStore.driver.Changes(ml.mountID, ml.cacheParent())
}

func (ml *mountedLayer) Metadata() (map[string]string, error) {
	return ml.layerStore.driver.GetMetadata(ml.mountID)
}

func (ml *mountedLayer) getReference() RWLayer {
	ref := &referencedRWLayer{
		mountedLayer: ml,
	}
	ml.Lock()
	ml.references[ref] = ref
	ml.Unlock()

	return ref
}

func (ml *mountedLayer) hasReferences() bool {
	ml.Lock()
	ret := len(ml.references) > 0
	ml.Unlock()

	return ret
}

func (ml *mountedLayer) deleteReference(ref RWLayer) error {
	ml.Lock()
	defer ml.Unlock()
	if _, ok := ml.references[ref]; !ok {
		return ErrLayerNotRetained
	}
	delete(ml.references, ref)
	return nil
}

func (ml *mountedLayer) retakeReference(r RWLayer) {
	if ref, ok := r.(*referencedRWLayer); ok {
		ml.Lock()
		ml.references[ref] = ref
		ml.Unlock()
	}
}

type referencedRWLayer struct {
	*mountedLayer
}

func (rl *referencedRWLayer) Mount(mountLabel string) (containerfs.ContainerFS, error) {
	return rl.layerStore.driver.Get(rl.mountedLayer.mountID, mountLabel)
}

// Unmount decrements the activity count and unmounts the underlying layer
// Callers should only call `Unmount` once per call to `Mount`, even on error.
func (rl *referencedRWLayer) Unmount() error {
	return rl.layerStore.driver.Put(rl.mountedLayer.mountID)
}

// ApplyDiff applies specified diff to the layer
func (rl *referencedRWLayer) ApplyDiff(diff io.Reader) (int64, error) {
	return rl.layerStore.driver.ApplyDiff(rl.mountID, rl.cacheParent(), diff)
}

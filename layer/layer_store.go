package layer // import "github.com/docker/docker/layer"

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/docker/distribution"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/locker"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/system"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
)

// maxLayerDepth represents the maximum number of
// layers which can be chained together. 125 was
// chosen to account for the 127 max in some
// graphdrivers plus the 2 additional layers
// used to create a rwlayer.
const maxLayerDepth = 125

type layerStore struct {
	store       *fileMetadataStore
	driver      graphdriver.Driver
	useTarSplit bool

	layerMap map[ChainID]*roLayer
	layerL   sync.Mutex

	mounts map[string]*mountedLayer
	mountL sync.Mutex

	// protect *RWLayer() methods from operating on the same name/id
	locker *locker.Locker

	os string
}

// StoreOptions are the options used to create a new Store instance
type StoreOptions struct {
	Root                      string
	MetadataStorePathTemplate string
	GraphDriver               string
	GraphDriverOptions        []string
	IDMapping                 *idtools.IdentityMapping
	PluginGetter              plugingetter.PluginGetter
	ExperimentalEnabled       bool
	OS                        string
}

// NewStoreFromOptions creates a new Store instance
func NewStoreFromOptions(options StoreOptions) (Store, error) {
	driver, err := graphdriver.New(options.GraphDriver, options.PluginGetter, graphdriver.Options{
		Root:                options.Root,
		DriverOptions:       options.GraphDriverOptions,
		UIDMaps:             options.IDMapping.UIDs(),
		GIDMaps:             options.IDMapping.GIDs(),
		ExperimentalEnabled: options.ExperimentalEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("error initializing graphdriver: %v", err)
	}
	logrus.Debugf("Initialized graph driver %s", driver)

	root := fmt.Sprintf(options.MetadataStorePathTemplate, driver)

	return newStoreFromGraphDriver(root, driver, options.OS)
}

// newStoreFromGraphDriver creates a new Store instance using the provided
// metadata store and graph driver. The metadata store will be used to restore
// the Store.
func newStoreFromGraphDriver(root string, driver graphdriver.Driver, os string) (Store, error) {
	if !system.IsOSSupported(os) {
		return nil, fmt.Errorf("failed to initialize layer store as operating system '%s' is not supported", os)
	}
	caps := graphdriver.Capabilities{}
	if capDriver, ok := driver.(graphdriver.CapabilityDriver); ok {
		caps = capDriver.Capabilities()
	}

	ms, err := newFSMetadataStore(root)
	if err != nil {
		return nil, err
	}

	ls := &layerStore{
		store:       ms,
		driver:      driver,
		layerMap:    map[ChainID]*roLayer{},
		mounts:      map[string]*mountedLayer{},
		locker:      locker.New(),
		useTarSplit: !caps.ReproducesExactDiffs,
		os:          os,
	}

	ids, mounts, err := ms.List()
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		l, err := ls.loadLayer(id)
		if err != nil {
			logrus.Debugf("Failed to load layer %s: %s", id, err)
			continue
		}
		if l.parent != nil {
			l.parent.referenceCount++
		}
	}

	for _, mount := range mounts {
		if err := ls.loadMount(mount); err != nil {
			logrus.Debugf("Failed to load mount %s: %s", mount, err)
		}
	}

	return ls, nil
}

func (ls *layerStore) Driver() graphdriver.Driver {
	return ls.driver
}

func (ls *layerStore) loadLayer(layer ChainID) (*roLayer, error) {
	cl, ok := ls.layerMap[layer]
	if ok {
		return cl, nil
	}

	diff, err := ls.store.GetDiffID(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get diff id for %s: %s", layer, err)
	}

	size, err := ls.store.GetSize(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get size for %s: %s", layer, err)
	}

	cacheID, err := ls.store.GetCacheID(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get cache id for %s: %s", layer, err)
	}

	parent, err := ls.store.GetParent(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get parent for %s: %s", layer, err)
	}

	descriptor, err := ls.store.GetDescriptor(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get descriptor for %s: %s", layer, err)
	}

	os, err := ls.store.getOS(layer)
	if err != nil {
		return nil, fmt.Errorf("failed to get operating system for %s: %s", layer, err)
	}

	if os != ls.os {
		return nil, fmt.Errorf("failed to load layer with os %s into layerstore for %s", os, ls.os)
	}

	cl = &roLayer{
		chainID:    layer,
		diffID:     diff,
		size:       size,
		cacheID:    cacheID,
		layerStore: ls,
		references: map[Layer]struct{}{},
		descriptor: descriptor,
	}

	if parent != "" {
		p, err := ls.loadLayer(parent)
		if err != nil {
			return nil, err
		}
		cl.parent = p
	}

	ls.layerMap[cl.chainID] = cl

	return cl, nil
}

func (ls *layerStore) loadMount(mount string) error {
	ls.mountL.Lock()
	defer ls.mountL.Unlock()
	if _, ok := ls.mounts[mount]; ok {
		return nil
	}

	mountID, err := ls.store.GetMountID(mount)
	if err != nil {
		return err
	}

	initID, err := ls.store.GetInitID(mount)
	if err != nil {
		return err
	}

	parent, err := ls.store.GetMountParent(mount)
	if err != nil {
		return err
	}

	ml := &mountedLayer{
		name:       mount,
		mountID:    mountID,
		initID:     initID,
		layerStore: ls,
		references: map[RWLayer]*referencedRWLayer{},
	}

	if parent != "" {
		p, err := ls.loadLayer(parent)
		if err != nil {
			return err
		}
		ml.parent = p

		p.referenceCount++
	}

	ls.mounts[ml.name] = ml

	return nil
}

func (ls *layerStore) applyTar(tx *fileMetadataTransaction, ts io.Reader, parent string, layer *roLayer) error {
	digester := digest.Canonical.Digester()
	tr := io.TeeReader(ts, digester.Hash())

	rdr := tr
	if ls.useTarSplit {
		tsw, err := tx.TarSplitWriter(true)
		if err != nil {
			return err
		}
		metaPacker := storage.NewJSONPacker(tsw)
		defer tsw.Close()

		// we're passing nil here for the file putter, because the ApplyDiff will
		// handle the extraction of the archive
		rdr, err = asm.NewInputTarStream(tr, metaPacker, nil)
		if err != nil {
			return err
		}
	}

	applySize, err := ls.driver.ApplyDiff(layer.cacheID, parent, rdr)
	// discard trailing data but ensure metadata is picked up to reconstruct stream
	// unconditionally call io.Copy here before checking err to ensure the resources
	// allocated by NewInputTarStream above are always released
	io.Copy(ioutil.Discard, rdr) // ignore error as reader may be closed
	if err != nil {
		return err
	}

	layer.size = applySize
	layer.diffID = DiffID(digester.Digest())

	logrus.Debugf("Applied tar %s to %s, size: %d", layer.diffID, layer.cacheID, applySize)

	return nil
}

func (ls *layerStore) Register(ts io.Reader, parent ChainID) (Layer, error) {
	return ls.registerWithDescriptor(ts, parent, distribution.Descriptor{})
}

func (ls *layerStore) registerWithDescriptor(ts io.Reader, parent ChainID, descriptor distribution.Descriptor) (Layer, error) {
	// err is used to hold the error which will always trigger
	// cleanup of creates sources but may not be an error returned
	// to the caller (already exists).
	var err error
	var pid string
	var p *roLayer

	if string(parent) != "" {
		p = ls.get(parent)
		if p == nil {
			return nil, ErrLayerDoesNotExist
		}
		pid = p.cacheID
		// Release parent chain if error
		defer func() {
			if err != nil {
				ls.layerL.Lock()
				ls.releaseLayer(p)
				ls.layerL.Unlock()
			}
		}()
		if p.depth() >= maxLayerDepth {
			err = ErrMaxDepthExceeded
			return nil, err
		}
	}

	// Create new roLayer
	layer := &roLayer{
		parent:         p,
		cacheID:        stringid.GenerateRandomID(),
		referenceCount: 1,
		layerStore:     ls,
		references:     map[Layer]struct{}{},
		descriptor:     descriptor,
	}

	if err = ls.driver.Create(layer.cacheID, pid, nil); err != nil {
		return nil, err
	}

	tx, err := ls.store.StartTransaction()
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			logrus.Debugf("Cleaning up layer %s: %v", layer.cacheID, err)
			if err := ls.driver.Remove(layer.cacheID); err != nil {
				logrus.Errorf("Error cleaning up cache layer %s: %v", layer.cacheID, err)
			}
			if err := tx.Cancel(); err != nil {
				logrus.Errorf("Error canceling metadata transaction %q: %s", tx.String(), err)
			}
		}
	}()

	if err = ls.applyTar(tx, ts, pid, layer); err != nil {
		return nil, err
	}

	if layer.parent == nil {
		layer.chainID = ChainID(layer.diffID)
	} else {
		layer.chainID = createChainIDFromParent(layer.parent.chainID, layer.diffID)
	}

	if err = storeLayer(tx, layer); err != nil {
		return nil, err
	}

	ls.layerL.Lock()
	defer ls.layerL.Unlock()

	if existingLayer := ls.getWithoutLock(layer.chainID); existingLayer != nil {
		// Set error for cleanup, but do not return the error
		err = errors.New("layer already exists")
		return existingLayer.getReference(), nil
	}

	if err = tx.Commit(layer.chainID); err != nil {
		return nil, err
	}

	ls.layerMap[layer.chainID] = layer

	return layer.getReference(), nil
}

func (ls *layerStore) getWithoutLock(layer ChainID) *roLayer {
	l, ok := ls.layerMap[layer]
	if !ok {
		return nil
	}

	l.referenceCount++

	return l
}

func (ls *layerStore) get(l ChainID) *roLayer {
	ls.layerL.Lock()
	defer ls.layerL.Unlock()
	return ls.getWithoutLock(l)
}

func (ls *layerStore) Get(l ChainID) (Layer, error) {
	ls.layerL.Lock()
	defer ls.layerL.Unlock()

	layer := ls.getWithoutLock(l)
	if layer == nil {
		return nil, ErrLayerDoesNotExist
	}

	return layer.getReference(), nil
}

func (ls *layerStore) Map() map[ChainID]Layer {
	ls.layerL.Lock()
	defer ls.layerL.Unlock()

	layers := map[ChainID]Layer{}

	for k, v := range ls.layerMap {
		layers[k] = v
	}

	return layers
}

func (ls *layerStore) deleteLayer(layer *roLayer, metadata *Metadata) error {
	// Rename layer digest folder first so we detect orphan layer(s)
	// if ls.driver.Remove fails
	dir := ls.store.getLayerDirectory(layer.chainID)
	for {
		dgst := digest.Digest(layer.chainID)
		tmpID := fmt.Sprintf("%s-%s-removing", dgst.Hex(), stringid.GenerateRandomID())
		dir := filepath.Join(ls.store.root, string(dgst.Algorithm()), tmpID)
		err := os.Rename(ls.store.getLayerDirectory(layer.chainID), dir)
		if os.IsExist(err) {
			continue
		}
		break
	}
	err := ls.driver.Remove(layer.cacheID)
	if err != nil {
		return err
	}
	err = os.RemoveAll(dir)
	if err != nil {
		return err
	}
	metadata.DiffID = layer.diffID
	metadata.ChainID = layer.chainID
	metadata.Size, err = layer.Size()
	if err != nil {
		return err
	}
	metadata.DiffSize = layer.size

	return nil
}

func (ls *layerStore) releaseLayer(l *roLayer) ([]Metadata, error) {
	depth := 0
	removed := []Metadata{}
	for {
		if l.referenceCount == 0 {
			panic("layer not retained")
		}
		l.referenceCount--
		if l.referenceCount != 0 {
			return removed, nil
		}

		if len(removed) == 0 && depth > 0 {
			panic("cannot remove layer with child")
		}
		if l.hasReferences() {
			panic("cannot delete referenced layer")
		}
		// Remove layer from layer map first so it is not considered to exist
		// when if ls.deleteLayer fails.
		delete(ls.layerMap, l.chainID)

		var metadata Metadata
		if err := ls.deleteLayer(l, &metadata); err != nil {
			return nil, err
		}
		removed = append(removed, metadata)

		if l.parent == nil {
			return removed, nil
		}

		depth++
		l = l.parent
	}
}

func (ls *layerStore) Release(l Layer) ([]Metadata, error) {
	ls.layerL.Lock()
	defer ls.layerL.Unlock()
	layer, ok := ls.layerMap[l.ChainID()]
	if !ok {
		return []Metadata{}, nil
	}
	if !layer.hasReference(l) {
		return nil, ErrLayerNotRetained
	}

	layer.deleteReference(l)

	return ls.releaseLayer(layer)
}

func (ls *layerStore) CreateRWLayer(name string, parent ChainID, opts *CreateRWLayerOpts) (_ RWLayer, err error) {
	var (
		storageOpt map[string]string
		initFunc   MountInit
		mountLabel string
	)

	if opts != nil {
		mountLabel = opts.MountLabel
		storageOpt = opts.StorageOpt
		initFunc = opts.InitFunc
	}

	ls.locker.Lock(name)
	defer ls.locker.Unlock(name)

	ls.mountL.Lock()
	_, ok := ls.mounts[name]
	ls.mountL.Unlock()
	if ok {
		return nil, ErrMountNameConflict
	}

	var pid string
	var p *roLayer
	if string(parent) != "" {
		p = ls.get(parent)
		if p == nil {
			return nil, ErrLayerDoesNotExist
		}
		pid = p.cacheID

		// Release parent chain if error
		defer func() {
			if err != nil {
				ls.layerL.Lock()
				ls.releaseLayer(p)
				ls.layerL.Unlock()
			}
		}()
	}

	m := &mountedLayer{
		name:       name,
		parent:     p,
		mountID:    ls.mountID(name),
		layerStore: ls,
		references: map[RWLayer]*referencedRWLayer{},
	}

	if initFunc != nil {
		pid, err = ls.initMount(m.mountID, pid, mountLabel, initFunc, storageOpt)
		if err != nil {
			return
		}
		m.initID = pid
	}

	createOpts := &graphdriver.CreateOpts{
		StorageOpt: storageOpt,
	}

	if err = ls.driver.CreateReadWrite(m.mountID, pid, createOpts); err != nil {
		return
	}
	if err = ls.saveMount(m); err != nil {
		return
	}

	return m.getReference(), nil
}

func (ls *layerStore) GetRWLayer(id string) (RWLayer, error) {
	ls.locker.Lock(id)
	defer ls.locker.Unlock(id)

	ls.mountL.Lock()
	mount := ls.mounts[id]
	ls.mountL.Unlock()
	if mount == nil {
		return nil, ErrMountDoesNotExist
	}

	return mount.getReference(), nil
}

func (ls *layerStore) GetMountID(id string) (string, error) {
	ls.mountL.Lock()
	mount := ls.mounts[id]
	ls.mountL.Unlock()

	if mount == nil {
		return "", ErrMountDoesNotExist
	}
	logrus.Debugf("GetMountID id: %s -> mountID: %s", id, mount.mountID)

	return mount.mountID, nil
}

func (ls *layerStore) ReleaseRWLayer(l RWLayer) ([]Metadata, error) {
	name := l.Name()
	ls.locker.Lock(name)
	defer ls.locker.Unlock(name)

	ls.mountL.Lock()
	m := ls.mounts[name]
	ls.mountL.Unlock()
	if m == nil {
		return []Metadata{}, nil
	}

	if err := m.deleteReference(l); err != nil {
		return nil, err
	}

	if m.hasReferences() {
		return []Metadata{}, nil
	}

	if err := ls.driver.Remove(m.mountID); err != nil {
		logrus.Errorf("Error removing mounted layer %s: %s", m.name, err)
		m.retakeReference(l)
		return nil, err
	}

	if m.initID != "" {
		if err := ls.driver.Remove(m.initID); err != nil {
			logrus.Errorf("Error removing init layer %s: %s", m.name, err)
			m.retakeReference(l)
			return nil, err
		}
	}

	if err := ls.store.RemoveMount(m.name); err != nil {
		logrus.Errorf("Error removing mount metadata: %s: %s", m.name, err)
		m.retakeReference(l)
		return nil, err
	}

	ls.mountL.Lock()
	delete(ls.mounts, name)
	ls.mountL.Unlock()

	ls.layerL.Lock()
	defer ls.layerL.Unlock()
	if m.parent != nil {
		return ls.releaseLayer(m.parent)
	}

	return []Metadata{}, nil
}

func (ls *layerStore) saveMount(mount *mountedLayer) error {
	if err := ls.store.SetMountID(mount.name, mount.mountID); err != nil {
		return err
	}

	if mount.initID != "" {
		if err := ls.store.SetInitID(mount.name, mount.initID); err != nil {
			return err
		}
	}

	if mount.parent != nil {
		if err := ls.store.SetMountParent(mount.name, mount.parent.chainID); err != nil {
			return err
		}
	}

	ls.mountL.Lock()
	ls.mounts[mount.name] = mount
	ls.mountL.Unlock()

	return nil
}

func (ls *layerStore) initMount(graphID, parent, mountLabel string, initFunc MountInit, storageOpt map[string]string) (string, error) {
	// Use "<graph-id>-init" to maintain compatibility with graph drivers
	// which are expecting this layer with this special name. If all
	// graph drivers can be updated to not rely on knowing about this layer
	// then the initID should be randomly generated.
	initID := fmt.Sprintf("%s-init", graphID)

	createOpts := &graphdriver.CreateOpts{
		MountLabel: mountLabel,
		StorageOpt: storageOpt,
	}

	if err := ls.driver.CreateReadWrite(initID, parent, createOpts); err != nil {
		return "", err
	}
	p, err := ls.driver.Get(initID, "")
	if err != nil {
		return "", err
	}

	if err := initFunc(p); err != nil {
		ls.driver.Put(initID)
		return "", err
	}

	if err := ls.driver.Put(initID); err != nil {
		return "", err
	}

	return initID, nil
}

func (ls *layerStore) getTarStream(rl *roLayer) (io.ReadCloser, error) {
	if !ls.useTarSplit {
		var parentCacheID string
		if rl.parent != nil {
			parentCacheID = rl.parent.cacheID
		}

		return ls.driver.Diff(rl.cacheID, parentCacheID)
	}

	r, err := ls.store.TarSplitReader(rl.chainID)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		err := ls.assembleTarTo(rl.cacheID, r, nil, pw)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	return pr, nil
}

func (ls *layerStore) assembleTarTo(graphID string, metadata io.ReadCloser, size *int64, w io.Writer) error {
	diffDriver, ok := ls.driver.(graphdriver.DiffGetterDriver)
	if !ok {
		diffDriver = &naiveDiffPathDriver{ls.driver}
	}

	defer metadata.Close()

	// get our relative path to the container
	fileGetCloser, err := diffDriver.DiffGetter(graphID)
	if err != nil {
		return err
	}
	defer fileGetCloser.Close()

	metaUnpacker := storage.NewJSONUnpacker(metadata)
	upackerCounter := &unpackSizeCounter{metaUnpacker, size}
	logrus.Debugf("Assembling tar data for %s", graphID)
	return asm.WriteOutputTarStream(fileGetCloser, upackerCounter, w)
}

func (ls *layerStore) Cleanup() error {
	orphanLayers, err := ls.store.getOrphan()
	if err != nil {
		logrus.Errorf("Cannot get orphan layers: %v", err)
	}
	logrus.Debugf("found %v orphan layers", len(orphanLayers))
	for _, orphan := range orphanLayers {
		logrus.Debugf("removing orphan layer, chain ID: %v , cache ID: %v", orphan.chainID, orphan.cacheID)
		err = ls.driver.Remove(orphan.cacheID)
		if err != nil && !os.IsNotExist(err) {
			logrus.WithError(err).WithField("cache-id", orphan.cacheID).Error("cannot remove orphan layer")
			continue
		}
		err = ls.store.Remove(orphan.chainID, orphan.cacheID)
		if err != nil {
			logrus.WithError(err).WithField("chain-id", orphan.chainID).Error("cannot remove orphan layer metadata")
		}
	}
	return ls.driver.Cleanup()
}

func (ls *layerStore) DriverStatus() [][2]string {
	return ls.driver.Status()
}

func (ls *layerStore) DriverName() string {
	return ls.driver.String()
}

type naiveDiffPathDriver struct {
	graphdriver.Driver
}

type fileGetPutter struct {
	storage.FileGetter
	driver graphdriver.Driver
	id     string
}

func (w *fileGetPutter) Close() error {
	return w.driver.Put(w.id)
}

func (n *naiveDiffPathDriver) DiffGetter(id string) (graphdriver.FileGetCloser, error) {
	p, err := n.Driver.Get(id, "")
	if err != nil {
		return nil, err
	}
	return &fileGetPutter{storage.NewPathFileGetter(p.Path()), n.Driver, id}, nil
}

package images

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	sharedUsageBaseLayerSize   int64 = 128
	sharedUsageFirstImageSize  int64 = 256
	sharedUsageSecondImageSize int64 = 512

	sharedUsageBaseLayerContent   = "shared-usage-base-layer"
	sharedUsageFirstLayerContent  = "shared-usage-first-layer"
	sharedUsageSecondLayerContent = "shared-usage-second-layer"
)

func TestImagesSharedSizePopulatedForSharedLayers(t *testing.T) {
	baseDiffID := digest.FromString(sharedUsageBaseLayerContent)
	firstDiffID := digest.FromString(sharedUsageFirstLayerContent)
	secondDiffID := digest.FromString(sharedUsageSecondLayerContent)

	baseChainID := chainID(baseDiffID)
	firstChainID := chainID(baseDiffID, firstDiffID)
	secondChainID := chainID(baseDiffID, secondDiffID)

	imageStore := &sharedUsageImageStore{
		images: map[image.ID]*image.Image{
			image.ID(digest.FromString("shared-usage-first-image")):  imageWithRootFS(baseDiffID, firstDiffID),
			image.ID(digest.FromString("shared-usage-second-image")): imageWithRootFS(baseDiffID, secondDiffID),
		},
	}
	layerStore := &sharedUsageLayerStore{
		layers: map[layer.ChainID]*sharedUsageLayer{
			baseChainID: {
				chainID:  baseChainID,
				diffID:   baseDiffID,
				size:     sharedUsageBaseLayerSize,
				diffSize: sharedUsageBaseLayerSize,
			},
			firstChainID: {
				chainID:  firstChainID,
				diffID:   firstDiffID,
				size:     sharedUsageFirstImageSize,
				diffSize: sharedUsageFirstImageSize - sharedUsageBaseLayerSize,
			},
			secondChainID: {
				chainID:  secondChainID,
				diffID:   secondDiffID,
				size:     sharedUsageSecondImageSize,
				diffSize: sharedUsageSecondImageSize - sharedUsageBaseLayerSize,
			},
		},
	}
	service := &ImageService{
		containers:     sharedUsageContainerStore{},
		imageStore:     imageStore,
		layerStore:     layerStore,
		referenceStore: sharedUsageReferenceStore{},
	}

	summaries, err := service.Images(context.Background(), imagebackend.ListOptions{
		All:        true,
		SharedSize: true,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Len(summaries, len(imageStore.images)))

	for _, summary := range summaries {
		assert.Check(t, is.Equal(summary.SharedSize, sharedUsageBaseLayerSize))
		assert.Check(t, summary.Size > summary.SharedSize, "expected image size to include a unique layer: size=%d shared=%d", summary.Size, summary.SharedSize)
	}
}

func imageWithRootFS(diffIDs ...layer.DiffID) *image.Image {
	rootFS := image.NewRootFS()
	for _, diffID := range diffIDs {
		rootFS.Append(diffID)
	}
	return &image.Image{
		RootFS: rootFS,
	}
}

func chainID(diffIDs ...layer.DiffID) layer.ChainID {
	rootFS := image.NewRootFS()
	for _, diffID := range diffIDs {
		rootFS.Append(diffID)
	}
	return rootFS.ChainID()
}

type sharedUsageImageStore struct {
	images map[image.ID]*image.Image
}

func (s *sharedUsageImageStore) Create([]byte) (image.ID, error) {
	return "", errors.New("unexpected sharedUsageImageStore.Create call")
}

func (s *sharedUsageImageStore) Get(id image.ID) (*image.Image, error) {
	img, ok := s.images[id]
	if !ok {
		return nil, errors.New("image not found")
	}
	return img, nil
}

func (s *sharedUsageImageStore) Delete(image.ID) ([]layer.Metadata, error) {
	return nil, errors.New("unexpected sharedUsageImageStore.Delete call")
}

func (s *sharedUsageImageStore) Search(string) (image.ID, error) {
	return "", errors.New("unexpected sharedUsageImageStore.Search call")
}

func (s *sharedUsageImageStore) SetParent(image.ID, image.ID) error {
	return errors.New("unexpected sharedUsageImageStore.SetParent call")
}

func (s *sharedUsageImageStore) GetParent(image.ID) (image.ID, error) {
	return "", errors.New("unexpected sharedUsageImageStore.GetParent call")
}

func (s *sharedUsageImageStore) SetLastUpdated(image.ID) error {
	return errors.New("unexpected sharedUsageImageStore.SetLastUpdated call")
}

func (s *sharedUsageImageStore) GetLastUpdated(image.ID) (time.Time, error) {
	return time.Time{}, errors.New("unexpected sharedUsageImageStore.GetLastUpdated call")
}

func (s *sharedUsageImageStore) SetBuiltLocally(image.ID) error {
	return errors.New("unexpected sharedUsageImageStore.SetBuiltLocally call")
}

func (s *sharedUsageImageStore) IsBuiltLocally(image.ID) (bool, error) {
	return false, errors.New("unexpected sharedUsageImageStore.IsBuiltLocally call")
}

func (s *sharedUsageImageStore) Children(image.ID) []image.ID {
	return nil
}

func (s *sharedUsageImageStore) Map() map[image.ID]*image.Image {
	return s.images
}

func (s *sharedUsageImageStore) Heads() map[image.ID]*image.Image {
	return s.images
}

func (s *sharedUsageImageStore) Len() int {
	return len(s.images)
}

type sharedUsageLayerStore struct {
	layers map[layer.ChainID]*sharedUsageLayer
}

func (s *sharedUsageLayerStore) Register(io.Reader, layer.ChainID) (layer.Layer, error) {
	return nil, errors.New("unexpected sharedUsageLayerStore.Register call")
}

func (s *sharedUsageLayerStore) Get(chainID layer.ChainID) (layer.Layer, error) {
	l, ok := s.layers[chainID]
	if !ok {
		return nil, layer.ErrLayerDoesNotExist
	}
	return l, nil
}

func (s *sharedUsageLayerStore) Map() map[layer.ChainID]layer.Layer {
	layers := make(map[layer.ChainID]layer.Layer, len(s.layers))
	for chainID, layer := range s.layers {
		layers[chainID] = layer
	}
	return layers
}

func (s *sharedUsageLayerStore) Release(layer.Layer) ([]layer.Metadata, error) {
	return nil, nil
}

func (s *sharedUsageLayerStore) CreateRWLayer(string, layer.ChainID, *layer.CreateRWLayerOpts) (layer.RWLayer, error) {
	return nil, errors.New("unexpected sharedUsageLayerStore.CreateRWLayer call")
}

func (s *sharedUsageLayerStore) GetRWLayer(string) (layer.RWLayer, error) {
	return nil, errors.New("unexpected sharedUsageLayerStore.GetRWLayer call")
}

func (s *sharedUsageLayerStore) GetMountID(string) (string, error) {
	return "", errors.New("unexpected sharedUsageLayerStore.GetMountID call")
}

func (s *sharedUsageLayerStore) ReleaseRWLayer(layer.RWLayer) ([]layer.Metadata, error) {
	return nil, errors.New("unexpected sharedUsageLayerStore.ReleaseRWLayer call")
}

func (s *sharedUsageLayerStore) Cleanup() error {
	return nil
}

func (s *sharedUsageLayerStore) DriverStatus() [][2]string {
	return nil
}

func (s *sharedUsageLayerStore) DriverName() string {
	return ""
}

type sharedUsageLayer struct {
	chainID  layer.ChainID
	diffID   layer.DiffID
	size     int64
	diffSize int64
}

func (l *sharedUsageLayer) TarStream() (io.ReadCloser, error) {
	return nil, errors.New("unexpected sharedUsageLayer.TarStream call")
}

func (l *sharedUsageLayer) TarStreamFrom(layer.ChainID) (io.ReadCloser, error) {
	return nil, errors.New("unexpected sharedUsageLayer.TarStreamFrom call")
}

func (l *sharedUsageLayer) ChainID() layer.ChainID {
	return l.chainID
}

func (l *sharedUsageLayer) DiffID() layer.DiffID {
	return l.diffID
}

func (l *sharedUsageLayer) Parent() layer.Layer {
	return nil
}

func (l *sharedUsageLayer) Size() int64 {
	return l.size
}

func (l *sharedUsageLayer) DiffSize() int64 {
	return l.diffSize
}

func (l *sharedUsageLayer) Metadata() (map[string]string, error) {
	return nil, errors.New("unexpected sharedUsageLayer.Metadata call")
}

type sharedUsageReferenceStore struct{}

func (sharedUsageReferenceStore) References(digest.Digest) []reference.Named {
	return nil
}

func (sharedUsageReferenceStore) ReferencesByName(reference.Named) []refstore.Association {
	return nil
}

func (sharedUsageReferenceStore) AddTag(reference.Named, digest.Digest, bool) error {
	return errors.New("unexpected sharedUsageReferenceStore.AddTag call")
}

func (sharedUsageReferenceStore) AddDigest(reference.Canonical, digest.Digest, bool) error {
	return errors.New("unexpected sharedUsageReferenceStore.AddDigest call")
}

func (sharedUsageReferenceStore) Delete(reference.Named) (bool, error) {
	return false, errors.New("unexpected sharedUsageReferenceStore.Delete call")
}

func (sharedUsageReferenceStore) Get(reference.Named) (digest.Digest, error) {
	return "", errors.New("unexpected sharedUsageReferenceStore.Get call")
}

type sharedUsageContainerStore struct{}

func (sharedUsageContainerStore) First(container.StoreFilter) *container.Container {
	return nil
}

func (sharedUsageContainerStore) List() []*container.Container {
	return nil
}

func (sharedUsageContainerStore) Get(string) *container.Container {
	return nil
}

var _ image.Store = (*sharedUsageImageStore)(nil)
var _ layer.Store = (*sharedUsageLayerStore)(nil)
var _ layer.Layer = (*sharedUsageLayer)(nil)
var _ refstore.Store = sharedUsageReferenceStore{}
var _ containerStore = sharedUsageContainerStore{}

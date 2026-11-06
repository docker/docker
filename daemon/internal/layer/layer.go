// Package layer is package for managing read-only
// and read-write mounts on the union file system
// driver. Read-only mounts are referenced using a
// content hash and are protected from mutation in
// the exposed interface. The tar format is used
// to create read-only layers and export both
// read-only and writable layers. The exported
// tar data for a read-only layer should match
// the tar used to create the layer.
package layer

import (
	"context"
	"errors"
	"io"

	"github.com/containerd/log"
	"github.com/docker/distribution"
	"github.com/moby/go-archive"
	"github.com/opencontainers/go-digest"
)

var (
	// ErrLayerDoesNotExist is used when an operation is
	// attempted on a layer which does not exist.
	ErrLayerDoesNotExist = errors.New("layer does not exist")

	// ErrLayerNotRetained is used when a release is
	// attempted on a layer which is not retained.
	ErrLayerNotRetained = errors.New("layer not retained")

	// ErrMountDoesNotExist is used when an operation is
	// attempted on a mount layer which does not exist.
	ErrMountDoesNotExist = errors.New("mount does not exist")

	// ErrMountNameConflict is used when a mount is attempted
	// to be created but there is already a mount with the name
	// used for creation.
	ErrMountNameConflict = errors.New("mount already exists with name")

	// ErrMaxDepthExceeded is used when a layer is attempted
	// to be created which would result in a layer depth
	// greater than the 125 max.
	ErrMaxDepthExceeded = errors.New("max depth exceeded")
)

// ChainID is the content-addressable ID of a layer.
type ChainID = digest.Digest

// DiffID is the hash of an individual layer tar.
type DiffID = digest.Digest

// TarStreamer represents an object which may
// have its contents exported as a tar stream.
type TarStreamer interface {
	// TarStream returns a tar archive stream
	// for the contents of a layer.
	TarStream() (io.ReadCloser, error)
}

// Layer represents a read-only layer
type Layer interface {
	TarStreamer

	// TarStreamFrom returns a tar archive stream for all the layer chain with
	// arbitrary depth.
	TarStreamFrom(ChainID) (io.ReadCloser, error)

	// ChainID returns the content hash of the entire layer chain. The hash
	// chain is made up of DiffID of top layer and all of its parents.
	ChainID() ChainID

	// DiffID returns the content hash of the layer
	// tar stream used to create this layer.
	DiffID() DiffID

	// Parent returns the next layer in the layer chain.
	Parent() Layer

	// Size returns the size of the entire layer chain. The size
	// is calculated from the total size of all files in the layers.
	Size() int64

	// DiffSize returns the size difference of the top layer
	// from parent layer.
	DiffSize() int64

	// Metadata returns the low level storage metadata associated
	// with layer.
	Metadata() (map[string]string, error)
}

// RWLayer represents a layer which is
// read and writable
type RWLayer interface {
	TarStreamer

	// Name of mounted layer
	Name() string

	// Parent returns the layer which the writable
	// layer was created from.
	Parent() Layer

	// Mount mounts the RWLayer and returns the filesystem path
	// to the writable layer.
	Mount(mountLabel string) (string, error)

	// Unmount unmounts the RWLayer. This should be called
	// for every mount. If there are multiple mount calls
	// this operation will only decrement the internal mount counter.
	Unmount() error

	// Size represents the size of the writable layer
	// as calculated by the total size of the files
	// changed in the mutable layer.
	Size() (int64, error)

	// SizeWithContext represents the size of the writable layer
	// respecting context cancellation.
	SizeWithContext(ctx context.Context) (int64, error)

	// Changes returns the set of changes for the mutable layer
	// from the base layer.
	Changes() ([]archive.Change, error)

	// Metadata returns the low level metadata for the mutable layer
	Metadata() (map[string]string, error)

	// ApplyDiff applies the diff to the RW layer
	ApplyDiff(diff io.Reader) (int64, error)
}

// Metadata holds information about a
// read-only layer
type Metadata struct {
	// ChainID is the content hash of the layer
	ChainID ChainID

	// DiffID is the hash of the tar data used to
	// create the layer
	DiffID DiffID

	// Size is the size of the layer and all parents
	Size int64

	// DiffSize is the size of the top layer
	DiffSize int64
}

// MountInit is a function to initialize a
// writable mount. Changes made here will
// not be included in the Tar stream of the
// RWLayer.
type MountInit func(root string) error

// CreateRWLayerOpts contains optional arguments to be passed to CreateRWLayer
type CreateRWLayerOpts struct {
	MountLabel string
	InitFunc   MountInit
	StorageOpt map[string]string
}

// Store represents a backend for managing both
// read-only and read-write layers.
type Store interface {
	Register(io.Reader, ChainID) (Layer, error)
	Get(ChainID) (Layer, error)
	Map() map[ChainID]Layer
	Release(Layer) ([]Metadata, error)
	CreateRWLayer(id string, parent ChainID, opts *CreateRWLayerOpts) (RWLayer, error)
	GetRWLayer(id string) (RWLayer, error)
	GetMountID(id string) (string, error)
	ReleaseRWLayer(RWLayer) ([]Metadata, error)
	Cleanup() error
	DriverStatus() [][2]string
	DriverName() string
}

// DescribableStore represents a layer store capable of storing
// descriptors for layers.
type DescribableStore interface {
	RegisterWithDescriptor(io.Reader, ChainID, distribution.Descriptor) (Layer, error)
}

// ReleaseAndLog releases the provided layer from the given layer
// store, logging any error and release metadata
func ReleaseAndLog(ls Store, l Layer) {
	metadata, err := ls.Release(l)
	if err != nil {
		log.G(context.TODO()).Errorf("Error releasing layer %s: %v", l.ChainID(), err)
	}
	for _, m := range metadata {
		log.G(context.TODO()).WithField("chainID", m.ChainID).Infof("Cleaned up layer %s", m.ChainID)
	}
}

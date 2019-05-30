package solver

import (
	"context"
	"time"

	"github.com/containerd/containerd/content"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Vertex is one node in the build graph
type Vertex interface {
	// Digest is a content-addressable vertex identifier
	Digest() digest.Digest
	// Sys returns an internal value that is used to execute the vertex. Usually
	// this is capured by the operation resolver method during solve.
	Sys() interface{}
	Options() VertexOptions
	// Array of edges current vertex depends on.
	Inputs() []Edge
	Name() string
}

// Index is a index value for output edge
type Index int

// Edge is a path to a specific output of the vertex
type Edge struct {
	Index  Index
	Vertex Vertex
}

// VertexOptions has optional metadata for the vertex that is not contained in digest
type VertexOptions struct {
	IgnoreCache  bool
	CacheSources []CacheManager
	Description  map[string]string // text values with no special meaning for solver
	ExportCache  *bool
	// WorkerConstraint
}

// Result is an abstract return value for a solve
type Result interface {
	ID() string
	Release(context.Context) error
	Sys() interface{}
}

// CachedResult is a result connected with its cache key
type CachedResult interface {
	Result
	CacheKeys() []ExportableCacheKey
}

// CacheExportMode is the type for setting cache exporting modes
type CacheExportMode int

const (
	// CacheExportModeMin exports a topmost allowed vertex and its dependencies
	// that already have transferable layers
	CacheExportModeMin CacheExportMode = iota
	// CacheExportModeMax exports all possible non-root vertexes
	CacheExportModeMax
	// CacheExportModeRemoteOnly only exports vertexes that already have
	// transferable layers
	CacheExportModeRemoteOnly
)

// CacheExportOpt defines options for exporting build cache
type CacheExportOpt struct {
	// Convert can convert a build result to transferable object
	Convert func(context.Context, Result) (*Remote, error)
	// Mode defines a cache export algorithm
	Mode CacheExportMode
}

// CacheExporter can export the artifacts of the build chain
type CacheExporter interface {
	ExportTo(ctx context.Context, t CacheExporterTarget, opt CacheExportOpt) ([]CacheExporterRecord, error)
}

// CacheExporterTarget defines object capable of receiving exports
type CacheExporterTarget interface {
	Add(dgst digest.Digest) CacheExporterRecord
	Visit(interface{})
	Visited(interface{}) bool
}

// CacheExporterRecord is a single object being exported
type CacheExporterRecord interface {
	AddResult(createdAt time.Time, result *Remote)
	LinkFrom(src CacheExporterRecord, index int, selector string)
}

// Remote is a descriptor or a list of stacked descriptors that can be pulled
// from a content provider
// TODO: add closer to keep referenced data from getting deleted
type Remote struct {
	Descriptors []ocispec.Descriptor
	Provider    content.Provider
}

// CacheLink is a link between two cache records
type CacheLink struct {
	Source   digest.Digest `json:",omitempty"`
	Input    Index         `json:",omitempty"`
	Output   Index         `json:",omitempty"`
	Base     digest.Digest `json:",omitempty"`
	Selector digest.Digest `json:",omitempty"`
}

// Op is an implementation for running a vertex
type Op interface {
	// CacheMap returns structure describing how the operation is cached.
	// Currently only roots are allowed to return multiple cache maps per op.
	CacheMap(context.Context, int) (*CacheMap, bool, error)
	// Exec runs an operation given results from previous operations.
	Exec(ctx context.Context, inputs []Result) (outputs []Result, err error)
}

type ResultBasedCacheFunc func(context.Context, Result) (digest.Digest, error)

type CacheMap struct {
	// Digest is a base digest for operation that needs to be combined with
	// inputs cache or selectors for dependencies.
	Digest digest.Digest
	Deps   []struct {
		// Optional digest that is merged with the cache key of the input
		Selector digest.Digest
		// Optional function that returns a digest for the input based on its
		// return value
		ComputeDigestFunc ResultBasedCacheFunc
	}
}

// ExportableCacheKey is a cache key connected with an exporter that can export
// a chain of cacherecords pointing to that key
type ExportableCacheKey struct {
	*CacheKey
	Exporter CacheExporter
}

// CacheRecord is an identifier for loading in cache
type CacheRecord struct {
	ID        string
	Size      int
	CreatedAt time.Time
	Priority  int

	cacheManager *cacheManager
	key          *CacheKey
}

// CacheManager implements build cache backend
type CacheManager interface {
	// ID is used to identify cache providers that are backed by same source
	// to avoid duplicate calls to the same provider
	ID() string
	// Query searches for cache paths from one cache key to the output of a
	// possible match.
	Query(inp []CacheKeyWithSelector, inputIndex Index, dgst digest.Digest, outputIndex Index) ([]*CacheKey, error)
	Records(ck *CacheKey) ([]*CacheRecord, error)
	// Load pulls and returns the cached result
	Load(ctx context.Context, rec *CacheRecord) (Result, error)
	// Save saves a result based on a cache key
	Save(key *CacheKey, s Result, createdAt time.Time) (*ExportableCacheKey, error)
}

package buildbackend

import (
	"io"

	"github.com/moby/moby/api/types/prune"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/filters"
)

type DiskUsageOptions struct {
	Verbose bool
}

// DiskUsage contains disk usage for the build cache.
type DiskUsage = build.DiskUsage

type CachePruneOptions struct {
	Progress      func(prune.Object)
	All           bool
	ReservedSpace int64
	MaxUsedSpace  int64
	MinFreeSpace  int64
	Filters       filters.Args
}

// PullOption defines different modes for accessing images
type PullOption int

const (
	// PullOptionNoPull only returns local images
	PullOptionNoPull PullOption = iota
	// PullOptionForcePull always tries to pull a ref from the registry first
	PullOptionForcePull
	// PullOptionPreferLocal uses local image if it exists, otherwise pulls
	PullOptionPreferLocal
)

// ProgressWriter is a data object to transport progress streams to the client
type ProgressWriter struct {
	Output             io.Writer
	StdoutFormatter    io.Writer
	StderrFormatter    io.Writer
	AuxFormatter       AuxEmitter
	ProgressReaderFunc func(io.ReadCloser) io.ReadCloser
}

// AuxEmitter is an interface for emitting aux messages during build progress
type AuxEmitter interface {
	Emit(string, any) error
}

// BuildConfig is the configuration used by a BuildManager to start a build
type BuildConfig struct {
	Source         io.ReadCloser
	ProgressWriter ProgressWriter
	Options        *BuildOptions
}

// BuildOptions holds the information
// necessary to build images.
type BuildOptions struct {
	Tags           []string
	SuppressOutput bool
	RemoteContext  string
	NoCache        bool
	Remove         bool
	ForceRemove    bool
	PullParent     bool
	Isolation      container.Isolation
	CPUSetCPUs     string
	CPUSetMems     string
	CPUShares      int64
	CPUQuota       int64
	CPUPeriod      int64
	Memory         int64
	MemorySwap     int64
	CgroupParent   string
	NetworkMode    string
	ShmSize        int64
	Dockerfile     string
	Ulimits        []*container.Ulimit
	// BuildArgs needs to be a *string instead of just a string so that
	// we can tell the difference between "" (empty string) and no value
	// at all (nil). See the parsing of buildArgs in
	// api/server/router/build/build_routes.go for even more info.
	BuildArgs   map[string]*string
	AuthConfigs map[string]registry.AuthConfig
	Context     io.Reader
	Labels      map[string]string
	// squash the resulting image's layers to the parent
	// preserves the original image and creates a new one from the parent with all
	// the changes applied to a single layer
	Squash bool
	// CacheFrom specifies images that are used for matching cache. Images
	// specified here do not need to have a valid parent chain to match cache.
	CacheFrom   []string
	SecurityOpt []string
	ExtraHosts  []string // List of extra hosts
	Target      string
	SessionID   string
	Platform    string
	// Version specifies the version of the underlying builder to use
	Version build.BuilderVersion
	// BuildID is an optional identifier that can be passed together with the
	// build request. The same identifier can be used to gracefully cancel the
	// build with the cancel request.
	BuildID string
	// Outputs defines configurations for exporting build results. Only supported
	// in BuildKit mode
	Outputs []BuildOutput
}

// BuildOutput defines configuration for exporting a build result
type BuildOutput struct {
	Type  string
	Attrs map[string]string
}

// GetImageAndLayerOptions are the options supported by GetImageAndReleasableLayer
type GetImageAndLayerOptions struct {
	PullOption PullOption
	AuthConfig map[string]registry.AuthConfig
	Output     io.Writer
	Platform   *ocispec.Platform
}

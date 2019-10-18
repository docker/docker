package image // import "github.com/docker/docker/api/server/router/image"

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Backend is all the methods that need to be implemented
// to provide image specific functionality.
type Backend interface {
	imageBackend
	importExportBackend
	registryBackend
}

type imageBackend interface {
	ImageDelete(ctx context.Context, imageRef string, force, prune bool) ([]types.ImageDeleteResponseItem, error)
	ImageHistory(ctx context.Context, imageName string) ([]*image.HistoryResponseItem, error)
	Images(ctx context.Context, imageFilters filters.Args, all bool, withExtraAttrs bool) ([]*types.ImageSummary, error)
	LookupImage(ctx context.Context, name string) (*types.ImageInspect, error)
	TagImage(ctx context.Context, imageName, repository, tag string) (string, error)
	ImagesPrune(ctx context.Context, pruneFilters filters.Args) (*types.ImagesPruneReport, error)
}

type importExportBackend interface {
	LoadImage(ctx context.Context, inTar io.ReadCloser, outStream io.Writer, quiet bool) error
	ImportImage(ctx context.Context, src string, repository, platform string, tag string, msg string, inConfig io.ReadCloser, outStream io.Writer, changes []string) error
	ExportImage(ctx context.Context, names []string, outStream io.Writer) error
}

type registryBackend interface {
	PullImage(ctx context.Context, image, tag string, platform *specs.Platform, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error
	PushImage(ctx context.Context, image, tag string, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error
	SearchRegistryForImages(ctx context.Context, filtersArgs string, term string, limit int, authConfig *types.AuthConfig, metaHeaders map[string][]string) (*registry.SearchResults, error)
}

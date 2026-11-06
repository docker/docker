package image

import (
	"context"
	"io"

	"github.com/moby/moby/v2/daemon/internal/pruneprogress"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/filters"
	dockerimage "github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Backend is all the methods that need to be implemented
// to provide image specific functionality.
type Backend interface {
	imageBackend
	importExportBackend
	registryBackend
}

type imageBackend interface {
	ImageDelete(ctx context.Context, imageRef string, options imagebackend.RemoveOptions) ([]image.DeleteResponse, error)
	ImageHistory(ctx context.Context, imageName string, platform *ocispec.Platform) ([]*image.HistoryResponseItem, error)
	Images(ctx context.Context, opts imagebackend.ListOptions) ([]image.Summary, error)
	GetImage(ctx context.Context, refOrID string, options imagebackend.GetImageOpts) (*dockerimage.Image, error)
	ImageInspect(ctx context.Context, refOrID string, options imagebackend.ImageInspectOpts) (*imagebackend.InspectData, error)
	ImageAttestations(ctx context.Context, refOrID string, opts imagebackend.AttestationOpts) ([]image.AttestationStatement, error)
	TagImage(ctx context.Context, id dockerimage.ID, newRef reference.Named) error
	ImagePrune(ctx context.Context, opts pruneprogress.Options) (*image.PruneReport, error)
}

type importExportBackend interface {
	LoadImage(ctx context.Context, inTar io.ReadCloser, platformList []ocispec.Platform, outStream io.Writer, quiet bool) error
	ImportImage(ctx context.Context, ref reference.Named, platform *ocispec.Platform, msg string, layerReader io.Reader, changes []string) (dockerimage.ID, error)
	ExportImage(ctx context.Context, names []string, platformList []ocispec.Platform, outStream io.Writer) error
}

type registryBackend interface {
	PullImage(ctx context.Context, ref reference.Named, options imagebackend.PullOptions) error
	PushImage(ctx context.Context, ref reference.Named, options imagebackend.PushOptions) error
}

type Searcher interface {
	Search(ctx context.Context, searchFilters filters.Args, term string, limit int, authConfig *registry.AuthConfig, headers map[string][]string) ([]registry.SearchResult, error)
}

package content

import (
	"context"

	api "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/services/content/contentserver"
	"github.com/moby/buildkit/session"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GRPCHeaderID is a gRPC header for store ID
const GRPCHeaderID = "buildkit-attachable-store-id"

type attachableContentStore struct {
	stores map[string]content.Store
}

func (cs *attachableContentStore) choose(ctx context.Context) (content.Store, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "request lacks metadata")
	}

	values := md[GRPCHeaderID]
	if len(values) == 0 {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "request lacks metadata %q", GRPCHeaderID)
	}
	id := values[0]
	store, ok := cs.stores[id]
	if !ok {
		return nil, errors.Wrapf(errdefs.ErrNotFound, "unknown store %s", id)
	}
	return store, nil
}

func (cs *attachableContentStore) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return content.Info{}, err
	}
	return store.Info(ctx, dgst)
}

func (cs *attachableContentStore) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return content.Info{}, err
	}
	return store.Update(ctx, info, fieldpaths...)
}

func (cs *attachableContentStore) Walk(ctx context.Context, fn content.WalkFunc, fs ...string) error {
	store, err := cs.choose(ctx)
	if err != nil {
		return err
	}
	return store.Walk(ctx, fn, fs...)
}

func (cs *attachableContentStore) Delete(ctx context.Context, dgst digest.Digest) error {
	store, err := cs.choose(ctx)
	if err != nil {
		return err
	}
	return store.Delete(ctx, dgst)
}

func (cs *attachableContentStore) ListStatuses(ctx context.Context, fs ...string) ([]content.Status, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return nil, err
	}
	return store.ListStatuses(ctx, fs...)
}

func (cs *attachableContentStore) Status(ctx context.Context, ref string) (content.Status, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return content.Status{}, err
	}
	return store.Status(ctx, ref)
}

func (cs *attachableContentStore) Abort(ctx context.Context, ref string) error {
	store, err := cs.choose(ctx)
	if err != nil {
		return err
	}
	return store.Abort(ctx, ref)
}

func (cs *attachableContentStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return nil, err
	}
	return store.Writer(ctx, opts...)
}

func (cs *attachableContentStore) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	store, err := cs.choose(ctx)
	if err != nil {
		return nil, err
	}
	return store.ReaderAt(ctx, desc)
}

type attachable struct {
	service api.ContentServer
}

// NewAttachable creates session.Attachable from aggregated stores.
// A key of the store map is an ID string that is used for choosing underlying store.
func NewAttachable(stores map[string]content.Store) session.Attachable {
	store := &attachableContentStore{stores: stores}
	service := contentserver.New(store)
	a := attachable{
		service: service,
	}
	return &a
}

func (a *attachable) Register(server *grpc.Server) {
	api.RegisterContentServer(server, a.service)
}

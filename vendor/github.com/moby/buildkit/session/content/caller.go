package content

import (
	"context"

	api "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/proxy"
	"github.com/moby/buildkit/session"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc/metadata"
)

type callerContentStore struct {
	store   content.Store
	storeID string
}

func (cs *callerContentStore) choose(ctx context.Context) context.Context {
	nsheader := metadata.Pairs(GRPCHeaderID, cs.storeID)
	md, ok := metadata.FromOutgoingContext(ctx) // merge with outgoing context.
	if !ok {
		md = nsheader
	} else {
		// order ensures the latest is first in this list.
		md = metadata.Join(nsheader, md)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func (cs *callerContentStore) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	ctx = cs.choose(ctx)
	return cs.store.Info(ctx, dgst)
}

func (cs *callerContentStore) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	ctx = cs.choose(ctx)
	return cs.store.Update(ctx, info, fieldpaths...)
}

func (cs *callerContentStore) Walk(ctx context.Context, fn content.WalkFunc, fs ...string) error {
	ctx = cs.choose(ctx)
	return cs.store.Walk(ctx, fn, fs...)
}

func (cs *callerContentStore) Delete(ctx context.Context, dgst digest.Digest) error {
	ctx = cs.choose(ctx)
	return cs.store.Delete(ctx, dgst)
}

func (cs *callerContentStore) ListStatuses(ctx context.Context, fs ...string) ([]content.Status, error) {
	ctx = cs.choose(ctx)
	return cs.store.ListStatuses(ctx, fs...)
}

func (cs *callerContentStore) Status(ctx context.Context, ref string) (content.Status, error) {
	ctx = cs.choose(ctx)
	return cs.store.Status(ctx, ref)
}

func (cs *callerContentStore) Abort(ctx context.Context, ref string) error {
	ctx = cs.choose(ctx)
	return cs.store.Abort(ctx, ref)
}

func (cs *callerContentStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	ctx = cs.choose(ctx)
	return cs.store.Writer(ctx, opts...)
}

func (cs *callerContentStore) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	ctx = cs.choose(ctx)
	return cs.store.ReaderAt(ctx, desc)
}

// NewCallerStore creates content.Store from session.Caller with specified storeID
func NewCallerStore(c session.Caller, storeID string) content.Store {
	client := api.NewContentClient(c.Conn())
	return &callerContentStore{
		store:   proxy.NewContentStore(client),
		storeID: storeID,
	}
}

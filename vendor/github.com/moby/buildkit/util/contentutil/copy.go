package contentutil

import (
	"context"
	"io"
	"sync"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func Copy(ctx context.Context, ingester content.Ingester, provider content.Provider, desc ocispec.Descriptor) error {
	if _, err := remotes.FetchHandler(ingester, &localFetcher{provider})(ctx, desc); err != nil {
		return err
	}
	return nil
}

type localFetcher struct {
	content.Provider
}

func (f *localFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	r, err := f.Provider.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	return &rc{ReaderAt: r}, nil
}

type rc struct {
	content.ReaderAt
	offset int
}

func (r *rc) Read(b []byte) (int, error) {
	n, err := r.ReadAt(b, int64(r.offset))
	r.offset += n
	if n > 0 && err == io.EOF {
		err = nil
	}
	return n, err
}

func CopyChain(ctx context.Context, ingester content.Ingester, provider content.Provider, desc ocispec.Descriptor) error {
	var m sync.Mutex
	manifestStack := []ocispec.Descriptor{}

	filterHandler := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest,
			images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
			m.Lock()
			manifestStack = append(manifestStack, desc)
			m.Unlock()
			return nil, images.ErrStopHandler
		default:
			return nil, nil
		}
	})
	handlers := []images.Handler{
		images.ChildrenHandler(provider),
		filterHandler,
		remotes.FetchHandler(ingester, &localFetcher{provider}),
	}

	if err := images.Dispatch(ctx, images.Handlers(handlers...), nil, desc); err != nil {
		return errors.WithStack(err)
	}

	for i := len(manifestStack) - 1; i >= 0; i-- {
		if err := Copy(ctx, ingester, provider, manifestStack[i]); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

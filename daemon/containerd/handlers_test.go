package containerd

import (
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

type recordingContentStore struct {
	content.Store
	info func(context.Context, digest.Digest) (content.Info, error)
}

func (s recordingContentStore) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	return s.info(ctx, dgst)
}

type recordingLeaseManager struct {
	noopLeasesManager
	calls    *[]string
	resource leases.Resource
}

func (m *recordingLeaseManager) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	*m.calls = append(*m.calls, "create")
	l := leases.Lease{ID: "content-walk"}
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return leases.Lease{}, err
		}
	}
	return l, nil
}

func (m *recordingLeaseManager) Delete(context.Context, leases.Lease, ...leases.DeleteOpt) error {
	*m.calls = append(*m.calls, "delete")
	return nil
}

func (m *recordingLeaseManager) AddResource(_ context.Context, _ leases.Lease, resource leases.Resource) error {
	*m.calls = append(*m.calls, "lease")
	m.resource = resource
	return nil
}

func TestWalkPresentChildrenLeasesBeforeContentAccess(t *testing.T) {
	var calls []string
	lm := &recordingLeaseManager{calls: &calls}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromString(t.Name()),
	}
	store := recordingContentStore{
		info: func(_ context.Context, dgst digest.Digest) (content.Info, error) {
			calls = append(calls, "info")
			return content.Info{Digest: dgst}, nil
		},
	}
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContentStore(store),
		containerd.WithLeasesService(lm),
	))
	assert.NilError(t, err)
	service := &ImageService{client: client, content: store}

	err = service.walkPresentChildren(t.Context(), desc, func(context.Context, ocispec.Descriptor) error {
		calls = append(calls, "handler")
		return nil
	})

	assert.NilError(t, err)
	assert.DeepEqual(t, calls, []string{"create", "lease", "info", "handler", "delete"})
	assert.DeepEqual(t, lm.resource, leases.Resource{
		ID:   desc.Digest.String(),
		Type: "content",
	})
}

func TestWalkPresentChildrenLeasesBeforeSkippingMissingContent(t *testing.T) {
	var calls []string
	lm := &recordingLeaseManager{calls: &calls}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromString(t.Name()),
	}
	store := recordingContentStore{
		info: func(context.Context, digest.Digest) (content.Info, error) {
			calls = append(calls, "info")
			return content.Info{}, cerrdefs.ErrNotFound
		},
	}
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithContentStore(store),
		containerd.WithLeasesService(lm),
	))
	assert.NilError(t, err)
	service := &ImageService{client: client, content: store}

	var handlerCalled bool
	err = service.walkPresentChildren(t.Context(), desc, func(context.Context, ocispec.Descriptor) error {
		handlerCalled = true
		return nil
	})

	assert.NilError(t, err)
	assert.DeepEqual(t, calls, []string{"create", "lease", "info", "delete"})
	assert.Check(t, !handlerCalled)
}

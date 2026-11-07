package distribution

import (
	"context"
	"net/url"
	"reflect"
	"testing"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/distribution/reference"
	"github.com/docker/distribution"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/progress"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	registrypkg "github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestGetRepositoryMountCandidates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hmacKey       string
		targetRepo    string
		maxCandidates int
		metadata      []metadata.V2Metadata
		candidates    []metadata.V2Metadata
	}{
		{
			name:          "empty metadata",
			targetRepo:    "busybox",
			maxCandidates: -1,
			metadata:      []metadata.V2Metadata{},
			candidates:    []metadata.V2Metadata{},
		},
		{
			name:          "one item not matching",
			targetRepo:    "busybox",
			maxCandidates: -1,
			metadata:      []metadata.V2Metadata{taggedMetadata("key", "dgst", "127.0.0.1/repo")},
			candidates:    []metadata.V2Metadata{},
		},
		{
			name:          "one item matching",
			targetRepo:    "busybox",
			maxCandidates: -1,
			metadata:      []metadata.V2Metadata{taggedMetadata("hash", "1", "docker.io/library/hello-world")},
			candidates:    []metadata.V2Metadata{taggedMetadata("hash", "1", "docker.io/library/hello-world")},
		},
		{
			name:          "allow missing SourceRepository",
			targetRepo:    "busybox",
			maxCandidates: -1,
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("1")},
				{Digest: digest.Digest("3")},
				{Digest: digest.Digest("2")},
			},
			candidates: []metadata.V2Metadata{},
		},
		{
			name:          "handle docker.io",
			targetRepo:    "user/app",
			maxCandidates: -1,
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("1"), SourceRepository: "docker.io/user/foo"},
				{Digest: digest.Digest("3"), SourceRepository: "docker.io/user/bar"},
				{Digest: digest.Digest("2"), SourceRepository: "docker.io/library/app"},
			},
			candidates: []metadata.V2Metadata{
				{Digest: digest.Digest("3"), SourceRepository: "docker.io/user/bar"},
				{Digest: digest.Digest("1"), SourceRepository: "docker.io/user/foo"},
				{Digest: digest.Digest("2"), SourceRepository: "docker.io/library/app"},
			},
		},
		{
			name:          "sort more items",
			hmacKey:       "abcd",
			targetRepo:    "127.0.0.1/foo/bar",
			maxCandidates: -1,
			metadata: []metadata.V2Metadata{
				taggedMetadata("hash", "1", "docker.io/library/hello-world"),
				taggedMetadata("efgh", "2", "127.0.0.1/hello-world"),
				taggedMetadata("abcd", "3", "docker.io/library/busybox"),
				taggedMetadata("hash", "4", "docker.io/library/busybox"),
				taggedMetadata("hash", "5", "127.0.0.1/foo"),
				taggedMetadata("hash", "6", "127.0.0.1/bar"),
				taggedMetadata("efgh", "7", "127.0.0.1/foo/bar"),
				taggedMetadata("abcd", "8", "127.0.0.1/xyz"),
				taggedMetadata("hash", "9", "127.0.0.1/foo/app"),
			},
			candidates: []metadata.V2Metadata{
				// first by matching hash
				taggedMetadata("abcd", "8", "127.0.0.1/xyz"),
				// then by longest matching prefix
				taggedMetadata("hash", "9", "127.0.0.1/foo/app"),
				taggedMetadata("hash", "5", "127.0.0.1/foo"),
				// sort the rest of the matching items in reversed order
				taggedMetadata("hash", "6", "127.0.0.1/bar"),
				taggedMetadata("efgh", "2", "127.0.0.1/hello-world"),
			},
		},
		{
			name:          "limit max candidates",
			hmacKey:       "abcd",
			targetRepo:    "user/app",
			maxCandidates: 3,
			metadata: []metadata.V2Metadata{
				taggedMetadata("abcd", "1", "docker.io/user/app1"),
				taggedMetadata("abcd", "2", "docker.io/user/app/base"),
				taggedMetadata("hash", "3", "docker.io/user/app"),
				taggedMetadata("abcd", "4", "127.0.0.1/user/app"),
				taggedMetadata("hash", "5", "docker.io/user/foo"),
				taggedMetadata("hash", "6", "docker.io/app/bar"),
			},
			candidates: []metadata.V2Metadata{
				// first by matching hash
				taggedMetadata("abcd", "2", "docker.io/user/app/base"),
				taggedMetadata("abcd", "1", "docker.io/user/app1"),
				// then by longest matching prefix
				// "docker.io/usr/app" is excluded since candidates must
				// be from a different repository
				taggedMetadata("hash", "5", "docker.io/user/foo"),
			},
		},
	} {
		repoInfo, err := reference.ParseNormalizedNamed(tc.targetRepo)
		if err != nil {
			t.Fatalf("[%s] failed to parse reference name: %v", tc.name, err)
		}
		candidates := getRepositoryMountCandidates(repoInfo, []byte(tc.hmacKey), tc.maxCandidates, tc.metadata)
		if len(candidates) != len(tc.candidates) {
			t.Errorf("[%s] got unexpected number of candidates: %d != %d", tc.name, len(candidates), len(tc.candidates))
		}
		for i := 0; i < len(candidates) && i < len(tc.candidates); i++ {
			if !reflect.DeepEqual(candidates[i], tc.candidates[i]) {
				t.Errorf("[%s] candidate %d does not match expected: %#+v != %#+v", tc.name, i, candidates[i], tc.candidates[i])
			}
		}
		for i := len(candidates); i < len(tc.candidates); i++ {
			t.Errorf("[%s] missing expected candidate at position %d (%#+v)", tc.name, i, tc.candidates[i])
		}
		for i := len(tc.candidates); i < len(candidates); i++ {
			t.Errorf("[%s] got unexpected candidate at position %d (%#+v)", tc.name, i, candidates[i])
		}
	}
}

func TestMediaTypeForMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata metadata.V2Metadata
		expected string
	}{
		{
			name:     "known media type",
			metadata: metadata.V2Metadata{MediaType: "application/vnd.oci.image.layer.v1.tar+zstd"},
			expected: "application/vnd.oci.image.layer.v1.tar+zstd",
		},
		{
			name:     "legacy metadata",
			metadata: metadata.V2Metadata{},
			expected: c8dimages.MediaTypeDockerSchema2LayerGzip,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, mediaTypeForMetadata(tc.metadata), tc.expected)
		})
	}
}

func TestLayerAlreadyExists(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		metadata               []metadata.V2Metadata
		targetRepo             string
		hmacKey                string
		maxExistenceChecks     int
		checkOtherRepositories bool
		remoteBlobs            map[digest.Digest]distribution.Descriptor
		remoteErrors           map[digest.Digest]error
		expectedDescriptor     distribution.Descriptor
		expectedExists         bool
		expectedError          error
		expectedRequests       []string
		expectedAdditions      []metadata.V2Metadata
		expectedRemovals       []metadata.V2Metadata
	}{
		{
			name:                   "empty metadata",
			targetRepo:             "busybox",
			maxExistenceChecks:     3,
			checkOtherRepositories: true,
		},
		{
			name:               "single not existent metadata",
			targetRepo:         "busybox",
			metadata:           []metadata.V2Metadata{{Digest: digest.Digest("pear"), SourceRepository: "docker.io/library/busybox"}},
			maxExistenceChecks: 3,
			expectedRequests:   []string{"pear"},
			expectedRemovals:   []metadata.V2Metadata{{Digest: digest.Digest("pear"), SourceRepository: "docker.io/library/busybox"}},
		},
		{
			name:               "access denied",
			targetRepo:         "busybox",
			maxExistenceChecks: 1,
			metadata:           []metadata.V2Metadata{{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/busybox"}},
			remoteErrors:       map[digest.Digest]error{digest.Digest("apple"): distribution.ErrAccessDenied},
			expectedError:      nil,
			expectedRequests:   []string{"apple"},
		},
		{
			name:               "not matching repositories",
			targetRepo:         "busybox",
			maxExistenceChecks: 3,
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/hello-world"},
				{Digest: digest.Digest("orange"), SourceRepository: "docker.io/library/busybox/subapp"},
				{Digest: digest.Digest("pear"), SourceRepository: "docker.io/busybox"},
				{Digest: digest.Digest("plum"), SourceRepository: "busybox"},
				{Digest: digest.Digest("banana"), SourceRepository: "127.0.0.1/busybox"},
			},
		},
		{
			name:                   "check other repositories",
			targetRepo:             "busybox",
			maxExistenceChecks:     10,
			checkOtherRepositories: true,
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/hello-world"},
				{Digest: digest.Digest("orange"), SourceRepository: "docker.io/busybox/subapp"},
				{Digest: digest.Digest("pear"), SourceRepository: "docker.io/busybox"},
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/library/busybox"},
				{Digest: digest.Digest("banana"), SourceRepository: "127.0.0.1/busybox"},
			},
			expectedRequests: []string{"plum", "apple", "pear", "orange", "banana"},
			expectedRemovals: []metadata.V2Metadata{
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/library/busybox"},
			},
		},
		{
			name:               "find existing blob",
			targetRepo:         "busybox",
			metadata:           []metadata.V2Metadata{{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/busybox"}},
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("apple"): {Digest: digest.Digest("apple")}},
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("apple"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"apple"},
		},
		{
			name:       "preserve source media type",
			targetRepo: "busybox",
			metadata: []metadata.V2Metadata{{
				Digest:           digest.Digest("apple"),
				SourceRepository: "docker.io/library/busybox",
				MediaType:        "application/vnd.oci.image.layer.v1.tar+zstd",
			}},
			maxExistenceChecks: 1,
			remoteBlobs: map[digest.Digest]distribution.Descriptor{
				digest.Digest("apple"): {Digest: digest.Digest("apple")},
			},
			expectedDescriptor: distribution.Descriptor{
				Digest:    digest.Digest("apple"),
				MediaType: "application/vnd.oci.image.layer.v1.tar+zstd",
			},
			expectedExists:   true,
			expectedRequests: []string{"apple"},
		},
		{
			name:               "find existing blob with different hmac",
			targetRepo:         "busybox",
			metadata:           []metadata.V2Metadata{{SourceRepository: "docker.io/library/busybox", Digest: digest.Digest("apple"), HMAC: "dummyhmac"}},
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("apple"): {Digest: digest.Digest("apple")}},
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("apple"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"apple"},
			expectedAdditions:  []metadata.V2Metadata{{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/busybox"}},
		},
		{
			name:               "overwrite media types",
			targetRepo:         "busybox",
			metadata:           []metadata.V2Metadata{{Digest: digest.Digest("apple"), SourceRepository: "docker.io/library/busybox"}},
			hmacKey:            "key",
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("apple"): {Digest: digest.Digest("apple"), MediaType: "custom-media-type"}},
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("apple"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"apple"},
			expectedAdditions:  []metadata.V2Metadata{taggedMetadata("key", "apple", "docker.io/library/busybox")},
		},
		{
			name:       "find existing blob among many",
			targetRepo: "127.0.0.1/myapp",
			hmacKey:    "key",
			metadata: []metadata.V2Metadata{
				taggedMetadata("someotherkey", "pear", "127.0.0.1/myapp"),
				taggedMetadata("key", "apple", "127.0.0.1/myapp"),
				taggedMetadata("", "plum", "127.0.0.1/myapp"),
			},
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("pear"): {Digest: digest.Digest("pear")}},
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("pear"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"apple", "plum", "pear"},
			expectedAdditions:  []metadata.V2Metadata{taggedMetadata("key", "pear", "127.0.0.1/myapp")},
			expectedRemovals: []metadata.V2Metadata{
				taggedMetadata("key", "apple", "127.0.0.1/myapp"),
				{Digest: digest.Digest("plum"), SourceRepository: "127.0.0.1/myapp"},
			},
		},
		{
			name:       "reach maximum existence checks",
			targetRepo: "user/app",
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("pear"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("apple"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("banana"), SourceRepository: "docker.io/user/app"},
			},
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("pear"): {Digest: digest.Digest("pear")}},
			expectedExists:     false,
			expectedRequests:   []string{"banana", "plum", "apple"},
			expectedRemovals: []metadata.V2Metadata{
				{Digest: digest.Digest("banana"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("apple"), SourceRepository: "docker.io/user/app"},
			},
		},
		{
			name:       "zero allowed existence checks",
			targetRepo: "user/app",
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("pear"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("apple"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/user/app"},
				{Digest: digest.Digest("banana"), SourceRepository: "docker.io/user/app"},
			},
			maxExistenceChecks: 0,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("pear"): {Digest: digest.Digest("pear")}},
		},
		{
			name:       "stat single digest just once",
			targetRepo: "busybox",
			metadata: []metadata.V2Metadata{
				taggedMetadata("key1", "pear", "docker.io/library/busybox"),
				taggedMetadata("key2", "apple", "docker.io/library/busybox"),
				taggedMetadata("key3", "apple", "docker.io/library/busybox"),
			},
			maxExistenceChecks: 3,
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("pear"): {Digest: digest.Digest("pear")}},
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("pear"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"apple", "pear"},
			expectedAdditions:  []metadata.V2Metadata{{Digest: digest.Digest("pear"), SourceRepository: "docker.io/library/busybox"}},
			expectedRemovals:   []metadata.V2Metadata{taggedMetadata("key3", "apple", "docker.io/library/busybox")},
		},
		{
			name:       "don't stop on first error",
			targetRepo: "user/app",
			hmacKey:    "key",
			metadata: []metadata.V2Metadata{
				taggedMetadata("key", "banana", "docker.io/user/app"),
				taggedMetadata("key", "orange", "docker.io/user/app"),
				taggedMetadata("key", "plum", "docker.io/user/app"),
			},
			maxExistenceChecks: 3,
			remoteErrors:       map[digest.Digest]error{"orange": distribution.ErrAccessDenied},
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("apple"): {}},
			expectedError:      nil,
			expectedRequests:   []string{"plum", "orange", "banana"},
			expectedRemovals: []metadata.V2Metadata{
				taggedMetadata("key", "plum", "docker.io/user/app"),
				taggedMetadata("key", "banana", "docker.io/user/app"),
			},
		},
		{
			name:       "remove outdated metadata",
			targetRepo: "docker.io/user/app",
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("plum"), SourceRepository: "docker.io/library/busybox"},
				{Digest: digest.Digest("orange"), SourceRepository: "docker.io/user/app"},
			},
			maxExistenceChecks: 3,
			remoteErrors:       map[digest.Digest]error{"orange": distribution.ErrBlobUnknown},
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("plum"): {}},
			expectedExists:     false,
			expectedRequests:   []string{"orange"},
			expectedRemovals:   []metadata.V2Metadata{{Digest: digest.Digest("orange"), SourceRepository: "docker.io/user/app"}},
		},
		{
			name:       "missing SourceRepository",
			targetRepo: "busybox",
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("1")},
				{Digest: digest.Digest("3")},
				{Digest: digest.Digest("2")},
			},
			maxExistenceChecks: 3,
			expectedExists:     false,
			expectedRequests:   []string{"2", "3", "1"},
		},

		{
			name:       "with and without SourceRepository",
			targetRepo: "busybox",
			metadata: []metadata.V2Metadata{
				{Digest: digest.Digest("1")},
				{Digest: digest.Digest("2"), SourceRepository: "docker.io/library/busybox"},
				{Digest: digest.Digest("3")},
			},
			remoteBlobs:        map[digest.Digest]distribution.Descriptor{digest.Digest("1"): {Digest: digest.Digest("1")}},
			maxExistenceChecks: 3,
			expectedDescriptor: distribution.Descriptor{Digest: digest.Digest("1"), MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip},
			expectedExists:     true,
			expectedRequests:   []string{"2", "3", "1"},
			expectedAdditions:  []metadata.V2Metadata{{Digest: digest.Digest("1"), SourceRepository: "docker.io/library/busybox"}},
			expectedRemovals: []metadata.V2Metadata{
				{Digest: digest.Digest("2"), SourceRepository: "docker.io/library/busybox"},
			},
		},
	} {
		repoInfo, err := reference.ParseNormalizedNamed(tc.targetRepo)
		if err != nil {
			t.Fatalf("[%s] failed to parse reference name: %v", tc.name, err)
		}
		repo := &mockRepo{
			t:        t,
			errors:   tc.remoteErrors,
			blobs:    tc.remoteBlobs,
			requests: []string{},
		}
		ctx := t.Context()
		ms := &mockV2MetadataService{}
		pd := &pushDescriptor{
			hmacKey:  []byte(tc.hmacKey),
			repoName: repoInfo,
			layer: &storeLayer{
				Layer: layer.EmptyLayer,
			},
			repo:            repo,
			metadataService: ms,
			pushState:       &pushState{remoteLayers: make(map[layer.DiffID]distribution.Descriptor)},
			checkedDigests:  make(map[digest.Digest]struct{}),
		}

		desc, exists, err := pd.layerAlreadyExists(ctx, &progressSink{t}, layer.EmptyLayer.DiffID(), tc.checkOtherRepositories, tc.maxExistenceChecks, tc.metadata)

		if !reflect.DeepEqual(desc, tc.expectedDescriptor) {
			t.Errorf("[%s] got unexpected descriptor: %#+v != %#+v", tc.name, desc, tc.expectedDescriptor)
		}
		if exists != tc.expectedExists {
			t.Errorf("[%s] got unexpected exists: %t != %t", tc.name, exists, tc.expectedExists)
		}
		assert.Check(t, is.ErrorIs(err, tc.expectedError))

		if len(repo.requests) != len(tc.expectedRequests) {
			t.Errorf("[%s] got unexpected number of requests: %d != %d", tc.name, len(repo.requests), len(tc.expectedRequests))
		}
		for i := 0; i < len(repo.requests) && i < len(tc.expectedRequests); i++ {
			if repo.requests[i] != tc.expectedRequests[i] {
				t.Errorf("[%s] request %d does not match expected: %q != %q", tc.name, i, repo.requests[i], tc.expectedRequests[i])
			}
		}
		for i := len(repo.requests); i < len(tc.expectedRequests); i++ {
			t.Errorf("[%s] missing expected request at position %d (%q)", tc.name, i, tc.expectedRequests[i])
		}
		for i := len(tc.expectedRequests); i < len(repo.requests); i++ {
			t.Errorf("[%s] got unexpected request at position %d (%q)", tc.name, i, repo.requests[i])
		}

		if len(ms.added) != len(tc.expectedAdditions) {
			t.Errorf("[%s] got unexpected number of additions: %d != %d", tc.name, len(ms.added), len(tc.expectedAdditions))
		}
		for i := 0; i < len(ms.added) && i < len(tc.expectedAdditions); i++ {
			if ms.added[i] != tc.expectedAdditions[i] {
				t.Errorf("[%s] added metadata at %d does not match expected: %q != %q", tc.name, i, ms.added[i], tc.expectedAdditions[i])
			}
		}
		for i := len(ms.added); i < len(tc.expectedAdditions); i++ {
			t.Errorf("[%s] missing expected addition at position %d (%q)", tc.name, i, tc.expectedAdditions[i])
		}
		for i := len(tc.expectedAdditions); i < len(ms.added); i++ {
			t.Errorf("[%s] unexpected metadata addition at position %d (%q)", tc.name, i, ms.added[i])
		}

		if len(ms.removed) != len(tc.expectedRemovals) {
			t.Errorf("[%s] got unexpected number of removals: %d != %d", tc.name, len(ms.removed), len(tc.expectedRemovals))
		}
		for i := 0; i < len(ms.removed) && i < len(tc.expectedRemovals); i++ {
			if ms.removed[i] != tc.expectedRemovals[i] {
				t.Errorf("[%s] removed metadata at %d does not match expected: %q != %q", tc.name, i, ms.removed[i], tc.expectedRemovals[i])
			}
		}
		for i := len(ms.removed); i < len(tc.expectedRemovals); i++ {
			t.Errorf("[%s] missing expected removal at position %d (%q)", tc.name, i, tc.expectedRemovals[i])
		}
		for i := len(tc.expectedRemovals); i < len(ms.removed); i++ {
			t.Errorf("[%s] removed unexpected metadata at position %d (%q)", tc.name, i, ms.removed[i])
		}
	}
}

type mockReferenceStore struct {
	refstore.Store
}

func (s *mockReferenceStore) ReferencesByName(ref reference.Named) []refstore.Association {
	return []refstore.Association{}
}

func TestUploadPreservesMediaTypeWhenBlobIsMounted(t *testing.T) {
	targetRepo, err := reference.ParseNormalizedNamed("registry.example/target")
	assert.NilError(t, err)
	sourceRepo, err := reference.ParseNormalizedNamed("registry.example/source")
	assert.NilError(t, err)

	blobDigest := digest.FromBytes([]byte("zstd blob"))
	sourceRef, err := reference.WithDigest(reference.TrimNamed(sourceRepo), blobDigest)
	assert.NilError(t, err)

	const zstdMediaType = "application/vnd.oci.image.layer.v1.tar+zstd"
	mounted := distribution.ErrBlobMounted{
		From: sourceRef,
		Descriptor: distribution.Descriptor{
			Digest: blobDigest,
			Size:   10,
		},
	}
	repo := &mockRepoWithMount{
		mockRepo: mockRepo{
			t:        t,
			blobs:    map[digest.Digest]distribution.Descriptor{},
			requests: []string{},
		},
		createErr: mounted,
	}
	ms := &mockMetadataServiceWithEntries{
		entries: []metadata.V2Metadata{{
			Digest:           blobDigest,
			SourceRepository: sourceRepo.Name(),
			MediaType:        zstdMediaType,
		}},
	}
	pd := &pushDescriptor{
		layer:           &storeLayer{Layer: layer.EmptyLayer},
		metadataService: ms,
		hmacKey:         nil,
		repoName:        targetRepo,
		ref:             targetRepo,
		repo:            repo,
		pushState:       &pushState{remoteLayers: make(map[layer.DiffID]distribution.Descriptor)},
		checkedDigests:  make(map[digest.Digest]struct{}),
	}

	desc, err := pd.Upload(t.Context(), &progressSink{t})
	assert.NilError(t, err)
	assert.Equal(t, desc.Digest, blobDigest)
	assert.Equal(t, desc.MediaType, zstdMediaType)
	assert.Equal(t, pd.pushState.remoteLayers[layer.EmptyLayer.DiffID()].MediaType, zstdMediaType)
	assert.Equal(t, len(ms.added), 1)
	assert.Equal(t, ms.added[0].MediaType, zstdMediaType)
}

type mockMetadataServiceWithEntries struct {
	mockV2MetadataService
	entries []metadata.V2Metadata
}

func (m *mockMetadataServiceWithEntries) GetMetadata(layer.DiffID) ([]metadata.V2Metadata, error) {
	return m.entries, nil
}

type mockRepoWithMount struct {
	mockRepo
	createErr error
}

func (m *mockRepoWithMount) Blobs(context.Context) distribution.BlobStore {
	return &mockBlobStoreWithMount{
		mockBlobStore: mockBlobStore{repo: &m.mockRepo},
		createErr:     m.createErr,
	}
}

type mockBlobStoreWithMount struct {
	mockBlobStore
	createErr error
}

func (m *mockBlobStoreWithMount) Create(context.Context, ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	return nil, m.createErr
}

func TestWhenEmptyAuthConfig(t *testing.T) {
	for _, authInfo := range []struct {
		username      string
		password      string
		registryToken string
		expected      bool
	}{
		{
			username:      "",
			password:      "",
			registryToken: "",
			expected:      false,
		},
		{
			username:      "username",
			password:      "password",
			registryToken: "",
			expected:      true,
		},
		{
			username:      "",
			password:      "",
			registryToken: "token",
			expected:      true,
		},
	} {
		imagePushConfig := &ImagePushConfig{}
		imagePushConfig.AuthConfig = &registry.AuthConfig{
			Username:      authInfo.username,
			Password:      authInfo.password,
			RegistryToken: authInfo.registryToken,
		}
		imagePushConfig.ReferenceStore = &mockReferenceStore{}
		repoName, _ := reference.ParseNormalizedNamed("xujihui1985/test.img")
		testPusher := &pusher{
			config:   imagePushConfig,
			repoName: repoName,
			endpoint: registrypkg.APIEndpoint{
				URL: &url.URL{
					Scheme: "https",
					Host:   registrypkg.IndexHostname,
				},
			},
		}
		_ = testPusher.push(t.Context())
		if testPusher.pushState.hasAuthInfo != authInfo.expected {
			t.Errorf("hasAuthInfo does not match expected: %t != %t", authInfo.expected, testPusher.pushState.hasAuthInfo)
		}
	}
}

type mockBlobStoreWithCreate struct {
	mockBlobStore
	repo *mockRepoWithBlob
}

func (blob *mockBlobStoreWithCreate) Create(ctx context.Context, options ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	return nil, errcode.Errors([]error{errcode.ErrorCodeUnauthorized.WithMessage("unauthorized")})
}

type mockRepoWithBlob struct {
	mockRepo
}

func (m *mockRepoWithBlob) Blobs(ctx context.Context) distribution.BlobStore {
	blob := &mockBlobStoreWithCreate{}
	blob.mockBlobStore.repo = &m.mockRepo
	blob.repo = m
	return blob
}

type mockMetadataService struct {
	mockV2MetadataService
}

func (m *mockMetadataService) GetMetadata(diffID layer.DiffID) ([]metadata.V2Metadata, error) {
	return []metadata.V2Metadata{
		taggedMetadata("abcd", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e28", "docker.io/user/app1"),
		taggedMetadata("abcd", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e22", "docker.io/user/app/base"),
		taggedMetadata("hash", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e23", "docker.io/user/app"),
		taggedMetadata("abcd", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e24", "127.0.0.1/user/app"),
		taggedMetadata("hash", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e25", "docker.io/user/foo"),
		taggedMetadata("hash", "sha256:ff3a5c916c92643ff77519ffa742d3ec61b7f591b6b7504599d95a4a41134e26", "docker.io/app/bar"),
	}, nil
}

var removeMetadata bool

func (m *mockMetadataService) Remove(metadata metadata.V2Metadata) error {
	removeMetadata = true
	return nil
}

func TestPushRegistryWhenAuthInfoEmpty(t *testing.T) {
	repoInfo, _ := reference.ParseNormalizedNamed("user/app")
	ms := &mockMetadataService{}
	remoteErrors := map[digest.Digest]error{digest.Digest("sha256:apple"): distribution.ErrAccessDenied}
	remoteBlobs := map[digest.Digest]distribution.Descriptor{digest.Digest("sha256:apple"): {Digest: digest.Digest("shar256:apple")}}
	repo := &mockRepoWithBlob{
		mockRepo: mockRepo{
			t:        t,
			errors:   remoteErrors,
			blobs:    remoteBlobs,
			requests: []string{},
		},
	}
	pd := &pushDescriptor{
		hmacKey:  []byte("abcd"),
		repoName: repoInfo,
		layer: &storeLayer{
			Layer: layer.EmptyLayer,
		},
		repo:            repo,
		metadataService: ms,
		pushState: &pushState{
			remoteLayers: make(map[layer.DiffID]distribution.Descriptor),
			hasAuthInfo:  false,
		},
		checkedDigests: make(map[digest.Digest]struct{}),
	}
	pd.Upload(t.Context(), &progressSink{t})
	if removeMetadata {
		t.Fatalf("expect remove not be called but called")
	}
}

func taggedMetadata(key string, dgst string, sourceRepo string) metadata.V2Metadata {
	meta := metadata.V2Metadata{
		Digest:           digest.Digest(dgst),
		SourceRepository: sourceRepo,
	}

	meta.HMAC = metadata.ComputeV2MetadataHMAC([]byte(key), &meta)
	return meta
}

type mockRepo struct {
	distribution.Repository
	t        *testing.T
	errors   map[digest.Digest]error
	blobs    map[digest.Digest]distribution.Descriptor
	requests []string
}

var _ distribution.Repository = &mockRepo{}

func (m *mockRepo) Blobs(ctx context.Context) distribution.BlobStore {
	return &mockBlobStore{
		repo: m,
	}
}

type mockBlobStore struct {
	distribution.BlobStore
	repo *mockRepo
}

var _ distribution.BlobStore = &mockBlobStore{}

func (m *mockBlobStore) Stat(ctx context.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	m.repo.requests = append(m.repo.requests, dgst.String())
	if err, exists := m.repo.errors[dgst]; exists {
		return distribution.Descriptor{}, err
	}
	if desc, exists := m.repo.blobs[dgst]; exists {
		return desc, nil
	}
	return distribution.Descriptor{}, distribution.ErrBlobUnknown
}

type mockV2MetadataService struct {
	metadata.V2MetadataService
	added   []metadata.V2Metadata
	removed []metadata.V2Metadata
}

var _ metadata.V2MetadataService = &mockV2MetadataService{}

func (m *mockV2MetadataService) Add(diffID layer.DiffID, metadata metadata.V2Metadata) error {
	m.added = append(m.added, metadata)
	return nil
}

func (m *mockV2MetadataService) TagAndAdd(diffID layer.DiffID, hmacKey []byte, meta metadata.V2Metadata) error {
	meta.HMAC = metadata.ComputeV2MetadataHMAC(hmacKey, &meta)
	m.Add(diffID, meta)
	return nil
}

func (m *mockV2MetadataService) Remove(metadata metadata.V2Metadata) error {
	m.removed = append(m.removed, metadata)
	return nil
}

type progressSink struct {
	t *testing.T
}

func (s *progressSink) WriteProgress(p progress.Progress) error {
	s.t.Logf("progress update: %#+v", p)
	return nil
}

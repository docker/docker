package containerd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log/logtest"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/util/attestation"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// normalizeRef normalizes an image reference to the canonical form that
// resolveImage expects (e.g. "test:latest" → "docker.io/library/test:latest").
func normalizeRef(t *testing.T, name string) string {
	t.Helper()
	ref, err := reference.ParseNormalizedNamed(name)
	assert.NilError(t, err)
	return reference.TagNameOnly(ref).String()
}

// provBlob writes content to dir/blobs/sha256/<digest> and returns its descriptor.
func provBlob(t *testing.T, dir, mt string, data []byte) ocispec.Descriptor {
	t.Helper()
	sha256Dir := filepath.Join(dir, "blobs", "sha256")
	assert.NilError(t, os.MkdirAll(sha256Dir, 0o755))
	dgst := digest.FromBytes(data)
	assert.NilError(t, os.WriteFile(filepath.Join(sha256Dir, dgst.Encoded()), data, 0o644))
	return ocispec.Descriptor{MediaType: mt, Digest: dgst, Size: int64(len(data))}
}

// provJSON marshals v and writes it as a blob.
func provJSON(t *testing.T, dir, mt string, v any) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	assert.NilError(t, err)
	return provBlob(t, dir, mt, b)
}

// attestationLayer describes one layer of an attestation manifest.
// When predicateType is empty the layer carries no in-toto annotation.
type attestationLayer struct {
	predicateType string
	content       []byte
}

// buildFullIndex writes a complete OCI image index containing a real platform
// image manifest and an attestation manifest for it. The platform manifest is
// always written to the content store; attestation layer blobs are written only
// when writeLayerBlobs is true. Returns the index descriptor, the platform
// image manifest descriptor, and the attestation manifest descriptor.
func buildFullIndex(t *testing.T, dir string, platform ocispec.Platform, stmts []attestationLayer, writeLayerBlobs bool) (ocispec.Descriptor, ocispec.Descriptor) {
	t.Helper()

	// Minimal platform image manifest (config + no layers).
	imgConfig := map[string]any{
		"os":           platform.OS,
		"architecture": platform.Architecture,
	}
	configDesc := provJSON(t, dir, ocispec.MediaTypeImageConfig, imgConfig)
	imgMfst := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{},
	}
	imgMfstDesc := provJSON(t, dir, ocispec.MediaTypeImageManifest, imgMfst)
	imgMfstDesc.Platform = &platform

	// Build attestation layer descriptors.
	var layerDescs []ocispec.Descriptor
	for _, s := range stmts {
		var desc ocispec.Descriptor
		if writeLayerBlobs {
			desc = provBlob(t, dir, "application/vnd.in-toto+json", s.content)
		} else {
			dgst := digest.FromBytes(s.content)
			desc = ocispec.Descriptor{
				MediaType: "application/vnd.in-toto+json",
				Digest:    dgst,
				Size:      int64(len(s.content)),
			}
		}
		if s.predicateType != "" {
			desc.Annotations = map[string]string{
				"in-toto.io/predicate-type": s.predicateType,
			}
		}
		layerDescs = append(layerDescs, desc)
	}

	// Attestation manifest.
	attConfig := provBlob(t, dir, ocispec.MediaTypeImageConfig, []byte(`{}`))
	attMfst := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    attConfig,
		Layers:    layerDescs,
	}
	attMfstDesc := provJSON(t, dir, ocispec.MediaTypeImageManifest, attMfst)
	attMfstDesc.Annotations = map[string]string{
		attestation.DockerAnnotationReferenceType:   attestation.DockerAnnotationReferenceTypeDefault,
		attestation.DockerAnnotationReferenceDigest: imgMfstDesc.Digest.String(),
	}
	attMfstDesc.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}

	// Index.
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{imgMfstDesc, attMfstDesc},
	}
	idxDesc := provJSON(t, dir, ocispec.MediaTypeImageIndex, idx)
	return idxDesc, imgMfstDesc
}

// buildAttestationIndex writes a minimal OCI image index containing a single
// attestation manifest whose layers are given by stmts. The index blob and the
// attestation manifest blob are always written to dir; layer blobs are written
// only when writeLayerBlobs is true (pass false to simulate unavailable content).
// Returns the index descriptor (suitable for registering with an image store) and
// the synthetic platform-image digest referenced by the attestation.
//
// This helper produces an index WITHOUT a real image manifest, which is useful
// for verifying that AttestationData.For is set correctly in image listings.
func buildAttestationIndex(t *testing.T, dir string, stmts []attestationLayer, writeLayerBlobs bool) (ocispec.Descriptor, digest.Digest) {
	t.Helper()

	// Minimal empty config for the attestation manifest.
	configDesc := provBlob(t, dir, ocispec.MediaTypeImageConfig, []byte(`{}`))

	// Build layer descriptors; blobs are written only when writeLayerBlobs is set.
	var layerDescs []ocispec.Descriptor
	for _, s := range stmts {
		var desc ocispec.Descriptor
		if writeLayerBlobs {
			desc = provBlob(t, dir, "application/vnd.in-toto+json", s.content)
		} else {
			dgst := digest.FromBytes(s.content)
			desc = ocispec.Descriptor{
				MediaType: "application/vnd.in-toto+json",
				Digest:    dgst,
				Size:      int64(len(s.content)),
			}
		}
		if s.predicateType != "" {
			desc.Annotations = map[string]string{
				"in-toto.io/predicate-type": s.predicateType,
			}
		}
		layerDescs = append(layerDescs, desc)
	}

	// Write the attestation manifest blob.
	attMfst := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layerDescs,
	}
	attMfstDesc := provJSON(t, dir, ocispec.MediaTypeImageManifest, attMfst)

	// Synthetic platform manifest digest — it need not be in the content store.
	platformDigest := digest.FromString("platform-manifest-placeholder")

	// Annotate the attestation manifest descriptor for the index.
	attMfstDesc.Annotations = map[string]string{
		attestation.DockerAnnotationReferenceType:   attestation.DockerAnnotationReferenceTypeDefault,
		attestation.DockerAnnotationReferenceDigest: platformDigest.String(),
	}
	attMfstDesc.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}

	// Write the index blob.
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{attMfstDesc},
	}
	idxDesc := provJSON(t, dir, ocispec.MediaTypeImageIndex, idx)
	idxDesc.Annotations = map[string]string{
		c8dimages.AnnotationImageName: "test:latest",
	}

	return idxDesc, platformDigest
}

// findAttestationManifest returns the first attestation-kind manifest summary in
// manifests, or fails the test if none is found.
func findAttestationManifest(t *testing.T, manifests []imagetypes.ManifestSummary) imagetypes.ManifestSummary {
	t.Helper()
	for _, m := range manifests {
		if m.Kind == imagetypes.ManifestKindAttestation {
			return m
		}
	}
	t.Fatal("no attestation manifest found in image summary")
	return imagetypes.ManifestSummary{}
}

// TestAttestationDataFor verifies that the AttestationData.For field in the
// image list response is set to the digest of the image manifest the
// attestation is for.
func TestAttestationDataFor(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing")
	ctx = logtest.WithT(ctx, t)

	dir := t.TempDir()
	idxDesc, platformDigest := buildAttestationIndex(t, dir, []attestationLayer{
		{predicateType: "https://slsa.dev/provenance/v0.2", content: []byte(`{}`)},
	}, true)

	cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
	svc := fakeImageService(t, ctx, cs)
	_, err := svc.images.Create(ctx, c8dimages.Image{Name: "test:latest", Target: idxDesc})
	assert.NilError(t, err)

	all, err := svc.Images(ctx, imagebackend.ListOptions{Manifests: true})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(all, 1))

	m := findAttestationManifest(t, all[0].Manifests)
	assert.Assert(t, m.AttestationData != nil)
	assert.Check(t, is.Equal(m.AttestationData.For, platformDigest))
}

// TestImageAttestations exercises the ImageAttestations method with various
// statement counts, predicate type filters, and error conditions.
func TestImageAttestations(t *testing.T) {
	linuxAMD64 := ocispec.Platform{OS: "linux", Architecture: "amd64"}

	t.Run("single_statement", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		stmtData, _ := json.Marshal(map[string]any{
			"predicateType": "https://slsa.dev/provenance/v0.2",
			"predicate": map[string]any{
				"materials": []map[string]any{
					{"uri": "pkg:docker/library/alpine@3.18"},
				},
			},
		})
		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "https://slsa.dev/provenance/v0.2", content: stmtData},
		}, true)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform:         &linuxAMD64,
			IncludeStatement: true,
		})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(stmts, 1))
		assert.Check(t, is.Equal(stmts[0].PredicateType, "https://slsa.dev/provenance/v0.2"))
		assert.Assert(t, stmts[0].Statement != nil)

		var parsed struct {
			PredicateType string `json:"predicateType"`
			Predicate     struct {
				Materials []struct {
					URI string `json:"uri"`
				} `json:"materials"`
			} `json:"predicate"`
		}
		assert.NilError(t, json.Unmarshal(*stmts[0].Statement, &parsed))
		assert.Check(t, is.Equal(parsed.PredicateType, "https://slsa.dev/provenance/v0.2"))
		assert.Check(t, is.Equal(len(parsed.Predicate.Materials), 1))
		assert.Check(t, is.Equal(parsed.Predicate.Materials[0].URI, "pkg:docker/library/alpine@3.18"))
	})

	t.Run("default_omits_statement_body", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		stmtData, _ := json.Marshal(map[string]any{"predicateType": "https://slsa.dev/provenance/v0.2"})
		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "https://slsa.dev/provenance/v0.2", content: stmtData},
		}, false /* layer blobs absent — proves we never read them */)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform: &linuxAMD64,
		})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(stmts, 1))
		assert.Check(t, is.Equal(stmts[0].PredicateType, "https://slsa.dev/provenance/v0.2"))
		assert.Check(t, stmts[0].Descriptor.Digest != "", "descriptor should be populated")
		assert.Check(t, stmts[0].Statement == nil, "statement body should be omitted by default")
	})

	t.Run("multiple_statements_order_preserved", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		v02Data, _ := json.Marshal(map[string]any{"predicateType": "https://slsa.dev/provenance/v0.2"})
		v1Data, _ := json.Marshal(map[string]any{"predicateType": "https://slsa.dev/provenance/v1"})

		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "https://slsa.dev/provenance/v0.2", content: v02Data},
			{predicateType: "https://slsa.dev/provenance/v1", content: v1Data},
		}, true)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform: &linuxAMD64,
		})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(stmts, 2))
		assert.Check(t, is.Equal(stmts[0].PredicateType, "https://slsa.dev/provenance/v0.2"))
		assert.Check(t, is.Equal(stmts[1].PredicateType, "https://slsa.dev/provenance/v1"))
	})

	t.Run("predicate_type_filter", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		v02Data, _ := json.Marshal(map[string]any{"predicateType": "https://slsa.dev/provenance/v0.2"})
		sbomData, _ := json.Marshal(map[string]any{"predicateType": "https://spdx.dev/Document"})

		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "https://slsa.dev/provenance/v0.2", content: v02Data},
			{predicateType: "https://spdx.dev/Document", content: sbomData},
		}, true)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform:       &linuxAMD64,
			PredicateTypes: []string{"https://slsa.dev/provenance/v0.2"},
		})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(stmts, 1))
		assert.Check(t, is.Equal(stmts[0].PredicateType, "https://slsa.dev/provenance/v0.2"))
	})

	t.Run("layer_without_predicate_type_skipped", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		annotatedData, _ := json.Marshal(map[string]any{"predicateType": "https://slsa.dev/provenance/v0.2"})

		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "", content: []byte(`{"some":"other"}`)},
			{predicateType: "https://slsa.dev/provenance/v0.2", content: annotatedData},
		}, true)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform: &linuxAMD64,
		})
		assert.NilError(t, err)
		assert.Assert(t, is.Len(stmts, 1), "only layers with in-toto annotation should be returned")
		assert.Check(t, is.Equal(stmts[0].PredicateType, "https://slsa.dev/provenance/v0.2"))
	})

	t.Run("unavailable_blobs_return_error", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		stmtData := []byte(`{"predicateType":"https://slsa.dev/provenance/v0.2"}`)

		idxDesc, _ := buildFullIndex(t, dir, linuxAMD64, []attestationLayer{
			{predicateType: "https://slsa.dev/provenance/v0.2", content: stmtData},
		}, false /* layer blobs absent */)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		// When a statement layer blob is not locally present we surface the
		// error rather than silently skipping. This matches BuildKit's
		// ResolveAttestations behavior via the shared policy-helpers library.
		_, err = svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform:         &linuxAMD64,
			IncludeStatement: true,
		})
		assert.ErrorContains(t, err, "not available locally")
	})

	t.Run("no_attestations_returns_nil", func(t *testing.T) {
		ctx := namespaces.WithNamespace(t.Context(), "testing")
		ctx = logtest.WithT(ctx, t)

		dir := t.TempDir()
		// Index with only an image manifest, no attestation manifest.
		imgConfig := map[string]any{"os": "linux", "architecture": "amd64"}
		configDesc := provJSON(t, dir, ocispec.MediaTypeImageConfig, imgConfig)
		imgMfst := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDesc,
			Layers:    []ocispec.Descriptor{},
		}
		imgMfstDesc := provJSON(t, dir, ocispec.MediaTypeImageManifest, imgMfst)
		imgMfstDesc.Platform = &linuxAMD64

		idx := ocispec.Index{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: []ocispec.Descriptor{imgMfstDesc},
		}
		idxDesc := provJSON(t, dir, ocispec.MediaTypeImageIndex, idx)

		cs := &blobsDirContentStore{blobs: filepath.Join(dir, "blobs", "sha256")}
		svc := fakeImageService(t, ctx, cs)
		imgName := normalizeRef(t, "test:latest")
		_, err := svc.images.Create(ctx, c8dimages.Image{Name: imgName, Target: idxDesc})
		assert.NilError(t, err)

		stmts, err := svc.ImageAttestations(ctx, "test:latest", imagebackend.AttestationOpts{
			Platform: &linuxAMD64,
		})
		assert.NilError(t, err)
		assert.Check(t, is.Nil(stmts))
	})
}

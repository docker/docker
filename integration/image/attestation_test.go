package image

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/util/attestation"
	"github.com/moby/moby/client"
	iimage "github.com/moby/moby/v2/integration/internal/image"
	"github.com/moby/moby/v2/internal/testutil/request"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// TestImageAttestations exercises GET /images/{name}/attestations against a
// live daemon.
func TestImageAttestations(t *testing.T) {
	skip.If(t, !testEnv.UsingSnapshotter(), "attestation manifests require the containerd image store")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	hostPlatform := platforms.DefaultSpec()
	const imageRef = "attestation-test:latest"
	const provenanceType = "https://slsa.dev/provenance/v0.2"
	const sbomType = "https://spdx.dev/Document"

	// Load an image that has both a SLSA provenance and an SPDX SBOM attestation.
	iimage.Load(ctx, t, apiClient, func(dir string) (*ocispec.Index, error) {
		return buildAttestationImage(t, dir, imageRef, hostPlatform, []statementLayer{
			{
				predicateType: provenanceType,
				payload: mustMarshal(map[string]any{
					"_type":         "https://in-toto.io/Statement/v0.1",
					"predicateType": provenanceType,
					"subject":       []map[string]any{{"name": imageRef}},
					"predicate": map[string]any{
						"builder":   map[string]any{"id": "https://github.com/moby/buildkit"},
						"buildType": "https://mobyproject.org/buildkit@v1",
					},
				}),
			},
			{
				predicateType: sbomType,
				payload: mustMarshal(map[string]any{
					"_type":         "https://in-toto.io/Statement/v0.1",
					"predicateType": sbomType,
					"subject":       []map[string]any{{"name": imageRef}},
					"predicate":     map[string]any{"spdxVersion": "SPDX-2.3", "name": imageRef},
				}),
			},
		})
	})
	defer func() {
		_, _ = apiClient.ImageRemove(ctx, imageRef, client.ImageRemoveOptions{Force: true})
	}()

	t.Run("default_omits_statement_body", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef)
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 2))

		types := map[string]bool{}
		for _, s := range result.Items {
			types[s.PredicateType] = true
			assert.Check(t, s.Statement == nil, "statement body should be omitted by default")
			assert.Check(t, s.Descriptor.Digest != "", "statement digest must not be empty")
			assert.Check(t, s.Descriptor.MediaType != "", "statement media type must not be empty")
		}
		assert.Check(t, types[provenanceType], "provenance statement not returned")
		assert.Check(t, types[sbomType], "SBOM statement not returned")
	})

	t.Run("with_statement_returns_bodies", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithStatement())
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 2))

		types := map[string]bool{}
		for _, s := range result.Items {
			types[s.PredicateType] = true
			assert.Assert(t, s.Statement != nil, "statement body should be present when opted in")
			var raw map[string]any
			assert.NilError(t, json.Unmarshal(*s.Statement, &raw), "statement is not valid JSON")
			assert.Check(t, s.Descriptor.Digest != "", "statement digest must not be empty")
			assert.Check(t, s.Descriptor.MediaType != "", "statement media type must not be empty")
		}
		assert.Check(t, types[provenanceType], "provenance statement not returned")
		assert.Check(t, types[sbomType], "SBOM statement not returned")
	})

	t.Run("filter_by_predicate_type", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithPredicateTypes(provenanceType))
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 1))
		assert.Check(t, is.Equal(result.Items[0].PredicateType, provenanceType))
	})

	t.Run("filter_by_multiple_predicate_types", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithPredicateTypes(provenanceType, sbomType))
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 2))

		types := map[string]bool{}
		for _, s := range result.Items {
			types[s.PredicateType] = true
		}
		assert.Check(t, types[provenanceType], "provenance statement missing from multi-type filter result")
		assert.Check(t, types[sbomType], "SBOM statement missing from multi-type filter result")
	})

	t.Run("explicit_platform_matches", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithPlatform(hostPlatform))
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 2))
	})

	t.Run("wrong_platform_returns_not_found", func(t *testing.T) {
		_, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithPlatform(ocispec.Platform{OS: "linux", Architecture: "riscv64"}))
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})

	t.Run("multiple_platforms_returns_invalid_parameter", func(t *testing.T) {
		// The high-level client only sends a single platform, so build the
		// request by hand to exercise the multi-value rejection path.
		p1, err := json.Marshal(ocispec.Platform{OS: "linux", Architecture: "amd64"})
		assert.NilError(t, err)
		p2, err := json.Marshal(ocispec.Platform{OS: "linux", Architecture: "arm64"})
		assert.NilError(t, err)

		q := url.Values{}
		q.Add("platform", string(p1))
		q.Add("platform", string(p2))

		endpoint := "/v" + testEnv.DaemonAPIVersion() + "/images/" + imageRef + "/attestations?" + q.Encode()
		resp, body, err := request.Get(ctx, endpoint, request.JSON)
		assert.NilError(t, err)
		assert.Equal(t, resp.StatusCode, http.StatusBadRequest)

		buf, err := request.ReadBody(body)
		assert.NilError(t, err)
		assert.Check(t, strings.Contains(string(buf), "only one platform value is currently supported"), string(buf))
	})

	t.Run("unknown_image_returns_not_found", func(t *testing.T) {
		_, err := apiClient.ImageAttestations(ctx, "no-such-image:latest")
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})

	t.Run("empty_filter_returns_all", func(t *testing.T) {
		result, err := apiClient.ImageAttestations(ctx, imageRef,
			client.ImageAttestationsWithPredicateTypes())
		assert.NilError(t, err)
		assert.Assert(t, is.Len(result.Items, 2))
	})
}

// statementLayer is a single in-toto statement to embed in the attestation manifest.
type statementLayer struct {
	predicateType string
	payload       []byte
}

// buildAttestationImage writes an OCI image layout to dir containing a minimal
// platform image manifest and an attestation manifest pointing to it.
func buildAttestationImage(t *testing.T, dir string, imageRef string, platform ocispec.Platform, stmts []statementLayer) (*ocispec.Index, error) {
	t.Helper()

	ref, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return nil, err
	}

	// Minimal platform image: one layer + config.
	layerDesc := writeBlob(t, dir, "application/vnd.oci.image.layer.v1.tar+gzip", []byte("layer"))
	configDesc := writeJSON(t, dir, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: platform,
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layerDesc.Digest}},
	})
	imgMfstDesc := writeJSON(t, dir, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	})
	imgMfstDesc.Platform = &platform

	// Attestation manifest.
	var layerDescs []ocispec.Descriptor
	for _, s := range stmts {
		d := writeBlob(t, dir, "application/vnd.in-toto+json", s.payload)
		d.Annotations = map[string]string{"in-toto.io/predicate-type": s.predicateType}
		layerDescs = append(layerDescs, d)
	}
	attConfigDesc := writeJSON(t, dir, ocispec.MediaTypeImageConfig, struct{}{})
	attMfstDesc := writeJSON(t, dir, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    attConfigDesc,
		Layers:    layerDescs,
	})
	attMfstDesc.Annotations = map[string]string{
		attestation.DockerAnnotationReferenceType:   attestation.DockerAnnotationReferenceTypeDefault,
		attestation.DockerAnnotationReferenceDigest: imgMfstDesc.Digest.String(),
	}
	attMfstDesc.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}

	// Outer index.
	innerIdx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{imgMfstDesc, attMfstDesc},
	}
	innerIdxDesc := writeJSON(t, dir, ocispec.MediaTypeImageIndex, innerIdx)
	innerIdxDesc.Annotations = map[string]string{
		c8dimages.AnnotationImageName: ref.String(),
		ocispec.AnnotationRefName:     ref.(reference.Tagged).Tag(),
	}

	outerIdx := &ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{innerIdxDesc},
	}
	b, err := json.Marshal(outerIdx)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return nil, err
	}
	return outerIdx, nil
}

func writeBlob(t *testing.T, dir, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	dgst := digest.FromBytes(data)
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	assert.NilError(t, os.MkdirAll(blobsDir, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(blobsDir, dgst.Encoded()), data, 0o644))
	return ocispec.Descriptor{MediaType: mediaType, Digest: dgst, Size: int64(len(data))}
}

func writeJSON(t *testing.T, dir, mediaType string, v any) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(v)
	assert.NilError(t, err)
	return writeBlob(t, dir, mediaType, b)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

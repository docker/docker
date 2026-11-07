package metadata

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"reflect"
	"testing"

	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
)

func TestV2MetadataHMAC(t *testing.T) {
	key := []byte("auth-config")
	meta := V2Metadata{
		Digest:           digest.Digest("sha256:layer"),
		SourceRepository: "registry.example/repo",
	}

	// Metadata persisted before MediaType was added must remain valid.
	legacyMAC := hmac.New(sha256.New, key)
	legacyMAC.Write([]byte(meta.Digest))
	legacyMAC.Write([]byte(meta.SourceRepository))
	meta.HMAC = hex.EncodeToString(legacyMAC.Sum(nil))
	if !CheckV2MetadataHMAC(&meta, key) {
		t.Fatal("legacy metadata HMAC was rejected")
	}

	meta.MediaType = "application/vnd.oci.image.layer.v1.tar+zstd"
	meta.HMAC = ComputeV2MetadataHMAC(key, &meta)
	if !CheckV2MetadataHMAC(&meta, key) {
		t.Fatal("metadata HMAC with media type was rejected")
	}
	meta.MediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
	if CheckV2MetadataHMAC(&meta, key) {
		t.Fatal("metadata HMAC accepted a changed media type")
	}
}

func TestV2MetadataService(t *testing.T) {
	tmpDir := t.TempDir()
	metadataStore, err := NewFSMetadataStore(tmpDir)
	if err != nil {
		t.Fatalf("could not create metadata store: %v", err)
	}
	metadataService := NewV2MetadataService(metadataStore)

	tooManyBlobSums := make([]V2Metadata, 100)
	for i := range tooManyBlobSums {
		randDigest := randomDigest()
		tooManyBlobSums[i] = V2Metadata{Digest: randDigest}
	}

	testVectors := []struct {
		diffID   layer.DiffID
		metadata []V2Metadata
	}{
		{
			diffID: layer.DiffID("sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4"),
			metadata: []V2Metadata{
				{
					Digest:    digest.Digest("sha256:f0cd5ca10b07f35512fc2f1cbf9a6cefbdb5cba70ac6b0c9e5988f4497f71937"),
					MediaType: "application/vnd.oci.image.layer.v1.tar+zstd",
				},
			},
		},
		{
			diffID: layer.DiffID("sha256:86e0e091d0da6bde2456dbb48306f3956bbeb2eae1b5b9a43045843f69fe4aaa"),
			metadata: []V2Metadata{
				{Digest: digest.Digest("sha256:f0cd5ca10b07f35512fc2f1cbf9a6cefbdb5cba70ac6b0c9e5988f4497f71937")},
				{Digest: digest.Digest("sha256:9e3447ca24cb96d86ebd5960cb34d1299b07e0a0e03801d90b9969a2c187dd6e")},
			},
		},
		{
			diffID:   layer.DiffID("sha256:03f4658f8b782e12230c1783426bd3bacce651ce582a4ffb6fbbfa2079428ecb"),
			metadata: tooManyBlobSums,
		},
	}

	// Set some associations
	for _, vec := range testVectors {
		for _, blobsum := range vec.metadata {
			err := metadataService.Add(vec.diffID, blobsum)
			if err != nil {
				t.Fatalf("error calling Set: %v", err)
			}
		}
	}

	// Check the correct values are read back
	for _, vec := range testVectors {
		metadata, err := metadataService.GetMetadata(vec.diffID)
		if err != nil {
			t.Fatalf("error calling Get: %v", err)
		}
		expectedMetadataEntries := min(len(vec.metadata), 50)
		if !reflect.DeepEqual(metadata, vec.metadata[len(vec.metadata)-expectedMetadataEntries:len(vec.metadata)]) {
			t.Fatal("Get returned incorrect layer ID")
		}
	}

	// Test GetMetadata on a nonexistent entry
	_, err = metadataService.GetMetadata("sha256:82379823067823853223359023576437723560923756b03560378f4497753917")
	if err == nil {
		t.Fatal("expected error looking up nonexistent entry")
	}

	// Test GetDiffID on a nonexistent entry
	_, err = metadataService.GetDiffID("sha256:82379823067823853223359023576437723560923756b03560378f4497753917")
	if err == nil {
		t.Fatal("expected error looking up nonexistent entry")
	}

	// Overwrite one of the entries and read it back
	err = metadataService.Add(testVectors[1].diffID, testVectors[0].metadata[0])
	if err != nil {
		t.Fatalf("error calling Add: %v", err)
	}
	diffID, err := metadataService.GetDiffID(testVectors[0].metadata[0].Digest)
	if err != nil {
		t.Fatalf("error calling GetDiffID: %v", err)
	}
	if diffID != testVectors[1].diffID {
		t.Fatal("GetDiffID returned incorrect diffID")
	}
}

func randomDigest() digest.Digest {
	b := [32]byte{}
	for i := range len(b) {
		b[i] = byte(rand.Intn(256))
	}
	d := hex.EncodeToString(b[:])
	return digest.Digest("sha256:" + d)
}

package images

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/archive/tartest"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ingest func(context.Context, content.Store) error

type construct func(*ocispec.Descriptor) ingest

type configOpt func(*ocispec.Image)

type manifestOpt func(*ocispec.Manifest) ingest

func nilIngest(context.Context, content.Store) error {
	return nil
}

func errIngest(err error) ingest {
	return func(context.Context, content.Store) error {
		return err
	}
}

func multiIngest(ingests ...ingest) ingest {
	return func(ctx context.Context, i content.Store) error {
		for _, ing := range ingests {
			if err := ing(ctx, i); err != nil {
				return err
			}
		}
		return nil
	}
}

func bytesIngest(p []byte, m string, opts ...content.Opt) ingest {
	desc := ocispec.Descriptor{
		MediaType: m,
		Digest:    digest.FromBytes(p),
		Size:      int64(len(p)),
	}

	return func(ctx context.Context, i content.Store) error {
		return content.WriteBlob(ctx, i, desc.Digest.String(), bytes.NewReader(p), desc, opts...)
	}
}

func withRootFS(diffIDs ...digest.Digest) configOpt {
	return func(i *ocispec.Image) {
		i.RootFS.Type = "layers"
		i.RootFS.DiffIDs = diffIDs
	}
}

func withConfig(opts ...configOpt) manifestOpt {
	return func(m *ocispec.Manifest) ingest {
		var diffIDs []digest.Digest
		for _, l := range m.Layers {
			if l.Annotations != nil {
				if uncompressed, ok := l.Annotations["uncompressed"]; ok {
					diffIDs = append(diffIDs, digest.Digest(uncompressed))
				}
			}
		}
		// Add at beginning so any overriding RootFS is used
		newopts := append([]configOpt{}, withRootFS(diffIDs...))

		return createConfig(append(newopts, opts...)...)(&m.Config)
	}
}

// withLayers creates all the layers and adds them to the manifest
func withLayers(layers ...tartest.WriterToTar) manifestOpt {
	return func(m *ocispec.Manifest) ingest {
		var ingests []ingest
		for _, l := range layers {
			br := bytes.NewBuffer(nil)
			dgstr := digest.Canonical.Digester()
			cw, err := compression.CompressStream(br, compression.Gzip)
			if err != nil {
				return errIngest(err)
			}
			r := io.TeeReader(tartest.TarFromWriterTo(l), dgstr.Hash())
			if _, err := io.Copy(cw, r); err != nil {
				return errIngest(err)
			}
			cw.Close()
			p := br.Bytes()
			desc := ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageLayerGzip,
				Digest:    digest.FromBytes(p),
				Size:      int64(len(p)),
				Annotations: map[string]string{
					"uncompressed": dgstr.Digest().String(),
				},
			}
			ingests = append(ingests, bytesIngest(p, desc.MediaType))
			m.Layers = append(m.Layers, desc)
		}

		return multiIngest(ingests...)
	}
}

func createConfig(opts ...configOpt) construct {
	p := platforms.DefaultSpec()
	config := ocispec.Image{
		OS:           p.OS,
		Architecture: p.Architecture,
	}
	for _, opt := range opts {
		opt(&config)
	}
	return func(desc *ocispec.Descriptor) ingest {
		p, err := json.Marshal(config)
		if err != nil {
			return errIngest(err)
		}
		*desc = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.FromBytes(p),
			Size:      int64(len(p)),
		}
		return bytesIngest(p, desc.MediaType)

	}
}

func createManifest(opts ...manifestOpt) construct {
	var m ocispec.Manifest
	var ingests []ingest
	for _, opt := range opts {
		ingests = append(ingests, opt(&m))
	}

	// strip annotations to match existing Docker behavior
	// TODO(containerd): consider this as optional
	for i := range m.Layers {
		m.Layers[i].Annotations = nil
	}

	return func(desc *ocispec.Descriptor) ingest {
		p, err := json.Marshal(m)
		if err != nil {
			return errIngest(err)
		}
		*desc = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromBytes(p),
			Size:      int64(len(p)),
		}
		labels := map[string]string{
			"containerd.io/gc.ref.content.config": m.Config.Digest.String(),
		}
		for i, l := range m.Layers {
			labels[fmt.Sprintf("containerd.io/gc.ref.content.l%d", i)] = l.Digest.String()
		}
		return multiIngest(append(ingests, bytesIngest(p, desc.MediaType, content.WithLabels(labels)))...)

	}
}

// TODO(containerd): find a way to add annotations...
func createIndex(references ...construct) construct {
	idx := ocispec.Index{
		Manifests: make([]ocispec.Descriptor, len(references)),
	}
	var ingests []ingest
	for i, ref := range references {
		ingests = append(ingests, ref(&idx.Manifests[i]))
	}

	return func(desc *ocispec.Descriptor) ingest {
		p, err := json.Marshal(idx)
		if err != nil {
			return errIngest(err)
		}
		*desc = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageIndex,
			Digest:    digest.FromBytes(p),
			Size:      int64(len(p)),
		}
		labels := map[string]string{}
		for i, m := range idx.Manifests {
			labels[fmt.Sprintf("containerd.io/gc.ref.content.m%d", i)] = m.Digest.String()
		}
		return multiIngest(append(ingests, bytesIngest(p, desc.MediaType, content.WithLabels(labels)))...)

	}
}

func randomLayer(size int) tartest.WriterToTar {
	now := time.Now()
	tc := tartest.TarContext{}.WithModTime(now.UTC())
	r := rand.New(rand.NewSource(now.UnixNano()))
	p := make([]byte, size)
	if l, err := r.Read(p); err != nil || l != size {
		panic(fmt.Sprintf("unable to read rand bytes: %d %v", l, err))
	}
	return tartest.TarAll(
		tc.Dir("/randomfiles", 0755),
		tc.File("/randomfiles/1", p, 0644),
	)
}

func randomManifest(layers int) construct {
	layerOpts := make([]tartest.WriterToTar, layers)
	for i := range layerOpts {
		layerOpts[i] = randomLayer(64 + 10*i)
	}
	return createManifest(withLayers(layerOpts...), withConfig())
}

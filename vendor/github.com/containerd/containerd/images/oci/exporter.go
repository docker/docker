/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package oci

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"sort"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	ocispecs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// V1Exporter implements OCI Image Spec v1.
// It is up to caller to put "org.opencontainers.image.ref.name" annotation to desc.
//
// TODO(AkihiroSuda): add V1Exporter{TranslateMediaTypes: true} that transforms media types,
//                    e.g. application/vnd.docker.image.rootfs.diff.tar.gzip
//                         -> application/vnd.oci.image.layer.v1.tar+gzip
type V1Exporter struct {
	AllPlatforms bool
}

// V1ExporterOpt allows the caller to set additional options to a new V1Exporter
type V1ExporterOpt func(c *V1Exporter) error

// DefaultV1Exporter return a default V1Exporter pointer
func DefaultV1Exporter() *V1Exporter {
	return &V1Exporter{
		AllPlatforms: false,
	}
}

// ResolveV1ExportOpt return a new V1Exporter with V1ExporterOpt
func ResolveV1ExportOpt(opts ...V1ExporterOpt) (*V1Exporter, error) {
	exporter := DefaultV1Exporter()
	for _, o := range opts {
		if err := o(exporter); err != nil {
			return exporter, err
		}
	}
	return exporter, nil
}

// WithAllPlatforms set V1Exporter`s AllPlatforms option
func WithAllPlatforms(allPlatforms bool) V1ExporterOpt {
	return func(c *V1Exporter) error {
		c.AllPlatforms = allPlatforms
		return nil
	}
}

// Export implements Exporter.
func (oe *V1Exporter) Export(ctx context.Context, store content.Provider, desc ocispec.Descriptor, writer io.Writer) error {
	tw := tar.NewWriter(writer)
	defer tw.Close()

	records := []tarRecord{
		ociLayoutFile(""),
		ociIndexRecord(desc),
	}

	algorithms := map[string]struct{}{}
	exportHandler := func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		records = append(records, blobRecord(store, desc))
		algorithms[desc.Digest.Algorithm().String()] = struct{}{}
		return nil, nil
	}

	childrenHandler := images.ChildrenHandler(store)

	if !oe.AllPlatforms {
		// get local default platform to fetch image manifest
		childrenHandler = images.FilterPlatforms(childrenHandler, platforms.Any(platforms.DefaultSpec()))
	}

	handlers := images.Handlers(
		childrenHandler,
		images.HandlerFunc(exportHandler),
	)

	// Walk sequentially since the number of fetchs is likely one and doing in
	// parallel requires locking the export handler
	if err := images.Walk(ctx, handlers, desc); err != nil {
		return err
	}

	if len(algorithms) > 0 {
		records = append(records, directoryRecord("blobs/", 0755))
		for alg := range algorithms {
			records = append(records, directoryRecord("blobs/"+alg+"/", 0755))
		}
	}

	return writeTar(ctx, tw, records)
}

type tarRecord struct {
	Header *tar.Header
	CopyTo func(context.Context, io.Writer) (int64, error)
}

func blobRecord(cs content.Provider, desc ocispec.Descriptor) tarRecord {
	path := "blobs/" + desc.Digest.Algorithm().String() + "/" + desc.Digest.Hex()
	return tarRecord{
		Header: &tar.Header{
			Name:     path,
			Mode:     0444,
			Size:     desc.Size,
			Typeflag: tar.TypeReg,
		},
		CopyTo: func(ctx context.Context, w io.Writer) (int64, error) {
			r, err := cs.ReaderAt(ctx, desc)
			if err != nil {
				return 0, errors.Wrap(err, "failed to get reader")
			}
			defer r.Close()

			// Verify digest
			dgstr := desc.Digest.Algorithm().Digester()

			n, err := io.Copy(io.MultiWriter(w, dgstr.Hash()), content.NewReader(r))
			if err != nil {
				return 0, errors.Wrap(err, "failed to copy to tar")
			}
			if dgstr.Digest() != desc.Digest {
				return 0, errors.Errorf("unexpected digest %s copied", dgstr.Digest())
			}
			return n, nil
		},
	}
}

func directoryRecord(name string, mode int64) tarRecord {
	return tarRecord{
		Header: &tar.Header{
			Name:     name,
			Mode:     mode,
			Typeflag: tar.TypeDir,
		},
	}
}

func ociLayoutFile(version string) tarRecord {
	if version == "" {
		version = ocispec.ImageLayoutVersion
	}
	layout := ocispec.ImageLayout{
		Version: version,
	}

	b, err := json.Marshal(layout)
	if err != nil {
		panic(err)
	}

	return tarRecord{
		Header: &tar.Header{
			Name:     ocispec.ImageLayoutFile,
			Mode:     0444,
			Size:     int64(len(b)),
			Typeflag: tar.TypeReg,
		},
		CopyTo: func(ctx context.Context, w io.Writer) (int64, error) {
			n, err := w.Write(b)
			return int64(n), err
		},
	}

}

func ociIndexRecord(manifests ...ocispec.Descriptor) tarRecord {
	index := ocispec.Index{
		Versioned: ocispecs.Versioned{
			SchemaVersion: 2,
		},
		Manifests: manifests,
	}

	b, err := json.Marshal(index)
	if err != nil {
		panic(err)
	}

	return tarRecord{
		Header: &tar.Header{
			Name:     "index.json",
			Mode:     0644,
			Size:     int64(len(b)),
			Typeflag: tar.TypeReg,
		},
		CopyTo: func(ctx context.Context, w io.Writer) (int64, error) {
			n, err := w.Write(b)
			return int64(n), err
		},
	}
}

func writeTar(ctx context.Context, tw *tar.Writer, records []tarRecord) error {
	sort.Slice(records, func(i, j int) bool {
		return records[i].Header.Name < records[j].Header.Name
	})

	for _, record := range records {
		if err := tw.WriteHeader(record.Header); err != nil {
			return err
		}
		if record.CopyTo != nil {
			n, err := record.CopyTo(ctx, tw)
			if err != nil {
				return err
			}
			if n != record.Header.Size {
				return errors.Errorf("unexpected copy size for %s", record.Header.Name)
			}
		} else if record.Header.Size > 0 {
			return errors.Errorf("no content to write to record with non-zero size for %s", record.Header.Name)
		}
	}
	return nil
}

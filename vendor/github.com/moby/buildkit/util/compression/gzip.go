package compression

import (
	"context"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

func (c gzipType) Compress(ctx context.Context, comp Config) (compressorFunc Compressor, finalize Finalizer) {
	return func(dest io.Writer, _ string) (io.WriteCloser, error) {
		return gzipWriter(comp)(dest)
	}, nil
}

func (c gzipType) Decompress(ctx context.Context, cs content.Store, desc ocispecs.Descriptor) (io.ReadCloser, error) {
	return decompress(ctx, cs, desc)
}

func (c gzipType) NeedsConversion(ctx context.Context, cs content.Store, desc ocispecs.Descriptor) (bool, error) {
	esgz, err := EStargz.Is(ctx, cs, desc.Digest)
	if err != nil {
		return false, err
	}
	if !images.IsLayerType(desc.MediaType) {
		return false, nil
	}
	ct, err := FromMediaType(desc.MediaType)
	if err != nil {
		return false, err
	}
	if ct == Gzip && !esgz {
		return false, nil
	}
	return true, nil
}

func (c gzipType) NeedsComputeDiffBySelf(comp Config) bool {
	// we allow compressing it with a customized compression level that containerd differ doesn't support so we compress it by self.
	return comp.Level != nil
}

func (c gzipType) OnlySupportOCITypes() bool {
	return false
}

func (c gzipType) MediaType() string {
	return ocispecs.MediaTypeImageLayerGzip
}

func (c gzipType) String() string {
	return "gzip"
}

func gzipWriter(comp Config) func(io.Writer) (io.WriteCloser, error) {
	return func(dest io.Writer) (io.WriteCloser, error) {
		level := go126GzipDefaultCompression
		if comp.Level != nil {
			level = *comp.Level
		}
		// Go 1.27 changed compress/flate output in CL 707355 for
		// golang/go#75532. Keep Go 1.26 encoding so existing compatibility
		// versions continue to produce the same compressed layer digests.
		return newGo126GzipWriterLevel(dest, level)
	}
}

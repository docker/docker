package mobyexporter

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/containerimage"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Differ can make a moby layer from a snapshot
type Differ interface {
	EnsureLayer(ctx context.Context, key string) ([]layer.DiffID, error)
}

type ImageTagger interface {
	TagImage(ctx context.Context, imageID image.ID, newTag reference.Named) error
}

// Opt defines a struct for creating new exporter
type Opt struct {
	ImageStore            image.Store
	Differ                Differ
	ImageTagger           ImageTagger
	ContentStore          content.Store
	LeaseManager          leases.Manager
	ImageExportedCallback func(ctx context.Context, id string, desc ocispec.Descriptor)
}

type imageExporter struct {
	opt Opt
}

// New creates a new moby imagestore exporter
func New(opt Opt) (exporter.Exporter, error) {
	im := &imageExporter{opt: opt}
	return im, nil
}

func (e *imageExporter) Resolve(ctx context.Context, id int, attrs map[string]string) (exporter.ExporterInstance, error) {
	i := &imageExporterInstance{
		imageExporter: e,
		id:            id,
		attrs:         attrs,
	}
	for k, v := range attrs {
		if ak, ok, err := exptypes.ParseAnnotationKey(k); ok && err == nil {
			switch ak.Type {
			case exptypes.AnnotationIndex, exptypes.AnnotationIndexDescriptor:
				return nil, errors.New("index annotations not supported for single platform export")
			}
		}
		switch exptypes.ImageExporterOptKey(k) {
		case exptypes.OptKeyName:
			for v := range strings.SplitSeq(v, ",") {
				ref, err := reference.ParseNormalizedNamed(v)
				if err != nil {
					return nil, err
				}
				i.targetNames = append(i.targetNames, ref)
			}
		default:
			if i.meta == nil {
				i.meta = make(map[string][]byte)
			}
			i.meta[k] = []byte(v)
		}
	}
	return i, nil
}

type imageExporterInstance struct {
	*imageExporter
	id          int
	targetNames []reference.Named
	meta        map[string][]byte
	attrs       map[string]string
}

func (e *imageExporterInstance) ID() int {
	return e.id
}

func (e *imageExporterInstance) Type() string {
	return "image"
}

func (e *imageExporterInstance) Name() string {
	return "exporting to image"
}

func (e *imageExporterInstance) Config() *exporter.Config {
	return exporter.NewConfig()
}

func (e *imageExporterInstance) Attrs() map[string]string {
	return e.attrs
}

func (e *imageExporterInstance) Export(ctx context.Context, inp *exporter.Source, buildInfo exporter.ExportBuildInfo) (map[string]string, exporter.FinalizeFunc, exporter.DescriptorReference, error) {
	if len(inp.Refs) > 1 {
		return nil, nil, nil, errors.New("exporting multiple references to image store is currently unsupported")
	}

	ref := inp.Ref
	if ref != nil && len(inp.Refs) == 1 {
		return nil, nil, nil, errors.New("invalid exporter input: Ref and Refs are mutually exclusive")
	}

	// only one loop
	for _, v := range inp.Refs {
		ref = v
	}

	var config []byte
	switch len(inp.Refs) {
	case 0:
		config = inp.Metadata[exptypes.ExporterImageConfigKey]
	case 1:
		ps, err := exptypes.ParsePlatforms(inp.Metadata)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot export image, failed to parse platforms: %w", err)
		}
		if len(ps.Platforms) != len(inp.Refs) {
			return nil, nil, nil, errors.Errorf("number of platforms does not match references %d %d", len(ps.Platforms), len(inp.Refs))
		}
		config = inp.Metadata[fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, ps.Platforms[0].ID)]
	}

	var diffs []digest.Digest
	if ref != nil {
		layersDone := progress.OneOff(ctx, "exporting layers")

		if err := ref.Finalize(ctx); err != nil {
			return nil, nil, nil, layersDone(err)
		}

		if err := ref.Extract(ctx, nil); err != nil {
			return nil, nil, nil, err
		}

		diffIDs, err := e.opt.Differ.EnsureLayer(ctx, ref.ID())
		if err != nil {
			return nil, nil, nil, layersDone(err)
		}

		diffs = slices.Clone(diffIDs)

		_ = layersDone(nil)
	}

	if len(config) == 0 {
		var err error
		config, err = emptyImageConfig()
		if err != nil {
			return nil, nil, nil, err
		}
	}

	history, err := parseHistoryFromConfig(config)
	if err != nil {
		return nil, nil, nil, err
	}

	diffs, history = normalizeLayersAndHistory(diffs, history, ref)

	var inlineCacheEntry *exptypes.InlineCacheEntry
	if buildInfo.InlineCache != nil {
		inlineCacheResult, err := buildInfo.InlineCache(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		if inlineCacheResult != nil {
			if ref != nil {
				inlineCacheEntry, _ = inlineCacheResult.FindRef(ref.ID())
			} else {
				inlineCacheEntry = inlineCacheResult.Ref
			}
		}
	}
	config, err = patchImageConfig(config, diffs, history, inlineCacheEntry)
	if err != nil {
		return nil, nil, nil, err
	}

	configDigest := digest.FromBytes(config)

	configDone := progress.OneOff(ctx, fmt.Sprintf("writing image %s", configDigest))
	id, err := e.opt.ImageStore.Create(config)
	if err != nil {
		return nil, nil, nil, configDone(err)
	}
	_ = configDone(nil)

	var names []string
	for _, targetName := range e.targetNames {
		names = append(names, targetName.String())
		if e.opt.ImageTagger != nil {
			tagDone := progress.OneOff(ctx, "naming to "+targetName.String())
			if err := e.opt.ImageTagger.TagImage(ctx, image.ID(digest.Digest(id)), targetName); err != nil {
				return nil, nil, nil, tagDone(err)
			}
			_ = tagDone(nil)
		}
	}

	resp := map[string]string{
		exptypes.ExporterImageConfigDigestKey: configDigest.String(),
		exptypes.ExporterImageDigestKey:       id.String(),
	}
	if len(names) > 0 {
		resp["image.name"] = strings.Join(names, ",")
	}

	descRef, err := e.newTempReference(ctx, config)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create a temporary descriptor reference: %w", err)
	}

	if e.opt.ImageExportedCallback != nil {
		e.opt.ImageExportedCallback(ctx, id.String(), descRef.Descriptor())
	}

	return resp, nil, descRef, nil
}

func (e *imageExporterInstance) newTempReference(ctx context.Context, config []byte) (exporter.DescriptorReference, error) {
	lm := e.opt.LeaseManager

	dgst := digest.FromBytes(config)
	leaseCtx, done, err := leaseutil.WithLease(ctx, lm, leaseutil.MakeTemporary)
	if err != nil {
		return nil, err
	}

	unlease := func(ctx context.Context) error {
		err := done(context.WithoutCancel(ctx))
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to delete descriptor reference lease")
		}
		return err
	}

	desc := ocispec.Descriptor{
		Digest:    dgst,
		MediaType: ocispec.MediaTypeImageConfig,
		Size:      int64(len(config)),
	}

	if err := content.WriteBlob(leaseCtx, e.opt.ContentStore, desc.Digest.String(), bytes.NewReader(config), desc); err != nil {
		unlease(leaseCtx)
		return nil, fmt.Errorf("failed to save temporary image config: %w", err)
	}

	return containerimage.NewDescriptorReference(desc, unlease), nil
}

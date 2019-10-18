package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/content"
	cerrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/layer"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ImageHistory returns a slice of ImageHistory structures for the specified image
// name by walking the image lineage.
func (i *ImageService) ImageHistory(ctx context.Context, name string) ([]*image.HistoryResponseItem, error) {
	start := time.Now()
	desc, err := i.ResolveImage(ctx, name)
	if err != nil {
		return nil, err
	}

	cs := i.client.ContentStore()
	m, err := images.Manifest(ctx, cs, desc, i.platforms)
	if err != nil {
		return nil, err
	}

	p, err := content.ReadBlob(ctx, cs, m.Config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read config")
	}

	var img ocispec.Image
	if err := json.Unmarshal(p, &img); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal config")
	}

	history := []*image.HistoryResponseItem{}

	layerCounter := 0
	rootFS := img.RootFS
	rootFS.DiffIDs = nil

	info, err := cs.Info(ctx, m.Config.Digest)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get config %s", m.Config.Digest.String())
	}
	var ls layer.Store
	for k := range info.Labels {
		if strings.HasPrefix(k, LabelLayerPrefix) {
			name := k[len(LabelLayerPrefix):]
			ils, ok := i.layerStores[name]
			if ok {
				ls = ils
				break
			}
		}
	}

	for _, h := range img.History {
		var layerSize int64

		if !h.EmptyLayer {
			if len(img.RootFS.DiffIDs) <= layerCounter {
				return nil, fmt.Errorf("too many non-empty layers in History section")
			}
			rootFS.DiffIDs = append(rootFS.DiffIDs, img.RootFS.DiffIDs[layerCounter])

			layerCounter++

			if ls != nil {
				l, err := ls.Get(layer.ChainID(identity.ChainID(rootFS.DiffIDs)))
				if err != nil {
					return nil, err
				}
				layerSize, err = l.DiffSize()
				layer.ReleaseAndLog(ls, l)
				if err != nil {
					return nil, err
				}
			}

		}

		history = append([]*image.HistoryResponseItem{{
			ID:        "<missing>",
			Created:   h.Created.Unix(),
			CreatedBy: h.CreatedBy,
			Comment:   h.Comment,
			Size:      layerSize,
		}}, history...)
	}

	//// Fill in image IDs
	id := desc.Digest
	for _, h := range history {
		// TODO(containerd): is it ok that parent IDs may not match images
		h.ID = id.String()

		// TODO(containerd): fill in tags or just ignore
		//	var tags []string
		//	for _, r := range histImg.references {
		//		if _, ok := r.(reference.NamedTagged); ok {
		//			tags = append(tags, reference.FamiliarString(r))
		//		}
		//	}

		//	h.Tags = tags

		parent := info.Labels[LabelImageParent]
		if parent == "" {
			break
		}
		info, err = cs.Info(ctx, m.Config.Digest)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				break
			}
			return nil, errors.Wrapf(err, "unable to get parent config %s", parent)
		}
	}
	imageActions.WithValues("history").UpdateSince(start)
	return history, nil
}

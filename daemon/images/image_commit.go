package images // import "github.com/docker/docker/daemon/images"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	cerrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// CommitImage creates a new image from a commit config
func (i *ImageService) CommitImage(ctx context.Context, c backend.CommitConfig) (ocispec.Descriptor, error) {
	ctx, done, err := i.client.WithLease(ctx)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	defer func() {
		if err := done(context.Background()); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to remove lease")
		}
	}()

	var img struct {
		ocispec.Image

		// Overwrite config for custom Docker fields
		Container       string            `json:"container,omitempty"`
		ContainerConfig container.Config  `json:"container_config,omitempty"`
		Config          *container.Config `json:"config,omitempty"`

		Comment    string   `json:"comment,omitempty"`
		OSVersion  string   `json:"os.version,omitempty"`
		OSFeatures []string `json:"os.features,omitempty"`
		Variant    string   `json:"variant,omitempty"`
		// TODO: Overwrite this with a label from config
		DockerVersion string `json:"docker_version,omitempty"`
	}

	var parentID string
	if c.ParentImage == nil {
		img.RootFS.Type = "layers"
	} else {
		ri, err := i.ResolveRuntimeImage(ctx, *c.ParentImage)
		if err != nil {
			return ocispec.Descriptor{}, errors.Wrap(err, "unable to resolve parent runtime image")
		}

		if err := json.Unmarshal(ri.ConfigBytes, &img); err != nil {
			return ocispec.Descriptor{}, errors.Wrap(err, "failed to unmarshal config")
		}
		parentID = ri.Config.Digest.String()
	}

	cl, err := i.commitLayer(ctx, identity.ChainID(img.RootFS.DiffIDs), c)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() {
		if cl.layer != nil {
			layer.ReleaseAndLog(cl.store, cl.layer)
		}
	}()

	// Get compressed layer descriptors, migrate is needed
	layers, err := i.compressedLayers(ctx, img.RootFS.DiffIDs)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	// Create and write config
	created := time.Now().UTC()

	img.Created = &created

	isEmptyLayer := layer.IsEmpty(layer.DiffID(cl.uncompressed.Digest))
	if !isEmptyLayer {
		img.RootFS.DiffIDs = append(img.RootFS.DiffIDs, cl.uncompressed.Digest)
		layers = append(layers, cl.compressed)
	}
	img.History = append(img.History, ocispec.History{
		Author:     c.Author,
		Created:    &created,
		CreatedBy:  strings.Join(c.ContainerConfig.Cmd, " "),
		Comment:    c.Comment,
		EmptyLayer: isEmptyLayer,
	})

	img.DockerVersion = dockerversion.Version
	img.Author = c.Author
	img.Comment = c.Comment
	if img.OS == "" {
		img.OS = c.ContainerOS
	}
	img.Container = c.ContainerID
	img.Config = c.Config
	img.ContainerConfig = *c.ContainerConfig

	config, err := json.Marshal(img)
	if err != nil {
		return ocispec.Descriptor{}, errors.Wrap(err, "failed to marshal committed image")
	}

	desc := ocispec.Descriptor{
		MediaType: images.MediaTypeDockerSchema2Config,
		Digest:    digest.FromBytes(config),
		Size:      int64(len(config)),
	}

	labels := map[string]string{}

	if cl.layer != nil {
		key := fmt.Sprintf("%s%s", LabelLayerPrefix, cl.store.DriverName())
		labels[key] = cl.layer.ChainID().String()
	}

	if parentID != "" {
		labels[LabelImageParent] = parentID
	}

	opts := []content.Opt{content.WithLabels(labels)}

	ref := fmt.Sprintf("config-%s-%s", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
	if err := content.WriteBlob(ctx, i.client.ContentStore(), ref, bytes.NewReader(config), desc, opts...); err != nil {
		return ocispec.Descriptor{}, errors.Wrap(err, "unable to store config")
	}

	// Create and write manifest
	m := struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Config        ocispec.Descriptor   `json:"config"`
		Layers        []ocispec.Descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     images.MediaTypeDockerSchema2Manifest,
		Config:        desc,
		Layers:        layers,
	}

	mb, err := json.Marshal(m)
	if err != nil {
		return ocispec.Descriptor{}, errors.Wrap(err, "failed to marshal committed image")
	}

	desc = ocispec.Descriptor{
		MediaType: images.MediaTypeDockerSchema2Manifest,
		Digest:    digest.FromBytes(mb),
		Size:      int64(len(mb)),
	}

	labels = map[string]string{
		"containerd.io/gc.ref.content.config": m.Config.Digest.String(),
	}
	for i, l := range m.Layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.l%d", i)] = l.Digest.String()
	}

	opts = []content.Opt{content.WithLabels(labels)}
	ref = fmt.Sprintf("manifest-%s-%s", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
	if err := content.WriteBlob(ctx, i.client.ContentStore(), ref, bytes.NewReader(mb), desc, opts...); err != nil {
		return ocispec.Descriptor{}, errors.Wrap(err, "unable to store manifest")
	}

	// Create a dangling image
	_, err = i.client.ImageService().Create(ctx, images.Image{
		// TODO(containerd): Add a more meaningful name component?
		Name:      "<commit>@" + desc.Digest.String(),
		Target:    desc,
		CreatedAt: created,
		UpdatedAt: created,
	})
	if err != nil {
		return ocispec.Descriptor{}, errors.Wrap(err, "unable to store image")
	}

	if cl.layer != nil {
		cache, err := i.getCache(ctx)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		cache.m.Lock()
		layerKey := digest.Digest(cl.layer.ChainID())
		if _, ok := cache.layers[cl.store.DriverName()][layerKey]; !ok {
			cache.layers[cl.store.DriverName()][layerKey] = cl.layer
			// Unset this to prevent defer from releasing
			cl.layer = nil
		}
		cache.m.Unlock()
	}

	return desc, nil
}

type committedLayer struct {
	uncompressed ocispec.Descriptor
	compressed   ocispec.Descriptor
	layer        layer.Layer
	store        layer.Store
}

func (i *ImageService) commitLayer(ctx context.Context, parent digest.Digest, c backend.CommitConfig) (committedLayer, error) {
	// TODO(containerd): get driver name from container metadata
	p := platforms.DefaultSpec()
	p.OS = c.ContainerOS
	p.OSVersion = ""
	p.OSFeatures = nil
	layerStore, err := i.getLayerStore(p)
	if err != nil {
		return committedLayer{}, err
	}
	rwTar, err := exportContainerRw(layerStore, c.ContainerID, c.ContainerMountLabel)
	if err != nil {
		return committedLayer{}, err
	}
	defer rwTar.Close()

	cs := i.client.ContentStore()

	// TODO(containerd): Handle unavailable error or use random id?
	w, err := cs.Writer(ctx, content.WithRef("container-commit-"+c.ContainerID))
	if err != nil {
		return committedLayer{}, err
	}
	defer func() {
		if err := w.Close(); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to close writer")
		}
	}()
	if err := w.Truncate(0); err != nil {
		return committedLayer{}, err
	}

	dc, err := compression.CompressStream(w, compression.Gzip)
	if err != nil {
		return committedLayer{}, err
	}

	l, err := layerStore.Register(io.TeeReader(rwTar, dc), layer.ChainID(parent))
	if err != nil {
		return committedLayer{}, err
	}
	dc.Close()

	diffID := digest.Digest(l.DiffID())
	cdgst := w.Digest()
	info, err := w.Status()
	if err != nil {
		return committedLayer{}, err

	}
	size := info.Offset
	if size == 0 {
		return committedLayer{}, errors.New("empty write for layer")
	}

	labels := map[string]string{
		"containerd.io/uncompressed": diffID.String(),
	}

	if err := w.Commit(ctx, size, cdgst, content.WithLabels(labels)); err != nil {
		if !cerrdefs.IsAlreadyExists(err) {
			return committedLayer{}, err
		}

	}

	return committedLayer{
		uncompressed: ocispec.Descriptor{
			MediaType: images.MediaTypeDockerSchema2Layer,
			Digest:    diffID,
			Size:      -1,
		},
		compressed: ocispec.Descriptor{
			MediaType: images.MediaTypeDockerSchema2LayerGzip,
			Digest:    cdgst,
			Size:      size,
		},
		layer: l,
		store: layerStore,
	}, nil
}

func (i *ImageService) compressedLayers(ctx context.Context, diffs []digest.Digest) ([]ocispec.Descriptor, error) {
	var filters []string
	for _, diff := range diffs {
		filters = append(filters, fmt.Sprintf("labels.\"containerd.io/uncompressed\"==%s", diff.String()))
	}
	descs := make([]ocispec.Descriptor, len(diffs))

	i.client.ContentStore().Walk(ctx, func(info content.Info) error {
		udgst := digest.Digest(info.Labels["containerd.io/uncompressed"])
		for i, diff := range diffs {
			if diff == udgst {
				descs[i] = ocispec.Descriptor{
					MediaType: images.MediaTypeDockerSchema2LayerGzip,
					Digest:    info.Digest,
					Size:      info.Size,
				}
			}
		}
		return nil
	}, filters...)

	for j, diff := range diffs {
		if descs[j].Digest != "" {
			continue
		}
		log.G(ctx).WithField("diff", diff).Debugf("compressed blob not found, migrating")

		// Look in all configured layer stores
		for _, store := range i.layerBackends {
			l, err := store.Get(layer.ChainID(identity.ChainID(diffs[:j+1])))
			if err != nil {
				if err == layer.ErrLayerDoesNotExist {
					continue
				}
				return nil, errors.Wrapf(err, "cannot get layer for %s", diff.String())
			}
			defer layer.ReleaseAndLog(store, l)

			cs := i.client.ContentStore()

			// TODO(containerd): Handle unavailable and synchronize
			w, err := cs.Writer(ctx, content.WithRef("layer-migrate-"+diff.String()))
			if err != nil {
				return nil, err
			}
			// Ensure any leftover data is abandoned
			if err := w.Truncate(0); err != nil {
				return nil, err
			}

			dc, err := compression.CompressStream(w, compression.Gzip)
			if err != nil {
				return nil, err
			}

			rc, err := l.TarStream()
			if err != nil {
				return nil, err
			}

			_, err = io.Copy(dc, rc)
			rc.Close()
			if err != nil {
				return nil, err
			}

			dc.Close()

			info, err := w.Status()
			if err != nil {
				return nil, err
			}
			n := info.Offset

			labels := map[string]string{
				"containerd.io/uncompressed": diff.String(),
			}

			cdgst := w.Digest()
			if err := w.Commit(ctx, n, cdgst, content.WithLabels(labels)); err != nil {
				if !cerrdefs.IsAlreadyExists(err) {
					return nil, err
				}
				if err := w.Close(); err != nil {
					log.G(ctx).WithError(err).Errorf("failed to close writer")
				}
			}

			descs[j] = ocispec.Descriptor{
				MediaType: images.MediaTypeDockerSchema2LayerGzip,
				Digest:    cdgst,
				Size:      n,
			}
			break
		}

		if descs[j].Digest == "" {
			return nil, errdefs.NotFound(errors.New("layer not found"))
		}
	}

	return descs, nil
}

func exportContainerRw(layerStore layer.Store, id, mountLabel string) (arch io.ReadCloser, err error) {
	rwlayer, err := layerStore.GetRWLayer(id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			layerStore.ReleaseRWLayer(rwlayer)
		}
	}()

	// TODO: this mount call is not necessary as we assume that TarStream() should
	// mount the layer if needed. But the Diff() function for windows requests that
	// the layer should be mounted when calling it. So we reserve this mount call
	// until windows driver can implement Diff() interface correctly.
	_, err = rwlayer.Mount(mountLabel)
	if err != nil {
		return nil, err
	}

	archive, err := rwlayer.TarStream()
	if err != nil {
		rwlayer.Unmount()
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(archive, func() error {
			archive.Close()
			err = rwlayer.Unmount()
			layerStore.ReleaseRWLayer(rwlayer)
			return err
		}),
		nil
}

// CommitBuildStep is used by the builder to create an image for each step in
// the build.
//
// This method is different from CreateImageFromContainer:
//   * it doesn't attempt to validate container state
//   * it doesn't send a commit action to metrics
//   * it doesn't log a container commit event
//
// This is a temporary shim. Should be removed when builder stops using commit.
func (i *ImageService) CommitBuildStep(ctx context.Context, c backend.CommitConfig) (ocispec.Descriptor, error) {
	container := i.containers.Get(c.ContainerID)
	if container == nil {
		// TODO(containerd): Use typed error here other than not found
		return ocispec.Descriptor{}, errors.Errorf("container not found: %s", c.ContainerID)
	}
	c.ContainerMountLabel = container.MountLabel
	c.ContainerOS = container.OS
	return i.CommitImage(ctx, c)
}

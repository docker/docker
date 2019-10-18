package images

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Load migrates and loads caches for the image service.
func (i *ImageService) Load(ctx context.Context, root string) (err error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	log.G(ctx).WithField("namespace", namespace).Debugf("migration and loading cache")

	var (
		c = &cache{
			layers: map[string]map[digest.Digest]layer.Layer{},
		}
		t1      = time.Now()
		entry   = log.G(ctx).WithField("namespace", namespace)
		done    func(context.Context) error
		version = []byte("1")
	)

	i.cacheL.Lock()
	defer func() {
		if err == nil {
			i.cache[namespace] = c
			entry.WithField("t", time.Since(t1)).Infof("finished load and migration")
		}
		i.cacheL.Unlock()
	}()

	for _, backend := range i.layerBackends {
		p := filepath.Join(root, "image", backend.DriverName())
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}

		var backendCache map[digest.Digest]layer.Layer
		if _, err := os.Stat(filepath.Join(p, "migration")); err == nil {
			// Version of migration not currently relevant, all non-empty can be skipped

			backendCache, err = i.loadLayers(ctx, backend)
			if err != nil {
				return err
			}

			// TODO(containerd): Add distribution metadata store if migration level is less than 2
		} else if !os.IsNotExist(err) {
			return err
		} else {
			if done == nil {
				ctx, done, err = i.client.WithLease(ctx)
				if err != nil {
					return err
				}

				defer func() {
					if err := done(context.Background()); err != nil {
						entry.WithError(err).Error("failed to remove lease")
					}
				}()
			}

			entry.WithField("root", p).Debugf("migrating images")

			var updates map[digest.Digest]*time.Time
			backendCache, updates, err = i.migrateImages(ctx, filepath.Join(p, "imagedb"), backend)
			if err != nil {
				return errors.Wrap(err, "failed to migrate images")
			}

			if err := i.migrateRepositories(ctx, p, updates); err != nil {
				return errors.Wrap(err, "failed to migrate repositories")
			}

			if err := ioutils.AtomicWriteFile(filepath.Join(p, "migration"), version, 0600); err != nil {
				return errors.Wrap(err, "failed to write migration file")
			}

		}

		c.layers[backend.DriverName()] = backendCache
	}

	return nil
}

func (i *ImageService) migrateImages(ctx context.Context, root string, ls layer.Store) (map[digest.Digest]layer.Layer, map[digest.Digest]*time.Time, error) {
	backendCache := map[digest.Digest]layer.Layer{}
	backendName := ls.DriverName()
	updates := map[digest.Digest]*time.Time{}
	cs := i.client.ContentStore()

	// Only Canonical digest (sha256) is currently supported
	dir := filepath.Join(root, "content", string(digest.Canonical))
	subs, err := ioutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return backendCache, updates, nil
		}
		return nil, nil, err
	}
	mpath := filepath.Join(root, "metadata", string(digest.Canonical))
	for _, v := range subs {
		dgst := digest.NewDigestFromHex(string(digest.Canonical), v.Name())
		if err := dgst.Validate(); err != nil {
			log.G(ctx).WithError(err).Debugf("skipping invalid digest %q", dgst)
			continue
		}

		contents, err := ioutil.ReadFile(filepath.Join(dir, v.Name()))
		if err != nil {
			log.G(ctx).WithError(err).WithField("id", dgst).Errorf("failed to read content")
			continue
		}

		var config ocispec.Image
		if err := json.Unmarshal(contents, &config); err != nil {
			log.G(ctx).WithError(err).WithField("id", dgst).Errorf("unable to parse config")
			continue
		}

		chainID := identity.ChainID(config.RootFS.DiffIDs)
		if _, ok := backendCache[chainID]; !ok {
			l, err := ls.Get(layer.ChainID(chainID))
			if err != nil {
				if err == layer.ErrLayerDoesNotExist {
					log.G(ctx).WithField("id", dgst).WithField("chainid", chainID).Warnf("missing referenced layer")
				} else {
					return nil, nil, errors.Wrap(err, "failed to get layer")
				}
			} else {
				backendCache[chainID] = l
			}
		}

		var parent digest.Digest
		if b, err := ioutil.ReadFile(filepath.Join(mpath, v.Name(), "parent")); err == nil && len(b) > 0 {
			parent := digest.Digest(b)
			if err := parent.Validate(); err != nil {
				log.G(ctx).WithError(err).Debugf("invalid parent %q", parent)
				parent = ""
			}
		} else if err != nil && !os.IsNotExist(err) {
			log.G(ctx).WithError(err).WithField("id", dgst).Errorf("failed to read parent")
		}

		var lastUpdated *time.Time
		if b, err := ioutil.ReadFile(filepath.Join(mpath, v.Name(), "lastUpdated")); err == nil && len(b) > 0 {
			t, err := time.Parse(time.RFC3339Nano, string(b))
			if err != nil {
				log.G(ctx).WithError(err).Debugf("invalid lastUpdated %q", string(b))
			} else {
				lastUpdated = &t
			}
		} else if err != nil && !os.IsNotExist(err) {
			log.G(ctx).WithError(err).WithField("id", dgst).Errorf("failed to read last updated")
		}
		updates[dgst] = lastUpdated

		desc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    dgst,
			Size:      int64(len(contents)),
		}

		labels := map[string]string{
			LabelLayerPrefix + backendName: chainID.String(),
		}

		if parent != "" {
			labels[LabelImageParent] = parent.String()
		}

		ref := "config-" + dgst.Algorithm().String() + "-" + dgst.Encoded()
		if err := content.WriteBlob(ctx, cs, ref, bytes.NewReader(contents), desc, content.WithLabels(labels)); err != nil {
			log.G(ctx).WithError(err).WithField("id", dgst).Errorf("can't store config")
		}

	}
	return backendCache, updates, nil
}

func (i *ImageService) migrateRepositories(ctx context.Context, root string, all map[digest.Digest]*time.Time) error {
	b, err := ioutil.ReadFile(filepath.Join(root, `repositories.json`))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return errors.Wrap(err, "failed to read repositories file")
	}
	var repos struct {
		Repositories map[string]map[string]digest.Digest
	}
	if err := json.Unmarshal(b, &repos); err != nil {
		return errors.Wrap(err, "invalid repositories file")
	}
	is := i.client.ImageService()
	cs := i.client.ContentStore()
	remaining := map[digest.Digest]struct{}{}
	for dgst, lastUpdated := range all {
		if lastUpdated != nil {
			// TODO(containerd): this value was only used for image inspect
			// Metadata.LastTagTime, this has been replaced by the actual
			// last tag time in containerd, but that updated time cannot
			// currently be backdated through the containerd API.
			log.G(ctx).WithField("id", dgst).WithField("lastUpdated", *lastUpdated).Debugf("dropping 'lastUpdated' value")
		}

		remaining[dgst] = struct{}{}
	}
	for _, repoGroup := range repos.Repositories {
		imgs := map[digest.Digest][]reference.Named{}
		for name, dgst := range repoGroup {
			named, err := reference.ParseNormalizedNamed(name)
			if err != nil {
				log.G(ctx).WithError(err).WithField("name", name).Warnf("skipping bad name")
			}
			imgs[dgst] = append(imgs[dgst], named)
		}
		for dgst, refs := range imgs {
			info, err := cs.Info(ctx, dgst)
			if err != nil {
				if !errdefs.IsNotFound(err) {
					return errors.Wrap(err, "unable to stat content")
				}
				log.G(ctx).WithField("digest", dgst).Errorf("missing image, ignoring tags")
				continue
			}
			var names []reference.Named
			var tags []string
			var untagged []reference.Named
			for _, ref := range refs {
				if tagged, ok := ref.(reference.NamedTagged); ok {
					names = append(names, tagged)
					tags = append(tags, tagged.Tag())
				} else {
					untagged = append(untagged, ref)
				}
			}

			for _, untagged := range refs {
				if len(tags) > 0 {
					for _, tag := range tags {
						nt, err := reference.WithTag(untagged, tag)
						if err != nil {
							log.G(ctx).WithError(err).WithField("tag", tag).Errorf("ignoring invalid tag")
							continue
						}
						names = append(names, nt)
					}
				} else {
					names = append(names, untagged)
				}
			}

			for _, named := range names {
				_, err = is.Create(ctx, images.Image{
					Name: named.String(),
					Target: ocispec.Descriptor{
						MediaType: images.MediaTypeDockerSchema2Config,
						Digest:    dgst,
						Size:      info.Size,
					},
					// TODO(containerd): Support setting created/updated time,
					// ignored by containerd daemon as of 1.2
				})
				if err != nil {
					if !errdefs.IsAlreadyExists(err) {
						return errors.Wrap(err, "failed to create image")
					}
					log.G(ctx).WithField("name", named.String()).WithField("digest", dgst).Debugf("image already exists, skipping")
				}
			}
			delete(remaining, dgst)

		}
	}

	for dgst := range remaining {
		info, err := cs.Info(ctx, dgst)
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return errors.Wrap(err, "unable to stat content")
			}
			log.G(ctx).WithField("digest", dgst).Errorf("missing image, ignoring tags")
			continue
		}

		_, err = is.Create(ctx, images.Image{
			Name: "<migrated>@" + dgst.String(),
			Target: ocispec.Descriptor{
				MediaType: images.MediaTypeDockerSchema2Config,
				Digest:    dgst,
				Size:      info.Size,
			},
			// TODO(containerd): Support setting created/updated time,
			// ignored by containerd daemon as of 1.2
		})
		if err != nil {
			if !errdefs.IsAlreadyExists(err) {
				return errors.Wrap(err, "failed to create image")
			}
		}
	}
	return nil
}

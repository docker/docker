package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	ctrerrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// default maximum concurrent downloads allowed during docker pull
const defaultMaxConcurrentDownloads = 3

// PullImage initiates a pull operation. image is the repository name to pull, and
// tag may be either empty, or indicate a specific tag to pull.
func (i *ImageService) PullImage(ctx context.Context, image, tag string, platform *ocispec.Platform, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	start := time.Now()
	// Special case: "pull -a" may send an image name with a
	// trailing :. This is ugly, but let's not break API
	// compatibility.
	image = strings.TrimSuffix(image, ":")

	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return errdefs.InvalidParameter(err)
	}

	if tag != "" {
		// The "tag" could actually be a digest.
		var dgst digest.Digest
		dgst, err = digest.Parse(tag)
		if err == nil {
			ref, err = reference.WithDigest(reference.TrimNamed(ref), dgst)
		} else {
			ref, err = reference.WithTag(ref, tag)
		}
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
	}

	err = i.pullImageWithReference(ctx, ref, platform, metaHeaders, authConfig, outStream)
	imageActions.WithValues("pull").UpdateSince(start)
	return err
}

func (i *ImageService) pullImageWithReference(ctx context.Context, ref reference.Named, platform *ocispec.Platform, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	progressOutput := streamformatter.NewJSONProgressOutput(outStream, false)
	ongoing := newJobs(ref.Name())
	pctx, stopProgress := context.WithCancel(ctx)
	progressC := make(chan struct{})

	go func() {
		// no progress bar, because it hides some debug logs
		showProgress(pctx, ongoing, ref, i.client.ContentStore(), progressOutput)
		close(progressC)
	}()

	h := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if desc.MediaType != images.MediaTypeDockerSchema1Manifest {
			ongoing.add(desc)
		}
		return nil, nil
	})

	var (
		layers   = map[digest.Digest][]ocispec.Descriptor{}
		dlStatus = map[digest.Digest]bool{}
		unpacks  int32 // how many unpacks occurred
	)
	grp, pctx := errgroup.WithContext(pctx)

	// unpackHandler handles layer unpacking concurrently as soon as
	// a layer has been downloaded in order
	unpackHandler := func(h images.Handler) images.Handler {
		var (
			index      bool
			unpackDesc = map[digest.Digest]struct{}{}
			lock       = sync.Mutex{}
			cond       = sync.NewCond(&lock)
		)
		return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			children, err := h.Handle(ctx, desc)
			if err != nil {
				return children, err
			}

			switch desc.MediaType {
			case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
				lock.Lock()
				index = true
				var unknown []ocispec.Descriptor
				for _, d := range children {
					if d.Platform == nil {
						unknown = append(unknown, d)
					} else if i.platforms.Match(*d.Platform) {
						unpackDesc[d.Digest] = struct{}{}
					}
				}
				if len(unpackDesc) == 0 && len(unknown) > 0 {
					unpackDesc[unknown[0].Digest] = struct{}{}
				}
				lock.Unlock()
			case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
				lock.Lock()
				if _, ok := unpackDesc[desc.Digest]; !ok && index {
					children = children[:1]

				}
				if len(children) > 1 {
					// map the config to layers
					layers[children[0].Digest] = children[1:]
				}
				lock.Unlock()
			case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
				lock.Lock()
				unpackLayers := layers[desc.Digest]
				lock.Unlock()
				if len(unpackLayers) > 0 {
					atomic.AddInt32(&unpacks, 1)
					grp.Go(func() error {
						return i.unpack(pctx, desc, unpackLayers, progressOutput, cond, dlStatus)
					})
				}
			case images.MediaTypeDockerSchema2LayerGzip, images.MediaTypeDockerSchema2Layer,
				ocispec.MediaTypeImageLayerGzip, ocispec.MediaTypeImageLayer:
				// a layer has been downloaded, signal downloaded status
				lock.Lock()
				dlStatus[desc.Digest] = true
				lock.Unlock()
				cond.Broadcast()
			}

			return children, nil
		})
	}

	pctx, done, err := i.client.WithLease(pctx)
	if err != nil {
		return err
	}

	defer func() {
		if err := done(context.Background()); err != nil {
			log.G(pctx).WithError(err).Errorf("failed to remove lease")
		}
	}()

	// TODO(containerd): Custom resolver
	//  - Auth config
	//  - Custom headers
	opts := []containerd.RemoteOpt{
		containerd.WithImageHandler(h),
		containerd.WithImageHandlerWrapper(unpackHandler),
		containerd.WithMaxConcurrentDownloads(defaultMaxConcurrentDownloads),
	}

	img, err := i.client.Fetch(pctx, ref.String(), opts...)
	if err != nil {
		return errors.Wrap(err, "failed to pull image")
	}

	if unpacks > 0 {
		if err := grp.Wait(); err != nil {
			return err
		}
	} else {
		// try to resolve config to unpack if none was done previously
		// schema 1 must be resolved and unpacked after pull
		config, err := img.Config(pctx, i.client.ContentStore(), i.platforms)
		if err != nil {
			return errors.Wrap(err, "failed to resolve image config for unpack")
		}

		l, ok := layers[config.Digest]
		if !ok {
			return errors.Wrap(err, "no layers found to unpack")
		}

		err = i.unpack(pctx, config, l, progressOutput, nil, nil)
		if err != nil {
			return errors.Wrapf(err, "failed to unpack %s", img.Target.Digest)
		}
	}

	fref := reference.FamiliarString(ref)
	c, err := reference.WithDigest(ref, img.Target.Digest)
	if err != nil {
		return errors.Wrap(err, "failed to create digest ref")
	}

	var newImage bool
	img.Name = c.String()
	_, err = i.client.ImageService().Create(ctx, img)
	if err != nil {
		if !ctrerrdefs.IsAlreadyExists(err) {
			return errors.Wrap(err, "failed to create image")
		}
	} else {
		newImage = true
	}

	stopProgress()
	<-progressC
	progress.Messagef(progressOutput, "", "Digest: %s", img.Target.Digest.String())
	if newImage {
		progress.Messagef(progressOutput, "", "Downloaded newer image for %s", fref)
	}
	return nil
}

func (i *ImageService) unpack(ctx context.Context, config ocispec.Descriptor, layers []ocispec.Descriptor, progressOutput progress.Output, cond *sync.Cond, status map[digest.Digest]bool) error {
	c, err := i.getCache(ctx)
	if err != nil {
		return err
	}

	cs := i.client.ContentStore()
	p, err := content.ReadBlob(ctx, cs, config)
	if err != nil {
		return err
	}

	var cfg struct {
		ocispec.Platform

		// RootFS references the layer content addresses used by the image.
		RootFS ocispec.RootFS `json:"rootfs"`
	}

	if err := json.Unmarshal(p, &cfg); err != nil {
		return errors.Wrap(err, "failed to parse config")
	}

	diffIDs := cfg.RootFS.DiffIDs
	if len(diffIDs) != len(layers) {
		return errors.Errorf("mismatched image rootfs and manifest layers")
	}

	// Resolve layerstore
	ls, err := i.getLayerStore(platforms.Normalize(cfg.Platform))
	if err != nil {
		return err
	}

	var (
		chain = []digest.Digest{}
		l     layer.Layer
	)
	for d := range diffIDs {
		chain = append(chain, diffIDs[d])
		// start extracting upon signaled after current layer downloading complete
		// otherwise wait upon the resource is ready
		if cond != nil && status != nil {
			cond.L.Lock()
			for !status[layers[d].Digest] {
				cond.Wait()
			}
			cond.L.Unlock()
		}

		nl, err := i.applyLayer(ctx, ls, layers[d], chain, progressOutput)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("apply layer failed")
			if l != nil {
				layer.ReleaseAndLog(ls, l)
			}
			return errors.Wrapf(err, "failed to apply layer %d", d)
		}
		logrus.Debugf("Layer applied: chain=%s %s (%s)", nl.ChainID(), nl.DiffID(), diffIDs[d])

		if l != nil {
			metadata, err := ls.Release(l)
			if err != nil {
				layer.ReleaseAndLog(ls, nl)
				return errors.Wrap(err, "failed to release layer after apply")
			}
			layer.LogReleaseMetadata(metadata)
		}

		if digest.Digest(nl.DiffID()) != diffIDs[d] {
			layer.ReleaseAndLog(ls, nl)
			return errors.Errorf("invalid diff id %s, expected %s", nl.DiffID(), diffIDs[d])
		}

		l = nl
	}

	key := fmt.Sprintf("%s%s", LabelLayerPrefix, ls.DriverName())
	info := content.Info{
		Digest: config.Digest,
		Labels: map[string]string{
			key: l.ChainID().String(),
		},
	}

	if _, err := cs.Update(ctx, info, "labels."+key); err != nil {
		layer.ReleaseAndLog(ls, l)
		return errors.Wrap(err, "failed to update image config label")
	}

	c.m.Lock()
	blayers, ok := c.layers[ls.DriverName()]
	if !ok {
		blayers = map[digest.Digest]layer.Layer{}
		c.layers[ls.DriverName()] = blayers
	}
	if ll, ok := blayers[digest.Digest(l.ChainID())]; ok {
		metadata, err := ls.Release(ll)
		if err != nil {
			log.G(ctx).WithError(err).WithField("driver", ls.DriverName()).WithField("name", string(ll.ChainID())).Errorf("failed to release retained layer")
		} else {
			layer.LogReleaseMetadata(metadata)
		}
	}
	blayers[digest.Digest(l.ChainID())] = l
	c.m.Unlock()

	return nil
}

func (i *ImageService) applyLayer(ctx context.Context, ls layer.Store, blob ocispec.Descriptor, layers []digest.Digest, progressOutput progress.Output) (layer.Layer, error) {
	cs := i.client.ContentStore()
	l, err := ls.Get(layer.ChainID(identity.ChainID(layers)))
	if err == nil {
		return l, nil
	} else if err != layer.ErrLayerDoesNotExist {
		return nil, err
	}

	ra, err := cs.ReaderAt(ctx, blob)
	if err != nil {
		return nil, err
	}
	defer ra.Close()

	rc := ioutil.NopCloser(content.NewReader(ra))
	blobID := stringid.TruncateID(blob.Digest.String())
	reader := ioutils.NewCancelReadCloser(ctx, rc)
	if progressOutput != nil {
		reader = progress.NewProgressReader(reader, progressOutput, blob.Size, blobID, "Extracting")
	}
	defer reader.Close()

	dc, err := compression.DecompressStream(reader)
	if err != nil {
		return nil, err
	}
	defer dc.Close()

	var parent digest.Digest
	if len(layers) > 1 {
		parent = identity.ChainID(layers[:len(layers)-1])
	}

	var r io.Reader
	var dgstr digest.Digester
	if dc.GetCompression() == compression.Gzip {
		dgstr = digest.Canonical.Digester()
		r = io.TeeReader(dc, dgstr.Hash())
	} else {
		r = dc
	}

	nl, err := ls.Register(r, layer.ChainID(parent))
	if err != nil {
		return nil, err
	}

	if dgstr != nil {
		info := content.Info{
			Digest: blob.Digest,
			Labels: map[string]string{
				"containerd.io/uncompressed": dgstr.Digest().String(),
			},
		}
		_, err := cs.Update(ctx, info, "labels.containerd.io/uncompressed")
		if err != nil {
			log.G(ctx).WithError(err).WithField("digest", blob.Digest.String()).Warnf("unable to set uncompressed label")
		}
	}

	return nl, nil
}

func getTagOrDigest(ref reference.Named) string {
	var (
		// manifest    distribution.Manifest
		tagOrDigest string // Used for logging/progress only
	)
	if digested, isDigested := ref.(reference.Canonical); isDigested {
		tagOrDigest = digested.Digest().String()
	} else if tagged, isTagged := ref.(reference.NamedTagged); isTagged {
		tagOrDigest = tagged.Tag()
	}
	// todo: is it safe to assume it is always a tag or digest?
	return tagOrDigest
}

const (
	downloading  = "Downloading"
	dlcomplete   = "Download complete"
	waiting      = "Waiting"
	exists       = "Already exists"
	pullcomplete = "Pull complete"
)

func showProgress(ctx context.Context, ongoing *jobs, ref reference.Named, cs content.Store, progressOutput progress.Output) {
	// progressOutput := streamformatter.NewJSONProgressOutput(out, false)
	progressOutput.WriteProgress(progress.Progress{ID: getTagOrDigest(ref), Message: "Pulling from " + reference.Path(ref)})
	var (
		ticker   = time.NewTicker(100 * time.Millisecond)
		start    = time.Now()
		statuses = map[string]StatusInfo{}
		done     bool
	)
	defer ticker.Stop()

outer:
	for {
		select {
		case <-ticker.C:
			activeSeen := map[string]struct{}{}
			if !done {
				active, err := cs.ListStatuses(ctx, "")
				if err != nil {
					logrus.Error("active check failed")
					continue
				}
				// update status of active entries!
				for _, active := range active {
					descID := stringid.TruncateID(active.Ref)
					if !strings.Contains(active.Ref, "layer") {
						continue
					}
					progressOutput.WriteProgress(progress.Progress{ID: descID, Action: downloading, Current: active.Offset, Total: active.Total, LastUpdate: false})
					statuses[descID] = StatusInfo{
						Status: downloading, // Downloading
					}
					activeSeen[descID] = struct{}{}
				}
			}

			// now, update the items in jobs that are not in active
			for _, j := range ongoing.jobs() {
				descID := stringid.TruncateID(j.Digest.String())
				if _, ok := activeSeen[descID]; ok {
					continue
				}
				// skip displaying non-layer info
				if !isLayer(j) {
					continue
				}
				status, ok := statuses[descID]
				if !done && (!ok || status.Status == downloading) {
					info, err := cs.Info(ctx, j.Digest)
					if err != nil {
						if !errdefs.IsNotFound(err) {
							logrus.Errorf("failed to get content info")
							continue outer
						} else {
							progressOutput.WriteProgress(progress.Progress{ID: descID, Action: waiting})
							statuses[descID] = StatusInfo{
								Status: waiting,
							}
						}
					} else if info.CreatedAt.After(start) {
						progressOutput.WriteProgress(progress.Progress{ID: descID, Action: dlcomplete})
						statuses[descID] = StatusInfo{
							Status: dlcomplete,
						}
					} else {
						progressOutput.WriteProgress(progress.Progress{ID: descID, Action: exists})
						statuses[descID] = StatusInfo{
							Status: exists,
						}
					}
				} else if done {
					progressOutput.WriteProgress(progress.Progress{ID: descID, Action: pullcomplete})
					if ok {
						if status.Status != dlcomplete && status.Status != exists {
							status.Status = pullcomplete
							statuses[descID] = status
						}
					} else {
						statuses[descID] = StatusInfo{
							Status: pullcomplete,
						}
					}
				}
			}

			if done {
				return
			}
		case <-ctx.Done():
			done = true // allow ui to update once more
		}
	}
}

// StatusInfo holds the status info for an upload or download
type StatusInfo struct {
	Status string
}

func isLayer(desc ocispec.Descriptor) bool {
	switch desc.MediaType {
	case images.MediaTypeDockerSchema2Layer, images.MediaTypeDockerSchema2LayerGzip,
		images.MediaTypeDockerSchema2LayerForeign, images.MediaTypeDockerSchema2LayerForeignGzip,
		ocispec.MediaTypeImageLayer, ocispec.MediaTypeImageLayerGzip,
		ocispec.MediaTypeImageLayerNonDistributable, ocispec.MediaTypeImageLayerNonDistributableGzip:
		return true
	default:
		return false
	}
}

// jobs provides a way of identifying the download keys for a particular task
// encountering during the pull walk.
//
// This is very minimal and will probably be replaced with something more
// featured.
type jobs struct {
	name  string
	added map[digest.Digest]struct{}
	descs []ocispec.Descriptor
	mu    sync.Mutex
}

func newJobs(name string) *jobs {
	return &jobs{
		name:  name,
		added: map[digest.Digest]struct{}{},
	}
}

func (j *jobs) add(desc ocispec.Descriptor) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if _, ok := j.added[desc.Digest]; ok {
		return
	}
	j.descs = append(j.descs, desc)
	j.added[desc.Digest] = struct{}{}
}

func (j *jobs) jobs() []ocispec.Descriptor {
	j.mu.Lock()
	defer j.mu.Unlock()

	var descs []ocispec.Descriptor
	return append(descs, j.descs...)
}

package tarexport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/docker/distribution"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/internal/image"
	v1 "github.com/moby/moby/v2/daemon/internal/image/v1"
	"github.com/moby/moby/v2/daemon/internal/ioutils"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/system"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type imageDescriptor struct {
	refs     []reference.NamedTagged
	layers   []layer.DiffID
	image    *image.Image
	layerRef layer.Layer
}

type saveSession struct {
	*tarexporter
	outDir       string
	images       map[image.ID]*imageDescriptor
	savedLayers  map[layer.DiffID]distribution.Descriptor
	savedConfigs map[string]struct{}
}

func (l *tarexporter) Save(ctx context.Context, names []string, outStream io.Writer) error {
	imgDescriptors, err := l.parseNames(ctx, names)
	if err != nil {
		return err
	}

	// Release all the image top layer references
	defer l.releaseLayerReferences(imgDescriptors)
	return (&saveSession{tarexporter: l, images: imgDescriptors}).save(ctx, outStream)
}

// parseNames will parse the image names to a map which contains image.ID to *imageDescriptor.
// Each imageDescriptor holds an image top layer reference named 'layerRef'. It is taken here, should be released later.
func (l *tarexporter) parseNames(ctx context.Context, names []string) (desc map[image.ID]*imageDescriptor, rErr error) {
	imgDescr := make(map[image.ID]*imageDescriptor)
	defer func() {
		if rErr != nil {
			l.releaseLayerReferences(imgDescr)
		}
	}()

	addAssoc := func(id image.ID, ref reference.Named) error {
		if _, ok := imgDescr[id]; !ok {
			descr := &imageDescriptor{}
			if err := l.takeLayerReference(id, descr); err != nil {
				return err
			}
			imgDescr[id] = descr
		}

		if ref != nil {
			if _, ok := ref.(reference.Canonical); ok {
				return nil
			}
			tagged, ok := reference.TagNameOnly(ref).(reference.NamedTagged)
			if !ok {
				return nil
			}

			for _, t := range imgDescr[id].refs {
				if tagged.String() == t.String() {
					return nil
				}
			}
			imgDescr[id].refs = append(imgDescr[id].refs, tagged)
		}
		return nil
	}

	for _, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		ref, err := reference.ParseAnyReference(name)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		namedRef, ok := ref.(reference.Named)
		if !ok {
			// Check if digest ID reference
			if digested, ok := ref.(reference.Digested); ok {
				if err := addAssoc(image.ID(digested.Digest()), nil); err != nil {
					return nil, err
				}
				continue
			}
			return nil, errdefs.InvalidParameter(errors.Errorf("invalid reference: %v", name))
		}

		if reference.FamiliarName(namedRef) == string(digest.Canonical) {
			imgID, err := l.is.Search(name)
			if err != nil {
				return nil, err
			}
			if err := addAssoc(imgID, nil); err != nil {
				return nil, err
			}
			continue
		}
		if reference.IsNameOnly(namedRef) {
			assocs := l.rs.ReferencesByName(namedRef)
			for _, assoc := range assocs {
				if err := addAssoc(image.ID(assoc.ID), assoc.Ref); err != nil {
					return nil, err
				}
			}
			if len(assocs) == 0 {
				imgID, err := l.is.Search(name)
				if err != nil {
					return nil, err
				}
				if err := addAssoc(imgID, nil); err != nil {
					return nil, err
				}
			}
			continue
		}
		id, err := l.rs.Get(namedRef)
		if err != nil {
			return nil, err
		}
		if err := addAssoc(image.ID(id), namedRef); err != nil {
			return nil, err
		}
	}
	return imgDescr, nil
}

// takeLayerReference will take/Get the image top layer reference
func (l *tarexporter) takeLayerReference(id image.ID, imgDescr *imageDescriptor) error {
	img, err := l.is.Get(id)
	if err != nil {
		return err
	}
	if err := image.CheckOS(img.OperatingSystem()); err != nil {
		return fmt.Errorf("os %q is not supported", img.OperatingSystem())
	}
	if l.platform != nil {
		if !l.platformMatcher.Match(img.Platform()) {
			return errors.New("no suitable export target found for platform " + platforms.FormatAll(*l.platform))
		}
	}
	imgDescr.image = img
	topLayerID := img.RootFS.ChainID()
	if topLayerID == "" {
		return nil
	}
	topLayer, err := l.lss.Get(topLayerID)
	if err != nil {
		return err
	}
	imgDescr.layerRef = topLayer
	return nil
}

// releaseLayerReferences will release all the image top layer references
func (l *tarexporter) releaseLayerReferences(imgDescr map[image.ID]*imageDescriptor) error {
	for _, descr := range imgDescr {
		if descr.layerRef != nil {
			l.lss.Release(descr.layerRef)
		}
	}
	return nil
}

func (s *saveSession) save(ctx context.Context, outStream io.Writer) error {
	s.savedConfigs = make(map[string]struct{})
	s.savedLayers = make(map[layer.DiffID]distribution.Descriptor)

	// get image json
	tempDir, err := os.MkdirTemp("", "docker-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	s.outDir = tempDir
	reposLegacy := make(map[string]map[string]string)

	var manifest []manifestItem
	var parentLinks []parentLink

	var manifestDescriptors []ocispec.Descriptor

	for id, imageDescr := range s.images {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		foreignSrcs, err := s.saveImage(ctx, id)
		if err != nil {
			return err
		}

		var (
			repoTags []string
			layers   []string
			foreign  = make([]ocispec.Descriptor, 0, len(foreignSrcs))
		)

		// Layers in manifest must follow the actual layer order from config.
		for _, l := range imageDescr.layers {
			desc := foreignSrcs[l]
			foreign = append(foreign, ocispec.Descriptor{
				MediaType:   desc.MediaType,
				Digest:      desc.Digest,
				Size:        desc.Size,
				URLs:        desc.URLs,
				Annotations: desc.Annotations,
				Platform:    desc.Platform,
			})
		}

		data, err := json.Marshal(ocispec.Manifest{
			Versioned: specs.Versioned{
				SchemaVersion: 2,
			},
			MediaType: ocispec.MediaTypeImageManifest,
			Config: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageConfig,
				Digest:    digest.Digest(imageDescr.image.ID()),
				Size:      int64(len(imageDescr.image.RawJSON())),
			},
			Layers: foreign,
		})
		if err != nil {
			return errors.Wrap(err, "error marshaling manifest")
		}
		dgst := digest.FromBytes(data)

		mFile := filepath.Join(s.outDir, ocispec.ImageBlobsDir, dgst.Algorithm().String(), dgst.Encoded())
		if err := mkdirAllWithChtimes(filepath.Dir(mFile), 0o755, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return errors.Wrap(err, "error creating blob directory")
		}
		if err := os.WriteFile(mFile, data, 0o644); err != nil {
			return errors.Wrap(err, "error writing oci manifest file")
		}
		if err := system.Chtimes(mFile, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return errors.Wrap(err, "error setting oci manifest timestamp")
		}
		if err := system.Chtimes(filepath.Dir(mFile), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return errors.Wrap(err, "error setting blob digest directory timestamp")
		}

		untaggedMfstDesc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    dgst,
			Size:      int64(len(data)),
		}
		for _, ref := range imageDescr.refs {
			familiarName := reference.FamiliarName(ref)
			if _, ok := reposLegacy[familiarName]; !ok {
				reposLegacy[familiarName] = make(map[string]string)
			}
			reposLegacy[familiarName][ref.Tag()] = imageDescr.layers[len(imageDescr.layers)-1].Encoded()
			repoTags = append(repoTags, reference.FamiliarString(ref))

			taggedManifest := untaggedMfstDesc
			taggedManifest.Annotations = map[string]string{
				c8dimages.AnnotationImageName: ref.String(),
				ocispec.AnnotationRefName:     ref.Tag(),
			}
			manifestDescriptors = append(manifestDescriptors, taggedManifest)
		}

		// If no ref was assigned, make sure still add the image is still included in index.json.
		if len(manifestDescriptors) == 0 {
			manifestDescriptors = append(manifestDescriptors, untaggedMfstDesc)
		}

		for _, lDgst := range imageDescr.layers {
			// IMPORTANT: We use path, not filepath here to ensure the layers
			// in the manifest use Unix-style forward-slashes.
			layers = append(layers, path.Join(ocispec.ImageBlobsDir, lDgst.Algorithm().String(), lDgst.Encoded()))
		}

		manifest = append(manifest, manifestItem{
			Config:       path.Join(ocispec.ImageBlobsDir, id.Digest().Algorithm().String(), id.Digest().Encoded()),
			RepoTags:     repoTags,
			Layers:       layers,
			LayerSources: foreignSrcs,
		})

		parentID, _ := s.is.GetParent(id)
		parentLinks = append(parentLinks, parentLink{id, parentID})
		s.tarexporter.loggerImgEvent.LogImageEvent(ctx, id.String(), id.String(), events.ActionSave)
	}

	for i, p := range validatedParentLinks(parentLinks) {
		if p.parentID != "" {
			manifest[i].Parent = p.parentID
		}
	}

	if len(reposLegacy) > 0 {
		reposFile := filepath.Join(tempDir, legacyRepositoriesFileName)
		rf, err := os.OpenFile(reposFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}

		if err := json.NewEncoder(rf).Encode(reposLegacy); err != nil {
			rf.Close()
			return err
		}

		rf.Close()

		if err := system.Chtimes(reposFile, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return err
		}
	}

	manifestPath := filepath.Join(tempDir, manifestFileName)
	f, err := os.OpenFile(manifestPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if err := json.NewEncoder(f).Encode(manifest); err != nil {
		f.Close()
		return err
	}

	f.Close()

	if err := system.Chtimes(manifestPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		return err
	}

	const ociLayoutContent = `{"imageLayoutVersion": "` + ocispec.ImageLayoutVersion + `"}`
	layoutPath := filepath.Join(tempDir, ocispec.ImageLayoutFile)
	if err := os.WriteFile(layoutPath, []byte(ociLayoutContent), 0o644); err != nil {
		return errors.Wrap(err, "error writing oci layout file")
	}
	if err := system.Chtimes(layoutPath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		return errors.Wrap(err, "error setting oci layout file timestamps")
	}

	data, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifestDescriptors,
	})
	if err != nil {
		return errors.Wrap(err, "error marshaling oci index")
	}

	idxFile := filepath.Join(s.outDir, ocispec.ImageIndexFile)
	if err := os.WriteFile(idxFile, data, 0o644); err != nil {
		return errors.Wrap(err, "error writing oci index file")
	}
	if err := system.Chtimes(idxFile, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		return errors.Wrap(err, "error setting oci index file timestamps")
	}

	return s.writeTar(ctx, tempDir, outStream)
}

func (s *saveSession) writeTar(ctx context.Context, tempDir string, outStream io.Writer) error {
	ctx, span := tracing.StartSpan(ctx, "writeTar")
	defer span.End()

	fs, err := archive.Tar(tempDir, compression.None)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	defer fs.Close()

	_, err = ioutils.CopyCtx(ctx, outStream, fs)

	span.SetStatus(err)
	return err
}

func (s *saveSession) saveImage(ctx context.Context, id image.ID) (_ map[layer.DiffID]distribution.Descriptor, outErr error) {
	ctx, span := tracing.StartSpan(ctx, "saveImage")
	span.SetAttributes(tracing.Attribute("image.id", id.String()))
	defer span.End()
	defer func() {
		span.SetStatus(outErr)
	}()

	img := s.images[id].image
	if len(img.RootFS.DiffIDs) == 0 {
		return nil, errors.New("empty export - not implemented")
	}

	ts := time.Unix(0, 0)
	if img.Created != nil {
		ts = *img.Created
	}

	var parent digest.Digest
	var layers []layer.DiffID
	var foreignSrcs map[layer.DiffID]distribution.Descriptor
	for i, diffID := range img.RootFS.DiffIDs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		v1ImgCreated := time.Unix(0, 0)
		v1Img := image.V1Image{
			// This is for backward compatibility used for
			// pre v1.9 docker.
			Created: &v1ImgCreated,
		}
		if i == len(img.RootFS.DiffIDs)-1 {
			v1Img = img.V1Image
		}
		rootFS := *img.RootFS
		rootFS.DiffIDs = rootFS.DiffIDs[:i+1]
		v1ID, err := v1.CreateID(v1Img, rootFS.ChainID(), parent)
		if err != nil {
			return nil, err
		}

		v1Img.ID = v1ID.Encoded()
		if parent != "" {
			v1Img.Parent = parent.Encoded()
		}

		v1Img.OS = img.OS
		src, err := s.saveConfigAndLayer(ctx, rootFS.ChainID(), v1Img, &ts)
		if err != nil {
			return nil, err
		}

		layers = append(layers, diffID)
		parent = v1ID
		if src.Digest != "" {
			if foreignSrcs == nil {
				foreignSrcs = make(map[layer.DiffID]distribution.Descriptor)
			}
			foreignSrcs[img.RootFS.DiffIDs[i]] = src
		}
	}

	data := img.RawJSON()
	dgst := digest.FromBytes(data)

	blobDir := filepath.Join(s.outDir, ocispec.ImageBlobsDir, dgst.Algorithm().String())
	if err := mkdirAllWithChtimes(blobDir, 0o755, ts, ts); err != nil {
		return nil, err
	}
	if err := system.Chtimes(blobDir, ts, ts); err != nil {
		return nil, err
	}
	if err := system.Chtimes(filepath.Dir(blobDir), ts, ts); err != nil {
		return nil, err
	}

	configFile := filepath.Join(blobDir, dgst.Encoded())
	if err := os.WriteFile(configFile, img.RawJSON(), 0o644); err != nil {
		return nil, err
	}
	if err := system.Chtimes(configFile, ts, ts); err != nil {
		return nil, err
	}

	s.images[id].layers = layers
	return foreignSrcs, nil
}

func (s *saveSession) saveConfigAndLayer(ctx context.Context, id layer.ChainID, legacyImg image.V1Image, createdTime *time.Time) (_ distribution.Descriptor, outErr error) {
	ctx, span := tracing.StartSpan(ctx, "saveConfigAndLayer")
	span.SetAttributes(
		tracing.Attribute("layer.id", id.String()),
		tracing.Attribute("image.id", legacyImg.ID),
	)
	defer span.End()
	defer func() {
		span.SetStatus(outErr)
	}()

	ts := time.Unix(0, 0)
	if createdTime != nil {
		ts = *createdTime
	}

	outDir := filepath.Join(s.outDir, ocispec.ImageBlobsDir)

	if _, ok := s.savedConfigs[legacyImg.ID]; !ok {
		if err := s.saveConfig(legacyImg, outDir, createdTime); err != nil {
			return distribution.Descriptor{}, err
		}
	}

	// serialize filesystem
	l, err := s.lss.Get(id)
	if err != nil {
		return distribution.Descriptor{}, err
	}
	defer layer.ReleaseAndLog(s.lss, l)

	lDiffID := l.DiffID()
	lDgst := lDiffID
	if _, ok := s.savedLayers[lDiffID]; ok {
		return s.savedLayers[lDiffID], nil
	}
	layerPath := filepath.Join(outDir, lDiffID.Algorithm().String(), lDiffID.Encoded())

	if _, err = os.Stat(layerPath); err == nil {
		// This is should not happen. If the layer path was already created, we should have returned early.
		// Log a warning an proceed to recreate the archive.
		log.G(context.TODO()).WithFields(log.Fields{
			"layerPath": layerPath,
			"id":        id,
			"lDgst":     lDgst,
		}).Warn("LayerPath already exists but the descriptor is not cached")
	} else if !os.IsNotExist(err) {
		return distribution.Descriptor{}, err
	}

	if err := mkdirAllWithChtimes(filepath.Dir(layerPath), 0o755, ts, ts); err != nil {
		return distribution.Descriptor{}, errors.Wrap(err, "could not create layer dir parent")
	}
	// We use sequential file access to avoid depleting the standby list on
	// Windows. On non-Windows platforms this equates to a regular os.Create.
	tarFile, err := os.OpenFile(layerPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC|windows_O_FILE_FLAG_SEQUENTIAL_SCAN, 0o666)
	if err != nil {
		return distribution.Descriptor{}, errors.Wrap(err, "error creating layer file")
	}
	defer tarFile.Close()

	arch, err := l.TarStream()
	if err != nil {
		return distribution.Descriptor{}, err
	}
	defer arch.Close()

	digester := digest.Canonical.Digester()
	digestedArch := io.TeeReader(arch, digester.Hash())

	tarSize, err := ioutils.CopyCtx(ctx, tarFile, digestedArch)
	if err != nil {
		return distribution.Descriptor{}, err
	}

	tarDigest := digester.Digest()
	if lDgst != tarDigest {
		log.G(context.TODO()).WithFields(log.Fields{
			"layerDigest":  lDgst,
			"actualDigest": tarDigest,
		}).Warn("layer digest doesn't match its tar archive digest")

		lDgst = digester.Digest()
		layerPath = filepath.Join(outDir, lDgst.Algorithm().String(), lDgst.Encoded())
	}

	for _, fname := range []string{outDir, layerPath} {
		// todo: maybe save layer created timestamp?
		if err := system.Chtimes(fname, ts, ts); err != nil {
			return distribution.Descriptor{}, errors.Wrap(err, "could not set layer timestamp")
		}
	}

	var desc distribution.Descriptor
	if fs, ok := l.(distribution.Describable); ok {
		desc = fs.Descriptor()
	}

	if desc.Digest == "" {
		desc.Digest = tarDigest
		desc.Size = tarSize
	}
	if desc.MediaType == "" {
		desc.MediaType = ocispec.MediaTypeImageLayer
	}
	s.savedLayers[lDiffID] = desc

	return desc, nil
}

func (s *saveSession) saveConfig(legacyImg image.V1Image, outDir string, createdTime *time.Time) error {
	imageConfig, err := json.Marshal(legacyImg)
	if err != nil {
		return err
	}

	ts := time.Unix(0, 0)
	if createdTime != nil {
		ts = *createdTime
	}

	cfgDgst := digest.FromBytes(imageConfig)
	configPath := filepath.Join(outDir, cfgDgst.Algorithm().String(), cfgDgst.Encoded())
	if err := mkdirAllWithChtimes(filepath.Dir(configPath), 0o755, ts, ts); err != nil {
		return errors.Wrap(err, "could not create layer dir parent")
	}

	if err := os.WriteFile(configPath, imageConfig, 0o644); err != nil {
		return err
	}

	if err := system.Chtimes(configPath, ts, ts); err != nil {
		return errors.Wrap(err, "could not set config timestamp")
	}

	s.savedConfigs[legacyImg.ID] = struct{}{}
	return nil
}

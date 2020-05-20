package plugin // import "github.com/docker/docker/plugin"

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	reference "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/authorization"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/pools"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/system"
	v2 "github.com/docker/docker/plugin/v2"
	"github.com/moby/sys/mount"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var acceptedPluginFilterTags = map[string]bool{
	"enabled":    true,
	"capability": true,
}

// Disable deactivates a plugin. This means resources (volumes, networks) cant use them.
func (pm *Manager) Disable(refOrID string, config *types.PluginDisableConfig) error {
	p, err := pm.config.Store.GetV2Plugin(refOrID)
	if err != nil {
		return err
	}
	pm.mu.RLock()
	c := pm.cMap[p]
	pm.mu.RUnlock()

	if !config.ForceDisable && p.GetRefCount() > 0 {
		return errors.WithStack(inUseError(p.Name()))
	}

	for _, typ := range p.GetTypes() {
		if typ.Capability == authorization.AuthZApiImplements {
			pm.config.AuthzMiddleware.RemovePlugin(p.Name())
		}
	}

	if err := pm.disable(p, c); err != nil {
		return err
	}
	pm.publisher.Publish(EventDisable{Plugin: p.PluginObj})
	pm.config.LogPluginEvent(p.GetID(), refOrID, "disable")
	return nil
}

// Enable activates a plugin, which implies that they are ready to be used by containers.
func (pm *Manager) Enable(refOrID string, config *types.PluginEnableConfig) error {
	p, err := pm.config.Store.GetV2Plugin(refOrID)
	if err != nil {
		return err
	}

	c := &controller{timeoutInSecs: config.Timeout}
	if err := pm.enable(p, c, false); err != nil {
		return err
	}
	pm.publisher.Publish(EventEnable{Plugin: p.PluginObj})
	pm.config.LogPluginEvent(p.GetID(), refOrID, "enable")
	return nil
}

// Inspect examines a plugin config
func (pm *Manager) Inspect(refOrID string) (tp *types.Plugin, err error) {
	p, err := pm.config.Store.GetV2Plugin(refOrID)
	if err != nil {
		return nil, err
	}

	return &p.PluginObj, nil
}

func computePrivileges(c types.PluginConfig) types.PluginPrivileges {
	var privileges types.PluginPrivileges
	if c.Network.Type != "null" && c.Network.Type != "bridge" && c.Network.Type != "" {
		privileges = append(privileges, types.PluginPrivilege{
			Name:        "network",
			Description: "permissions to access a network",
			Value:       []string{c.Network.Type},
		})
	}
	if c.IpcHost {
		privileges = append(privileges, types.PluginPrivilege{
			Name:        "host ipc namespace",
			Description: "allow access to host ipc namespace",
			Value:       []string{"true"},
		})
	}
	if c.PidHost {
		privileges = append(privileges, types.PluginPrivilege{
			Name:        "host pid namespace",
			Description: "allow access to host pid namespace",
			Value:       []string{"true"},
		})
	}
	for _, mount := range c.Mounts {
		if mount.Source != nil {
			privileges = append(privileges, types.PluginPrivilege{
				Name:        "mount",
				Description: "host path to mount",
				Value:       []string{*mount.Source},
			})
		}
	}
	for _, device := range c.Linux.Devices {
		if device.Path != nil {
			privileges = append(privileges, types.PluginPrivilege{
				Name:        "device",
				Description: "host device to access",
				Value:       []string{*device.Path},
			})
		}
	}
	if c.Linux.AllowAllDevices {
		privileges = append(privileges, types.PluginPrivilege{
			Name:        "allow-all-devices",
			Description: "allow 'rwm' access to all devices",
			Value:       []string{"true"},
		})
	}
	if len(c.Linux.Capabilities) > 0 {
		privileges = append(privileges, types.PluginPrivilege{
			Name:        "capabilities",
			Description: "list of additional capabilities required",
			Value:       c.Linux.Capabilities,
		})
	}

	return privileges
}

// Privileges pulls a plugin config and computes the privileges required to install it.
func (pm *Manager) Privileges(ctx context.Context, ref reference.Named, metaHeader http.Header, authConfig *types.AuthConfig) (types.PluginPrivileges, error) {
	var (
		config     types.PluginConfig
		configSeen bool
	)

	h := func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		switch desc.MediaType {
		case schema2.MediaTypeManifest, specs.MediaTypeImageManifest:
			data, err := content.ReadBlob(ctx, pm.blobStore, desc)
			if err != nil {
				return nil, errors.Wrapf(err, "error reading image manifest from blob store for %s", ref)
			}

			var m specs.Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, errors.Wrapf(err, "error unmarshaling image manifest for %s", ref)
			}
			return []specs.Descriptor{m.Config}, nil
		case schema2.MediaTypePluginConfig:
			configSeen = true
			data, err := content.ReadBlob(ctx, pm.blobStore, desc)
			if err != nil {
				return nil, errors.Wrapf(err, "error reading plugin config from blob store for %s", ref)
			}

			if err := json.Unmarshal(data, &config); err != nil {
				return nil, errors.Wrapf(err, "error unmarshaling plugin config for %s", ref)
			}
		}

		return nil, nil
	}

	if err := pm.fetch(ctx, ref, authConfig, progress.DiscardOutput(), metaHeader, images.HandlerFunc(h)); err != nil {
		return types.PluginPrivileges{}, nil
	}

	if !configSeen {
		return types.PluginPrivileges{}, errors.Errorf("did not find plugin config for specified reference %s", ref)
	}

	return computePrivileges(config), nil
}

// Upgrade upgrades a plugin
//
// TODO: replace reference package usage with simpler url.Parse semantics
func (pm *Manager) Upgrade(ctx context.Context, ref reference.Named, name string, metaHeader http.Header, authConfig *types.AuthConfig, privileges types.PluginPrivileges, outStream io.Writer) (err error) {
	p, err := pm.config.Store.GetV2Plugin(name)
	if err != nil {
		return err
	}

	if p.IsEnabled() {
		return errors.Wrap(enabledError(p.Name()), "plugin must be disabled before upgrading")
	}

	// revalidate because Pull is public
	if _, err := reference.ParseNormalizedNamed(name); err != nil {
		return errors.Wrapf(errdefs.InvalidParameter(err), "failed to parse %q", name)
	}

	pm.muGC.RLock()
	defer pm.muGC.RUnlock()

	tmpRootFSDir, err := ioutil.TempDir(pm.tmpDir(), ".rootfs")
	if err != nil {
		return errors.Wrap(err, "error creating tmp dir for plugin rootfs")
	}

	var md fetchMeta

	ctx, cancel := context.WithCancel(ctx)
	out, waitProgress := setupProgressOutput(outStream, cancel)
	defer waitProgress()

	if err := pm.fetch(ctx, ref, authConfig, out, metaHeader, storeFetchMetadata(&md), childrenHandler(pm.blobStore), applyLayer(pm.blobStore, tmpRootFSDir, out)); err != nil {
		return err
	}
	pm.config.LogPluginEvent(reference.FamiliarString(ref), name, "pull")

	if err := validateFetchedMetadata(md); err != nil {
		return err
	}

	if err := pm.upgradePlugin(p, md.config, md.manifest, md.blobs, tmpRootFSDir, &privileges); err != nil {
		return err
	}
	p.PluginObj.PluginReference = ref.String()
	return nil
}

// Pull pulls a plugin, check if the correct privileges are provided and install the plugin.
//
// TODO: replace reference package usage with simpler url.Parse semantics
func (pm *Manager) Pull(ctx context.Context, ref reference.Named, name string, metaHeader http.Header, authConfig *types.AuthConfig, privileges types.PluginPrivileges, outStream io.Writer, opts ...CreateOpt) (err error) {
	pm.muGC.RLock()
	defer pm.muGC.RUnlock()

	// revalidate because Pull is public
	nameref, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return errors.Wrapf(errdefs.InvalidParameter(err), "failed to parse %q", name)
	}
	name = reference.FamiliarString(reference.TagNameOnly(nameref))

	if err := pm.config.Store.validateName(name); err != nil {
		return errdefs.InvalidParameter(err)
	}

	tmpRootFSDir, err := ioutil.TempDir(pm.tmpDir(), ".rootfs")
	if err != nil {
		return errors.Wrap(errdefs.System(err), "error preparing upgrade")
	}
	defer os.RemoveAll(tmpRootFSDir)

	var md fetchMeta

	ctx, cancel := context.WithCancel(ctx)
	out, waitProgress := setupProgressOutput(outStream, cancel)
	defer waitProgress()

	if err := pm.fetch(ctx, ref, authConfig, out, metaHeader, storeFetchMetadata(&md), childrenHandler(pm.blobStore), applyLayer(pm.blobStore, tmpRootFSDir, out)); err != nil {
		return err
	}
	pm.config.LogPluginEvent(reference.FamiliarString(ref), name, "pull")

	if err := validateFetchedMetadata(md); err != nil {
		return err
	}

	refOpt := func(p *v2.Plugin) {
		p.PluginObj.PluginReference = ref.String()
	}
	optsList := make([]CreateOpt, 0, len(opts)+1)
	optsList = append(optsList, opts...)
	optsList = append(optsList, refOpt)

	// TODO: tmpRootFSDir is empty but should have layers in it
	p, err := pm.createPlugin(name, md.config, md.manifest, md.blobs, tmpRootFSDir, &privileges, optsList...)
	if err != nil {
		return err
	}

	pm.publisher.Publish(EventCreate{Plugin: p.PluginObj})

	return nil
}

// List displays the list of plugins and associated metadata.
func (pm *Manager) List(pluginFilters filters.Args) ([]types.Plugin, error) {
	if err := pluginFilters.Validate(acceptedPluginFilterTags); err != nil {
		return nil, err
	}

	enabledOnly := false
	disabledOnly := false
	if pluginFilters.Contains("enabled") {
		if pluginFilters.ExactMatch("enabled", "true") {
			enabledOnly = true
		} else if pluginFilters.ExactMatch("enabled", "false") {
			disabledOnly = true
		} else {
			return nil, invalidFilter{"enabled", pluginFilters.Get("enabled")}
		}
	}

	plugins := pm.config.Store.GetAll()
	out := make([]types.Plugin, 0, len(plugins))

next:
	for _, p := range plugins {
		if enabledOnly && !p.PluginObj.Enabled {
			continue
		}
		if disabledOnly && p.PluginObj.Enabled {
			continue
		}
		if pluginFilters.Contains("capability") {
			for _, f := range p.GetTypes() {
				if !pluginFilters.Match("capability", f.Capability) {
					continue next
				}
			}
		}
		out = append(out, p.PluginObj)
	}
	return out, nil
}

// Push pushes a plugin to the registry.
func (pm *Manager) Push(ctx context.Context, name string, metaHeader http.Header, authConfig *types.AuthConfig, outStream io.Writer) error {
	p, err := pm.config.Store.GetV2Plugin(name)
	if err != nil {
		return err
	}

	ref, err := reference.ParseNormalizedNamed(p.Name())
	if err != nil {
		return errors.Wrapf(err, "plugin has invalid name %v for push", p.Name())
	}

	statusTracker := docker.NewInMemoryTracker()

	resolver, err := pm.newResolver(ctx, statusTracker, authConfig, metaHeader, false)
	if err != nil {
		return err
	}

	pusher, err := resolver.Pusher(ctx, ref.String())
	if err != nil {

		return errors.Wrap(err, "error creating plugin pusher")
	}

	pj := newPushJobs(statusTracker)

	ctx, cancel := context.WithCancel(ctx)
	out, waitProgress := setupProgressOutput(outStream, cancel)
	defer waitProgress()

	progressHandler := images.HandlerFunc(func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		logrus.WithField("mediaType", desc.MediaType).WithField("digest", desc.Digest.String()).Debug("Preparing to push plugin layer")
		id := stringid.TruncateID(desc.Digest.String())
		pj.add(remotes.MakeRefKey(ctx, desc), id)
		progress.Update(out, id, "Preparing")
		return nil, nil
	})

	desc, err := pm.getManifestDescriptor(ctx, p)
	if err != nil {
		return errors.Wrap(err, "error reading plugin manifest")
	}

	progress.Messagef(out, "", "The push refers to repository [%s]", reference.FamiliarName(ref))

	// TODO: If a layer already exists on the registry, the progress output just says "Preparing"
	go func() {
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		if !timer.Stop() {
			<-timer.C
		}
		var statuses []contentStatus
		for {
			timer.Reset(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				statuses = pj.status()
			}

			for _, s := range statuses {
				out.WriteProgress(progress.Progress{ID: s.Ref, Current: s.Offset, Total: s.Total, Action: s.Status, LastUpdate: s.Offset == s.Total})
			}
		}
	}()

	// Make sure we can authenticate the request since the auth scope for plugin repos is different than a normal repo.
	ctx = docker.WithScope(ctx, scope(ref, true))
	if err := remotes.PushContent(ctx, pusher, desc, pm.blobStore, nil, func(h images.Handler) images.Handler {
		return images.Handlers(progressHandler, h)
	}); err != nil {
		// Try fallback to http.
		// This is needed because the containerd pusher will only attempt the first registry config we pass, which would
		// typically be https.
		// If there are no http-only host configs found we'll error out anyway.
		resolver, _ := pm.newResolver(ctx, statusTracker, authConfig, metaHeader, true)
		if resolver != nil {
			pusher, _ := resolver.Pusher(ctx, ref.String())
			if pusher != nil {
				logrus.WithField("ref", ref).Debug("Re-attmpting push with http-fallback")
				err2 := remotes.PushContent(ctx, pusher, desc, pm.blobStore, nil, func(h images.Handler) images.Handler {
					return images.Handlers(progressHandler, h)
				})
				if err2 == nil {
					err = nil
				} else {
					logrus.WithError(err2).WithField("ref", ref).Debug("Error while attempting push with http-fallback")
				}
			}
		}
		if err != nil {
			return errors.Wrap(err, "error pushing plugin")
		}
	}

	// For blobs that already exist in the registry we need to make sure to update the progress otherwise it will just say "pending"
	// TODO: How to check if the layer already exists? Is it worth it?
	for _, j := range pj.jobs {
		progress.Update(out, pj.names[j], "Upload complete")
	}

	// Signal the client for content trust verification
	progress.Aux(out, types.PushResult{Tag: ref.(reference.Tagged).Tag(), Digest: desc.Digest.String(), Size: int(desc.Size)})

	return nil
}

// manifest wraps an OCI manifest, because...
// Historically the registry does not support plugins unless the media type on the manifest is specifically schema2.MediaTypeManifest
// So the OCI manifest media type is not supported.
// Additionally, there is extra validation for the docker schema2 manifest than there is a mediatype set on the manifest itself
// even though this is set on the descriptor
// The OCI types do not have this field.
type manifest struct {
	specs.Manifest
	MediaType string `json:"mediaType,omitempty"`
}

func buildManifest(ctx context.Context, s content.Manager, config digest.Digest, layers []digest.Digest) (manifest, error) {
	var m manifest
	m.MediaType = images.MediaTypeDockerSchema2Manifest
	m.SchemaVersion = 2

	configInfo, err := s.Info(ctx, config)
	if err != nil {
		return m, errors.Wrapf(err, "error reading plugin config content for digest %s", config)
	}
	m.Config = specs.Descriptor{
		MediaType: mediaTypePluginConfig,
		Size:      configInfo.Size,
		Digest:    configInfo.Digest,
	}

	for _, l := range layers {
		info, err := s.Info(ctx, l)
		if err != nil {
			return m, errors.Wrapf(err, "error fetching info for content digest %s", l)
		}
		m.Layers = append(m.Layers, specs.Descriptor{
			MediaType: specs.MediaTypeImageLayerGzip, // TODO: This is assuming everything is a gzip compressed layer, but that may not be true.
			Digest:    l,
			Size:      info.Size,
		})
	}
	return m, nil
}

// getManifestDescriptor gets the OCI descriptor for a manifest
// It will generate a manifest if one does not exist
func (pm *Manager) getManifestDescriptor(ctx context.Context, p *v2.Plugin) (specs.Descriptor, error) {
	logger := logrus.WithField("plugin", p.Name()).WithField("digest", p.Manifest)
	if p.Manifest != "" {
		info, err := pm.blobStore.Info(ctx, p.Manifest)
		if err == nil {
			desc := specs.Descriptor{
				Size:      info.Size,
				Digest:    info.Digest,
				MediaType: images.MediaTypeDockerSchema2Manifest,
			}
			return desc, nil
		}
		logger.WithError(err).Debug("Could not find plugin manifest in content store")
	} else {
		logger.Info("Plugin does not have manifest digest")
	}
	logger.Info("Building a new plugin manifest")

	manifest, err := buildManifest(ctx, pm.blobStore, p.Config, p.Blobsums)
	if err != nil {
		return specs.Descriptor{}, err
	}

	desc, err := writeManifest(ctx, pm.blobStore, &manifest)
	if err != nil {
		return desc, err
	}

	if err := pm.save(p); err != nil {
		logger.WithError(err).Error("Could not save plugin with manifest digest")
	}
	return desc, nil
}

func writeManifest(ctx context.Context, cs content.Store, m *manifest) (specs.Descriptor, error) {
	platform := platforms.DefaultSpec()
	desc := specs.Descriptor{
		MediaType: images.MediaTypeDockerSchema2Manifest,
		Platform:  &platform,
	}
	data, err := json.Marshal(m)
	if err != nil {
		return desc, errors.Wrap(err, "error encoding manifest")
	}
	desc.Digest = digest.FromBytes(data)
	desc.Size = int64(len(data))

	if err := content.WriteBlob(ctx, cs, remotes.MakeRefKey(ctx, desc), bytes.NewReader(data), desc); err != nil {
		return desc, errors.Wrap(err, "error writing plugin manifest")
	}
	return desc, nil
}

// Remove deletes plugin's root directory.
func (pm *Manager) Remove(name string, config *types.PluginRmConfig) error {
	p, err := pm.config.Store.GetV2Plugin(name)
	pm.mu.RLock()
	c := pm.cMap[p]
	pm.mu.RUnlock()

	if err != nil {
		return err
	}

	if !config.ForceRemove {
		if p.GetRefCount() > 0 {
			return inUseError(p.Name())
		}
		if p.IsEnabled() {
			return enabledError(p.Name())
		}
	}

	if p.IsEnabled() {
		if err := pm.disable(p, c); err != nil {
			logrus.Errorf("failed to disable plugin '%s': %s", p.Name(), err)
		}
	}

	defer func() {
		go pm.GC()
	}()

	id := p.GetID()
	pluginDir := filepath.Join(pm.config.Root, id)

	if err := mount.RecursiveUnmount(pluginDir); err != nil {
		return errors.Wrap(err, "error unmounting plugin data")
	}

	if err := atomicRemoveAll(pluginDir); err != nil {
		return err
	}

	pm.config.Store.Remove(p)
	pm.config.LogPluginEvent(id, name, "remove")
	pm.publisher.Publish(EventRemove{Plugin: p.PluginObj})
	return nil
}

// Set sets plugin args
func (pm *Manager) Set(name string, args []string) error {
	p, err := pm.config.Store.GetV2Plugin(name)
	if err != nil {
		return err
	}
	if err := p.Set(args); err != nil {
		return err
	}
	return pm.save(p)
}

// CreateFromContext creates a plugin from the given pluginDir which contains
// both the rootfs and the config.json and a repoName with optional tag.
func (pm *Manager) CreateFromContext(ctx context.Context, tarCtx io.ReadCloser, options *types.PluginCreateOptions) (err error) {
	pm.muGC.RLock()
	defer pm.muGC.RUnlock()

	ref, err := reference.ParseNormalizedNamed(options.RepoName)
	if err != nil {
		return errors.Wrapf(err, "failed to parse reference %v", options.RepoName)
	}
	if _, ok := ref.(reference.Canonical); ok {
		return errors.Errorf("canonical references are not permitted")
	}
	name := reference.FamiliarString(reference.TagNameOnly(ref))

	if err := pm.config.Store.validateName(name); err != nil { // fast check, real check is in createPlugin()
		return err
	}

	tmpRootFSDir, err := ioutil.TempDir(pm.tmpDir(), ".rootfs")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory")
	}
	defer os.RemoveAll(tmpRootFSDir)

	var configJSON []byte
	rootFS := splitConfigRootFSFromTar(tarCtx, &configJSON)

	rootFSBlob, err := pm.blobStore.Writer(ctx, content.WithRef(name))
	if err != nil {
		return err
	}
	defer rootFSBlob.Close()

	gzw := gzip.NewWriter(rootFSBlob)
	rootFSReader := io.TeeReader(rootFS, gzw)

	if err := chrootarchive.Untar(rootFSReader, tmpRootFSDir, nil); err != nil {
		return err
	}
	if err := rootFS.Close(); err != nil {
		return err
	}

	if configJSON == nil {
		return errors.New("config not found")
	}

	if err := gzw.Close(); err != nil {
		return errors.Wrap(err, "error closing gzip writer")
	}

	var config types.PluginConfig
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return errors.Wrap(err, "failed to parse config")
	}

	if err := pm.validateConfig(config); err != nil {
		return err
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	if err := rootFSBlob.Commit(ctx, 0, ""); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			go pm.GC()
		}
	}()

	config.Rootfs = &types.PluginConfigRootfs{
		Type:    "layers",
		DiffIds: []string{rootFSBlob.Digest().String()},
	}

	config.DockerVersion = dockerversion.Version

	configBlob, err := pm.blobStore.Writer(ctx, content.WithRef(name+"-config.json"))
	if err != nil {
		return err
	}
	defer configBlob.Close()
	if err := json.NewEncoder(configBlob).Encode(config); err != nil {
		return errors.Wrap(err, "error encoding json config")
	}
	if err := configBlob.Commit(ctx, 0, ""); err != nil {
		return err
	}

	configDigest := configBlob.Digest()
	layers := []digest.Digest{rootFSBlob.Digest()}

	manifest, err := buildManifest(ctx, pm.blobStore, configDigest, layers)
	if err != nil {
		return err
	}
	desc, err := writeManifest(ctx, pm.blobStore, &manifest)
	if err != nil {
		return
	}

	p, err := pm.createPlugin(name, configDigest, desc.Digest, layers, tmpRootFSDir, nil)
	if err != nil {
		return err
	}
	p.PluginObj.PluginReference = name

	pm.publisher.Publish(EventCreate{Plugin: p.PluginObj})
	pm.config.LogPluginEvent(p.PluginObj.ID, name, "create")

	return nil
}

func (pm *Manager) validateConfig(config types.PluginConfig) error {
	return nil // TODO:
}

func splitConfigRootFSFromTar(in io.ReadCloser, config *[]byte) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tarReader := tar.NewReader(in)
		tarWriter := tar.NewWriter(pw)
		defer in.Close()

		hasRootFS := false

		for {
			hdr, err := tarReader.Next()
			if err == io.EOF {
				if !hasRootFS {
					pw.CloseWithError(errors.Wrap(err, "no rootfs found"))
					return
				}
				// Signals end of archive.
				tarWriter.Close()
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(errors.Wrap(err, "failed to read from tar"))
				return
			}

			content := io.Reader(tarReader)
			name := path.Clean(hdr.Name)
			if path.IsAbs(name) {
				name = name[1:]
			}
			if name == configFileName {
				dt, err := ioutil.ReadAll(content)
				if err != nil {
					pw.CloseWithError(errors.Wrapf(err, "failed to read %s", configFileName))
					return
				}
				*config = dt
			}
			if parts := strings.Split(name, "/"); len(parts) != 0 && parts[0] == rootFSFileName {
				hdr.Name = path.Clean(path.Join(parts[1:]...))
				if hdr.Typeflag == tar.TypeLink && strings.HasPrefix(strings.ToLower(hdr.Linkname), rootFSFileName+"/") {
					hdr.Linkname = hdr.Linkname[len(rootFSFileName)+1:]
				}
				if err := tarWriter.WriteHeader(hdr); err != nil {
					pw.CloseWithError(errors.Wrap(err, "error writing tar header"))
					return
				}
				if _, err := pools.Copy(tarWriter, content); err != nil {
					pw.CloseWithError(errors.Wrap(err, "error copying tar data"))
					return
				}
				hasRootFS = true
			} else {
				io.Copy(ioutil.Discard, content)
			}
		}
	}()
	return pr
}

func atomicRemoveAll(dir string) error {
	renamed := dir + "-removing"

	err := os.Rename(dir, renamed)
	switch {
	case os.IsNotExist(err), err == nil:
		// even if `dir` doesn't exist, we can still try and remove `renamed`
	case os.IsExist(err):
		// Some previous remove failed, check if the origin dir exists
		if e := system.EnsureRemoveAll(renamed); e != nil {
			return errors.Wrap(err, "rename target already exists and could not be removed")
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			// origin doesn't exist, nothing left to do
			return nil
		}

		// attempt to rename again
		if err := os.Rename(dir, renamed); err != nil {
			return errors.Wrap(err, "failed to rename dir for atomic removal")
		}
	default:
		return errors.Wrap(err, "failed to rename dir for atomic removal")
	}

	if err := system.EnsureRemoveAll(renamed); err != nil {
		os.Rename(renamed, dir)
		return err
	}
	return nil
}

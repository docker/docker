package image

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/pkg/authconfig"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/builder/remotecontext"
	"github.com/moby/moby/v2/daemon/internal/compat"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/dockerversion"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Creates an image from Pull or from Import
func (ir *imageRouter) postImagesCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		img         = r.Form.Get("fromImage")
		repo        = r.Form.Get("repo")
		tag         = r.Form.Get("tag")
		comment     = r.Form.Get("message")
		progressErr error
		output      = ioutils.NewWriteFlusher(w)
		platform    *ocispec.Platform
	)
	defer output.Close()

	w.Header().Set("Content-Type", "application/json")

	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, "1.32") {
		if p := r.FormValue("platform"); p != "" {
			sp, err := platforms.Parse(p)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
			platform = &sp
		}
	}

	if img != "" { // pull
		metaHeaders := map[string][]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Meta-") {
				metaHeaders[k] = v
			}
		}

		// Special case: "pull -a" may send an image name with a
		// trailing :. This is ugly, but let's not break API
		// compatibility.
		imgName := strings.TrimSuffix(img, ":")

		ref, err := reference.ParseNormalizedNamed(imgName)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}

		// TODO(thaJeztah) this could use a WithTagOrDigest() utility
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

		if err := validateRepoName(ref); err != nil {
			return errdefs.Forbidden(err)
		}

		// For a pull it is not an error if no auth was given. Ignore invalid
		// AuthConfig to increase compatibility with the existing API.
		//
		// TODO(thaJeztah): accept empty values but return an error when failing to decode.
		authConfig, _ := authconfig.Decode(r.Header.Get(registry.AuthHeader))
		pullOptions := imagebackend.PullOptions{
			AuthConfig:  authConfig,
			MetaHeaders: metaHeaders,
			OutStream:   output,
		}
		if platform != nil {
			pullOptions.Platforms = append(pullOptions.Platforms, *platform)
		}
		progressErr = ir.backend.PullImage(ctx, ref, pullOptions)
	} else { // import
		src := r.Form.Get("fromSrc")

		tagRef, err := httputils.RepoTagReference(repo, tag)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}

		if comment == "" {
			comment = "Imported from " + src
		}

		var layerReader io.ReadCloser
		defer r.Body.Close()
		if src == "-" {
			layerReader = r.Body
		} else {
			if len(strings.Split(src, "://")) == 1 {
				src = "http://" + src
			}
			u, err := url.Parse(src)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}

			resp, err := remotecontext.GetWithStatusError(u.String())
			if err != nil {
				return err
			}
			output.Write(streamformatter.FormatStatus("", "Downloading from %s", u))
			progressOutput := streamformatter.NewJSONProgressOutput(output, true)
			layerReader = progress.NewProgressReader(resp.Body, progressOutput, resp.ContentLength, "", "Importing")
			defer layerReader.Close()
		}

		var id image.ID
		id, progressErr = ir.backend.ImportImage(ctx, tagRef, platform, comment, layerReader, r.Form["changes"])

		if progressErr == nil {
			_, _ = output.Write(streamformatter.FormatStatus("", "%v", id.String()))
		}
	}
	if progressErr != nil {
		if !output.Flushed() {
			return progressErr
		}
		_, _ = output.Write(streamformatter.FormatError(progressErr))
	}

	return nil
}

func (ir *imageRouter) postImagesPush(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	metaHeaders := map[string][]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Meta-") {
			metaHeaders[k] = v
		}
	}
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	// Handle the authConfig as a header, but ignore invalid AuthConfig
	// to increase compatibility with the existing API.
	//
	// TODO(thaJeztah): accept empty values but return an error when failing to decode.
	authConfig, _ := authconfig.Decode(r.Header.Get(registry.AuthHeader))

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()

	w.Header().Set("Content-Type", "application/json")

	img := vars["name"]
	tag := r.Form.Get("tag")

	var ref reference.Named

	// Tag is empty only in case PushOptions.All is true.
	if tag != "" {
		r, err := httputils.RepoTagReference(img, tag)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		ref = r
	} else {
		r, err := reference.ParseNormalizedNamed(img)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		ref = r
	}

	var platform *ocispec.Platform
	// Platform is optional, and only supported in API version 1.46 and later.
	// However the PushOptions struct previously was an alias for the PullOptions struct
	// which also contained a Platform field.
	// This means that older clients may be sending a platform field, even
	// though it wasn't really supported by the server.
	// Don't break these clients and just ignore the platform field on older APIs.
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.46") {
		if formPlatform := r.Form.Get("platform"); formPlatform != "" {
			p, err := httputils.DecodePlatform(formPlatform)
			if err != nil {
				return err
			}
			platform = p
		}
	}
	pushOptions := imagebackend.PushOptions{
		AuthConfig:  authConfig,
		MetaHeaders: metaHeaders,
		OutStream:   output,
	}
	if platform != nil {
		pushOptions.Platforms = append(pushOptions.Platforms, *platform)
	}
	if err := ir.backend.PushImage(ctx, ref, pushOptions); err != nil {
		if !output.Flushed() {
			return err
		}
		_, _ = output.Write(streamformatter.FormatError(err))
	}
	return nil
}

func (ir *imageRouter) getImagesGet(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/x-tar")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()

	var names []string
	if name, ok := vars["name"]; ok {
		names = []string{name}
	} else {
		names = r.Form["names"]
	}

	var platformList []ocispec.Platform
	// platform param was introduced in API version 1.48
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.48") {
		var err error
		formPlatforms := r.Form["platform"]
		// multi-platform params were introduced in API version 1.52
		if versions.LessThan(httputils.VersionFromContext(ctx), "1.52") && len(formPlatforms) > 1 {
			return errdefs.InvalidParameter(errors.New("multiple platform parameters are not supported in this API version; use API version 1.52 or later"))
		}
		platformList, err = httputils.DecodePlatforms(formPlatforms)
		if err != nil {
			return err
		}
	}

	if err := ir.backend.ExportImage(ctx, names, platformList, output); err != nil {
		if !output.Flushed() {
			return err
		}
		_, _ = output.Write(streamformatter.FormatError(err))
	}

	return nil
}

func (ir *imageRouter) postImagesLoad(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var platformList []ocispec.Platform
	// platform param was introduced in API version 1.48
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.48") {
		var err error
		formPlatforms := r.Form["platform"]
		// multi-platform params were introduced in API version 1.52
		if versions.LessThan(httputils.VersionFromContext(ctx), "1.52") && len(formPlatforms) > 1 {
			return errdefs.InvalidParameter(errors.New("multiple platform parameters are not supported in this API version; use API version 1.52 or later"))
		}
		platformList, err = httputils.DecodePlatforms(formPlatforms)
		if err != nil {
			return err
		}
	}
	quiet := httputils.BoolValueOrDefault(r, "quiet", true)

	w.Header().Set("Content-Type", "application/json")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()

	if err := ir.backend.LoadImage(ctx, r.Body, platformList, output, quiet); err != nil {
		_, _ = output.Write(streamformatter.FormatError(err))
	}
	return nil
}

type missingImageError struct{}

func (missingImageError) Error() string {
	return "image name cannot be blank"
}

func (missingImageError) InvalidParameter() {}

func (ir *imageRouter) deleteImages(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]

	if strings.TrimSpace(name) == "" {
		return missingImageError{}
	}

	force := httputils.BoolValue(r, "force")
	prune := !httputils.BoolValue(r, "noprune")

	var p []ocispec.Platform
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.50") {
		val, err := httputils.DecodePlatforms(r.Form["platforms"])
		if err != nil {
			return err
		}
		p = val
	}

	list, err := ir.backend.ImageDelete(ctx, name, imagebackend.RemoveOptions{
		Force:         force,
		PruneChildren: prune,
		Platforms:     p,
	})
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, list)
}

func (ir *imageRouter) getImagesByName(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var manifests bool
	if r.Form.Get("manifests") != "" && versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.48") {
		manifests = httputils.BoolValue(r, "manifests")
	}

	var platform *ocispec.Platform
	if r.Form.Get("platform") != "" && versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.49") {
		p, err := httputils.DecodePlatform(r.Form.Get("platform"))
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		platform = p
	}

	if manifests && platform != nil {
		return errdefs.InvalidParameter(errors.New("conflicting options: manifests and platform options cannot both be set"))
	}

	inspectData, err := ir.backend.ImageInspect(ctx, vars["name"], imagebackend.ImageInspectOpts{
		Manifests: manifests,
		Identity:  versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.53"),
		Platform:  platform,
	})
	if err != nil {
		return err
	}
	imageInspect := inspectData.InspectResponse

	var legacyOptions []compat.Option

	// Make sure we output empty arrays instead of nil. While Go nil slice is functionally equivalent to an empty slice,
	// it matters for the JSON representation.
	if imageInspect.RepoTags == nil {
		imageInspect.RepoTags = []string{}
	}
	if imageInspect.RepoDigests == nil {
		imageInspect.RepoDigests = []string{}
	}

	version := httputils.VersionFromContext(ctx)
	if versions.LessThan(version, "1.44") {
		legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{
			"VirtualSize": imageInspect.Size,
		}))
		if imageInspect.Created == "" {
			// backwards compatibility for Created not existing returning "0001-01-01T00:00:00Z"
			// https://github.com/moby/moby/issues/47368
			imageInspect.Created = time.Time{}.Format(time.RFC3339Nano)
		}
	}
	if versions.LessThan(version, "1.45") {
		legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{
			"Container":       inspectData.Container,
			"ContainerConfig": inspectData.ContainerConfig,
		}))
	}
	if versions.LessThan(version, "1.48") {
		imageInspect.Descriptor = nil
	}
	if versions.LessThan(version, "1.52") {
		// These fields have "omitempty" on API v1.52 and higher,
		// but older API versions returned them unconditionally.
		legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{
			"Parent":        inspectData.Parent, // field is deprecated, but still included in response when present (built with legacy builder).
			"Comment":       inspectData.Comment,
			"DockerVersion": inspectData.DockerVersion, // field is deprecated, but still included in response when present.
			"Author":        inspectData.Author,
		}))

		// preserve fields in the image Config that have an "omitempty"
		// in the OCI spec, but weren't omitted in API v1.51 and lower.
		if versions.LessThan(version, "1.50") {
			legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{
				"Config": legacyConfigFields["v1.49"],
			}))
		} else {
			legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{
				"Config": legacyConfigFields["v1.50-v1.51"],
			}))
		}

		// Restore the GraphDriver field, now omitted when a snapshotter is used.
		if imageInspect.GraphDriver == nil && inspectData.GraphDriverLegacy != nil {
			imageInspect.GraphDriver = inspectData.GraphDriverLegacy
		}
	} else {
		if inspectData.Parent != "" {
			// field is deprecated, but still included in response when present (built with legacy builder).
			legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{"Parent": inspectData.Parent}))
		}
		if inspectData.DockerVersion != "" {
			// field is deprecated, but still included in response when present.
			legacyOptions = append(legacyOptions, compat.WithExtraFields(map[string]any{"DockerVersion": inspectData.DockerVersion}))
		}
	}

	if len(legacyOptions) > 0 {
		resp := compat.Wrap(imageInspect, legacyOptions...)
		return httputils.WriteJSON(w, http.StatusOK, resp)
	}
	return httputils.WriteJSON(w, http.StatusOK, imageInspect)
}

func (ir *imageRouter) getImagesJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	imageFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)

	// clients may be actively removing the new filter on API 1.24
	// and under: https://github.com/moby/moby/blob/v28.4.0/client/image_list.go#L34-L40
	if versions.LessThan(version, "1.25") && !imageFilters.Contains("reference") {
		// NOTE: filter is a shell glob string applied to repository names.
		if filterParam := r.Form.Get("filter"); filterParam != "" {
			imageFilters.Add("reference", filterParam)
		}
	}

	var manifests bool
	if versions.GreaterThanOrEqualTo(version, "1.47") {
		manifests = httputils.BoolValue(r, "manifests")
	}

	var identity bool
	if versions.GreaterThanOrEqualTo(version, "1.54") {
		identity = httputils.BoolValue(r, "identity")
		if identity && !manifests {
			return errdefs.InvalidParameter(errors.New("conflicting options: identity requires manifests=1"))
		}
	}

	images, err := ir.backend.Images(ctx, imagebackend.ListOptions{
		All:        httputils.BoolValue(r, "all"),
		Filters:    imageFilters,
		SharedSize: httputils.BoolValue(r, "shared-size"),
		Manifests:  manifests,
		Identity:   identity,
	})
	if err != nil {
		return err
	}

	addVirtualSize := versions.LessThan(version, "1.44")
	noDescriptor := versions.LessThan(version, "1.48")
	noContainers := versions.LessThan(version, "1.51")
	if noDescriptor || noContainers {
		for i := range images {
			if noDescriptor {
				images[i].Descriptor = nil
			}
			if noContainers {
				images[i].Containers = -1
			}
		}
	}
	if addVirtualSize {
		wrapped := make([]*compat.Wrapper, len(images))
		for i := range images {
			wrapped[i] = compat.Wrap(&images[i], compat.WithExtraFields(map[string]any{
				"VirtualSize": images[i].Size,
			}))
		}
		return httputils.WriteJSON(w, http.StatusOK, wrapped)
	}

	return httputils.WriteJSON(w, http.StatusOK, images)
}

func (ir *imageRouter) getImagesHistory(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var platform *ocispec.Platform
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.48") {
		if formPlatform := r.Form.Get("platform"); formPlatform != "" {
			p, err := httputils.DecodePlatform(formPlatform)
			if err != nil {
				return err
			}
			platform = p
		}
	}
	history, err := ir.backend.ImageHistory(ctx, vars["name"], platform)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, history)
}

func (ir *imageRouter) postImagesTag(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	ref, err := httputils.RepoTagReference(r.Form.Get("repo"), r.Form.Get("tag"))
	if ref == nil || err != nil {
		return errdefs.InvalidParameter(err)
	}

	refName := reference.FamiliarName(ref)
	if refName == string(digest.Canonical) {
		return errdefs.InvalidParameter(errors.New("refusing to create an ambiguous tag using digest algorithm as name"))
	}

	img, err := ir.backend.GetImage(ctx, vars["name"], imagebackend.GetImageOpts{})
	if err != nil {
		return errdefs.NotFound(err)
	}

	if err := ir.backend.TagImage(ctx, img.ID(), ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (ir *imageRouter) getImagesSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var limit int
	if r.Form.Get("limit") != "" {
		var err error
		limit, err = strconv.Atoi(r.Form.Get("limit"))
		if err != nil || limit < 0 {
			return errdefs.InvalidParameter(errors.Wrap(err, "invalid limit specified"))
		}
	}
	searchFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	// For a search it is not an error if no auth was given. Ignore invalid
	// AuthConfig to increase compatibility with the existing API.
	authConfig, _ := authconfig.Decode(r.Header.Get(registry.AuthHeader))

	headers := http.Header{}
	for k, v := range r.Header {
		k = http.CanonicalHeaderKey(k)
		if strings.HasPrefix(k, "X-Meta-") {
			headers[k] = v
		}
	}
	headers.Set("User-Agent", dockerversion.DockerUserAgent(ctx))
	res, err := ir.searcher.Search(ctx, searchFilters, r.Form.Get("term"), limit, authConfig, headers)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, res)
}

func (ir *imageRouter) postImagesPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	pruneFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	pruneReport, err := ir.backend.ImagePrune(ctx, pruneFilters)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, pruneReport)
}

func (ir *imageRouter) getImageAttestations(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	// The platform parameter is declared as a multi-value array in the
	// swagger so the wire shape stays forward-compatible, but only a single
	// value is currently accepted. Reject requests that pass more than one.
	var platform *ocispec.Platform
	if ps := r.Form["platform"]; len(ps) > 0 {
		if len(ps) > 1 {
			return errdefs.InvalidParameter(errors.New("only one platform value is currently supported"))
		}
		if ps[0] != "" {
			decoded, err := httputils.DecodePlatform(ps[0])
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
			platform = decoded
		}
	}

	var predicateTypes []string
	for _, pt := range r.Form["type"] {
		if pt = strings.TrimSpace(pt); pt != "" {
			predicateTypes = append(predicateTypes, pt)
		}
	}

	statements, err := ir.backend.ImageAttestations(ctx, vars["name"], imagebackend.AttestationOpts{
		Platform:         platform,
		PredicateTypes:   predicateTypes,
		IncludeStatement: httputils.BoolValue(r, "statement"),
	})
	if err != nil {
		return err
	}

	if statements == nil {
		statements = []imagetypes.AttestationStatement{}
	}

	return httputils.WriteJSON(w, http.StatusOK, statements)
}

// noBaseImageSpecifier is the symbol used by the FROM
// command to specify that no base image is to be used.
const noBaseImageSpecifier = "scratch"

// validateRepoName validates the name of a repository.
func validateRepoName(name reference.Named) error {
	familiarName := reference.FamiliarName(name)
	if familiarName == noBaseImageSpecifier {
		return fmt.Errorf("'%s' is a reserved name", familiarName)
	}
	return nil
}

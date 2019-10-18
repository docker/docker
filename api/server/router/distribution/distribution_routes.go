package distribution // import "github.com/docker/docker/api/server/router/distribution"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
)

func (s *distributionRouter) getDistributionInfo(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")

	var (
		config              = &types.AuthConfig{}
		authEncoded         = r.Header.Get("X-Registry-Auth")
		distributionInspect registrytypes.DistributionInspect
	)

	if authEncoded != "" {
		authJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
		if err := json.NewDecoder(authJSON).Decode(&config); err != nil {
			// for a search it is not an error if no auth was given
			// to increase compatibility with the existing api it is defaulting to be empty
			config = &types.AuthConfig{}
		}
	}

	image := vars["name"]

	// TODO why is reference.ParseAnyReference() / reference.ParseNormalizedNamed() not using the reference.ErrTagInvalidFormat (and so on) errors?
	ref, err := reference.ParseAnyReference(image)
	if err != nil {
		return errdefs.InvalidParameter(err)
	}
	//namedRef, ok := ref.(reference.Named)
	_, ok := ref.(reference.Named)
	if !ok {
		if _, ok := ref.(reference.Digested); ok {
			// full image ID
			return errors.Errorf("no manifest found for full image ID")
		}
		return errdefs.InvalidParameter(errors.Errorf("unknown image reference format: %s", image))
	}

	// TODO: Just pull manifest blob, done...

	// TODO: Umm, get rid of this and just use images.Manifest

	//// retrieve platform information depending on the type of manifest
	//switch mnfstObj := mnfst.(type) {
	//case *manifestlist.DeserializedManifestList:
	//	for _, m := range mnfstObj.Manifests {
	//		distributionInspect.Platforms = append(distributionInspect.Platforms, v1.Platform{
	//			Architecture: m.Platform.Architecture,
	//			OS:           m.Platform.OS,
	//			OSVersion:    m.Platform.OSVersion,
	//			OSFeatures:   m.Platform.OSFeatures,
	//			Variant:      m.Platform.Variant,
	//		})
	//	}
	//case *schema2.DeserializedManifest:
	//	configJSON, err := blobsrvc.Get(ctx, mnfstObj.Config.Digest)
	//	var platform v1.Platform
	//	if err == nil {
	//		err := json.Unmarshal(configJSON, &platform)
	//		if err == nil && (platform.OS != "" || platform.Architecture != "") {
	//			distributionInspect.Platforms = append(distributionInspect.Platforms, platform)
	//		}
	//	}
	//case *schema1.SignedManifest:
	//	platform := v1.Platform{
	//		Architecture: mnfstObj.Architecture,
	//		OS:           "linux",
	//	}
	//	distributionInspect.Platforms = append(distributionInspect.Platforms, platform)
	//}

	return httputils.WriteJSON(w, http.StatusOK, distributionInspect)
}

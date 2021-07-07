package images // import "github.com/docker/docker/daemon/images"

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder/dockerfile"
	"github.com/docker/docker/builder/remotecontext"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ImportImage imports an image, getting the archived layer data either from
// inConfig (if src is "-"), or from a URI specified in src. Progress output is
// written to outStream. Repository and tag names can optionally be given in
// the repo and tag arguments, respectively.
func (i *ImageService) ImportImage(src string, repository, os string, tag string, msg string, inConfig io.ReadCloser, outStream io.Writer, changes []string) error {
	var (
		rc     io.ReadCloser
		resp   *http.Response
		newRef reference.Named
	)

	// Default the operating system if not supplied.
	if os == "" {
		os = runtime.GOOS
	}

	if repository != "" {
		var err error
		newRef, err = reference.ParseNormalizedNamed(repository)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		if _, isCanonical := newRef.(reference.Canonical); isCanonical {
			return errdefs.InvalidParameter(errors.New("cannot import digest reference"))
		}

		if tag != "" {
			newRef, err = reference.WithTag(newRef, tag)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
		}
	}

	config, err := dockerfile.BuildFromConfig(&container.Config{}, changes, os)
	if err != nil {
		return err
	}
	if src == "-" {
		rc = inConfig
	} else {
		inConfig.Close()
		if len(strings.Split(src, "://")) == 1 {
			src = "http://" + src
		}
		u, err := url.Parse(src)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}

		resp, err = remotecontext.GetWithStatusError(u.String())
		if err != nil {
			return err
		}
		outStream.Write(streamformatter.FormatStatus("", "Downloading from %s", u))
		progressOutput := streamformatter.NewJSONProgressOutput(outStream, true)
		rc = progress.NewProgressReader(resp.Body, progressOutput, resp.ContentLength, "", "Importing")
	}

	defer rc.Close()
	if len(msg) == 0 {
		msg = "Imported from " + src
	}

	inflatedLayerData, err := archive.DecompressStream(rc)
	if err != nil {
		return err
	}
	l, err := i.layerStore.Register(inflatedLayerData, "")
	if err != nil {
		return err
	}
	defer layer.ReleaseAndLog(i.layerStore, l)

	created := time.Now().UTC()
	imgConfig, err := json.Marshal(&image.Image{
		V1Image: image.V1Image{
			DockerVersion: dockerversion.Version,
			Config:        config,
			Platform: ocispec.Platform{
				Architecture: runtime.GOARCH,
				OS:           os,
			},
			Created: created,
			Comment: msg,
		},
		RootFS: &image.RootFS{
			Type:    "layers",
			DiffIDs: []layer.DiffID{l.DiffID()},
		},
		History: []image.History{{
			Created: created,
			Comment: msg,
		}},
	})
	if err != nil {
		return err
	}

	id, err := i.imageStore.Create(imgConfig)
	if err != nil {
		return err
	}

	// FIXME: connect with commit code and call refstore directly
	if newRef != nil {
		if err := i.TagImageWithReference(id, newRef); err != nil {
			return err
		}
	}

	i.LogImageEvent(id.String(), id.String(), "import")
	outStream.Write(streamformatter.FormatStatus("", id.String()))
	return nil
}

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"strings"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/docker/distribution/registry/api/errcode"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type dockerFetcher struct {
	*dockerBase
}

func (r dockerFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(
		logrus.Fields{
			"base":   r.base.String(),
			"digest": desc.Digest,
		},
	))

	urls, err := r.getV2URLPaths(ctx, desc)
	if err != nil {
		return nil, err
	}

	ctx, err = contextWithRepositoryScope(ctx, r.refspec, false)
	if err != nil {
		return nil, err
	}

	return newHTTPReadSeeker(desc.Size, func(offset int64) (io.ReadCloser, error) {
		for _, u := range urls {
			rc, err := r.open(ctx, u, desc.MediaType, offset)
			if err != nil {
				if errdefs.IsNotFound(err) {
					continue // try one of the other urls.
				}

				return nil, err
			}

			return rc, nil
		}

		return nil, errors.Wrapf(errdefs.ErrNotFound,
			"could not fetch content descriptor %v (%v) from remote",
			desc.Digest, desc.MediaType)

	})
}

func (r dockerFetcher) open(ctx context.Context, u, mediatype string, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", strings.Join([]string{mediatype, `*`}, ", "))

	if offset > 0 {
		// Note: "Accept-Ranges: bytes" cannot be trusted as some endpoints
		// will return the header without supporting the range. The content
		// range must always be checked.
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := r.doRequestWithRetries(ctx, req, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode > 299 {
		// TODO(stevvooe): When doing a offset specific request, we should
		// really distinguish between a 206 and a 200. In the case of 200, we
		// can discard the bytes, hiding the seek behavior from the
		// implementation.
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil, errors.Wrapf(errdefs.ErrNotFound, "content at %v not found", u)
		}
		var registryErr errcode.Errors
		if err := json.NewDecoder(resp.Body).Decode(&registryErr); err != nil || registryErr.Len() < 1 {
			return nil, errors.Errorf("unexpected status code %v: %v", u, resp.Status)
		}
		return nil, errors.Errorf("unexpected status code %v: %s - Server message: %s", u, resp.Status, registryErr.Error())
	}
	if offset > 0 {
		cr := resp.Header.Get("content-range")
		if cr != "" {
			if !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
				return nil, errors.Errorf("unhandled content range in response: %v", cr)

			}
		} else {
			// TODO: Should any cases where use of content range
			// without the proper header be considered?
			// 206 responses?

			// Discard up to offset
			// Could use buffer pool here but this case should be rare
			n, err := io.Copy(ioutil.Discard, io.LimitReader(resp.Body, offset))
			if err != nil {
				return nil, errors.Wrap(err, "failed to discard to offset")
			}
			if n != offset {
				return nil, errors.Errorf("unable to discard to offset")
			}

		}
	}

	return resp.Body, nil
}

// getV2URLPaths generates the candidate urls paths for the object based on the
// set of hints and the provided object id. URLs are returned in the order of
// most to least likely succeed.
func (r *dockerFetcher) getV2URLPaths(ctx context.Context, desc ocispec.Descriptor) ([]string, error) {
	var urls []string

	if len(desc.URLs) > 0 {
		// handle fetch via external urls.
		for _, u := range desc.URLs {
			log.G(ctx).WithField("url", u).Debug("adding alternative url")
			urls = append(urls, u)
		}
	}

	switch desc.MediaType {
	case images.MediaTypeDockerSchema2Manifest, images.MediaTypeDockerSchema2ManifestList,
		images.MediaTypeDockerSchema1Manifest,
		ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex:
		urls = append(urls, r.url(path.Join("manifests", desc.Digest.String())))
	}

	// always fallback to attempting to get the object out of the blobs store.
	urls = append(urls, r.url(path.Join("blobs", desc.Digest.String())))

	return urls, nil
}

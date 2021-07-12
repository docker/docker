package client // import "github.com/docker/docker/client"

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
)

// ImageCreate creates a new image based on the parent options.
// It returns the JSON content in the response body.
func (cli *Client) ImageCreate(ctx context.Context, parentReference string, options types.ImageCreateOptions) (io.ReadCloser, error) {
	ref, err := reference.ParseNormalizedNamed(parentReference)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("fromImage", reference.FamiliarName(ref))
	query.Set("tag", getAPITagFromNamedRef(ref))
	if options.Platform != "" {
		query.Set("platform", strings.ToLower(options.Platform))
	}
	resp, err := cli.tryImageCreate(ctx, query, nil /* extra headers */, options.RegistryAuth)
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

func (cli *Client) tryImageCreate(ctx context.Context, query url.Values, extraHeaders http.Header, registryAuth string) (serverResponse, error) {
	// Careful not to mutate extraHeaders.
	headers := make(map[string][]string, len(extraHeaders))
	headers["X-Registry-Auth"] = []string{registryAuth}
	for key, values := range extraHeaders {
		if vs, ok := headers[key]; ok {
			headers[key] = append(vs, values...)
		} else {
			headers[key] = values
		}
	}
	return cli.post(ctx, "/images/create", query, nil, headers)
}

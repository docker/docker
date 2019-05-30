package client // import "github.com/docker/docker/client"

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func TestSecretUpdateUnsupported(t *testing.T) {
	client := &Client{
		version: "1.24",
		client:  &http.Client{},
	}
	err := client.SecretUpdate(context.Background(), "secret_id", swarm.Version{}, swarm.SecretSpec{})
	assert.Check(t, is.Error(err, `"secret update" requires API version 1.25, but the Docker daemon API version is 1.24`))
}

func TestSecretUpdateError(t *testing.T) {
	client := &Client{
		version: "1.25",
		client:  newMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	}

	err := client.SecretUpdate(context.Background(), "secret_id", swarm.Version{}, swarm.SecretSpec{})
	if err == nil || err.Error() != "Error response from daemon: Server error" {
		t.Fatalf("expected a Server Error, got %v", err)
	}
	if !errdefs.IsSystem(err) {
		t.Fatalf("expected a Server Error, got %T", err)
	}
}

func TestSecretUpdate(t *testing.T) {
	expectedURL := "/v1.25/secrets/secret_id/update"

	client := &Client{
		version: "1.25",
		client: newMockClient(func(req *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(req.URL.Path, expectedURL) {
				return nil, fmt.Errorf("Expected URL '%s', got '%s'", expectedURL, req.URL)
			}
			if req.Method != "POST" {
				return nil, fmt.Errorf("expected POST method, got %s", req.Method)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte("body"))),
			}, nil
		}),
	}

	err := client.SecretUpdate(context.Background(), "secret_id", swarm.Version{}, swarm.SecretSpec{})
	if err != nil {
		t.Fatal(err)
	}
}

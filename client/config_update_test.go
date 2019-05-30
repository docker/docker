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

func TestConfigUpdateUnsupported(t *testing.T) {
	client := &Client{
		version: "1.29",
		client:  &http.Client{},
	}
	err := client.ConfigUpdate(context.Background(), "config_id", swarm.Version{}, swarm.ConfigSpec{})
	assert.Check(t, is.Error(err, `"config update" requires API version 1.30, but the Docker daemon API version is 1.29`))
}

func TestConfigUpdateError(t *testing.T) {
	client := &Client{
		version: "1.30",
		client:  newMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	}

	err := client.ConfigUpdate(context.Background(), "config_id", swarm.Version{}, swarm.ConfigSpec{})
	if err == nil || err.Error() != "Error response from daemon: Server error" {
		t.Fatalf("expected a Server Error, got %v", err)
	}
	if !errdefs.IsSystem(err) {
		t.Fatalf("expected a Server Error, got %T", err)
	}
}

func TestConfigUpdate(t *testing.T) {
	expectedURL := "/v1.30/configs/config_id/update"

	client := &Client{
		version: "1.30",
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

	err := client.ConfigUpdate(context.Background(), "config_id", swarm.Version{}, swarm.ConfigSpec{})
	if err != nil {
		t.Fatal(err)
	}
}

package client // import "github.com/docker/docker/client"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func TestConfigInspectNotFound(t *testing.T) {
	client := &Client{
		client: newMockClient(errorMock(http.StatusNotFound, "Server error")),
	}

	_, _, err := client.ConfigInspectWithRaw(context.Background(), "unknown")
	if err == nil || !IsErrNotFound(err) {
		t.Fatalf("expected a NotFoundError error, got %v", err)
	}
}

func TestConfigInspectWithEmptyID(t *testing.T) {
	client := &Client{
		client: newMockClient(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("should not make request")
		}),
	}
	_, _, err := client.ConfigInspectWithRaw(context.Background(), "")
	if !IsErrNotFound(err) {
		t.Fatalf("Expected NotFoundError, got %v", err)
	}
}

func TestConfigInspectUnsupported(t *testing.T) {
	client := &Client{
		version: "1.29",
		client:  &http.Client{},
	}
	_, _, err := client.ConfigInspectWithRaw(context.Background(), "nothing")
	assert.Check(t, is.Error(err, `"config inspect" requires API version 1.30, but the Docker daemon API version is 1.29`))
}

func TestConfigInspectError(t *testing.T) {
	client := &Client{
		version: "1.30",
		client:  newMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	}

	_, _, err := client.ConfigInspectWithRaw(context.Background(), "nothing")
	if err == nil || err.Error() != "Error response from daemon: Server error" {
		t.Fatalf("expected a Server Error, got %v", err)
	}
	if !errdefs.IsSystem(err) {
		t.Fatalf("expected a Server Error, got %T", err)
	}
}

func TestConfigInspectConfigNotFound(t *testing.T) {
	client := &Client{
		version: "1.30",
		client:  newMockClient(errorMock(http.StatusNotFound, "Server error")),
	}

	_, _, err := client.ConfigInspectWithRaw(context.Background(), "unknown")
	if err == nil || !IsErrNotFound(err) {
		t.Fatalf("expected a configNotFoundError error, got %v", err)
	}
}

func TestConfigInspect(t *testing.T) {
	expectedURL := "/v1.30/configs/config_id"
	client := &Client{
		version: "1.30",
		client: newMockClient(func(req *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(req.URL.Path, expectedURL) {
				return nil, fmt.Errorf("expected URL '%s', got '%s'", expectedURL, req.URL)
			}
			content, err := json.Marshal(swarm.Config{
				ID: "config_id",
			})
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader(content)),
			}, nil
		}),
	}

	configInspect, _, err := client.ConfigInspectWithRaw(context.Background(), "config_id")
	if err != nil {
		t.Fatal(err)
	}
	if configInspect.ID != "config_id" {
		t.Fatalf("expected `config_id`, got %s", configInspect.ID)
	}
}

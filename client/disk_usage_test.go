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

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/errdefs"
)

func TestDiskUsageError(t *testing.T) {
	client := &Client{
		client: newMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	}
	_, err := client.DiskUsage(context.Background())
	if !errdefs.IsSystem(err) {
		t.Fatalf("expected a Server Error, got %[1]T: %[1]v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	expectedURL := "/system/df"
	expectedRawQuery := ""
	client := &Client{
		client: newMockClient(func(req *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(req.URL.Path, expectedURL) {
				return nil, fmt.Errorf("Expected URL '%s', got '%s'", expectedURL, req.URL)
			}

			if !strings.Contains(req.URL.RawQuery, expectedRawQuery) {
				return nil, fmt.Errorf("Expected Query '%s', got '%s'", expectedRawQuery, req.URL.RawQuery)
			}

			du := types.DiskUsage{
				LayersSize: int64(100),
				Images:     nil,
				Containers: nil,
				Volumes:    nil,
				BuildCache: nil,
			}

			b, err := json.Marshal(du)
			if err != nil {
				return nil, err
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader(b)),
			}, nil
		}),
	}
	if _, err := client.DiskUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDiskUsageWithOptions(t *testing.T) {
	expectedURL := "/system/df"
	expectedRawQuery := "build-cache=0&containers=0&images=0&layer-size=0&volumes=0"
	client := &Client{
		client: newMockClient(func(req *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(req.URL.Path, expectedURL) {
				return nil, fmt.Errorf("Expected URL '%s', got '%s'", expectedURL, req.URL)
			}

			if !strings.Contains(req.URL.RawQuery, expectedRawQuery) {
				return nil, fmt.Errorf("Expected Query '%s', got '%s'", expectedRawQuery, req.URL.RawQuery)
			}

			du := types.DiskUsage{
				LayersSize: int64(100),
				Images:     nil,
				Containers: nil,
				Volumes:    nil,
				BuildCache: nil,
			}

			b, err := json.Marshal(du)
			if err != nil {
				return nil, err
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader(b)),
			}, nil
		}),
	}
	if _, err := client.DiskUsageWithOptions(context.Background(), types.DiskUsageOptions{
		NoContainers: true,
		NoImages:     true,
		NoVolumes:    true,
		NoLayerSize:  true,
		NoBuildCache: true,
	}); err != nil {
		t.Fatal(err)
	}
}

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

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
)

func TestImageHistoryError(t *testing.T) {
	client := &Client{
		client: newMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	}
	_, err := client.ImageHistory(context.Background(), "nothing")
	if err == nil || err.Error() != "Error response from daemon: Server error" {
		t.Fatalf("expected a Server error, got %v", err)
	}
	if !errdefs.IsSystem(err) {
		t.Fatalf("expected a Server Error, got %T", err)
	}
}

func TestImageHistory(t *testing.T) {
	expectedURL := "/images/image_id/history"
	client := &Client{
		client: newMockClient(func(r *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(r.URL.Path, expectedURL) {
				return nil, fmt.Errorf("Expected URL '%s', got '%s'", expectedURL, r.URL)
			}
			b, err := json.Marshal([]image.HistoryResponseItem{
				{
					ID:   "image_id1",
					Tags: []string{"tag1", "tag2"},
				},
				{
					ID:   "image_id2",
					Tags: []string{"tag1", "tag2"},
				},
			})
			if err != nil {
				return nil, err
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader(b)),
			}, nil
		}),
	}
	imageHistories, err := client.ImageHistory(context.Background(), "image_id")
	if err != nil {
		t.Fatal(err)
	}
	if len(imageHistories) != 2 {
		t.Fatalf("expected 2 containers, got %v", imageHistories)
	}
}

package image // import "github.com/docker/docker/image"

import (
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/layer"
	"github.com/google/go-cmp/cmp"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const sampleImageJSON = `{
	"architecture": "amd64",
	"os": "linux",
	"config": {},
	"rootfs": {
		"type": "layers",
		"diff_ids": []
	}
}`

func TestNewFromJSON(t *testing.T) {
	img, err := NewFromJSON([]byte(sampleImageJSON))
	assert.NilError(t, err)
	assert.Check(t, is.Equal(sampleImageJSON, string(img.RawJSON())))
}

func TestNewFromJSONWithInvalidJSON(t *testing.T) {
	_, err := NewFromJSON([]byte("{}"))
	assert.Check(t, is.Error(err, "invalid image JSON, no RootFS key"))
}

func TestMarshalKeyOrder(t *testing.T) {
	b, err := json.Marshal(&Image{
		V1Image: V1Image{
			Comment:  "a",
			Author:   "b",
			Platform: ocispec.Platform{Architecture: "c"},
		},
	})
	assert.Check(t, err)

	expectedOrder := []string{"architecture", "author", "comment"}
	var indexes []int
	for _, k := range expectedOrder {
		indexes = append(indexes, strings.Index(string(b), k))
	}

	if !sort.IntsAreSorted(indexes) {
		t.Fatal("invalid key order in JSON: ", string(b))
	}
}

const sampleHistoryJSON = `{
	"created": "2021-01-13T09:35:56Z",
	"created_by": "image_test.go"
}`

func TestHistoryEqual(t *testing.T) {
	h := historyFromJSON(t, sampleHistoryJSON)
	hCopy := h
	assert.Check(t, h.Equal(hCopy))

	hUTC := historyFromJSON(t, `{"created": "2021-01-13T14:00:00Z"}`)
	hOffset0 := historyFromJSON(t, `{"created": "2021-01-13T14:00:00+00:00"}`)
	assert.Check(t, hUTC.Created != hOffset0.Created)
	assert.Check(t, hUTC.Equal(hOffset0))
}

func historyFromJSON(t *testing.T, historyJSON string) History {
	var h History
	err := json.Unmarshal([]byte(historyJSON), &h)
	assert.Check(t, err)
	return h
}

func TestImage(t *testing.T) {
	cid := "50a16564e727"
	config := &container.Config{
		Hostname:   "hostname",
		Domainname: "domain",
		User:       "root",
	}
	os := runtime.GOOS

	img := &Image{
		V1Image: V1Image{
			Config: config,
		},
		computedID: ID(cid),
	}

	assert.Check(t, is.Equal(cid, img.ImageID()))
	assert.Check(t, is.Equal(cid, img.ID().String()))
	assert.Check(t, is.Equal(os, img.OperatingSystem()))
	assert.Check(t, is.DeepEqual(config, img.RunConfig()))
}

func TestImageOSNotEmpty(t *testing.T) {
	os := "os"
	img := &Image{
		V1Image: V1Image{
			Platform: ocispec.Platform{
				OS:        os,
				OSVersion: "osversion",
			},
		},
	}
	assert.Check(t, is.Equal(os, img.OperatingSystem()))
}

func TestNewChildImageFromImageWithRootFS(t *testing.T) {
	rootFS := NewRootFS()
	rootFS.Append(layer.DiffID("ba5e"))
	parent := &Image{
		RootFS: rootFS,
		History: []History{
			NewHistory("a", "c", "r", false),
		},
	}
	childConfig := ChildConfig{
		DiffID:  layer.DiffID("abcdef"),
		Author:  "author",
		Comment: "comment",
		ContainerConfig: &container.Config{
			Cmd: []string{"echo", "foo"},
		},
		Config: &container.Config{},
	}

	newImage := NewChildImage(parent, childConfig, "platform")
	expectedDiffIDs := []layer.DiffID{layer.DiffID("ba5e"), layer.DiffID("abcdef")}
	assert.Check(t, is.DeepEqual(expectedDiffIDs, newImage.RootFS.DiffIDs))
	assert.Check(t, is.Equal(childConfig.Author, newImage.Author))
	assert.Check(t, is.DeepEqual(childConfig.Config, newImage.Config))
	assert.Check(t, is.DeepEqual(*childConfig.ContainerConfig, newImage.ContainerConfig))
	assert.Check(t, is.Equal("platform", newImage.OS))
	assert.Check(t, is.DeepEqual(childConfig.Config, newImage.Config))

	assert.Check(t, is.Len(newImage.History, 2))
	assert.Check(t, is.Equal(childConfig.Comment, newImage.History[1].Comment))

	assert.Check(t, !cmp.Equal(parent.RootFS.DiffIDs, newImage.RootFS.DiffIDs),
		"RootFS should be copied not mutated")
}

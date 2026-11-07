package image

import (
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	testExposedPortRange      = "33060-33061/tcp"
	testExposedPortRangeStart = "33060/tcp"
	testExposedPortRangeEnd   = "33061/tcp"
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

func TestNewFromJSONWithExposedPortRange(t *testing.T) {
	imageJSON := `{
		"architecture": "amd64",
		"os": "linux",
		"config": {
			"ExposedPorts": {
				"` + testExposedPortRange + `": {}
			}
		},
		"rootfs": {
			"type": "layers",
			"diff_ids": []
		}
	}`

	img, err := NewFromJSON([]byte(imageJSON))
	assert.NilError(t, err)
	assert.Check(t, is.Equal(imageJSON, string(img.RawJSON())))
	assert.Check(t, is.Contains(img.Config.ExposedPorts, network.MustParsePort(testExposedPortRangeStart)))
	assert.Check(t, is.Contains(img.Config.ExposedPorts, network.MustParsePort(testExposedPortRangeEnd)))
}

func TestNewFromJSONWithInvalidJSON(t *testing.T) {
	_, err := NewFromJSON([]byte("{}"))
	assert.Check(t, is.Error(err, "invalid image JSON, no RootFS key"))
}

func TestMarshalKeyOrder(t *testing.T) {
	b, err := json.Marshal(&Image{
		V1Image: V1Image{
			Comment:      "a",
			Author:       "b",
			Architecture: "c",
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
			OS: os,
		},
		OSVersion: "osversion",
	}
	assert.Check(t, is.Equal(os, img.OperatingSystem()))
}

func TestNewChildImageFromImageWithRootFS(t *testing.T) {
	rootFS := NewRootFS()
	rootFS.Append("ba5e")
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
	expectedDiffIDs := []layer.DiffID{"ba5e", "abcdef"}
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

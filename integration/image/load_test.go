package image

import (
	"slices"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	iimage "github.com/moby/moby/v2/integration/internal/image"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

const (
	exposedPortRangeStart = "33060/tcp"
	exposedPortRangeEnd   = "33061/tcp"
)

func TestLoadDanglingImages(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	iimage.Load(ctx, t, apiClient, func(dir string) (*ocispec.Index, error) {
		return specialimage.MultiLayerCustom(dir, "namedimage:latest", []specialimage.SingleFileLayer{
			{Name: "bar", Content: []byte("1")},
		})
	})

	// Should be one image.
	imageList, err := apiClient.ImageList(ctx, client.ImageListOptions{})
	assert.NilError(t, err)

	findImageByName := func(images []image.Summary, imageName string) (image.Summary, error) {
		index := slices.IndexFunc(images, func(img image.Summary) bool {
			return slices.Index(img.RepoTags, imageName) >= 0
		})
		if index < 0 {
			return image.Summary{}, cerrdefs.ErrNotFound
		}
		return images[index], nil
	}

	oldImage, err := findImageByName(imageList.Items, "namedimage:latest")
	assert.NilError(t, err)

	// Retain a copy of the old image and then replace it with a new one.
	iimage.Load(ctx, t, apiClient, func(dir string) (*ocispec.Index, error) {
		return specialimage.MultiLayerCustom(dir, "namedimage:latest", []specialimage.SingleFileLayer{
			{Name: "bar", Content: []byte("2")},
		})
	})

	imageList, err = apiClient.ImageList(ctx, client.ImageListOptions{})
	assert.NilError(t, err)

	newImage, err := findImageByName(imageList.Items, "namedimage:latest")
	assert.NilError(t, err)

	// IDs should be different.
	assert.Check(t, oldImage.ID != newImage.ID)

	// Should be able to find the original digest.
	findImageById := func(images []image.Summary, imageId string) (image.Summary, error) {
		index := slices.IndexFunc(images, func(img image.Summary) bool {
			return img.ID == imageId
		})
		if index < 0 {
			return image.Summary{}, cerrdefs.ErrNotFound
		}
		return images[index], nil
	}

	danglingImage, err := findImageById(imageList.Items, oldImage.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Len(danglingImage.RepoTags, 0))
}

func TestLoadImageWithExposedPortRange(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	imageRef := iimage.Load(ctx, t, apiClient, specialimage.ExposedPortRange)

	createResp, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           &containertypes.Config{Image: imageRef},
		HostConfig:       &containertypes.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(t, err)
	t.Cleanup(func() {
		_, err := apiClient.ContainerRemove(ctx, createResp.ID, client.ContainerRemoveOptions{Force: true})
		assert.Check(t, err)
	})

	inspect, err := apiClient.ContainerInspect(ctx, createResp.ID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Contains(inspect.Container.Config.ExposedPorts, network.MustParsePort(exposedPortRangeStart)))
	assert.Check(t, is.Contains(inspect.Container.Config.ExposedPorts, network.MustParsePort(exposedPortRangeEnd)))
}

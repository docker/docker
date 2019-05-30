package volume

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/request"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

func TestVolumesCreateAndList(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	name := t.Name()
	vol, err := client.VolumeCreate(ctx, volumetypes.VolumeCreateBody{
		Name: name,
	})
	assert.NilError(t, err)

	expected := types.Volume{
		// Ignore timestamp of CreatedAt
		CreatedAt:  vol.CreatedAt,
		Driver:     "local",
		Scope:      "local",
		Name:       name,
		Mountpoint: filepath.Join(testEnv.DaemonInfo.DockerRootDir, "volumes", name, "_data"),
	}
	assert.Check(t, is.DeepEqual(vol, expected, cmpopts.EquateEmpty()))

	volList, err := client.VolumeList(ctx, filters.Args{})
	assert.NilError(t, err)
	assert.Assert(t, len(volList.Volumes) > 0)

	volumes := volList.Volumes[:0]
	for _, v := range volList.Volumes {
		if v.Name == vol.Name {
			volumes = append(volumes, v)
		}
	}

	assert.Check(t, is.Equal(len(volumes), 1))
	assert.Check(t, volumes[0] != nil)
	assert.Check(t, is.DeepEqual(*volumes[0], expected, cmpopts.EquateEmpty()))
}

func TestVolumesRemove(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows", "FIXME")
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	prefix, slash := getPrefixAndSlashFromDaemonPlatform()

	id := container.Create(t, ctx, client, container.WithVolume(prefix+slash+"foo"))

	c, err := client.ContainerInspect(ctx, id)
	assert.NilError(t, err)
	vname := c.Mounts[0].Name

	err = client.VolumeRemove(ctx, vname, false)
	assert.Check(t, is.ErrorContains(err, "volume is in use"))

	err = client.ContainerRemove(ctx, id, types.ContainerRemoveOptions{
		Force: true,
	})
	assert.NilError(t, err)

	err = client.VolumeRemove(ctx, vname, false)
	assert.NilError(t, err)
}

func TestVolumesInspect(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	now := time.Now()
	vol, err := client.VolumeCreate(ctx, volumetypes.VolumeCreateBody{})
	assert.NilError(t, err)

	inspected, err := client.VolumeInspect(ctx, vol.Name)
	assert.NilError(t, err)

	assert.Check(t, is.DeepEqual(inspected, vol, cmpopts.EquateEmpty()))

	// comparing CreatedAt field time for the new volume to now. Truncate to 1 minute precision to avoid false positive
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(inspected.CreatedAt))
	assert.NilError(t, err)
	assert.Check(t, createdAt.Truncate(time.Minute).Equal(now.Truncate(time.Minute)), "CreatedAt (%s) not equal to creation time (%s)", createdAt, now)
}

func TestVolumesInvalidJSON(t *testing.T) {
	defer setupTest(t)()

	endpoints := []string{"/volumes/create"}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			t.Parallel()

			res, body, err := request.Post(ep, request.RawString("{invalid json"), request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, http.StatusBadRequest)

			buf, err := request.ReadBody(body)
			assert.NilError(t, err)
			assert.Check(t, is.Contains(string(buf), "invalid character 'i' looking for beginning of object key string"))

			res, body, err = request.Post(ep, request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, http.StatusBadRequest)

			buf, err = request.ReadBody(body)
			assert.NilError(t, err)
			assert.Check(t, is.Contains(string(buf), "got EOF while reading request body"))
		})
	}
}

func getPrefixAndSlashFromDaemonPlatform() (prefix, slash string) {
	if testEnv.OSType == "windows" {
		return "c:", `\`
	}
	return "", "/"
}

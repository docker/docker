package oci

import (
	"os"
	"path/filepath"
	"testing"

	coci "github.com/containerd/containerd/v2/pkg/oci"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestDevicesFromPathResolvesSymlinkedDevicesInDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	deviceDir := filepath.Join(tmpDir, "by-uuid")
	deviceLinkPath := filepath.Join(deviceDir, "linked-device")

	expectedDevice, err := coci.DeviceFromPath("/dev/null")
	assert.NilError(t, err)

	assert.NilError(t, os.Mkdir(deviceDir, 0o755))
	assert.NilError(t, os.Symlink("/dev/null", deviceLinkPath))

	devs, permissions, err := DevicesFromPath(deviceDir, "/dev/disk/by-uuid", "rwm")
	assert.NilError(t, err)
	assert.Check(t, is.Len(devs, 1))
	assert.Check(t, is.Len(permissions, 1))

	assert.Equal(t, devs[0].Path, "/dev/disk/by-uuid/linked-device")
	assert.Equal(t, devs[0].Type, expectedDevice.Type)
	assert.Equal(t, devs[0].Major, expectedDevice.Major)
	assert.Equal(t, devs[0].Minor, expectedDevice.Minor)
	assert.Equal(t, permissions[0].Access, "rwm")
	assert.Equal(t, permissions[0].Type, expectedDevice.Type)
	assert.Equal(t, *permissions[0].Major, expectedDevice.Major)
	assert.Equal(t, *permissions[0].Minor, expectedDevice.Minor)
}

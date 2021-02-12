// +build !windows

package image // import "github.com/docker/docker/integration/image"

import (
	"context"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/internal/test/daemon"
	"github.com/docker/docker/internal/test/fakecontext"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

// This is a regression test for #38488
// It ensures that orphan layers can be found and cleaned up
// after unsuccessful image removal
func TestRemoveImageGarbageCollector(t *testing.T) {
	// This test uses very platform specific way to prevent
	// daemon for remove image layer.
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, os.Getenv("DOCKER_ENGINE_GOARCH") != "amd64")

	// Create daemon with overlay2 graphdriver because vfs uses disk differently
	// and this test case would not work with it.
	d := daemon.New(t, daemon.WithStorageDriver("overlay2"), daemon.WithImageService)
	d.Start(t)
	defer d.Stop(t)

	ctx := context.Background()
	client := d.NewClientT(t)
	i := d.ImageService()

	img := "test-garbage-collector"

	// Build a image with multiple layers
	dockerfile := `FROM busybox
	RUN echo echo Running... > /run.sh`
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()
	resp, err := client.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{img},
		})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	image, _, err := client.ImageInspectWithRaw(ctx, img)
	assert.NilError(t, err)

	// Mark latest image layer to immutable
	data := image.GraphDriver.Data
	file, _ := os.Open(data["UpperDir"])
	attr := 0x00000010
	fsflags := uintptr(0x40086602)
	argp := uintptr(unsafe.Pointer(&attr))
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), fsflags, argp)
	assert.Equal(t, "errno 0", errno.Error())

	// Try to remove the image, it should generate error
	// but marking layer back to mutable before checking errors (so we don't break CI server)
	_, err = client.ImageRemove(ctx, img, types.ImageRemoveOptions{})
	attr = 0x00000000
	argp = uintptr(unsafe.Pointer(&attr))
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), fsflags, argp)
	assert.Equal(t, "errno 0", errno.Error())
	assert.Assert(t, err != nil)
	errStr := err.Error()
	if !(strings.Contains(errStr, "permission denied") || strings.Contains(errStr, "operation not permitted")) {
		t.Errorf("ImageRemove error not an permission error %s", errStr)
	}

	// Verify that layer remaining on disk
	dir, _ := os.Stat(data["UpperDir"])
	assert.Equal(t, "true", strconv.FormatBool(dir.IsDir()))

	// Run imageService.Cleanup() and make sure that layer was removed from disk
	i.Cleanup()
	dir, err = os.Stat(data["UpperDir"])
	assert.ErrorContains(t, err, "no such file or directory")
}

package image // import "github.com/docker/docker/integration/image"

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

// Ensure we don't regress on CVE-2017-14992.
func TestImportExtremelyLargeImageWorks(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, runtime.GOARCH == "arm64", "effective test will be time out")
	skip.If(t, testEnv.OSType == "windows", "TODO enable on windows")
	t.Parallel()

	// Spin up a new daemon, so that we can run this test in parallel (it's a slow test)
	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)

	client := d.NewClientT(t)

	// Construct an empty tar archive with about 8GB of junk padding at the
	// end. This should not cause any crashes (the padding should be mostly
	// ignored).
	var tarBuffer bytes.Buffer

	tw := tar.NewWriter(&tarBuffer)
	err := tw.Close()
	assert.NilError(t, err)
	imageRdr := io.MultiReader(&tarBuffer, io.LimitReader(testutil.DevZero, 8*1024*1024*1024))

	_, err = client.ImageImport(context.Background(),
		types.ImageImportSource{Source: imageRdr, SourceName: "-"},
		"test1234:v42",
		types.ImageImportOptions{})
	assert.NilError(t, err)
}

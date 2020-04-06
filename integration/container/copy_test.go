package container // import "github.com/docker/docker/integration/container"

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/testutil/fakecontext"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

func TestCopyFromContainerPathDoesNotExist(t *testing.T) {
	defer setupTest(t)()
	skip.If(t, testEnv.OSType == "windows")

	ctx := context.Background()
	apiclient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiclient)

	_, _, err := apiclient.CopyFromContainer(ctx, cid, "/dne")
	assert.Check(t, client.IsErrNotFound(err))
	expected := fmt.Sprintf("No such container:path: %s:%s", cid, "/dne")
	assert.Check(t, is.ErrorContains(err, expected))
}

func TestCopyFromContainerPathIsNotDir(t *testing.T) {
	defer setupTest(t)()
	skip.If(t, testEnv.OSType == "windows")

	ctx := context.Background()
	apiclient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiclient)

	_, _, err := apiclient.CopyFromContainer(ctx, cid, "/etc/passwd/")
	assert.Assert(t, is.ErrorContains(err, "not a directory"))
}

func TestCopyToContainerPathDoesNotExist(t *testing.T) {
	defer setupTest(t)()
	skip.If(t, testEnv.OSType == "windows")

	ctx := context.Background()
	apiclient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiclient)

	err := apiclient.CopyToContainer(ctx, cid, "/dne", nil, types.CopyToContainerOptions{})
	assert.Check(t, client.IsErrNotFound(err))
	expected := fmt.Sprintf("No such container:path: %s:%s", cid, "/dne")
	assert.Check(t, is.ErrorContains(err, expected))
}

func TestCopyToContainerPathIsNotDir(t *testing.T) {
	defer setupTest(t)()
	skip.If(t, testEnv.OSType == "windows")

	ctx := context.Background()
	apiclient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiclient)

	err := apiclient.CopyToContainer(ctx, cid, "/etc/passwd/", nil, types.CopyToContainerOptions{})
	assert.Assert(t, is.ErrorContains(err, "not a directory"))
}

func TestCopyFromContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()

	ctx := context.Background()
	apiClient := testEnv.APIClient()

	dir, err := ioutil.TempDir("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(dir)

	buildCtx := fakecontext.New(t, dir, fakecontext.WithFile("foo", "hello"), fakecontext.WithFile("baz", "world"), fakecontext.WithDockerfile(`
		FROM busybox
		COPY foo /foo
		COPY baz /bar/quux/baz
		RUN ln -s notexist /bar/notarget && ln -s quux/baz /bar/filesymlink && ln -s quux /bar/dirsymlink && ln -s / /bar/root
		CMD /fake
	`))
	defer buildCtx.Close()

	resp, err := apiClient.ImageBuild(ctx, buildCtx.AsTarReader(t), types.ImageBuildOptions{})
	assert.NilError(t, err)
	defer resp.Body.Close()

	var imageID string
	err = jsonmessage.DisplayJSONMessagesStream(resp.Body, ioutil.Discard, 0, false, func(msg jsonmessage.JSONMessage) {
		var r types.BuildResult
		assert.NilError(t, json.Unmarshal(*msg.Aux, &r))
		imageID = r.ID
	})
	assert.NilError(t, err)
	assert.Assert(t, imageID != "")

	cid := container.Create(ctx, t, apiClient, container.WithImage(imageID))

	for _, x := range []struct {
		src    string
		expect map[string]string
	}{
		{"/", map[string]string{"/": "", "/foo": "hello", "/bar/quux/baz": "world", "/bar/filesymlink": "", "/bar/dirsymlink": "", "/bar/notarget": ""}},
		{"/bar/root", map[string]string{"root": ""}},
		{"/bar/root/", map[string]string{"root/": "", "root/foo": "hello", "root/bar/quux/baz": "world", "root/bar/filesymlink": "", "root/bar/dirsymlink": "", "root/bar/notarget": ""}},

		{"bar/quux", map[string]string{"quux/": "", "quux/baz": "world"}},
		{"bar/quux/", map[string]string{"quux/": "", "quux/baz": "world"}},
		{"bar/quux/baz", map[string]string{"baz": "world"}},

		{"bar/filesymlink", map[string]string{"filesymlink": ""}},
		{"bar/dirsymlink", map[string]string{"dirsymlink": ""}},
		{"bar/dirsymlink/", map[string]string{"dirsymlink/": "", "dirsymlink/baz": "world"}},
		{"bar/notarget", map[string]string{"notarget": ""}},
	} {
		t.Run(x.src, func(t *testing.T) {
			rdr, _, err := apiClient.CopyFromContainer(ctx, cid, x.src)
			assert.NilError(t, err)
			defer rdr.Close()

			found := make(map[string]bool, len(x.expect))
			var numFound int
			tr := tar.NewReader(rdr)
			for numFound < len(x.expect) {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				assert.NilError(t, err)

				expected, exists := x.expect[h.Name]
				if !exists {
					// this archive will have extra stuff in it since we are copying from root
					// and docker adds a bunch of stuff
					continue
				}

				numFound++
				found[h.Name] = true

				buf, err := ioutil.ReadAll(tr)
				if err == nil {
					assert.Check(t, is.Equal(string(buf), expected))
				}
			}

			for f := range x.expect {
				assert.Check(t, found[f], f+" not found in archive")
			}
		})
	}
}

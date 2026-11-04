package container

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/go-archive"
	buildtypes "github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/moby/moby/v2/integration/internal/build"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestCopyFromContainerPathDoesNotExist(t *testing.T) {
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiClient)

	_, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: "/dne"})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	assert.Check(t, is.ErrorContains(err, "Could not find the file /dne in container "+cid))
}

// TestCopyFromContainerPathIsNotDir tests that an error is returned when
// trying to create a directory on a path that's a file.
func TestCopyFromContainerPathIsNotDir(t *testing.T) {
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiClient)

	// Pick a path that already exists as a file; on Linux "/etc/passwd"
	// is expected to be there, so we pick that for convenience.
	existingFile := "/etc/passwd"
	existingDir := "/etc"
	expected := []string{"not a directory"}
	if testEnv.DaemonInfo.OSType == "windows" {
		existingFile = "c:/windows/system32/drivers/etc/hosts"
		existingDir = "c:/windows/system32/drivers/etc"
		expected = append(expected,
			"The directory name is invalid.",                                     // ERROR_DIRECTORY
			"The filename, directory name, or volume label syntax is incorrect.", // ERROR_INVALID_NAME
		)
	}

	isNotADirErr := func(err error) bool {
		for _, e := range expected {
			if err != nil && strings.Contains(err.Error(), e) {
				return true
			}
		}
		return false
	}

	t.Run("file with trailing separator errors", func(t *testing.T) {
		_, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: existingFile + "/"})
		assert.Check(t, isNotADirErr(err), "expected a 'not a directory' error, but got %v", err)
	})

	t.Run("file with trailing dot errors", func(t *testing.T) {
		// The graphdriver surfaces this as a not-found error rather than
		// "not a directory", so we only assert that the copy fails.
		_, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: existingFile + "/."})
		assert.Check(t, err != nil, "expected an error for a file path asserting a directory")
	})

	t.Run("file without trailing separator is ok", func(t *testing.T) {
		res, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: existingFile})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.Stat.Mode.IsDir(), false))
		_, err = io.Copy(io.Discard, res.Content)
		assert.NilError(t, err)
		assert.NilError(t, res.Content.Close())
	})

	t.Run("directory with trailing separator is ok", func(t *testing.T) {
		res, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: existingDir + "/"})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.Stat.Mode.IsDir(), true))
		_, err = io.Copy(io.Discard, res.Content)
		assert.NilError(t, err)
		assert.NilError(t, res.Content.Close())
	})
}

// TestCopyFromContainerDirSymlinkTrailingSeparator is a regression test for
// moby/moby#47107: a symlink copied with a trailing separator (or "/.") is
// resolved to its target first, so a directory symlink is accepted while a file
// symlink is still rejected. The check is in the Windows StatPath, so this is
// Windows-only; Linux is covered by TestCopyFromContainer ("bar/dirsymlink/").
func TestCopyFromContainerDirSymlinkTrailingSeparator(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "windows")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	// mklink requires SeCreateSymbolicLinkPrivilege, hence ContainerAdministrator.
	buildCtx := fakecontext.New(t, t.TempDir(), fakecontext.WithDockerfile(`
		FROM `+testEnv.PlatformDefaults.BaseImage+`
		USER ContainerAdministrator
		RUN cmd /c mklink /D C:\linkdir C:\Windows\System32\drivers\etc
		RUN cmd /c mklink C:\filelink C:\Windows\System32\drivers\etc\hosts
	`))
	defer buildCtx.Close()

	imageID := build.Do(ctx, t, apiClient, buildCtx, client.ImageBuildOptions{})
	cid := container.Create(ctx, t, apiClient, container.WithImage(imageID))

	// A directory symlink resolves to a directory and must be accepted. Only the
	// trailing-separator form is reliable across drivers: windowsfilter's
	// os.Lstat rejects a trailing "/." even on a real directory.
	t.Run("c:/linkdir/", func(t *testing.T) {
		res, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: "c:/linkdir/"})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.Stat.Mode.IsDir(), true))
		_, err = io.Copy(io.Discard, res.Content)
		assert.NilError(t, err)
		assert.NilError(t, res.Content.Close())
	})

	// A file symlink resolves to a file and must be rejected (error varies by driver).
	for _, src := range []string{"c:/filelink/", "c:/filelink/."} {
		t.Run(src, func(t *testing.T) {
			_, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: src})
			assert.Check(t, err != nil, "expected an error for a file symlink asserting a directory")
		})
	}
}

func TestCopyToContainerPathDoesNotExist(t *testing.T) {
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiClient)

	_, err := apiClient.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: "/dne", Content: bytes.NewReader([]byte(""))})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	assert.Check(t, is.ErrorContains(err, "Could not find the file /dne in container "+cid))
}

func TestCopyEmptyFile(t *testing.T) {
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiClient)

	// empty content
	dstDir, _ := makeEmptyArchive(t)
	_, err := apiClient.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: dstDir, Content: bytes.NewReader([]byte(""))})
	assert.NilError(t, err)

	// tar with empty file
	dstDir, preparedArchive := makeEmptyArchive(t)
	_, err = apiClient.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: dstDir, Content: preparedArchive})
	assert.NilError(t, err)

	// tar with empty file archive mode
	dstDir, preparedArchive = makeEmptyArchive(t)
	_, err = apiClient.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: dstDir, Content: preparedArchive, CopyUIDGID: true})
	assert.NilError(t, err)

	// copy from empty file
	res, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: dstDir})
	rdr := res.Content
	assert.NilError(t, err)
	defer rdr.Close()
}

func TestCopyToContainerCopyUIDGID(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	imageID := makeTestImage(ctx, t)

	tests := []struct {
		doc      string
		user     string
		expected string
	}{
		{
			doc:      "image default",
			expected: "2375:2376",
		},
		{
			// Align with behavior of docker run, which treats a UID with
			// empty groupname as default (0 (root)).
			//
			//	docker run --rm --user "7777:" alpine id
			//	uid=7777 gid=0(root) groups=0(root)
			doc:      "trailing colon",
			user:     "7777:",
			expected: "7777:0",
		},
		{
			// Align with behavior of docker run, which treats a GID with
			// empty username as default (0 (root)).
			//
			//	docker run --rm --user ":7777" alpine id
			//	uid=0(root) gid=7777 groups=7777
			doc:      "leading colon",
			user:     ":7777",
			expected: "0:7777",
		},
		{
			doc:      "known UID",
			user:     "2375",
			expected: "2375:2376",
		},
		{
			doc:      "unknown UID",
			user:     "7777",
			expected: "7777:0",
		},
		{
			doc:      "UID and GID",
			user:     "2375:2376",
			expected: "2375:2376",
		},
		{
			doc:      "username and groupname",
			user:     "testuser:testgroup",
			expected: "2375:2376",
		},
		{
			doc:      "username",
			user:     "testuser",
			expected: "2375:2376",
		},
		{
			doc:      "username and GID",
			user:     "testuser:7777",
			expected: "2375:7777",
		},
		{
			doc:      "UID and groupname",
			user:     "7777:testgroup",
			expected: "7777:2376",
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()

			cID := container.Run(ctx, t, apiClient, container.WithImage(imageID), container.WithUser(tc.user))
			defer container.Remove(ctx, t, apiClient, cID, client.ContainerRemoveOptions{Force: true})

			// tar with empty file
			dstDir, preparedArchive := makeEmptyArchive(t)
			_, err := apiClient.CopyToContainer(ctx, cID, client.CopyToContainerOptions{DestinationPath: dstDir, Content: preparedArchive, CopyUIDGID: true})
			assert.NilError(t, err)

			res, err := container.Exec(ctx, apiClient, cID, []string{"stat", "-c", "%u:%g", "/empty-file.txt"})
			assert.NilError(t, err)
			assert.Equal(t, res.ExitCode, 0)
			assert.Equal(t, strings.TrimSpace(res.Stdout()), tc.expected)
		})
	}
}

func makeTestImage(ctx context.Context, t *testing.T) (imageID string) {
	t.Helper()
	apiClient := testEnv.APIClient()
	tmpDir := t.TempDir()
	buildCtx := fakecontext.New(t, tmpDir, fakecontext.WithDockerfile(`
		FROM busybox
		RUN addgroup -g 2376 testgroup && adduser -D -u 2375 -G testgroup testuser
		USER testuser:testgroup
	`))
	defer buildCtx.Close()

	resp, err := apiClient.ImageBuild(ctx, buildCtx.AsTarReader(t), client.ImageBuildOptions{})
	assert.NilError(t, err)
	defer resp.Body.Close()

	err = jsonmessage.DisplayStream(resp.Body, io.Discard, jsonmessage.WithAuxCallback(func(msg jsonstream.Message) {
		var r buildtypes.Result
		assert.NilError(t, json.Unmarshal(*msg.Aux, &r))
		imageID = r.ID
	}))
	assert.NilError(t, err)
	assert.Assert(t, imageID != "")
	return imageID
}

func makeEmptyArchive(t *testing.T) (string, io.ReadCloser) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "empty-file.txt")
	err := os.WriteFile(srcPath, []byte(""), 0o400)
	assert.NilError(t, err)

	// TODO(thaJeztah) Add utilities to the client to make steps below less complicated.
	// Code below is taken from copyToContainer() in docker/cli.
	srcInfo, err := archive.CopyInfoSourcePath(srcPath, false)
	assert.NilError(t, err)

	srcArchive, err := archive.TarResource(srcInfo)
	assert.NilError(t, err)
	t.Cleanup(func() {
		srcArchive.Close()
	})

	ctrPath := "/empty-file.txt"
	dstInfo := archive.CopyInfo{Path: ctrPath}
	dstDir, preparedArchive, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, dstInfo)
	assert.NilError(t, err)
	t.Cleanup(func() {
		preparedArchive.Close()
	})
	return dstDir, preparedArchive
}

func TestCopyToContainerPathIsNotDir(t *testing.T) {
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	cid := container.Create(ctx, t, apiClient)

	path := "/etc/passwd/"
	if testEnv.DaemonInfo.OSType == "windows" {
		path = "c:/windows/system32/drivers/etc/hosts/"
	}
	_, err := apiClient.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: path})
	assert.Check(t, is.ErrorContains(err, "not a directory"))
}

func TestCopyFromContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	dir, err := os.MkdirTemp("", t.Name())
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

	resp, err := apiClient.ImageBuild(ctx, buildCtx.AsTarReader(t), client.ImageBuildOptions{})
	assert.NilError(t, err)
	defer resp.Body.Close()

	var imageID string
	err = jsonmessage.DisplayStream(resp.Body, io.Discard, jsonmessage.WithAuxCallback(func(msg jsonstream.Message) {
		var r buildtypes.Result
		assert.NilError(t, json.Unmarshal(*msg.Aux, &r))
		imageID = r.ID
	}))
	assert.NilError(t, err)
	assert.Assert(t, imageID != "")

	cid := container.Create(ctx, t, apiClient, container.WithImage(imageID))

	for _, x := range []struct {
		src    string
		expect map[string]string
	}{
		{"/", map[string]string{"/": "", "/foo": "hello", "/bar/quux/baz": "world", "/bar/filesymlink": "", "/bar/dirsymlink": "", "/bar/notarget": ""}},
		{".", map[string]string{"./": "", "./foo": "hello", "./bar/quux/baz": "world", "./bar/filesymlink": "", "./bar/dirsymlink": "", "./bar/notarget": ""}},
		{"/.", map[string]string{"./": "", "./foo": "hello", "./bar/quux/baz": "world", "./bar/filesymlink": "", "./bar/dirsymlink": "", "./bar/notarget": ""}},
		{"./", map[string]string{"./": "", "./foo": "hello", "./bar/quux/baz": "world", "./bar/filesymlink": "", "./bar/dirsymlink": "", "./bar/notarget": ""}},
		{"/./", map[string]string{"./": "", "./foo": "hello", "./bar/quux/baz": "world", "./bar/filesymlink": "", "./bar/dirsymlink": "", "./bar/notarget": ""}},
		{"/bar/root", map[string]string{"root": ""}},
		{"/bar/root/", map[string]string{"root/": "", "root/foo": "hello", "root/bar/quux/baz": "world", "root/bar/filesymlink": "", "root/bar/dirsymlink": "", "root/bar/notarget": ""}},
		{"/bar/root/.", map[string]string{"./": "", "./foo": "hello", "./bar/quux/baz": "world", "./bar/filesymlink": "", "./bar/dirsymlink": "", "./bar/notarget": ""}},

		{"bar/quux", map[string]string{"quux/": "", "quux/baz": "world"}},
		{"bar/quux/", map[string]string{"quux/": "", "quux/baz": "world"}},
		{"bar/quux/.", map[string]string{"./": "", "./baz": "world"}},
		{"bar/quux/baz", map[string]string{"baz": "world"}},

		{"bar/filesymlink", map[string]string{"filesymlink": ""}},
		{"bar/dirsymlink", map[string]string{"dirsymlink": ""}},
		{"bar/dirsymlink/", map[string]string{"dirsymlink/": "", "dirsymlink/baz": "world"}},
		{"bar/dirsymlink/.", map[string]string{"./": "", "./baz": "world"}},
		{"bar/notarget", map[string]string{"notarget": ""}},
	} {
		t.Run(x.src, func(t *testing.T) {
			res, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: x.src})
			rdr := res.Content
			assert.NilError(t, err)
			defer rdr.Close()

			found := make(map[string]bool, len(x.expect))
			var numFound int
			tr := tar.NewReader(rdr)
			for numFound < len(x.expect) {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
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

				buf, err := io.ReadAll(tr)
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

// TestCopyToContainerXZBinaryNotExecutedOnDaemon tests that when
// uploading an xz-compressed archive to a container via the
// PUT /containers/{id}/archive API, the daemon does NOT execute the xz
// binary found inside the container's filesystem.
// This is a regression test for
// https://github.com/moby/moby/security/advisories/GHSA-x86f-5xw2-fm2r
func TestCopyToContainerXZBinaryNotExecutedOnDaemon(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")
	ctx := setupTest(t)

	const tokenEnv = "MOBY_EXPLOIT_TEST_TOKEN"
	const tokenVal = "host-boundary-crossed"

	d := daemon.New(t)
	defer d.Cleanup(t)
	d.SetEnvVar(tokenEnv, tokenVal)
	d.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	apiClient := d.NewClientT(t)

	dir := t.TempDir()
	buildCtx := fakecontext.New(t, dir,
		// The fake xz writes the daemon's secret env var to a marker file.
		// A process inside the container would not have this env var.
		fakecontext.WithFile("fake-xz", "#!/bin/sh\necho $"+tokenEnv+" > /xz-was-executed\n"),
		fakecontext.WithDockerfile(`FROM busybox
COPY fake-xz /usr/bin/xz
RUN chmod +x /usr/bin/xz`),
	)
	defer buildCtx.Close()

	imageID := build.Do(ctx, t, apiClient, buildCtx, client.ImageBuildOptions{})

	cID := container.Run(ctx, t, apiClient, container.WithImage(imageID))
	defer container.Remove(ctx, t, apiClient, cID, client.ContainerRemoveOptions{Force: true})

	// Craft a payload that starts with xz magic bytes so the daemon's
	// DecompressStream identifies it as xz-compressed. The payload is
	// deliberately invalid xz data; we only care whether the daemon
	// attempts to run the container's /usr/bin/xz to decompress it.
	xzMagic := []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00}
	payload := append(xzMagic, []byte("not-real-xz-data")...)

	// CopyToContainer is expected to fail because the payload is not a
	// valid xz archive. We don't care about the error; we care about
	// whether the container's xz binary was invoked.
	_, _ = apiClient.CopyToContainer(ctx, cID, client.CopyToContainerOptions{
		DestinationPath: "/tmp",
		Content:         bytes.NewReader(payload),
	})

	t.Run("binary not executed", func(t *testing.T) {
		// If the container's /usr/bin/xz was executed, the marker file
		// will exist, and this assertion will fail.
		res, err := container.Exec(ctx, apiClient, cID, []string{"test", "-f", "/xz-was-executed"})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.ExitCode, 1),
			"container's xz binary was executed by the daemon during archive extraction; marker file /xz-was-executed was created")
	})

	t.Run("runs container", func(t *testing.T) {
		// If the binary ran, check that it ran in the daemon's process
		// context by looking for the daemon's secret env var in the
		// marker file. A container process would not have this env var.
		res, err := container.Exec(ctx, apiClient, cID, []string{"cat", "/xz-was-executed"})
		assert.NilError(t, err)
		assert.Check(t, !strings.Contains(res.Stdout(), tokenVal),
			"container's xz binary was executed in the daemon's process context: marker file contains the daemon's secret env var")
	})
}

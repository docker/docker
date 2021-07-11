package build // import "github.com/docker/docker/integration/build"

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/testutil/fakecontext"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestBuildWithRemoveAndForceRemove(t *testing.T) {
	defer setupTest(t)()

	cases := []struct {
		name                           string
		dockerfile                     string
		numberOfIntermediateContainers int
		rm                             bool
		forceRm                        bool
	}{
		{
			name: "successful build with no removal",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 0`,
			numberOfIntermediateContainers: 2,
			rm:                             false,
			forceRm:                        false,
		},
		{
			name: "successful build with remove",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 0`,
			numberOfIntermediateContainers: 0,
			rm:                             true,
			forceRm:                        false,
		},
		{
			name: "successful build with remove and force remove",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 0`,
			numberOfIntermediateContainers: 0,
			rm:                             true,
			forceRm:                        true,
		},
		{
			name: "failed build with no removal",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 1`,
			numberOfIntermediateContainers: 2,
			rm:                             false,
			forceRm:                        false,
		},
		{
			name: "failed build with remove",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 1`,
			numberOfIntermediateContainers: 1,
			rm:                             true,
			forceRm:                        false,
		},
		{
			name: "failed build with remove and force remove",
			dockerfile: `FROM busybox
			RUN exit 0
			RUN exit 1`,
			numberOfIntermediateContainers: 0,
			rm:                             true,
			forceRm:                        true,
		},
	}

	client := testEnv.APIClient()
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dockerfile := []byte(c.dockerfile)

			buff := bytes.NewBuffer(nil)
			tw := tar.NewWriter(buff)
			assert.NilError(t, tw.WriteHeader(&tar.Header{
				Name: "Dockerfile",
				Size: int64(len(dockerfile)),
			}))
			_, err := tw.Write(dockerfile)
			assert.NilError(t, err)
			assert.NilError(t, tw.Close())
			resp, err := client.ImageBuild(ctx, buff, types.ImageBuildOptions{Remove: c.rm, ForceRemove: c.forceRm, NoCache: true})
			assert.NilError(t, err)
			defer resp.Body.Close()
			filter, err := buildContainerIdsFilter(resp.Body)
			assert.NilError(t, err)
			remainingContainers, err := client.ContainerList(ctx, types.ContainerListOptions{Filters: filter, All: true})
			assert.NilError(t, err)
			assert.Equal(t, c.numberOfIntermediateContainers, len(remainingContainers), "Expected %v remaining intermediate containers, got %v", c.numberOfIntermediateContainers, len(remainingContainers))
		})
	}
}

func buildContainerIdsFilter(buildOutput io.Reader) (filters.Args, error) {
	const intermediateContainerPrefix = " ---> Running in "
	filter := filters.NewArgs()

	dec := json.NewDecoder(buildOutput)
	for {
		m := jsonmessage.JSONMessage{}
		err := dec.Decode(&m)
		if err == io.EOF {
			return filter, nil
		}
		if err != nil {
			return filter, err
		}
		if ix := strings.Index(m.Stream, intermediateContainerPrefix); ix != -1 {
			filter.Add("id", strings.TrimSpace(m.Stream[ix+len(intermediateContainerPrefix):]))
		}
	}
}

// TestBuildMultiStageCopy verifies that copying between stages works correctly.
//
// Regression test for docker/for-win#4349, ENGCORE-935, where creating the target
// directory failed on Windows, because `os.MkdirAll()` was called with a volume
// GUID path (\\?\Volume{dae8d3ac-b9a1-11e9-88eb-e8554b2ba1db}\newdir\hello}),
// which currently isn't supported by Golang.
func TestBuildMultiStageCopy(t *testing.T) {
	ctx := context.Background()

	dockerfile, err := ioutil.ReadFile("testdata/Dockerfile." + t.Name())
	assert.NilError(t, err)

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(string(dockerfile)))
	defer source.Close()

	apiclient := testEnv.APIClient()

	for _, target := range []string{"copy_to_root", "copy_to_newdir", "copy_to_newdir_nested", "copy_to_existingdir", "copy_to_newsubdir"} {
		t.Run(target, func(t *testing.T) {
			imgName := strings.ToLower(t.Name())

			resp, err := apiclient.ImageBuild(
				ctx,
				source.AsTarReader(t),
				types.ImageBuildOptions{
					Remove:      true,
					ForceRemove: true,
					Target:      target,
					Tags:        []string{imgName},
				},
			)
			assert.NilError(t, err)

			out := bytes.NewBuffer(nil)
			_, err = io.Copy(out, resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Log(out)
			}
			assert.NilError(t, err)

			// verify the image was successfully built
			_, _, err = apiclient.ImageInspectWithRaw(ctx, imgName)
			if err != nil {
				t.Log(out)
			}
			assert.NilError(t, err)
		})
	}
}

func TestBuildMultiStageParentConfig(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.35"), "broken in earlier versions")
	dockerfile := `
		FROM busybox AS stage0
		ENV WHO=parent
		WORKDIR /foo

		FROM stage0
		ENV WHO=sibling1
		WORKDIR sub1

		FROM stage0
		WORKDIR sub2
	`
	ctx := context.Background()
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	imgName := strings.ToLower(t.Name())
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{imgName},
		})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	image, _, err := apiclient.ImageInspectWithRaw(ctx, imgName)
	assert.NilError(t, err)

	expected := "/foo/sub2"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `C:\foo\sub2`
	}
	assert.Check(t, is.Equal(expected, image.Config.WorkingDir))
	assert.Check(t, is.Contains(image.Config.Env, "WHO=parent"))
}

// Test cases in #36996
func TestBuildLabelWithTargets(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "test added after 1.38")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	imgName := strings.ToLower(t.Name() + "-a")
	testLabels := map[string]string{
		"foo":  "bar",
		"dead": "beef",
	}

	dockerfile := `
		FROM busybox AS target-a
		CMD ["/dev"]
		LABEL label-a=inline-a
		FROM busybox AS target-b
		CMD ["/dist"]
		LABEL label-b=inline-b
		`

	ctx := context.Background()
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	// For `target-a` build
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{imgName},
			Labels:      testLabels,
			Target:      "target-a",
		})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	image, _, err := apiclient.ImageInspectWithRaw(ctx, imgName)
	assert.NilError(t, err)

	testLabels["label-a"] = "inline-a"
	for k, v := range testLabels {
		x, ok := image.Config.Labels[k]
		assert.Assert(t, ok)
		assert.Assert(t, x == v)
	}

	// For `target-b` build
	imgName = strings.ToLower(t.Name() + "-b")
	delete(testLabels, "label-a")
	resp, err = apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{imgName},
			Labels:      testLabels,
			Target:      "target-b",
		})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	image, _, err = apiclient.ImageInspectWithRaw(ctx, imgName)
	assert.NilError(t, err)

	testLabels["label-b"] = "inline-b"
	for k, v := range testLabels {
		x, ok := image.Config.Labels[k]
		assert.Assert(t, ok)
		assert.Assert(t, x == v)
	}
}

func TestBuildWithEmptyLayers(t *testing.T) {
	dockerfile := `
		FROM    busybox
		COPY    1/ /target/
		COPY    2/ /target/
		COPY    3/ /target/
	`
	ctx := context.Background()
	source := fakecontext.New(t, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFile("1/a", "asdf"),
		fakecontext.WithFile("2/a", "asdf"),
		fakecontext.WithFile("3/a", "asdf"))
	defer source.Close()

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
}

// TestBuildMultiStageOnBuild checks that ONBUILD commands are applied to
// multiple subsequent stages
// #35652
func TestBuildMultiStageOnBuild(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.33"), "broken in earlier versions")
	defer setupTest(t)()
	// test both metadata and layer based commands as they may be implemented differently
	dockerfile := `FROM busybox AS stage1
ONBUILD RUN echo 'foo' >somefile
ONBUILD ENV bar=baz

FROM stage1
# fails if ONBUILD RUN fails
RUN cat somefile

FROM stage1
RUN cat somefile`

	ctx := context.Background()
	source := fakecontext.New(t, "",
		fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	assert.Check(t, is.Contains(out.String(), "Successfully built"))

	imageIDs, err := getImageIDsFromBuild(out.Bytes())
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(3, len(imageIDs)))

	image, _, err := apiclient.ImageInspectWithRaw(context.Background(), imageIDs[2])
	assert.NilError(t, err)
	assert.Check(t, is.Contains(image.Config.Env, "bar=baz"))
}

// #35403 #36122
func TestBuildUncleanTarFilenames(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.37"), "broken in earlier versions")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")

	ctx := context.TODO()
	defer setupTest(t)()

	dockerfile := `FROM scratch
COPY foo /
FROM scratch
COPY bar /`

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	writeTarRecord(t, w, "../foo", "foocontents0")
	writeTarRecord(t, w, "/bar", "barcontents0")
	err := w.Close()
	assert.NilError(t, err)

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		buf,
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	// repeat with changed data should not cause cache hits

	buf = bytes.NewBuffer(nil)
	w = tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	writeTarRecord(t, w, "../foo", "foocontents1")
	writeTarRecord(t, w, "/bar", "barcontents1")
	err = w.Close()
	assert.NilError(t, err)

	resp, err = apiclient.ImageBuild(ctx,
		buf,
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out = bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(out.String(), "Using cache"))
}

// docker/for-linux#135
// #35641
func TestBuildMultiStageLayerLeak(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.37"), "broken in earlier versions")
	ctx := context.TODO()
	defer setupTest(t)()

	// all commands need to match until COPY
	dockerfile := `FROM busybox
WORKDIR /foo
COPY foo .
FROM busybox
WORKDIR /foo
COPY bar .
RUN [ -f bar ]
RUN [ ! -f foo ]
`

	source := fakecontext.New(t, "",
		fakecontext.WithFile("foo", "0"),
		fakecontext.WithFile("bar", "1"),
		fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)

	assert.Check(t, is.Contains(out.String(), "Successfully built"))
}

// #37581
// #40444 (Windows Containers only)
func TestBuildWithHugeFile(t *testing.T) {
	ctx := context.TODO()
	defer setupTest(t)()

	dockerfile := `FROM busybox
`

	if testEnv.DaemonInfo.OSType == "windows" {
		dockerfile += `# create a file with size of 8GB
RUN powershell "fsutil.exe file createnew bigfile.txt 8589934592 ; dir bigfile.txt"`
	} else {
		dockerfile += `# create a sparse file with size over 8GB
RUN for g in $(seq 0 8); do dd if=/dev/urandom of=rnd bs=1K count=1 seek=$((1024*1024*g)) status=none; done && \
    ls -la rnd && du -sk rnd`
	}

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	err := w.Close()
	assert.NilError(t, err)

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		buf,
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	assert.Check(t, is.Contains(out.String(), "Successfully built"))
}

func TestBuildWCOWSandboxSize(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "windows", "only Windows has sandbox size control")
	ctx := context.TODO()
	defer setupTest(t)()

	dockerfile := `FROM busybox AS intermediate
WORKDIR C:\\stuff
# Create and delete a 21GB file
RUN fsutil file createnew C:\\stuff\\bigfile_0.txt 22548578304 && del bigfile_0.txt
# Create three 7GB files
RUN fsutil file createnew C:\\stuff\\bigfile_1.txt 7516192768
RUN fsutil file createnew C:\\stuff\\bigfile_2.txt 7516192768
RUN fsutil file createnew C:\\stuff\\bigfile_3.txt 7516192768
# Copy that 21GB of data out into a new target
FROM busybox
COPY --from=intermediate C:\\stuff C:\\stuff
`

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	err := w.Close()
	assert.NilError(t, err)

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		buf,
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	// The test passes if either:
	// - the image build succeeded; or
	// - The "COPY --from=intermediate" step ran out of space during re-exec'd writing of the transport layer information to hcsshim's temp directory
	// The latter case means we finished the COPY operation, so the sandbox must have been larger than 20GB, which was the test,
	// and _then_ ran out of space on the host during `importLayer` in the WindowsFilter graph driver, while committing the layer.
	// See https://github.com/moby/moby/pull/41636#issuecomment-723038517 for more details on the operations being done here.
	// Specifically, this happens on the Docker Jenkins CI Windows-RS5 build nodes.
	// The two parts of the acceptable-failure case are on different lines, so we need two regexp checks.
	assert.Check(t, is.Regexp("Successfully built|COPY --from=intermediate", out.String()))
	assert.Check(t, is.Regexp("Successfully built|re-exec error: exit status 1: output: write.*daemon\\\\\\\\tmp\\\\\\\\hcs.*bigfile_[1-3].txt: There is not enough space on the disk.", out.String()))
}

func TestBuildWithEmptyDockerfile(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "broken in earlier versions")
	ctx := context.TODO()
	defer setupTest(t)()

	tests := []struct {
		name        string
		dockerfile  string
		expectedErr string
	}{
		{
			name:        "empty-dockerfile",
			dockerfile:  "",
			expectedErr: "cannot be empty",
		},
		{
			name: "empty-lines-dockerfile",
			dockerfile: `
			
			
			
			`,
			expectedErr: "file with no instructions",
		},
		{
			name:        "comment-only-dockerfile",
			dockerfile:  `# this is a comment`,
			expectedErr: "file with no instructions",
		},
	}

	apiclient := testEnv.APIClient()

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := bytes.NewBuffer(nil)
			w := tar.NewWriter(buf)
			writeTarRecord(t, w, "Dockerfile", tc.dockerfile)
			err := w.Close()
			assert.NilError(t, err)

			_, err = apiclient.ImageBuild(ctx,
				buf,
				types.ImageBuildOptions{
					Remove:      true,
					ForceRemove: true,
				})

			assert.Check(t, is.Contains(err.Error(), tc.expectedErr))
		})
	}
}

func TestBuildPreserveOwnership(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "broken in earlier versions")

	ctx := context.Background()

	dockerfile, err := ioutil.ReadFile("testdata/Dockerfile." + t.Name())
	assert.NilError(t, err)

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(string(dockerfile)))
	defer source.Close()

	apiclient := testEnv.APIClient()

	for _, target := range []string{"copy_from", "copy_from_chowned"} {
		t.Run(target, func(t *testing.T) {
			resp, err := apiclient.ImageBuild(
				ctx,
				source.AsTarReader(t),
				types.ImageBuildOptions{
					Remove:      true,
					ForceRemove: true,
					Target:      target,
				},
			)
			assert.NilError(t, err)

			out := bytes.NewBuffer(nil)
			_, err = io.Copy(out, resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Log(out)
			}
			assert.NilError(t, err)
		})
	}
}

func TestBuildPlatformInvalid(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "experimental in older versions")

	ctx := context.Background()
	defer setupTest(t)()

	dockerfile := `FROM busybox
`

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	err := w.Close()
	assert.NilError(t, err)

	apiclient := testEnv.APIClient()
	_, err = apiclient.ImageBuild(ctx,
		buf,
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Platform:    "foobar",
		})

	assert.Assert(t, err != nil)
	assert.ErrorContains(t, err, "unknown operating system or architecture")
	assert.Assert(t, errdefs.IsInvalidParameter(err))
}

// Test case in #40591
func TestBuildWithCopyEmptyDir(t *testing.T) {
	dockerfile := `
                FROM    busybox
                COPY    emptydir /var
                COPY    dir /opt
        `
	ctx := context.Background()
	source := fakecontext.New(t, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithDir("emptydir"),
		fakecontext.WithFile("dir/emptydirtest", "emptydirtest"))
	defer source.Close()

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			NoCache:     true,
		})
	assert.NilError(t, err)

	out := bytes.NewBuffer(nil)
	_, err = io.Copy(out, resp.Body)
	resp.Body.Close()
	assert.NilError(t, err)
	assert.Check(t, is.Contains(out.String(), "Successfully built"))
}

func writeTarRecord(t *testing.T, w *tar.Writer, fn, contents string) {
	err := w.WriteHeader(&tar.Header{
		Name:     fn,
		Mode:     0600,
		Size:     int64(len(contents)),
		Typeflag: '0',
	})
	assert.NilError(t, err)
	_, err = w.Write([]byte(contents))
	assert.NilError(t, err)
}

type buildLine struct {
	Stream string
	Aux    struct {
		ID string
	}
}

func getImageIDsFromBuild(output []byte) ([]string, error) {
	var ids []string
	for _, line := range bytes.Split(output, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		entry := buildLine{}
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, err
		}
		if entry.Aux.ID != "" {
			ids = append(ids, entry.Aux.ID)
		}
	}
	return ids, nil
}

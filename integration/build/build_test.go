package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	buildtypes "github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/moby/moby/v2/integration/internal/build"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"github.com/moby/moby/v2/internal/testutil/request"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestBuildWithRemoveAndForceRemove(t *testing.T) {
	ctx := setupTest(t)

	tests := []struct {
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

	apiClient := testEnv.APIClient()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			dockerfile := []byte(tc.dockerfile)

			buff := bytes.NewBuffer(nil)
			tw := tar.NewWriter(buff)
			assert.NilError(t, tw.WriteHeader(&tar.Header{
				Name: "Dockerfile",
				Size: int64(len(dockerfile)),
			}))
			_, err := tw.Write(dockerfile)
			assert.NilError(t, err)
			assert.NilError(t, tw.Close())
			resp, err := apiClient.ImageBuild(ctx, buff, client.ImageBuildOptions{Remove: tc.rm, ForceRemove: tc.forceRm, NoCache: true})
			assert.NilError(t, err)
			defer resp.Body.Close()
			filter, err := buildContainerIdsFilter(resp.Body)
			assert.NilError(t, err)
			remainingContainers, err := apiClient.ContainerList(ctx, client.ContainerListOptions{Filters: filter, All: true})
			assert.NilError(t, err)
			assert.Equal(t, tc.numberOfIntermediateContainers, len(remainingContainers.Items), "Expected %v remaining intermediate containers, got %v", tc.numberOfIntermediateContainers, len(remainingContainers.Items))
		})
	}
}

func buildContainerIdsFilter(buildOutput io.Reader) (client.Filters, error) {
	const intermediateContainerPrefix = " ---> Running in "
	filter := client.Filters{}

	dec := json.NewDecoder(buildOutput)
	for {
		m := jsonstream.Message{}
		err := dec.Decode(&m)
		if err == io.EOF {
			return filter, nil
		}
		if err != nil {
			return filter, err
		}
		if _, id, ok := strings.Cut(m.Stream, intermediateContainerPrefix); ok {
			filter.Add("id", strings.TrimSpace(id))
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
	ctx := setupTest(t)

	dockerfile, err := os.ReadFile("testdata/Dockerfile." + t.Name())
	assert.NilError(t, err)

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(string(dockerfile)))
	defer source.Close()

	apiclient := testEnv.APIClient()

	for _, target := range []string{"copy_to_root", "copy_to_newdir", "copy_to_newdir_nested", "copy_to_existingdir", "copy_to_newsubdir"} {
		t.Run(target, func(t *testing.T) {
			build.Do(ctx, t, apiclient, source, client.ImageBuildOptions{
				Remove:      true,
				ForceRemove: true,
				Target:      target,
				Tags:        []string{strings.ToLower(t.Name())},
			})
		})
	}
}

func TestBuildMultiStageParentConfig(t *testing.T) {
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

	ctx := setupTest(t)
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	imgName := strings.ToLower(t.Name())
	build.Do(ctx, t, apiclient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Tags:        []string{imgName},
	})

	img, err := apiclient.ImageInspect(ctx, imgName)
	assert.NilError(t, err)

	expected := "/foo/sub2"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `C:\foo\sub2`
	}
	assert.Check(t, is.Equal(expected, img.Config.WorkingDir))
	assert.Check(t, is.Contains(img.Config.Env, "WHO=parent"))
}

// Test cases in #36996
func TestBuildLabelWithTargets(t *testing.T) {
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

	ctx := setupTest(t)
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	// For `target-a` build
	build.Do(ctx, t, apiclient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Tags:        []string{imgName},
		Labels:      testLabels,
		Target:      "target-a",
	})

	img, err := apiclient.ImageInspect(ctx, imgName)
	assert.NilError(t, err)

	testLabels["label-a"] = "inline-a"
	for k, v := range testLabels {
		x, ok := img.Config.Labels[k]
		assert.Assert(t, ok)
		assert.Assert(t, x == v)
	}

	// For `target-b` build
	imgName = strings.ToLower(t.Name() + "-b")
	delete(testLabels, "label-a")
	build.Do(ctx, t, apiclient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Tags:        []string{imgName},
		Labels:      testLabels,
		Target:      "target-b",
	})

	img, err = apiclient.ImageInspect(ctx, imgName)
	assert.NilError(t, err)

	testLabels["label-b"] = "inline-b"
	for k, v := range testLabels {
		x, ok := img.Config.Labels[k]
		assert.Check(t, ok)
		assert.Check(t, x == v)
	}
}

func TestBuildWithEmptyLayers(t *testing.T) {
	const dockerfile = `
FROM    busybox
COPY    1/ /target/
COPY    2/ /target/
COPY    3/ /target/
`
	ctx := setupTest(t)
	source := fakecontext.New(t, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFile("1/a", "asdf"),
		fakecontext.WithFile("2/a", "asdf"),
		fakecontext.WithFile("3/a", "asdf"))
	defer source.Close()

	apiclient := testEnv.APIClient()
	build.Do(ctx, t, apiclient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})
}

// TestBuildMultiStageOnBuild checks that ONBUILD commands are applied to
// multiple subsequent stages
// #35652
func TestBuildMultiStageOnBuild(t *testing.T) {
	ctx := setupTest(t)

	// test both metadata and layer based commands as they may be implemented differently
	const dockerfile = `
FROM busybox AS stage1
ONBUILD RUN echo 'foo' >somefile
ONBUILD ENV bar=baz

FROM stage1
# fails if ONBUILD RUN fails
RUN cat somefile

FROM stage1
RUN cat somefile`

	source := fakecontext.New(t, "",
		fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	err = readBuildResponse(resp.Body, out)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)

	assert.Check(t, is.Contains(out.String(), "Successfully built"))

	imageIDs, err := getImageIDsFromBuild(out.Bytes())
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(3, len(imageIDs)))

	img, err := apiclient.ImageInspect(ctx, imageIDs[2])
	assert.NilError(t, err)
	assert.Check(t, is.Contains(img.Config.Env, "bar=baz"))
}

// TestBuildUncleanTarFilenames is a regression test for cache invalidation
// with non-canonical, in-context archive paths.
//
// See:
// - https://github.com/moby/moby/issues/35403
// - https://github.com/moby/moby/issues/36122
func TestBuildUncleanTarFilenames(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")

	ctx := setupTest(t)

	const dockerfile = `
FROM scratch
COPY foo /
FROM scratch
COPY bar /
`

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	writeTarRecord(t, w, "dir/../foo", "foocontents0")
	writeTarRecord(t, w, "/bar", "barcontents0")
	err := w.Close()
	assert.NilError(t, err)

	apiclient := testEnv.APIClient()
	resp, err := apiclient.ImageBuild(ctx, buf, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	err = readBuildResponse(resp.Body, out)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)

	// repeat with changed data should not cause cache hits

	buf = bytes.NewBuffer(nil)
	w = tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	writeTarRecord(t, w, "dir/../foo", "foocontents1")
	writeTarRecord(t, w, "/bar", "barcontents1")
	err = w.Close()
	assert.NilError(t, err)

	resp, err = apiclient.ImageBuild(ctx,
		buf,
		client.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
		})

	out = bytes.NewBuffer(nil)
	assert.NilError(t, err)
	err = readBuildResponse(resp.Body, out)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(out.String(), "Using cache"))
}

// docker/for-linux#135
// #35641
func TestBuildMultiStageLayerLeak(t *testing.T) {
	ctx := setupTest(t)

	// all commands need to match until COPY
	const dockerfile = `
FROM busybox
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

	apiClient := testEnv.APIClient()
	build.Do(ctx, t, apiClient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})
}

// #37581
// #40444 (Windows Containers only)
func TestBuildWithHugeFile(t *testing.T) {
	t.Skip("Test is flaky, and often causes out of space issues on GitHub Actions")
	ctx := setupTest(t)

	var dockerfile string
	if testEnv.DaemonInfo.OSType == "windows" {
		dockerfile = `
FROM busybox

# create a file with size of 8GB
RUN powershell "fsutil.exe file createnew bigfile.txt 8589934592 ; dir bigfile.txt"
`
	} else {
		dockerfile = `
FROM busybox

# create a sparse file with size over 8GB
RUN for g in $(seq 0 8); do dd if=/dev/urandom of=rnd bs=1K count=1 seek=$((1024*1024*g)) status=none; done \
 && ls -la rnd && du -sk rnd
`
	}

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", dockerfile)
	err := w.Close()
	assert.NilError(t, err)

	apiClient := testEnv.APIClient()
	resp, err := apiClient.ImageBuild(ctx, buf, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	err = readBuildResponse(resp.Body, out)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)
	assert.Check(t, is.Contains(out.String(), "Successfully built"))
}

func TestBuildWCOWSandboxSize(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "windows", "only Windows has sandbox size control")
	ctx := setupTest(t)

	//nolint:dupword // ignore duplicate "C:\\stuff"
	const dockerfile = `
FROM busybox AS intermediate
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

	apiClient := testEnv.APIClient()
	resp, err := apiClient.ImageBuild(ctx, buf, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
	})

	out := bytes.NewBuffer(nil)
	assert.NilError(t, err)
	_, err = io.Copy(out, resp.Body)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)
	// The test passes if any of the following are true:
	// - the image build succeeded; or
	// - The "COPY --from=intermediate" step ran out of space during re-exec'd writing of the transport layer information to hcsshim's temp directory
	//   (This means we finished the COPY operation, so the sandbox must have been larger than 20GB; _then_ ran out of space on the host during
	//   `importLayer` in the WindowsFilter graph driver while committing the layer.)
	//   See https://github.com/moby/moby/pull/41636#issuecomment-[REDACTED] for more details.
	// - On some Windows nodes the ImportLayer call fails with ERROR_PATH_NOT_FOUND (0x3); this is also acceptable.
	// - On some Windows CI nodes the sandbox is too small even for the RUN step (Error 112 / ERROR_DISK_FULL), meaning the sandbox size
	//   enforcement is working (the disk fills up before we can even create the big file). This is also an acceptable outcome.
	// The two parts of the re-exec acceptable-failure case are on different lines, so we need two regexp checks.
	assert.Check(t, is.Regexp("Successfully built|COPY --from=intermediate|There is not enough space on the disk.", out.String()))
	assert.Check(t, is.Regexp(
		"Successfully built"+
			"|re-exec error: exit status 1: output: write.*daemon\\\\\\\\tmp\\\\\\\\hcs.*bigfile_[1-3].txt: There is not enough space on the disk."+
			"|hcsshim::ImportLayer.*The system cannot find the path specified"+
			"|There is not enough space on the disk.",
		out.String()))
}

func TestBuildWithEmptyDockerfile(t *testing.T) {
	ctx := setupTest(t)

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

	apiClient := testEnv.APIClient()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := bytes.NewBuffer(nil)
			w := tar.NewWriter(buf)
			writeTarRecord(t, w, "Dockerfile", tc.dockerfile)
			err := w.Close()
			assert.NilError(t, err)

			_, err = apiClient.ImageBuild(ctx,
				buf,
				client.ImageBuildOptions{
					Remove:      true,
					ForceRemove: true,
				})

			assert.Check(t, is.ErrorContains(err, tc.expectedErr))
		})
	}
}

func TestBuildPreserveOwnership(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")

	ctx := setupTest(t)

	dockerfile, err := os.ReadFile("testdata/Dockerfile." + t.Name())
	assert.NilError(t, err)

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(string(dockerfile)))
	defer source.Close()

	apiClient := testEnv.APIClient()

	for _, target := range []string{"copy_from", "copy_from_chowned"} {
		t.Run(target, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			build.Do(ctx, t, apiClient, source, client.ImageBuildOptions{
				Remove:      true,
				ForceRemove: true,
				Target:      target,
			})
		})
	}
}

func TestBuildPlatformInvalid(t *testing.T) {
	ctx := setupTest(t)

	buf := bytes.NewBuffer(nil)
	w := tar.NewWriter(buf)
	writeTarRecord(t, w, "Dockerfile", `FROM busybox`)
	err := w.Close()
	assert.NilError(t, err)

	_, err = testEnv.APIClient().ImageBuild(ctx, buf, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Platforms:   []ocispec.Platform{{OS: "foobar"}},
	})

	assert.Check(t, is.ErrorContains(err, "unknown operating system or architecture"))
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
}

func TestBuildCacheUsesDockerfilePlatform(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "test images are only available on linux")

	platform := "linux/arm64/v8"
	baseImage := "hello-world:arm64"
	daemonPlatform := platforms.Normalize(ocispec.Platform{
		OS:           "linux",
		Architecture: testEnv.DaemonInfo.Architecture,
	})
	if daemonPlatform.Architecture == "arm64" {
		platform = "linux/amd64"
		baseImage = "hello-world:amd64"
	}
	requestedPlatform, err := platforms.Parse(platform)
	assert.NilError(t, err)
	assert.Assert(t, !platforms.Only(daemonPlatform).Match(requestedPlatform))

	ctx := setupTest(t)
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(fmt.Sprintf(`
FROM --platform=%s %s
ENV CACHE_TEST=1
`, platform, baseImage)))
	defer source.Close()

	buildImage := func() string {
		resp, err := testEnv.APIClient().ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
			Version: buildtypes.BuilderV1,
		})
		assert.NilError(t, err)
		defer resp.Body.Close()

		out := bytes.NewBuffer(nil)
		err = readBuildResponse(resp.Body, out)
		assert.NilError(t, err)
		return out.String()
	}

	buildImage()
	out := buildImage()
	if !assert.Check(t, is.Contains(out, "Using cache")) {
		t.Log(out)
	}
}

// TestBuildWorkdirNoCacheMiss is a regression test for https://github.com/moby/moby/issues/47627
func TestBuildWorkdirNoCacheMiss(t *testing.T) {
	ctx := setupTest(t)

	for _, tc := range []struct {
		name       string
		dockerfile string
	}{
		{name: "trailing slash", dockerfile: "FROM busybox\nWORKDIR /foo/"},
		{name: "no trailing slash", dockerfile: "FROM busybox\nWORKDIR /foo"},
	} {
		dockerfile := tc.dockerfile
		t.Run(tc.name, func(t *testing.T) {
			source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
			defer source.Close()

			apiClient := testEnv.APIClient()

			buildAndGetID := func() string {
				resp, err := apiClient.ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
					Version: buildtypes.BuilderV1,
				})
				assert.NilError(t, err)
				defer resp.Body.Close()

				id := readBuildImageIDs(t, resp.Body)
				assert.Check(t, id != "")
				return id
			}

			firstId := buildAndGetID()
			secondId := buildAndGetID()

			assert.Check(t, is.Equal(firstId, secondId), "expected cache to be used")
		})
	}
}

func TestBuildEmitsEvents(t *testing.T) {
	ctx := setupTest(t)

	dockerfile := "FROM busybox\nRUN echo hello > /hello"
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiClient := testEnv.APIClient()

	for _, builderVersion := range []buildtypes.BuilderVersion{buildtypes.BuilderV1, buildtypes.BuilderBuildKit} {
		t.Run("v"+string(builderVersion), func(t *testing.T) {
			skip.If(t, builderVersion == buildtypes.BuilderBuildKit && testEnv.DaemonInfo.OSType == "windows" && !testEnv.UsingSnapshotter(),
				"Buildkit is not supported on Windows with graphdrivers")
			for _, tc := range []struct {
				name           string
				tag            string
				expectCreate   int
				expectTag      int
				expectTagValue string
			}{
				{
					name:         "no tag",
					tag:          "",
					expectCreate: 1,
					expectTag:    0,
				},
				{
					name:           "with tag",
					tag:            "testbuildemitsevents",
					expectCreate:   1,
					expectTag:      1,
					expectTagValue: "testbuildemitsevents:latest",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {

					ctx, cancel := context.WithCancel(ctx)
					defer cancel()

					// Use the daemon's own clock for the event window, not the test
					// runner's: the two can drift when the test suite talks to a
					// remote daemon, which would make daemon-stamped build events
					// fall outside a runner-clock-bounded window.
					since := request.DaemonUnixTime(ctx, t, apiClient, testEnv)

					buildOpts := client.ImageBuildOptions{
						Version: builderVersion,
						NoCache: true,
					}
					if tc.tag != "" {
						buildOpts.Tags = []string{tc.tag}
					}

					resp, err := apiClient.ImageBuild(ctx, source.AsTarReader(t), buildOpts)
					assert.NilError(t, err)

					defer resp.Body.Close()

					out := bytes.NewBuffer(nil)
					err = readBuildResponse(resp.Body, out)
					assert.NilError(t, err)
					buildLogs := out.String()
					imageID := readBuildImageIDs(t, bytes.NewReader(out.Bytes()))
					assert.Assert(t, imageID != "", "could not determine built image ID from build output:\n%s", buildLogs)

					// Clean up tagged image if we created one
					if tc.tag != "" {
						t.Cleanup(func() {
							if _, err := apiClient.ImageRemove(t.Context(), tc.tag, client.ImageRemoveOptions{Force: true}); err != nil {
								t.Logf("failed to remove image %s: %v", tc.tag, err)
							}
						})
					}

					until := request.DaemonUnixTime(ctx, t, apiClient, testEnv)
					result := apiClient.Events(ctx, client.EventsListOptions{
						Since: since,
						Until: until,
						// Scope to the image this subtest built: the daemon is shared
						// across (sub)tests, so an unfiltered stream would also pick up
						// create/tag events from unrelated concurrent activity.
						Filters: make(client.Filters).Add(string(events.ImageEventType), imageID),
					})
					eventsChan := result.Messages
					errs := result.Err

					var eventsReceived []string
					imageCreateEvts := 0
					imageTagEvts := 0
					var imageTagEvtName string
					finished := false
					for !finished {
						select {
						case evt := <-eventsChan:
							eventsReceived = append(eventsReceived, fmt.Sprintf("type: %v, action: %v", evt.Type, evt.Action))
							if evt.Type == events.ImageEventType && evt.Action == events.ActionCreate {
								imageCreateEvts++
							}
							if evt.Type == events.ImageEventType && evt.Action == events.ActionTag {
								imageTagEvts++
								imageTagEvtName = evt.Actor.Attributes["name"]
							}
						case err := <-errs:
							assert.Check(t, err == nil || errors.Is(err, io.EOF))
							finished = true
						}
					}

					createOk := assert.Check(t, is.Equal(tc.expectCreate, imageCreateEvts))
					tagOk := assert.Check(t, is.Equal(tc.expectTag, imageTagEvts))
					if !createOk || !tagOk {
						t.Logf("build-logs:\n%s", buildLogs)
						t.Logf("events received:\n%s", strings.Join(eventsReceived, "\n"))
					}
					if tc.expectTagValue != "" {
						assert.Check(t, is.Equal(tc.expectTagValue, imageTagEvtName))
					}
				})
			}
		})
	}
}

func TestBuildHistoryDoesNotPreventRemoval(t *testing.T) {
	skip.If(t, !testEnv.UsingSnapshotter(), "only relevant to c8d integration")

	ctx := setupTest(t)

	dockerfile := "FROM busybox\nRUN echo hello world > /hello"
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	apiClient := testEnv.APIClient()

	buildImage := func(imgName string) error {
		resp, err := apiClient.ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{imgName},
			Version:     buildtypes.BuilderBuildKit,
		})
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		err = readBuildResponse(resp.Body, nil)
		return err
	}

	err := buildImage("history-a")
	assert.NilError(t, err)

	res, err := apiClient.ImageRemove(ctx, "history-a", client.ImageRemoveOptions{})
	assert.NilError(t, err)
	assert.Check(t, slices.ContainsFunc(res.Items, func(r image.DeleteResponse) bool {
		return r.Deleted != ""
	}))
}

// readBuildResponse reads the build response stream from body, copying it
// as is to out (unless nil). Unlike io.Copy, it returns an error if the
// stream contains one, such as a build error.
func readBuildResponse(body io.Reader, out io.Writer) error {
	if out != nil {
		body = io.TeeReader(body, out)
	}
	return jsonmessage.DisplayStream(body, io.Discard)
}

func readBuildImageIDs(t *testing.T, rd io.Reader) string {
	t.Helper()
	decoder := json.NewDecoder(rd)
	for {
		var jm jsonstream.Message
		if err := decoder.Decode(&jm); err != nil {
			if err == io.EOF {
				break
			}
			assert.NilError(t, err)
		}

		if jm.Aux == nil {
			continue
		}

		var auxId struct {
			ID string `json:"ID"`
		}

		json.Unmarshal(*jm.Aux, &auxId)
		if auxId.ID != "" {
			return auxId.ID
		}
	}

	return ""
}

func writeTarRecord(t *testing.T, w *tar.Writer, fn, contents string) {
	err := w.WriteHeader(&tar.Header{
		Name:     fn,
		Mode:     0o600,
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
	for line := range bytes.SplitSeq(output, []byte("\n")) {
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

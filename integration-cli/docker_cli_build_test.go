package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"github.com/moby/moby/v2/internal/testutil/fakegit"
	"github.com/moby/moby/v2/internal/testutil/fakestorage"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/skip"
)

type DockerCLIBuildSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIBuildSuite) SetUpTest(_ context.Context, t *testing.T) {
	// This test-suite is for the classic builder.
	t.Setenv("DOCKER_BUILDKIT", "0")
}

func (s *DockerCLIBuildSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIBuildSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (s *DockerCLIBuildSuite) TestBuildJSONEmptyRun(c *testing.T) {
	cli.BuildCmd(c, "testbuildjsonemptyrun", build.WithDockerfile(`
    FROM busybox
    RUN []
    `))
}

func (s *DockerCLIBuildSuite) TestBuildShCmdJSONEntrypoint(c *testing.T) {
	const name = "testbuildshcmdjsonentrypoint"
	expected := "/bin/sh -c echo test"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "cmd /S /C echo test"
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`
    FROM busybox
    ENTRYPOINT ["echo"]
    CMD echo test
    `))
	out := cli.DockerCmd(c, "run", "--rm", name).Combined()

	if strings.TrimSpace(out) != expected {
		c.Fatalf("CMD did not contain %q : %q", expected, out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementUser(c *testing.T) {
	// Windows does not support FROM scratch or the USER command
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvironmentreplacement"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM scratch
  ENV user foo
  USER ${user}
  `))
	res := inspectFieldJSON(c, name, "Config.User")

	if res != `"foo"` {
		c.Fatal("User foo from environment not in Config.User on image")
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementVolume(c *testing.T) {
	const name = "testbuildenvironmentreplacement"

	var volumePath string

	if testEnv.DaemonInfo.OSType == "windows" {
		volumePath = "c:/quux"
	} else {
		volumePath = "/quux"
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  ENV volume `+volumePath+`
  VOLUME ${volume}
  `))

	var volumes map[string]any
	inspectFieldAndUnmarshall(c, name, "Config.Volumes", &volumes)
	if _, ok := volumes[volumePath]; !ok {
		c.Fatal("Volume " + volumePath + " from environment not in Config.Volumes on image")
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementExpose(c *testing.T) {
	// Windows does not support FROM scratch or the EXPOSE command
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvironmentreplacement"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM scratch
  ENV port 80
  EXPOSE ${port}
  ENV ports "  99   100 "
  EXPOSE ${ports}
  `))

	var exposedPorts map[string]any
	inspectFieldAndUnmarshall(c, name, "Config.ExposedPorts", &exposedPorts)
	exp := []int{80, 99, 100}
	for _, p := range exp {
		tmp := fmt.Sprintf("%d/tcp", p)
		if _, ok := exposedPorts[tmp]; !ok {
			c.Fatalf("Exposed port %d from environment not in Config.ExposedPorts on image", p)
		}
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementWorkdir(c *testing.T) {
	const name = "testbuildenvironmentreplacement"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM busybox
  ENV MYWORKDIR /work
  RUN mkdir ${MYWORKDIR}
  WORKDIR ${MYWORKDIR}
  `))
	res := inspectFieldJSON(c, name, "Config.WorkingDir")

	expected := `"/work"`
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `"C:\\work"`
	}
	if res != expected {
		c.Fatalf("Workdir /workdir from environment not in Config.WorkingDir on image: %s", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementAddCopy(c *testing.T) {
	const name = "testbuildenvironmentreplacement"

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
  FROM `+minimalBaseImage()+`
  ENV baz foo
  ENV quux bar
  ENV dot .
  ENV fee fff
  ENV gee ggg

  ADD ${baz} ${dot}
  COPY ${quux} ${dot}
  ADD ${zzz:-${fee}} ${dot}
  COPY ${zzz:-${gee}} ${dot}
  `),
		build.WithFile("foo", "test1"),
		build.WithFile("bar", "test2"),
		build.WithFile("fff", "test3"),
		build.WithFile("ggg", "test4"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildEnvironmentReplacementEnv(c *testing.T) {
	// ENV expansions work differently in Windows
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvironmentreplacement"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM busybox
  ENV foo zzz
  ENV bar ${foo}
  ENV abc1='$foo'
  ENV env1=$foo env2=${foo} env3="$foo" env4="${foo}"
  RUN [ "$abc1" = '$foo' ] && (echo "$abc1" | grep -q foo)
  ENV abc2="\$foo"
  RUN [ "$abc2" = '$foo' ] && (echo "$abc2" | grep -q foo)
  ENV abc3 '$foo'
  RUN [ "$abc3" = '$foo' ] && (echo "$abc3" | grep -q foo)
  ENV abc4 "\$foo"
  RUN [ "$abc4" = '$foo' ] && (echo "$abc4" | grep -q foo)
  ENV foo2="abc\def"
  RUN [ "$foo2" = 'abc\def' ]
  ENV foo3="abc\\def"
  RUN [ "$foo3" = 'abc\def' ]
  ENV foo4='abc\\def'
  RUN [ "$foo4" = 'abc\\def' ]
  ENV foo5='abc\def'
  RUN [ "$foo5" = 'abc\def' ]
  `))

	var envResult []string
	inspectFieldAndUnmarshall(c, name, "Config.Env", &envResult)
	found := false
	envCount := 0

	for _, env := range envResult {
		k, v, _ := strings.Cut(env, "=")
		if k == "bar" {
			found = true
			if v != "zzz" {
				c.Fatalf("Could not find replaced var for env `bar`: got %q instead of `zzz`", v)
			}
		} else if strings.HasPrefix(k, "env") {
			envCount++
			if v != "zzz" {
				c.Fatalf("%s should be 'zzz' but instead its %q", k, v)
			}
		} else if strings.HasPrefix(k, "env") {
			envCount++
			if v != "foo" {
				c.Fatalf("%s should be 'foo' but instead its %q", k, v)
			}
		}
	}

	if !found {
		c.Fatal("Never found the `bar` env variable")
	}

	if envCount != 4 {
		c.Fatalf("Didn't find all env vars - only saw %d\n%s", envCount, envResult)
	}
}

func (s *DockerCLIBuildSuite) TestBuildHandleEscapesInVolume(c *testing.T) {
	// The volume paths used in this test are invalid on Windows
	testRequires(c, DaemonIsLinux)
	const name = "testbuildhandleescapes"

	testCases := []struct {
		volumeValue string
		expected    string
	}{
		{
			volumeValue: "${FOO}",
			expected:    "bar",
		},
		{
			volumeValue: `\${FOO}`,
			expected:    "${FOO}",
		},
		// this test in particular provides *7* backslashes and expects 6 to come back.
		// Like above, the first escape is swallowed and the rest are treated as
		// literals, this one is just less obvious because of all the character noise.
		{
			volumeValue: `\\\\\\\${FOO}`,
			expected:    `\\\${FOO}`,
		},
	}

	for _, tc := range testCases {
		cli.BuildCmd(c, name, build.WithDockerfile(fmt.Sprintf(`
  FROM scratch
  ENV FOO bar
  VOLUME %s
  `, tc.volumeValue)))

		var result map[string]map[string]struct{}
		inspectFieldAndUnmarshall(c, name, "Config.Volumes", &result)
		if _, ok := result[tc.expected]; !ok {
			c.Fatalf("Could not find volume %s set from env foo in volumes table, got %q", tc.expected, result)
		}

		// Remove the image for the next iteration
		cli.DockerCmd(c, "rmi", name)
	}
}

func (s *DockerCLIBuildSuite) TestBuildOnBuildLowercase(c *testing.T) {
	const name = "testbuildonbuildlowercase"
	const name2 = "testbuildonbuildlowercase2"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM busybox
  onbuild run echo quux
  `))

	result := cli.Docker(cli.Args("build", "-t", name2), build.WithDockerfile(fmt.Sprintf(`
  FROM %s
  `, name)))
	result.Assert(c, icmd.Success)

	if !strings.Contains(result.Combined(), "quux") {
		c.Fatalf("Did not receive the expected echo text, got %s", result.Combined())
	}

	//nolint:dupword
	if strings.Contains(result.Combined(), "ONBUILD ONBUILD") {
		c.Fatalf("Got an ONBUILD ONBUILD error with no error: got %s", result.Combined())
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvEscapes(c *testing.T) {
	// ENV expansions work differently in Windows
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvescapes"
	cli.BuildCmd(c, name, build.WithDockerfile(`
    FROM busybox
    ENV TEST foo
    CMD echo \$
    `))

	out := cli.DockerCmd(c, "run", "-t", name).Combined()
	if strings.TrimSpace(out) != "$" {
		c.Fatalf("Env TEST was not overwritten with bar when foo was supplied to dockerfile: was %q", strings.TrimSpace(out))
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnvOverwrite(c *testing.T) {
	// ENV expansions work differently in Windows
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvoverwrite"
	cli.BuildCmd(c, name, build.WithDockerfile(`
    FROM busybox
    ENV TEST foo
    CMD echo ${TEST}
    `))

	out := cli.DockerCmd(c, "run", "-e", "TEST=bar", "-t", name).Combined()
	if strings.TrimSpace(out) != "bar" {
		c.Fatalf("Env TEST was not overwritten with bar when foo was supplied to dockerfile: was %q", strings.TrimSpace(out))
	}
}

// FIXME(vdemeester) why we disabled cache here ?
func (s *DockerCLIBuildSuite) TestBuildOnBuildCmdEntrypointJSON(c *testing.T) {
	const name1 = "onbuildcmd"
	const name2 = "onbuildgenerated"

	cli.BuildCmd(c, name1, build.WithDockerfile(`
FROM busybox
ONBUILD CMD ["hello world"]
ONBUILD ENTRYPOINT ["echo"]
ONBUILD RUN ["true"]`))

	cli.BuildCmd(c, name2, build.WithDockerfile("FROM "+name1))

	result := cli.DockerCmd(c, "run", name2)
	result.Assert(c, icmd.Expected{Out: "hello world"})
}

// FIXME(vdemeester) why we disabled cache here ?
func (s *DockerCLIBuildSuite) TestBuildOnBuildEntrypointJSON(c *testing.T) {
	const name1 = "onbuildcmd"
	const name2 = "onbuildgenerated"

	cli.BuildCmd(c, name1, build.WithDockerfile(`
FROM busybox
ONBUILD ENTRYPOINT ["echo"]`))

	cli.BuildCmd(c, name2, build.WithDockerfile(fmt.Sprintf("FROM %s\nCMD [\"hello world\"]\n", name1)))

	out := cli.DockerCmd(c, "run", name2).Combined()
	if !regexp.MustCompile(`(?m)^hello world`).MatchString(out) {
		c.Fatal("got malformed output from onbuild", out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildCacheAdd(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows doesn't have httpserver image yet
	const name = "testbuildtwoimageswithadd"
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"robots.txt": "hello",
		"index.html": "world",
	}))
	defer server.Close()

	cli.BuildCmd(c, name, build.WithDockerfile(fmt.Sprintf(`FROM scratch
		ADD %s/robots.txt /`, server.URL())))

	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(fmt.Sprintf(`FROM scratch
		ADD %s/index.html /`, server.URL())))
	result.Assert(c, icmd.Success)
	if strings.Contains(result.Combined(), "Using cache") {
		c.Fatal("2nd build used cache on ADD, it shouldn't")
	}
}

func (s *DockerCLIBuildSuite) TestBuildLastModified(c *testing.T) {
	// Temporary fix for #30890. TODO: figure out what
	// has changed in the master busybox image.
	testRequires(c, DaemonIsLinux)

	const name = "testbuildlastmodified"

	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"file": "hello",
	}))
	defer server.Close()

	var out, out2 string
	args := []string{"run", name, "ls", "-l", "--full-time", "/file"}

	dFmt := `FROM busybox
ADD %s/file /`
	dockerfile := fmt.Sprintf(dFmt, server.URL())

	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(dockerfile))
	out = cli.DockerCmd(c, args...).Combined()

	// Build it again and make sure the mtime of the file didn't change.
	// Wait a few seconds to make sure the time changed enough to notice
	time.Sleep(2 * time.Second)

	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(dockerfile))
	out2 = cli.DockerCmd(c, args...).Combined()

	if out != out2 {
		c.Fatalf("MTime changed:\nOrigin:%s\nNew:%s", out, out2)
	}

	// Now 'touch' the file and make sure the timestamp DID change this time
	// Create a new fakeStorage instead of just using Add() to help windows
	server = fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"file": "hello",
	}))
	defer server.Close()

	dockerfile = fmt.Sprintf(dFmt, server.URL())
	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(dockerfile))
	out2 = cli.DockerCmd(c, args...).Combined()

	if out == out2 {
		c.Fatalf("MTime didn't change:\nOrigin:%s\nNew:%s", out, out2)
	}
}

// Regression for https://github.com/moby/moby/pull/27805
// Makes sure that we don't use the cache if the contents of
// a file in a subfolder of the context is modified and we re-build.
func (s *DockerCLIBuildSuite) TestBuildModifyFileInFolder(c *testing.T) {
	const name = "testbuildmodifyfileinfolder"

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`FROM busybox
RUN ["mkdir", "/test"]
ADD folder/file /test/changetarget`))
	defer ctx.Close()
	if err := ctx.Add("folder/file", "first"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	if err := ctx.Add("folder/file", "second"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	if id1 == id2 {
		c.Fatal("cache was used even though file contents in folder was changed")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddSingleFileToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testaddimg", build.WithBuildContext(c,
		build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio /exists
ADD test_file /
RUN [ $(ls -l /test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_file | awk '{print $1}') = '%s' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`, expectedFileChmod)),
		build.WithFile("test_file", "test1")))
}

// Issue #3960: "ADD src ." hangs
func (s *DockerCLIBuildSuite) TestBuildAddSingleFileToWorkdir(c *testing.T) {
	const name = "testaddsinglefiletoworkdir"
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(
		`FROM busybox
	       ADD test_file .`),
		fakecontext.WithFiles(map[string]string{
			"test_file": "test1",
		}))
	defer ctx.Close()

	errChan := make(chan error, 1)
	go func() {
		errChan <- cli.Docker(cli.Args("build", "-t", name), build.WithExternalBuildContext(ctx)).Error
		close(errChan)
	}()
	select {
	case <-time.After(15 * time.Second):
		c.Fatal("Build with adding to workdir timed out")
	case err := <-errChan:
		assert.NilError(c, err)
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddSingleFileToExistDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testaddsinglefiletoexistdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN mkdir /exists
RUN touch /exists/exists_file
RUN chown -R dockerio.dockerio /exists
ADD test_file /exists/
RUN [ $(ls -l / | grep exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/exists_file | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopyAddMultipleFiles(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"robots.txt": "hello",
	}))
	defer server.Close()

	cli.BuildCmd(c, "testcopymultiplefilestofile", build.WithBuildContext(c,
		build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN mkdir /exists
RUN touch /exists/exists_file
RUN chown -R dockerio.dockerio /exists
COPY test_file1 test_file2 /exists/
ADD test_file3 test_file4 %s/robots.txt /exists/
RUN [ $(ls -l / | grep exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/test_file1 | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/test_file2 | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/test_file3 | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/test_file4 | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/robots.txt | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/exists_file | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
`, server.URL())),
		build.WithFile("test_file1", "test1"),
		build.WithFile("test_file2", "test2"),
		build.WithFile("test_file3", "test3"),
		build.WithFile("test_file4", "test4"),
	))
}

// These tests are mainly for user namespaces to verify that new directories
// are created as the remapped root uid/gid pair
func (s *DockerCLIBuildSuite) TestBuildUsernamespaceValidateRemappedRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	testCases := []string{
		"ADD . /new_dir",
		"COPY test_dir /new_dir",
		"WORKDIR /new_dir",
	}
	const name = "testbuildusernamespacevalidateremappedroot"
	for _, tc := range testCases {
		cli.BuildCmd(c, name, build.WithBuildContext(c,
			build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
%s
RUN [ $(ls -l / | grep new_dir | awk '{print $3":"$4}') = 'root:root' ]`, tc)),
			build.WithFile("test_dir/test_file", "test file")))

		cli.DockerCmd(c, "rmi", name)
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddAndCopyFileWithWhitespace(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Not currently passing on Windows
	const name = "testaddfilewithwhitespace"

	for _, command := range []string{"ADD", "COPY"} {
		cli.BuildCmd(c, name, build.WithBuildContext(c,
			build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN mkdir "/test dir"
RUN mkdir "/test_dir"
%s [ "test file1", "/test_file1" ]
%s [ "test_file2", "/test file2" ]
%s [ "test file3", "/test file3" ]
%s [ "test dir/test_file4", "/test_dir/test_file4" ]
%s [ "test_dir/test_file5", "/test dir/test_file5" ]
%s [ "test dir/test_file6", "/test dir/test_file6" ]
RUN [ $(cat "/test_file1") = 'test1' ]
RUN [ $(cat "/test file2") = 'test2' ]
RUN [ $(cat "/test file3") = 'test3' ]
RUN [ $(cat "/test_dir/test_file4") = 'test4' ]
RUN [ $(cat "/test dir/test_file5") = 'test5' ]
RUN [ $(cat "/test dir/test_file6") = 'test6' ]`, command, command, command, command, command, command)),
			build.WithFile("test file1", "test1"),
			build.WithFile("test_file2", "test2"),
			build.WithFile("test file3", "test3"),
			build.WithFile("test dir/test_file4", "test4"),
			build.WithFile("test_dir/test_file5", "test5"),
			build.WithFile("test dir/test_file6", "test6"),
		))

		cli.DockerCmd(c, "rmi", name)
	}
}

func (s *DockerCLIBuildSuite) TestBuildCopyFileWithWhitespaceOnWindows(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	dockerfile := `FROM ` + testEnv.PlatformDefaults.BaseImage + `
RUN mkdir "C:/test dir"
RUN mkdir "C:/test_dir"
COPY [ "test file1", "/test_file1" ]
COPY [ "test_file2", "/test file2" ]
COPY [ "test file3", "/test file3" ]
COPY [ "test dir/test_file4", "/test_dir/test_file4" ]
COPY [ "test_dir/test_file5", "/test dir/test_file5" ]
COPY [ "test dir/test_file6", "/test dir/test_file6" ]
RUN find "test1" "C:/test_file1"
RUN find "test2" "C:/test file2"
RUN find "test3" "C:/test file3"
RUN find "test4" "C:/test_dir/test_file4"
RUN find "test5" "C:/test dir/test_file5"
RUN find "test6" "C:/test dir/test_file6"`

	const name = "testcopyfilewithwhitespace"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("test file1", "test1"),
		build.WithFile("test_file2", "test2"),
		build.WithFile("test file3", "test3"),
		build.WithFile("test dir/test_file4", "test4"),
		build.WithFile("test_dir/test_file5", "test5"),
		build.WithFile("test dir/test_file6", "test6"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildCopyWildcard(c *testing.T) {
	const name = "testcopywildcard"
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"robots.txt": "hello",
		"index.html": "world",
	}))
	defer server.Close()

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(fmt.Sprintf(`FROM busybox
	COPY file*.txt /tmp/
	RUN ls /tmp/file1.txt /tmp/file2.txt
	RUN [ "mkdir",  "/tmp1" ]
	COPY dir* /tmp1/
	RUN ls /tmp1/dirt /tmp1/nested_file /tmp1/nested_dir/nest_nest_file
	RUN [ "mkdir",  "/tmp2" ]
        ADD dir/*dir %s/robots.txt /tmp2/
	RUN ls /tmp2/nest_nest_file /tmp2/robots.txt
	`, server.URL())),
		fakecontext.WithFiles(map[string]string{
			"file1.txt":                     "test1",
			"file2.txt":                     "test2",
			"dir/nested_file":               "nested file",
			"dir/nested_dir/nest_nest_file": "2 times nested",
			"dirt":                          "dirty",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)

	// Now make sure we use a cache the 2nd time
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)

	if id1 != id2 {
		c.Fatal("didn't use the cache")
	}
}

func (s *DockerCLIBuildSuite) TestBuildCopyWildcardInName(c *testing.T) {
	// Run this only on Linux
	// Below is the original comment (that I don't agree with — vdemeester)
	// Normally we would do c.Fatal(err) here but given that
	// the odds of this failing are so rare, it must be because
	// the OS we're running the client on doesn't support * in
	// filenames (like windows).  So, instead of failing the test
	// just let it pass. Then we don't need to explicitly
	// say which OSs this works on or not.
	testRequires(c, DaemonIsLinux, UnixCli)

	cli.BuildCmd(c, "testcopywildcardinname", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
	COPY *.txt /tmp/
	RUN [ "$(cat /tmp/\*.txt)" = 'hi there' ]
	`),
		build.WithFile("*.txt", "hi there"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildCopyWildcardCache(c *testing.T) {
	const name = "testcopywildcardcache"
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`FROM busybox
	COPY file1.txt /tmp/`),
		fakecontext.WithFiles(map[string]string{
			"file1.txt": "test1",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)

	// Now make sure we use a cache the 2nd time even with wild cards.
	// Use the same context so the file is the same and the checksum will match
	ctx.Add("Dockerfile", `FROM busybox
	COPY file*.txt /tmp/`)

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)

	if id1 != id2 {
		c.Fatal("didn't use the cache")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddSingleFileToNonExistingDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testaddsinglefiletononexistingdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio /exists
ADD test_file /test_dir/
RUN [ $(ls -l / | grep test_dir | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildAddDirContentToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testadddircontenttoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio exists
ADD test_dir /
RUN [ $(ls -l /test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_dir/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildAddDirContentToExistingDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testadddircontenttoexistingdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN mkdir /exists
RUN touch /exists/exists_file
RUN chown -R dockerio.dockerio /exists
ADD test_dir/ /exists/
RUN [ $(ls -l / | grep exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/exists_file | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/test_file | awk '{print $3":"$4}') = 'root:root' ]`),
		build.WithFile("test_dir/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildAddWholeDirToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testaddwholedirtoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio exists
ADD test_dir /test_dir
RUN [ $(ls -l / | grep test_dir | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l / | grep test_dir | awk '{print $1}') = 'drwxr-xr-x' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $1}') = '%s' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`, expectedFileChmod)),
		build.WithFile("test_dir/test_file", "test1")))
}

// Testing #5941 : Having an etc directory in context conflicts with the /etc/mtab
func (s *DockerCLIBuildSuite) TestBuildAddOrCopyEtcToRootShouldNotConflict(c *testing.T) {
	cli.BuildCmd(c, "testaddetctoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM `+minimalBaseImage()+`
ADD . /`),
		build.WithFile("etc/test_file", "test1")))
	cli.BuildCmd(c, "testcopyetctoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM `+minimalBaseImage()+`
COPY . /`),
		build.WithFile("etc/test_file", "test1")))
}

// Testing #9401 : Losing setuid flag after a ADD
func (s *DockerCLIBuildSuite) TestBuildAddPreservesFilesSpecialBits(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testaddetctoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
ADD suidbin /usr/bin/suidbin
RUN chmod 4755 /usr/bin/suidbin
RUN [ $(ls -l /usr/bin/suidbin | awk '{print $1}') = '-rwsr-xr-x' ]
ADD ./data/ /
RUN [ $(ls -l /usr/bin/suidbin | awk '{print $1}') = '-rwsr-xr-x' ]`),
		build.WithFile("suidbin", "suidbin"),
		build.WithFile("/data/usr/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopySingleFileToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testcopysinglefiletoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio /exists
COPY test_file /
RUN [ $(ls -l /test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_file | awk '{print $1}') = '%s' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`, expectedFileChmod)),
		build.WithFile("test_file", "test1")))
}

// Issue #3960: "ADD src ." hangs - adapted for COPY
func (s *DockerCLIBuildSuite) TestBuildCopySingleFileToWorkdir(c *testing.T) {
	const name = "testcopysinglefiletoworkdir"
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`FROM busybox
COPY test_file .`),
		fakecontext.WithFiles(map[string]string{
			"test_file": "test1",
		}))
	defer ctx.Close()

	errChan := make(chan error, 1)
	go func() {
		errChan <- cli.Docker(cli.Args("build", "-t", name), build.WithExternalBuildContext(ctx)).Error
		close(errChan)
	}()
	select {
	case <-time.After(15 * time.Second):
		c.Fatal("Build with adding to workdir timed out")
	case err := <-errChan:
		assert.NilError(c, err)
	}
}

func (s *DockerCLIBuildSuite) TestBuildCopySingleFileToExistDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testcopysinglefiletoexistdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN mkdir /exists
RUN touch /exists/exists_file
RUN chown -R dockerio.dockerio /exists
COPY test_file /exists/
RUN [ $(ls -l / | grep exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists/exists_file | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopySingleFileToNonExistDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific
	cli.BuildCmd(c, "testcopysinglefiletononexistdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio /exists
COPY test_file /test_dir/
RUN [ $(ls -l / | grep test_dir | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopyDirContentToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testcopydircontenttoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio exists
COPY test_dir /
RUN [ $(ls -l /test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`),
		build.WithFile("test_dir/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopyDirContentToExistDir(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testcopydircontenttoexistdir", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN mkdir /exists
RUN touch /exists/exists_file
RUN chown -R dockerio.dockerio /exists
COPY test_dir/ /exists/
RUN [ $(ls -l / | grep exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/exists_file | awk '{print $3":"$4}') = 'dockerio:dockerio' ]
RUN [ $(ls -l /exists/test_file | awk '{print $3":"$4}') = 'root:root' ]`),
		build.WithFile("test_dir/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildCopyWholeDirToRoot(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Linux specific test
	cli.BuildCmd(c, "testcopywholedirtoroot", build.WithBuildContext(c,
		build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
RUN echo 'dockerio:x:1001:' >> /etc/group
RUN touch /exists
RUN chown dockerio.dockerio exists
COPY test_dir /test_dir
RUN [ $(ls -l / | grep test_dir | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l / | grep test_dir | awk '{print $1}') = 'drwxr-xr-x' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $3":"$4}') = 'root:root' ]
RUN [ $(ls -l /test_dir/test_file | awk '{print $1}') = '%s' ]
RUN [ $(ls -l /exists | awk '{print $3":"$4}') = 'dockerio:dockerio' ]`, expectedFileChmod)),
		build.WithFile("test_dir/test_file", "test1")))
}

func (s *DockerCLIBuildSuite) TestBuildAddBadLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Not currently working on Windows

	dockerfile := `
		FROM scratch
		ADD links.tar /
		ADD foo.txt /symlink/
		`
	targetFile := "foo.txt"
	const name = "test-link-absolute"
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile))
	defer ctx.Close()

	tempDir, err := os.MkdirTemp("", "test-link-absolute-temp-")
	if err != nil {
		c.Fatalf("failed to create temporary directory: %s", tempDir)
	}
	defer os.RemoveAll(tempDir)

	var symlinkTarget string
	if runtime.GOOS == "windows" {
		var driveLetter string
		if abs, err := filepath.Abs(tempDir); err != nil {
			c.Fatal(err)
		} else {
			driveLetter = abs[:1]
		}
		tempDirWithoutDrive := tempDir[2:]
		symlinkTarget = fmt.Sprintf(`%s:\..\..\..\..\..\..\..\..\..\..\..\..%s`, driveLetter, tempDirWithoutDrive)
	} else {
		symlinkTarget = "/../../../../../../../../../../../.." + tempDir
	}

	tarPath := filepath.Join(ctx.Dir, "links.tar")
	nonExistingFile := filepath.Join(tempDir, targetFile)
	fooPath := filepath.Join(ctx.Dir, targetFile)

	tarOut, err := os.Create(tarPath)
	if err != nil {
		c.Fatal(err)
	}
	defer tarOut.Close()

	tarWriter := tar.NewWriter(tarOut)
	defer tarWriter.Close()

	header := &tar.Header{
		Name:     "symlink",
		Typeflag: tar.TypeSymlink,
		Linkname: symlinkTarget,
		Mode:     0o755,
		Uid:      0,
		Gid:      0,
	}

	err = tarWriter.WriteHeader(header)
	if err != nil {
		c.Fatal(err)
	}

	err = os.WriteFile(fooPath, []byte("test"), 0o666)
	if err != nil {
		c.Fatal(err)
	}

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	if _, err := os.Stat(nonExistingFile); err == nil || !os.IsNotExist(err) {
		c.Fatalf("%s shouldn't have been written and it shouldn't exist", nonExistingFile)
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddBadLinksVolume(c *testing.T) {
	testRequires(c, DaemonIsLinux) // ln not implemented on Windows busybox
	const (
		dockerfileTemplate = `
		FROM busybox
		RUN ln -s /../../../../../../../../%s /x
		VOLUME /x
		ADD foo.txt /x/`
		targetFile = "foo.txt"
	)

	tempDir, err := os.MkdirTemp("", "test-link-absolute-volume-temp-")
	if err != nil {
		c.Fatalf("failed to create temporary directory: %s", tempDir)
	}
	defer os.RemoveAll(tempDir)

	dockerfile := fmt.Sprintf(dockerfileTemplate, tempDir)
	nonExistingFile := filepath.Join(tempDir, targetFile)

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile))
	defer ctx.Close()
	fooPath := filepath.Join(ctx.Dir, targetFile)

	err = os.WriteFile(fooPath, []byte("test"), 0o666)
	if err != nil {
		c.Fatal(err)
	}

	cli.BuildCmd(c, "test-link-absolute-volume", build.WithExternalBuildContext(ctx))
	if _, err := os.Stat(nonExistingFile); err == nil || !os.IsNotExist(err) {
		c.Fatalf("%s shouldn't have been written and it shouldn't exist", nonExistingFile)
	}
}

// Issue #5270 - ensure we throw a better error than "unexpected EOF"
// when we can't access files in the context.
func (s *DockerCLIBuildSuite) TestBuildWithInaccessibleFilesInContext(c *testing.T) {
	t := c
	testRequires(c, DaemonIsLinux, UnixCli, testEnv.IsLocalDaemon) // test uses chown/chmod: not available on windows

	t.Run("inaccessible files", func(t *testing.T) {
		const name = "testbuildinaccessiblefiles"
		buildCTX := fakecontext.New(t, "",
			fakecontext.WithDockerfile("FROM scratch\nADD . /foo/"),
			fakecontext.WithFiles(map[string]string{"fileWithoutReadAccess": "foo"}),
		)
		defer buildCTX.Close()
		// This is used to ensure we detect inaccessible files early during build in the cli client
		pathToFileWithoutReadAccess := filepath.Join(buildCTX.Dir, "fileWithoutReadAccess")

		if err := os.Chown(pathToFileWithoutReadAccess, 0, 0); err != nil {
			t.Fatalf("failed to chown file to root: %s", err)
		}
		if err := os.Chmod(pathToFileWithoutReadAccess, 0o700); err != nil {
			t.Fatalf("failed to chmod file to 700: %s", err)
		}
		result := icmd.RunCmd(icmd.Cmd{
			Command: []string{"su", "unprivilegeduser", "-c", fmt.Sprintf("DOCKER_BUILDKIT=0 %s build -t %s .", dockerBinary, name)},
			Dir:     buildCTX.Dir,
		})
		if result.Error == nil {
			t.Fatalf("build should have failed: %s %s", result.Error, result.Combined())
		}

		// check if we've detected the failure before we started building
		assert.Check(t, is.Contains(result.Combined(), "no permission to read from"))
		assert.Check(t, is.Contains(result.Combined(), "checking context"))
	})
	t.Run("inaccessible directory", func(t *testing.T) {
		const name = "testbuildinaccessibledirectory"
		buildCTX := fakecontext.New(t, "",
			fakecontext.WithDockerfile("FROM scratch\nADD . /foo/"),
			fakecontext.WithFiles(map[string]string{"directoryWeCantStat/bar": "foo"}),
		)
		defer buildCTX.Close()
		// This is used to ensure we detect inaccessible directories early during build in the cli client
		pathToDirectoryWithoutReadAccess := filepath.Join(buildCTX.Dir, "directoryWeCantStat")
		pathToFileInDirectoryWithoutReadAccess := filepath.Join(pathToDirectoryWithoutReadAccess, "bar")

		if err := os.Chown(pathToDirectoryWithoutReadAccess, 0, 0); err != nil {
			t.Fatalf("failed to chown directory to root: %s", err)
		}
		if err := os.Chmod(pathToDirectoryWithoutReadAccess, 0o444); err != nil {
			t.Fatalf("failed to chmod directory to 444: %s", err)
		}
		if err := os.Chmod(pathToFileInDirectoryWithoutReadAccess, 0o700); err != nil {
			t.Fatalf("failed to chmod file to 700: %s", err)
		}

		result := icmd.RunCmd(icmd.Cmd{
			Command: []string{"su", "unprivilegeduser", "-c", fmt.Sprintf("DOCKER_BUILDKIT=0 %s build -t %s .", dockerBinary, name)},
			Dir:     buildCTX.Dir,
		})
		if result.Error == nil {
			t.Fatalf("build should have failed: %s %s", result.Error, result.Combined())
		}

		// check if we've detected the failure before we started building
		assert.Check(t, is.Contains(result.Combined(), "can't stat"))
		assert.Check(t, is.Contains(result.Combined(), "checking context"))
	})
	t.Run("links OK", func(t *testing.T) {
		const name = "testlinksok"
		buildCTX := fakecontext.New(t, "", fakecontext.WithDockerfile("FROM scratch\nADD . /foo/"))
		defer buildCTX.Close()

		target := "../../../../../../../../../../../../../../../../../../../azA"
		if err := os.Symlink(filepath.Join(buildCTX.Dir, "g"), target); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(target)
		// This is used to ensure we don't follow links when checking if everything in the context is accessible
		// This test doesn't require that we run commands as an unprivileged user
		cli.BuildCmd(t, name, build.WithExternalBuildContext(buildCTX))
	})
	t.Run("inaccessible ignored files", func(t *testing.T) {
		const name = "testbuildignoredinaccessible"
		ctx := fakecontext.New(t, "",
			fakecontext.WithDockerfile("FROM scratch\nADD . /foo/"),
			fakecontext.WithFiles(map[string]string{
				"directoryWeCantStat/bar": "foo",
				".dockerignore":           "directoryWeCantStat",
			}),
		)
		defer ctx.Close()
		// This is used to ensure we don't try to add inaccessible files when they are ignored by a .dockerignore pattern
		pathToDirectoryWithoutReadAccess := filepath.Join(ctx.Dir, "directoryWeCantStat")
		pathToFileInDirectoryWithoutReadAccess := filepath.Join(pathToDirectoryWithoutReadAccess, "bar")
		if err := os.Chown(pathToDirectoryWithoutReadAccess, 0, 0); err != nil {
			t.Fatalf("failed to chown directory to root: %s", err)
		}
		if err := os.Chmod(pathToDirectoryWithoutReadAccess, 0o444); err != nil {
			t.Fatalf("failed to chmod directory to 444: %s", err)
		}
		if err := os.Chmod(pathToFileInDirectoryWithoutReadAccess, 0o700); err != nil {
			t.Fatalf("failed to chmod file to 700: %s", err)
		}

		result := icmd.RunCmd(icmd.Cmd{
			Dir: ctx.Dir,
			Command: []string{
				"su", "unprivilegeduser", "-c",
				fmt.Sprintf("DOCKER_BUILDKIT=0 %s build -t %s .", dockerBinary, name),
			},
		})
		result.Assert(t, icmd.Success)
	})
}

func (s *DockerCLIBuildSuite) TestBuildForceRm(c *testing.T) {
	containerCountBefore := getContainerCount(c)
	const name = "testbuildforcerm"

	r := cli.Docker(cli.Args("build", "-t", name), cli.WithFlags("--force-rm"), build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
	RUN true
	RUN thiswillfail`)))
	if r.ExitCode != 1 && r.ExitCode != 127 { // different on Linux / Windows
		c.Fatalf("Wrong exit code")
	}

	containerCountAfter := getContainerCount(c)
	assert.Check(c, is.Equal(containerCountBefore, containerCountAfter), "--force-rm shouldn't have left containers behind")
}

func (s *DockerCLIBuildSuite) TestBuildRm(c *testing.T) {
	const name = "testbuildrm"

	testCases := []struct {
		buildflags                []string
		shouldLeftContainerBehind bool
	}{
		// Default case (i.e. --rm=true)
		{
			buildflags:                []string{},
			shouldLeftContainerBehind: false,
		},
		{
			buildflags:                []string{"--rm"},
			shouldLeftContainerBehind: false,
		},
		{
			buildflags:                []string{"--rm=false"},
			shouldLeftContainerBehind: true,
		},
	}

	for _, tc := range testCases {
		containerCountBefore := getContainerCount(c)

		cli.BuildCmd(c, name, cli.WithFlags(tc.buildflags...), build.WithDockerfile(`FROM busybox
	RUN echo hello world`))

		containerCountAfter := getContainerCount(c)
		if tc.shouldLeftContainerBehind {
			if containerCountBefore == containerCountAfter {
				c.Fatalf("flags %v should have left containers behind", tc.buildflags)
			}
		} else {
			if containerCountBefore != containerCountAfter {
				c.Fatalf("flags %v shouldn't have left containers behind", tc.buildflags)
			}
		}

		cli.DockerCmd(c, "rmi", name)
	}
}

func (s *DockerCLIBuildSuite) TestBuildWithVolumes(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Invalid volume paths on Windows
	var (
		result   map[string]map[string]struct{}
		name     = "testbuildvolumes"
		emptyMap = make(map[string]struct{})
		expected = map[string]map[string]struct{}{
			"/test1":  emptyMap,
			"/test2":  emptyMap,
			"/test3":  emptyMap,
			"/test4":  emptyMap,
			"/test5":  emptyMap,
			"/test6":  emptyMap,
			"[/test7": emptyMap,
			"/test8]": emptyMap,
		}
	)

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM scratch
		VOLUME /test1
		VOLUME /test2
    VOLUME /test3 /test4
    VOLUME ["/test5", "/test6"]
    VOLUME [/test7 /test8]
    `))

	inspectFieldAndUnmarshall(c, name, "Config.Volumes", &result)

	equal := reflect.DeepEqual(&result, &expected)
	if !equal {
		c.Fatalf("Volumes %s, expected %s", result, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildMaintainer(c *testing.T) {
	const name = "testbuildmaintainer"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
        MAINTAINER dockerio`))

	expected := "dockerio"
	res := inspectField(c, name, "Author")
	if res != expected {
		c.Fatalf("Maintainer %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildUser(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuilduser"
	expected := "dockerio"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
		USER dockerio
		RUN [ $(whoami) = 'dockerio' ]`))
	res := inspectField(c, name, "Config.User")
	if res != expected {
		c.Fatalf("User %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildRelativeWorkdir(c *testing.T) {
	const name = "testbuildrelativeworkdir"

	var (
		expected1     string
		expected2     string
		expected3     string
		expected4     string
		expectedFinal string
	)

	if testEnv.DaemonInfo.OSType == "windows" {
		expected1 = `C:/`
		expected2 = `C:/test1`
		expected3 = `C:/test2`
		expected4 = `C:/test2/test3`
		expectedFinal = `C:\test2\test3` // Note inspect is going to return Windows paths, as it's not in busybox
	} else {
		expected1 = `/`
		expected2 = `/test1`
		expected3 = `/test2`
		expected4 = `/test2/test3`
		expectedFinal = `/test2/test3`
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN sh -c "[ "$PWD" = "`+expected1+`" ]"
		WORKDIR test1
		RUN sh -c "[ "$PWD" = "`+expected2+`" ]"
		WORKDIR /test2
		RUN sh -c "[ "$PWD" = "`+expected3+`" ]"
		WORKDIR test3
		RUN sh -c "[ "$PWD" = "`+expected4+`" ]"`))

	res := inspectField(c, name, "Config.WorkingDir")
	if res != expectedFinal {
		c.Fatalf("Workdir %s, expected %s", res, expectedFinal)
	}
}

// #22181 Regression test. Single end-to-end test of using
// Windows semantics. Most path handling verifications are in unit tests
func (s *DockerCLIBuildSuite) TestBuildWindowsWorkdirProcessing(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	cli.BuildCmd(c, "testbuildwindowsworkdirprocessing", build.WithDockerfile(`FROM busybox
		WORKDIR C:\\foo
		WORKDIR bar
		RUN sh -c "[ "$PWD" = "C:/foo/bar" ]"
		`))
}

// #22181 Regression test. Most paths handling verifications are in unit test.
// One functional test for end-to-end
func (s *DockerCLIBuildSuite) TestBuildWindowsAddCopyPathProcessing(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	// TODO Windows. Needs a follow-up PR to 22181 to
	// support backslash such as .\\ being equivalent to ./ and c:\\ being
	// equivalent to c:/. This is not currently (nor ever has been) supported
	// by docker on the Windows platform.
	cli.BuildCmd(c, "testbuildwindowsaddcopypathprocessing", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
			# No trailing slash on COPY/ADD
			# Results in dir being changed to a file
			WORKDIR /wc1
			COPY wc1 c:/wc1
			WORKDIR /wc2
			ADD wc2 c:/wc2
			WORKDIR c:/
			RUN sh -c "[ $(cat c:/wc1/wc1) = 'hellowc1' ]"
			RUN sh -c "[ $(cat c:/wc2/wc2) = 'worldwc2' ]"

			# Trailing slash on COPY/ADD, Windows-style path.
			WORKDIR /wd1
			COPY wd1 c:/wd1/
			WORKDIR /wd2
			ADD wd2 c:/wd2/
			RUN sh -c "[ $(cat c:/wd1/wd1) = 'hellowd1' ]"
			RUN sh -c "[ $(cat c:/wd2/wd2) = 'worldwd2' ]"
			`),
		build.WithFile("wc1", "hellowc1"),
		build.WithFile("wc2", "worldwc2"),
		build.WithFile("wd1", "hellowd1"),
		build.WithFile("wd2", "worldwd2"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildWorkdirWithEnvVariables(c *testing.T) {
	const name = "testbuildworkdirwithenvvariables"

	var expected string
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `C:\test1\test2`
	} else {
		expected = `/test1/test2`
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		ENV DIRPATH /test1
		ENV SUBDIRNAME test2
		WORKDIR $DIRPATH
		WORKDIR $SUBDIRNAME/$MISSING_VAR`))
	res := inspectField(c, name, "Config.WorkingDir")
	if res != expected {
		c.Fatalf("Workdir %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildRelativeCopy(c *testing.T) {
	// cat /test1/test2/foo gets permission denied for the user
	testRequires(c, NotUserNamespace)

	var expected string
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `C:/test1/test2`
	} else {
		expected = `/test1/test2`
	}

	cli.BuildCmd(c, "testbuildrelativecopy", build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
			WORKDIR /test1
			WORKDIR test2
			RUN sh -c "[ "$PWD" = '`+expected+`' ]"
			COPY foo ./
			RUN sh -c "[ $(cat /test1/test2/foo) = 'hello' ]"
			ADD foo ./bar/baz
			RUN sh -c "[ $(cat /test1/test2/bar/baz) = 'hello' ]"
			COPY foo ./bar/baz2
			RUN sh -c "[ $(cat /test1/test2/bar/baz2) = 'hello' ]"
			WORKDIR ..
			COPY foo ./
			RUN sh -c "[ $(cat /test1/foo) = 'hello' ]"
			COPY foo /test3/
			RUN sh -c "[ $(cat /test3/foo) = 'hello' ]"
			WORKDIR /test4
			COPY . .
			RUN sh -c "[ $(cat /test4/foo) = 'hello' ]"
			WORKDIR /test5/test6
			COPY foo ../
			RUN sh -c "[ $(cat /test5/foo) = 'hello' ]"
			`),
		build.WithFile("foo", "hello"),
	))
}

// FIXME(vdemeester) should be unit test
func (s *DockerCLIBuildSuite) TestBuildBlankName(c *testing.T) {
	const name = "testbuildblankname"
	testCases := []struct {
		expression     string
		expectedStderr string
	}{
		{
			expression:     "ENV =",
			expectedStderr: "ENV names can not be blank",
		},
		{
			expression:     "LABEL =",
			expectedStderr: "LABEL names can not be blank",
		},
		{
			expression:     "ARG =foo",
			expectedStderr: "ARG names can not be blank",
		},
	}

	for _, tc := range testCases {
		cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile("FROM busybox\n"+tc.expression)).Assert(c, icmd.Expected{
			ExitCode: 1,
			Err:      tc.expectedStderr,
		})
	}
}

func (s *DockerCLIBuildSuite) TestBuildEnv(c *testing.T) {
	testRequires(c, DaemonIsLinux) // ENV expansion is different in Windows
	const name = "testbuildenv"
	expected := "[PATH=/test:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin PORT=2375]"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		ENV PATH /test:$PATH
		ENV PORT 2375
		RUN [ $(env | grep PORT) = 'PORT=2375' ]`))
	res := inspectField(c, name, "Config.Env")
	if res != expected {
		c.Fatalf("Env %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildPATH(c *testing.T) {
	testRequires(c, DaemonIsLinux) // ENV expansion is different in Windows

	defPath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	fn := func(dockerfile string, expected string) {
		cli.BuildCmd(c, "testbldpath", build.WithDockerfile(dockerfile))
		res := inspectField(c, "testbldpath", "Config.Env")
		if res != expected {
			c.Fatalf("Env %q, expected %q for dockerfile:%q", res, expected, dockerfile)
		}
	}

	tests := []struct{ dockerfile, exp string }{
		{"FROM scratch\nMAINTAINER me", "[PATH=" + defPath + "]"},
		{"FROM busybox\nMAINTAINER me", "[PATH=" + defPath + "]"},
		{"FROM scratch\nENV FOO=bar", "[PATH=" + defPath + " FOO=bar]"},
		{"FROM busybox\nENV FOO=bar", "[PATH=" + defPath + " FOO=bar]"},
		{"FROM scratch\nENV PATH=/test", "[PATH=/test]"},
		{"FROM busybox\nENV PATH=/test", "[PATH=/test]"},
		{"FROM scratch\nENV PATH=''", "[PATH=]"},
		{"FROM busybox\nENV PATH=''", "[PATH=]"},
	}

	for _, test := range tests {
		fn(test.dockerfile, test.exp)
	}
}

func (s *DockerCLIBuildSuite) TestBuildContextCleanup(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	const name = "testbuildcontextcleanup"
	entries, err := os.ReadDir(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "tmp"))
	if err != nil {
		c.Fatalf("failed to list contents of tmp dir: %s", err)
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
        ENTRYPOINT ["/bin/echo"]`))

	entriesFinal, err := os.ReadDir(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "tmp"))
	if err != nil {
		c.Fatalf("failed to list contents of tmp dir: %s", err)
	}
	if err = compareDirectoryEntries(entries, entriesFinal); err != nil {
		c.Fatalf("context should have been deleted, but wasn't")
	}
}

func (s *DockerCLIBuildSuite) TestBuildContextCleanupFailedBuild(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	const name = "testbuildcontextcleanup"
	entries, err := os.ReadDir(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "tmp"))
	if err != nil {
		c.Fatalf("failed to list contents of tmp dir: %s", err)
	}

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM `+minimalBaseImage()+`
	RUN /non/existing/command`)).Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	entriesFinal, err := os.ReadDir(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "tmp"))
	if err != nil {
		c.Fatalf("failed to list contents of tmp dir: %s", err)
	}
	if err = compareDirectoryEntries(entries, entriesFinal); err != nil {
		c.Fatalf("context should have been deleted, but wasn't")
	}
}

// compareDirectoryEntries compares two sets of DirEntry (usually taken from a directory)
// and returns an error if different.
func compareDirectoryEntries(e1 []os.DirEntry, e2 []os.DirEntry) error {
	var (
		e1Entries = make(map[string]struct{})
		e2Entries = make(map[string]struct{})
	)
	for _, e := range e1 {
		e1Entries[e.Name()] = struct{}{}
	}
	for _, e := range e2 {
		e2Entries[e.Name()] = struct{}{}
	}
	if !reflect.DeepEqual(e1Entries, e2Entries) {
		return errors.New("entries differ")
	}
	return nil
}

func (s *DockerCLIBuildSuite) TestBuildCmd(c *testing.T) {
	const name = "testbuildcmd"
	expected := "[/bin/echo Hello World]"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
        CMD ["/bin/echo", "Hello World"]`))

	res := inspectField(c, name, "Config.Cmd")
	if res != expected {
		c.Fatalf("Cmd %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildExpose(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Expose not implemented on Windows
	const name = "testbuildexpose"
	expected := "map[2375/tcp:{}]"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM scratch
        EXPOSE 2375`))

	res := inspectField(c, name, "Config.ExposedPorts")
	if res != expected {
		c.Fatalf("Exposed ports %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildExposeMorePorts(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Expose not implemented on Windows
	// start building docker file with a large number of ports
	portList := make([]string, 50)
	line := make([]string, 100)
	expectedPorts := make([]int, len(portList)*len(line))
	for i := range portList {
		for j := range line {
			p := i*len(line) + j + 1
			line[j] = strconv.Itoa(p)
			expectedPorts[p-1] = p
		}
		if i == len(portList)-1 {
			portList[i] = strings.Join(line, " ")
		} else {
			portList[i] = strings.Join(line, " ") + ` \`
		}
	}

	dockerfile := `FROM scratch
	EXPOSE {{range .}} {{.}}
	{{end}}`
	tmpl := template.Must(template.New("dockerfile").Parse(dockerfile))
	buf := bytes.NewBuffer(nil)
	tmpl.Execute(buf, portList)

	const name = "testbuildexpose"
	cli.BuildCmd(c, name, build.WithDockerfile(buf.String()))

	// check if all the ports are saved inside Config.ExposedPorts
	res := inspectFieldJSON(c, name, "Config.ExposedPorts")
	var exposedPorts map[string]any
	if err := json.Unmarshal([]byte(res), &exposedPorts); err != nil {
		c.Fatal(err)
	}

	for _, p := range expectedPorts {
		ep := fmt.Sprintf("%d/tcp", p)
		if _, ok := exposedPorts[ep]; !ok {
			c.Errorf("Port(%s) is not exposed", ep)
		} else {
			delete(exposedPorts, ep)
		}
	}
	if len(exposedPorts) != 0 {
		c.Errorf("Unexpected extra exposed ports %v", exposedPorts)
	}
}

func (s *DockerCLIBuildSuite) TestBuildExposeOrder(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Expose not implemented on Windows
	buildID := func(name, exposed string) string {
		cli.BuildCmd(c, name, build.WithDockerfile("FROM scratch\nEXPOSE "+exposed))
		id := inspectField(c, name, "Id")
		return id
	}

	id1 := buildID("testbuildexpose1", "80 2375")
	id2 := buildID("testbuildexpose2", "2375 80")
	if id1 != id2 {
		c.Errorf("EXPOSE should invalidate the cache only when ports actually changed")
	}
}

func (s *DockerCLIBuildSuite) TestBuildExposeUpperCaseProto(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Expose not implemented on Windows
	const name = "testbuildexposeuppercaseproto"
	expected := "map[5678/udp:{}]"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM scratch
        EXPOSE 5678/UDP`))
	res := inspectField(c, name, "Config.ExposedPorts")
	if res != expected {
		c.Fatalf("Exposed ports %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEmptyEntrypointInheritance(c *testing.T) {
	const name = "testbuildentrypointinheritance"
	const name2 = "testbuildentrypointinheritance2"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
        ENTRYPOINT ["/bin/echo"]`))
	res := inspectField(c, name, "Config.Entrypoint")

	expected := "[/bin/echo]"
	if res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}

	cli.BuildCmd(c, name2, build.WithDockerfile(fmt.Sprintf(`FROM %s
        ENTRYPOINT []`, name)))
	res = inspectField(c, name2, "Config.Entrypoint")

	expected = "[]"
	if res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEmptyEntrypoint(c *testing.T) {
	const name = "testbuildentrypoint"
	expected := "[]"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
        ENTRYPOINT []`))

	res := inspectField(c, name, "Config.Entrypoint")
	if res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEntrypoint(c *testing.T) {
	const name = "testbuildentrypoint"

	expected := "[/bin/echo]"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
        ENTRYPOINT ["/bin/echo"]`))

	res := inspectField(c, name, "Config.Entrypoint")
	if res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}
}

// #6445 ensure ONBUILD triggers aren't committed to grandchildren
func (s *DockerCLIBuildSuite) TestBuildOnBuildLimitedInheritance(c *testing.T) {
	cli.BuildCmd(c, "testonbuildtrigger1", build.WithDockerfile(`
		FROM busybox
		RUN echo "GRANDPARENT"
		ONBUILD RUN echo "ONBUILD PARENT"
		`))
	// ONBUILD should be run in second build.
	cli.Docker(cli.Args("build", "-t", "testonbuildtrigger2"), build.WithDockerfile("FROM testonbuildtrigger1")).Assert(c, icmd.Expected{
		Out: "ONBUILD PARENT",
	})
	// ONBUILD should *not* be run in third build.
	result := cli.Docker(cli.Args("build", "-t", "testonbuildtrigger3"), build.WithDockerfile("FROM testonbuildtrigger2"))
	result.Assert(c, icmd.Success)
	if strings.Contains(result.Combined(), "ONBUILD PARENT") {
		c.Fatalf("ONBUILD instruction ran in grandchild of ONBUILD parent")
	}
}

func (s *DockerCLIBuildSuite) TestBuildSameDockerfileWithAndWithoutCache(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Expose not implemented on Windows
	const name = "testbuildwithcache"
	dockerfile := `FROM scratch
		MAINTAINER dockerio
		EXPOSE 5432
        ENTRYPOINT ["/bin/echo"]`
	cli.BuildCmd(c, name, build.WithDockerfile(dockerfile))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithDockerfile(dockerfile))
	id2 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(dockerfile))
	id3 := getIDByName(c, name)
	if id1 != id2 {
		c.Fatal("The cache should have been used but hasn't.")
	}
	if id1 == id3 {
		c.Fatal("The cache should have been invalided but hasn't.")
	}
}

// Make sure that ADD/COPY still populate the cache even if they don't use it
func (s *DockerCLIBuildSuite) TestBuildConditionalCache(c *testing.T) {
	const name = "testbuildconditionalcache"

	dockerfile := `
		FROM busybox
        ADD foo /tmp/`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "hello",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)

	if err := ctx.Add("foo", "bye"); err != nil {
		c.Fatalf("Error modifying foo: %s", err)
	}

	// Updating a file should invalidate the cache
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	if id2 == id1 {
		c.Fatal("Should not have used the cache")
	}

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id3 := getIDByName(c, name)
	if id3 != id2 {
		c.Fatal("Should have used the cache")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddMultipleLocalFileWithAndWithoutCache(c *testing.T) {
	const name = "testbuildaddmultiplelocalfilewithcache"
	baseName := name + "-base"

	cli.BuildCmd(c, baseName, build.WithDockerfile(`
		FROM busybox
		ENTRYPOINT ["/bin/sh"]
	`))

	dockerfile := `
		FROM testbuildaddmultiplelocalfilewithcache-base
        MAINTAINER dockerio
        ADD foo Dockerfile /usr/lib/bla/
		RUN sh -c "[ $(cat /usr/lib/bla/foo) = "hello" ]"`
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile), fakecontext.WithFiles(map[string]string{
		"foo": "hello",
	}))
	defer ctx.Close()
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	result2 := cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	result3 := cli.BuildCmd(c, name, build.WithoutCache, build.WithExternalBuildContext(ctx))
	id3 := getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("The cache should have been used but hasn't: %s", result2.Stdout())
	}
	if id1 == id3 {
		c.Fatalf("The cache should have been invalided but hasn't: %s", result3.Stdout())
	}
}

func (s *DockerCLIBuildSuite) TestBuildCopyDirButNotFile(c *testing.T) {
	const name = "testbuildcopydirbutnotfile"
	const name2 = "testbuildcopydirbutnotfile2"

	dockerfile := `
        FROM ` + minimalBaseImage() + `
        COPY dir /tmp/`
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile), fakecontext.WithFiles(map[string]string{
		"dir/foo": "hello",
	}))
	defer ctx.Close()
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	// Check that adding file with similar name doesn't mess with cache
	if err := ctx.Add("dir_file", "hello2"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name2, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name2)
	if id1 != id2 {
		c.Fatal("The cache should have been used but wasn't")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddCurrentDirWithCache(c *testing.T) {
	const name = "testbuildaddcurrentdirwithcache"
	const name2 = name + "2"
	const name3 = name + "3"
	const name4 = name + "4"
	dockerfile := `
        FROM ` + minimalBaseImage() + `
        MAINTAINER dockerio
        ADD . /usr/lib/bla`
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile), fakecontext.WithFiles(map[string]string{
		"foo": "hello",
	}))
	defer ctx.Close()
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	// Check that adding file invalidate cache of "ADD ."
	if err := ctx.Add("bar", "hello2"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name2, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name2)
	if id1 == id2 {
		c.Fatal("The cache should have been invalided but hasn't.")
	}
	// Check that changing file invalidate cache of "ADD ."
	if err := ctx.Add("foo", "hello1"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name3, build.WithExternalBuildContext(ctx))
	id3 := getIDByName(c, name3)
	if id2 == id3 {
		c.Fatal("The cache should have been invalided but hasn't.")
	}
	// Check that changing file to same content with different mtime does not
	// invalidate cache of "ADD ."
	time.Sleep(1 * time.Second) // wait second because of mtime precision
	if err := ctx.Add("foo", "hello1"); err != nil {
		c.Fatal(err)
	}
	cli.BuildCmd(c, name4, build.WithExternalBuildContext(ctx))
	id4 := getIDByName(c, name4)
	if id3 != id4 {
		c.Fatal("The cache should have been used but hasn't.")
	}
}

// FIXME(vdemeester) this really seems to test the same thing as before (TestBuildAddMultipleLocalFileWithAndWithoutCache)
func (s *DockerCLIBuildSuite) TestBuildAddCurrentDirWithoutCache(c *testing.T) {
	const name = "testbuildaddcurrentdirwithoutcache"
	dockerfile := `
        FROM ` + minimalBaseImage() + `
        MAINTAINER dockerio
        ADD . /usr/lib/bla`
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile), fakecontext.WithFiles(map[string]string{
		"foo": "hello",
	}))
	defer ctx.Close()
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithoutCache, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	if id1 == id2 {
		c.Fatal("The cache should have been invalided but hasn't.")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddRemoteFileWithAndWithoutCache(c *testing.T) {
	const name = "testbuildaddremotefilewithcache"
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"baz": "hello",
	}))
	defer server.Close()

	dockerfile := fmt.Sprintf(`FROM `+minimalBaseImage()+`
        MAINTAINER dockerio
        ADD %s/baz /usr/lib/baz/quux`, server.URL())
	cli.BuildCmd(c, name, build.WithDockerfile(dockerfile))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithDockerfile(dockerfile))
	id2 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(dockerfile))
	id3 := getIDByName(c, name)

	if id1 != id2 {
		c.Fatal("The cache should have been used but hasn't.")
	}
	if id1 == id3 {
		c.Fatal("The cache should have been invalided but hasn't.")
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddRemoteFileMTime(c *testing.T) {
	const name = "testbuildaddremotefilemtime"
	const name2 = name + "2"
	const name3 = name + "3"

	files := map[string]string{"baz": "hello"}
	server := fakestorage.New(c, "", fakecontext.WithFiles(files))
	defer server.Close()

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(fmt.Sprintf(`FROM `+minimalBaseImage()+`
        MAINTAINER dockerio
        ADD %s/baz /usr/lib/baz/quux`, server.URL())))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name2, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name2)
	if id1 != id2 {
		c.Fatal("The cache should have been used but wasn't - #1")
	}

	// Now create a different server with same contents (causes different mtime)
	// The cache should still be used

	// allow some time for clock to pass as mtime precision is only 1s
	time.Sleep(2 * time.Second)

	server2 := fakestorage.New(c, "", fakecontext.WithFiles(files))
	defer server2.Close()

	ctx2 := fakecontext.New(c, "", fakecontext.WithDockerfile(fmt.Sprintf(`FROM `+minimalBaseImage()+`
        MAINTAINER dockerio
        ADD %s/baz /usr/lib/baz/quux`, server2.URL())))
	defer ctx2.Close()
	cli.BuildCmd(c, name3, build.WithExternalBuildContext(ctx2))
	id3 := getIDByName(c, name3)
	if id1 != id3 {
		c.Fatal("The cache should have been used but wasn't")
	}
}

// FIXME(vdemeester) this really seems to test the same thing as before (combined)
func (s *DockerCLIBuildSuite) TestBuildAddLocalAndRemoteFilesWithAndWithoutCache(c *testing.T) {
	const name = "testbuildaddlocalandremotefilewithcache"
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{
		"baz": "hello",
	}))
	defer server.Close()

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(fmt.Sprintf(`FROM `+minimalBaseImage()+`
        MAINTAINER dockerio
        ADD foo /usr/lib/bla/bar
        ADD %s/baz /usr/lib/baz/quux`, server.URL())),
		fakecontext.WithFiles(map[string]string{
			"foo": "hello world",
		}))
	defer ctx.Close()
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithoutCache, build.WithExternalBuildContext(ctx))
	id3 := getIDByName(c, name)
	if id1 != id2 {
		c.Fatal("The cache should have been used but hasn't.")
	}
	if id1 == id3 {
		c.Fatal("The cache should have been invalidated but hasn't.")
	}
}

func testContextTar(t *testing.T, comp compression.Compression) {
	ctx := fakecontext.New(t, "",
		fakecontext.WithDockerfile(`FROM busybox
ADD foo /foo
CMD ["cat", "/foo"]`),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}),
	)
	defer ctx.Close()
	buildContext, err := archive.Tar(ctx.Dir, comp)
	if err != nil {
		t.Fatalf("failed to build context tar: %v", err)
	}
	const name = "contexttar"

	cli.BuildCmd(t, name, build.WithStdinContext(buildContext))
}

func (s *DockerCLIBuildSuite) TestBuildContextTarGzip(c *testing.T) {
	testContextTar(c, compression.Gzip)
}

func (s *DockerCLIBuildSuite) TestBuildContextTarNoCompression(c *testing.T) {
	testContextTar(c, compression.None)
}

func (s *DockerCLIBuildSuite) TestBuildNoContext(c *testing.T) {
	const name = "nocontext"
	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "build", "-t", name, "-"},
		Stdin: strings.NewReader(
			`FROM busybox
			CMD ["echo", "ok"]`),
	}).Assert(c, icmd.Success)

	if out := cli.DockerCmd(c, "run", "--rm", "nocontext").Combined(); out != "ok\n" {
		c.Fatalf("run produced invalid output: %q, expected %q", out, "ok")
	}
}

// FIXME(vdemeester) migrate to docker/cli e2e
func (s *DockerCLIBuildSuite) TestBuildDockerfileStdin(c *testing.T) {
	const name = "stdindockerfile"
	tmpDir, err := os.MkdirTemp("", "fake-context")
	assert.NilError(c, err)
	err = os.WriteFile(filepath.Join(tmpDir, "foo"), []byte("bar"), 0o600)
	assert.NilError(c, err)

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "build", "-t", name, "-f", "-", tmpDir},
		Stdin: strings.NewReader(
			`FROM busybox
ADD foo /foo
CMD ["cat", "/foo"]`),
	}).Assert(c, icmd.Success)

	res := inspectField(c, name, "Config.Cmd")
	assert.Equal(c, strings.TrimSpace(res), `[cat /foo]`)
}

// FIXME(vdemeester) migrate to docker/cli tests (unit or e2e)
func (s *DockerCLIBuildSuite) TestBuildDockerfileStdinConflict(c *testing.T) {
	const name = "stdindockerfiletarcontext"
	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "build", "-t", name, "-f", "-", "-"},
	}).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "use stdin for both build context and dockerfile",
	})
}

func (s *DockerCLIBuildSuite) TestBuildDockerfileStdinNoExtraFiles(c *testing.T) {
	s.testBuildDockerfileStdinNoExtraFiles(c, false, false)
}

func (s *DockerCLIBuildSuite) TestBuildDockerfileStdinDockerignore(c *testing.T) {
	s.testBuildDockerfileStdinNoExtraFiles(c, true, false)
}

func (s *DockerCLIBuildSuite) TestBuildDockerfileStdinDockerignoreIgnored(c *testing.T) {
	s.testBuildDockerfileStdinNoExtraFiles(c, true, true)
}

func (s *DockerCLIBuildSuite) testBuildDockerfileStdinNoExtraFiles(t *testing.T, hasDockerignore, ignoreDockerignore bool) {
	const name = "stdindockerfilenoextra"
	tmpDir, err := os.MkdirTemp("", "fake-context")
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)

	writeFile := func(filename, content string) {
		err = os.WriteFile(filepath.Join(tmpDir, filename), []byte(content), 0o600)
		assert.NilError(t, err)
	}

	writeFile("foo", "bar")

	if hasDockerignore {
		// Add an empty Dockerfile to verify that it is not added to the image
		writeFile("Dockerfile", "")

		ignores := "Dockerfile\n"
		if ignoreDockerignore {
			ignores += ".dockerignore\n"
		}
		writeFile(".dockerignore", ignores)
	}

	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "build", "-t", name, "-f", "-", tmpDir},
		Stdin: strings.NewReader(
			`FROM busybox
COPY . /baz`),
	})
	result.Assert(t, icmd.Success)

	result = cli.DockerCmd(t, "run", "--rm", name, "ls", "-A", "/baz")
	if hasDockerignore && !ignoreDockerignore {
		assert.Equal(t, result.Stdout(), ".dockerignore\nfoo\n")
	} else {
		assert.Equal(t, result.Stdout(), "foo\n")
	}
}

func (s *DockerCLIBuildSuite) TestBuildWithVolumeOwnership(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildimg"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox:latest
        RUN mkdir /test && chown daemon:wheel /test && chmod 0600 /test
        VOLUME /test`))

	out := cli.DockerCmd(c, "run", "--rm", "testbuildimg", "ls", "-la", "/test").Combined()
	if expected := "drw-------"; !strings.Contains(out, expected) {
		c.Fatalf("expected %s received %s", expected, out)
	}
	if expected := "daemon   wheel"; !strings.Contains(out, expected) {
		c.Fatalf("expected %s received %s", expected, out)
	}
}

// testing #1405 - config.Cmd does not get cleaned up if
// utilizing cache
func (s *DockerCLIBuildSuite) TestBuildEntrypointRunCleanup(c *testing.T) {
	const name = "testbuildcmdcleanup"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
        RUN echo "hello"`))

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
        RUN echo "hello"
        ADD foo /foo
        ENTRYPOINT ["/bin/echo"]`),
		build.WithFile("foo", "hello")))

	res := inspectField(c, name, "Config.Cmd")
	// Cmd must be cleaned up
	if res != "[]" {
		c.Fatalf("Cmd %s, expected nil", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildAddFileNotFound(c *testing.T) {
	const name = "testbuildaddnotfound"

	cli.Docker(cli.Args("build", "-t", name), build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM `+minimalBaseImage()+`
        ADD foo /usr/local/bar`),
		build.WithFile("bar", "hello"))).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "stat foo: file does not exist",
	})
}

func (s *DockerCLIBuildSuite) TestBuildInheritance(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildinheritance"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM scratch
		EXPOSE 2375`))
	ports1 := inspectField(c, name, "Config.ExposedPorts")

	cli.BuildCmd(c, name, build.WithDockerfile(fmt.Sprintf(`FROM %s
		ENTRYPOINT ["/bin/echo"]`, name)))

	res := inspectField(c, name, "Config.Entrypoint")
	if expected := "[/bin/echo]"; res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}
	ports2 := inspectField(c, name, "Config.ExposedPorts")
	if ports1 != ports2 {
		c.Fatalf("Ports must be same: %s != %s", ports1, ports2)
	}
}

func (s *DockerCLIBuildSuite) TestBuildFails(c *testing.T) {
	const name = "testbuildfails"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
		RUN sh -c "exit 23"`)).Assert(c, icmd.Expected{
		ExitCode: 23,
		Err:      "returned a non-zero code: 23",
	})
}

func (s *DockerCLIBuildSuite) TestBuildOnBuild(c *testing.T) {
	const name = "testbuildonbuild"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		ONBUILD RUN touch foobar`))
	cli.BuildCmd(c, name, build.WithDockerfile(fmt.Sprintf(`FROM %s
		RUN [ -f foobar ]`, name)))
}

// gh #2446
func (s *DockerCLIBuildSuite) TestBuildAddToSymlinkDest(c *testing.T) {
	makeLink := `ln -s /foo /bar`
	if testEnv.DaemonInfo.OSType == "windows" {
		makeLink = `mklink /D C:\bar C:\foo`
	}
	const name = "testbuildaddtosymlinkdest"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox
		RUN sh -c "mkdir /foo"
		RUN `+makeLink+`
		ADD foo /bar/
		RUN sh -c "[ -f /bar/foo ]"
		RUN sh -c "[ -f /foo/foo ]"`),
		build.WithFile("foo", "hello"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildEscapeWhitespace(c *testing.T) {
	const name = "testbuildescapewhitespace"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  # ESCAPE=\
  FROM busybox
  MAINTAINER "Docker \
IO <io@\
docker.com>"
  `))

	res := inspectField(c, name, "Author")
	if res != `"Docker IO <io@docker.com>"` {
		c.Fatalf("Parsed string did not match the escaped string. Got: %q", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildVerifyIntString(c *testing.T) {
	// Verify that strings that look like ints are still passed as strings
	const name = "testbuildstringing"

	cli.BuildCmd(c, name, build.WithDockerfile(`
	FROM busybox
	MAINTAINER 123`))

	out := cli.DockerCmd(c, "inspect", name).Stdout()
	if !strings.Contains(out, `"123"`) {
		c.Fatalf("Output does not contain the int as a string:\n%s", out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildDockerignore(c *testing.T) {
	const name = "testbuilddockerignore"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox
		 ADD . /bla
		RUN sh -c "[[ -f /bla/src/x.go ]]"
		RUN sh -c "[[ -f /bla/Makefile ]]"
		RUN sh -c "[[ ! -e /bla/src/_vendor ]]"
		RUN sh -c "[[ ! -e /bla/.gitignore ]]"
		RUN sh -c "[[ ! -e /bla/README.md ]]"
		RUN sh -c "[[ ! -e /bla/dir/foo ]]"
		RUN sh -c "[[ ! -e /bla/foo ]]"
		RUN sh -c "[[ ! -e /bla/.git ]]"
		RUN sh -c "[[ ! -e v.cc ]]"
		RUN sh -c "[[ ! -e src/v.cc ]]"
		RUN sh -c "[[ ! -e src/_vendor/v.cc ]]"`),
		build.WithFile("Makefile", "all:"),
		build.WithFile(".git/HEAD", "ref: foo"),
		build.WithFile("src/x.go", "package main"),
		build.WithFile("src/_vendor/v.go", "package main"),
		build.WithFile("src/_vendor/v.cc", "package main"),
		build.WithFile("src/v.cc", "package main"),
		build.WithFile("v.cc", "package main"),
		build.WithFile("dir/foo", ""),
		build.WithFile(".gitignore", ""),
		build.WithFile("README.md", "readme"),
		build.WithFile(".dockerignore", `
.git
pkg
.gitignore
src/_vendor
*.md
**/*.cc
dir`),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoreCleanPaths(c *testing.T) {
	const name = "testbuilddockerignorecleanpaths"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
        FROM busybox
        ADD . /tmp/
        RUN sh -c "(! ls /tmp/foo) && (! ls /tmp/foo2) && (! ls /tmp/dir1/foo)"`),
		build.WithFile("foo", "foo"),
		build.WithFile("foo2", "foo2"),
		build.WithFile("dir1/foo", "foo in dir1"),
		build.WithFile(".dockerignore", "./foo\ndir1//foo\n./dir1/../foo2"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoreExceptions(c *testing.T) {
	const name = "testbuilddockerignoreexceptions"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox
		ADD . /bla
		RUN sh -c "[[ -f /bla/src/x.go ]]"
		RUN sh -c "[[ -f /bla/Makefile ]]"
		RUN sh -c "[[ ! -e /bla/src/_vendor ]]"
		RUN sh -c "[[ ! -e /bla/.gitignore ]]"
		RUN sh -c "[[ ! -e /bla/README.md ]]"
		RUN sh -c "[[  -e /bla/dir/dir/foo ]]"
		RUN sh -c "[[ ! -e /bla/dir/foo1 ]]"
		RUN sh -c "[[ -f /bla/dir/e ]]"
		RUN sh -c "[[ -f /bla/dir/e-dir/foo ]]"
		RUN sh -c "[[ ! -e /bla/foo ]]"
		RUN sh -c "[[ ! -e /bla/.git ]]"
		RUN sh -c "[[ -e /bla/dir/a.cc ]]"`),
		build.WithFile("Makefile", "all:"),
		build.WithFile(".git/HEAD", "ref: foo"),
		build.WithFile("src/x.go", "package main"),
		build.WithFile("src/_vendor/v.go", "package main"),
		build.WithFile("dir/foo", ""),
		build.WithFile("dir/foo1", ""),
		build.WithFile("dir/dir/f1", ""),
		build.WithFile("dir/dir/foo", ""),
		build.WithFile("dir/e", ""),
		build.WithFile("dir/e-dir/foo", ""),
		build.WithFile(".gitignore", ""),
		build.WithFile("README.md", "readme"),
		build.WithFile("dir/a.cc", "hello"),
		build.WithFile(".dockerignore", `
.git
pkg
.gitignore
src/_vendor
*.md
dir
!dir/e*
!dir/dir/foo
**/*.cc
!**/*.cc`),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringDockerfile(c *testing.T) {
	const name = "testbuilddockerignoredockerfile"
	dockerfile := `
		FROM busybox
		ADD . /tmp/
		RUN sh -c "! ls /tmp/Dockerfile"
		RUN ls /tmp/.dockerignore`
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", "Dockerfile\n"),
	))
	// FIXME(vdemeester) why twice ?
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", "./Dockerfile\n"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringRenamedDockerfile(c *testing.T) {
	const name = "testbuilddockerignoredockerfile"
	dockerfile := `
		FROM busybox
		ADD . /tmp/
		RUN ls /tmp/Dockerfile
		RUN sh -c "! ls /tmp/MyDockerfile"
		RUN ls /tmp/.dockerignore`
	cli.BuildCmd(c, name, cli.WithFlags("-f", "MyDockerfile"), build.WithBuildContext(c,
		build.WithFile("Dockerfile", "Should not use me"),
		build.WithFile("MyDockerfile", dockerfile),
		build.WithFile(".dockerignore", "MyDockerfile\n"),
	))
	// FIXME(vdemeester) why twice ?
	cli.BuildCmd(c, name, cli.WithFlags("-f", "MyDockerfile"), build.WithBuildContext(c,
		build.WithFile("Dockerfile", "Should not use me"),
		build.WithFile("MyDockerfile", dockerfile),
		build.WithFile(".dockerignore", "./MyDockerfile\n"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringDockerignore(c *testing.T) {
	const name = "testbuilddockerignoredockerignore"
	dockerfile := `
		FROM busybox
		ADD . /tmp/
		RUN sh -c "! ls /tmp/.dockerignore"
		RUN ls /tmp/Dockerfile`
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", ".dockerignore\n"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoreTouchDockerfile(c *testing.T) {
	const name = "testbuilddockerignoretouchdockerfile"
	dockerfile := `
        FROM busybox
		ADD . /tmp/`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			".dockerignore": "Dockerfile\n",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, name)

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("Didn't use the cache - 1")
	}

	// Now make sure touching Dockerfile doesn't invalidate the cache
	if err := ctx.Add("Dockerfile", dockerfile+"\n# hi"); err != nil {
		c.Fatalf("Didn't add Dockerfile: %s", err)
	}
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 = getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("Didn't use the cache - 2")
	}

	// One more time but just 'touch' it instead of changing the content
	if err := ctx.Add("Dockerfile", dockerfile+"\n# hi"); err != nil {
		c.Fatalf("Didn't add Dockerfile: %s", err)
	}
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	id2 = getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("Didn't use the cache - 3")
	}
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringWholeDir(c *testing.T) {
	const name = "testbuilddockerignorewholedir"

	dockerfile := `
		FROM busybox
		COPY . /
		RUN sh -c "[[ ! -e /.gitignore ]]"
		RUN sh -c "[[ ! -e /Makefile ]]"`

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", "*\n"),
		build.WithFile("Makefile", "all:"),
		build.WithFile(".gitignore", ""),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringOnlyDotfiles(c *testing.T) {
	const name = "testbuilddockerignorewholedir"

	dockerfile := `
		FROM busybox
		COPY . /
		RUN sh -c "[[ ! -e /.gitignore ]]"
		RUN sh -c "[[ -f /Makefile ]]"`

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", ".*"),
		build.WithFile("Makefile", "all:"),
		build.WithFile(".gitignore", ""),
	))
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringBadExclusion(c *testing.T) {
	const name = "testbuilddockerignorebadexclusion"
	cli.Docker(cli.Args("build", "-t", name), build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox
		COPY . /
		RUN sh -c "[[ ! -e /.gitignore ]]"
		RUN sh -c "[[ -f /Makefile ]]"`),
		build.WithFile("Makefile", "all:"),
		build.WithFile(".gitignore", ""),
		build.WithFile(".dockerignore", "!\n"),
	)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      `illegal exclusion pattern: "!"`,
	})
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringWildTopDir(c *testing.T) {
	dockerfile := `
		FROM busybox
		COPY . /
		RUN sh -c "[[ ! -e /.dockerignore ]]"
		RUN sh -c "[[ ! -e /Dockerfile ]]"
		RUN sh -c "[[ ! -e /file1 ]]"
		RUN sh -c "[[ ! -e /dir ]]"`

	// All of these should result in ignoring all files
	for _, variant := range []string{"**", "**/", "**/**", "*"} {
		cli.BuildCmd(c, "noname", build.WithBuildContext(c,
			build.WithFile("Dockerfile", dockerfile),
			build.WithFile("file1", ""),
			build.WithFile("dir/file1", ""),
			build.WithFile(".dockerignore", variant),
		))

		cli.DockerCmd(c, "rmi", "noname")
	}
}

func (s *DockerCLIBuildSuite) TestBuildDockerignoringWildDirs(c *testing.T) {
	dockerfile := `
        FROM busybox
		COPY . /
		#RUN sh -c "[[ -e /.dockerignore ]]"
		RUN sh -c "[[ -e /Dockerfile ]]           && \
		           [[ ! -e /file0 ]]              && \
		           [[ ! -e /dir1/file0 ]]         && \
		           [[ ! -e /dir2/file0 ]]         && \
		           [[ ! -e /file1 ]]              && \
		           [[ ! -e /dir1/file1 ]]         && \
		           [[ ! -e /dir1/dir2/file1 ]]    && \
		           [[ ! -e /dir1/file2 ]]         && \
		           [[   -e /dir1/dir2/file2 ]]    && \
		           [[ ! -e /dir1/dir2/file4 ]]    && \
		           [[ ! -e /dir1/dir2/file5 ]]    && \
		           [[ ! -e /dir1/dir2/file6 ]]    && \
		           [[ ! -e /dir1/dir3/file7 ]]    && \
		           [[ ! -e /dir1/dir3/file8 ]]    && \
		           [[   -e /dir1/dir3 ]]          && \
		           [[   -e /dir1/dir4 ]]          && \
		           [[ ! -e 'dir1/dir5/fileAA' ]]  && \
		           [[   -e 'dir1/dir5/fileAB' ]]  && \
		           [[   -e 'dir1/dir5/fileB' ]]"   # "." in pattern means nothing

		RUN echo all done!`

	dockerignore := `
**/file0
**/*file1
**/dir1/file2
dir1/**/file4
**/dir2/file5
**/dir1/dir2/file6
dir1/dir3/**
**/dir4/**
**/file?A
**/file\?B
**/dir5/file.
`

	cli.BuildCmd(c, "noname", build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", dockerignore),
		build.WithFile("dir1/file0", ""),
		build.WithFile("dir1/dir2/file0", ""),
		build.WithFile("file1", ""),
		build.WithFile("dir1/file1", ""),
		build.WithFile("dir1/dir2/file1", ""),
		build.WithFile("dir1/file2", ""),
		build.WithFile("dir1/dir2/file2", ""), // remains
		build.WithFile("dir1/dir2/file4", ""),
		build.WithFile("dir1/dir2/file5", ""),
		build.WithFile("dir1/dir2/file6", ""),
		build.WithFile("dir1/dir3/file7", ""),
		build.WithFile("dir1/dir3/file8", ""),
		build.WithFile("dir1/dir4/file9", ""),
		build.WithFile("dir1/dir5/fileAA", ""),
		build.WithFile("dir1/dir5/fileAB", ""),
		build.WithFile("dir1/dir5/fileB", ""),
	))
}

func (s *DockerCLIBuildSuite) TestBuildLineBreak(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildlinebreak"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM  busybox
RUN    sh -c 'echo root:testpass \
	> /tmp/passwd'
RUN    mkdir -p /var/run/sshd
RUN    sh -c "[ "$(cat /tmp/passwd)" = "root:testpass" ]"
RUN    sh -c "[ "$(ls -d /var/run/sshd)" = "/var/run/sshd" ]"`))
}

func (s *DockerCLIBuildSuite) TestBuildEOLInLine(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildeolinline"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM   busybox
RUN    sh -c 'echo root:testpass > /tmp/passwd'
RUN    echo "foo \n bar"; echo "baz"
RUN    mkdir -p /var/run/sshd
RUN    sh -c "[ "$(cat /tmp/passwd)" = "root:testpass" ]"
RUN    sh -c "[ "$(ls -d /var/run/sshd)" = "/var/run/sshd" ]"`))
}

func (s *DockerCLIBuildSuite) TestBuildCommentsShebangs(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildcomments"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
# This is an ordinary comment.
RUN { echo '#!/bin/sh'; echo 'echo hello world'; } > /hello.sh
RUN [ ! -x /hello.sh ]
# comment with line break \
RUN chmod +x /hello.sh
RUN [ -x /hello.sh ]
RUN [ "$(cat /hello.sh)" = $'#!/bin/sh\necho hello world' ]
RUN [ "$(/hello.sh)" = "hello world" ]`))
}

func (s *DockerCLIBuildSuite) TestBuildUsersAndGroups(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildusers"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox

# Make sure our defaults work
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)" = '0:0/root:root' ]

# TODO decide if "args.user = strconv.Itoa(syscall.Getuid())" is acceptable behavior for changeUser in sysvinit instead of "return nil" when "USER" isn't specified (so that we get the proper group list even if that is the empty list, even in the default case of not supplying an explicit USER to run as, which implies USER 0)
USER root
RUN [ "$(id -G):$(id -Gn)" = '0 10:root wheel' ]

# Setup dockerio user and group
RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd && \
	echo 'dockerio:x:1001:' >> /etc/group

# Make sure we can switch to our user and all the information is exactly as we expect it to be
USER dockerio
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001:dockerio' ]

# Switch back to root and double check that worked exactly as we might expect it to
USER root
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '0:0/root:root/0 10:root wheel' ] && \
        # Add a "supplementary" group for our dockerio user
	echo 'supplementary:x:1002:dockerio' >> /etc/group

# ... and then go verify that we get it like we expect
USER dockerio
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001 1002:dockerio supplementary' ]
USER 1001
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001 1002:dockerio supplementary' ]

# super test the new "user:group" syntax
USER dockerio:dockerio
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001:dockerio' ]
USER 1001:dockerio
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001:dockerio' ]
USER dockerio:1001
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001:dockerio' ]
USER 1001:1001
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1001/dockerio:dockerio/1001:dockerio' ]
USER dockerio:supplementary
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1002/dockerio:supplementary/1002:supplementary' ]
USER dockerio:1002
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1002/dockerio:supplementary/1002:supplementary' ]
USER 1001:supplementary
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1002/dockerio:supplementary/1002:supplementary' ]
USER 1001:1002
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1001:1002/dockerio:supplementary/1002:supplementary' ]

# make sure unknown uid/gid still works properly
USER 1042:1043
RUN [ "$(id -u):$(id -g)/$(id -un):$(id -gn)/$(id -G):$(id -Gn)" = '1042:1043/1042:1043/1043:1043' ]`))
}

// FIXME(vdemeester) rename this test (and probably "merge" it with the one below TestBuildEnvUsage2)
func (s *DockerCLIBuildSuite) TestBuildEnvUsage(c *testing.T) {
	// /docker/world/hello is not owned by the correct user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvusage"
	dockerfile := `FROM busybox
ENV    HOME /root
ENV    PATH $HOME/bin:$PATH
ENV    PATH /tmp:$PATH
RUN    [ "$PATH" = "/tmp:$HOME/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" ]
ENV    FOO /foo/baz
ENV    BAR /bar
ENV    BAZ $BAR
ENV    FOOPATH $PATH:$FOO
RUN    [ "$BAR" = "$BAZ" ]
RUN    [ "$FOOPATH" = "$PATH:/foo/baz" ]
ENV    FROM hello/docker/world
ENV    TO /docker/world/hello
ADD    $FROM $TO
RUN    [ "$(cat $TO)" = "hello" ]
ENV    abc=def
ENV    ghi=$abc
RUN    [ "$ghi" = "def" ]
`
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("hello/docker/world", "hello"),
	))
}

// FIXME(vdemeester) rename this test (and probably "merge" it with the one above TestBuildEnvUsage)
func (s *DockerCLIBuildSuite) TestBuildEnvUsage2(c *testing.T) {
	// /docker/world/hello is not owned by the correct user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	const name = "testbuildenvusage2"
	dockerfile := `FROM busybox
ENV    abc=def def="hello world"
RUN    [ "$abc,$def" = "def,hello world" ]
ENV    def=hello\ world v1=abc v2="hi there" v3='boogie nights' v4="with'quotes too"
RUN    [ "$def,$v1,$v2,$v3,$v4" = "hello world,abc,hi there,boogie nights,with'quotes too" ]
ENV    abc=zzz FROM=hello/docker/world
ENV    abc=zzz TO=/docker/world/hello
ADD    $FROM $TO
RUN    [ "$abc,$(cat $TO)" = "zzz,hello" ]
ENV    abc 'yyy'
RUN    [ $abc = 'yyy' ]
ENV    abc=
RUN    [ "$abc" = "" ]

# use grep to make sure if the builder substitutes \$foo by mistake
# we don't get a false positive
ENV    abc=\$foo
RUN    [ "$abc" = "\$foo" ] && (echo "$abc" | grep foo)
ENV    abc \$foo
RUN    [ "$abc" = "\$foo" ] && (echo "$abc" | grep foo)

ENV    abc=\'foo\' abc2=\"foo\"
RUN    [ "$abc,$abc2" = "'foo',\"foo\"" ]
ENV    abc "foo"
RUN    [ "$abc" = "foo" ]
ENV    abc 'foo'
RUN    [ "$abc" = 'foo' ]
ENV    abc \'foo\'
RUN    [ "$abc" = "'foo'" ]
ENV    abc \"foo\"
RUN    [ "$abc" = '"foo"' ]

ENV    abc=ABC
RUN    [ "$abc" = "ABC" ]
ENV    def1=${abc:-DEF} def2=${ccc:-DEF}
ENV    def3=${ccc:-${def2}xx} def4=${abc:+ALT} def5=${def2:+${abc}:} def6=${ccc:-\$abc:} def7=${ccc:-\${abc}:}
RUN    [ "$def1,$def2,$def3,$def4,$def5,$def6,$def7" = 'ABC,DEF,DEFxx,ALT,ABC:,$abc:,${abc:}' ]
ENV    mypath=${mypath:+$mypath:}/home
ENV    mypath=${mypath:+$mypath:}/away
RUN    [ "$mypath" = '/home:/away' ]

ENV    e1=bar
ENV    e2=$e1 e3=$e11 e4=\$e1 e5=\$e11
RUN    [ "$e0,$e1,$e2,$e3,$e4,$e5" = ',bar,bar,,$e1,$e11' ]

ENV    ee1 bar
ENV    ee2 $ee1
ENV    ee3 $ee11
ENV    ee4 \$ee1
ENV    ee5 \$ee11
RUN    [ "$ee1,$ee2,$ee3,$ee4,$ee5" = 'bar,bar,,$ee1,$ee11' ]

ENV    eee1="foo" eee2='foo'
ENV    eee3 "foo"
ENV    eee4 'foo'
RUN    [ "$eee1,$eee2,$eee3,$eee4" = 'foo,foo,foo,foo' ]

`
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("hello/docker/world", "hello"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildAddScript(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildaddscript"
	dockerfile := `
FROM busybox
ADD test /test
RUN ["chmod","+x","/test"]
RUN ["/test"]
RUN [ "$(cat /testfile)" = 'test!' ]`

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("test", "#!/bin/sh\necho 'test!' > /testfile"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildAddTar(c *testing.T) {
	// /test/foo is not owned by the correct user
	testRequires(c, NotUserNamespace)
	const name = "testbuildaddtar"

	ctx := func() *fakecontext.Fake {
		dockerfile := `
FROM busybox
ADD test.tar /
RUN cat /test/foo | grep Hi
ADD test.tar /test.tar
RUN cat /test.tar/test/foo | grep Hi
ADD test.tar /unlikely-to-exist
RUN cat /unlikely-to-exist/test/foo | grep Hi
ADD test.tar /unlikely-to-exist-trailing-slash/
RUN cat /unlikely-to-exist-trailing-slash/test/foo | grep Hi
RUN sh -c "mkdir /existing-directory" #sh -c is needed on Windows to use the correct mkdir
ADD test.tar /existing-directory
RUN cat /existing-directory/test/foo | grep Hi
ADD test.tar /existing-directory-trailing-slash/
RUN cat /existing-directory-trailing-slash/test/foo | grep Hi`
		tmpDir, err := os.MkdirTemp("", "fake-context")
		assert.NilError(c, err)
		testTar, err := os.Create(filepath.Join(tmpDir, "test.tar"))
		if err != nil {
			c.Fatalf("failed to create test.tar archive: %v", err)
		}
		defer testTar.Close()

		tw := tar.NewWriter(testTar)

		if err := tw.WriteHeader(&tar.Header{
			Name: "test/foo",
			Size: 2,
		}); err != nil {
			c.Fatalf("failed to write tar file header: %v", err)
		}
		if _, err := tw.Write([]byte("Hi")); err != nil {
			c.Fatalf("failed to write tar file content: %v", err)
		}
		if err := tw.Close(); err != nil {
			c.Fatalf("failed to close tar archive: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			c.Fatalf("failed to open destination dockerfile: %v", err)
		}
		return fakecontext.New(c, tmpDir)
	}()
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
}

func (s *DockerCLIBuildSuite) TestBuildAddBrokenTar(c *testing.T) {
	const name = "testbuildaddbrokentar"

	ctx := func() *fakecontext.Fake {
		dockerfile := `
FROM busybox
ADD test.tar /`
		tmpDir, err := os.MkdirTemp("", "fake-context")
		assert.NilError(c, err)
		testTar, err := os.Create(filepath.Join(tmpDir, "test.tar"))
		if err != nil {
			c.Fatalf("failed to create test.tar archive: %v", err)
		}
		defer testTar.Close()

		tw := tar.NewWriter(testTar)

		if err := tw.WriteHeader(&tar.Header{
			Name: "test/foo",
			Size: 2,
		}); err != nil {
			c.Fatalf("failed to write tar file header: %v", err)
		}
		if _, err := tw.Write([]byte("Hi")); err != nil {
			c.Fatalf("failed to write tar file content: %v", err)
		}
		if err := tw.Close(); err != nil {
			c.Fatalf("failed to close tar archive: %v", err)
		}

		// Corrupt the tar by removing one byte off the end
		stat, err := testTar.Stat()
		if err != nil {
			c.Fatalf("failed to stat tar archive: %v", err)
		}
		if err := testTar.Truncate(stat.Size() - 1); err != nil {
			c.Fatalf("failed to truncate tar archive: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			c.Fatalf("failed to open destination dockerfile: %v", err)
		}
		return fakecontext.New(c, tmpDir)
	}()
	defer ctx.Close()

	cli.Docker(cli.Args("build", "-t", name), build.WithExternalBuildContext(ctx)).Assert(c, icmd.Expected{
		ExitCode: 1,
	})
}

func (s *DockerCLIBuildSuite) TestBuildAddNonTar(c *testing.T) {
	const name = "testbuildaddnontar"

	// Should not try to extract test.tar
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox
		ADD test.tar /
		RUN test -f /test.tar`),
		build.WithFile("test.tar", "not_a_tar_file"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildAddTarXz(c *testing.T) {
	// /test/foo is not owned by the correct user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	const name = "testbuildaddtarxz"

	ctx := func() *fakecontext.Fake {
		dockerfile := `
			FROM busybox
			ADD test.tar.xz /
			RUN cat /test/foo | grep Hi`
		tmpDir, err := os.MkdirTemp("", "fake-context")
		assert.NilError(c, err)
		testTar, err := os.Create(filepath.Join(tmpDir, "test.tar"))
		if err != nil {
			c.Fatalf("failed to create test.tar archive: %v", err)
		}
		defer testTar.Close()

		tw := tar.NewWriter(testTar)

		if err := tw.WriteHeader(&tar.Header{
			Name: "test/foo",
			Size: 2,
		}); err != nil {
			c.Fatalf("failed to write tar file header: %v", err)
		}
		if _, err := tw.Write([]byte("Hi")); err != nil {
			c.Fatalf("failed to write tar file content: %v", err)
		}
		if err := tw.Close(); err != nil {
			c.Fatalf("failed to close tar archive: %v", err)
		}

		icmd.RunCmd(icmd.Cmd{
			Command: []string{"xz", "-k", "test.tar"},
			Dir:     tmpDir,
		}).Assert(c, icmd.Success)
		if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			c.Fatalf("failed to open destination dockerfile: %v", err)
		}
		return fakecontext.New(c, tmpDir)
	}()

	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
}

func (s *DockerCLIBuildSuite) TestBuildAddTarXzGz(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildaddtarxzgz"

	ctx := func() *fakecontext.Fake {
		dockerfile := `
			FROM busybox
			ADD test.tar.xz.gz /
			RUN ls /test.tar.xz.gz`
		tmpDir, err := os.MkdirTemp("", "fake-context")
		assert.NilError(c, err)
		testTar, err := os.Create(filepath.Join(tmpDir, "test.tar"))
		if err != nil {
			c.Fatalf("failed to create test.tar archive: %v", err)
		}
		defer testTar.Close()

		tw := tar.NewWriter(testTar)

		if err := tw.WriteHeader(&tar.Header{
			Name: "test/foo",
			Size: 2,
		}); err != nil {
			c.Fatalf("failed to write tar file header: %v", err)
		}
		if _, err := tw.Write([]byte("Hi")); err != nil {
			c.Fatalf("failed to write tar file content: %v", err)
		}
		if err := tw.Close(); err != nil {
			c.Fatalf("failed to close tar archive: %v", err)
		}

		icmd.RunCmd(icmd.Cmd{
			Command: []string{"xz", "-k", "test.tar"},
			Dir:     tmpDir,
		}).Assert(c, icmd.Success)

		icmd.RunCmd(icmd.Cmd{
			Command: []string{"gzip", "test.tar.xz"},
			Dir:     tmpDir,
		})
		if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			c.Fatalf("failed to open destination dockerfile: %v", err)
		}
		return fakecontext.New(c, tmpDir)
	}()

	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
}

// FIXME(vdemeester) most of the from git tests could be moved to `docker/cli` e2e tests
func (s *DockerCLIBuildSuite) TestBuildFromGit(c *testing.T) {
	const name = "testbuildfromgit"
	git := fakegit.New(c, "repo", map[string]string{
		"Dockerfile": `FROM busybox
		ADD first /first
		RUN [ -f /first ]
		MAINTAINER docker`,
		"first": "test git data",
	}, true)
	defer git.Close()

	cli.BuildCmd(c, name, build.WithContextPath(git.RepoURL))

	res := inspectField(c, name, "Author")
	if res != "docker" {
		c.Fatalf("Maintainer should be docker, got %s", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildFromGitWithContext(c *testing.T) {
	const name = "testbuildfromgit"
	git := fakegit.New(c, "repo", map[string]string{
		"docker/Dockerfile": `FROM busybox
					ADD first /first
					RUN [ -f /first ]
					MAINTAINER docker`,
		"docker/first": "test git data",
	}, true)
	defer git.Close()

	cli.BuildCmd(c, name, build.WithContextPath(git.RepoURL+"#master:docker"))

	res := inspectField(c, name, "Author")
	if res != "docker" {
		c.Fatalf("Maintainer should be docker, got %s", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildFromGitWithF(c *testing.T) {
	const name = "testbuildfromgitwithf"
	git := fakegit.New(c, "repo", map[string]string{
		"myApp/myDockerfile": `FROM busybox
					RUN echo hi from Dockerfile`,
	}, true)
	defer git.Close()

	cli.Docker(cli.Args("build", "-t", name), cli.WithFlags("-f", "myApp/myDockerfile"), build.WithContextPath(git.RepoURL)).Assert(c, icmd.Expected{
		Out: "hi from Dockerfile",
	})
}

func (s *DockerCLIBuildSuite) TestBuildFromRemoteTarball(c *testing.T) {
	const name = "testbuildfromremotetarball"

	buffer := new(bytes.Buffer)
	tw := tar.NewWriter(buffer)
	defer tw.Close()

	dockerfile := []byte(`FROM busybox
					MAINTAINER docker`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Size: int64(len(dockerfile)),
	}); err != nil {
		c.Fatalf("failed to write tar file header: %v", err)
	}
	if _, err := tw.Write(dockerfile); err != nil {
		c.Fatalf("failed to write tar file content: %v", err)
	}
	if err := tw.Close(); err != nil {
		c.Fatalf("failed to close tar archive: %v", err)
	}

	server := fakestorage.New(c, "", fakecontext.WithBinaryFiles(map[string]*bytes.Buffer{
		"testT.tar": buffer,
	}))
	defer server.Close()

	cli.BuildCmd(c, name, build.WithContextPath(server.URL()+"/testT.tar"))

	res := inspectField(c, name, "Author")
	if res != "docker" {
		c.Fatalf("Maintainer should be docker, got %s", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildCleanupCmdOnEntrypoint(c *testing.T) {
	const name = "testbuildcmdcleanuponentrypoint"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
		CMD ["test"]
		ENTRYPOINT ["echo"]`))
	cli.BuildCmd(c, name, build.WithDockerfile(fmt.Sprintf(`FROM %s
		ENTRYPOINT ["cat"]`, name)))

	res := inspectField(c, name, "Config.Cmd")
	if res != "[]" {
		c.Fatalf("Cmd %s, expected nil", res)
	}
	res = inspectField(c, name, "Config.Entrypoint")
	if expected := "[cat]"; res != expected {
		c.Fatalf("Entrypoint %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildClearCmd(c *testing.T) {
	const name = "testbuildclearcmd"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
   ENTRYPOINT ["/bin/bash"]
   CMD []`))

	cmd := inspectFieldJSON(c, name, "Config.Cmd")
	assert.Check(c, is.Equal(cmd, "null"))
}

func (s *DockerCLIBuildSuite) TestBuildEmptyCmd(c *testing.T) {
	// Skip on Windows. Base image on Windows has a CMD set in the image.
	testRequires(c, DaemonIsLinux)

	const name = "testbuildemptycmd"
	cli.BuildCmd(c, name, build.WithDockerfile("FROM "+minimalBaseImage()+"\nMAINTAINER quux\n"))

	res := inspectFieldJSON(c, name, "Config.Cmd")
	if res != "null" {
		c.Fatalf("Cmd %s, expected %s", res, "null")
	}
}

func (s *DockerCLIBuildSuite) TestBuildOnBuildOutput(c *testing.T) {
	const name = "testbuildonbuildparent"
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nONBUILD RUN echo foo\n"))

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile("FROM "+name+"\nMAINTAINER quux\n")).Assert(c, icmd.Expected{
		Out: "# Executing 1 build trigger",
	})
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildInvalidTag(c *testing.T) {
	name := "abcd:" + testutil.RandomAlpha(200)
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile("FROM "+minimalBaseImage()+"\nMAINTAINER quux\n")).Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "invalid reference format",
	})
}

func (s *DockerCLIBuildSuite) TestBuildCmdShDashC(c *testing.T) {
	const name = "testbuildcmdshc"
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nCMD echo cmd\n"))

	res := inspectFieldJSON(c, name, "Config.Cmd")
	expected := `["/bin/sh","-c","echo cmd"]`
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `["cmd /S /C echo cmd"]`
	}
	if res != expected {
		c.Fatalf("Expected value %s not in Config.Cmd: %s", expected, res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildCmdSpaces(c *testing.T) {
	// Test to make sure that when we strcat arrays we take into account
	// the arg separator to make sure ["echo","hi"] and ["echo hi"] don't
	// look the same
	const name = "testbuildcmdspaces"

	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nCMD [\"echo hi\"]\n"))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nCMD [\"echo\", \"hi\"]\n"))
	id2 := getIDByName(c, name)

	if id1 == id2 {
		c.Fatal("Should not have resulted in the same CMD")
	}

	// Now do the same with ENTRYPOINT
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nENTRYPOINT [\"echo hi\"]\n"))
	id1 = getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nENTRYPOINT [\"echo\", \"hi\"]\n"))
	id2 = getIDByName(c, name)

	if id1 == id2 {
		c.Fatal("Should not have resulted in the same ENTRYPOINT")
	}
}

func (s *DockerCLIBuildSuite) TestBuildCmdJSONNoShDashC(c *testing.T) {
	const name = "testbuildcmdjson"
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nCMD [\"echo\", \"cmd\"]"))

	res := inspectFieldJSON(c, name, "Config.Cmd")
	expected := `["echo","cmd"]`
	if res != expected {
		c.Fatalf("Expected value %s not in Config.Cmd: %s", expected, res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildEntrypointCanBeOverriddenByChild(c *testing.T) {
	cli.BuildCmd(c, "parent", build.WithDockerfile(`
    FROM busybox
    ENTRYPOINT exit 130
    `))

	icmd.RunCommand(dockerBinary, "run", "parent").Assert(c, icmd.Expected{
		ExitCode: 130,
	})

	cli.BuildCmd(c, "child", build.WithDockerfile(`
    FROM parent
    ENTRYPOINT exit 5
    `))

	icmd.RunCommand(dockerBinary, "run", "child").Assert(c, icmd.Expected{
		ExitCode: 5,
	})
}

func (s *DockerCLIBuildSuite) TestBuildEntrypointCanBeOverriddenByChildInspect(c *testing.T) {
	var (
		name     = "testbuildepinherit"
		name2    = "testbuildepinherit2"
		expected = `["/bin/sh","-c","echo quux"]`
	)

	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `["cmd /S /C echo quux"]`
	}

	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nENTRYPOINT /foo/bar"))
	cli.BuildCmd(c, name2, build.WithDockerfile(fmt.Sprintf("FROM %s\nENTRYPOINT echo quux", name)))

	res := inspectFieldJSON(c, name2, "Config.Entrypoint")
	if res != expected {
		c.Fatalf("Expected value %s not in Config.Entrypoint: %s", expected, res)
	}

	icmd.RunCommand(dockerBinary, "run", name2).Assert(c, icmd.Expected{
		Out: "quux",
	})
}

func (s *DockerCLIBuildSuite) TestBuildRunShEntrypoint(c *testing.T) {
	const name = "testbuildentrypoint"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
                                ENTRYPOINT echo`))
	cli.DockerCmd(c, "run", "--rm", name)
}

func (s *DockerCLIBuildSuite) TestBuildExoticShellInterpolation(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildexoticshellinterpolation"

	cli.BuildCmd(c, name, build.WithDockerfile(`
		FROM busybox

		ENV SOME_VAR a.b.c

		RUN [ "$SOME_VAR"       = 'a.b.c' ]
		RUN [ "${SOME_VAR}"     = 'a.b.c' ]
		RUN [ "${SOME_VAR%.*}"  = 'a.b'   ]
		RUN [ "${SOME_VAR%%.*}" = 'a'     ]
		RUN [ "${SOME_VAR#*.}"  = 'b.c'   ]
		RUN [ "${SOME_VAR##*.}" = 'c'     ]
		RUN [ "${SOME_VAR/c/d}" = 'a.b.d' ]
		RUN [ "${#SOME_VAR}"    = '5'     ]

		RUN [ "${SOME_UNSET_VAR:-$SOME_VAR}" = 'a.b.c' ]
		RUN [ "${SOME_VAR:+Version: ${SOME_VAR}}" = 'Version: a.b.c' ]
		RUN [ "${SOME_UNSET_VAR:+${SOME_VAR}}" = '' ]
		RUN [ "${SOME_UNSET_VAR:-${SOME_VAR:-d.e.f}}" = 'a.b.c' ]
	`))
}

func (s *DockerCLIBuildSuite) TestBuildVerifySingleQuoteFails(c *testing.T) {
	// This testcase is supposed to generate an error because the
	// JSON array we're passing in on the CMD uses single quotes instead
	// of double quotes (per the JSON spec). This means we interpret it
	// as a "string" instead of "JSON array" and pass it on to "sh -c" and
	// it should barf on it.
	const name = "testbuildsinglequotefails"
	expectedExitCode := 2

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		CMD [ '/bin/sh', '-c', 'echo hi' ]`))

	icmd.RunCommand(dockerBinary, "run", "--rm", name).Assert(c, icmd.Expected{
		ExitCode: expectedExitCode,
	})
}

func (s *DockerCLIBuildSuite) TestBuildVerboseOut(c *testing.T) {
	const name = "testbuildverboseout"
	expected := "\n123\n"

	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "\n123\r\n"
	}

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
RUN echo 123`)).Assert(c, icmd.Expected{
		Out: expected,
	})
}

func (s *DockerCLIBuildSuite) TestBuildWithTabs(c *testing.T) {
	skip.If(c, versions.GreaterThan(testEnv.DaemonAPIVersion(), "1.44"), "ContainerConfig is deprecated")
	skip.If(c, testEnv.UsingSnapshotter, "ContainerConfig is not filled in c8d")

	const name = "testbuildwithtabs"
	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nRUN echo\tone\t\ttwo"))
	res := inspectFieldJSON(c, name, "ContainerConfig.Cmd")
	expected1 := `["/bin/sh","-c","echo\tone\t\ttwo"]`
	expected2 := `["/bin/sh","-c","echo\u0009one\u0009\u0009two"]` // syntactically equivalent, and what Go 1.3 generates
	if testEnv.DaemonInfo.OSType == "windows" {
		expected1 = `["cmd /S /C echo\tone\t\ttwo"]`
		expected2 = `["cmd /S /C echo\u0009one\u0009\u0009two"]` // syntactically equivalent, and what Go 1.3 generates
	}
	if res != expected1 && res != expected2 {
		c.Fatalf("Missing tabs.\nGot: %s\nExp: %s or %s", res, expected1, expected2)
	}
}

func (s *DockerCLIBuildSuite) TestBuildLabels(c *testing.T) {
	const name = "testbuildlabel"
	expected := `{"License":"GPL","Vendor":"Acme"}`
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		LABEL Vendor=Acme
                LABEL License GPL`))
	res := inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}
}

func (s *DockerCLIBuildSuite) TestBuildLabelsCache(c *testing.T) {
	const name = "testbuildlabelcache"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		LABEL Vendor=Acme`))
	id1 := getIDByName(c, name)
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		LABEL Vendor=Acme`))
	id2 := getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("Build 2 should have worked & used cache(%s,%s)", id1, id2)
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		LABEL Vendor=Acme1`))
	id2 = getIDByName(c, name)
	if id1 == id2 {
		c.Fatalf("Build 3 should have worked & NOT used cache(%s,%s)", id1, id2)
	}

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		LABEL Vendor Acme`))
	id2 = getIDByName(c, name)
	if id1 != id2 {
		c.Fatalf("Build 4 should have worked & used cache(%s,%s)", id1, id2)
	}

	// Now make sure the cache isn't used by mistake
	cli.BuildCmd(c, name, build.WithoutCache, build.WithDockerfile(`FROM busybox
       LABEL f1=b1 f2=b2`))

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
       LABEL f1=b1 f2=b2`))
	id2 = getIDByName(c, name)
	if id1 == id2 {
		c.Fatalf("Build 6 should have worked & NOT used the cache(%s,%s)", id1, id2)
	}
}

// FIXME(vdemeester) port to docker/cli e2e tests (api tests should test suppressOutput option though)
func (s *DockerCLIBuildSuite) TestBuildNotVerboseSuccess(c *testing.T) {
	c.Skip("FIXME: test breaks due to classic builder deprecation message") // FIXME(thaJeztah): test breaks due to classic builder deprecation message in output

	// This test makes sure that -q works correctly when build is successful:
	// stdout has only the image ID (long image ID) and stderr is empty.
	outRegexp := regexp.MustCompile(`^(sha256:|)[a-z0-9]{64}\n$`)
	buildFlags := cli.WithFlags("-q")

	tt := []struct {
		Name      string
		BuildFunc func(string) *icmd.Result
	}{
		{
			Name: "quiet_build_stdin_success",
			BuildFunc: func(name string) *icmd.Result {
				return cli.Docker(cli.Args("build", "-t", name), buildFlags, build.WithDockerfile("FROM busybox"))
			},
		},
		{
			Name: "quiet_build_ctx_success",
			BuildFunc: func(name string) *icmd.Result {
				return cli.Docker(cli.Args("build", "-t", name), buildFlags, build.WithBuildContext(c,
					build.WithFile("Dockerfile", "FROM busybox"),
					build.WithFile("quiet_build_success_fctx", "test"),
				))
			},
		},
		{
			Name: "quiet_build_git_success",
			BuildFunc: func(name string) *icmd.Result {
				git := fakegit.New(c, "repo", map[string]string{
					"Dockerfile": "FROM busybox",
				}, true)
				return cli.Docker(cli.Args("build", "-t", name), buildFlags, build.WithContextPath(git.RepoURL))
			},
		},
	}

	for _, te := range tt {
		result := te.BuildFunc(te.Name)
		result.Assert(c, icmd.Success)
		if outRegexp.Find([]byte(result.Stdout())) == nil {
			c.Fatalf("Test %s expected stdout to match the [%v] regexp, but it is [%v]", te.Name, outRegexp, result.Stdout())
		}

		if result.Stderr() != "" {
			c.Fatalf("Test %s expected stderr to be empty, but it is [%#v]", te.Name, result.Stderr())
		}
	}
}

// FIXME(vdemeester) migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildNotVerboseFailureWithNonExistImage(c *testing.T) {
	c.Skip("FIXME: test breaks due to classic builder deprecation message") // FIXME(thaJeztah): test breaks due to classic builder deprecation message in output

	// This test makes sure that -q works correctly when build fails by
	// comparing between the stderr output in quiet mode and in stdout
	// and stderr output in verbose mode
	testRequires(c, Network)
	testName := "quiet_build_not_exists_image"
	dockerfile := "FROM busybox11"
	quietResult := cli.Docker(cli.Args("build", "-t", testName), cli.WithFlags("-q"), build.WithDockerfile(dockerfile))
	quietResult.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	result := cli.Docker(cli.Args("build", "-t", testName), build.WithDockerfile(dockerfile))
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	if quietResult.Stderr() != result.Combined() {
		c.Fatal(fmt.Errorf("Test[%s] expected that quiet stderr and verbose stdout are equal; quiet [%v], verbose [%v]", testName, quietResult.Stderr(), result.Combined()))
	}
}

// FIXME(vdemeester) migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildNotVerboseFailure(c *testing.T) {
	c.Skip("FIXME: test breaks due to classic builder deprecation message") // FIXME(thaJeztah): test breaks due to classic builder deprecation message in output

	// This test makes sure that -q works correctly when build fails by
	// comparing between the stderr output in quiet mode and in stdout
	// and stderr output in verbose mode
	testCases := []struct {
		testName   string
		dockerfile string
	}{
		{"quiet_build_no_from_at_the_beginning", "RUN whoami"},
		{"quiet_build_unknown_instr", "FROMD busybox"},
	}

	for _, tc := range testCases {
		quietResult := cli.Docker(cli.Args("build", "-t", tc.testName), cli.WithFlags("-q"), build.WithDockerfile(tc.dockerfile))
		quietResult.Assert(c, icmd.Expected{
			ExitCode: 1,
		})
		result := cli.Docker(cli.Args("build", "-t", tc.testName), build.WithDockerfile(tc.dockerfile))
		result.Assert(c, icmd.Expected{
			ExitCode: 1,
		})
		if quietResult.Stderr() != result.Combined() {
			c.Fatal(fmt.Errorf("Test[%s] expected that quiet stderr and verbose stdout are equal; quiet [%v], verbose [%v]", tc.testName, quietResult.Stderr(), result.Combined()))
		}
	}
}

// FIXME(vdemeester) migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildNotVerboseFailureRemote(c *testing.T) {
	c.Skip("FIXME: test breaks due to classic builder deprecation message") // FIXME(thaJeztah): test breaks due to classic builder deprecation message in output

	// This test ensures that when given a wrong URL, stderr in quiet mode and
	// stderr in verbose mode are identical.
	// TODO(vdemeester) with cobra, stdout has a carriage return too much so this test should not check stdout
	URL := "http://something.invalid"
	const name = "quiet_build_wrong_remote"
	quietResult := cli.Docker(cli.Args("build", "-t", name), cli.WithFlags("-q"), build.WithContextPath(URL))
	quietResult.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	result := cli.Docker(cli.Args("build", "-t", name), build.WithContextPath(URL))
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	// An error message should contain name server IP and port, like this:
	//  "dial tcp: lookup something.invalid on 172.29.128.11:53: no such host"
	// The IP:port need to be removed in order to not trigger a test failure
	// when more than one nameserver is configured.
	// While at it, also strip excessive newlines.
	normalize := func(msg string) string {
		return strings.TrimSpace(regexp.MustCompile("[1-9][0-9.]+:[0-9]+").ReplaceAllLiteralString(msg, "<ip:port>"))
	}

	if normalize(quietResult.Stderr()) != normalize(result.Combined()) {
		c.Fatal(fmt.Errorf("Test[%s] expected that quiet stderr and verbose stdout are equal; quiet [%v], verbose [%v]", name, quietResult.Stderr(), result.Combined()))
	}
}

// FIXME(vdemeester) migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildStderr(c *testing.T) {
	c.Skip("FIXME: test breaks due to classic builder deprecation message") // FIXME(thaJeztah): test breaks due to classic builder deprecation message in output

	// This test just makes sure that no non-error output goes
	// to stderr
	const name = "testbuildstderr"
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile("FROM busybox\nRUN echo one"))
	result.Assert(c, icmd.Success)

	// Windows to non-Windows should have a security warning
	if runtime.GOOS == "windows" && testEnv.DaemonInfo.OSType != "windows" && !strings.Contains(result.Stdout(), "SECURITY WARNING:") {
		c.Fatalf("Stdout contains unexpected output: %q", result.Stdout())
	}

	// Stderr should always be empty
	if result.Stderr() != "" {
		c.Fatalf("Stderr should have been empty, instead it's: %q", result.Stderr())
	}
}

func (s *DockerCLIBuildSuite) TestBuildChownSingleFile(c *testing.T) {
	testRequires(c, UnixCli, DaemonIsLinux) // test uses chown: not available on windows

	const name = "testbuildchownsinglefile"

	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
FROM busybox
COPY test /
RUN ls -l /test
RUN [ $(ls -l /test | awk '{print $3":"$4}') = 'root:root' ]
`),
		fakecontext.WithFiles(map[string]string{
			"test": "test",
		}))
	defer ctx.Close()

	if err := os.Chown(filepath.Join(ctx.Dir, "test"), 4242, 4242); err != nil {
		c.Fatal(err)
	}

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
}

func (s *DockerCLIBuildSuite) TestBuildSymlinkBreakout(c *testing.T) {
	const name = "testbuildsymlinkbreakout"
	tmpdir, err := os.MkdirTemp("", name)
	assert.NilError(c, err)

	// See https://github.com/moby/moby/pull/37770 for reason for next line.
	tmpdir, err = getLongPathName(tmpdir)
	assert.NilError(c, err)

	defer os.RemoveAll(tmpdir)
	ctx := filepath.Join(tmpdir, "context")
	if err := os.MkdirAll(ctx, 0o755); err != nil {
		c.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(`
	FROM `+minimalBaseImage()+`
	ADD symlink.tar /
	ADD inject /symlink/
	`), 0o644); err != nil {
		c.Fatal(err)
	}
	inject := filepath.Join(ctx, "inject")
	if err := os.WriteFile(inject, nil, 0o644); err != nil {
		c.Fatal(err)
	}
	f, err := os.Create(filepath.Join(ctx, "symlink.tar"))
	if err != nil {
		c.Fatal(err)
	}
	w := tar.NewWriter(f)
	w.WriteHeader(&tar.Header{
		Name:     "symlink2",
		Typeflag: tar.TypeSymlink,
		Linkname: "/../../../../../../../../../../../../../../",
		Uid:      os.Getuid(),
		Gid:      os.Getgid(),
	})
	w.WriteHeader(&tar.Header{
		Name:     "symlink",
		Typeflag: tar.TypeSymlink,
		Linkname: filepath.Join("symlink2", tmpdir),
		Uid:      os.Getuid(),
		Gid:      os.Getgid(),
	})
	w.Close()
	f.Close()

	cli.BuildCmd(c, name, build.WithoutCache, build.WithExternalBuildContext(fakecontext.New(c, ctx)))
	if _, err := os.Lstat(filepath.Join(tmpdir, "inject")); err == nil {
		c.Fatal("symlink breakout - inject")
	} else if !os.IsNotExist(err) {
		c.Fatalf("unexpected error: %v", err)
	}
}

func (s *DockerCLIBuildSuite) TestBuildXZHost(c *testing.T) {
	// /usr/local/sbin/xz gets permission denied for the user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	const name = "testbuildxzhost"

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
FROM busybox
ADD xz /usr/local/sbin/
RUN chmod 755 /usr/local/sbin/xz
ADD test.xz /
RUN [ ! -e /injected ]`),
		build.WithFile("test.xz", "\xfd\x37\x7a\x58\x5a\x00\x00\x04\xe6\xd6\xb4\x46\x02\x00"+"\x21\x01\x16\x00\x00\x00\x74\x2f\xe5\xa3\x01\x00\x3f\xfd"+"\x37\x7a\x58\x5a\x00\x00\x04\xe6\xd6\xb4\x46\x02\x00\x21"),
		build.WithFile("xz", "#!/bin/sh\ntouch /injected"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildVolumesRetainContents(c *testing.T) {
	// /foo/file gets permission denied for the user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux) // TODO Windows: Issue #20127
	var (
		name     = "testbuildvolumescontent"
		expected = "some text"
		volName  = "/foo"
	)

	if testEnv.DaemonInfo.OSType == "windows" {
		volName = "C:/foo"
	}

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
FROM busybox
COPY content /foo/file
VOLUME `+volName+`
CMD cat /foo/file`),
		build.WithFile("content", expected),
	))

	out := cli.DockerCmd(c, "run", "--rm", name).Combined()
	if out != expected {
		c.Fatalf("expected file contents for /foo/file to be %q but received %q", expected, out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildFromMixedcaseDockerfile(c *testing.T) {
	testRequires(c, UnixCli) // Dockerfile overwrites dockerfile on windows
	testRequires(c, DaemonIsLinux)

	// If Dockerfile is not present, use dockerfile
	cli.Docker(cli.Args("build", "-t", "test1"), build.WithBuildContext(c,
		build.WithFile("dockerfile", `FROM busybox
	RUN echo from dockerfile`),
	)).Assert(c, icmd.Expected{
		Out: "from dockerfile",
	})

	// Prefer Dockerfile in place of dockerfile
	cli.Docker(cli.Args("build", "-t", "test1"), build.WithBuildContext(c,
		build.WithFile("dockerfile", `FROM busybox
	RUN echo from dockerfile`),
		build.WithFile("Dockerfile", `FROM busybox
	RUN echo from Dockerfile`),
	)).Assert(c, icmd.Expected{
		Out: "from Dockerfile",
	})
}

// FIXME(vdemeester) should migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildFromURLWithF(c *testing.T) {
	c.Skip("FIXME(thaJeztah): test is broken or invalid on current versions") // FIXME(thaJeztah) produces "ambiguous Dockerfile source: both stdin and flag correspond to Dockerfiles"
	server := fakestorage.New(c, "", fakecontext.WithFiles(map[string]string{"baz": `FROM busybox
RUN echo from baz
COPY * /tmp/
RUN find /tmp/`}))
	defer server.Close()

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`FROM busybox
	RUN echo from Dockerfile`))
	defer ctx.Close()

	// Make sure that -f is ignored and that we don't use the Dockerfile
	// that's in the current dir
	result := cli.BuildCmd(c, "test1", cli.WithFlags("-f", "baz", server.URL()+"/baz"), func(cmd *icmd.Cmd) func() {
		cmd.Dir = ctx.Dir
		return nil
	})

	if !strings.Contains(result.Combined(), "from baz") ||
		strings.Contains(result.Combined(), "/tmp/baz") ||
		!strings.Contains(result.Combined(), "/tmp/Dockerfile") {
		c.Fatalf("Missing proper output: %s", result.Combined())
	}
}

// FIXME(vdemeester) should migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildFromStdinWithF(c *testing.T) {
	testRequires(c, DaemonIsLinux)                                            // TODO Windows: This test is flaky; no idea why
	c.Skip("FIXME(thaJeztah): test is broken or invalid on current versions") // FIXME(thaJeztah) produces "ambiguous Dockerfile source: both stdin and flag correspond to Dockerfiles"

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`FROM busybox
RUN echo "from Dockerfile"`))
	defer ctx.Close()

	// Make sure that -f is ignored and that we don't use the Dockerfile
	// that's in the current dir
	result := cli.BuildCmd(c, "test1", cli.WithFlags("-f", "baz", "-"), func(cmd *icmd.Cmd) func() {
		cmd.Dir = ctx.Dir
		cmd.Stdin = strings.NewReader(`FROM busybox
RUN echo "from baz"
COPY * /tmp/
RUN sh -c "find /tmp/" # sh -c is needed on Windows to use the correct find`)
		return nil
	})

	if !strings.Contains(result.Combined(), "from baz") ||
		strings.Contains(result.Combined(), "/tmp/baz") ||
		!strings.Contains(result.Combined(), "/tmp/Dockerfile") {
		c.Fatalf("Missing proper output: %s", result.Combined())
	}
}

func (s *DockerCLIBuildSuite) TestBuildFromOfficialNames(c *testing.T) {
	const name = "testbuildfromofficial"
	fromNames := []string{
		"busybox",
		"docker.io/busybox",
		"index.docker.io/busybox",
		"library/busybox",
		"docker.io/library/busybox",
		"index.docker.io/library/busybox",
	}
	for idx, fromName := range fromNames {
		imgName := fmt.Sprintf("%s%d", name, idx)
		cli.BuildCmd(c, imgName, build.WithDockerfile("FROM "+fromName))
		cli.DockerCmd(c, "rmi", imgName)
	}
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildSpaces(c *testing.T) {
	// Test to make sure that leading/trailing spaces on a command
	// doesn't change the error msg we get
	const name = "testspaces"
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile("FROM busybox\nCOPY\n"))
	defer ctx.Close()

	result1 := cli.Docker(cli.Args("build", "-t", name), build.WithExternalBuildContext(ctx))
	result1.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	ctx.Add("Dockerfile", "FROM busybox\nCOPY    ")
	result2 := cli.Docker(cli.Args("build", "-t", name), build.WithExternalBuildContext(ctx))
	result2.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	removeLogTimestamps := func(s string) string {
		return regexp.MustCompile(`time="(.*?)"`).ReplaceAllString(s, `time=[TIMESTAMP]`)
	}

	// Skip over the times
	e1 := removeLogTimestamps(result1.Error.Error())
	e2 := removeLogTimestamps(result2.Error.Error())

	// Ignore whitespace since that's what were verifying doesn't change stuff
	if strings.ReplaceAll(e1, " ", "") != strings.ReplaceAll(e2, " ", "") {
		c.Fatalf("Build 2's error wasn't the same as build 1's\n1:%s\n2:%s", result1.Error, result2.Error)
	}

	ctx.Add("Dockerfile", "FROM busybox\n   COPY")
	result2 = cli.Docker(cli.Args("build", "-t", name), build.WithoutCache, build.WithExternalBuildContext(ctx))
	result2.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	// Skip over the times
	e1 = removeLogTimestamps(result1.Error.Error())
	e2 = removeLogTimestamps(result2.Error.Error())

	// Ignore whitespace since that's what were verifying doesn't change stuff
	if strings.ReplaceAll(e1, " ", "") != strings.ReplaceAll(e2, " ", "") {
		c.Fatalf("Build 3's error wasn't the same as build 1's\n1:%s\n3:%s", result1.Error, result2.Error)
	}

	ctx.Add("Dockerfile", "FROM busybox\n   COPY    ")
	result2 = cli.Docker(cli.Args("build", "-t", name), build.WithoutCache, build.WithExternalBuildContext(ctx))
	result2.Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	// Skip over the times
	e1 = removeLogTimestamps(result1.Error.Error())
	e2 = removeLogTimestamps(result2.Error.Error())

	// Ignore whitespace since that's what were verifying doesn't change stuff
	if strings.ReplaceAll(e1, " ", "") != strings.ReplaceAll(e2, " ", "") {
		c.Fatalf("Build 4's error wasn't the same as build 1's\n1:%s\n4:%s", result1.Error, result2.Error)
	}
}

func (s *DockerCLIBuildSuite) TestBuildSpacesWithQuotes(c *testing.T) {
	// Test to make sure that spaces in quotes aren't lost
	const name = "testspacesquotes"

	dockerfile := `FROM busybox
RUN echo "  \
  foo  "`

	expected := "\n    foo  \n"
	// Windows uses the builtin echo, which preserves quotes
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "\"    foo  \""
	}

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(dockerfile)).Assert(c, icmd.Expected{
		Out: expected,
	})
}

// #4393
func (s *DockerCLIBuildSuite) TestBuildVolumeFileExistsinContainer(c *testing.T) {
	testRequires(c, DaemonIsLinux) // TODO Windows: This should error out
	cli.Docker(cli.Args("build", "-t", "docker-test-errcreatevolumewithfile"), build.WithDockerfile(`
	FROM busybox
	RUN touch /foo
	VOLUME /foo
	`)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "file exists",
	})
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildMissingArgs(c *testing.T) {
	// Test to make sure that all Dockerfile commands (except the ones listed
	// in skipCmds) will generate an error if no args are provided.
	// Note: INSERT is deprecated so we exclude it because of that.
	skipCmds := map[string]struct{}{
		"CMD":        {},
		"RUN":        {},
		"ENTRYPOINT": {},
		"INSERT":     {},
	}

	if testEnv.DaemonInfo.OSType == "windows" {
		skipCmds = map[string]struct{}{
			"CMD":        {},
			"RUN":        {},
			"ENTRYPOINT": {},
			"INSERT":     {},
			"STOPSIGNAL": {},
			"ARG":        {},
			"USER":       {},
			"EXPOSE":     {},
		}
	}

	for cmd := range command.Commands {
		cmd = strings.ToUpper(cmd)
		if _, ok := skipCmds[cmd]; ok {
			continue
		}
		var dockerfile string
		if cmd == "FROM" {
			dockerfile = cmd
		} else {
			// Add FROM to make sure we don't complain about it missing
			dockerfile = "FROM busybox\n" + cmd
		}

		cli.Docker(cli.Args("build", "-t", "args"), build.WithDockerfile(dockerfile)).Assert(c, icmd.Expected{
			ExitCode: 1,
			Err:      cmd + " requires",
		})
	}
}

func (s *DockerCLIBuildSuite) TestBuildEmptyScratch(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.Docker(cli.Args("build", "-t", "sc"), build.WithDockerfile("FROM scratch")).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "No image was generated",
	})
}

func (s *DockerCLIBuildSuite) TestBuildDotDotFile(c *testing.T) {
	cli.BuildCmd(c, "sc", build.WithBuildContext(c,
		build.WithFile("Dockerfile", "FROM busybox\n"),
		build.WithFile("..gitme", ""),
	))
}

func (s *DockerCLIBuildSuite) TestBuildRUNoneJSON(c *testing.T) {
	testRequires(c, DaemonIsLinux) // No hello-world Windows image
	const name = "testbuildrunonejson"

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM hello-world:frozen
RUN [ "/hello" ]`)).Assert(c, icmd.Expected{
		Out: "Hello from Docker",
	})
}

func (s *DockerCLIBuildSuite) TestBuildEmptyStringVolume(c *testing.T) {
	const name = "testbuildemptystringvolume"

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`
  FROM busybox
  ENV foo=""
  VOLUME $foo
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
	})
}

func (s *DockerCLIBuildSuite) TestBuildContainerWithCgroupParent(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	cgroupParent := "test"
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		c.Fatalf("failed to read '/proc/self/cgroup - %v", err)
	}
	selfCgroupPaths := ParseCgroupPaths(string(data))
	_, found := selfCgroupPaths["memory"]
	if !found {
		c.Fatalf("unable to find self memory cgroup path. CgroupsPath: %v", selfCgroupPaths)
	}
	result := cli.Docker(cli.Args("build", "-t", "buildcgroupparent"),
		cli.WithFlags("--cgroup-parent", cgroupParent),
		build.WithDockerfile(`
FROM busybox
RUN cat /proc/self/cgroup
`))
	result.Assert(c, icmd.Success)
	m, err := regexp.MatchString(fmt.Sprintf("memory:.*/%s/.*", cgroupParent), result.Combined())
	assert.NilError(c, err)
	if !m {
		c.Fatalf("There is no expected memory cgroup with parent /%s/: %s", cgroupParent, result.Combined())
	}
}

// FIXME(vdemeester) could be a unit test
func (s *DockerCLIBuildSuite) TestBuildNoDupOutput(c *testing.T) {
	// Check to make sure our build output prints the Dockerfile cmd
	// property - there was a bug that caused it to be duplicated on the
	// Step X  line
	const name = "testbuildnodupoutput"
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`
  FROM busybox
  RUN env`))
	result.Assert(c, icmd.Success)
	exp := "\nStep 2/2 : RUN env\n"
	if !strings.Contains(result.Combined(), exp) {
		c.Fatalf("Bad output\nGot:%s\n\nExpected to contain:%s\n", result.Combined(), exp)
	}
}

// GH15826
// FIXME(vdemeester) could be a unit test
func (s *DockerCLIBuildSuite) TestBuildStartsFromOne(c *testing.T) {
	// Explicit check to ensure that build starts from step 1 rather than 0
	const name = "testbuildstartsfromone"
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox`))
	result.Assert(c, icmd.Success)
	exp := "\nStep 1/1 : FROM busybox\n"
	if !strings.Contains(result.Combined(), exp) {
		c.Fatalf("Bad output\nGot:%s\n\nExpected to contain:%s\n", result.Combined(), exp)
	}
}

func (s *DockerCLIBuildSuite) TestBuildRUNErrMsg(c *testing.T) {
	// Test to make sure the bad command is quoted with just "s and
	// not as a Go []string
	const name = "testbuildbadrunerrmsg"
	shell := "/bin/sh -c"
	exitCode := 127
	if testEnv.DaemonInfo.OSType == "windows" {
		shell = "cmd /S /C"
		// architectural - Windows has to start the container to determine the exe is bad, Linux does not
		exitCode = 1
	}
	exp := fmt.Sprintf(`The command '%s badEXE a1 \& a2	a3' returned a non-zero code: %d`, shell, exitCode)

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`
  FROM busybox
  RUN badEXE a1 \& a2	a3`)).Assert(c, icmd.Expected{
		ExitCode: exitCode,
		Err:      exp,
	})
}

// Issue #15634: COPY fails when path starts with "null"
func (s *DockerCLIBuildSuite) TestBuildNullStringInAddCopyVolume(c *testing.T) {
	const name = "testbuildnullstringinaddcopyvolume"
	volName := "nullvolume"
	if testEnv.DaemonInfo.OSType == "windows" {
		volName = `C:\\nullvolume`
	}

	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `
		FROM busybox

		ADD null /
		COPY nullfile /
		VOLUME `+volName+`
		`),
		build.WithFile("null", "test1"),
		build.WithFile("nullfile", "test2"),
	))
}

func (s *DockerCLIBuildSuite) TestBuildStopSignal(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support STOPSIGNAL yet
	imgName := strings.ToLower(c.Name())
	cli.BuildCmd(c, imgName, build.WithDockerfile(`FROM busybox
		 STOPSIGNAL SIGKILL`))
	res := inspectFieldJSON(c, imgName, "Config.StopSignal")
	if res != `"SIGKILL"` {
		c.Fatalf("Signal %s, expected SIGKILL", res)
	}

	containerName := "test-container-stop-signal"
	cli.DockerCmd(c, "run", "-d", "--name", containerName, imgName, "top")
	res = inspectFieldJSON(c, containerName, "Config.StopSignal")
	if res != `"SIGKILL"` {
		c.Fatalf("Signal %s, expected SIGKILL", res)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArg(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	var dockerfile string
	if testEnv.DaemonInfo.OSType == "windows" {
		// Bugs in Windows busybox port - use the default base image and native cmd stuff
		dockerfile = fmt.Sprintf(`FROM `+minimalBaseImage()+`
			ARG %s
			RUN echo %%%s%%
			CMD setlocal enableextensions && if defined %s (echo %%%s%%)`, envKey, envKey, envKey, envKey)
	} else {
		dockerfile = fmt.Sprintf(`FROM busybox
			ARG %s
			RUN echo $%s
			CMD echo $%s`, envKey, envKey, envKey)
	}
	cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	).Assert(c, icmd.Expected{
		Out: envVal,
	})

	containerName := "bldargCont"
	out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined()
	out = strings.Trim(out, " \r\n'")
	if out != "" {
		c.Fatalf("run produced invalid output: %q, expected empty string", out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgHistory(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	envDef := "bar1"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s=%s`, envKey, envDef)
	cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	).Assert(c, icmd.Expected{
		Out: envVal,
	})

	out := cli.DockerCmd(c, "history", "--no-trunc", imgName).Combined()
	outputTabs := strings.Split(out, "\n")[1]
	if !strings.Contains(outputTabs, envDef) {
		c.Fatalf("failed to find arg default in image history output: %q expected: %q", outputTabs, envDef)
	}
}

func (s *DockerCLIBuildSuite) TestBuildTimeArgHistoryExclusions(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	proxy := "HTTP_PROXY=http://user:password@proxy.example.com"
	explicitProxyKey := "http_proxy"
	explicitProxyVal := "http://user:password@someproxy.example.com"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		ARG %s
		RUN echo "Testing Build Args!"`, envKey, explicitProxyKey)

	buildImage := func(imgName string) string {
		cli.BuildCmd(c, imgName,
			cli.WithFlags("--build-arg", "https_proxy=https://proxy.example.com",
				"--build-arg", fmt.Sprintf("%s=%s", envKey, envVal),
				"--build-arg", fmt.Sprintf("%s=%s", explicitProxyKey, explicitProxyVal),
				"--build-arg", proxy),
			build.WithDockerfile(dockerfile),
		)
		return getIDByName(c, imgName)
	}

	origID := buildImage(imgName)
	result := cli.DockerCmd(c, "history", "--no-trunc", imgName)
	out := result.Stdout()

	if strings.Contains(out, proxy) {
		c.Fatalf("failed to exclude proxy settings from history!")
	}
	if strings.Contains(out, "https_proxy") {
		c.Fatalf("failed to exclude proxy settings from history!")
	}
	result.Assert(c, icmd.Expected{Out: fmt.Sprintf("%s=%s", envKey, envVal)})
	result.Assert(c, icmd.Expected{Out: fmt.Sprintf("%s=%s", explicitProxyKey, explicitProxyVal)})

	cacheID := buildImage(imgName + "-two")
	assert.Equal(c, origID, cacheID)
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgCacheHit(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		RUN echo $%s`, envKey, envKey)
	cli.BuildCmd(c, imgName,
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	origImgID := getIDByName(c, imgName)

	imgNameCache := "bldargtestcachehit"
	cli.BuildCmd(c, imgNameCache,
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	newImgID := getIDByName(c, imgName)
	if newImgID != origImgID {
		c.Fatalf("build didn't use cache! expected image id: %q built image id: %q", origImgID, newImgID)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgCacheMissExtraArg(c *testing.T) {
	imgName := "bldargtest"
	envKey := "foo"
	envVal := "bar"
	extraEnvKey := "foo1"
	extraEnvVal := "bar1"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		ARG %s
		RUN echo $%s`, envKey, extraEnvKey, envKey)
	cli.BuildCmd(c, imgName,
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	origImgID := getIDByName(c, imgName)

	imgNameCache := "bldargtestcachemiss"
	cli.BuildCmd(c, imgNameCache,
		cli.WithFlags(
			"--build-arg", fmt.Sprintf("%s=%s", envKey, envVal),
			"--build-arg", fmt.Sprintf("%s=%s", extraEnvKey, extraEnvVal),
		),
		build.WithDockerfile(dockerfile),
	)
	newImgID := getIDByName(c, imgNameCache)

	if newImgID == origImgID {
		c.Fatalf("build used cache, expected a miss!")
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgCacheMissSameArgDiffVal(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	newEnvVal := "bar1"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		RUN echo $%s`, envKey, envKey)
	cli.BuildCmd(c, imgName,
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	origImgID := getIDByName(c, imgName)

	imgNameCache := "bldargtestcachemiss"
	cli.BuildCmd(c, imgNameCache,
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, newEnvVal)),
		build.WithDockerfile(dockerfile),
	)
	newImgID := getIDByName(c, imgNameCache)
	if newImgID == origImgID {
		c.Fatalf("build used cache, expected a miss!")
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgOverrideArgDefinedBeforeEnv(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	envValOverride := "barOverride"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		ENV %s %s
		RUN echo $%s
		CMD echo $%s
        `, envKey, envKey, envValOverride, envKey, envKey)

	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if strings.Count(result.Combined(), envValOverride) != 2 {
		c.Fatalf("failed to access environment variable in output: %q expected: %q", result.Combined(), envValOverride)
	}

	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); !strings.Contains(out, envValOverride) {
		c.Fatalf("run produced invalid output: %q, expected %q", out, envValOverride)
	}
}

// FIXME(vdemeester) might be useful to merge with the one above ?
func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgOverrideEnvDefinedBeforeArg(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	envValOverride := "barOverride"
	dockerfile := fmt.Sprintf(`FROM busybox
		ENV %s %s
		ARG %s
		RUN echo $%s
		CMD echo $%s
        `, envKey, envValOverride, envKey, envKey, envKey)
	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if strings.Count(result.Combined(), envValOverride) != 2 {
		c.Fatalf("failed to access environment variable in output: %q expected: %q", result.Combined(), envValOverride)
	}

	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); !strings.Contains(out, envValOverride) {
		c.Fatalf("run produced invalid output: %q, expected %q", out, envValOverride)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgExpansion(c *testing.T) {
	imgName := strings.ToLower(c.Name())

	wdVar := "WDIR"
	wdVal := "/tmp"
	addVar := "AFILE"
	addVal := "addFile"
	copyVar := "CFILE"
	copyVal := "copyFile"
	envVar := "foo"
	envVal := "bar"
	exposeVar := "EPORT"
	exposeVal := "9999"
	userVar := "USER"
	userVal := "testUser"
	volVar := "VOL"
	volVal := "/testVol/"
	if DaemonIsWindows() {
		volVal = `C:\testVol`
		wdVal = `C:\tmp`
	}

	cli.BuildCmd(c, imgName,
		cli.WithFlags(
			"--build-arg", fmt.Sprintf("%s=%s", wdVar, wdVal),
			"--build-arg", fmt.Sprintf("%s=%s", addVar, addVal),
			"--build-arg", fmt.Sprintf("%s=%s", copyVar, copyVal),
			"--build-arg", fmt.Sprintf("%s=%s", envVar, envVal),
			"--build-arg", fmt.Sprintf("%s=%s", exposeVar, exposeVal),
			"--build-arg", fmt.Sprintf("%s=%s", userVar, userVal),
			"--build-arg", fmt.Sprintf("%s=%s", volVar, volVal),
		),
		build.WithBuildContext(c,
			build.WithFile("Dockerfile", fmt.Sprintf(`FROM busybox
		ARG %s
		WORKDIR ${%s}
		ARG %s
		ADD ${%s} testDir/
		ARG %s
		COPY $%s testDir/
		ARG %s
		ENV %s=${%s}
		ARG %s
		EXPOSE $%s
		ARG %s
		USER $%s
		ARG %s
		VOLUME ${%s}`,
				wdVar, wdVar, addVar, addVar, copyVar, copyVar, envVar, envVar,
				envVar, exposeVar, exposeVar, userVar, userVar, volVar, volVar)),
			build.WithFile(addVal, "some stuff"),
			build.WithFile(copyVal, "some stuff"),
		),
	)

	res := inspectField(c, imgName, "Config.WorkingDir")
	assert.Equal(c, filepath.ToSlash(res), filepath.ToSlash(wdVal))

	var resArr []string
	inspectFieldAndUnmarshall(c, imgName, "Config.Env", &resArr)

	found := slices.Contains(resArr, fmt.Sprintf("%s=%s", envVar, envVal))
	if !found {
		c.Fatalf("Config.Env value mismatch. Expected <key=value> to exist: %s=%s, got: %v",
			envVar, envVal, resArr)
	}

	var resMap map[string]any
	inspectFieldAndUnmarshall(c, imgName, "Config.ExposedPorts", &resMap)
	if _, ok := resMap[exposeVal+"/tcp"]; !ok {
		c.Fatalf("Config.ExposedPorts value mismatch. Expected exposed port: %s/tcp, got: %v", exposeVal, resMap)
	}

	res = inspectField(c, imgName, "Config.User")
	if res != userVal {
		c.Fatalf("Config.User value mismatch. Expected: %s, got: %s", userVal, res)
	}

	inspectFieldAndUnmarshall(c, imgName, "Config.Volumes", &resMap)
	if _, ok := resMap[volVal]; !ok {
		c.Fatalf("Config.Volumes value mismatch. Expected volume: %s, got: %v", volVal, resMap)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgExpansionOverride(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	envKey1 := "foo1"
	envValOverride := "barOverride"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		ENV %s %s
		ENV %s ${%s}
		RUN echo $%s
		CMD echo $%s`, envKey, envKey, envValOverride, envKey1, envKey, envKey1, envKey1)
	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if strings.Count(result.Combined(), envValOverride) != 2 {
		c.Fatalf("failed to access environment variable in output: %q expected: %q", result.Combined(), envValOverride)
	}

	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); !strings.Contains(out, envValOverride) {
		c.Fatalf("run produced invalid output: %q, expected %q", out, envValOverride)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgUntrustedDefinedAfterUse(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	dockerfile := fmt.Sprintf(`FROM busybox
		RUN echo $%s
		ARG %s
		CMD echo $%s`, envKey, envKey, envKey)
	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if strings.Contains(result.Combined(), envVal) {
		c.Fatalf("able to access environment variable in output: %q expected to be missing", result.Combined())
	}

	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); out != "\n" {
		c.Fatalf("run produced invalid output: %q, expected empty string", out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgBuiltinArg(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support --build-arg
	imgName := strings.ToLower(c.Name())
	envKey := "HTTP_PROXY"
	envVal := "bar"
	dockerfile := fmt.Sprintf(`FROM busybox
		RUN echo $%s
		CMD echo $%s`, envKey, envKey)

	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if !strings.Contains(result.Combined(), envVal) {
		c.Fatalf("failed to access environment variable in output: %q expected: %q", result.Combined(), envVal)
	}
	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); out != "\n" {
		c.Fatalf("run produced invalid output: %q, expected empty string", out)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgDefaultOverride(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	envValOverride := "barOverride"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s=%s
		ENV %s $%s
		RUN echo $%s
		CMD echo $%s`, envKey, envVal, envKey, envKey, envKey, envKey)
	result := cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envValOverride)),
		build.WithDockerfile(dockerfile),
	)
	result.Assert(c, icmd.Success)
	if strings.Count(result.Combined(), envValOverride) != 1 {
		c.Fatalf("failed to access environment variable in output: %q expected: %q", result.Combined(), envValOverride)
	}

	containerName := "bldargCont"
	if out := cli.DockerCmd(c, "run", "--name", containerName, imgName).Combined(); !strings.Contains(out, envValOverride) {
		c.Fatalf("run produced invalid output: %q, expected %q", out, envValOverride)
	}
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgUnconsumedArg(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envVal := "bar"
	dockerfile := fmt.Sprintf(`FROM busybox
		RUN echo $%s
		CMD echo $%s`, envKey, envKey)
	warnStr := "[Warning] One or more build-args"
	cli.Docker(cli.Args("build", "-t", imgName),
		cli.WithFlags("--build-arg", fmt.Sprintf("%s=%s", envKey, envVal)),
		build.WithDockerfile(dockerfile),
	).Assert(c, icmd.Expected{
		Out: warnStr,
	})
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgEnv(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	dockerfile := `FROM busybox
		ARG FOO1=fromfile
		ARG FOO2=fromfile
		ARG FOO3=fromfile
		ARG FOO4=fromfile
		ARG FOO5
		ARG FOO6
		ARG FO10
		RUN env
		RUN [ "$FOO1" = "fromcmd" ]
		RUN [ "$FOO2" = "" ]
		RUN [ "$FOO3" = "fromenv" ]
		RUN [ "$FOO4" = "fromfile" ]
		RUN [ "$FOO5" = "fromcmd" ]
		# The following should not exist at all in the env
		RUN [ "$(env | grep FOO6)" = "" ]
		RUN [ "$(env | grep FOO7)" = "" ]
		RUN [ "$(env | grep FOO8)" = "" ]
		RUN [ "$(env | grep FOO9)" = "" ]
		RUN [ "$FO10" = "" ]
	    `
	result := cli.Docker(cli.Args("build", "-t", "testbuildtimeargenv"),
		cli.WithFlags(
			"--build-arg", "FOO1=fromcmd",
			"--build-arg", "FOO2=",
			"--build-arg", "FOO3", // set in env
			"--build-arg", "FOO4", // not set in env
			"--build-arg", "FOO5=fromcmd",
			// FOO6 is not set at all
			"--build-arg", "FOO7=fromcmd", // should produce a warning
			"--build-arg", "FOO8=", // should produce a warning
			"--build-arg", "FOO9", // should produce a warning
			"--build-arg", "FO10", // not set in env, empty value
		),
		cli.WithEnvironmentVariables(append(os.Environ(),
			"FOO1=fromenv",
			"FOO2=fromenv",
			"FOO3=fromenv")...),
		build.WithBuildContext(c,
			build.WithFile("Dockerfile", dockerfile),
		),
	)
	result.Assert(c, icmd.Success)

	// Now check to make sure we got a warning msg about unused build-args
	i := strings.Index(result.Combined(), "[Warning]")
	if i < 0 {
		c.Fatalf("Missing the build-arg warning in %q", result.Combined())
	}

	out := result.Combined()[i:] // "out" should contain just the warning message now

	// These were specified on a --build-arg but no ARG was in the Dockerfile
	assert.Assert(c, is.Contains(out, "FOO7"))
	assert.Assert(c, is.Contains(out, "FOO8"))
	assert.Assert(c, is.Contains(out, "FOO9"))
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgQuotedValVariants(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envKey1 := "foo1"
	envKey2 := "foo2"
	envKey3 := "foo3"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s=""
		ARG %s=''
		ARG %s="''"
		ARG %s='""'
		RUN [ "$%s" != "$%s" ]
		RUN [ "$%s" != "$%s" ]
		RUN [ "$%s" != "$%s" ]
		RUN [ "$%s" != "$%s" ]
		RUN [ "$%s" != "$%s" ]`, envKey, envKey1, envKey2, envKey3,
		envKey, envKey2, envKey, envKey3, envKey1, envKey2, envKey1, envKey3,
		envKey2, envKey3)
	cli.BuildCmd(c, imgName, build.WithDockerfile(dockerfile))
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgEmptyValVariants(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support ARG
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	envKey1 := "foo1"
	envKey2 := "foo2"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s=
		ARG %s=""
		ARG %s=''
		RUN [ "$%s" = "$%s" ]
		RUN [ "$%s" = "$%s" ]
		RUN [ "$%s" = "$%s" ]`, envKey, envKey1, envKey2, envKey, envKey1, envKey1, envKey2, envKey, envKey2)
	cli.BuildCmd(c, imgName, build.WithDockerfile(dockerfile))
}

func (s *DockerCLIBuildSuite) TestBuildBuildTimeArgDefinitionWithNoEnvInjection(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	envKey := "foo"
	dockerfile := fmt.Sprintf(`FROM busybox
		ARG %s
		RUN env`, envKey)

	result := cli.BuildCmd(c, imgName, build.WithDockerfile(dockerfile))
	result.Assert(c, icmd.Success)
	if strings.Count(result.Combined(), envKey) != 1 {
		c.Fatalf("unexpected number of occurrences of the arg in output: %q expected: 1", result.Combined())
	}
}

// TestBuildMultiStageArg verifies that build-args are scoped to the FROM
// they're defined in. Test for https://github.com/moby/moby/issues/31892
func (s *DockerCLIBuildSuite) TestBuildMultiStageArg(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	const dockerfile = `FROM busybox
ARG stage_1_arg=AAAA
LABEL multifromtest=1
RUN env > /out
FROM busybox
ARG stage_2_arg=BBBB
RUN env > /out
`

	cli.BuildCmd(c, imgName, build.WithDockerfile(dockerfile))

	result := cli.DockerCmd(c, "run", "--rm", imgName, "cat", "/out")
	assert.Check(c, !strings.Contains(result.Stdout(), "stage_1_arg"), "build arg leaked to second stage")
	assert.Assert(c, is.Contains(result.Stdout(), "stage_2_arg=BBBB"), "build arg not applied to second stage")
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageGlobalArg(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	const dockerfile = `ARG tag=nosuchtag
FROM busybox:${tag}
LABEL multifromtest2=1
RUN env > /out
FROM busybox:${tag}
ARG tag
RUN env > /out`

	cli.BuildCmd(c, imgName, build.WithDockerfile(dockerfile), cli.WithFlags("--build-arg", "tag=latest"))

	result := cli.DockerCmd(c, "run", "--rm", imgName, "cat", "/out")
	assert.Assert(c, is.Contains(result.Stdout(), "tag=latest"))
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageUnusedArg(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	dockerfile := `FROM busybox
    ARG foo
    FROM busybox
    ARG bar
    RUN env > /out`

	result := cli.BuildCmd(c, imgName,
		build.WithDockerfile(dockerfile),
		cli.WithFlags("--build-arg", "baz=abc"))
	result.Assert(c, icmd.Success)
	assert.Assert(c, is.Contains(result.Combined(), "[Warning]"))
	assert.Assert(c, is.Contains(result.Combined(), "[baz] were not consumed"))
	result = cli.DockerCmd(c, "run", "--rm", imgName, "cat", "/out")
	assert.Assert(c, !strings.Contains(result.Stdout(), "bar"))
	assert.Assert(c, !strings.Contains(result.Stdout(), "baz"))
}

func (s *DockerCLIBuildSuite) TestBuildNoNamedVolume(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	volName := "testname:/foo"
	expError := `ls: /foo/oops: No such file or directory`

	if testEnv.DaemonInfo.OSType == "windows" {
		volName = "testname:C:\\foo"
		expError = `invalid volume specification`
	}
	cli.DockerCmd(c, "run", "-v", volName, "busybox", "sh", "-c", "touch /foo/oops")

	// Named volumes are not supported in VOLUME, so the `<volume-name>:<path>`
	// should be used as-is and to be considered a path inside the image.
	dockerFile := `FROM busybox
	VOLUME ` + volName + `
	RUN ls /foo/oops
	`

	res := cli.Docker(cli.Args("build", "-t", imgName), build.WithDockerfile(dockerFile)).Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	assert.Check(c, is.Contains(res.Combined(), expError))
}

func (s *DockerCLIBuildSuite) TestBuildTagEvent(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	since := daemonUnixTime(c)

	dockerFile := `FROM busybox
	RUN echo events
	`
	cli.BuildCmd(c, imgName, build.WithDockerfile(dockerFile))

	until := daemonUnixTime(c)
	out := cli.DockerCmd(c, "events", "--since", since, "--until", until, "--filter", "type=image").Stdout()
	events := strings.Split(strings.TrimSpace(out), "\n")
	actions := eventActionsByIDAndType(c, events, imgName+":latest", "image")
	var foundTag bool
	if slices.Contains(actions, "tag") {
		foundTag = true
	}

	assert.Assert(c, foundTag, "No tag event found:\n%s", out)
}

// #15780
func (s *DockerCLIBuildSuite) TestBuildMultipleTags(c *testing.T) {
	dockerfile := `
	FROM busybox
	MAINTAINER test-15780
	`
	cli.BuildCmd(c, "tag1", cli.WithFlags("-t", "tag2:v2", "-t", "tag1:latest", "-t", "tag1"), build.WithDockerfile(dockerfile))

	id1 := getIDByName(c, "tag1")
	id2 := getIDByName(c, "tag2:v2")
	assert.Equal(c, id1, id2)
}

// #17290
func (s *DockerCLIBuildSuite) TestBuildCacheBrokenSymlink(c *testing.T) {
	const name = "testbuildbrokensymlink"
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
	FROM busybox
	COPY . ./`),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}))
	defer ctx.Close()

	err := os.Symlink(filepath.Join(ctx.Dir, "nosuchfile"), filepath.Join(ctx.Dir, "asymlink"))
	assert.NilError(c, err)

	// warm up cache
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	// add new file to context, should invalidate cache
	err = os.WriteFile(filepath.Join(ctx.Dir, "newfile"), []byte("foo"), 0o644)
	assert.NilError(c, err)

	result := cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	if strings.Contains(result.Combined(), "Using cache") {
		c.Fatal("2nd build used cache on ADD, it shouldn't")
	}
}

func (s *DockerCLIBuildSuite) TestBuildFollowSymlinkToFile(c *testing.T) {
	const name = "testbuildbrokensymlink"
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
	FROM busybox
	COPY asymlink target`),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}))
	defer ctx.Close()

	err := os.Symlink("foo", filepath.Join(ctx.Dir, "asymlink"))
	assert.NilError(c, err)

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "--rm", name, "cat", "target").Combined()
	assert.Assert(c, is.Regexp("^bar$", out))

	// change target file should invalidate cache
	err = os.WriteFile(filepath.Join(ctx.Dir, "foo"), []byte("baz"), 0o644)
	assert.NilError(c, err)

	result := cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	assert.Assert(c, !strings.Contains(result.Combined(), "Using cache"))
	out = cli.DockerCmd(c, "run", "--rm", name, "cat", "target").Combined()
	assert.Assert(c, is.Regexp("^baz$", out))
}

func (s *DockerCLIBuildSuite) TestBuildFollowSymlinkToDir(c *testing.T) {
	const name = "testbuildbrokensymlink"
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
	FROM busybox
	COPY asymlink /`),
		fakecontext.WithFiles(map[string]string{
			"foo/abc": "bar",
			"foo/def": "baz",
		}))
	defer ctx.Close()

	err := os.Symlink("foo", filepath.Join(ctx.Dir, "asymlink"))
	assert.NilError(c, err)

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "--rm", name, "cat", "abc", "def").Combined()
	assert.Assert(c, is.Regexp("^barbaz$", out))

	// change target file should invalidate cache
	err = os.WriteFile(filepath.Join(ctx.Dir, "foo/def"), []byte("bax"), 0o644)
	assert.NilError(c, err)

	result := cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))
	assert.Assert(c, !strings.Contains(result.Combined(), "Using cache"))
	out = cli.DockerCmd(c, "run", "--rm", name, "cat", "abc", "def").Combined()
	assert.Assert(c, is.Regexp("^barbax$", out))
}

// TestBuildSymlinkBasename tests that target file gets basename from symlink,
// not from the target file.
func (s *DockerCLIBuildSuite) TestBuildSymlinkBasename(c *testing.T) {
	const name = "testbuildbrokensymlink"
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
	FROM busybox
	COPY asymlink /`),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}))
	defer ctx.Close()

	err := os.Symlink("foo", filepath.Join(ctx.Dir, "asymlink"))
	assert.NilError(c, err)

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "--rm", name, "cat", "asymlink").Combined()
	assert.Assert(c, is.Regexp("^bar$", out))
}

// #17827
func (s *DockerCLIBuildSuite) TestBuildCacheRootSource(c *testing.T) {
	const name = "testbuildrootsource"
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(`
	FROM busybox
	COPY / /data`),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}))
	defer ctx.Close()

	// warm up cache
	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	// change file, should invalidate cache
	err := os.WriteFile(filepath.Join(ctx.Dir, "foo"), []byte("baz"), 0o644)
	assert.NilError(c, err)

	result := cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	assert.Assert(c, !strings.Contains(result.Combined(), "Using cache"))
}

// #19375
// FIXME(vdemeester) should migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildFailsGitNotCallable(c *testing.T) {
	cli.Docker(cli.Args("build", "-t", "gitnotcallable"), cli.WithEnvironmentVariables("PATH="),
		build.WithContextPath("github.com/docker/v1.10-migrator.git")).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "unable to prepare context: unable to find 'git': ",
	})

	cli.Docker(cli.Args("build", "-t", "gitnotcallable"), cli.WithEnvironmentVariables("PATH="),
		build.WithContextPath("https://github.com/docker/v1.10-migrator.git")).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "unable to prepare context: unable to find 'git': ",
	})
}

// TestBuildWorkdirWindowsPath tests that a Windows style path works as a workdir
func (s *DockerCLIBuildSuite) TestBuildWorkdirWindowsPath(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name = "testbuildworkdirwindowspath"
	cli.BuildCmd(c, name, build.WithDockerfile(`
	FROM `+testEnv.PlatformDefaults.BaseImage+`
	RUN mkdir C:\\work
	WORKDIR C:\\work
	RUN if "%CD%" NEQ "C:\work" exit -1
	`))
}

func (s *DockerCLIBuildSuite) TestBuildLabel(c *testing.T) {
	const name = "testbuildlabel"
	testLabel := "foo"

	cli.BuildCmd(c, name, cli.WithFlags("--label", testLabel),
		build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  LABEL default foo
`))

	var labels map[string]string
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &labels)
	if _, ok := labels[testLabel]; !ok {
		c.Fatal("label not found in image")
	}
}

func (s *DockerCLIBuildSuite) TestBuildLabelOneNode(c *testing.T) {
	const name = "testbuildlabel"
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo=bar"),
		build.WithDockerfile("FROM busybox"))

	var labels map[string]string
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &labels)
	v, ok := labels["foo"]
	if !ok {
		c.Fatal("label `foo` not found in image")
	}
	assert.Equal(c, v, "bar")
}

func (s *DockerCLIBuildSuite) TestBuildLabelCacheCommit(c *testing.T) {
	const name = "testbuildlabelcachecommit"
	testLabel := "foo"

	cli.BuildCmd(c, name, build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  LABEL default foo
  `))
	cli.BuildCmd(c, name, cli.WithFlags("--label", testLabel),
		build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  LABEL default foo
  `))

	var labels map[string]string
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &labels)
	if _, ok := labels[testLabel]; !ok {
		c.Fatal("label not found in image")
	}
}

func (s *DockerCLIBuildSuite) TestBuildLabelMultiple(c *testing.T) {
	const name = "testbuildlabelmultiple"
	testLabels := map[string]string{
		"foo": "bar",
		"123": "456",
	}
	var labelArgs []string
	for k, v := range testLabels {
		labelArgs = append(labelArgs, "--label", k+"="+v)
	}

	cli.BuildCmd(c, name, cli.WithFlags(labelArgs...),
		build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  LABEL default foo
`))

	var labels map[string]string
	inspectFieldAndUnmarshall(c, name, "Config.Labels", &labels)
	for k, v := range testLabels {
		if x, ok := labels[k]; !ok || x != v {
			c.Fatalf("label %s=%s not found in image", k, v)
		}
	}
}

func (s *DockerRegistryAuthHtpasswdSuite) TestBuildFromAuthenticatedRegistry(c *testing.T) {
	cli.DockerCmd(c, "login", "-u", s.reg.Username(), "-p", s.reg.Password(), privateRegistryURL)
	baseImage := privateRegistryURL + "/baseimage"

	cli.BuildCmd(c, baseImage, build.WithDockerfile(`
	FROM busybox
	ENV env1 val1
	`))

	cli.DockerCmd(c, "push", baseImage)
	cli.DockerCmd(c, "rmi", baseImage)

	cli.BuildCmd(c, baseImage, build.WithDockerfile(fmt.Sprintf(`
	FROM %s
	ENV env2 val2
	`, baseImage)))
}

func (s *DockerRegistryAuthHtpasswdSuite) TestBuildWithExternalAuth(c *testing.T) {
	workingDir, err := os.Getwd()
	assert.NilError(c, err)
	absolute, err := filepath.Abs(filepath.Join(workingDir, "fixtures", "auth"))
	assert.NilError(c, err)

	osPath := os.Getenv("PATH")
	testPath := fmt.Sprintf("%s%c%s", osPath, filepath.ListSeparator, absolute)
	c.Setenv("PATH", testPath)

	imgName := fmt.Sprintf("%v/dockercli/busybox:authtest", privateRegistryURL)

	tmp, err := os.MkdirTemp("", "integration-cli-")
	assert.NilError(c, err)

	externalAuthConfig := `{ "credsStore": "shell-test" }`

	configPath := filepath.Join(tmp, "config.json")
	err = os.WriteFile(configPath, []byte(externalAuthConfig), 0o644)
	assert.NilError(c, err)

	cli.DockerCmd(c, "--config", tmp, "login", "-u", s.reg.Username(), "-p", s.reg.Password(), privateRegistryURL)

	b, err := os.ReadFile(configPath)
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(string(b), "\"auth\":"))
	cli.DockerCmd(c, "--config", tmp, "tag", "busybox", imgName)
	cli.DockerCmd(c, "--config", tmp, "push", imgName)

	// make sure the image is pulled when building
	cli.DockerCmd(c, "rmi", imgName)

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "--config", tmp, "build", "-"},
		Stdin:   strings.NewReader("FROM " + imgName),
	}).Assert(c, icmd.Success)
}

// Test cases in #22036
func (s *DockerCLIBuildSuite) TestBuildLabelsOverride(c *testing.T) {
	// Command line option labels will always override
	name := "scratchy"
	expected := `{"bar":"from-flag","foo":"from-flag"}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo=from-flag", "--label", "bar=from-flag"),
		build.WithDockerfile(`FROM `+minimalBaseImage()+`
                LABEL foo=from-dockerfile`))
	res := inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	name = "from"
	expected = `{"foo":"from-dockerfile"}`
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
                LABEL foo from-dockerfile`))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	// Command line option label will override even via `FROM`
	name = "new"
	expected = `{"bar":"from-dockerfile2","foo":"new"}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo=new"),
		build.WithDockerfile(`FROM from
                LABEL bar from-dockerfile2`))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	// Command line option without a value set (--label foo, --label bar=)
	// will be treated as --label foo="", --label bar=""
	name = "scratchy2"
	expected = `{"bar":"","foo":""}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo", "--label", "bar="),
		build.WithDockerfile(`FROM `+minimalBaseImage()+`
                LABEL foo=from-dockerfile`))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	// Command line option without a value set (--label foo, --label bar=)
	// will be treated as --label foo="", --label bar=""
	// This time is for inherited images
	name = "new2"
	expected = `{"bar":"","foo":""}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo=", "--label", "bar"),
		build.WithDockerfile(`FROM from
                LABEL bar from-dockerfile2`))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	// Command line option labels with only `FROM`
	name = "scratchy"
	expected = `{"bar":"from-flag","foo":"from-flag"}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "foo=from-flag", "--label", "bar=from-flag"),
		build.WithDockerfile(`FROM `+minimalBaseImage()))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}

	// Command line option labels with env var
	name = "scratchz"
	expected = `{"bar":"$PATH"}`
	cli.BuildCmd(c, name, cli.WithFlags("--label", "bar=$PATH"),
		build.WithDockerfile(`FROM `+minimalBaseImage()))
	res = inspectFieldJSON(c, name, "Config.Labels")
	if res != expected {
		c.Fatalf("Labels %s, expected %s", res, expected)
	}
}

// Test case for #22855
func (s *DockerCLIBuildSuite) TestBuildDeleteCommittedFile(c *testing.T) {
	const name = "test-delete-committed-file"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN echo test > file
		RUN test -e file
		RUN rm file
		RUN sh -c "! test -e file"`))
}

// #20083
func (s *DockerCLIBuildSuite) TestBuildDockerignoreComment(c *testing.T) {
	// TODO Windows: Figure out why this test is flakey on TP5. If you add
	// something like RUN sleep 5, or even RUN ls /tmp after the ADD line,
	// it is more reliable, but that's not a good fix.
	testRequires(c, DaemonIsLinux)

	const name = "testbuilddockerignorecleanpaths"
	dockerfile := `
        FROM busybox
        ADD . /tmp/
        RUN sh -c "(ls -la /tmp/#1)"
        RUN sh -c "(! ls -la /tmp/#2)"
        RUN sh -c "(! ls /tmp/foo) && (! ls /tmp/foo2) && (ls /tmp/dir1/foo)"`
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("foo", "foo"),
		build.WithFile("foo2", "foo2"),
		build.WithFile("dir1/foo", "foo in dir1"),
		build.WithFile("#1", "# file 1"),
		build.WithFile("#2", "# file 2"),
		build.WithFile(".dockerignore", `# Visual C++ cache files
# because we have git ;-)
# The above comment is from #20083
foo
#dir1/foo
foo2
# The following is considered as comment as # is at the beginning
#1
# The following is not considered as comment as # is not at the beginning
  #2
`)))
}

// Test case for #23221
func (s *DockerCLIBuildSuite) TestBuildWithUTF8BOM(c *testing.T) {
	const name = "test-with-utf8-bom"
	dockerfile := []byte(`FROM busybox`)
	bomDockerfile := append([]byte{0xEF, 0xBB, 0xBF}, dockerfile...)
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", string(bomDockerfile)),
	))
}

// Test case for UTF-8 BOM in .dockerignore, related to #23221
func (s *DockerCLIBuildSuite) TestBuildWithUTF8BOMDockerignore(c *testing.T) {
	const name = "test-with-utf8-bom-dockerignore"
	dockerfile := `
        FROM busybox
		ADD . /tmp/
		RUN ls -la /tmp
		RUN sh -c "! ls /tmp/Dockerfile"
		RUN ls /tmp/.dockerignore`
	dockerignore := []byte("./Dockerfile\n")
	bomDockerignore := append([]byte{0xEF, 0xBB, 0xBF}, dockerignore...)
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile(".dockerignore", string(bomDockerignore)),
	))
}

// #22489 Shell test to confirm config gets updated correctly
func (s *DockerCLIBuildSuite) TestBuildShellUpdatesConfig(c *testing.T) {
	skip.If(c, versions.GreaterThan(testEnv.DaemonAPIVersion(), "1.44"), "ContainerConfig is deprecated")
	skip.If(c, testEnv.UsingSnapshotter, "ContainerConfig is not filled in c8d")

	const name = "testbuildshellupdatesconfig"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM `+minimalBaseImage()+`
        SHELL ["foo", "-bar"]`))
	expected := `["foo","-bar","#(nop) ","SHELL [foo -bar]"]`
	res := inspectFieldJSON(c, name, "ContainerConfig.Cmd")
	if res != expected {
		c.Fatalf("%s, expected %s", res, expected)
	}
	res = inspectFieldJSON(c, name, "ContainerConfig.Shell")
	if res != `["foo","-bar"]` {
		c.Fatalf(`%s, expected ["foo","-bar"]`, res)
	}
}

// #22489 Changing the shell multiple times and CMD after.
func (s *DockerCLIBuildSuite) TestBuildShellMultiple(c *testing.T) {
	const name = "testbuildshellmultiple"

	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
		RUN echo defaultshell
		SHELL ["echo"]
		RUN echoshell
		SHELL ["ls"]
		RUN -l
		CMD -l`))
	result.Assert(c, icmd.Success)

	// Must contain 'defaultshell' twice
	if len(strings.Split(result.Combined(), "defaultshell")) != 3 {
		c.Fatalf("defaultshell should have appeared twice in %s", result.Combined())
	}

	// Must contain 'echoshell' twice
	if len(strings.Split(result.Combined(), "echoshell")) != 3 {
		c.Fatalf("echoshell should have appeared twice in %s", result.Combined())
	}

	// Must contain "total " (part of ls -l)
	if !strings.Contains(result.Combined(), "total ") {
		c.Fatalf("%s should have contained 'total '", result.Combined())
	}

	// A container started from the image uses the shell-form CMD.
	// Last shell is ls. CMD is -l. So should contain 'total '.
	outrun := cli.DockerCmd(c, "run", "--rm", name).Combined()
	if !strings.Contains(outrun, "total ") {
		c.Fatalf("Expected started container to run ls -l. %s", outrun)
	}
}

// #22489. Changed SHELL with ENTRYPOINT
func (s *DockerCLIBuildSuite) TestBuildShellEntrypoint(c *testing.T) {
	const name = "testbuildshellentrypoint"

	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		SHELL ["ls"]
		ENTRYPOINT -l`))
	// A container started from the image uses the shell-form ENTRYPOINT.
	// Shell is ls. ENTRYPOINT is -l. So should contain 'total '.
	outrun := cli.DockerCmd(c, "run", "--rm", name).Combined()
	if !strings.Contains(outrun, "total ") {
		c.Fatalf("Expected started container to run ls -l. %s", outrun)
	}
}

// #22489 Shell test to confirm shell is inherited in a subsequent build
func (s *DockerCLIBuildSuite) TestBuildShellInherited(c *testing.T) {
	const name1 = "testbuildshellinherited1"
	cli.BuildCmd(c, name1, build.WithDockerfile(`FROM busybox
        SHELL ["ls"]`))
	const name2 = "testbuildshellinherited2"
	cli.Docker(cli.Args("build", "-t", name2), build.WithDockerfile(`FROM `+name1+`
        RUN -l`)).Assert(c, icmd.Expected{
		// ls -l has "total " followed by some number in it, ls without -l does not.
		Out: "total ",
	})
}

// #22489 Shell test to confirm non-JSON doesn't work
func (s *DockerCLIBuildSuite) TestBuildShellNotJSON(c *testing.T) {
	const name = "testbuildshellnotjson"

	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM `+minimalBaseImage()+`
        sHeLl exec -form`, // Casing explicit to ensure error is upper-cased.
	)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "SHELL requires the arguments to be in JSON form",
	})
}

// #22489 Windows shell test to confirm native is powershell if executing a PS command
// This would error if the default shell were still cmd.
func (s *DockerCLIBuildSuite) TestBuildShellWindowsPowershell(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name = "testbuildshellpowershell"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM `+minimalBaseImage()+`
        SHELL ["powershell", "-command"]
		RUN Write-Host John`)).Assert(c, icmd.Expected{
		Out: "\nJohn\n",
	})
}

// Verify that escape is being correctly applied to words when escape directive is not \.
// Tests WORKDIR, ADD
func (s *DockerCLIBuildSuite) TestBuildEscapeNotBackslashWordTest(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name1 = "testbuildescapenotbackslashwordtesta"
	cli.Docker(cli.Args("build", "-t", name1), build.WithDockerfile(`# escape= `+"`"+`
		FROM `+minimalBaseImage()+`
        WORKDIR c:\windows
		RUN dir /w`)).Assert(c, icmd.Expected{
		Out: "[System32]",
	})

	const name2 = "testbuildescapenotbackslashwordtestb"
	cli.Docker(cli.Args("build", "-t", name2), build.WithDockerfile(`# escape= `+"`"+`
		FROM `+minimalBaseImage()+`
		SHELL ["powershell.exe"]
        WORKDIR c:\foo
		ADD Dockerfile c:\foo\
		RUN dir Dockerfile`)).Assert(c, icmd.Expected{
		Out: "-a----",
	})
}

// #22868. Make sure shell-form CMD is not marked as escaped in the config of the image,
// but an exec-form CMD is marked.
func (s *DockerCLIBuildSuite) TestBuildCmdShellArgsEscaped(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name1 = "testbuildcmdshellescapedshellform"
	cli.BuildCmd(c, name1, build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  CMD "ipconfig"
  `))
	res := inspectFieldJSON(c, name1, "Config.ArgsEscaped")
	if res != "true" {
		c.Fatalf("CMD did not update Config.ArgsEscaped on image: %v", res)
	}
	cli.DockerCmd(c, "run", "--name", "inspectme1", name1)
	cli.DockerCmd(c, "wait", "inspectme1")
	res = inspectFieldJSON(c, name1, "Config.Cmd")

	if res != `["cmd /S /C \"ipconfig\""]` {
		c.Fatalf("CMD incorrect in Config.Cmd: got %v", res)
	}

	// Now in JSON/exec-form
	const name2 = "testbuildcmdshellescapedexecform"
	cli.BuildCmd(c, name2, build.WithDockerfile(`
  FROM `+minimalBaseImage()+`
  CMD ["ipconfig"]
  `))
	res = inspectFieldJSON(c, name2, "Config.ArgsEscaped")
	if res != "false" {
		c.Fatalf("CMD set Config.ArgsEscaped on image: %v", res)
	}
	cli.DockerCmd(c, "run", "--name", "inspectme2", name2)
	cli.DockerCmd(c, "wait", "inspectme2")
	res = inspectFieldJSON(c, name2, "Config.Cmd")

	if res != `["ipconfig"]` {
		c.Fatalf("CMD incorrect in Config.Cmd: got %v", res)
	}
}

// Test case for #24912.
func (s *DockerCLIBuildSuite) TestBuildStepsWithProgress(c *testing.T) {
	const name = "testbuildstepswithprogress"
	totalRun := 5
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile("FROM busybox\n"+strings.Repeat("RUN echo foo\n", totalRun)))
	result.Assert(c, icmd.Success)
	assert.Assert(c, is.Contains(result.Combined(), fmt.Sprintf("Step 1/%d : FROM busybox", 1+totalRun)))
	for i := 2; i <= 1+totalRun; i++ {
		assert.Assert(c, is.Contains(result.Combined(), fmt.Sprintf("Step %d/%d : RUN echo foo", i, 1+totalRun)))
	}
}

func (s *DockerCLIBuildSuite) TestBuildWithFailure(c *testing.T) {
	const name = "testbuildwithfailure"

	// First test case can only detect `nobody` in runtime so all steps will show up
	dockerfile := "FROM busybox\nRUN nobody"
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(dockerfile))
	assert.Assert(c, result.Error != nil)
	assert.Assert(c, is.Contains(result.Stdout(), "Step 1/2 : FROM busybox"))
	assert.Assert(c, is.Contains(result.Stdout(), "Step 2/2 : RUN nobody"))
	// Second test case `FFOM` should have been detected before build runs so no steps
	dockerfile = "FFOM nobody\nRUN nobody"
	result = cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(dockerfile))
	assert.Assert(c, result.Error != nil)
	assert.Assert(c, !strings.Contains(result.Stdout(), "Step 1/2 : FROM busybox"))
	assert.Assert(c, !strings.Contains(result.Stdout(), "Step 2/2 : RUN nobody"))
}

func (s *DockerCLIBuildSuite) TestBuildCacheFromEqualDiffIDsLength(c *testing.T) {
	dockerfile := `
		FROM busybox
		RUN echo "test"
		ENTRYPOINT ["sh"]`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"Dockerfile": dockerfile,
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, "build1")

	// rebuild with cache-from
	result := cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, "build2")
	assert.Equal(c, id1, id2)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 2)
}

func (s *DockerCLIBuildSuite) TestBuildCacheFrom(c *testing.T) {
	testRequires(c, DaemonIsLinux) // All tests that do save are skipped in windows
	dockerfile := `
		FROM busybox
		ENV FOO=bar
		ADD baz /
		RUN touch bax`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"Dockerfile": dockerfile,
			"baz":        "baz",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, "build1")

	// rebuild with cache-from
	result := cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, "build2")
	assert.Equal(c, id1, id2)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 3)
	cli.DockerCmd(c, "rmi", "build2")

	// no cache match with unknown source
	result = cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=nosuchtag"), build.WithExternalBuildContext(ctx))
	id2 = getIDByName(c, "build2")
	assert.Assert(c, id1 != id2)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 0)
	cli.DockerCmd(c, "rmi", "build2")

	// Modify file, everything up to last command and layers are reused
	dockerfile = `
		FROM busybox
		ENV FOO=bar
		ADD baz /
		RUN touch newfile`
	err := os.WriteFile(filepath.Join(ctx.Dir, "Dockerfile"), []byte(dockerfile), 0o644)
	assert.NilError(c, err)

	result = cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	id2 = getIDByName(c, "build2")
	assert.Assert(c, id1 != id2)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 2)

	layers1Str := cli.DockerCmd(c, "inspect", "-f", "{{json .RootFS.Layers}}", "build1").Combined()
	layers2Str := cli.DockerCmd(c, "inspect", "-f", "{{json .RootFS.Layers}}", "build2").Combined()

	var layers1 []string
	var layers2 []string
	assert.NilError(c, json.Unmarshal([]byte(layers1Str), &layers1))
	assert.NilError(c, json.Unmarshal([]byte(layers2Str), &layers2))

	assert.Equal(c, len(layers1), len(layers2))
	for i := 0; i < len(layers1)-1; i++ {
		assert.Equal(c, layers1[i], layers2[i])
	}
	assert.Assert(c, layers1[len(layers1)-1] != layers2[len(layers1)-1])
}

func (s *DockerCLIBuildSuite) TestBuildCacheFromLoad(c *testing.T) {
	skip.If(c, testEnv.UsingSnapshotter, "Parent-child relations are lost when save/load-ing with the containerd image store")
	testRequires(c, DaemonIsLinux) // All tests that do save are skipped in windows
	dockerfile := `
		FROM busybox
		ENV FOO=bar
		ADD baz /
		RUN touch bax`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"Dockerfile": dockerfile,
			"baz":        "baz",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))
	id1 := getIDByName(c, "build1")

	// clear parent images
	tempDir, err := os.MkdirTemp("", "test-build-cache-from-")
	if err != nil {
		c.Fatalf("failed to create temporary directory: %s", tempDir)
	}
	defer os.RemoveAll(tempDir)
	tempFile := filepath.Join(tempDir, "img.tar")
	cli.DockerCmd(c, "save", "-o", tempFile, "build1")
	cli.DockerCmd(c, "rmi", "build1")
	cli.DockerCmd(c, "load", "-i", tempFile)

	// cache still applies without parents
	result := cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	id2 := getIDByName(c, "build2")
	assert.Equal(c, id1, id2)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 3)
	history1 := cli.DockerCmd(c, "history", "-q", "build2").Combined()
	// Retry, no new intermediate images
	result = cli.BuildCmd(c, "build3", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	id3 := getIDByName(c, "build3")
	assert.Equal(c, id1, id3)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 3)
	history2 := cli.DockerCmd(c, "history", "-q", "build3").Combined()

	assert.Equal(c, history1, history2)
	cli.DockerCmd(c, "rmi", "build2")
	cli.DockerCmd(c, "rmi", "build3")
	cli.DockerCmd(c, "rmi", "build1")
	cli.DockerCmd(c, "load", "-i", tempFile)
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageCache(c *testing.T) {
	testRequires(c, DaemonIsLinux) // All tests that do save are skipped in windows
	dockerfile := `
		FROM busybox
		ADD baz /
		FROM busybox
    ADD baz /`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"Dockerfile": dockerfile,
			"baz":        "baz",
		}))
	defer ctx.Close()

	result := cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))
	// second part of dockerfile was a repeat of first so should be cached
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 1)

	result = cli.BuildCmd(c, "build2", cli.WithFlags("--cache-from=build1"), build.WithExternalBuildContext(ctx))
	// now both parts of dockerfile should be cached
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 2)
}

func (s *DockerCLIBuildSuite) TestBuildNetNone(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildnetnone"
	cli.Docker(cli.Args("build", "-t", name), cli.WithFlags("--network=none"), build.WithDockerfile(`
  FROM busybox
  RUN ping -c 1 8.8.8.8
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Out:      "unreachable",
	})
}

func (s *DockerCLIBuildSuite) TestBuildNetContainer(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	id := cli.DockerCmd(c, "run", "--hostname", "foobar", "-d", "busybox", "nc", "-ll", "-p", "1234", "-e", "hostname").Stdout()

	const name = "testbuildnetcontainer"
	cli.BuildCmd(c, name, cli.WithFlags("--network=container:"+strings.TrimSpace(id)),
		build.WithDockerfile(`
  FROM busybox
  RUN nc localhost 1234 > /otherhost
  `))

	host := cli.DockerCmd(c, "run", "testbuildnetcontainer", "cat", "/otherhost").Combined()
	assert.Equal(c, strings.TrimSpace(host), "foobar")
}

func (s *DockerCLIBuildSuite) TestBuildWithExtraHost(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	const name = "testbuildwithextrahost"
	cli.BuildCmd(c, name,
		cli.WithFlags(
			"--add-host", "foo:127.0.0.1",
			"--add-host", "bar:127.0.0.1",
		),
		build.WithDockerfile(`
  FROM busybox
  RUN ping -c 1 foo
  RUN ping -c 1 bar
  `))
}

func (s *DockerCLIBuildSuite) TestBuildWithExtraHostInvalidFormat(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	dockerfile := `
		FROM busybox
		RUN ping -c 1 foo`

	testCases := []struct {
		testName   string
		dockerfile string
		buildFlag  string
	}{
		{"extra_host_missing_ip", dockerfile, "--add-host=foo"},
		{"extra_host_missing_ip_with_delimiter", dockerfile, "--add-host=foo:"},
		{"extra_host_missing_hostname", dockerfile, "--add-host=:127.0.0.1"},
		{"extra_host_invalid_ipv4", dockerfile, "--add-host=foo:101.10.2"},
		{"extra_host_invalid_ipv6", dockerfile, "--add-host=foo:2001::1::3F"},
	}

	for _, tc := range testCases {
		result := cli.Docker(cli.Args("build", "-t", tc.testName), cli.WithFlags(tc.buildFlag), build.WithDockerfile(tc.dockerfile))
		result.Assert(c, icmd.Expected{
			ExitCode: 125,
		})
	}
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageCopyFromSyntax(c *testing.T) {
	//nolint:dupword
	const dockerfile = `
		FROM busybox AS first
		COPY foo bar

		FROM busybox
		%s
		COPY baz baz
		RUN echo mno > baz/cc

		FROM busybox
		COPY bar /
		COPY --from=1 baz sub/
		COPY --from=0 bar baz
		COPY --from=first bar bay`

	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(fmt.Sprintf(dockerfile, "")),
		fakecontext.WithFiles(map[string]string{
			"foo":    "abc",
			"bar":    "def",
			"baz/aa": "ghi",
			"baz/bb": "jkl",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	cli.DockerCmd(c, "run", "build1", "cat", "bar").Assert(c, icmd.Expected{Out: "def"})
	cli.DockerCmd(c, "run", "build1", "cat", "sub/aa").Assert(c, icmd.Expected{Out: "ghi"})
	cli.DockerCmd(c, "run", "build1", "cat", "sub/cc").Assert(c, icmd.Expected{Out: "mno"})
	cli.DockerCmd(c, "run", "build1", "cat", "baz").Assert(c, icmd.Expected{Out: "abc"})
	cli.DockerCmd(c, "run", "build1", "cat", "bay").Assert(c, icmd.Expected{Out: "abc"})

	result := cli.BuildCmd(c, "build2", build.WithExternalBuildContext(ctx))

	// all commands should be cached
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 7)
	assert.Equal(c, getIDByName(c, "build1"), getIDByName(c, "build2"))

	err := os.WriteFile(filepath.Join(ctx.Dir, "Dockerfile"), fmt.Appendf(nil, dockerfile, "COPY baz/aa foo"), 0o644)
	assert.NilError(c, err)

	// changing file in parent block should not affect last block
	result = cli.BuildCmd(c, "build3", build.WithExternalBuildContext(ctx))
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 5)

	err = os.WriteFile(filepath.Join(ctx.Dir, "foo"), []byte("pqr"), 0o644)
	assert.NilError(c, err)

	// changing file in parent block should affect both first and last block
	result = cli.BuildCmd(c, "build4", build.WithExternalBuildContext(ctx))
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 5)

	cli.DockerCmd(c, "run", "build4", "cat", "bay").Assert(c, icmd.Expected{Out: "pqr"})
	cli.DockerCmd(c, "run", "build4", "cat", "baz").Assert(c, icmd.Expected{Out: "pqr"})
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageCopyFromErrors(c *testing.T) {
	testCases := []struct {
		dockerfile    string
		expectedError string
	}{
		{
			dockerfile: `
		FROM busybox
		COPY --from=foo foo bar`,
			expectedError: "invalid from flag value foo",
		},
		{
			dockerfile: `
		FROM busybox
		COPY --from=0 foo bar`,
			expectedError: "invalid from flag value 0: refers to current build stage",
		},
		{
			dockerfile: `
		FROM busybox AS foo
		COPY --from=bar foo bar`,
			expectedError: "invalid from flag value bar",
		},
		{
			dockerfile: `
		FROM busybox AS 1
		COPY --from=1 foo bar`,
			expectedError: "invalid name for build stage",
		},
	}

	for _, tc := range testCases {
		ctx := fakecontext.New(c, "",
			fakecontext.WithDockerfile(tc.dockerfile),
			fakecontext.WithFiles(map[string]string{
				"foo": "abc",
			}))

		cli.Docker(cli.Args("build", "-t", "build1"), build.WithExternalBuildContext(ctx)).Assert(c, icmd.Expected{
			ExitCode: 1,
			Err:      tc.expectedError,
		})

		ctx.Close()
	}
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageMultipleBuilds(c *testing.T) {
	dockerfile := `
		FROM busybox
		COPY foo bar`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "abc",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	dockerfile = `
		FROM build1:latest AS foo
    FROM busybox
		COPY --from=foo bar /
		COPY foo /`
	ctx = fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "def",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build2", build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "build2", "cat", "bar").Combined()
	assert.Equal(c, strings.TrimSpace(out), "abc")
	out = cli.DockerCmd(c, "run", "build2", "cat", "foo").Combined()
	assert.Equal(c, strings.TrimSpace(out), "def")
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageImplicitFrom(c *testing.T) {
	dockerfile := `
		FROM busybox
		COPY --from=busybox /etc/passwd /mypasswd
		RUN cmp /etc/passwd /mypasswd`

	if DaemonIsWindows() {
		dockerfile = `
			FROM busybox
			COPY --from=busybox License.txt foo`
	}

	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
	)
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	if DaemonIsWindows() {
		out := cli.DockerCmd(c, "run", "build1", "cat", "License.txt").Combined()
		assert.Assert(c, len(out) > 10)
		out2 := cli.DockerCmd(c, "run", "build1", "cat", "foo").Combined()
		assert.Equal(c, out, out2)
	}
}

func (s *DockerRegistrySuite) TestBuildMultiStageImplicitPull(c *testing.T) {
	imgName := fmt.Sprintf("%v/dockercli/testf", privateRegistryURL)

	dockerfile := `
		FROM busybox
		COPY foo bar`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "abc",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, imgName, build.WithExternalBuildContext(ctx))

	cli.DockerCmd(c, "push", imgName)
	cli.DockerCmd(c, "rmi", imgName)

	dockerfile = `
		FROM busybox
		COPY --from=%s bar baz`

	ctx = fakecontext.New(c, "", fakecontext.WithDockerfile(fmt.Sprintf(dockerfile, imgName)))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	cli.Docker(cli.Args("run", "build1", "cat", "baz")).Assert(c, icmd.Expected{Out: "abc"})
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageNameVariants(c *testing.T) {
	dockerfile := `
		FROM busybox as foo
		COPY foo /
		FROM foo as foo1
		RUN echo 1 >> foo
		FROM foo as foO2
		RUN echo 2 >> foo
		FROM foo
		COPY --from=foo1 foo f1
		COPY --from=FOo2 foo f2
		` // foo2 case also tests that names are case insensitive
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "bar",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))
	cli.Docker(cli.Args("run", "build1", "cat", "foo")).Assert(c, icmd.Expected{Out: "bar"})
	cli.Docker(cli.Args("run", "build1", "cat", "f1")).Assert(c, icmd.Expected{Out: "bar1"})
	cli.Docker(cli.Args("run", "build1", "cat", "f2")).Assert(c, icmd.Expected{Out: "bar2"})
}

func (s *DockerCLIBuildSuite) TestBuildMultiStageMultipleBuildsWindows(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	dockerfile := `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY foo c:\\bar`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "abc",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	dockerfile = `
		FROM build1:latest
    	FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY --from=0 c:\\bar /
		COPY foo /`
	ctx = fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"foo": "def",
		}))
	defer ctx.Close()

	cli.BuildCmd(c, "build2", build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "build2", "cmd.exe", "/s", "/c", "type", "c:\\bar").Combined()
	assert.Equal(c, strings.TrimSpace(out), "abc")
	out = cli.DockerCmd(c, "run", "build2", "cmd.exe", "/s", "/c", "type", "c:\\foo").Combined()
	assert.Equal(c, strings.TrimSpace(out), "def")
}

func (s *DockerCLIBuildSuite) TestBuildCopyFromForbidWindowsSystemPaths(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	dockerfile := `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY --from=0 %s c:\\oscopy
		`
	exp := icmd.Expected{
		ExitCode: 1,
		Err:      "copy from c:\\ or c:\\windows is not allowed on windows",
	}
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths1"), build.WithDockerfile(fmt.Sprintf(dockerfile, "c:\\\\"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths2"), build.WithDockerfile(fmt.Sprintf(dockerfile, "C:\\\\"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths3"), build.WithDockerfile(fmt.Sprintf(dockerfile, "c:\\\\windows"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths4"), build.WithDockerfile(fmt.Sprintf(dockerfile, "c:\\\\wInDows"))).Assert(c, exp)
}

func (s *DockerCLIBuildSuite) TestBuildCopyFromForbidWindowsRelativePaths(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	dockerfile := `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY --from=0 %s c:\\oscopy
		`
	exp := icmd.Expected{
		ExitCode: 1,
		Err:      "copy from c:\\ or c:\\windows is not allowed on windows",
	}
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths1"), build.WithDockerfile(fmt.Sprintf(dockerfile, "c:"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths2"), build.WithDockerfile(fmt.Sprintf(dockerfile, "."))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths3"), build.WithDockerfile(fmt.Sprintf(dockerfile, "..\\\\"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths4"), build.WithDockerfile(fmt.Sprintf(dockerfile, ".\\\\windows"))).Assert(c, exp)
	cli.Docker(cli.Args("build", "-t", "testforbidsystempaths5"), build.WithDockerfile(fmt.Sprintf(dockerfile, "\\\\windows"))).Assert(c, exp)
}

func (s *DockerCLIBuildSuite) TestBuildCopyFromWindowsIsCaseInsensitive(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	dockerfile := `
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY foo /
		FROM ` + testEnv.PlatformDefaults.BaseImage + `
		COPY --from=0 c:\\fOo c:\\copied
		RUN type c:\\copied
		`
	cli.Docker(cli.Args("build", "-t", "copyfrom-windows-insensitive"), build.WithBuildContext(c,
		build.WithFile("Dockerfile", dockerfile),
		build.WithFile("foo", "hello world"),
	)).Assert(c, icmd.Expected{
		ExitCode: 0,
		Out:      "hello world",
	})
}

// #33176
func (s *DockerCLIBuildSuite) TestBuildMultiStageResetScratch(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	dockerfile := `
		FROM busybox
		WORKDIR /foo/bar
		FROM scratch
		ENV FOO=bar
		`
	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
	)
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx))

	res := cli.InspectCmd(c, "build1", cli.Format(".Config.WorkingDir")).Combined()
	assert.Equal(c, strings.TrimSpace(res), "")
}

func (s *DockerCLIBuildSuite) TestBuildIntermediateTarget(c *testing.T) {
	dockerfile := `
		FROM busybox AS build-env
		CMD ["/dev"]
		FROM busybox
		CMD ["/dist"]
		`
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile))
	defer ctx.Close()

	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx),
		cli.WithFlags("--target", "build-env"))

	res := cli.InspectCmd(c, "build1", cli.Format("json .Config.Cmd")).Combined()
	assert.Equal(c, strings.TrimSpace(res), `["/dev"]`)

	// Stage name is case-insensitive by design
	cli.BuildCmd(c, "build1", build.WithExternalBuildContext(ctx),
		cli.WithFlags("--target", "BUIld-EnV"))

	res = cli.InspectCmd(c, "build1", cli.Format("json .Config.Cmd")).Combined()
	assert.Equal(c, strings.TrimSpace(res), `["/dev"]`)

	result := cli.Docker(cli.Args("build", "-t", "build1"), build.WithExternalBuildContext(ctx),
		cli.WithFlags("--target", "nosuchtarget"))
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "target stage \"nosuchtarget\" could not be found",
	})
}

// TestBuildOpaqueDirectory tests that a build succeeds which
// creates opaque directories.
// See https://github.com/moby/moby/issues/25244
func (s *DockerCLIBuildSuite) TestBuildOpaqueDirectory(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	dockerFile := `
		FROM busybox
		RUN mkdir /dir1 && touch /dir1/f1
		RUN rm -rf /dir1 && mkdir /dir1 && touch /dir1/f2
		RUN touch /dir1/f3
		RUN [ -f /dir1/f2 ]
		`
	// Test that build succeeds, last command fails if opaque directory
	// was not handled correctly
	cli.BuildCmd(c, "testopaquedirectory", build.WithDockerfile(dockerFile))
}

// Windows test for USER in dockerfile
func (s *DockerCLIBuildSuite) TestBuildWindowsUser(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name = "testbuildwindowsuser"
	//nolint:dupword
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM `+testEnv.PlatformDefaults.BaseImage+`
		RUN net user user /add
		USER user
		RUN set username
		`)).Assert(c, icmd.Expected{
		Out: "USERNAME=user",
	})
}

// Verifies if COPY file . when WORKDIR is set to a non-existing directory,
// the directory is created and the file is copied into the directory,
// as opposed to the file being copied as a file with the name of the
// directory. Fix for 27545 (found on Windows, but regression good for Linux too).
// Note 27545 was reverted in 28505, but a new fix was added subsequently in 28514.
func (s *DockerCLIBuildSuite) TestBuildCopyFileDotWithWorkdir(c *testing.T) {
	const name = "testbuildcopyfiledotwithworkdir"
	cli.BuildCmd(c, name, build.WithBuildContext(c,
		build.WithFile("Dockerfile", `FROM busybox
WORKDIR /foo
COPY file .
RUN ["cat", "/foo/file"]
`),
		build.WithFile("file", "content"),
	))
}

// Case-insensitive environment variables on Windows
func (s *DockerCLIBuildSuite) TestBuildWindowsEnvCaseInsensitive(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	const name = "testbuildwindowsenvcaseinsensitive"
	cli.BuildCmd(c, name, build.WithDockerfile(`
		FROM `+testEnv.PlatformDefaults.BaseImage+`
		ENV FOO=bar foo=baz
  `))
	res := inspectFieldJSON(c, name, "Config.Env")
	if res != `["foo=baz"]` { // Should not have FOO=bar in it - takes the last one processed. And only one entry as deduped.
		c.Fatalf("Case insensitive environment variables on Windows failed. Got %s", res)
	}
}

// Test case for 29667
func (s *DockerCLIBuildSuite) TestBuildWorkdirImageCmd(c *testing.T) {
	imgName := strings.ToLower(c.Name())
	cli.BuildCmd(c, imgName, build.WithDockerfile(`
FROM busybox
WORKDIR /foo/bar
`))
	out := cli.DockerCmd(c, "inspect", "--format", "{{ json .Config.Cmd }}", imgName).Stdout()
	assert.Equal(c, strings.TrimSpace(out), `["sh"]`)

	imgName = "testworkdirlabelimagecmd"
	cli.BuildCmd(c, imgName, build.WithDockerfile(`
FROM busybox
WORKDIR /foo/bar
LABEL a=b
`))

	out = cli.DockerCmd(c, "inspect", "--format", "{{ json .Config.Cmd }}", imgName).Stdout()
	assert.Equal(c, strings.TrimSpace(out), `["sh"]`)
}

// Test case for 28902/28909
func (s *DockerCLIBuildSuite) TestBuildWorkdirCmd(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testbuildworkdircmd"
	dockerFile := `
                FROM busybox
                WORKDIR /
                `
	cli.BuildCmd(c, name, build.WithDockerfile(dockerFile))
	result := cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(dockerFile))
	result.Assert(c, icmd.Success)
	assert.Equal(c, strings.Count(result.Combined(), "Using cache"), 1)
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildLineErrorOnBuild(c *testing.T) {
	const name = "test_build_line_error_onbuild"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
  ONBUILD
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "parse error on line 2: ONBUILD requires at least one argument",
	})
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildLineErrorUnknownInstruction(c *testing.T) {
	const name = "test_build_line_error_unknown_instruction"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
  RUN echo hello world
  NOINSTRUCTION echo ba
  RUN echo hello
  ERROR
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "parse error on line 3: unknown instruction: NOINSTRUCTION",
	})
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildLineErrorWithEmptyLines(c *testing.T) {
	const name = "test_build_line_error_with_empty_lines"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`
  FROM busybox

  RUN echo hello world

  NOINSTRUCTION echo ba

  CMD ["/bin/init"]
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "parse error on line 6: unknown instruction: NOINSTRUCTION",
	})
}

// FIXME(vdemeester) should be a unit test
func (s *DockerCLIBuildSuite) TestBuildLineErrorWithComments(c *testing.T) {
	const name = "test_build_line_error_with_comments"
	cli.Docker(cli.Args("build", "-t", name), build.WithDockerfile(`FROM busybox
  # This will print hello world
  # and then ba
  RUN echo hello world
  NOINSTRUCTION echo ba
  `)).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "parse error on line 5: unknown instruction: NOINSTRUCTION",
	})
}

// #31957
func (s *DockerCLIBuildSuite) TestBuildSetCommandWithDefinedShell(c *testing.T) {
	cli.BuildCmd(c, "build1", build.WithDockerfile(`
FROM busybox
SHELL ["/bin/sh", "-c"]
`))
	cli.BuildCmd(c, "build2", build.WithDockerfile(`
FROM build1
CMD echo foo
`))

	out := cli.DockerCmd(c, "inspect", "--format", "{{ json .Config.Cmd }}", "build2").Stdout()
	expected := `["/bin/sh","-c","echo foo"]`
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = `["/bin/sh -c echo foo"]`
	}
	assert.Equal(c, strings.TrimSpace(out), expected)
}

// FIXME(vdemeester) should migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildIidFile(c *testing.T) {
	tmpDir, err := os.MkdirTemp("", "TestBuildIidFile")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	tmpIidFile := filepath.Join(tmpDir, "iid")

	const name = "testbuildiidfile"
	// Use a Dockerfile with multiple stages to ensure we get the last one
	cli.BuildCmd(c, name,
		build.WithDockerfile(`FROM `+minimalBaseImage()+` AS stage1
ENV FOO FOO1
FROM `+minimalBaseImage()+`
ENV BAR BAZ`),
		cli.WithFlags("--iidfile", tmpIidFile))

	id, err := os.ReadFile(tmpIidFile)
	assert.NilError(c, err)
	d, err := digest.Parse(string(id))
	assert.NilError(c, err)
	assert.Equal(c, d.String(), getIDByName(c, name))
}

// FIXME(vdemeester) should migrate to docker/cli tests
func (s *DockerCLIBuildSuite) TestBuildIidFileCleanupOnFail(c *testing.T) {
	tmpDir, err := os.MkdirTemp("", "TestBuildIidFileCleanupOnFail")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	tmpIidFile := filepath.Join(tmpDir, "iid")

	err = os.WriteFile(tmpIidFile, []byte("Dummy"), 0o666)
	assert.NilError(c, err)

	cli.Docker(cli.Args("build", "-t", "testbuildiidfilecleanuponfail"),
		build.WithDockerfile(`FROM `+minimalBaseImage()+`
	RUN /non/existing/command`),
		cli.WithFlags("--iidfile", tmpIidFile)).Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	_, err = os.Stat(tmpIidFile)
	assert.ErrorContains(c, err, "")
	assert.Equal(c, os.IsNotExist(err), true)
}

func (s *DockerCLIBuildSuite) TestBuildEmitsEvents(t *testing.T) {
	for _, builder := range []struct {
		buildkit bool
	}{
		{buildkit: false},
		{buildkit: true},
	} {
		for _, tc := range []struct {
			name  string
			args  []string
			check func(t *testing.T, stdout string)
		}{
			{
				name: "no tag",
				args: []string{},
				check: func(t *testing.T, stdout string) {
					if assert.Check(t, is.Contains(stdout, "image create")) {
						assert.Check(t, strings.Count(stdout, "image create") == 1)
					}
					assert.Check(t, !strings.Contains(stdout, "image tag"))
				},
			},
			{
				name: "with tag",
				args: []string{"-t", "testbuildemitsimagetagevent"},
				check: func(t *testing.T, stdout string) {
					if assert.Check(t, is.Contains(stdout, "image create")) {
						assert.Check(t, strings.Count(stdout, "image create") == 1)
					}
					if assert.Check(t, is.Contains(stdout, "image tag")) {
						assert.Check(t, strings.Count(stdout, "image tag") == 1)
					}
					assert.Check(t, is.Contains(stdout, "testbuildemitsimagetagevent"))
				},
			},
		} {
			t.Run(fmt.Sprintf("buildkit=%v/%s", builder.buildkit, tc.name), func(t *testing.T) {
				skip.If(t, builder.buildkit && DaemonIsWindows() && !containerdSnapshotterEnabled(),
					"Buildkit is not supported on Windows with graphdrivers")

				since := daemonUnixTime(t)

				iidFile := filepath.Join(t.TempDir(), "iid")
				args := []string{"build", "--iidfile", iidFile}
				args = append(args, tc.args...)

				b := cli.Docker(cli.Args(args...),
					build.WithoutCache,
					build.WithDockerfile("FROM busybox\nRUN echo hi >/hello"),
					build.WithBuildkit(builder.buildkit),
				)
				assert.NilError(t, b.Compare(icmd.Success), b.Combined())

				imageID, err := os.ReadFile(iidFile)
				assert.NilError(t, err)

				until := daemonUnixTime(t)

				cmd := cli.Docker(
					cli.Args("events",
						"--filter", "type=image",
						"--filter", "image="+strings.TrimSpace(string(imageID)),
						"--since", since,
						"--until", until,
					),
					cli.WithEnvironmentVariables("DOCKER_API_VERSION=v1.46"), // FIXME(thaJeztah): integration-cli runs docker CLI 25.0; we're "upgrading" the API version to a version it doesn't support here ;)
				)

				tc.check(t, cmd.Stdout())
			})
		}
	}
}

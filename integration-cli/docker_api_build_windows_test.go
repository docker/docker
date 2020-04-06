// +build windows

package main

import (
	"net/http"
	"testing"

	"github.com/docker/docker/testutil/fakecontext"
	"github.com/docker/docker/testutil/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func (s *DockerSuite) TestBuildWithRecycleBin(c *testing.T) {
	testRequires(c, DaemonIsWindows)

	dockerfile := "" +
		"FROM " + testEnv.PlatformDefaults.BaseImage + "\n" +
		"RUN md $REcycLE.biN && md missing\n" +
		"RUN dir $Recycle.Bin && exit 1 || exit 0\n" +
		"RUN dir missing\n"

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(dockerfile))
	defer ctx.Close()

	res, body, err := request.Post(
		"/build",
		request.RawContent(ctx.AsTarReader(c)),
		request.ContentType("application/x-tar"))

	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusOK)

	out, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Check(c, is.Contains(string(out), "Successfully built"))
}

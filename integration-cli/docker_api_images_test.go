package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/internal/test/request"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

func (s *DockerSuite) TestAPIImagesFilter(c *check.C) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	name := "utest:tag1"
	name2 := "utest/docker:tag2"
	name3 := "utest:5000/docker:tag3"
	for _, n := range []string{name, name2, name3} {
		dockerCmd(c, "tag", "busybox", n)
	}
	getImages := func(filter string) []types.ImageSummary {
		filters := filters.NewArgs()
		filters.Add("reference", filter)
		options := types.ImageListOptions{
			All:     false,
			Filters: filters,
		}
		images, err := cli.ImageList(context.Background(), options)
		assert.NilError(c, err)

		return images
	}

	//incorrect number of matches returned
	images := getImages("utest*/*")
	assert.Equal(c, len(images[0].RepoTags), 2)

	images = getImages("utest")
	assert.Equal(c, len(images[0].RepoTags), 1)

	images = getImages("utest*")
	assert.Equal(c, len(images[0].RepoTags), 1)

	images = getImages("*5000*/*")
	assert.Equal(c, len(images[0].RepoTags), 1)
}

func (s *DockerSuite) TestAPIImagesSaveAndLoad(c *check.C) {
	if runtime.GOOS == "windows" {
		v, err := kernel.GetKernelVersion()
		assert.NilError(c, err)
		build, _ := strconv.Atoi(strings.Split(strings.SplitN(v.String(), " ", 3)[2][1:], ".")[0])
		if build == 16299 {
			c.Skip("Temporarily disabled on RS3 builds")
		}
	}

	testRequires(c, Network)
	buildImageSuccessfully(c, "saveandload", build.WithDockerfile("FROM busybox\nENV FOO bar"))
	id := getIDByName(c, "saveandload")

	res, body, err := request.Get("/images/" + id + "/get")
	assert.NilError(c, err)
	defer body.Close()
	assert.Equal(c, res.StatusCode, http.StatusOK)

	dockerCmd(c, "rmi", id)

	res, loadBody, err := request.Post("/images/load", request.RawContent(body), request.ContentType("application/x-tar"))
	assert.NilError(c, err)
	defer loadBody.Close()
	assert.Equal(c, res.StatusCode, http.StatusOK)

	inspectOut := cli.InspectCmd(c, id, cli.Format(".Id")).Combined()
	assert.Equal(c, strings.TrimSpace(string(inspectOut)), id, "load did not work properly")
}

func (s *DockerSuite) TestAPIImagesDelete(c *check.C) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	if testEnv.OSType != "windows" {
		testRequires(c, Network)
	}
	name := "test-api-images-delete"
	buildImageSuccessfully(c, name, build.WithDockerfile("FROM busybox\nENV FOO bar"))
	id := getIDByName(c, name)

	dockerCmd(c, "tag", name, "test:tag1")

	_, err = cli.ImageRemove(context.Background(), id, types.ImageRemoveOptions{})
	assert.ErrorContains(c, err, "unable to delete")

	_, err = cli.ImageRemove(context.Background(), "test:noexist", types.ImageRemoveOptions{})
	assert.ErrorContains(c, err, "No such image")

	_, err = cli.ImageRemove(context.Background(), "test:tag1", types.ImageRemoveOptions{})
	assert.NilError(c, err)
}

func (s *DockerSuite) TestAPIImagesHistory(c *check.C) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	if testEnv.OSType != "windows" {
		testRequires(c, Network)
	}
	name := "test-api-images-history"
	buildImageSuccessfully(c, name, build.WithDockerfile("FROM busybox\nENV FOO bar"))
	id := getIDByName(c, name)

	historydata, err := cli.ImageHistory(context.Background(), id)
	assert.NilError(c, err)

	assert.Assert(c, len(historydata) != 0)
	var found bool
	for _, tag := range historydata[0].Tags {
		if tag == "test-api-images-history:latest" {
			found = true
			break
		}
	}
	assert.Assert(c, found)
}

func (s *DockerSuite) TestAPIImagesImportBadSrc(c *check.C) {
	if runtime.GOOS == "windows" {
		v, err := kernel.GetKernelVersion()
		assert.NilError(c, err)
		build, _ := strconv.Atoi(strings.Split(strings.SplitN(v.String(), " ", 3)[2][1:], ".")[0])
		if build == 16299 {
			c.Skip("Temporarily disabled on RS3 builds")
		}
	}

	testRequires(c, Network, testEnv.IsLocalDaemon)

	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	tt := []struct {
		statusExp int
		fromSrc   string
	}{
		{http.StatusNotFound, server.URL + "/nofile.tar"},
		{http.StatusNotFound, strings.TrimPrefix(server.URL, "http://") + "/nofile.tar"},
		{http.StatusNotFound, strings.TrimPrefix(server.URL, "http://") + "%2Fdata%2Ffile.tar"},
		{http.StatusInternalServerError, "%2Fdata%2Ffile.tar"},
	}

	for _, te := range tt {
		res, _, err := request.Post(strings.Join([]string{"/images/create?fromSrc=", te.fromSrc}, ""), request.JSON)
		assert.NilError(c, err)
		assert.Equal(c, res.StatusCode, te.statusExp)
		assert.Equal(c, res.Header.Get("Content-Type"), "application/json")
	}

}

// #14846
func (s *DockerSuite) TestAPIImagesSearchJSONContentType(c *check.C) {
	testRequires(c, Network)

	res, b, err := request.Get("/images/search?term=test", request.JSON)
	assert.NilError(c, err)
	b.Close()
	assert.Equal(c, res.StatusCode, http.StatusOK)
	assert.Equal(c, res.Header.Get("Content-Type"), "application/json")
}

// Test case for 30027: image size reported as -1 in v1.12 client against v1.13 daemon.
// This test checks to make sure both v1.12 and v1.13 client against v1.13 daemon get correct `Size` after the fix.
func (s *DockerSuite) TestAPIImagesSizeCompatibility(c *check.C) {
	apiclient := testEnv.APIClient()
	defer apiclient.Close()

	images, err := apiclient.ImageList(context.Background(), types.ImageListOptions{})
	assert.NilError(c, err)
	assert.Assert(c, len(images) != 0)
	for _, image := range images {
		assert.Assert(c, image.Size != int64(-1))
	}

	apiclient, err = client.NewClientWithOpts(client.FromEnv, client.WithVersion("v1.24"))
	assert.NilError(c, err)
	defer apiclient.Close()

	v124Images, err := apiclient.ImageList(context.Background(), types.ImageListOptions{})
	assert.NilError(c, err)
	assert.Assert(c, len(v124Images) != 0)
	for _, image := range v124Images {
		assert.Assert(c, image.Size != int64(-1))
	}
}

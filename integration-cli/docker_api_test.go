package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/internal/test/request"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

func (s *DockerSuite) TestAPIOptionsRoute(c *check.C) {
	resp, _, err := request.Do("/", request.Method(http.MethodOptions))
	assert.NilError(c, err)
	assert.Equal(c, resp.StatusCode, http.StatusOK)
}

func (s *DockerSuite) TestAPIGetEnabledCORS(c *check.C) {
	res, body, err := request.Get("/version")
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusOK)
	body.Close()
	// TODO: @runcom incomplete tests, why old integration tests had this headers
	// and here none of the headers below are in the response?
	//c.Log(res.Header)
	//assert.Equal(c, res.Header.Get("Access-Control-Allow-Origin"), "*")
	//assert.Equal(c, res.Header.Get("Access-Control-Allow-Headers"), "Origin, X-Requested-With, Content-Type, Accept, X-Registry-Auth")
}

func (s *DockerSuite) TestAPIClientVersionOldNotSupported(c *check.C) {
	if testEnv.OSType != runtime.GOOS {
		c.Skip("Daemon platform doesn't match test platform")
	}
	if api.MinVersion == api.DefaultVersion {
		c.Skip("API MinVersion==DefaultVersion")
	}
	v := strings.Split(api.MinVersion, ".")
	vMinInt, err := strconv.Atoi(v[1])
	assert.NilError(c, err)
	vMinInt--
	v[1] = strconv.Itoa(vMinInt)
	version := strings.Join(v, ".")

	resp, body, err := request.Get("/v" + version + "/version")
	assert.NilError(c, err)
	defer body.Close()
	assert.Equal(c, resp.StatusCode, http.StatusBadRequest)
	expected := fmt.Sprintf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", version, api.MinVersion)
	content, err := ioutil.ReadAll(body)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(string(content)), expected)
}

func (s *DockerSuite) TestAPIErrorJSON(c *check.C) {
	httpResp, body, err := request.Post("/containers/create", request.JSONBody(struct{}{}))
	assert.NilError(c, err)
	if versions.LessThan(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, httpResp.StatusCode, http.StatusInternalServerError)
	} else {
		assert.Equal(c, httpResp.StatusCode, http.StatusBadRequest)
	}
	assert.Assert(c, strings.Contains(httpResp.Header.Get("Content-Type"), "application/json"))
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Equal(c, getErrorMessage(c, b), "Config cannot be empty in order to create a container")
}

func (s *DockerSuite) TestAPIErrorPlainText(c *check.C) {
	// Windows requires API 1.25 or later. This test is validating a behaviour which was present
	// in v1.23, but changed in 1.24, hence not applicable on Windows. See apiVersionSupportsJSONErrors
	testRequires(c, DaemonIsLinux)
	httpResp, body, err := request.Post("/v1.23/containers/create", request.JSONBody(struct{}{}))
	assert.NilError(c, err)
	if versions.LessThan(testEnv.DaemonAPIVersion(), "1.32") {
		assert.Equal(c, httpResp.StatusCode, http.StatusInternalServerError)
	} else {
		assert.Equal(c, httpResp.StatusCode, http.StatusBadRequest)
	}
	assert.Assert(c, strings.Contains(httpResp.Header.Get("Content-Type"), "text/plain"))
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(string(b)), "Config cannot be empty in order to create a container")
}

func (s *DockerSuite) TestAPIErrorNotFoundJSON(c *check.C) {
	// 404 is a different code path to normal errors, so test separately
	httpResp, body, err := request.Get("/notfound", request.JSON)
	assert.NilError(c, err)
	assert.Equal(c, httpResp.StatusCode, http.StatusNotFound)
	assert.Assert(c, strings.Contains(httpResp.Header.Get("Content-Type"), "application/json"))
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Equal(c, getErrorMessage(c, b), "page not found")
}

func (s *DockerSuite) TestAPIErrorNotFoundPlainText(c *check.C) {
	httpResp, body, err := request.Get("/v1.23/notfound", request.JSON)
	assert.NilError(c, err)
	assert.Equal(c, httpResp.StatusCode, http.StatusNotFound)
	assert.Assert(c, strings.Contains(httpResp.Header.Get("Content-Type"), "text/plain"))
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(string(b)), "page not found")
}

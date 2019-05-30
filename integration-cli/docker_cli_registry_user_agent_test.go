package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"

	"github.com/docker/docker/internal/test/registry"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

// unescapeBackslashSemicolonParens unescapes \;()
func unescapeBackslashSemicolonParens(s string) string {
	re := regexp.MustCompile(`\\;`)
	ret := re.ReplaceAll([]byte(s), []byte(";"))

	re = regexp.MustCompile(`\\\(`)
	ret = re.ReplaceAll([]byte(ret), []byte("("))

	re = regexp.MustCompile(`\\\)`)
	ret = re.ReplaceAll([]byte(ret), []byte(")"))

	re = regexp.MustCompile(`\\\\`)
	ret = re.ReplaceAll([]byte(ret), []byte(`\`))

	return string(ret)
}

func regexpCheckUA(c *check.C, ua string) {
	re := regexp.MustCompile("(?P<dockerUA>.+) UpstreamClient(?P<upstreamUA>.+)")
	substrArr := re.FindStringSubmatch(ua)

	assert.Equal(c, len(substrArr), 3, "Expected 'UpstreamClient()' with upstream client UA")
	dockerUA := substrArr[1]
	upstreamUAEscaped := substrArr[2]

	// check dockerUA looks correct
	reDockerUA := regexp.MustCompile("^docker/[0-9A-Za-z+]")
	bMatchDockerUA := reDockerUA.MatchString(dockerUA)
	assert.Assert(c, bMatchDockerUA, "Docker Engine User-Agent malformed")

	// check upstreamUA looks correct
	// Expecting something like:  Docker-Client/1.11.0-dev (linux)
	upstreamUA := unescapeBackslashSemicolonParens(upstreamUAEscaped)
	reUpstreamUA := regexp.MustCompile("^\\(Docker-Client/[0-9A-Za-z+]")
	bMatchUpstreamUA := reUpstreamUA.MatchString(upstreamUA)
	assert.Assert(c, bMatchUpstreamUA, "(Upstream) Docker Client User-Agent malformed")
}

// registerUserAgentHandler registers a handler for the `/v2/*` endpoint.
// Note that a 404 is returned to prevent the client to proceed.
// We are only checking if the client sent a valid User Agent string along
// with the request.
func registerUserAgentHandler(reg *registry.Mock, result *string) {
	reg.RegisterHandler("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"errors":[{"code": "UNSUPPORTED","message": "this is a mock registry"}]}`))
		var ua string
		for k, v := range r.Header {
			if k == "User-Agent" {
				ua = v[0]
			}
		}
		*result = ua
	})
}

// TestUserAgentPassThrough verifies that when an image is pulled from
// a registry, the registry should see a User-Agent string of the form
// [docker engine UA] UpstreamClientSTREAM-CLIENT([client UA])
func (s *DockerRegistrySuite) TestUserAgentPassThrough(c *check.C) {
	var ua string

	reg, err := registry.NewMock(c)
	defer reg.Close()
	assert.NilError(c, err)
	registerUserAgentHandler(reg, &ua)
	repoName := fmt.Sprintf("%s/busybox", reg.URL())

	s.d.StartWithBusybox(c, "--insecure-registry", reg.URL())

	tmp, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)
	defer os.RemoveAll(tmp)

	dockerfile, err := makefile(tmp, fmt.Sprintf("FROM %s", repoName))
	assert.NilError(c, err, "Unable to create test dockerfile")

	s.d.Cmd("build", "--file", dockerfile, tmp)
	regexpCheckUA(c, ua)

	s.d.Cmd("login", "-u", "richard", "-p", "testtest", reg.URL())
	regexpCheckUA(c, ua)

	s.d.Cmd("pull", repoName)
	regexpCheckUA(c, ua)

	s.d.Cmd("tag", "busybox", repoName)
	s.d.Cmd("push", repoName)
	regexpCheckUA(c, ua)
}

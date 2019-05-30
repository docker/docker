package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/docker/docker/internal/test/registry"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

func makefile(path string, contents string) (string, error) {
	f, err := ioutil.TempFile(path, "tmp")
	if err != nil {
		return "", err
	}
	err = ioutil.WriteFile(f.Name(), []byte(contents), os.ModePerm)
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

// TestV2Only ensures that a daemon does not
// attempt to contact any v1 registry endpoints.
func (s *DockerRegistrySuite) TestV2Only(c *check.C) {
	reg, err := registry.NewMock(c)
	defer reg.Close()
	assert.NilError(c, err)

	reg.RegisterHandler("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})

	reg.RegisterHandler("/v1/.*", func(w http.ResponseWriter, r *http.Request) {
		c.Fatal("V1 registry contacted")
	})

	repoName := fmt.Sprintf("%s/busybox", reg.URL())

	s.d.Start(c, "--insecure-registry", reg.URL())

	tmp, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)
	defer os.RemoveAll(tmp)

	dockerfileName, err := makefile(tmp, fmt.Sprintf("FROM %s/busybox", reg.URL()))
	assert.NilError(c, err, "Unable to create test dockerfile")

	s.d.Cmd("build", "--file", dockerfileName, tmp)

	s.d.Cmd("run", repoName)
	s.d.Cmd("login", "-u", "richard", "-p", "testtest", reg.URL())
	s.d.Cmd("tag", "busybox", repoName)
	s.d.Cmd("push", repoName)
	s.d.Cmd("pull", repoName)
}

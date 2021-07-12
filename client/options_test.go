package client

import (
	"testing"
	"time"

	"github.com/docker/docker/api"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/env"
)

func TestOptionWithHostFromEnv(t *testing.T) {
	c, err := NewClientWithOpts(WithHostFromEnv())
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.host, DefaultDockerHost)
	assert.Equal(t, c.proto, defaultProto)
	assert.Equal(t, c.addr, defaultAddr)
	assert.Equal(t, c.basePath, "")

	defer env.Patch(t, "DOCKER_HOST", "tcp://foo.example.com:2376/test/")()

	c, err = NewClientWithOpts(WithHostFromEnv())
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.host, "tcp://foo.example.com:2376/test/")
	assert.Equal(t, c.proto, "tcp")
	assert.Equal(t, c.addr, "foo.example.com:2376")
	assert.Equal(t, c.basePath, "/test/")
}

func TestOptionWithTimeout(t *testing.T) {
	timeout := 10 * time.Second
	c, err := NewClientWithOpts(WithTimeout(timeout))
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.client.Timeout, timeout)
}

func TestOptionWithVersionFromEnv(t *testing.T) {
	c, err := NewClientWithOpts(WithVersionFromEnv())
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.version, api.DefaultVersion)
	assert.Equal(t, c.manualOverride, false)

	defer env.Patch(t, "DOCKER_API_VERSION", "2.9999")()

	c, err = NewClientWithOpts(WithVersionFromEnv())
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.version, "2.9999")
	assert.Equal(t, c.manualOverride, true)
}

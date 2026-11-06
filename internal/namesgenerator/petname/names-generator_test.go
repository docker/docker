package petname

import (
	"context"
	"testing"

	"github.com/moby/extensions/host"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	servicenamegeneratorv0 "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestGenerateNames(t *testing.T) {
	t.Parallel()

	h, err := host.New(t.Context(), host.WithExtensions(Extension))
	assert.NilError(t, err)
	t.Cleanup(func() {
		assert.NilError(t, h.Shutdown(context.WithoutCancel(t.Context())))
	})

	containerGenerator, err := containernamegeneratorv0.Point.Single(h)
	assert.NilError(t, err)
	container, err := containerGenerator.GenerateContainerName(t.Context(), &containernamegeneratorv0.GenerateContainerNameRequest{})
	assert.NilError(t, err)
	assert.Check(t, is.Regexp(`^[a-z]+_[a-z]+$`, container.Name))

	serviceGenerator, err := servicenamegeneratorv0.Point.Single(h)
	assert.NilError(t, err)
	service, err := serviceGenerator.GenerateServiceName(t.Context(), &servicenamegeneratorv0.GenerateServiceNameRequest{})
	assert.NilError(t, err)
	assert.Check(t, is.Regexp(`^[a-z]+_[a-z]+$`, service.Name))
}

func TestNilRequests(t *testing.T) {
	t.Parallel()
	container, err := (provider{}).GenerateContainerName(t.Context(), nil)
	assert.Error(t, err, "generate container name request is required")
	assert.Assert(t, container == nil)
	service, err := (provider{}).GenerateServiceName(t.Context(), nil)
	assert.Error(t, err, "generate service name request is required")
	assert.Assert(t, service == nil)
}

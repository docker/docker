package system

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/versions"
	"github.com/google/uuid"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

func TestUUIDGeneration(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "ID format changed")
	defer setupTest(t)()

	c := testEnv.APIClient()
	info, err := c.Info(context.Background())
	assert.NilError(t, err)

	_, err = uuid.Parse(info.ID)
	assert.NilError(t, err, info.ID)
}

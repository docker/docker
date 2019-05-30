package client

import (
	"testing"
	"time"

	"gotest.tools/assert"
)

func TestOptionWithTimeout(t *testing.T) {
	timeout := 10 * time.Second
	c, err := NewClientWithOpts(WithTimeout(timeout))
	assert.NilError(t, err)
	assert.Check(t, c.client != nil)
	assert.Equal(t, c.client.Timeout, timeout)
}

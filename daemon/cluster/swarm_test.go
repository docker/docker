package cluster

import (
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
)

func TestClusterUpdateNameInvalid(t *testing.T) {
	c := &Cluster{}

	err := c.Update(1, swarm.Spec{Annotations: swarm.Annotations{Name: "whoops"}}, swarm.UpdateFlags{})
	assert.EqualError(t, err, `invalid Name "whoops": swarm spec must be named "default"`)
}

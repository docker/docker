//go:build !windows

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
)

func (s *DockerSwarmSuite) TestAPISwarmListNodes(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)
	d3 := s.AddDaemon(ctx, c, true, false)

	nodes := d1.ListNodes(ctx, c)
	assert.Equal(c, len(nodes), 3, fmt.Sprintf("nodes: %#v", nodes))

loop0:
	for _, n := range nodes {
		for _, d := range []*daemon.Daemon{d1, d2, d3} {
			if n.ID == d.NodeID() {
				continue loop0
			}
		}
		c.Errorf("unknown nodeID %v", n.ID)
	}
}

func (s *DockerSwarmSuite) TestAPISwarmNodeUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)

	d := s.AddDaemon(ctx, c, true, true)

	nodes := d.ListNodes(ctx, c)

	d.UpdateNode(ctx, c, nodes[0].ID, func(n *swarm.Node) {
		n.Spec.Availability = swarm.NodeAvailabilityPause
	})

	n := d.GetNode(ctx, c, nodes[0].ID)
	assert.Equal(c, n.Spec.Availability, swarm.NodeAvailabilityPause)
}

func (s *DockerSwarmSuite) TestAPISwarmNodeRemove(c *testing.T) {
	testRequires(c, Network)

	ctx := testutil.GetContext(c)

	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)
	_ = s.AddDaemon(ctx, c, true, false)

	nodes := d1.ListNodes(ctx, c)
	assert.Equal(c, len(nodes), 3, fmt.Sprintf("nodes: %#v", nodes))

	// Getting the info so we can take the NodeID
	d2Info := d2.SwarmInfo(ctx, c)

	// forceful removal of d2 should work
	d1.RemoveNode(ctx, c, d2Info.NodeID, true)

	nodes = d1.ListNodes(ctx, c)
	assert.Equal(c, len(nodes), 2, fmt.Sprintf("nodes: %#v", nodes))

	// Restart the node that was removed
	d2.RestartNode(c)

	// Give some time for the node to rejoin
	time.Sleep(1 * time.Second)

	// Make sure the node didn't rejoin
	nodes = d1.ListNodes(ctx, c)
	assert.Equal(c, len(nodes), 2, fmt.Sprintf("nodes: %#v", nodes))
}

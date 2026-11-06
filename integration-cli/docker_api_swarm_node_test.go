//go:build !windows

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
)

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

func (s *DockerSwarmSuite) TestAPISwarmNodeDrainPause(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)

	time.Sleep(1 * time.Second) // make sure all daemons are ready to accept tasks

	// start a service, expect balanced distribution
	instances := 2
	id := d1.CreateService(ctx, c, simpleTestService, setInstances(instances))

	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	// drain d2, all containers should move to d1
	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Availability = swarm.NodeAvailabilityDrain
	})
	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))

	// set d2 back to active
	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Availability = swarm.NodeAvailabilityActive
	})

	instances = 1
	d1.UpdateService(ctx, c, d1.GetService(ctx, c, id), setInstances(instances))
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout*2))

	instances = 2
	d1.UpdateService(ctx, c, d1.GetService(ctx, c, id), setInstances(instances))

	// drained node first so we don't get any old containers
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.GreaterThan(0)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout*2))

	d2ContainerCount := len(d2.ActiveContainers(testutil.GetContext(c), c))

	// set d2 to paused, scale service up, only d1 gets new tasks
	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Availability = swarm.NodeAvailabilityPause
	})

	instances = 4
	d1.UpdateService(ctx, c, d1.GetService(ctx, c, id), setInstances(instances))
	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.Equals(instances-d2ContainerCount)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.Equals(d2ContainerCount)), poll.WithTimeout(defaultReconciliationTimeout))
}

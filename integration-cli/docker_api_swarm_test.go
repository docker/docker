//go:build !windows

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/initca"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/internal/testutil"
	testdaemon "github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/request"
	"github.com/moby/swarmkit/v2/ca"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

var defaultReconciliationTimeout = 30 * time.Second

func (s *DockerSwarmSuite) TestAPISwarmJoinToken(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, false, false)
	d1.SwarmInit(ctx, c, swarm.InitRequest{})

	// todo: error message differs depending if some components of token are valid

	d2 := s.AddDaemon(ctx, c, false, false)
	c2 := d2.NewClientT(c)
	_, err := c2.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(c, err, "join token is necessary")
	info := d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	_, err = c2.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		JoinToken:   "foobaz",
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(c, err, "invalid join token")
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	workerToken := d1.JoinTokens(c).Worker

	d2.SwarmJoin(ctx, c, swarm.JoinRequest{
		ListenAddr:  d2.SwarmListenAddr(),
		JoinToken:   workerToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	assert.NilError(c, d2.SwarmLeave(ctx, c, false))
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	// change tokens
	d1.RotateTokens(c)

	_, err = c2.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		JoinToken:   workerToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(c, err, "join token is necessary")
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	workerToken = d1.JoinTokens(c).Worker

	d2.SwarmJoin(ctx, c, swarm.JoinRequest{JoinToken: workerToken, RemoteAddrs: []string{d1.SwarmListenAddr()}})
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	assert.NilError(c, d2.SwarmLeave(ctx, c, false))
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	// change spec, don't change tokens
	d1.UpdateSwarm(c, func(s *swarm.Spec) {})

	_, err = c2.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(c, err, "join token is necessary")
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	d2.SwarmJoin(ctx, c, swarm.JoinRequest{JoinToken: workerToken, RemoteAddrs: []string{d1.SwarmListenAddr()}})
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	assert.NilError(c, d2.SwarmLeave(ctx, c, false))
	info = d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)
}

func (s *DockerSwarmSuite) TestUpdateSwarmAddExternalCA(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, false, false)
	d1.SwarmInit(ctx, c, swarm.InitRequest{})
	d1.UpdateSwarm(c, func(s *swarm.Spec) {
		s.CAConfig.ExternalCAs = []*swarm.ExternalCA{
			{
				Protocol: swarm.ExternalCAProtocolCFSSL,
				URL:      "https://thishasnoca.org",
			},
			{
				Protocol: swarm.ExternalCAProtocolCFSSL,
				URL:      "https://thishasacacert.org",
				CACert:   "cacert",
			},
		}
	})
	info := d1.SwarmInfo(ctx, c)
	assert.Equal(c, len(info.Cluster.Spec.CAConfig.ExternalCAs), 2)
	assert.Equal(c, info.Cluster.Spec.CAConfig.ExternalCAs[0].CACert, "")
	assert.Equal(c, info.Cluster.Spec.CAConfig.ExternalCAs[1].CACert, "cacert")
}

func (s *DockerSwarmSuite) TestAPISwarmPromoteDemote(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, false, false)
	d1.SwarmInit(ctx, c, swarm.InitRequest{})
	d2 := s.AddDaemon(ctx, c, true, false)

	info := d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.ControlAvailable, false)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)

	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Role = swarm.NodeRoleManager
	})

	poll.WaitOn(c, pollCheck(c, d2.CheckControlAvailable(ctx), checker.True()), poll.WithTimeout(defaultReconciliationTimeout))

	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Role = swarm.NodeRoleWorker
	})

	poll.WaitOn(c, pollCheck(c, d2.CheckControlAvailable(ctx), checker.False()), poll.WithTimeout(defaultReconciliationTimeout))

	// Wait for the role to change to worker in the cert. This is partially
	// done because it's something worth testing in its own right, and
	// partially because changing the role from manager to worker and then
	// back to manager quickly might cause the node to pause for awhile
	// while waiting for the role to change to worker, and the test can
	// time out during this interval.
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		certBytes, err := os.ReadFile(filepath.Join(d2.Folder, "root", "swarm", "certificates", "swarm-node.crt"))
		if err != nil {
			return "", fmt.Sprintf("error: %v", err)
		}
		certs, err := helpers.ParseCertificatesPEM(certBytes)
		if err == nil && len(certs) > 0 && len(certs[0].Subject.OrganizationalUnit) > 0 {
			return certs[0].Subject.OrganizationalUnit[0], ""
		}
		return "", "could not get organizational unit from certificate"
	}, checker.Equals("swarm-worker")), poll.WithTimeout(defaultReconciliationTimeout))

	// Demoting last node should fail
	node := d1.GetNode(ctx, c, d1.NodeID())
	node.Spec.Role = swarm.NodeRoleWorker
	url := fmt.Sprintf("/nodes/%s/update?version=%d", node.ID, node.Version.Index)
	res, body, err := request.Post(testutil.GetContext(c), url, request.Host(d1.Sock()), request.JSONBody(node.Spec))
	assert.NilError(c, err)
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusBadRequest, "output: %q", string(b))

	// The warning specific to demoting the last manager is best-effort and
	// won't appear until the Role field of the demoted manager has been
	// updated.
	// Yes, I know this looks silly, but checker.Matches is broken, since
	// it anchors the regexp contrary to the documentation, and this makes
	// it impossible to match something that includes a line break.
	if !strings.Contains(string(b), "last manager of the swarm") {
		assert.Assert(c, is.Contains(string(b), "this would result in a loss of quorum"))
	}
	info = d1.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	assert.Equal(c, info.ControlAvailable, true)

	// Promote already demoted node
	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Role = swarm.NodeRoleManager
	})

	poll.WaitOn(c, pollCheck(c, d2.CheckControlAvailable(ctx), checker.True()), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestAPISwarmLeaderProxy(c *testing.T) {
	ctx := testutil.GetContext(c)
	// add three managers, one of these is leader
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	// start a service by hitting each of the 3 managers
	d1.CreateService(ctx, c, simpleTestService, func(s *swarm.Service) {
		s.Spec.Name = "test1"
	})
	d2.CreateService(ctx, c, simpleTestService, func(s *swarm.Service) {
		s.Spec.Name = "test2"
	})
	d3.CreateService(ctx, c, simpleTestService, func(s *swarm.Service) {
		s.Spec.Name = "test3"
	})

	// 3 services should be started now, because the requests were proxied to leader
	// query each node and make sure it returns 3 services
	for _, d := range []*daemon.Daemon{d1, d2, d3} {
		services := d.ListServices(ctx, c)
		assert.Equal(c, len(services), 3)
	}
}

func (s *DockerSwarmSuite) TestAPISwarmLeaderElection(c *testing.T) {
	ctx := testutil.GetContext(c)
	if runtime.GOARCH == "s390x" {
		c.Skip("Disabled on s390x")
	}
	if runtime.GOARCH == "ppc64le" {
		c.Skip("Disabled on  ppc64le")
	}

	// Create 3 nodes
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	// assert that the first node we made is the leader, and the other two are followers
	assert.Equal(c, d1.GetNode(ctx, c, d1.NodeID()).ManagerStatus.Leader, true)
	assert.Equal(c, d1.GetNode(ctx, c, d2.NodeID()).ManagerStatus.Leader, false)
	assert.Equal(c, d1.GetNode(ctx, c, d3.NodeID()).ManagerStatus.Leader, false)

	d1.Stop(c)

	var (
		leader    *daemon.Daemon   // keep track of leader
		followers []*daemon.Daemon // keep track of followers
	)
	var lastErr error
	checkLeader := func(nodes ...*daemon.Daemon) checkF {
		return func(t *testing.T) (any, string) {
			// clear these out before each run
			leader = nil
			followers = nil
			for _, d := range nodes {
				n := d.GetNode(ctx, t, d.NodeID(), func(err error) bool {
					if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) || strings.Contains(err.Error(), "swarm does not have a leader") {
						lastErr = err
						return true
					}
					return false
				})
				if n == nil {
					return false, fmt.Sprintf("failed to get node: %v", lastErr)
				}
				if n.ManagerStatus.Leader {
					leader = d
				} else {
					followers = append(followers, d)
				}
			}

			if leader == nil {
				return false, "no leader elected"
			}

			return true, fmt.Sprintf("elected %v", leader.ID())
		}
	}

	// wait for an election to occur
	c.Logf("Waiting for election to occur...")
	poll.WaitOn(c, pollCheck(c, checkLeader(d2, d3), checker.True()), poll.WithTimeout(defaultReconciliationTimeout))

	// assert that we have a new leader
	assert.Assert(c, leader != nil)

	// Keep track of the current leader, since we want that to be chosen.
	stableleader := leader

	// add the d1, the initial leader, back
	d1.StartNode(c)

	// wait for possible election
	c.Logf("Waiting for possible election...")
	poll.WaitOn(c, pollCheck(c, checkLeader(d1, d2, d3), checker.True()), poll.WithTimeout(defaultReconciliationTimeout))
	// pick out the leader and the followers again

	// verify that we still only have 1 leader and 2 followers
	assert.Assert(c, leader != nil)
	assert.Equal(c, len(followers), 2)
	// and that after we added d1 back, the leader hasn't changed
	assert.Equal(c, leader.NodeID(), stableleader.NodeID())
}

func (s *DockerSwarmSuite) TestAPISwarmRaftQuorum(c *testing.T) {
	ctx := testutil.GetContext(c)
	if runtime.GOARCH == "s390x" {
		c.Skip("Disabled on s390x")
	}
	if runtime.GOARCH == "ppc64le" {
		c.Skip("Disabled on  ppc64le")
	}

	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	d1.CreateService(ctx, c, simpleTestService)

	d2.Stop(c)

	// make sure there is a leader
	poll.WaitOn(c, pollCheck(c, d1.CheckLeader(ctx), checker.IsNil()), poll.WithTimeout(defaultReconciliationTimeout))

	d1.CreateService(ctx, c, simpleTestService, func(s *swarm.Service) {
		s.Spec.Name = "top1"
	})

	d3.Stop(c)

	var service swarm.Service
	simpleTestService(&service)
	service.Spec.Name = "top2"
	cli := d1.NewClientT(c)
	defer cli.Close()

	// d1 will eventually step down from leader because there is no longer an active quorum, wait for that to happen
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		_, err := cli.ServiceCreate(testutil.GetContext(t), client.ServiceCreateOptions{
			Spec: service.Spec,
		})
		return err.Error(), ""
	}, checker.Contains("Make sure more than half of the managers are online.")), poll.WithTimeout(defaultReconciliationTimeout*2))

	d2.StartNode(c)

	// make sure there is a leader
	poll.WaitOn(c, pollCheck(c, d1.CheckLeader(ctx), checker.IsNil()), poll.WithTimeout(defaultReconciliationTimeout))

	d1.CreateService(ctx, c, simpleTestService, func(s *swarm.Service) {
		s.Spec.Name = "top3"
	})
}

func (s *DockerSwarmSuite) TestAPISwarmLeaveRemovesContainer(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	instances := 2
	d.CreateService(ctx, c, simpleTestService, setInstances(instances))

	id, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, id)
	id = strings.TrimSpace(id)

	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances+1)), poll.WithTimeout(defaultReconciliationTimeout))

	assert.ErrorContains(c, d.SwarmLeave(ctx, c, false), "")
	assert.NilError(c, d.SwarmLeave(ctx, c, true))

	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	id2, err := d.Cmd("ps", "-q")
	assert.NilError(c, err, id2)
	assert.Assert(c, strings.HasPrefix(id, strings.TrimSpace(id2)))
}

// #23629
func (s *DockerSwarmSuite) TestAPISwarmLeaveOnPendingJoin(c *testing.T) {
	testRequires(c, Network)

	ctx := testutil.GetContext(c)
	s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, false, false)

	id, err := d2.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, id)
	id = strings.TrimSpace(id)

	c2 := d2.NewClientT(c)
	_, err = c2.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{"123.123.123.123:1234"},
	})
	assert.ErrorContains(c, err, "Timeout was reached")

	info := d2.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStatePending)

	assert.NilError(c, d2.SwarmLeave(ctx, c, true))

	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	id2, err := d2.Cmd("ps", "-q")
	assert.NilError(c, err, id2)
	assert.Assert(c, strings.HasPrefix(id, strings.TrimSpace(id2)))
}

// #23705
func (s *DockerSwarmSuite) TestAPISwarmRestoreOnPendingJoin(c *testing.T) {
	testRequires(c, Network)

	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)
	cli := d.NewClientT(c)
	_, err := cli.SwarmJoin(testutil.GetContext(c), client.SwarmJoinOptions{
		ListenAddr:  d.SwarmListenAddr(),
		RemoteAddrs: []string{"123.123.123.123:1234"},
	})
	assert.ErrorContains(c, err, "Timeout was reached")

	poll.WaitOn(c, pollCheck(c, d.CheckLocalNodeState(ctx), checker.Equals(swarm.LocalNodeStatePending)), poll.WithTimeout(defaultReconciliationTimeout))

	d.RestartNode(c)

	info := d.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)
}

func (s *DockerSwarmSuite) TestAPISwarmManagerRestore(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)

	instances := 2
	id := d1.CreateService(ctx, c, simpleTestService, setInstances(instances))

	d1.GetService(ctx, c, id)
	d1.RestartNode(c)
	d1.GetService(ctx, c, id)

	d2 := s.AddDaemon(ctx, c, true, true)
	d2.GetService(ctx, c, id)
	d2.RestartNode(c)
	d2.GetService(ctx, c, id)

	d3 := s.AddDaemon(ctx, c, true, true)
	d3.GetService(ctx, c, id)
	d3.RestartNode(c)
	d3.GetService(ctx, c, id)

	err := d3.Kill()
	assert.NilError(c, err)
	d3.StartNode(c)
	d3.GetService(ctx, c, id)
}

func (s *DockerSwarmSuite) TestAPISwarmScaleNoRollingUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	instances := 2
	id := d.CreateService(ctx, c, simpleTestService, setInstances(instances))

	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	containers := d.ActiveContainers(ctx, c)
	instances = 4
	d.UpdateService(ctx, c, d.GetService(ctx, c, id), setInstances(instances))
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	containers2 := d.ActiveContainers(ctx, c)

loop0:
	for _, c1 := range containers {
		for _, c2 := range containers2 {
			if c1 == c2 {
				continue loop0
			}
		}
		c.Errorf("container %v not found in new set %#v", c1, containers2)
	}
}

func (s *DockerSwarmSuite) TestAPISwarmInvalidAddress(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)
	req := swarm.InitRequest{
		ListenAddr: "",
	}
	res, _, err := request.Post(testutil.GetContext(c), "/swarm/init", request.Host(d.Sock()), request.JSONBody(req))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusBadRequest)

	req2 := swarm.JoinRequest{
		ListenAddr:  "0.0.0.0:2377",
		RemoteAddrs: []string{""},
	}
	res, _, err = request.Post(testutil.GetContext(c), "/swarm/join", request.Host(d.Sock()), request.JSONBody(req2))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusBadRequest)
}

func (s *DockerSwarmSuite) TestAPISwarmForceNewCluster(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)

	instances := 2
	id := d1.CreateService(ctx, c, simpleTestService, setInstances(instances))
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	// drain d2, all containers should move to d1
	d1.UpdateNode(ctx, c, d2.NodeID(), func(n *swarm.Node) {
		n.Spec.Availability = swarm.NodeAvailabilityDrain
	})
	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, d2.CheckActiveContainerCount(ctx), checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))

	d2.Stop(c)

	d1.SwarmInit(ctx, c, swarm.InitRequest{
		ForceNewCluster: true,
		Spec:            swarm.Spec{},
	})

	poll.WaitOn(c, pollCheck(c, d1.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	d3 := s.AddDaemon(ctx, c, true, true)
	info := d3.SwarmInfo(ctx, c)
	assert.Equal(c, info.ControlAvailable, true)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)

	instances = 4
	d3.UpdateService(ctx, c, d3.GetService(ctx, c, id), setInstances(instances))

	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d3.CheckActiveContainerCount(ctx)), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
}

func simpleTestService(s *swarm.Service) {
	ureplicas := uint64(1)
	restartDelay := 100 * time.Millisecond

	s.Spec = swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   "busybox:latest",
				Command: []string{"/bin/top"},
			},
			RestartPolicy: &swarm.RestartPolicy{
				Delay: &restartDelay,
			},
		},
		Mode: swarm.ServiceMode{
			Replicated: &swarm.ReplicatedService{
				Replicas: &ureplicas,
			},
		},
	}
	s.Spec.Name = "top"
}

func serviceForUpdate(s *swarm.Service) {
	ureplicas := uint64(1)
	restartDelay := 100 * time.Millisecond

	s.Spec = swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   "busybox:latest",
				Command: []string{"/bin/top"},
			},
			RestartPolicy: &swarm.RestartPolicy{
				Delay: &restartDelay,
			},
		},
		Mode: swarm.ServiceMode{
			Replicated: &swarm.ReplicatedService{
				Replicas: &ureplicas,
			},
		},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   2,
			Delay:         4 * time.Second,
			FailureAction: swarm.UpdateFailureActionContinue,
		},
		RollbackConfig: &swarm.UpdateConfig{
			Parallelism:   3,
			Delay:         4 * time.Second,
			FailureAction: swarm.UpdateFailureActionContinue,
		},
	}
	s.Spec.Name = "updatetest"
}

func setInstances(replicas int) testdaemon.ServiceConstructor {
	ureplicas := uint64(replicas)
	return func(s *swarm.Service) {
		s.Spec.Mode = swarm.ServiceMode{
			Replicated: &swarm.ReplicatedService{
				Replicas: &ureplicas,
			},
		}
	}
}

func setUpdateOrder(order swarm.UpdateOrder) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.UpdateConfig == nil {
			s.Spec.UpdateConfig = &swarm.UpdateConfig{}
		}
		s.Spec.UpdateConfig.Order = order
	}
}

func setRollbackOrder(order swarm.UpdateOrder) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.RollbackConfig == nil {
			s.Spec.RollbackConfig = &swarm.UpdateConfig{}
		}
		s.Spec.RollbackConfig.Order = order
	}
}

func setImage(image string) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.TaskTemplate.ContainerSpec == nil {
			s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
		}
		s.Spec.TaskTemplate.ContainerSpec.Image = image
	}
}

func setFailureAction(failureAction swarm.FailureAction) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		s.Spec.UpdateConfig.FailureAction = failureAction
	}
}

func setMaxFailureRatio(maxFailureRatio float32) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		s.Spec.UpdateConfig.MaxFailureRatio = maxFailureRatio
	}
}

func setParallelism(parallelism uint64) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		s.Spec.UpdateConfig.Parallelism = parallelism
	}
}

func setConstraints(constraints []string) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.TaskTemplate.Placement == nil {
			s.Spec.TaskTemplate.Placement = &swarm.Placement{}
		}
		s.Spec.TaskTemplate.Placement.Constraints = constraints
	}
}

func setPlacementPrefs(prefs []swarm.PlacementPreference) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.TaskTemplate.Placement == nil {
			s.Spec.TaskTemplate.Placement = &swarm.Placement{}
		}
		s.Spec.TaskTemplate.Placement.Preferences = prefs
	}
}

func setGlobalMode(s *swarm.Service) {
	s.Spec.Mode = swarm.ServiceMode{
		Global: &swarm.GlobalService{},
	}
}

func checkClusterHealth(t *testing.T, cl []*daemon.Daemon, managerCount, workerCount int) {
	var totalMCount, totalWCount int

	ctx := testutil.GetContext(t)
	for _, d := range cl {
		var info swarm.Info

		// check info in a poll.WaitOn(), because if the cluster doesn't have a leader, `info` will return an error
		checkInfo := func(t *testing.T) (any, string) {
			apiClient := d.NewClientT(t)
			result, err := apiClient.Info(ctx, client.InfoOptions{})
			info = result.Info.Swarm
			return err, "cluster not ready in time"
		}
		poll.WaitOn(t, pollCheck(t, checkInfo, checker.IsNil()), poll.WithTimeout(defaultReconciliationTimeout))
		if !info.ControlAvailable {
			totalWCount++
			continue
		}

		var leaderFound bool
		totalMCount++
		var mCount, wCount int

		for _, n := range d.ListNodes(ctx, t) {
			waitReady := func(t *testing.T) (any, string) {
				if n.Status.State == swarm.NodeStateReady {
					return true, ""
				}
				nn := d.GetNode(ctx, t, n.ID)
				n = *nn
				return n.Status.State == swarm.NodeStateReady, fmt.Sprintf("state of node %s, reported by %s", n.ID, d.NodeID())
			}
			poll.WaitOn(t, pollCheck(t, waitReady, checker.True()), poll.WithTimeout(defaultReconciliationTimeout))

			waitActive := func(t *testing.T) (any, string) {
				if n.Spec.Availability == swarm.NodeAvailabilityActive {
					return true, ""
				}
				nn := d.GetNode(ctx, t, n.ID)
				n = *nn
				return n.Spec.Availability == swarm.NodeAvailabilityActive, fmt.Sprintf("availability of node %s, reported by %s", n.ID, d.NodeID())
			}
			poll.WaitOn(t, pollCheck(t, waitActive, checker.True()), poll.WithTimeout(defaultReconciliationTimeout))

			if n.Spec.Role == swarm.NodeRoleManager {
				assert.Assert(t, n.ManagerStatus != nil, "manager status of node %s (manager), reported by %s", n.ID, d.NodeID())
				if n.ManagerStatus.Leader {
					leaderFound = true
				}
				mCount++
			} else {
				assert.Assert(t, n.ManagerStatus == nil, "manager status of node %s (worker), reported by %s", n.ID, d.NodeID())
				wCount++
			}
		}
		assert.Equal(t, leaderFound, true, "lack of leader reported by node %s", info.NodeID)
		assert.Equal(t, mCount, managerCount, "managers count reported by node %s", info.NodeID)
		assert.Equal(t, wCount, workerCount, "workers count reported by node %s", info.NodeID)
	}
	assert.Equal(t, totalMCount, managerCount)
	assert.Equal(t, totalWCount, workerCount)
}

func (s *DockerSwarmSuite) TestAPISwarmRestartCluster(c *testing.T) {
	ctx := testutil.GetContext(c)
	mCount, wCount := 5, 1

	var nodes []*daemon.Daemon
	for range mCount {
		manager := s.AddDaemon(ctx, c, true, true)
		info := manager.SwarmInfo(ctx, c)
		assert.Equal(c, info.ControlAvailable, true)
		assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
		nodes = append(nodes, manager)
	}

	for range wCount {
		worker := s.AddDaemon(ctx, c, true, false)
		info := worker.SwarmInfo(ctx, c)
		assert.Equal(c, info.ControlAvailable, false)
		assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
		nodes = append(nodes, worker)
	}

	// stop whole cluster
	{
		var wg sync.WaitGroup
		wg.Add(len(nodes))
		errs := make(chan error, len(nodes))

		for _, d := range nodes {
			go func(daemon *daemon.Daemon) {
				defer wg.Done()
				if err := daemon.StopWithError(); err != nil {
					errs <- err
				}
			}(d)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NilError(c, err)
		}
	}

	// start whole cluster
	{
		var wg sync.WaitGroup
		wg.Add(len(nodes))
		errs := make(chan error, len(nodes))

		for _, d := range nodes {
			go func(daemon *daemon.Daemon) {
				defer wg.Done()
				if err := daemon.StartWithError("--iptables=false", "--ip6tables=false"); err != nil {
					errs <- err
				}
			}(d)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NilError(c, err)
		}
	}

	checkClusterHealth(c, nodes, mCount, wCount)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesUpdateWithName(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	instances := 2
	id := d.CreateService(ctx, c, simpleTestService, setInstances(instances))
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	service := d.GetService(ctx, c, id)
	instances = 5

	setInstances(instances)(service)
	cli := d.NewClientT(c)
	defer cli.Close()
	_, err := cli.ServiceUpdate(ctx, service.Spec.Name, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(c, err)
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))
}

// Unlocking an unlocked swarm results in an error
func (s *DockerSwarmSuite) TestAPISwarmUnlockNotLocked(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	err := d.SwarmUnlock(c, swarm.UnlockRequest{UnlockKey: "wrong-key"})
	assert.ErrorContains(c, err, "swarm is not locked")
}

// #29885
func (s *DockerSwarmSuite) TestAPISwarmErrorHandling(c *testing.T) {
	ctx := testutil.GetContext(c)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", defaultSwarmPort))
	assert.NilError(c, err)
	defer ln.Close()
	d := s.AddDaemon(ctx, c, false, false)
	cli := d.NewClientT(c)
	_, err = cli.SwarmInit(testutil.GetContext(c), client.SwarmInitOptions{
		ListenAddr: d.SwarmListenAddr(),
	})
	assert.ErrorContains(c, err, "address already in use")
}

// Test case for 30178
func (s *DockerSwarmSuite) TestAPISwarmHealthcheckNone(c *testing.T) {
	// Issue #36386 can be a independent one, which is worth further investigation.
	c.Skip("Root cause of Issue #36386 is needed")
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "-d", "overlay", "lb")
	assert.NilError(c, err, out)

	instances := 1
	d.CreateService(ctx, c, simpleTestService, setInstances(instances), func(s *swarm.Service) {
		if s.Spec.TaskTemplate.ContainerSpec == nil {
			s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
		}
		s.Spec.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{}
		s.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
			{Target: "lb"},
		}
	})

	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(instances)), poll.WithTimeout(defaultReconciliationTimeout))

	containers := d.ActiveContainers(testutil.GetContext(c), c)

	out, err = d.Cmd("exec", containers[0], "ping", "-c1", "-W3", "top")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmRepeatedRootRotation(c *testing.T) {
	ctx := testutil.GetContext(c)
	m := s.AddDaemon(ctx, c, true, true)
	w := s.AddDaemon(ctx, c, true, false)

	info := m.SwarmInfo(ctx, c)

	currentTrustRoot := info.Cluster.TLSInfo.TrustRoot

	// rotate multiple times
	for i := range 4 {
		var err error
		var cert, key []byte
		if i%2 != 0 {
			cert, _, key, err = initca.New(&csr.CertificateRequest{
				CN:         "newRoot",
				KeyRequest: csr.NewKeyRequest(),
				CA:         &csr.CAConfig{Expiry: ca.RootCAExpiration},
			})
			assert.NilError(c, err)
		}
		expectedCert := string(cert)
		m.UpdateSwarm(c, func(s *swarm.Spec) {
			s.CAConfig.SigningCACert = expectedCert
			s.CAConfig.SigningCAKey = string(key)
			s.CAConfig.ForceRotate++
		})

		// poll to make sure update succeeds
		var clusterTLSInfo swarm.TLSInfo
		for range 18 {
			info := m.SwarmInfo(ctx, c)

			// the desired CA cert and key is always redacted
			assert.Equal(c, info.Cluster.Spec.CAConfig.SigningCAKey, "")
			assert.Equal(c, info.Cluster.Spec.CAConfig.SigningCACert, "")

			clusterTLSInfo = info.Cluster.TLSInfo

			// if root rotation is done and the trust root has changed, we don't have to poll anymore
			if !info.Cluster.RootRotationInProgress && clusterTLSInfo.TrustRoot != currentTrustRoot {
				break
			}

			// root rotation not done
			time.Sleep(250 * time.Millisecond)
		}
		if cert != nil {
			assert.Equal(c, clusterTLSInfo.TrustRoot, expectedCert)
		}
		// could take another second or two for the nodes to trust the new roots after they've all gotten
		// new TLS certificates
		for range 18 {
			mInfo := m.GetNode(ctx, c, m.NodeID()).Description.TLSInfo
			wInfo := m.GetNode(ctx, c, w.NodeID()).Description.TLSInfo

			if mInfo.TrustRoot == clusterTLSInfo.TrustRoot && wInfo.TrustRoot == clusterTLSInfo.TrustRoot {
				break
			}

			// nodes don't trust root certs yet
			time.Sleep(250 * time.Millisecond)
		}

		assert.DeepEqual(c, m.GetNode(ctx, c, m.NodeID()).Description.TLSInfo, clusterTLSInfo)
		assert.DeepEqual(c, m.GetNode(ctx, c, w.NodeID()).Description.TLSInfo, clusterTLSInfo)
		currentTrustRoot = clusterTLSInfo.TrustRoot
	}
}

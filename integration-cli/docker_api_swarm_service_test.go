// +build !windows

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/integration-cli/daemon"
	testdaemon "github.com/docker/docker/internal/test/daemon"
	"github.com/go-check/check"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

func setPortConfig(portConfig []swarm.PortConfig) testdaemon.ServiceConstructor {
	return func(s *swarm.Service) {
		if s.Spec.EndpointSpec == nil {
			s.Spec.EndpointSpec = &swarm.EndpointSpec{}
		}
		s.Spec.EndpointSpec.Ports = portConfig
	}
}

func (s *DockerSwarmSuite) TestAPIServiceUpdatePort(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// Create a service with a port mapping of 8080:8081.
	portConfig := []swarm.PortConfig{{TargetPort: 8081, PublishedPort: 8080}}
	serviceID := d.CreateService(c, simpleTestService, setInstances(1), setPortConfig(portConfig))
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 1)

	// Update the service: changed the port mapping from 8080:8081 to 8082:8083.
	updatedPortConfig := []swarm.PortConfig{{TargetPort: 8083, PublishedPort: 8082}}
	remoteService := d.GetService(c, serviceID)
	d.UpdateService(c, remoteService, setPortConfig(updatedPortConfig))

	// Inspect the service and verify port mapping.
	updatedService := d.GetService(c, serviceID)
	assert.Assert(c, updatedService.Spec.EndpointSpec != nil)
	assert.Equal(c, len(updatedService.Spec.EndpointSpec.Ports), 1)
	assert.Equal(c, updatedService.Spec.EndpointSpec.Ports[0].TargetPort, uint32(8083))
	assert.Equal(c, updatedService.Spec.EndpointSpec.Ports[0].PublishedPort, uint32(8082))
}

func (s *DockerSwarmSuite) TestAPISwarmServicesEmptyList(c *check.C) {
	d := s.AddDaemon(c, true, true)

	services := d.ListServices(c)
	assert.Assert(c, services != nil)
	assert.Assert(c, len(services) == 0, "services: %#v", services)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesCreate(c *check.C) {
	d := s.AddDaemon(c, true, true)

	instances := 2
	id := d.CreateService(c, simpleTestService, setInstances(instances))
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, instances)

	client := d.NewClientT(c)
	defer client.Close()

	options := types.ServiceInspectOptions{InsertDefaults: true}

	// insertDefaults inserts UpdateConfig when service is fetched by ID
	resp, _, err := client.ServiceInspectWithRaw(context.Background(), id, options)
	out := fmt.Sprintf("%+v", resp)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "UpdateConfig"))

	// insertDefaults inserts UpdateConfig when service is fetched by ID
	resp, _, err = client.ServiceInspectWithRaw(context.Background(), "top", options)
	out = fmt.Sprintf("%+v", resp)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "UpdateConfig"))

	service := d.GetService(c, id)
	instances = 5
	d.UpdateService(c, service, setInstances(instances))
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, instances)

	d.RemoveService(c, service.ID)
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 0)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesMultipleAgents(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, false)
	d3 := s.AddDaemon(c, true, false)

	time.Sleep(1 * time.Second) // make sure all daemons are ready to accept tasks

	instances := 9
	id := d1.CreateService(c, simpleTestService, setInstances(instances))

	waitAndAssert(c, defaultReconciliationTimeout, d1.CheckActiveContainerCount, checker.GreaterThan, 0)
	waitAndAssert(c, defaultReconciliationTimeout, d2.CheckActiveContainerCount, checker.GreaterThan, 0)
	waitAndAssert(c, defaultReconciliationTimeout, d3.CheckActiveContainerCount, checker.GreaterThan, 0)

	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d2.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

	// reconciliation on d2 node down
	d2.Stop(c)

	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

	// test downscaling
	instances = 5
	d1.UpdateService(c, d1.GetService(c, id), setInstances(instances))
	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

}

func (s *DockerSwarmSuite) TestAPISwarmServicesCreateGlobal(c *check.C) {
	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, false)
	d3 := s.AddDaemon(c, true, false)

	d1.CreateService(c, simpleTestService, setGlobalMode)

	waitAndAssert(c, defaultReconciliationTimeout, d1.CheckActiveContainerCount, checker.Equals, 1)
	waitAndAssert(c, defaultReconciliationTimeout, d2.CheckActiveContainerCount, checker.Equals, 1)
	waitAndAssert(c, defaultReconciliationTimeout, d3.CheckActiveContainerCount, checker.Equals, 1)

	d4 := s.AddDaemon(c, true, false)
	d5 := s.AddDaemon(c, true, false)

	waitAndAssert(c, defaultReconciliationTimeout, d4.CheckActiveContainerCount, checker.Equals, 1)
	waitAndAssert(c, defaultReconciliationTimeout, d5.CheckActiveContainerCount, checker.Equals, 1)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesUpdate(c *check.C) {
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(c, true, i == 0)
	}
	// wait for nodes ready
	waitAndAssert(c, 5*time.Second, daemons[0].CheckNodeReadyCount, checker.Equals, nodeCount)

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "busybox:test"

	// create a different tag
	for _, d := range daemons {
		out, err := d.Cmd("tag", image1, image2)
		assert.NilError(c, err, out)
	}

	// create service
	instances := 5
	parallelism := 2
	rollbackParallelism := 3
	id := daemons[0].CreateService(c, serviceForUpdate, setInstances(instances))

	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})

	// issue service update
	service := daemons[0].GetService(c, id)
	daemons[0].UpdateService(c, service, setImage(image2))

	// first batch
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - parallelism, image2: parallelism})

	// 2nd batch
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})

	// 3nd batch
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image2: instances})

	// Roll back to the previous version. This uses the CLI because
	// rollback used to be a client-side operation.
	out, err := daemons[0].Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	// first batch
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image2: instances - rollbackParallelism, image1: rollbackParallelism})

	// 2nd batch
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})
}

func (s *DockerSwarmSuite) TestAPISwarmServicesUpdateStartFirst(c *check.C) {
	d := s.AddDaemon(c, true, true)

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "testhealth:latest"

	// service started from this image won't pass health check
	result := cli.BuildCmd(c, image2, cli.Daemon(d),
		build.WithDockerfile(`FROM busybox
		HEALTHCHECK --interval=1s --timeout=30s --retries=1024 \
		  CMD cat /status`),
	)
	result.Assert(c, icmd.Success)

	// create service
	instances := 5
	parallelism := 2
	rollbackParallelism := 3
	id := d.CreateService(c, serviceForUpdate, setInstances(instances), setUpdateOrder(swarm.UpdateOrderStartFirst), setRollbackOrder(swarm.UpdateOrderStartFirst))

	checkStartingTasks := func(expected int) []swarm.Task {
		var startingTasks []swarm.Task
		waitAndAssert(c, defaultReconciliationTimeout, func(c *check.C) (interface{}, check.CommentInterface) {
			tasks := d.GetServiceTasks(c, id)
			startingTasks = nil
			for _, t := range tasks {
				if t.Status.State == swarm.TaskStateStarting {
					startingTasks = append(startingTasks, t)
				}
			}
			return startingTasks, nil
		}, checker.HasLen, expected)

		return startingTasks
	}

	makeTasksHealthy := func(tasks []swarm.Task) {
		for _, t := range tasks {
			containerID := t.Status.ContainerStatus.ContainerID
			d.Cmd("exec", containerID, "touch", "/status")
		}
	}

	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})

	// issue service update
	service := d.GetService(c, id)
	d.UpdateService(c, service, setImage(image2))

	// first batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks := checkStartingTasks(parallelism)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})

	// make it healthy
	makeTasksHealthy(startingTasks)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - parallelism, image2: parallelism})

	// 2nd batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks = checkStartingTasks(parallelism)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - parallelism, image2: parallelism})

	// make it healthy
	makeTasksHealthy(startingTasks)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})

	// 3nd batch

	// The old tasks should be running, and the new ones should be starting.
	startingTasks = checkStartingTasks(1)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances - 2*parallelism, image2: 2 * parallelism})

	// make it healthy
	makeTasksHealthy(startingTasks)

	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image2: instances})

	// Roll back to the previous version. This uses the CLI because
	// rollback is a client-side operation.
	out, err := d.Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	// first batch
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image2: instances - rollbackParallelism, image1: rollbackParallelism})

	// 2nd batch
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})
}

func (s *DockerSwarmSuite) TestAPISwarmServicesFailedUpdate(c *check.C) {
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(c, true, i == 0)
	}
	// wait for nodes ready
	waitAndAssert(c, 5*time.Second, daemons[0].CheckNodeReadyCount, checker.Equals, nodeCount)

	// service image at start
	image1 := "busybox:latest"
	// target image in update
	image2 := "busybox:badtag"

	// create service
	instances := 5
	id := daemons[0].CreateService(c, serviceForUpdate, setInstances(instances))

	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})

	// issue service update
	service := daemons[0].GetService(c, id)
	daemons[0].UpdateService(c, service, setImage(image2), setFailureAction(swarm.UpdateFailureActionPause), setMaxFailureRatio(0.25), setParallelism(1))

	// should update 2 tasks and then pause
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceUpdateState(id), checker.Equals, swarm.UpdateStatePaused)
	v, _ := daemons[0].CheckServiceRunningTasks(id)(c)
	assert.Assert(c, v == instances-2)

	// Roll back to the previous version. This uses the CLI because
	// rollback used to be a client-side operation.
	out, err := daemons[0].Cmd("service", "update", "--detach", "--rollback", id)
	assert.NilError(c, err, out)

	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckRunningTaskImages, checker.DeepEquals,
		map[string]int{image1: instances})
}

func (s *DockerSwarmSuite) TestAPISwarmServiceConstraintRole(c *check.C) {
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(c, true, i == 0)
	}
	// wait for nodes ready
	waitAndAssert(c, 5*time.Second, daemons[0].CheckNodeReadyCount, checker.Equals, nodeCount)

	// create service
	constraints := []string{"node.role==worker"}
	instances := 3
	id := daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	// validate tasks are running on worker nodes
	tasks := daemons[0].GetServiceTasks(c, id)
	for _, task := range tasks {
		node := daemons[0].GetNode(c, task.NodeID)
		assert.Equal(c, node.Spec.Role, swarm.NodeRoleWorker)
	}
	//remove service
	daemons[0].RemoveService(c, id)

	// create service
	constraints = []string{"node.role!=worker"}
	id = daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	tasks = daemons[0].GetServiceTasks(c, id)
	// validate tasks are running on manager nodes
	for _, task := range tasks {
		node := daemons[0].GetNode(c, task.NodeID)
		assert.Equal(c, node.Spec.Role, swarm.NodeRoleManager)
	}
	//remove service
	daemons[0].RemoveService(c, id)

	// create service
	constraints = []string{"node.role==nosuchrole"}
	id = daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceTasks(id), checker.Equals, instances)
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	// validate tasks are not assigned to any node
	tasks = daemons[0].GetServiceTasks(c, id)
	for _, task := range tasks {
		assert.Equal(c, task.NodeID, "")
	}
}

func (s *DockerSwarmSuite) TestAPISwarmServiceConstraintLabel(c *check.C) {
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(c, true, i == 0)
	}
	// wait for nodes ready
	waitAndAssert(c, 5*time.Second, daemons[0].CheckNodeReadyCount, checker.Equals, nodeCount)
	nodes := daemons[0].ListNodes(c)
	assert.Equal(c, len(nodes), nodeCount)

	// add labels to nodes
	daemons[0].UpdateNode(c, nodes[0].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"security": "high",
		}
	})
	for i := 1; i < nodeCount; i++ {
		daemons[0].UpdateNode(c, nodes[i].ID, func(n *swarm.Node) {
			n.Spec.Annotations.Labels = map[string]string{
				"security": "low",
			}
		})
	}

	// create service
	instances := 3
	constraints := []string{"node.labels.security==high"}
	id := daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	tasks := daemons[0].GetServiceTasks(c, id)
	// validate all tasks are running on nodes[0]
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == nodes[0].ID)
	}
	//remove service
	daemons[0].RemoveService(c, id)

	// create service
	constraints = []string{"node.labels.security!=high"}
	id = daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	tasks = daemons[0].GetServiceTasks(c, id)
	// validate all tasks are NOT running on nodes[0]
	for _, task := range tasks {
		assert.Assert(c, task.NodeID != nodes[0].ID)
	}
	//remove service
	daemons[0].RemoveService(c, id)

	constraints = []string{"node.labels.security==medium"}
	id = daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceTasks(id), checker.Equals, instances)
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	tasks = daemons[0].GetServiceTasks(c, id)
	// validate tasks are not assigned
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == "")
	}
	//remove service
	daemons[0].RemoveService(c, id)

	// multiple constraints
	constraints = []string{
		"node.labels.security==high",
		fmt.Sprintf("node.id==%s", nodes[1].ID),
	}
	id = daemons[0].CreateService(c, simpleTestService, setConstraints(constraints), setInstances(instances))
	// wait for tasks created
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceTasks(id), checker.Equals, instances)
	// let scheduler try
	time.Sleep(250 * time.Millisecond)
	tasks = daemons[0].GetServiceTasks(c, id)
	// validate tasks are not assigned
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == "")
	}
	// make nodes[1] fulfills the constraints
	daemons[0].UpdateNode(c, nodes[1].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"security": "high",
		}
	})
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	tasks = daemons[0].GetServiceTasks(c, id)
	for _, task := range tasks {
		assert.Assert(c, task.NodeID == nodes[1].ID)
	}
}

func (s *DockerSwarmSuite) TestAPISwarmServicePlacementPrefs(c *check.C) {
	const nodeCount = 3
	var daemons [nodeCount]*daemon.Daemon
	for i := 0; i < nodeCount; i++ {
		daemons[i] = s.AddDaemon(c, true, i == 0)
	}
	// wait for nodes ready
	waitAndAssert(c, 5*time.Second, daemons[0].CheckNodeReadyCount, checker.Equals, nodeCount)
	nodes := daemons[0].ListNodes(c)
	assert.Equal(c, len(nodes), nodeCount)

	// add labels to nodes
	daemons[0].UpdateNode(c, nodes[0].ID, func(n *swarm.Node) {
		n.Spec.Annotations.Labels = map[string]string{
			"rack": "a",
		}
	})
	for i := 1; i < nodeCount; i++ {
		daemons[0].UpdateNode(c, nodes[i].ID, func(n *swarm.Node) {
			n.Spec.Annotations.Labels = map[string]string{
				"rack": "b",
			}
		})
	}

	// create service
	instances := 4
	prefs := []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.rack"}}}
	id := daemons[0].CreateService(c, simpleTestService, setPlacementPrefs(prefs), setInstances(instances))
	// wait for tasks ready
	waitAndAssert(c, defaultReconciliationTimeout, daemons[0].CheckServiceRunningTasks(id), checker.Equals, instances)
	tasks := daemons[0].GetServiceTasks(c, id)
	// validate all tasks are running on nodes[0]
	tasksOnNode := make(map[string]int)
	for _, task := range tasks {
		tasksOnNode[task.NodeID]++
	}
	assert.Assert(c, tasksOnNode[nodes[0].ID] == 2)
	assert.Assert(c, tasksOnNode[nodes[1].ID] == 1)
	assert.Assert(c, tasksOnNode[nodes[2].ID] == 1)
}

func (s *DockerSwarmSuite) TestAPISwarmServicesStateReporting(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon)
	testRequires(c, DaemonIsLinux)

	d1 := s.AddDaemon(c, true, true)
	d2 := s.AddDaemon(c, true, true)
	d3 := s.AddDaemon(c, true, false)

	time.Sleep(1 * time.Second) // make sure all daemons are ready to accept

	instances := 9
	d1.CreateService(c, simpleTestService, setInstances(instances))

	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d2.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

	getContainers := func() map[string]*daemon.Daemon {
		m := make(map[string]*daemon.Daemon)
		for _, d := range []*daemon.Daemon{d1, d2, d3} {
			for _, id := range d.ActiveContainers(c) {
				m[id] = d
			}
		}
		return m
	}

	containers := getContainers()
	assert.Assert(c, len(containers) == instances)
	var toRemove string
	for i := range containers {
		toRemove = i
	}

	_, err := containers[toRemove].Cmd("stop", toRemove)
	assert.NilError(c, err)

	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d2.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

	containers2 := getContainers()
	assert.Assert(c, len(containers2) == instances)
	for i := range containers {
		if i == toRemove {
			assert.Assert(c, containers2[i] == nil)
		} else {
			assert.Assert(c, containers2[i] != nil)
		}
	}

	containers = containers2
	for i := range containers {
		toRemove = i
	}

	// try with killing process outside of docker
	pidStr, err := containers[toRemove].Cmd("inspect", "-f", "{{.State.Pid}}", toRemove)
	assert.NilError(c, err)
	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	assert.NilError(c, err)
	assert.NilError(c, unix.Kill(pid, unix.SIGKILL))

	time.Sleep(time.Second) // give some time to handle the signal

	waitAndAssert(c, defaultReconciliationTimeout, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount, d2.CheckActiveContainerCount, d3.CheckActiveContainerCount), checker.Equals, instances)

	containers2 = getContainers()
	assert.Assert(c, len(containers2) == instances)
	for i := range containers {
		if i == toRemove {
			assert.Assert(c, containers2[i] == nil)
		} else {
			assert.Assert(c, containers2[i] != nil)
		}
	}
}

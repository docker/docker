package service // import "github.com/docker/docker/integration/service"

import (
	"context"
	"fmt"
	"io/ioutil"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestServiceCreateInit(t *testing.T) {
	defer setupTest(t)()
	t.Run("daemonInitDisabled", testServiceCreateInit(false))
	t.Run("daemonInitEnabled", testServiceCreateInit(true))
}

func testServiceCreateInit(daemonEnabled bool) func(t *testing.T) {
	return func(t *testing.T) {
		var ops = []daemon.Option{}

		if daemonEnabled {
			ops = append(ops, daemon.WithInit)
		}
		d := swarm.NewSwarm(t, testEnv, ops...)
		defer d.Stop(t)
		client := d.NewClientT(t)
		defer client.Close()

		booleanTrue := true
		booleanFalse := false

		serviceID := swarm.CreateService(t, d)
		poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, 1), swarm.ServicePoll)
		i := inspectServiceContainer(t, client, serviceID)
		// HostConfig.Init == nil means that it delegates to daemon configuration
		assert.Check(t, i.HostConfig.Init == nil)

		serviceID = swarm.CreateService(t, d, swarm.ServiceWithInit(&booleanTrue))
		poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, 1), swarm.ServicePoll)
		i = inspectServiceContainer(t, client, serviceID)
		assert.Check(t, is.Equal(true, *i.HostConfig.Init))

		serviceID = swarm.CreateService(t, d, swarm.ServiceWithInit(&booleanFalse))
		poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, 1), swarm.ServicePoll)
		i = inspectServiceContainer(t, client, serviceID)
		assert.Check(t, is.Equal(false, *i.HostConfig.Init))
	}
}

func inspectServiceContainer(t *testing.T, client client.APIClient, serviceID string) types.ContainerJSON {
	t.Helper()
	filter := filters.NewArgs()
	filter.Add("label", fmt.Sprintf("com.docker.swarm.service.id=%s", serviceID))
	containers, err := client.ContainerList(context.Background(), types.ContainerListOptions{Filters: filter})
	assert.NilError(t, err)
	assert.Check(t, is.Len(containers, 1))

	i, err := client.ContainerInspect(context.Background(), containers[0].ID)
	assert.NilError(t, err)
	return i
}

func TestCreateServiceMultipleTimes(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	overlayName := "overlay1_" + t.Name()
	overlayID := network.CreateNoError(ctx, t, client, overlayName,
		network.WithCheckDuplicate(),
		network.WithDriver("overlay"),
	)

	var instances uint64 = 4

	serviceName := "TestService_" + t.Name()
	serviceSpec := []swarm.ServiceSpecOpt{
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(overlayName),
	}

	serviceID := swarm.CreateService(t, d, serviceSpec...)
	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, instances), swarm.ServicePoll)

	_, _, err := client.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)

	err = client.ServiceRemove(context.Background(), serviceID)
	assert.NilError(t, err)

	poll.WaitOn(t, swarm.NoTasksForService(ctx, client, serviceID), swarm.ServicePoll)

	serviceID2 := swarm.CreateService(t, d, serviceSpec...)
	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID2, instances), swarm.ServicePoll)

	err = client.ServiceRemove(context.Background(), serviceID2)
	assert.NilError(t, err)

	poll.WaitOn(t, swarm.NoTasksForService(ctx, client, serviceID2), swarm.ServicePoll)

	err = client.NetworkRemove(context.Background(), overlayID)
	assert.NilError(t, err)

	poll.WaitOn(t, network.IsRemoved(context.Background(), client, overlayID), poll.WithTimeout(1*time.Minute), poll.WithDelay(10*time.Second))
}

func TestCreateServiceConflict(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	serviceName := "TestService_" + t.Name()
	serviceSpec := []swarm.ServiceSpecOpt{
		swarm.ServiceWithName(serviceName),
	}

	swarm.CreateService(t, d, serviceSpec...)

	spec := swarm.CreateServiceSpec(t, serviceSpec...)
	_, err := c.ServiceCreate(ctx, spec, types.ServiceCreateOptions{})
	assert.Check(t, errdefs.IsConflict(err))
	assert.ErrorContains(t, err, "service "+serviceName+" already exists")
}

func TestCreateServiceMaxReplicas(t *testing.T) {
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()

	var maxReplicas uint64 = 2
	serviceSpec := []swarm.ServiceSpecOpt{
		swarm.ServiceWithReplicas(maxReplicas),
		swarm.ServiceWithMaxReplicas(maxReplicas),
	}

	serviceID := swarm.CreateService(t, d, serviceSpec...)
	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, maxReplicas), swarm.ServicePoll)

	_, _, err := client.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)
}

func TestCreateWithDuplicateNetworkNames(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	name := "foo_" + t.Name()
	n1 := network.CreateNoError(ctx, t, client, name, network.WithDriver("bridge"))
	n2 := network.CreateNoError(ctx, t, client, name, network.WithDriver("bridge"))

	// Duplicates with name but with different driver
	n3 := network.CreateNoError(ctx, t, client, name, network.WithDriver("overlay"))

	// Create Service with the same name
	var instances uint64 = 1

	serviceName := "top_" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(name),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, instances), swarm.ServicePoll)

	resp, _, err := client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(n3, resp.Spec.TaskTemplate.Networks[0].Target))

	// Remove Service, and wait for its tasks to be removed
	err = client.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	poll.WaitOn(t, swarm.NoTasksForService(ctx, client, serviceID), swarm.ServicePoll)

	// Remove networks
	err = client.NetworkRemove(context.Background(), n3)
	assert.NilError(t, err)

	err = client.NetworkRemove(context.Background(), n2)
	assert.NilError(t, err)

	err = client.NetworkRemove(context.Background(), n1)
	assert.NilError(t, err)

	// Make sure networks have been destroyed.
	poll.WaitOn(t, network.IsRemoved(context.Background(), client, n3), poll.WithTimeout(1*time.Minute), poll.WithDelay(10*time.Second))
	poll.WaitOn(t, network.IsRemoved(context.Background(), client, n2), poll.WithTimeout(1*time.Minute), poll.WithDelay(10*time.Second))
	poll.WaitOn(t, network.IsRemoved(context.Background(), client, n1), poll.WithTimeout(1*time.Minute), poll.WithDelay(10*time.Second))
}

func TestCreateServiceSecretFileMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()

	ctx := context.Background()
	secretName := "TestSecret_" + t.Name()
	secretResp, err := client.SecretCreate(ctx, swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: secretName,
		},
		Data: []byte("TESTSECRET"),
	})
	assert.NilError(t, err)

	var instances uint64 = 1
	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithCommand([]string{"/bin/sh", "-c", "ls -l /etc/secret || /bin/top"}),
		swarm.ServiceWithSecret(&swarmtypes.SecretReference{
			File: &swarmtypes.SecretReferenceFileTarget{
				Name: "/etc/secret",
				UID:  "0",
				GID:  "0",
				Mode: 0777,
			},
			SecretID:   secretResp.ID,
			SecretName: secretName,
		}),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, instances), swarm.ServicePoll)

	filter := filters.NewArgs()
	filter.Add("service", serviceID)
	tasks, err := client.TaskList(ctx, types.TaskListOptions{
		Filters: filter,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(tasks), 1))

	body, err := client.ContainerLogs(ctx, tasks[0].Status.ContainerStatus.ContainerID, types.ContainerLogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)
	defer body.Close()

	content, err := ioutil.ReadAll(body)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(string(content), "-rwxrwxrwx"))

	err = client.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	poll.WaitOn(t, swarm.NoTasksForService(ctx, client, serviceID), swarm.ServicePoll)

	err = client.SecretRemove(ctx, secretName)
	assert.NilError(t, err)
}

func TestCreateServiceConfigFileMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()

	ctx := context.Background()
	configName := "TestConfig_" + t.Name()
	configResp, err := client.ConfigCreate(ctx, swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name: configName,
		},
		Data: []byte("TESTCONFIG"),
	})
	assert.NilError(t, err)

	var instances uint64 = 1
	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithCommand([]string{"/bin/sh", "-c", "ls -l /etc/config || /bin/top"}),
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithConfig(&swarmtypes.ConfigReference{
			File: &swarmtypes.ConfigReferenceFileTarget{
				Name: "/etc/config",
				UID:  "0",
				GID:  "0",
				Mode: 0777,
			},
			ConfigID:   configResp.ID,
			ConfigName: configName,
		}),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, instances))

	filter := filters.NewArgs()
	filter.Add("service", serviceID)
	tasks, err := client.TaskList(ctx, types.TaskListOptions{
		Filters: filter,
	})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(tasks), 1))

	body, err := client.ContainerLogs(ctx, tasks[0].Status.ContainerStatus.ContainerID, types.ContainerLogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)
	defer body.Close()

	content, err := ioutil.ReadAll(body)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(string(content), "-rwxrwxrwx"))

	err = client.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	poll.WaitOn(t, swarm.NoTasksForService(ctx, client, serviceID))

	err = client.ConfigRemove(ctx, configName)
	assert.NilError(t, err)
}

// TestServiceCreateSysctls tests that a service created with sysctl options in
// the ContainerSpec correctly applies those options.
//
// To test this, we're going to create a service with the sysctl option
//
//   {"net.ipv4.ip_nonlocal_bind": "0"}
//
// We'll get the service's tasks to get the container ID, and then we'll
// inspect the container. If the output of the container inspect contains the
// sysctl option with the correct value, we can assume that the sysctl has been
// plumbed correctly.
//
// Next, we'll remove that service and create a new service with that option
// set to 1. This means that no matter what the default is, we can be confident
// that the sysctl option is applying as intended.
//
// Additionally, we'll do service and task inspects to verify that the inspect
// output includes the desired sysctl option.
//
// We're using net.ipv4.ip_nonlocal_bind because it's something that I'm fairly
// confident won't be modified by the container runtime, and won't blow
// anything up in the test environment
func TestCreateServiceSysctls(t *testing.T) {
	skip.If(
		t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"),
		"setting service sysctls is unsupported before api v1.40",
	)

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()

	ctx := context.Background()

	// run thie block twice, so that no matter what the default value of
	// net.ipv4.ip_nonlocal_bind is, we can verify that setting the sysctl
	// options works
	for _, expected := range []string{"0", "1"} {

		// store the map we're going to be using everywhere.
		expectedSysctls := map[string]string{"net.ipv4.ip_nonlocal_bind": expected}

		// Create the service with the sysctl options
		var instances uint64 = 1
		serviceID := swarm.CreateService(t, d,
			swarm.ServiceWithSysctls(expectedSysctls),
		)

		// wait for the service to converge to 1 running task as expected
		poll.WaitOn(t, swarm.RunningTasksCount(client, serviceID, instances))

		// we're going to check 3 things:
		//
		//   1. Does the container, when inspected, have the sysctl option set?
		//   2. Does the task have the sysctl in the spec?
		//   3. Does the service have the sysctl in the spec?
		//
		// if all 3 of these things are true, we know that the sysctl has been
		// plumbed correctly through the engine.
		//
		// We don't actually have to get inside the container and check its
		// logs or anything. If we see the sysctl set on the container inspect,
		// we know that the sysctl is plumbed correctly. everything below that
		// level has been tested elsewhere. (thanks @thaJeztah, because an
		// earlier version of this test had to get container logs and was much
		// more complex)

		// get all of the tasks of the service, so we can get the container
		filter := filters.NewArgs()
		filter.Add("service", serviceID)
		tasks, err := client.TaskList(ctx, types.TaskListOptions{
			Filters: filter,
		})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(len(tasks), 1))

		// verify that the container has the sysctl option set
		ctnr, err := client.ContainerInspect(ctx, tasks[0].Status.ContainerStatus.ContainerID)
		assert.NilError(t, err)
		assert.DeepEqual(t, ctnr.HostConfig.Sysctls, expectedSysctls)

		// verify that the task has the sysctl option set in the task object
		assert.DeepEqual(t, tasks[0].Spec.ContainerSpec.Sysctls, expectedSysctls)

		// verify that the service also has the sysctl set in the spec.
		service, _, err := client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
		assert.NilError(t, err)
		assert.DeepEqual(t,
			service.Spec.TaskTemplate.ContainerSpec.Sysctls, expectedSysctls,
		)
	}
}

package service

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestServiceUpdatePort(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	// Create a service with a port mapping of 8080:8081.
	serviceName := "TestServiceUpdatePort_" + t.Name()
	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithReplicas(1),
		swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{
			Ports: []swarmtypes.PortConfig{
				{TargetPort: 8081, PublishedPort: 8080},
			},
		}),
	)
	defer func() {
		_, err := apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
	}()

	poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, serviceID, 1), swarm.ServicePoll)

	// Update the service: changed the port mapping from 8080:8081 to 8082:8083.
	service := getService(ctx, t, apiClient, serviceID)
	service.Spec.EndpointSpec.Ports = []swarmtypes.PortConfig{
		{TargetPort: 8083, PublishedPort: 8082},
	}
	_, err := apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)

	// Inspect the service and verify port mapping.
	updatedService := getService(ctx, t, apiClient, serviceID)
	assert.Assert(t, updatedService.Spec.EndpointSpec != nil)
	assert.Equal(t, len(updatedService.Spec.EndpointSpec.Ports), 1)
	assert.Equal(t, updatedService.Spec.EndpointSpec.Ports[0].TargetPort, uint32(8083))
	assert.Equal(t, updatedService.Spec.EndpointSpec.Ports[0].PublishedPort, uint32(8082))
}

func TestServiceUpdateLabel(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(ctx, t, d, swarm.ServiceWithName(serviceName))
	service := getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{}))

	// add label to empty set
	service.Spec.Labels["foo"] = "bar"
	_, err := apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	// add label to non-empty set
	service.Spec.Labels["foo2"] = "bar"
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar", "foo2": "bar"}))

	delete(service.Spec.Labels, "foo2")
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	delete(service.Spec.Labels, "foo")
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{}))

	// now make sure we can add again
	service.Spec.Labels["foo"] = "bar"
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceSpecIsUpdated(ctx, apiClient, serviceID, service.Version.Index), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	_, err = apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
	assert.NilError(t, err)
}

func TestServiceUpdateSecrets(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	secretName := "TestSecret_" + t.Name()
	secretTarget := "targetName"
	secretResp, err := apiClient.SecretCreate(ctx, client.SecretCreateOptions{
		Spec: swarmtypes.SecretSpec{
			Annotations: swarmtypes.Annotations{
				Name: secretName,
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(t, err)
	assert.Check(t, secretResp.ID != "")

	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(ctx, t, d, swarm.ServiceWithName(serviceName))
	// Avoid racing the update with the initial task startup.
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, serviceID, 1), swarm.ServicePoll)
	service := getService(ctx, t, apiClient, serviceID)

	// add secret
	service.Spec.TaskTemplate.ContainerSpec.Secrets = append(service.Spec.TaskTemplate.ContainerSpec.Secrets,
		&swarmtypes.SecretReference{
			File: &swarmtypes.SecretReferenceFileTarget{
				Name: secretTarget,
				UID:  "0",
				GID:  "0",
				Mode: 0o600,
			},
			SecretID:   secretResp.ID,
			SecretName: secretName,
		},
	)
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)

	service = getService(ctx, t, apiClient, serviceID)
	secrets := service.Spec.TaskTemplate.ContainerSpec.Secrets
	assert.Assert(t, is.Equal(1, len(secrets)))

	secret := *secrets[0]
	assert.Check(t, is.Equal(secretName, secret.SecretName))
	assert.Check(t, secret.File != nil)
	assert.Check(t, is.Equal(secretTarget, secret.File.Name))

	// remove
	service.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarmtypes.SecretReference{}
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.Equal(0, len(service.Spec.TaskTemplate.ContainerSpec.Secrets)))

	_, err = apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
	assert.NilError(t, err)
}

func TestServiceUpdateConfigs(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	configName := "TestConfig_" + t.Name()
	configTarget := "targetName"
	resp, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{
		Spec: swarmtypes.ConfigSpec{
			Annotations: swarmtypes.Annotations{
				Name: configName,
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(t, err)
	assert.Check(t, resp.ID != "")

	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(ctx, t, d, swarm.ServiceWithName(serviceName))
	// Avoid racing the update with the initial task startup.
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, serviceID, 1), swarm.ServicePoll)
	service := getService(ctx, t, apiClient, serviceID)

	// add config
	service.Spec.TaskTemplate.ContainerSpec.Configs = append(service.Spec.TaskTemplate.ContainerSpec.Configs,
		&swarmtypes.ConfigReference{
			File: &swarmtypes.ConfigReferenceFileTarget{
				Name: configTarget,
				UID:  "0",
				GID:  "0",
				Mode: 0o600,
			},
			ConfigID:   resp.ID,
			ConfigName: configName,
		},
	)
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)

	service = getService(ctx, t, apiClient, serviceID)
	configs := service.Spec.TaskTemplate.ContainerSpec.Configs
	assert.Assert(t, is.Equal(1, len(configs)))

	config := *configs[0]
	assert.Check(t, is.Equal(configName, config.ConfigName))
	assert.Check(t, config.File != nil)
	assert.Check(t, is.Equal(configTarget, config.File.Name))

	// remove
	service.Spec.TaskTemplate.ContainerSpec.Configs = []*swarmtypes.ConfigReference{}
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)
	service = getService(ctx, t, apiClient, serviceID)
	assert.Check(t, is.Equal(0, len(service.Spec.TaskTemplate.ContainerSpec.Configs)))

	_, err = apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
	assert.NilError(t, err)
}

func TestServiceUpdateNetwork(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	// Create a overlay network
	testNet := "testNet" + t.Name()
	overlayID := network.CreateNoError(ctx, t, apiClient, testNet,
		network.WithDriver("overlay"))

	var instances uint64 = 1
	// Create service with the overlay network
	serviceName := "TestServiceUpdateNetworkRM_" + t.Name()
	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(testNet))

	poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, serviceID, instances), swarm.ServicePoll)
	service := getService(ctx, t, apiClient, serviceID)
	netInfo, err := apiClient.NetworkInspect(ctx, testNet, client.NetworkInspectOptions{
		Verbose: true,
		Scope:   "swarm",
	})
	assert.NilError(t, err)
	assert.Assert(t, len(netInfo.Network.Containers) == 2, "Expected 2 endpoints, one for container and one for LB Sandbox")

	// Remove network from service
	service.Spec.TaskTemplate.Networks = []swarmtypes.NetworkAttachmentConfig{}
	_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: service.Version,
		Spec:    service.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)

	netInfo, err = apiClient.NetworkInspect(ctx, testNet, client.NetworkInspectOptions{
		Verbose: true,
		Scope:   "swarm",
	})

	assert.NilError(t, err)
	assert.Assert(t, len(netInfo.Network.Containers) == 0, "Load balancing endpoint still exists in network")

	_, err = apiClient.NetworkRemove(ctx, overlayID, client.NetworkRemoveOptions{})
	assert.NilError(t, err)

	_, err = apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
	assert.NilError(t, err)
}

// TestServiceUpdatePidsLimit tests creating and updating a service with PidsLimit
func TestServiceUpdatePidsLimit(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	tests := []struct {
		name      string
		pidsLimit int64
		expected  int64
	}{
		{
			name:      "create service with PidsLimit 300",
			pidsLimit: 300,
			expected:  300,
		},
		{
			name:      "unset PidsLimit to 0",
			pidsLimit: 0,
			expected:  0,
		},
		{
			name:      "update PidsLimit to 100",
			pidsLimit: 100,
			expected:  100,
		},
	}

	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer func() { _ = apiClient.Close() }()
	var (
		serviceID string
		service   swarmtypes.Service
	)
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			if i == 0 {
				serviceID = swarm.CreateService(ctx, t, d, swarm.ServiceWithPidsLimit(tc.pidsLimit))
			} else {
				service = getService(ctx, t, apiClient, serviceID)
				if service.Spec.TaskTemplate.Resources == nil {
					service.Spec.TaskTemplate.Resources = &swarmtypes.ResourceRequirements{}
				}
				if service.Spec.TaskTemplate.Resources.Limits == nil {
					service.Spec.TaskTemplate.Resources.Limits = &swarmtypes.Limit{}
				}
				service.Spec.TaskTemplate.Resources.Limits.Pids = tc.pidsLimit
				_, err := apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
					Version: service.Version,
					Spec:    service.Spec,
				})
				assert.NilError(t, err)
				poll.WaitOn(t, serviceIsUpdated(ctx, apiClient, serviceID), swarm.ServicePoll)
			}

			poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, serviceID, 1), swarm.ServicePoll)
			service = getService(ctx, t, apiClient, serviceID)
			ctr := getServiceTaskContainer(ctx, t, apiClient, serviceID)
			assert.Equal(t, service.Spec.TaskTemplate.Resources.Limits.Pids, tc.expected)
			if tc.expected == 0 {
				if ctr.HostConfig.Resources.PidsLimit != nil {
					t.Fatalf("Expected container.HostConfig.Resources.PidsLimit to be nil")
				}
			} else {
				assert.Assert(t, ctr.HostConfig.Resources.PidsLimit != nil)
				assert.Equal(t, *ctr.HostConfig.Resources.PidsLimit, tc.expected)
			}
		})
	}

	_, err := apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
	assert.NilError(t, err)
}

func getServiceTaskContainer(ctx context.Context, t *testing.T, cli client.APIClient, serviceID string) container.InspectResponse {
	t.Helper()
	taskList, err := cli.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("service", serviceID).Add("desired-state", "running"),
	})
	assert.NilError(t, err)
	assert.Assert(t, len(taskList.Items) > 0)

	inspect, err := cli.ContainerInspect(ctx, taskList.Items[0].Status.ContainerStatus.ContainerID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, inspect.Container.State.Running, true)
	return inspect.Container
}

func getService(ctx context.Context, t *testing.T, apiClient client.ServiceAPIClient, serviceID string) swarmtypes.Service {
	t.Helper()
	result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	assert.NilError(t, err)
	return result.Service
}

func serviceIsUpdated(ctx context.Context, apiClient client.ServiceAPIClient, serviceID string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err != nil {
			return poll.Error(err)
		}
		service := result.Service
		switch {
		case service.UpdateStatus != nil && service.UpdateStatus.State == swarmtypes.UpdateStateCompleted:
			return poll.Success()
		default:
			if service.UpdateStatus != nil {
				return poll.Continue("waiting for service %s to be updated, state: %s, message: %s", serviceID, service.UpdateStatus.State, service.UpdateStatus.Message)
			}
			return poll.Continue("waiting for service %s to be updated", serviceID)
		}
	}
}

func serviceSpecIsUpdated(ctx context.Context, apiClient client.ServiceAPIClient, serviceID string, serviceOldVersion uint64) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		switch {
		case err != nil:
			return poll.Error(err)
		case result.Service.Version.Index > serviceOldVersion:
			return poll.Success()
		default:
			return poll.Continue("waiting for service %s to be updated", serviceID)
		}
	}
}

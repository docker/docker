package service // import "github.com/docker/docker/integration/service"

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestServiceUpdateLabel(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	cli := d.NewClientT(t)
	defer cli.Close()

	ctx := context.Background()
	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(t, d, swarm.ServiceWithName(serviceName))
	service := getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{}))

	// add label to empty set
	service.Spec.Labels["foo"] = "bar"
	_, err := cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	// add label to non-empty set
	service.Spec.Labels["foo2"] = "bar"
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar", "foo2": "bar"}))

	delete(service.Spec.Labels, "foo2")
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	delete(service.Spec.Labels, "foo")
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{}))

	// now make sure we can add again
	service.Spec.Labels["foo"] = "bar"
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.DeepEqual(service.Spec.Labels, map[string]string{"foo": "bar"}))

	err = cli.ServiceRemove(context.Background(), serviceID)
	assert.NilError(t, err)
}

func TestServiceUpdateSecrets(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	cli := d.NewClientT(t)
	defer cli.Close()

	ctx := context.Background()
	secretName := "TestSecret_" + t.Name()
	secretTarget := "targetName"
	resp, err := cli.SecretCreate(ctx, swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: secretName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.NilError(t, err)
	assert.Check(t, resp.ID != "")

	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(t, d, swarm.ServiceWithName(serviceName))
	service := getService(t, cli, serviceID)

	// add secret
	service.Spec.TaskTemplate.ContainerSpec.Secrets = append(service.Spec.TaskTemplate.ContainerSpec.Secrets,
		&swarmtypes.SecretReference{
			File: &swarmtypes.SecretReferenceFileTarget{
				Name: secretTarget,
				UID:  "0",
				GID:  "0",
				Mode: 0600,
			},
			SecretID:   resp.ID,
			SecretName: secretName,
		},
	)
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)

	service = getService(t, cli, serviceID)
	secrets := service.Spec.TaskTemplate.ContainerSpec.Secrets
	assert.Assert(t, is.Equal(1, len(secrets)))

	secret := *secrets[0]
	assert.Check(t, is.Equal(secretName, secret.SecretName))
	assert.Check(t, nil != secret.File)
	assert.Check(t, is.Equal(secretTarget, secret.File.Name))

	// remove
	service.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarmtypes.SecretReference{}
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.Equal(0, len(service.Spec.TaskTemplate.ContainerSpec.Secrets)))

	err = cli.ServiceRemove(context.Background(), serviceID)
	assert.NilError(t, err)
}

func TestServiceUpdateConfigs(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	cli := d.NewClientT(t)
	defer cli.Close()

	ctx := context.Background()
	configName := "TestConfig_" + t.Name()
	configTarget := "targetName"
	resp, err := cli.ConfigCreate(ctx, swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name: configName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.NilError(t, err)
	assert.Check(t, resp.ID != "")

	serviceName := "TestService_" + t.Name()
	serviceID := swarm.CreateService(t, d, swarm.ServiceWithName(serviceName))
	service := getService(t, cli, serviceID)

	// add config
	service.Spec.TaskTemplate.ContainerSpec.Configs = append(service.Spec.TaskTemplate.ContainerSpec.Configs,
		&swarmtypes.ConfigReference{
			File: &swarmtypes.ConfigReferenceFileTarget{
				Name: configTarget,
				UID:  "0",
				GID:  "0",
				Mode: 0600,
			},
			ConfigID:   resp.ID,
			ConfigName: configName,
		},
	)
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)

	service = getService(t, cli, serviceID)
	configs := service.Spec.TaskTemplate.ContainerSpec.Configs
	assert.Assert(t, is.Equal(1, len(configs)))

	config := *configs[0]
	assert.Check(t, is.Equal(configName, config.ConfigName))
	assert.Check(t, nil != config.File)
	assert.Check(t, is.Equal(configTarget, config.File.Name))

	// remove
	service.Spec.TaskTemplate.ContainerSpec.Configs = []*swarmtypes.ConfigReference{}
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)
	service = getService(t, cli, serviceID)
	assert.Check(t, is.Equal(0, len(service.Spec.TaskTemplate.ContainerSpec.Configs)))

	err = cli.ServiceRemove(context.Background(), serviceID)
	assert.NilError(t, err)
}

func TestServiceUpdateNetwork(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	cli := d.NewClientT(t)
	defer cli.Close()

	ctx := context.Background()

	// Create a overlay network
	testNet := "testNet" + t.Name()
	overlayID := network.CreateNoError(t, ctx, cli, testNet,
		network.WithDriver("overlay"))

	var instances uint64 = 1
	// Create service with the overlay network
	serviceName := "TestServiceUpdateNetworkRM_" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(testNet))

	poll.WaitOn(t, swarm.RunningTasksCount(cli, serviceID, instances), swarm.ServicePoll)
	service := getService(t, cli, serviceID)
	netInfo, err := cli.NetworkInspect(ctx, testNet, types.NetworkInspectOptions{
		Verbose: true,
		Scope:   "swarm",
	})
	assert.NilError(t, err)
	assert.Assert(t, len(netInfo.Containers) == 2, "Expected 2 endpoints, one for container and one for LB Sandbox")

	//Remove network from service
	service.Spec.TaskTemplate.Networks = []swarmtypes.NetworkAttachmentConfig{}
	_, err = cli.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(cli, serviceID), swarm.ServicePoll)

	netInfo, err = cli.NetworkInspect(ctx, testNet, types.NetworkInspectOptions{
		Verbose: true,
		Scope:   "swarm",
	})

	assert.NilError(t, err)
	assert.Assert(t, len(netInfo.Containers) == 0, "Load balancing endpoint still exists in network")

	err = cli.NetworkRemove(ctx, overlayID)
	assert.NilError(t, err)

	err = cli.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func getService(t *testing.T, cli client.ServiceAPIClient, serviceID string) swarmtypes.Service {
	t.Helper()
	service, _, err := cli.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)
	return service
}

func serviceIsUpdated(client client.ServiceAPIClient, serviceID string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		service, _, err := client.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
		switch {
		case err != nil:
			return poll.Error(err)
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

package swarm

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/test/daemon"
	"github.com/docker/docker/internal/test/environment"
	"gotest.tools/assert"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

// ServicePoll tweaks the pollSettings for `service`
func ServicePoll(config *poll.Settings) {
	// Override the default pollSettings for `service` resource here ...
	config.Timeout = 30 * time.Second
	config.Delay = 100 * time.Millisecond
	if runtime.GOARCH == "arm64" || runtime.GOARCH == "arm" {
		config.Timeout = 90 * time.Second
	}
}

// NetworkPoll tweaks the pollSettings for `network`
func NetworkPoll(config *poll.Settings) {
	// Override the default pollSettings for `network` resource here ...
	config.Timeout = 30 * time.Second
	config.Delay = 100 * time.Millisecond

	if runtime.GOARCH == "arm64" || runtime.GOARCH == "arm" {
		config.Timeout = 50 * time.Second
	}
}

// ContainerPoll tweaks the pollSettings for `container`
func ContainerPoll(config *poll.Settings) {
	// Override the default pollSettings for `container` resource here ...

	if runtime.GOARCH == "arm64" || runtime.GOARCH == "arm" {
		config.Timeout = 30 * time.Second
		config.Delay = 100 * time.Millisecond
	}
}

// NewSwarm creates a swarm daemon for testing
func NewSwarm(t *testing.T, testEnv *environment.Execution, ops ...func(*daemon.Daemon)) *daemon.Daemon {
	t.Helper()
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	if testEnv.DaemonInfo.ExperimentalBuild {
		ops = append(ops, daemon.WithExperimental)
	}
	d := daemon.New(t, ops...)
	d.StartAndSwarmInit(t)
	return d
}

// ServiceSpecOpt is used with `CreateService` to pass in service spec modifiers
type ServiceSpecOpt func(*swarmtypes.ServiceSpec)

// CreateService creates a service on the passed in swarm daemon.
func CreateService(t *testing.T, d *daemon.Daemon, opts ...ServiceSpecOpt) string {
	t.Helper()

	client := d.NewClientT(t)
	defer client.Close()

	spec := CreateServiceSpec(t, opts...)
	resp, err := client.ServiceCreate(context.Background(), spec, types.ServiceCreateOptions{})
	assert.NilError(t, err, "error creating service")
	return resp.ID
}

// CreateServiceSpec creates a default service-spec, and applies the provided options
func CreateServiceSpec(t *testing.T, opts ...ServiceSpecOpt) swarmtypes.ServiceSpec {
	t.Helper()
	var spec swarmtypes.ServiceSpec
	ServiceWithImage("busybox:latest")(&spec)
	ServiceWithCommand([]string{"/bin/top"})(&spec)
	ServiceWithReplicas(1)(&spec)

	for _, o := range opts {
		o(&spec)
	}
	return spec
}

// ServiceWithInit sets whether the service should use init or not
func ServiceWithInit(b *bool) func(*swarmtypes.ServiceSpec) {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Init = b
	})
}

// ServiceWithImage sets the image to use for the service
func ServiceWithImage(image string) func(*swarmtypes.ServiceSpec) {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Image = image
	})
}

// ServiceWithCommand sets the command to use for the service
func ServiceWithCommand(cmd []string) ServiceSpecOpt {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Command = cmd
	})
}

// ServiceWithConfig adds the config reference to the service
func ServiceWithConfig(configRef *swarmtypes.ConfigReference) ServiceSpecOpt {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Configs = append(spec.TaskTemplate.ContainerSpec.Configs, configRef)
	})
}

// ServiceWithSecret adds the secret reference to the service
func ServiceWithSecret(secretRef *swarmtypes.SecretReference) ServiceSpecOpt {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Secrets = append(spec.TaskTemplate.ContainerSpec.Secrets, secretRef)
	})
}

// ServiceWithReplicas sets the replicas for the service
func ServiceWithReplicas(n uint64) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		spec.Mode = swarmtypes.ServiceMode{
			Replicated: &swarmtypes.ReplicatedService{
				Replicas: &n,
			},
		}
	}
}

// ServiceWithMaxReplicas sets the max replicas for the service
func ServiceWithMaxReplicas(n uint64) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		ensurePlacement(spec)
		spec.TaskTemplate.Placement.MaxReplicas = n
	}
}

// ServiceWithName sets the name of the service
func ServiceWithName(name string) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		spec.Annotations.Name = name
	}
}

// ServiceWithNetwork sets the network of the service
func ServiceWithNetwork(network string) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks,
			swarmtypes.NetworkAttachmentConfig{Target: network})
	}
}

// ServiceWithEndpoint sets the Endpoint of the service
func ServiceWithEndpoint(endpoint *swarmtypes.EndpointSpec) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		spec.EndpointSpec = endpoint
	}
}

// ServiceWithSysctls sets the Sysctls option of the service's ContainerSpec.
func ServiceWithSysctls(sysctls map[string]string) ServiceSpecOpt {
	return withEnsureContainerSpec(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.ContainerSpec.Sysctls = sysctls
	})
}

// ServiceWithMemorySwap sets the memory swap value of the service's TaskSpec
// it also requires setting a memory limit, since the engine enforces that
func ServiceWithMemorySwap(swap *int64, memoryLimit int64) ServiceSpecOpt {
	return withEnsureResourceLimits(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.Resources.SwapBytes = swap
		spec.TaskTemplate.Resources.Limits.MemoryBytes = memoryLimit
	})
}

// ServiceWithMemorySwappiness sets the memory swappiness value of the service's ContainerSpec
func ServiceWithMemorySwappiness(swappiness *int64) ServiceSpecOpt {
	return withEnsureResourceLimits(func(spec *swarmtypes.ServiceSpec) {
		spec.TaskTemplate.Resources.MemorySwappiness = swappiness
	})
}

// GetRunningTasks gets the list of running tasks for a service
func GetRunningTasks(t *testing.T, c client.ServiceAPIClient, serviceID string) []swarmtypes.Task {
	t.Helper()

	tasks, err := c.TaskList(context.Background(), types.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", serviceID),
			filters.Arg("desired-state", "running"),
		),
	})

	assert.NilError(t, err)
	return tasks
}

// ExecTask runs the passed in exec config on the given task
func ExecTask(t *testing.T, d *daemon.Daemon, task swarmtypes.Task, config types.ExecConfig) types.HijackedResponse {
	t.Helper()
	client := d.NewClientT(t)
	defer client.Close()

	ctx := context.Background()
	resp, err := client.ContainerExecCreate(ctx, task.Status.ContainerStatus.ContainerID, config)
	assert.NilError(t, err, "error creating exec")

	startCheck := types.ExecStartCheck{}
	attach, err := client.ContainerExecAttach(ctx, resp.ID, startCheck)
	assert.NilError(t, err, "error attaching to exec")
	return attach
}

func withEnsureContainerSpec(modifier ServiceSpecOpt) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		if spec.TaskTemplate.ContainerSpec == nil {
			spec.TaskTemplate.ContainerSpec = &swarmtypes.ContainerSpec{}
		}
		modifier(spec)
	}
}

func withEnsureResources(modifier ServiceSpecOpt) ServiceSpecOpt {
	return func(spec *swarmtypes.ServiceSpec) {
		if spec.TaskTemplate.Resources == nil {
			spec.TaskTemplate.Resources = &swarmtypes.ResourceRequirements{}
		}
		modifier(spec)
	}
}

func withEnsureResourceLimits(modifier ServiceSpecOpt) ServiceSpecOpt {
	return withEnsureResources(func(spec *swarmtypes.ServiceSpec) {
		if spec.TaskTemplate.Resources.Limits == nil {
			spec.TaskTemplate.Resources.Limits = &swarmtypes.Resources{}
		}
		modifier(spec)
	})
}

func ensurePlacement(spec *swarmtypes.ServiceSpec) {
	if spec.TaskTemplate.Placement == nil {
		spec.TaskTemplate.Placement = &swarmtypes.Placement{}
	}
}

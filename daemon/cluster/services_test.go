package cluster

import (
	types "github.com/docker/docker/api/types/swarm"
	"gotest.tools/v3/assert"
	"testing"
)

func TestValidateServiceSpecWithPublishedPortsAndHostNetwork(t *testing.T) {
	portConfig := types.PortConfig{}
	ports := make([]types.PortConfig, 1)
	ports[0] = portConfig
	e := types.EndpointSpec{
		Ports: ports,
	}
	task := types.TaskSpec{}
	networks := make([]types.NetworkAttachmentConfig, 1)
	networks[0].Target = "host"
	task.Networks = networks
	s := types.ServiceSpec{
		EndpointSpec: &e,
		TaskTemplate: task,
	}
	err := validateServiceSpec(&s)
	assert.Error(t, err, "cannot bind ports in host network mode")
}

func TestValidateServiceSpecWithPublishedPorts(t *testing.T) {
	portConfig := types.PortConfig{}
	ports := make([]types.PortConfig, 1)
	ports[0] = portConfig
	e := types.EndpointSpec{
		Ports: ports,
	}
	task := types.TaskSpec{}
	s := types.ServiceSpec{
		EndpointSpec: &e,
		TaskTemplate: task,
	}
	err := validateServiceSpec(&s)
	assert.NilError(t, err)
}

func TestValidateServiceSpecWithHostNetwork(t *testing.T) {
	e := types.EndpointSpec{}
	task := types.TaskSpec{}
	networks := make([]types.NetworkAttachmentConfig, 1)
	networks[0].Target = "host"
	task.Networks = networks
	s := types.ServiceSpec{
		EndpointSpec: &e,
		TaskTemplate: task,
	}
	err := validateServiceSpec(&s)
	assert.NilError(t, err)
}

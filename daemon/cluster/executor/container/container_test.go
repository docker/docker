package container // import "github.com/docker/docker/daemon/cluster/executor/container"

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	swarmapi "github.com/docker/swarmkit/api"
	"gotest.tools/v3/assert"
)

func TestIsolationConversion(t *testing.T) {
	cases := []struct {
		name string
		from swarmapi.ContainerSpec_Isolation
		to   container.Isolation
	}{
		{name: "default", from: swarmapi.ContainerIsolationDefault, to: container.IsolationDefault},
		{name: "process", from: swarmapi.ContainerIsolationProcess, to: container.IsolationProcess},
		{name: "hyperv", from: swarmapi.ContainerIsolationHyperV, to: container.IsolationHyperV},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Image:     "alpine:latest",
							Isolation: c.from,
						},
					},
				},
			}
			config := containerConfig{task: &task}
			assert.Equal(t, c.to, config.hostConfig().Isolation)
		})
	}
}

func TestContainerLabels(t *testing.T) {
	c := &containerConfig{
		task: &swarmapi.Task{
			ID: "real-task.id",
			Spec: swarmapi.TaskSpec{
				Runtime: &swarmapi.TaskSpec_Container{
					Container: &swarmapi.ContainerSpec{
						Labels: map[string]string{
							"com.docker.swarm.task":          "user-specified-task",
							"com.docker.swarm.task.id":       "user-specified-task.id",
							"com.docker.swarm.task.name":     "user-specified-task.name",
							"com.docker.swarm.node.id":       "user-specified-node.id",
							"com.docker.swarm.node.hostname": "user-specified-node.hostname",
							"com.docker.swarm.service.id":    "user-specified-service.id",
							"com.docker.swarm.service.name":  "user-specified-service.name",
							"this-is-a-user-label":           "this is a user label's value",
						},
					},
				},
			},
			ServiceID: "real-service.id",
			Slot:      123,
			NodeID:    "real-node.id",
			Annotations: swarmapi.Annotations{
				Name: "real-service.name.123.real-task.id",
			},
			ServiceAnnotations: swarmapi.Annotations{
				Name: "real-service.name",
			},
		},
		node: &swarmapi.NodeDescription{
			Hostname: "real-node.hostname",
		},
	}

	expected := map[string]string{
		"com.docker.swarm.task":          "",
		"com.docker.swarm.task.id":       "real-task.id",
		"com.docker.swarm.task.name":     "real-service.name.123.real-task.id",
		"com.docker.swarm.node.id":       "real-node.id",
		"com.docker.swarm.node.hostname": "real-node.hostname",
		"com.docker.swarm.service.id":    "real-service.id",
		"com.docker.swarm.service.name":  "real-service.name",
		"this-is-a-user-label":           "this is a user label's value",
	}

	labels := c.labels()
	assert.DeepEqual(t, expected, labels)
}

func TestCredentialSpecConversion(t *testing.T) {
	cases := []struct {
		name string
		from swarmapi.Privileges_CredentialSpec
		to   []string
	}{
		{
			name: "none",
			from: swarmapi.Privileges_CredentialSpec{},
			to:   nil,
		},
		{
			name: "config",
			from: swarmapi.Privileges_CredentialSpec{
				Source: &swarmapi.Privileges_CredentialSpec_Config{Config: "0bt9dmxjvjiqermk6xrop3ekq"},
			},
			to: []string{"credentialspec=config://0bt9dmxjvjiqermk6xrop3ekq"},
		},
		{
			name: "file",
			from: swarmapi.Privileges_CredentialSpec{
				Source: &swarmapi.Privileges_CredentialSpec_File{File: "foo.json"},
			},
			to: []string{"credentialspec=file://foo.json"},
		},
		{
			name: "registry",
			from: swarmapi.Privileges_CredentialSpec{
				Source: &swarmapi.Privileges_CredentialSpec_Registry{Registry: "testing"},
			},
			to: []string{"credentialspec=registry://testing"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Privileges: &swarmapi.Privileges{
								CredentialSpec: &c.from,
							},
						},
					},
				},
			}
			config := containerConfig{task: &task}
			assert.DeepEqual(t, c.to, config.hostConfig().SecurityOpt)
		})
	}
}

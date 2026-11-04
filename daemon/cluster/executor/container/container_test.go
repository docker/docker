package container

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestIsolationConversion(t *testing.T) {
	tests := []struct {
		name string
		from swarmapi.ContainerSpec_Isolation
		to   container.Isolation
	}{
		{name: "default", from: swarmapi.ContainerIsolationDefault, to: container.IsolationDefault},
		{name: "process", from: swarmapi.ContainerIsolationProcess, to: container.IsolationProcess},
		{name: "hyperv", from: swarmapi.ContainerIsolationHyperV, to: container.IsolationHyperV},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Image:     "alpine:latest",
							Isolation: tc.from,
						},
					},
				},
			}
			config := containerConfig{task: &task}
			// NOTE(dperny): you shouldn't ever pass nil outside of testing,
			// because if there are CSI volumes, the code will panic. However,
			// in testing. this is acceptable.
			assert.Equal(t, tc.to, config.hostConfig(nil).Isolation)
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
							"com.docker.swarm.task":         "user-specified-task",
							"com.docker.swarm.task.id":      "user-specified-task.id",
							"com.docker.swarm.task.name":    "user-specified-task.name",
							"com.docker.swarm.node.id":      "user-specified-node.id",
							"com.docker.swarm.service.id":   "user-specified-service.id",
							"com.docker.swarm.service.name": "user-specified-service.name",
							"this-is-a-user-label":          "this is a user label's value",
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
	}

	expected := map[string]string{
		"com.docker.swarm.task":         "",
		"com.docker.swarm.task.id":      "real-task.id",
		"com.docker.swarm.task.name":    "real-service.name.123.real-task.id",
		"com.docker.swarm.node.id":      "real-node.id",
		"com.docker.swarm.service.id":   "real-service.id",
		"com.docker.swarm.service.name": "real-service.name",
		"this-is-a-user-label":          "this is a user label's value",
	}

	labels := c.labels()
	assert.DeepEqual(t, expected, labels)
}

func TestExtraHostsConversion(t *testing.T) {
	tests := []struct {
		name string
		from []string
		to   []string
	}{
		{name: "single name", from: []string{"1.2.3.4 foo.example.com"}, to: []string{"foo.example.com:1.2.3.4"}},
		{name: "aliases", from: []string{"1.1.1.1 host1 host2"}, to: []string{"host1 host2:1.1.1.1"}},
		{name: "ipv6 with aliases", from: []string{"2001:db8::1 host1 host2"}, to: []string{"host1 host2:2001:db8::1"}},
		{name: "extra whitespace", from: []string{"  1.1.1.1\thost1   host2  "}, to: []string{"host1 host2:1.1.1.1"}},
		{name: "ip without hostname is skipped", from: []string{"10.0.0.1"}, to: nil},
		{name: "multiple entries", from: []string{"1.1.1.1 host1 host2", "2.2.2.2 host3"}, to: []string{"host1 host2:1.1.1.1", "host3:2.2.2.2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Image: "alpine:latest",
							Hosts: tc.from,
						},
					},
				},
			}
			config := containerConfig{task: &task}
			assert.DeepEqual(t, tc.to, config.hostConfig(nil).ExtraHosts)
		})
	}
}

func TestCredentialSpecConversion(t *testing.T) {
	tests := []struct {
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Privileges: &swarmapi.Privileges{
								CredentialSpec: &tc.from,
							},
						},
					},
				},
			}
			config := containerConfig{task: &task}
			// NOTE(dperny): you shouldn't ever pass nil outside of testing,
			// because if there are CSI volumes, the code will panic. However,
			// in testing. this is acceptable.
			assert.DeepEqual(t, tc.to, config.hostConfig(nil).SecurityOpt)
		})
	}
}

func TestTmpfsConversion(t *testing.T) {
	tests := []struct {
		name string
		from []swarmapi.Mount
		to   []mount.Mount
	}{
		{
			name: "tmpfs-exec",
			from: []swarmapi.Mount{
				{
					Source: "/foo",
					Target: "/bar",
					Type:   swarmapi.MountTypeTmpfs,
					TmpfsOptions: &swarmapi.Mount_TmpfsOptions{
						Options: "[[\"exec\"]]",
					},
				},
			},
			to: []mount.Mount{
				{
					Source: "/foo",
					Target: "/bar",
					Type:   mount.TypeTmpfs,
					TmpfsOptions: &mount.TmpfsOptions{
						Options: [][]string{{"exec"}},
					},
				},
			},
		},
		{
			name: "tmpfs-noexec",
			from: []swarmapi.Mount{
				{
					Source: "/foo",
					Target: "/bar",
					Type:   swarmapi.MountTypeTmpfs,
					TmpfsOptions: &swarmapi.Mount_TmpfsOptions{
						Options: "[[\"noexec\"]]",
					},
				},
			},
			to: []mount.Mount{
				{
					Source: "/foo",
					Target: "/bar",
					Type:   mount.TypeTmpfs,
					TmpfsOptions: &mount.TmpfsOptions{
						Options: [][]string{{"noexec"}},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := swarmapi.Task{
				Spec: swarmapi.TaskSpec{
					Runtime: &swarmapi.TaskSpec_Container{
						Container: &swarmapi.ContainerSpec{
							Image:  "alpine:latest",
							Mounts: tc.from,
						},
					},
				},
			}
			config := containerConfig{task: &task}
			assert.Check(t, is.DeepEqual(tc.to, config.hostConfig(nil).Mounts))
		})
	}
}

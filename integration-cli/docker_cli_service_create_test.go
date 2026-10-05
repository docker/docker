//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

func (s *DockerSwarmSuite) TestServiceCreateMountVolume(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--mount", "type=volume,source=foo,target=/foo,volume-nocopy", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container mount config
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .HostConfig.Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mountConfig []mount.Mount
	assert.NilError(c, json.Unmarshal([]byte(out), &mountConfig))
	assert.Equal(c, len(mountConfig), 1)

	assert.Equal(c, mountConfig[0].Source, "foo")
	assert.Equal(c, mountConfig[0].Target, "/foo")
	assert.Equal(c, mountConfig[0].Type, mount.TypeVolume)
	assert.Assert(c, mountConfig[0].VolumeOptions != nil)
	assert.Assert(c, mountConfig[0].VolumeOptions.NoCopy)

	// check container mounts actual
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mounts []container.MountPoint
	assert.NilError(c, json.Unmarshal([]byte(out), &mounts))
	assert.Equal(c, len(mounts), 1)

	assert.Equal(c, mounts[0].Type, mount.TypeVolume)
	assert.Equal(c, mounts[0].Name, "foo")
	assert.Equal(c, mounts[0].Destination, "/foo")
	assert.Equal(c, mounts[0].RW, true)
}

func (s *DockerSwarmSuite) TestServiceCreateWithSecretSimple(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	apiClient := d.NewClientT(c)

	serviceName := "test-service-secret"
	testName := "test_secret"
	scr, err := apiClient.SecretCreate(ctx, client.SecretCreateOptions{
		Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{
				Name: testName,
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(c, err)
	assert.Assert(c, scr.ID != "", "secrets: %s", scr.ID)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--secret", testName, "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Secrets }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.SecretReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), 1)

	assert.Equal(c, refs[0].SecretName, testName)
	assert.Assert(c, refs[0].File != nil)
	assert.Equal(c, refs[0].File.Name, testName)
	assert.Equal(c, refs[0].File.UID, "0")
	assert.Equal(c, refs[0].File.GID, "0")

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
	_, err = apiClient.SecretRemove(c.Context(), testName, client.SecretRemoveOptions{})
	assert.NilError(c, err)
}

func (s *DockerSwarmSuite) TestServiceCreateWithSecretSourceTargetPaths(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	testPaths := map[string]string{
		"app":                  "/etc/secret",
		"test_secret":          "test_secret",
		"relative_secret":      "relative/secret",
		"escapes_in_container": "../secret",
	}

	var secretFlags []string

	apiClient := d.NewClientT(c)
	for testName, testTarget := range testPaths {
		scr, err := apiClient.SecretCreate(ctx, client.SecretCreateOptions{
			Spec: swarm.SecretSpec{
				Annotations: swarm.Annotations{
					Name: testName,
				},
				Data: []byte("TESTINGDATA " + testName + " " + testTarget),
			},
		})
		assert.NilError(c, err)
		assert.Assert(c, scr.ID != "", "secrets: %s", scr.ID)
		secretFlags = append(secretFlags, "--secret", fmt.Sprintf("source=%s,target=%s", testName, testTarget))
	}

	serviceName := "svc"
	serviceCmd := []string{"service", "create", "--detach", "--no-resolve-image", "--name", serviceName}
	serviceCmd = append(serviceCmd, secretFlags...)
	serviceCmd = append(serviceCmd, "busybox", "top")
	out, err := d.Cmd(serviceCmd...)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Secrets }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.SecretReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), len(testPaths))

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	for testName, testTarget := range testPaths {
		path := testTarget
		if !filepath.IsAbs(path) {
			path = filepath.Join("/run/secrets", path)
		}
		out, err := d.Cmd("exec", task.Status.ContainerStatus.ContainerID, "cat", path)
		assert.NilError(c, err)
		assert.Equal(c, out, "TESTINGDATA "+testName+" "+testTarget)
	}

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestServiceCreateWithSecretReferencedTwice(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	apiClient := d.NewClientT(c)
	scr, err := apiClient.SecretCreate(ctx, client.SecretCreateOptions{
		Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{
				Name: "mysecret",
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(c, err)
	assert.Assert(c, scr.ID != "", "secrets: %s", scr.ID)

	serviceName := "svc"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--secret", "source=mysecret,target=target1", "--secret", "source=mysecret,target=target2", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Secrets }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.SecretReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), 2)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	for _, target := range []string{"target1", "target2"} {
		assert.NilError(c, err, out)
		path := filepath.Join("/run/secrets", target)
		out, err := d.Cmd("exec", task.Status.ContainerStatus.ContainerID, "cat", path)
		assert.NilError(c, err)
		assert.Equal(c, out, "TESTINGDATA")
	}

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestServiceCreateWithConfigSimple(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	apiClient := d.NewClientT(c)

	serviceName := "test-service-config"
	testName := "test_config"
	result, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{
		Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name: testName,
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(c, err)
	assert.Assert(c, result.ID != "", "configs: %s", result.ID)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--config", testName, "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Configs }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.ConfigReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), 1)

	assert.Equal(c, refs[0].ConfigName, testName)
	assert.Assert(c, refs[0].File != nil)
	assert.Equal(c, refs[0].File.Name, testName)
	assert.Equal(c, refs[0].File.UID, "0")
	assert.Equal(c, refs[0].File.GID, "0")

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
	_, err = apiClient.ConfigRemove(ctx, testName, client.ConfigRemoveOptions{})
	assert.NilError(c, err)
}

func (s *DockerSwarmSuite) TestServiceCreateWithConfigSourceTargetPaths(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	testPaths := map[string]string{
		"app":             "/etc/config",
		"test_config":     "test_config",
		"relative_config": "relative/config",
	}

	var configFlags []string

	apiClient := d.NewClientT(c)
	for testName, testTarget := range testPaths {
		result, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{
			Spec: swarm.ConfigSpec{
				Annotations: swarm.Annotations{
					Name: testName,
				},
				Data: []byte("TESTINGDATA " + testName + " " + testTarget),
			},
		})
		assert.NilError(c, err)
		assert.Assert(c, result.ID != "", "configs: %s", result.ID)
		configFlags = append(configFlags, "--config", fmt.Sprintf("source=%s,target=%s", testName, testTarget))
	}

	serviceName := "svc"
	serviceCmd := []string{"service", "create", "--detach", "--no-resolve-image", "--name", serviceName}
	serviceCmd = append(serviceCmd, configFlags...)
	serviceCmd = append(serviceCmd, "busybox", "top")
	out, err := d.Cmd(serviceCmd...)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Configs }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.ConfigReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), len(testPaths))

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	for testName, testTarget := range testPaths {
		path := testTarget
		if !filepath.IsAbs(path) {
			path = filepath.Join("/", path)
		}
		out, err := d.Cmd("exec", task.Status.ContainerStatus.ContainerID, "cat", path)
		assert.NilError(c, err)
		assert.Equal(c, out, "TESTINGDATA "+testName+" "+testTarget)
	}

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestServiceCreateWithConfigReferencedTwice(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	apiClient := d.NewClientT(c)
	result, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{
		Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name: "myconfig",
			},
			Data: []byte("TESTINGDATA"),
		},
	})
	assert.NilError(c, err)
	assert.Assert(c, result.ID != "", "configs: %s", result.ID)

	serviceName := "svc"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--config", "source=myconfig,target=target1", "--config", "source=myconfig,target=target2", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Configs }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.ConfigReference
	assert.NilError(c, json.Unmarshal([]byte(out), &refs))
	assert.Equal(c, len(refs), 2)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	for _, target := range []string{"target1", "target2"} {
		assert.NilError(c, err, out)
		path := filepath.Join("/", target)
		out, err := d.Cmd("exec", task.Status.ContainerStatus.ContainerID, "cat", path)
		assert.NilError(c, err)
		assert.Equal(c, out, "TESTINGDATA")
	}

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestServiceCreateMountTmpfs(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--mount", "type=tmpfs,target=/foo,tmpfs-size=1MB", "busybox", "sh", "-c", "mount | grep foo; exec tail -f /dev/null")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container mount config
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .HostConfig.Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mountConfig []mount.Mount
	assert.NilError(c, json.Unmarshal([]byte(out), &mountConfig))
	assert.Equal(c, len(mountConfig), 1)

	assert.Equal(c, mountConfig[0].Source, "")
	assert.Equal(c, mountConfig[0].Target, "/foo")
	assert.Equal(c, mountConfig[0].Type, mount.TypeTmpfs)
	assert.Assert(c, mountConfig[0].TmpfsOptions != nil)
	assert.Equal(c, mountConfig[0].TmpfsOptions.SizeBytes, int64(1048576))

	// check container mounts actual
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mounts []container.MountPoint
	assert.NilError(c, json.Unmarshal([]byte(out), &mounts))
	assert.Equal(c, len(mounts), 1)

	assert.Equal(c, mounts[0].Type, mount.TypeTmpfs)
	assert.Equal(c, mounts[0].Name, "")
	assert.Equal(c, mounts[0].Destination, "/foo")
	assert.Equal(c, mounts[0].RW, true)

	out, err = s.nodeCmd(c, task.NodeID, "logs", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.HasPrefix(strings.TrimSpace(out), "tmpfs on /foo type tmpfs"))
	assert.Assert(c, is.Contains(strings.TrimSpace(out), "size=1024k"))
}

func (s *DockerSwarmSuite) TestServiceCreateWithNetworkAlias(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	out, err := d.Cmd("network", "create", "--scope=swarm", "test_swarm_br")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--network=name=test_swarm_br,alias=srv_alias", "--name=alias_tst_container", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		tasks = d.GetServiceTasks(ctx, t, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (any, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(ctx, t, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container alias config
	/*
		This works when run manually;

		docker pull busybox
		docker network create --scope=swarm test_swarm_br
		docker service create --no-resolve-image --detach=true --network=name=test_swarm_br,alias=srv_alias --name=alias_tst_container busybox top

		docker ps
		CONTAINER ID   IMAGE            COMMAND                  CREATED          STATUS          PORTS           NAMES
		a6b3a4fec528   busybox:latest   "top"                    24 minutes ago   Up 24 minutes                   alias_tst_container.1.5mfcyrzhq2tui1rtlo99881wp

		docker inspect a6b3a4fec528 --format '{{json .NetworkSettings.Networks.test_swarm_br.DNSNames}}'
		["alias_tst_container.1.5mfcyrzhq2tui1rtlo99881wp","a6b3a4fec528"]
	*/
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .NetworkSettings.Networks}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)
	c.Log(out)

	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .NetworkSettings.Networks.test_swarm_br.DNSNames}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	// Make sure the only alias seen is the container-id
	var dnsNames []string
	assert.NilError(c, json.Unmarshal([]byte(out), &dnsNames))
	assert.Check(c, is.Len(dnsNames, 1), out)
	assert.Assert(c, is.Contains(task.Status.ContainerStatus.ContainerID, dnsNames[0]))
}

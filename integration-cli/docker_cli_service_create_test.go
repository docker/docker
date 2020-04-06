// +build !windows

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/integration-cli/checker"
	"gotest.tools/assert"
	"gotest.tools/poll"
)

func (s *DockerSwarmSuite) TestServiceCreateMountVolume(c *testing.T) {
	d := s.AddDaemon(c, true, true)
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--mount", "type=volume,source=foo,target=/foo,volume-nocopy", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container mount config
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .HostConfig.Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mountConfig []mount.Mount
	assert.Assert(c, json.Unmarshal([]byte(out), &mountConfig) == nil)
	assert.Equal(c, len(mountConfig), 1)

	assert.Equal(c, mountConfig[0].Source, "foo")
	assert.Equal(c, mountConfig[0].Target, "/foo")
	assert.Equal(c, mountConfig[0].Type, mount.TypeVolume)
	assert.Assert(c, mountConfig[0].VolumeOptions != nil)
	assert.Assert(c, mountConfig[0].VolumeOptions.NoCopy)

	// check container mounts actual
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mounts []types.MountPoint
	assert.Assert(c, json.Unmarshal([]byte(out), &mounts) == nil)
	assert.Equal(c, len(mounts), 1)

	assert.Equal(c, mounts[0].Type, mount.TypeVolume)
	assert.Equal(c, mounts[0].Name, "foo")
	assert.Equal(c, mounts[0].Destination, "/foo")
	assert.Equal(c, mounts[0].RW, true)
}

func (s *DockerSwarmSuite) TestServiceCreateWithSecretSimple(c *testing.T) {
	d := s.AddDaemon(c, true, true)

	serviceName := "test-service-secret"
	testName := "test_secret"
	id := d.CreateSecret(c, swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "secrets: %s", id)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--secret", testName, "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Secrets }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.SecretReference
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), 1)

	assert.Equal(c, refs[0].SecretName, testName)
	assert.Assert(c, refs[0].File != nil)
	assert.Equal(c, refs[0].File.Name, testName)
	assert.Equal(c, refs[0].File.UID, "0")
	assert.Equal(c, refs[0].File.GID, "0")

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
	d.DeleteSecret(c, testName)
}

func (s *DockerSwarmSuite) TestServiceCreateWithSecretSourceTargetPaths(c *testing.T) {
	d := s.AddDaemon(c, true, true)

	testPaths := map[string]string{
		"app":                  "/etc/secret",
		"test_secret":          "test_secret",
		"relative_secret":      "relative/secret",
		"escapes_in_container": "../secret",
	}

	var secretFlags []string

	for testName, testTarget := range testPaths {
		id := d.CreateSecret(c, swarm.SecretSpec{
			Annotations: swarm.Annotations{
				Name: testName,
			},
			Data: []byte("TESTINGDATA " + testName + " " + testTarget),
		})
		assert.Assert(c, id != "", "secrets: %s", id)

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
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), len(testPaths))

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
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
	d := s.AddDaemon(c, true, true)

	id := d.CreateSecret(c, swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: "mysecret",
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "secrets: %s", id)

	serviceName := "svc"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--secret", "source=mysecret,target=target1", "--secret", "source=mysecret,target=target2", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Secrets }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.SecretReference
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), 2)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
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
	d := s.AddDaemon(c, true, true)

	serviceName := "test-service-config"
	testName := "test_config"
	id := d.CreateConfig(c, swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "configs: %s", id)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--config", testName, "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Configs }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.ConfigReference
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), 1)

	assert.Equal(c, refs[0].ConfigName, testName)
	assert.Assert(c, refs[0].File != nil)
	assert.Equal(c, refs[0].File.Name, testName)
	assert.Equal(c, refs[0].File.UID, "0")
	assert.Equal(c, refs[0].File.GID, "0")

	out, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err, out)
	d.DeleteConfig(c, testName)
}

func (s *DockerSwarmSuite) TestServiceCreateWithConfigSourceTargetPaths(c *testing.T) {
	d := s.AddDaemon(c, true, true)

	testPaths := map[string]string{
		"app":             "/etc/config",
		"test_config":     "test_config",
		"relative_config": "relative/config",
	}

	var configFlags []string

	for testName, testTarget := range testPaths {
		id := d.CreateConfig(c, swarm.ConfigSpec{
			Annotations: swarm.Annotations{
				Name: testName,
			},
			Data: []byte("TESTINGDATA " + testName + " " + testTarget),
		})
		assert.Assert(c, id != "", "configs: %s", id)

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
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), len(testPaths))

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
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
	d := s.AddDaemon(c, true, true)

	id := d.CreateConfig(c, swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: "myconfig",
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "configs: %s", id)

	serviceName := "svc"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--config", "source=myconfig,target=target1", "--config", "source=myconfig,target=target2", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ json .Spec.TaskTemplate.ContainerSpec.Configs }}", serviceName)
	assert.NilError(c, err)

	var refs []swarm.ConfigReference
	assert.Assert(c, json.Unmarshal([]byte(out), &refs) == nil)
	assert.Equal(c, len(refs), 2)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, serviceName)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
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
	d := s.AddDaemon(c, true, true)
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--mount", "type=tmpfs,target=/foo,tmpfs-size=1MB", "busybox", "sh", "-c", "mount | grep foo; exec tail -f /dev/null")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container mount config
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .HostConfig.Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mountConfig []mount.Mount
	assert.Assert(c, json.Unmarshal([]byte(out), &mountConfig) == nil)
	assert.Equal(c, len(mountConfig), 1)

	assert.Equal(c, mountConfig[0].Source, "")
	assert.Equal(c, mountConfig[0].Target, "/foo")
	assert.Equal(c, mountConfig[0].Type, mount.TypeTmpfs)
	assert.Assert(c, mountConfig[0].TmpfsOptions != nil)
	assert.Equal(c, mountConfig[0].TmpfsOptions.SizeBytes, int64(1048576))

	// check container mounts actual
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .Mounts}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	var mounts []types.MountPoint
	assert.Assert(c, json.Unmarshal([]byte(out), &mounts) == nil)
	assert.Equal(c, len(mounts), 1)

	assert.Equal(c, mounts[0].Type, mount.TypeTmpfs)
	assert.Equal(c, mounts[0].Name, "")
	assert.Equal(c, mounts[0].Destination, "/foo")
	assert.Equal(c, mounts[0].RW, true)

	out, err = s.nodeCmd(c, task.NodeID, "logs", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.HasPrefix(strings.TrimSpace(out), "tmpfs on /foo type tmpfs"))
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "size=1024k"))
}

func (s *DockerSwarmSuite) TestServiceCreateWithNetworkAlias(c *testing.T) {
	d := s.AddDaemon(c, true, true)
	out, err := d.Cmd("network", "create", "--scope=swarm", "test_swarm_br")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--network=name=test_swarm_br,alias=srv_alias", "--name=alias_tst_container", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	var tasks []swarm.Task
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		tasks = d.GetServiceTasks(c, id)
		return len(tasks) > 0, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	task := tasks[0]
	poll.WaitOn(c, pollCheck(c, func(c *testing.T) (interface{}, string) {
		if task.NodeID == "" || task.Status.ContainerStatus == nil {
			task = d.GetTask(c, task.ID)
		}
		return task.NodeID != "" && task.Status.ContainerStatus != nil, ""
	}, checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	// check container alias config
	out, err = s.nodeCmd(c, task.NodeID, "inspect", "--format", "{{json .NetworkSettings.Networks.test_swarm_br.Aliases}}", task.Status.ContainerStatus.ContainerID)
	assert.NilError(c, err, out)

	// Make sure the only alias seen is the container-id
	var aliases []string
	assert.Assert(c, json.Unmarshal([]byte(out), &aliases) == nil)
	assert.Equal(c, len(aliases), 1)

	assert.Assert(c, strings.Contains(task.Status.ContainerStatus.ContainerID, aliases[0]))
}

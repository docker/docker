package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/versions/v1p20"
	"github.com/docker/docker/client"
	"github.com/go-check/check"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func (s *DockerSuite) TestInspectAPIContainerResponse(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "true")

	cleanedContainerID := strings.TrimSpace(out)
	keysBase := []string{"Id", "State", "Created", "Path", "Args", "Config", "Image", "NetworkSettings",
		"ResolvConfPath", "HostnamePath", "HostsPath", "LogPath", "Name", "Driver", "MountLabel", "ProcessLabel", "GraphDriver"}

	type acase struct {
		version string
		keys    []string
	}

	var cases []acase

	if testEnv.OSType == "windows" {
		cases = []acase{
			{"v1.25", append(keysBase, "Mounts")},
		}

	} else {
		cases = []acase{
			{"v1.20", append(keysBase, "Mounts")},
			{"v1.19", append(keysBase, "Volumes", "VolumesRW")},
		}
	}

	for _, cs := range cases {
		body := getInspectBody(c, cs.version, cleanedContainerID)

		var inspectJSON map[string]interface{}
		err := json.Unmarshal(body, &inspectJSON)
		assert.NilError(c, err, "Unable to unmarshal body for version %s", cs.version)

		for _, key := range cs.keys {
			_, ok := inspectJSON[key]
			assert.Check(c, ok, "%s does not exist in response for version %s", key, cs.version)
		}

		//Issue #6830: type not properly converted to JSON/back
		_, ok := inspectJSON["Path"].(bool)
		assert.Assert(c, !ok, "Path of `true` should not be converted to boolean `true` via JSON marshalling")
	}
}

func (s *DockerSuite) TestInspectAPIContainerVolumeDriverLegacy(c *check.C) {
	// No legacy implications for Windows
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "true")

	cleanedContainerID := strings.TrimSpace(out)

	cases := []string{"v1.19", "v1.20"}
	for _, version := range cases {
		body := getInspectBody(c, version, cleanedContainerID)

		var inspectJSON map[string]interface{}
		err := json.Unmarshal(body, &inspectJSON)
		assert.NilError(c, err, "Unable to unmarshal body for version %s", version)

		config, ok := inspectJSON["Config"]
		assert.Assert(c, ok, "Unable to find 'Config'")
		cfg := config.(map[string]interface{})
		_, ok = cfg["VolumeDriver"]
		assert.Assert(c, ok, "API version %s expected to include VolumeDriver in 'Config'", version)
	}
}

func (s *DockerSuite) TestInspectAPIContainerVolumeDriver(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "--volume-driver", "local", "busybox", "true")

	cleanedContainerID := strings.TrimSpace(out)

	body := getInspectBody(c, "v1.25", cleanedContainerID)

	var inspectJSON map[string]interface{}
	err := json.Unmarshal(body, &inspectJSON)
	assert.NilError(c, err, "Unable to unmarshal body for version 1.25")

	config, ok := inspectJSON["Config"]
	assert.Assert(c, ok, "Unable to find 'Config'")
	cfg := config.(map[string]interface{})
	_, ok = cfg["VolumeDriver"]
	assert.Assert(c, !ok, "API version 1.25 expected to not include VolumeDriver in 'Config'")

	config, ok = inspectJSON["HostConfig"]
	assert.Assert(c, ok, "Unable to find 'HostConfig'")
	cfg = config.(map[string]interface{})
	_, ok = cfg["VolumeDriver"]
	assert.Assert(c, ok, "API version 1.25 expected to include VolumeDriver in 'HostConfig'")
}

func (s *DockerSuite) TestInspectAPIImageResponse(c *check.C) {
	dockerCmd(c, "tag", "busybox:latest", "busybox:mytag")
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	imageJSON, _, err := cli.ImageInspectWithRaw(context.Background(), "busybox")
	assert.NilError(c, err)

	assert.Check(c, len(imageJSON.RepoTags) == 2)
	assert.Check(c, is.Contains(imageJSON.RepoTags, "busybox:latest"))
	assert.Check(c, is.Contains(imageJSON.RepoTags, "busybox:mytag"))
}

// #17131, #17139, #17173
func (s *DockerSuite) TestInspectAPIEmptyFieldsInConfigPre121(c *check.C) {
	// Not relevant on Windows
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "true")

	cleanedContainerID := strings.TrimSpace(out)

	cases := []string{"v1.19", "v1.20"}
	for _, version := range cases {
		body := getInspectBody(c, version, cleanedContainerID)

		var inspectJSON map[string]interface{}
		err := json.Unmarshal(body, &inspectJSON)
		assert.NilError(c, err, "Unable to unmarshal body for version %s", version)
		config, ok := inspectJSON["Config"]
		assert.Assert(c, ok, "Unable to find 'Config'")
		cfg := config.(map[string]interface{})
		for _, f := range []string{"MacAddress", "NetworkDisabled", "ExposedPorts"} {
			_, ok := cfg[f]
			assert.Check(c, ok, "API version %s expected to include %s in 'Config'", version, f)
		}
	}
}

func (s *DockerSuite) TestInspectAPIBridgeNetworkSettings120(c *check.C) {
	// Not relevant on Windows, and besides it doesn't have any bridge network settings
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	containerID := strings.TrimSpace(out)
	waitRun(containerID)

	body := getInspectBody(c, "v1.20", containerID)

	var inspectJSON v1p20.ContainerJSON
	err := json.Unmarshal(body, &inspectJSON)
	assert.NilError(c, err)

	settings := inspectJSON.NetworkSettings
	assert.Assert(c, len(settings.IPAddress) != 0)
}

func (s *DockerSuite) TestInspectAPIBridgeNetworkSettings121(c *check.C) {
	// Windows doesn't have any bridge network settings
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	containerID := strings.TrimSpace(out)
	waitRun(containerID)

	body := getInspectBody(c, "v1.21", containerID)

	var inspectJSON types.ContainerJSON
	err := json.Unmarshal(body, &inspectJSON)
	assert.NilError(c, err)

	settings := inspectJSON.NetworkSettings
	assert.Assert(c, len(settings.IPAddress) != 0)
	assert.Assert(c, settings.Networks["bridge"] != nil)
	assert.Equal(c, settings.IPAddress, settings.Networks["bridge"].IPAddress)
}

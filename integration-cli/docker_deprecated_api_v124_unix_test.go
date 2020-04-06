// +build !windows

package main

import (
	"strings"
	"testing"

	"github.com/docker/docker/testutil/request"
	"gotest.tools/assert"
)

// #19100 This is a deprecated feature test, it should be removed in Docker 1.12
func (s *DockerNetworkSuite) TestDeprecatedDockerNetworkStartAPIWithHostconfig(c *testing.T) {
	netName := "test"
	conName := "foo"
	dockerCmd(c, "network", "create", netName)
	dockerCmd(c, "create", "--name", conName, "busybox", "top")

	config := map[string]interface{}{
		"HostConfig": map[string]interface{}{
			"NetworkMode": netName,
		},
	}
	_, _, err := request.Post(formatV123StartAPIURL("/containers/"+conName+"/start"), request.JSONBody(config))
	assert.NilError(c, err)
	assert.NilError(c, waitRun(conName))
	networks := inspectField(c, conName, "NetworkSettings.Networks")
	assert.Assert(c, strings.Contains(networks, netName), "Should contain '%s' network", netName)
	assert.Assert(c, !strings.Contains(networks, "bridge"), "Should not contain 'bridge' network")
}

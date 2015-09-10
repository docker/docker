package main

import (
	"fmt"
	"strings"

	"github.com/docker/docker/pkg/integration/checker"
	"github.com/docker/docker/utils"
	"github.com/go-check/check"
)

// ensure docker info succeeds
func (s *DockerSuite) TestInfoEnsureSucceeds(c *check.C) {
	out, _ := dockerCmd(c, "info")

	// always shown fields
	stringsToCheck := []string{
		"ID:",
		"Containers:",
		"Images:",
		"Execution Driver:",
		"Logging Driver:",
		"Operating System:",
		"CPUs:",
		"Total Memory:",
		"Kernel Version:",
		"Storage Driver:",
	}

	if utils.ExperimentalBuild() {
		stringsToCheck = append(stringsToCheck, "Experimental: true")
	}

	for _, linePrefix := range stringsToCheck {
		if !strings.Contains(out, linePrefix) {
			c.Errorf("couldn't find string %v in output", linePrefix)
		}
	}
}

// TestInfoDiscoveryBackend verifies that a daemon run with `--discovery-address` and
// `--discovery-backend` properly show the backend's endpoint in info output.
func (s *DockerSuite) TestInfoDiscoveryBackend(c *check.C) {
	discoveryBackend := "consul://consuladdr:consulport/some/path"

	d := NewDaemon(c)
	if err := d.Start(fmt.Sprintf("--discovery-backend=%s", discoveryBackend), "--discovery-address=foo"); err != nil {
		c.Fatal(err)
	}
	defer d.Stop()

	out, err := d.Cmd("info")
	c.Assert(err, checker.IsNil)
	c.Assert(out, checker.Contains, fmt.Sprintf("Discovery backend: %s\n", discoveryBackend))
}

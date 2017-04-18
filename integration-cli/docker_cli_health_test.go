package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/go-check/check"
)

func waitForHealthStatus(c *check.C, name string, prev string, expected string) {
	prev = prev + "\n"
	expected = expected + "\n"
	for {
		out, _ := dockerCmd(c, "inspect", "--format={{.State.Health.Status}}", name)
		if out == expected {
			return
		}
		c.Check(out, checker.Equals, prev)
		if out != prev {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func getHealth(c *check.C, name string) *types.Health {
	out, _ := dockerCmd(c, "inspect", "--format={{json .State.Health}}", name)
	var health types.Health
	err := json.Unmarshal([]byte(out), &health)
	c.Check(err, checker.Equals, nil)
	return &health
}

func (s *DockerSuite) TestHealth(c *check.C) {
	testRequires(c, DaemonIsLinux)

	imageName := "testhealth"
	buildImageSuccessfully(c, imageName, build.WithDockerfile(`FROM busybox
		RUN echo OK > /status
		CMD ["/bin/sleep", "120"]
		STOPSIGNAL SIGKILL
		HEALTHCHECK --interval=1s --timeout=30s \
		  CMD cat /status`))

	// No health status before starting
	name := "test_health"
	dockerCmd(c, "create", "--name", name, imageName)
	out, _ := dockerCmd(c, "ps", "-a", "--format={{.Status}}")
	c.Check(out, checker.Equals, "Created\n")

	// Inspect the options
	out, _ = dockerCmd(c, "inspect",
		"--format=timeout={{.Config.Healthcheck.Timeout}} interval={{.Config.Healthcheck.Interval}} retries={{.Config.Healthcheck.Retries}} test={{.Config.Healthcheck.Test}}", name)
	c.Check(out, checker.Equals, "timeout=30s interval=1s retries=0 test=[CMD-SHELL cat /status]\n")

	// Start
	dockerCmd(c, "start", name)
	waitForHealthStatus(c, name, "starting", "healthy")

	// Make it fail
	dockerCmd(c, "exec", name, "rm", "/status")
	waitForHealthStatus(c, name, "healthy", "unhealthy")

	// Inspect the status
	out, _ = dockerCmd(c, "inspect", "--format={{.State.Health.Status}}", name)
	c.Check(out, checker.Equals, "unhealthy\n")

	// Make it healthy again
	dockerCmd(c, "exec", name, "touch", "/status")
	waitForHealthStatus(c, name, "unhealthy", "healthy")

	// Remove container
	dockerCmd(c, "rm", "-f", name)

	// Disable the check from the CLI
	out, _ = dockerCmd(c, "create", "--name=noh", "--no-healthcheck", imageName)
	out, _ = dockerCmd(c, "inspect", "--format={{.Config.Healthcheck.Test}}", "noh")
	c.Check(out, checker.Equals, "[NONE]\n")
	dockerCmd(c, "rm", "noh")

	// Disable the check with a new build
	buildImageSuccessfully(c, "no_healthcheck", build.WithDockerfile(`FROM testhealth
		HEALTHCHECK NONE`))

	out, _ = dockerCmd(c, "inspect", "--format={{.ContainerConfig.Healthcheck.Test}}", "no_healthcheck")
	c.Check(out, checker.Equals, "[NONE]\n")

	// Enable the checks from the CLI
	_, _ = dockerCmd(c, "run", "-d", "--name=fatal_healthcheck",
		"--health-interval=1s",
		"--health-retries=3",
		"--health-cmd=cat /status",
		"no_healthcheck")
	waitForHealthStatus(c, "fatal_healthcheck", "starting", "healthy")
	health := getHealth(c, "fatal_healthcheck")
	c.Check(health.Status, checker.Equals, "healthy")
	c.Check(health.FailingStreak, checker.Equals, 0)
	last := health.Log[len(health.Log)-1]
	c.Check(last.ExitCode, checker.Equals, 0)
	c.Check(last.Output, checker.Equals, "OK\n")

	// Fail the check
	dockerCmd(c, "exec", "fatal_healthcheck", "rm", "/status")
	waitForHealthStatus(c, "fatal_healthcheck", "healthy", "unhealthy")

	failsStr, _ := dockerCmd(c, "inspect", "--format={{.State.Health.FailingStreak}}", "fatal_healthcheck")
	fails, err := strconv.Atoi(strings.TrimSpace(failsStr))
	c.Check(err, check.IsNil)
	c.Check(fails >= 3, checker.Equals, true)
	dockerCmd(c, "rm", "-f", "fatal_healthcheck")

	// Check timeout
	// Note: if the interval is too small, it seems that Docker spends all its time running health
	// checks and never gets around to killing it.
	_, _ = dockerCmd(c, "run", "-d", "--name=test",
		"--health-interval=1s", "--health-cmd=sleep 5m", "--health-timeout=1s", imageName)
	waitForHealthStatus(c, "test", "starting", "unhealthy")
	health = getHealth(c, "test")
	last = health.Log[len(health.Log)-1]
	c.Check(health.Status, checker.Equals, "unhealthy")
	c.Check(last.ExitCode, checker.Equals, -1)
	c.Check(last.Output, checker.Equals, "Health check exceeded timeout (1s)")
	dockerCmd(c, "rm", "-f", "test")

	// Check JSON-format
	buildImageSuccessfully(c, imageName, build.WithDockerfile(`FROM busybox
		RUN echo OK > /status
		CMD ["/bin/sleep", "120"]
		STOPSIGNAL SIGKILL
		HEALTHCHECK --interval=1s --timeout=30s \
		  CMD ["cat", "/my status"]`))
	out, _ = dockerCmd(c, "inspect",
		"--format={{.Config.Healthcheck.Test}}", imageName)
	c.Check(out, checker.Equals, "[CMD cat /my status]\n")

}

func (s *DockerSuite) TestHealthHttp(c *check.C) {
	testRequires(c, DaemonIsLinux)

	name := "healthcheck_http"
	// Enable the checks from the CLI
	_, _ = dockerCmd(c, "run", "-d", "--name", name,
		"--health-interval=0.5s",
		"--health-retries=3",
		"--health-http=:8080/health",
		"busybox", "sh", "-c", "echo -e \"HTTP/1.1 200 OK\n\nHealthCheck Successful\" > /status && while true ; do cat /status | nc -l -p 8080 ; done")
	waitForHealthStatus(c, name, "starting", "healthy")
	health := getHealth(c, name)
	c.Check(health.Status, checker.Equals, "healthy")
	c.Check(health.FailingStreak, checker.Equals, 0)
	last := health.Log[len(health.Log)-1]
	c.Check(last.ExitCode, checker.Equals, 0)
	c.Check(last.Output, checker.Equals, "HealthCheck Successful\n")

	// Fail the check
	dockerCmd(c, "exec", name, "sh", "-c", "echo -e \"HTTP/1.1 500 Internal Server Error\n\nHealthCheck Failed\" > /status")
	waitForHealthStatus(c, name, "healthy", "unhealthy")

	health = getHealth(c, name)
	c.Check(health.Status, checker.Equals, "unhealthy")
	c.Check(health.FailingStreak >= 3, checker.Equals, true)
	last = health.Log[len(health.Log)-1]
	c.Check(last.ExitCode, checker.Equals, 1)
	c.Check(last.Output, checker.Equals, "HealthCheck Failed\n")
}

func (s *DockerSuite) TestHealthHttpBuild(c *check.C) {
	testRequires(c, DaemonIsLinux)

	imageName := "testhealth"
	_, err := buildImage(imageName,
		`FROM busybox
		STOPSIGNAL SIGKILL
		HEALTHCHECK --interval=1s --timeout=30s \
		  HTTP :8080/health`,
		true)

	c.Check(err, check.IsNil)

	// No health status before starting
	name := "healthcheck_http"
	dockerCmd(c, "create", "--name", name, imageName, "sh", "-c", "echo -e \"HTTP/1.1 200 OK\n\nHealthCheck Successful\" > /status && while true ; do cat /status | nc -l -p 8080 ; done")
	out, _ := dockerCmd(c, "ps", "-a", "--format={{.Status}}")
	c.Check(out, checker.Equals, "Created\n")

	// Inspect the options
	out, _ = dockerCmd(c, "inspect", "--format", "timeout={{.Config.Healthcheck.Timeout}} "+
		"interval={{.Config.Healthcheck.Interval}} "+
		"retries={{.Config.Healthcheck.Retries}} "+
		"test={{.Config.Healthcheck.Test}}", name)
	c.Check(out, checker.Equals, "timeout=30s interval=1s retries=0 test=[HTTP :8080/health]\n")

	// Start
	dockerCmd(c, "start", name)
	waitForHealthStatus(c, name, "starting", "healthy")

	// Make it fail
	dockerCmd(c, "exec", name, "rm", "/status")
	waitForHealthStatus(c, name, "healthy", "unhealthy")

	// Inspect the status
	out, _ = dockerCmd(c, "inspect", "--format={{.State.Health.Status}}", name)
	c.Check(out, checker.Equals, "unhealthy\n")

	// Make it healthy again
	dockerCmd(c, "exec", name, "sh", "-c", "echo -e \"HTTP/1.1 200 OK\n\nHealthCheck Successful\" > /status")
	waitForHealthStatus(c, name, "unhealthy", "healthy")

	// Remove container
	dockerCmd(c, "rm", "-f", name)
}

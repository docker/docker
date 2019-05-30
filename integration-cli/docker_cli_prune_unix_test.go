// +build !windows

package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/go-check/check"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

func pruneNetworkAndVerify(c *check.C, d *daemon.Daemon, kept, pruned []string) {
	_, err := d.Cmd("network", "prune", "--force")
	assert.NilError(c, err)

	for _, s := range kept {
		waitAndAssert(c, defaultReconciliationTimeout, func(*check.C) (interface{}, check.CommentInterface) {
			out, err := d.Cmd("network", "ls", "--format", "{{.Name}}")
			assert.NilError(c, err)
			return out, nil
		}, checker.Contains, s)
	}

	for _, s := range pruned {
		waitAndAssert(c, defaultReconciliationTimeout, func(*check.C) (interface{}, check.CommentInterface) {
			out, err := d.Cmd("network", "ls", "--format", "{{.Name}}")
			assert.NilError(c, err)
			return out, nil
		}, checker.Not(checker.Contains), s)
	}
}

func (s *DockerSwarmSuite) TestPruneNetwork(c *check.C) {
	d := s.AddDaemon(c, true, true)
	_, err := d.Cmd("network", "create", "n1") // used by container (testprune)
	assert.NilError(c, err)
	_, err = d.Cmd("network", "create", "n2")
	assert.NilError(c, err)
	_, err = d.Cmd("network", "create", "n3", "--driver", "overlay") // used by service (testprunesvc)
	assert.NilError(c, err)
	_, err = d.Cmd("network", "create", "n4", "--driver", "overlay")
	assert.NilError(c, err)

	cName := "testprune"
	_, err = d.Cmd("run", "-d", "--name", cName, "--net", "n1", "busybox", "top")
	assert.NilError(c, err)

	serviceName := "testprunesvc"
	replicas := 1
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image",
		"--name", serviceName,
		"--replicas", strconv.Itoa(replicas),
		"--network", "n3",
		"busybox", "top")
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, replicas+1)

	// prune and verify
	pruneNetworkAndVerify(c, d, []string{"n1", "n3"}, []string{"n2", "n4"})

	// remove containers, then prune and verify again
	_, err = d.Cmd("rm", "-f", cName)
	assert.NilError(c, err)
	_, err = d.Cmd("service", "rm", serviceName)
	assert.NilError(c, err)
	waitAndAssert(c, defaultReconciliationTimeout, d.CheckActiveContainerCount, checker.Equals, 0)

	pruneNetworkAndVerify(c, d, []string{}, []string{"n1", "n3"})
}

func (s *DockerDaemonSuite) TestPruneImageDangling(c *check.C) {
	s.d.StartWithBusybox(c)

	result := cli.BuildCmd(c, "test", cli.Daemon(s.d),
		build.WithDockerfile(`FROM busybox
                 LABEL foo=bar`),
		cli.WithFlags("-q"),
	)
	result.Assert(c, icmd.Success)
	id := strings.TrimSpace(result.Combined())

	out, err := s.d.Cmd("images", "-q", "--no-trunc")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id)

	out, err = s.d.Cmd("image", "prune", "--force")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id)

	out, err = s.d.Cmd("images", "-q", "--no-trunc")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id)

	out, err = s.d.Cmd("image", "prune", "--force", "--all")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id)

	out, err = s.d.Cmd("images", "-q", "--no-trunc")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id)
}

func (s *DockerSuite) TestPruneContainerUntil(c *check.C) {
	out := cli.DockerCmd(c, "run", "-d", "busybox").Combined()
	id1 := strings.TrimSpace(out)
	cli.WaitExited(c, id1, 5*time.Second)

	until := daemonUnixTime(c)

	out = cli.DockerCmd(c, "run", "-d", "busybox").Combined()
	id2 := strings.TrimSpace(out)
	cli.WaitExited(c, id2, 5*time.Second)

	out = cli.DockerCmd(c, "container", "prune", "--force", "--filter", "until="+until).Combined()
	c.Assert(strings.TrimSpace(out), checker.Contains, id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)

	out = cli.DockerCmd(c, "ps", "-a", "-q", "--no-trunc").Combined()
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
}

func (s *DockerSuite) TestPruneContainerLabel(c *check.C) {
	out := cli.DockerCmd(c, "run", "-d", "--label", "foo", "busybox").Combined()
	id1 := strings.TrimSpace(out)
	cli.WaitExited(c, id1, 5*time.Second)

	out = cli.DockerCmd(c, "run", "-d", "--label", "bar", "busybox").Combined()
	id2 := strings.TrimSpace(out)
	cli.WaitExited(c, id2, 5*time.Second)

	out = cli.DockerCmd(c, "run", "-d", "busybox").Combined()
	id3 := strings.TrimSpace(out)
	cli.WaitExited(c, id3, 5*time.Second)

	out = cli.DockerCmd(c, "run", "-d", "--label", "foobar", "busybox").Combined()
	id4 := strings.TrimSpace(out)
	cli.WaitExited(c, id4, 5*time.Second)

	// Add a config file of label=foobar, that will have no impact if cli is label!=foobar
	config := `{"pruneFilters": ["label=foobar"]}`
	d, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)
	defer os.RemoveAll(d)
	err = ioutil.WriteFile(filepath.Join(d, "config.json"), []byte(config), 0644)
	assert.NilError(c, err)

	// With config.json only, prune based on label=foobar
	out = cli.DockerCmd(c, "--config", d, "container", "prune", "--force").Combined()
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)
	c.Assert(strings.TrimSpace(out), checker.Contains, id4)

	out = cli.DockerCmd(c, "container", "prune", "--force", "--filter", "label=foo").Combined()
	c.Assert(strings.TrimSpace(out), checker.Contains, id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)

	out = cli.DockerCmd(c, "ps", "-a", "-q", "--no-trunc").Combined()
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
	c.Assert(strings.TrimSpace(out), checker.Contains, id3)

	out = cli.DockerCmd(c, "container", "prune", "--force", "--filter", "label!=bar").Combined()
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Contains, id3)

	out = cli.DockerCmd(c, "ps", "-a", "-q", "--no-trunc").Combined()
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)

	// With config.json label=foobar and CLI label!=foobar, CLI label!=foobar supersede
	out = cli.DockerCmd(c, "--config", d, "container", "prune", "--force", "--filter", "label!=foobar").Combined()
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)

	out = cli.DockerCmd(c, "ps", "-a", "-q", "--no-trunc").Combined()
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
}

func (s *DockerSuite) TestPruneVolumeLabel(c *check.C) {
	out, _ := dockerCmd(c, "volume", "create", "--label", "foo")
	id1 := strings.TrimSpace(out)
	c.Assert(id1, checker.Not(checker.Equals), "")

	out, _ = dockerCmd(c, "volume", "create", "--label", "bar")
	id2 := strings.TrimSpace(out)
	c.Assert(id2, checker.Not(checker.Equals), "")

	out, _ = dockerCmd(c, "volume", "create")
	id3 := strings.TrimSpace(out)
	c.Assert(id3, checker.Not(checker.Equals), "")

	out, _ = dockerCmd(c, "volume", "create", "--label", "foobar")
	id4 := strings.TrimSpace(out)
	c.Assert(id4, checker.Not(checker.Equals), "")

	// Add a config file of label=foobar, that will have no impact if cli is label!=foobar
	config := `{"pruneFilters": ["label=foobar"]}`
	d, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)
	defer os.RemoveAll(d)
	err = ioutil.WriteFile(filepath.Join(d, "config.json"), []byte(config), 0644)
	assert.NilError(c, err)

	// With config.json only, prune based on label=foobar
	out, _ = dockerCmd(c, "--config", d, "volume", "prune", "--force")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)
	c.Assert(strings.TrimSpace(out), checker.Contains, id4)

	out, _ = dockerCmd(c, "volume", "prune", "--force", "--filter", "label=foo")
	c.Assert(strings.TrimSpace(out), checker.Contains, id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)

	out, _ = dockerCmd(c, "volume", "ls", "--format", "{{.Name}}")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
	c.Assert(strings.TrimSpace(out), checker.Contains, id3)

	out, _ = dockerCmd(c, "volume", "prune", "--force", "--filter", "label!=bar")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
	c.Assert(strings.TrimSpace(out), checker.Contains, id3)

	out, _ = dockerCmd(c, "volume", "ls", "--format", "{{.Name}}")
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id3)

	// With config.json label=foobar and CLI label!=foobar, CLI label!=foobar supersede
	out, _ = dockerCmd(c, "--config", d, "volume", "prune", "--force", "--filter", "label!=foobar")
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)

	out, _ = dockerCmd(c, "volume", "ls", "--format", "{{.Name}}")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)
}

func (s *DockerSuite) TestPruneNetworkLabel(c *check.C) {
	dockerCmd(c, "network", "create", "--label", "foo", "n1")
	dockerCmd(c, "network", "create", "--label", "bar", "n2")
	dockerCmd(c, "network", "create", "n3")

	out, _ := dockerCmd(c, "network", "prune", "--force", "--filter", "label=foo")
	c.Assert(strings.TrimSpace(out), checker.Contains, "n1")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n2")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n3")

	out, _ = dockerCmd(c, "network", "prune", "--force", "--filter", "label!=bar")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n1")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n2")
	c.Assert(strings.TrimSpace(out), checker.Contains, "n3")

	out, _ = dockerCmd(c, "network", "prune", "--force")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n1")
	c.Assert(strings.TrimSpace(out), checker.Contains, "n2")
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), "n3")
}

func (s *DockerDaemonSuite) TestPruneImageLabel(c *check.C) {
	s.d.StartWithBusybox(c)

	result := cli.BuildCmd(c, "test1", cli.Daemon(s.d),
		build.WithDockerfile(`FROM busybox
                 LABEL foo=bar`),
		cli.WithFlags("-q"),
	)
	result.Assert(c, icmd.Success)
	id1 := strings.TrimSpace(result.Combined())
	out, err := s.d.Cmd("images", "-q", "--no-trunc")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id1)

	result = cli.BuildCmd(c, "test2", cli.Daemon(s.d),
		build.WithDockerfile(`FROM busybox
                 LABEL bar=foo`),
		cli.WithFlags("-q"),
	)
	result.Assert(c, icmd.Success)
	id2 := strings.TrimSpace(result.Combined())
	out, err = s.d.Cmd("images", "-q", "--no-trunc")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)

	out, err = s.d.Cmd("image", "prune", "--force", "--all", "--filter", "label=foo=bar")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Contains, id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)

	out, err = s.d.Cmd("image", "prune", "--force", "--all", "--filter", "label!=bar=foo")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id2)

	out, err = s.d.Cmd("image", "prune", "--force", "--all", "--filter", "label=bar=foo")
	assert.NilError(c, err)
	c.Assert(strings.TrimSpace(out), checker.Not(checker.Contains), id1)
	c.Assert(strings.TrimSpace(out), checker.Contains, id2)
}

// +build !windows

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/go-check/check"
	"github.com/kr/pty"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
)

// #5979
func (s *DockerSuite) TestEventsRedirectStdout(c *check.C) {
	since := daemonUnixTime(c)
	dockerCmd(c, "run", "busybox", "true")

	file, err := ioutil.TempFile("", "")
	assert.NilError(c, err, "could not create temp file")
	defer os.Remove(file.Name())

	command := fmt.Sprintf("%s events --since=%s --until=%s > %s", dockerBinary, since, daemonUnixTime(c), file.Name())
	_, tty, err := pty.Open()
	assert.NilError(c, err, "Could not open pty")
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	assert.NilError(c, cmd.Run(), "run err for command %q", command)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		for _, ch := range scanner.Text() {
			assert.Check(c, unicode.IsControl(ch) == false, "found control character %v", []byte(string(ch)))
		}
	}
	assert.NilError(c, scanner.Err(), "Scan err for command %q", command)
}

func (s *DockerSuite) TestEventsOOMDisableFalse(c *check.C) {
	testRequires(c, DaemonIsLinux, oomControl, memoryLimitSupport, swapMemorySupport, NotPpc64le)

	errChan := make(chan error)
	go func() {
		defer close(errChan)
		out, exitCode, _ := dockerCmdWithError("run", "--name", "oomFalse", "-m", "10MB", "busybox", "sh", "-c", "x=a; while true; do x=$x$x$x$x; done")
		if expected := 137; exitCode != expected {
			errChan <- fmt.Errorf("wrong exit code for OOM container: expected %d, got %d (output: %q)", expected, exitCode, out)
		}
	}()
	select {
	case err := <-errChan:
		assert.NilError(c, err)
	case <-time.After(30 * time.Second):
		c.Fatal("Timeout waiting for container to die on OOM")
	}

	out, _ := dockerCmd(c, "events", "--since=0", "-f", "container=oomFalse", "--until", daemonUnixTime(c))
	events := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	nEvents := len(events)

	assert.Assert(c, nEvents >= 5)
	assert.Equal(c, parseEventAction(c, events[nEvents-5]), "create")
	assert.Equal(c, parseEventAction(c, events[nEvents-4]), "attach")
	assert.Equal(c, parseEventAction(c, events[nEvents-3]), "start")
	assert.Equal(c, parseEventAction(c, events[nEvents-2]), "oom")
	assert.Equal(c, parseEventAction(c, events[nEvents-1]), "die")
}

func (s *DockerSuite) TestEventsOOMDisableTrue(c *check.C) {
	testRequires(c, DaemonIsLinux, oomControl, memoryLimitSupport, NotArm, swapMemorySupport, NotPpc64le)

	errChan := make(chan error)
	observer, err := newEventObserver(c)
	assert.NilError(c, err)
	err = observer.Start()
	assert.NilError(c, err)
	defer observer.Stop()

	go func() {
		defer close(errChan)
		out, exitCode, _ := dockerCmdWithError("run", "--oom-kill-disable=true", "--name", "oomTrue", "-m", "10MB", "busybox", "sh", "-c", "x=a; while true; do x=$x$x$x$x; done")
		if expected := 137; exitCode != expected {
			errChan <- fmt.Errorf("wrong exit code for OOM container: expected %d, got %d (output: %q)", expected, exitCode, out)
		}
	}()

	assert.NilError(c, waitRun("oomTrue"))
	defer dockerCmdWithResult("kill", "oomTrue")
	containerID := inspectField(c, "oomTrue", "Id")

	testActions := map[string]chan bool{
		"oom": make(chan bool),
	}

	matcher := matchEventLine(containerID, "container", testActions)
	processor := processEventMatch(testActions)
	go observer.Match(matcher, processor)

	select {
	case <-time.After(20 * time.Second):
		observer.CheckEventError(c, containerID, "oom", matcher)
	case <-testActions["oom"]:
	// ignore, done
	case errRun := <-errChan:
		if errRun != nil {
			c.Fatalf("%v", errRun)
		} else {
			c.Fatalf("container should be still running but it's not")
		}
	}

	status := inspectField(c, "oomTrue", "State.Status")
	assert.Equal(c, strings.TrimSpace(status), "running", "container should be still running")
}

// #18453
func (s *DockerSuite) TestEventsContainerFilterByName(c *check.C) {
	testRequires(c, DaemonIsLinux)
	cOut, _ := dockerCmd(c, "run", "--name=foo", "-d", "busybox", "top")
	c1 := strings.TrimSpace(cOut)
	waitRun("foo")
	cOut, _ = dockerCmd(c, "run", "--name=bar", "-d", "busybox", "top")
	c2 := strings.TrimSpace(cOut)
	waitRun("bar")
	out, _ := dockerCmd(c, "events", "-f", "container=foo", "--since=0", "--until", daemonUnixTime(c))
	assert.Assert(c, strings.Contains(out, c1), out)
	assert.Assert(c, !strings.Contains(out, c2), out)
}

// #18453
func (s *DockerSuite) TestEventsContainerFilterBeforeCreate(c *check.C) {
	testRequires(c, DaemonIsLinux)
	buf := &bytes.Buffer{}
	cmd := exec.Command(dockerBinary, "events", "-f", "container=foo", "--since=0")
	cmd.Stdout = buf
	assert.NilError(c, cmd.Start())
	defer cmd.Wait()
	defer cmd.Process.Kill()

	// Sleep for a second to make sure we are testing the case where events are listened before container starts.
	time.Sleep(time.Second)
	id, _ := dockerCmd(c, "run", "--name=foo", "-d", "busybox", "top")
	cID := strings.TrimSpace(id)
	for i := 0; ; i++ {
		out := buf.String()
		if strings.Contains(out, cID) {
			break
		}
		if i > 30 {
			c.Fatalf("Missing event of container (foo, %v), got %q", cID, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *DockerSuite) TestVolumeEvents(c *check.C) {
	testRequires(c, DaemonIsLinux)

	since := daemonUnixTime(c)

	// Observe create/mount volume actions
	dockerCmd(c, "volume", "create", "test-event-volume-local")
	dockerCmd(c, "run", "--name", "test-volume-container", "--volume", "test-event-volume-local:/foo", "-d", "busybox", "true")
	waitRun("test-volume-container")

	// Observe unmount/destroy volume actions
	dockerCmd(c, "rm", "-f", "test-volume-container")
	dockerCmd(c, "volume", "rm", "test-event-volume-local")

	until := daemonUnixTime(c)
	out, _ := dockerCmd(c, "events", "--since", since, "--until", until)
	events := strings.Split(strings.TrimSpace(out), "\n")
	assert.Assert(c, len(events) > 4)

	volumeEvents := eventActionsByIDAndType(c, events, "test-event-volume-local", "volume")
	assert.Equal(c, len(volumeEvents), 5)
	assert.Equal(c, volumeEvents[0], "create")
	assert.Equal(c, volumeEvents[1], "create")
	assert.Equal(c, volumeEvents[2], "mount")
	assert.Equal(c, volumeEvents[3], "unmount")
	assert.Equal(c, volumeEvents[4], "destroy")
}

func (s *DockerSuite) TestNetworkEvents(c *check.C) {
	testRequires(c, DaemonIsLinux)

	since := daemonUnixTime(c)

	// Observe create/connect network actions
	dockerCmd(c, "network", "create", "test-event-network-local")
	dockerCmd(c, "run", "--name", "test-network-container", "--net", "test-event-network-local", "-d", "busybox", "true")
	waitRun("test-network-container")

	// Observe disconnect/destroy network actions
	dockerCmd(c, "rm", "-f", "test-network-container")
	dockerCmd(c, "network", "rm", "test-event-network-local")

	until := daemonUnixTime(c)
	out, _ := dockerCmd(c, "events", "--since", since, "--until", until)
	events := strings.Split(strings.TrimSpace(out), "\n")
	assert.Assert(c, len(events) > 4)

	netEvents := eventActionsByIDAndType(c, events, "test-event-network-local", "network")
	assert.Equal(c, len(netEvents), 4)
	assert.Equal(c, netEvents[0], "create")
	assert.Equal(c, netEvents[1], "connect")
	assert.Equal(c, netEvents[2], "disconnect")
	assert.Equal(c, netEvents[3], "destroy")
}

func (s *DockerSuite) TestEventsContainerWithMultiNetwork(c *check.C) {
	testRequires(c, DaemonIsLinux)

	// Observe create/connect network actions
	dockerCmd(c, "network", "create", "test-event-network-local-1")
	dockerCmd(c, "network", "create", "test-event-network-local-2")
	dockerCmd(c, "run", "--name", "test-network-container", "--net", "test-event-network-local-1", "-td", "busybox", "sh")
	waitRun("test-network-container")
	dockerCmd(c, "network", "connect", "test-event-network-local-2", "test-network-container")

	since := daemonUnixTime(c)

	dockerCmd(c, "stop", "-t", "1", "test-network-container")

	until := daemonUnixTime(c)
	out, _ := dockerCmd(c, "events", "--since", since, "--until", until, "-f", "type=network")
	netEvents := strings.Split(strings.TrimSpace(out), "\n")

	// received two network disconnect events
	assert.Equal(c, len(netEvents), 2)
	assert.Assert(c, strings.Contains(netEvents[0], "disconnect"))
	assert.Assert(c, strings.Contains(netEvents[1], "disconnect"))

	//both networks appeared in the network event output
	assert.Assert(c, strings.Contains(out, "test-event-network-local-1"))
	assert.Assert(c, strings.Contains(out, "test-event-network-local-2"))
}

func (s *DockerSuite) TestEventsStreaming(c *check.C) {
	testRequires(c, DaemonIsLinux)

	observer, err := newEventObserver(c)
	assert.NilError(c, err)
	err = observer.Start()
	assert.NilError(c, err)
	defer observer.Stop()

	out, _ := dockerCmd(c, "run", "-d", "busybox:latest", "true")
	containerID := strings.TrimSpace(out)

	testActions := map[string]chan bool{
		"create":  make(chan bool, 1),
		"start":   make(chan bool, 1),
		"die":     make(chan bool, 1),
		"destroy": make(chan bool, 1),
	}

	matcher := matchEventLine(containerID, "container", testActions)
	processor := processEventMatch(testActions)
	go observer.Match(matcher, processor)

	select {
	case <-time.After(5 * time.Second):
		observer.CheckEventError(c, containerID, "create", matcher)
	case <-testActions["create"]:
		// ignore, done
	}

	select {
	case <-time.After(5 * time.Second):
		observer.CheckEventError(c, containerID, "start", matcher)
	case <-testActions["start"]:
		// ignore, done
	}

	select {
	case <-time.After(5 * time.Second):
		observer.CheckEventError(c, containerID, "die", matcher)
	case <-testActions["die"]:
		// ignore, done
	}

	dockerCmd(c, "rm", containerID)

	select {
	case <-time.After(5 * time.Second):
		observer.CheckEventError(c, containerID, "destroy", matcher)
	case <-testActions["destroy"]:
		// ignore, done
	}
}

func (s *DockerSuite) TestEventsImageUntagDelete(c *check.C) {
	testRequires(c, DaemonIsLinux)

	observer, err := newEventObserver(c)
	assert.NilError(c, err)
	err = observer.Start()
	assert.NilError(c, err)
	defer observer.Stop()

	name := "testimageevents"
	buildImageSuccessfully(c, name, build.WithDockerfile(`FROM scratch
		MAINTAINER "docker"`))
	imageID := getIDByName(c, name)
	assert.NilError(c, deleteImages(name))

	testActions := map[string]chan bool{
		"untag":  make(chan bool, 1),
		"delete": make(chan bool, 1),
	}

	matcher := matchEventLine(imageID, "image", testActions)
	processor := processEventMatch(testActions)
	go observer.Match(matcher, processor)

	select {
	case <-time.After(10 * time.Second):
		observer.CheckEventError(c, imageID, "untag", matcher)
	case <-testActions["untag"]:
		// ignore, done
	}

	select {
	case <-time.After(10 * time.Second):
		observer.CheckEventError(c, imageID, "delete", matcher)
	case <-testActions["delete"]:
		// ignore, done
	}
}

func (s *DockerSuite) TestEventsFilterVolumeAndNetworkType(c *check.C) {
	testRequires(c, DaemonIsLinux)

	since := daemonUnixTime(c)

	dockerCmd(c, "network", "create", "test-event-network-type")
	dockerCmd(c, "volume", "create", "test-event-volume-type")

	out, _ := dockerCmd(c, "events", "--filter", "type=volume", "--filter", "type=network", "--since", since, "--until", daemonUnixTime(c))
	events := strings.Split(strings.TrimSpace(out), "\n")
	assert.Assert(c, len(events) >= 2, out)

	networkActions := eventActionsByIDAndType(c, events, "test-event-network-type", "network")
	volumeActions := eventActionsByIDAndType(c, events, "test-event-volume-type", "volume")

	assert.Equal(c, volumeActions[0], "create")
	assert.Equal(c, networkActions[0], "create")
}

func (s *DockerSuite) TestEventsFilterVolumeID(c *check.C) {
	testRequires(c, DaemonIsLinux)

	since := daemonUnixTime(c)

	dockerCmd(c, "volume", "create", "test-event-volume-id")
	out, _ := dockerCmd(c, "events", "--filter", "volume=test-event-volume-id", "--since", since, "--until", daemonUnixTime(c))
	events := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(c, len(events), 1)

	assert.Equal(c, len(events), 1)
	assert.Assert(c, strings.Contains(events[0], "test-event-volume-id"))
	assert.Assert(c, strings.Contains(events[0], "driver=local"))
}

func (s *DockerSuite) TestEventsFilterNetworkID(c *check.C) {
	testRequires(c, DaemonIsLinux)

	since := daemonUnixTime(c)

	dockerCmd(c, "network", "create", "test-event-network-local")
	out, _ := dockerCmd(c, "events", "--filter", "network=test-event-network-local", "--since", since, "--until", daemonUnixTime(c))
	events := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(c, len(events), 1)
	assert.Assert(c, strings.Contains(events[0], "test-event-network-local"))
	assert.Assert(c, strings.Contains(events[0], "type=bridge"))
}

func (s *DockerDaemonSuite) TestDaemonEvents(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	configFilePath := "test.json"
	configFile, err := os.Create(configFilePath)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	daemonConfig := `{"labels":["foo=bar"]}`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()
	s.d.Start(c, fmt.Sprintf("--config-file=%s", configFilePath))

	// Get daemon ID
	out, err := s.d.Cmd("info")
	assert.NilError(c, err)
	daemonID := ""
	daemonName := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ID: ") {
			daemonID = strings.TrimPrefix(line, "ID: ")
		} else if strings.HasPrefix(line, "Name: ") {
			daemonName = strings.TrimPrefix(line, "Name: ")
		}
	}
	assert.Assert(c, daemonID != "")

	configFile, err = os.Create(configFilePath)
	assert.NilError(c, err)
	daemonConfig = `{"max-concurrent-downloads":1,"labels":["bar=foo"], "shutdown-timeout": 10}`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()

	assert.NilError(c, s.d.Signal(unix.SIGHUP))

	time.Sleep(3 * time.Second)

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c))
	assert.NilError(c, err)

	// only check for values known (daemon ID/name) or explicitly set above,
	// otherwise just check for names being present.
	expectedSubstrings := []string{
		" daemon reload " + daemonID + " ",
		"(allow-nondistributable-artifacts=[",
		" cluster-advertise=, ",
		" cluster-store=, ",
		" cluster-store-opts=",
		" debug=true, ",
		" default-ipc-mode=",
		" default-runtime=",
		" default-shm-size=",
		" insecure-registries=[",
		" labels=[\"bar=foo\"], ",
		" live-restore=",
		" max-concurrent-downloads=1, ",
		" max-concurrent-uploads=5, ",
		" name=" + daemonName,
		" registry-mirrors=[",
		" runtimes=",
		" shutdown-timeout=10)",
	}

	for _, s := range expectedSubstrings {
		assert.Check(c, strings.Contains(out, s))
	}
}

func (s *DockerDaemonSuite) TestDaemonEventsWithFilters(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	configFilePath := "test.json"
	configFile, err := os.Create(configFilePath)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	daemonConfig := `{"labels":["foo=bar"]}`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()
	s.d.Start(c, fmt.Sprintf("--config-file=%s", configFilePath))

	// Get daemon ID
	out, err := s.d.Cmd("info")
	assert.NilError(c, err)
	daemonID := ""
	daemonName := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ID: ") {
			daemonID = strings.TrimPrefix(line, "ID: ")
		} else if strings.HasPrefix(line, "Name: ") {
			daemonName = strings.TrimPrefix(line, "Name: ")
		}
	}
	assert.Assert(c, daemonID != "")
	assert.NilError(c, s.d.Signal(unix.SIGHUP))

	time.Sleep(3 * time.Second)

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c), "--filter", fmt.Sprintf("daemon=%s", daemonID))
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("daemon reload %s", daemonID)))

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c), "--filter", fmt.Sprintf("daemon=%s", daemonName))
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("daemon reload %s", daemonID)))

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c), "--filter", "daemon=foo")
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(out, fmt.Sprintf("daemon reload %s", daemonID)))

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c), "--filter", "type=daemon")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("daemon reload %s", daemonID)))

	out, err = s.d.Cmd("events", "--since=0", "--until", daemonUnixTime(c), "--filter", "type=container")
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(out, fmt.Sprintf("daemon reload %s", daemonID)))
}

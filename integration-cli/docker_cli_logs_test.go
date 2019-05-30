package main

import (
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/go-check/check"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

// This used to work, it test a log of PageSize-1 (gh#4851)
func (s *DockerSuite) TestLogsContainerSmallerThanPage(c *check.C) {
	testLogsContainerPagination(c, 32767)
}

// Regression test: When going over the PageSize, it used to panic (gh#4851)
func (s *DockerSuite) TestLogsContainerBiggerThanPage(c *check.C) {
	testLogsContainerPagination(c, 32768)
}

// Regression test: When going much over the PageSize, it used to block (gh#4851)
func (s *DockerSuite) TestLogsContainerMuchBiggerThanPage(c *check.C) {
	testLogsContainerPagination(c, 33000)
}

func testLogsContainerPagination(c *check.C, testLen int) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "sh", "-c", fmt.Sprintf("for i in $(seq 1 %d); do echo -n = >> a.a; done; echo >> a.a; cat a.a", testLen))
	id := strings.TrimSpace(out)
	dockerCmd(c, "wait", id)
	out, _ = dockerCmd(c, "logs", id)
	assert.Equal(c, len(out), testLen+1)
}

func (s *DockerSuite) TestLogsTimestamps(c *check.C) {
	testLen := 100
	out, _ := dockerCmd(c, "run", "-d", "busybox", "sh", "-c", fmt.Sprintf("for i in $(seq 1 %d); do echo = >> a.a; done; cat a.a", testLen))

	id := strings.TrimSpace(out)
	dockerCmd(c, "wait", id)

	out, _ = dockerCmd(c, "logs", "-t", id)

	lines := strings.Split(out, "\n")

	assert.Equal(c, len(lines), testLen+1)

	ts := regexp.MustCompile(`^.* `)

	for _, l := range lines {
		if l != "" {
			_, err := time.Parse(jsonmessage.RFC3339NanoFixed+" ", ts.FindString(l))
			assert.NilError(c, err, "Failed to parse timestamp from %v", l)
			// ensure we have padded 0's
			assert.Equal(c, l[29], uint8('Z'))
		}
	}
}

func (s *DockerSuite) TestLogsSeparateStderr(c *check.C) {
	msg := "stderr_log"
	out := cli.DockerCmd(c, "run", "-d", "busybox", "sh", "-c", fmt.Sprintf("echo %s 1>&2", msg)).Combined()
	id := strings.TrimSpace(out)
	cli.DockerCmd(c, "wait", id)
	cli.DockerCmd(c, "logs", id).Assert(c, icmd.Expected{
		Out: "",
		Err: msg,
	})
}

func (s *DockerSuite) TestLogsStderrInStdout(c *check.C) {
	// TODO Windows: Needs investigation why this fails. Obtained string includes
	// a bunch of ANSI escape sequences before the "stderr_log" message.
	testRequires(c, DaemonIsLinux)
	msg := "stderr_log"
	out := cli.DockerCmd(c, "run", "-d", "-t", "busybox", "sh", "-c", fmt.Sprintf("echo %s 1>&2", msg)).Combined()
	id := strings.TrimSpace(out)
	cli.DockerCmd(c, "wait", id)

	cli.DockerCmd(c, "logs", id).Assert(c, icmd.Expected{
		Out: msg,
		Err: "",
	})
}

func (s *DockerSuite) TestLogsTail(c *check.C) {
	testLen := 100
	out := cli.DockerCmd(c, "run", "-d", "busybox", "sh", "-c", fmt.Sprintf("for i in $(seq 1 %d); do echo =; done;", testLen)).Combined()

	id := strings.TrimSpace(out)
	cli.DockerCmd(c, "wait", id)

	out = cli.DockerCmd(c, "logs", "--tail", "0", id).Combined()
	lines := strings.Split(out, "\n")
	assert.Equal(c, len(lines), 1)

	out = cli.DockerCmd(c, "logs", "--tail", "5", id).Combined()
	lines = strings.Split(out, "\n")
	assert.Equal(c, len(lines), 6)

	out = cli.DockerCmd(c, "logs", "--tail", "99", id).Combined()
	lines = strings.Split(out, "\n")
	assert.Equal(c, len(lines), 100)

	out = cli.DockerCmd(c, "logs", "--tail", "all", id).Combined()
	lines = strings.Split(out, "\n")
	assert.Equal(c, len(lines), testLen+1)

	out = cli.DockerCmd(c, "logs", "--tail", "-1", id).Combined()
	lines = strings.Split(out, "\n")
	assert.Equal(c, len(lines), testLen+1)

	out = cli.DockerCmd(c, "logs", "--tail", "random", id).Combined()
	lines = strings.Split(out, "\n")
	assert.Equal(c, len(lines), testLen+1)
}

func (s *DockerSuite) TestLogsFollowStopped(c *check.C) {
	dockerCmd(c, "run", "--name=test", "busybox", "echo", "hello")
	id := getIDByName(c, "test")

	logsCmd := exec.Command(dockerBinary, "logs", "-f", id)
	assert.NilError(c, logsCmd.Start())

	errChan := make(chan error)
	go func() {
		errChan <- logsCmd.Wait()
		close(errChan)
	}()

	select {
	case err := <-errChan:
		assert.NilError(c, err)
	case <-time.After(30 * time.Second):
		c.Fatal("Following logs is hanged")
	}
}

func (s *DockerSuite) TestLogsSince(c *check.C) {
	name := "testlogssince"
	dockerCmd(c, "run", "--name="+name, "busybox", "/bin/sh", "-c", "for i in $(seq 1 3); do sleep 2; echo log$i; done")
	out, _ := dockerCmd(c, "logs", "-t", name)

	log2Line := strings.Split(strings.Split(out, "\n")[1], " ")
	t, err := time.Parse(time.RFC3339Nano, log2Line[0]) // the timestamp log2 is written
	assert.NilError(c, err)
	since := t.Unix() + 1 // add 1s so log1 & log2 doesn't show up
	out, _ = dockerCmd(c, "logs", "-t", fmt.Sprintf("--since=%v", since), name)

	// Skip 2 seconds
	unexpected := []string{"log1", "log2"}
	for _, v := range unexpected {
		assert.Check(c, !strings.Contains(out, v), "unexpected log message returned, since=%v", since)
	}

	// Test to make sure a bad since format is caught by the client
	out, _, _ = dockerCmdWithError("logs", "-t", "--since=2006-01-02T15:04:0Z", name)
	assert.Assert(c, strings.Contains(out, `cannot parse "0Z" as "05"`), "bad since format passed to server")

	// Test with default value specified and parameter omitted
	expected := []string{"log1", "log2", "log3"}
	for _, cmd := range [][]string{
		{"logs", "-t", name},
		{"logs", "-t", "--since=0", name},
	} {
		result := icmd.RunCommand(dockerBinary, cmd...)
		result.Assert(c, icmd.Success)
		for _, v := range expected {
			assert.Check(c, strings.Contains(result.Combined(), v))
		}
	}
}

func (s *DockerSuite) TestLogsSinceFutureFollow(c *check.C) {
	// TODO Windows TP5 - Figure out why this test is so flakey. Disabled for now.
	testRequires(c, DaemonIsLinux)
	name := "testlogssincefuturefollow"
	dockerCmd(c, "run", "-d", "--name", name, "busybox", "/bin/sh", "-c", `for i in $(seq 1 5); do echo log$i; sleep 1; done`)

	// Extract one timestamp from the log file to give us a starting point for
	// our `--since` argument. Because the log producer runs in the background,
	// we need to check repeatedly for some output to be produced.
	var timestamp string
	for i := 0; i != 100 && timestamp == ""; i++ {
		if out, _ := dockerCmd(c, "logs", "-t", name); out == "" {
			time.Sleep(time.Millisecond * 100) // Retry
		} else {
			timestamp = strings.Split(strings.Split(out, "\n")[0], " ")[0]
		}
	}

	assert.Assert(c, timestamp != "")
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	assert.NilError(c, err)

	since := t.Unix() + 2
	out, _ := dockerCmd(c, "logs", "-t", "-f", fmt.Sprintf("--since=%v", since), name)
	assert.Assert(c, len(out) != 0, "cannot read from empty log")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, v := range lines {
		ts, err := time.Parse(time.RFC3339Nano, strings.Split(v, " ")[0])
		assert.NilError(c, err, "cannot parse timestamp output from log: '%v'", v)
		assert.Assert(c, ts.Unix() >= since, "earlier log found. since=%v logdate=%v", since, ts)
	}
}

// Regression test for #8832
func (s *DockerSuite) TestLogsFollowSlowStdoutConsumer(c *check.C) {
	// TODO Windows: Fix this test for TP5.
	testRequires(c, DaemonIsLinux)
	expected := 150000
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", fmt.Sprintf("usleep 600000; yes X | head -c %d", expected))

	id := strings.TrimSpace(out)

	stopSlowRead := make(chan bool)

	go func() {
		dockerCmd(c, "wait", id)
		stopSlowRead <- true
	}()

	logCmd := exec.Command(dockerBinary, "logs", "-f", id)
	stdout, err := logCmd.StdoutPipe()
	assert.NilError(c, err)
	assert.NilError(c, logCmd.Start())
	defer func() { go logCmd.Wait() }()

	// First read slowly
	bytes1, err := ConsumeWithSpeed(stdout, 10, 50*time.Millisecond, stopSlowRead)
	assert.NilError(c, err)

	// After the container has finished we can continue reading fast
	bytes2, err := ConsumeWithSpeed(stdout, 32*1024, 0, nil)
	assert.NilError(c, err)

	assert.NilError(c, logCmd.Wait())

	actual := bytes1 + bytes2
	assert.Equal(c, actual, expected)
}

// ConsumeWithSpeed reads chunkSize bytes from reader before sleeping
// for interval duration. Returns total read bytes. Send true to the
// stop channel to return before reading to EOF on the reader.
func ConsumeWithSpeed(reader io.Reader, chunkSize int, interval time.Duration, stop chan bool) (n int, err error) {
	buffer := make([]byte, chunkSize)
	for {
		var readBytes int
		readBytes, err = reader.Read(buffer)
		n += readBytes
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
	}
}

func (s *DockerSuite) TestLogsFollowGoroutinesWithStdout(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "while true; do echo hello; sleep 2; done")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	nroutines, err := getGoroutineNumber()
	assert.NilError(c, err)
	cmd := exec.Command(dockerBinary, "logs", "-f", id)
	r, w := io.Pipe()
	cmd.Stdout = w
	assert.NilError(c, cmd.Start())
	go cmd.Wait()

	// Make sure pipe is written to
	chErr := make(chan error)
	go func() {
		b := make([]byte, 1)
		_, err := r.Read(b)
		chErr <- err
	}()
	assert.NilError(c, <-chErr)
	assert.NilError(c, cmd.Process.Kill())
	r.Close()
	cmd.Wait()
	// NGoroutines is not updated right away, so we need to wait before failing
	assert.NilError(c, waitForGoroutines(nroutines))
}

func (s *DockerSuite) TestLogsFollowGoroutinesNoOutput(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "while true; do sleep 2; done")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	nroutines, err := getGoroutineNumber()
	assert.NilError(c, err)
	cmd := exec.Command(dockerBinary, "logs", "-f", id)
	assert.NilError(c, cmd.Start())
	go cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	assert.NilError(c, cmd.Process.Kill())
	cmd.Wait()

	// NGoroutines is not updated right away, so we need to wait before failing
	assert.NilError(c, waitForGoroutines(nroutines))
}

func (s *DockerSuite) TestLogsCLIContainerNotFound(c *check.C) {
	name := "testlogsnocontainer"
	out, _, _ := dockerCmdWithError("logs", name)
	message := fmt.Sprintf("No such container: %s\n", name)
	assert.Assert(c, strings.Contains(out, message))
}

func (s *DockerSuite) TestLogsWithDetails(c *check.C) {
	dockerCmd(c, "run", "--name=test", "--label", "foo=bar", "-e", "baz=qux", "--log-opt", "labels=foo", "--log-opt", "env=baz", "busybox", "echo", "hello")
	out, _ := dockerCmd(c, "logs", "--details", "--timestamps", "test")

	logFields := strings.Fields(strings.TrimSpace(out))
	assert.Equal(c, len(logFields), 3, out)

	details := strings.Split(logFields[1], ",")
	assert.Equal(c, len(details), 2)
	assert.Equal(c, details[0], "baz=qux")
	assert.Equal(c, details[1], "foo=bar")
}

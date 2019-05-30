package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/test/request"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

func (s *DockerSuite) TestLogsAPIWithStdout(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "-t", "busybox", "/bin/sh", "-c", "while true; do echo hello; sleep 1; done")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	type logOut struct {
		out string
		err error
	}

	chLog := make(chan logOut)
	res, body, err := request.Get(fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&timestamps=1", id))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusOK)

	go func() {
		defer body.Close()
		out, err := bufio.NewReader(body).ReadString('\n')
		if err != nil {
			chLog <- logOut{"", err}
			return
		}
		chLog <- logOut{strings.TrimSpace(out), err}
	}()

	select {
	case l := <-chLog:
		assert.NilError(c, l.err)
		if !strings.HasSuffix(l.out, "hello") {
			c.Fatalf("expected log output to container 'hello', but it does not")
		}
	case <-time.After(30 * time.Second):
		c.Fatal("timeout waiting for logs to exit")
	}
}

func (s *DockerSuite) TestLogsAPINoStdoutNorStderr(c *check.C) {
	name := "logs_test"
	dockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")
	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer cli.Close()

	_, err = cli.ContainerLogs(context.Background(), name, types.ContainerLogsOptions{})
	assert.ErrorContains(c, err, "Bad parameters: you must choose at least one stream")
}

// Regression test for #12704
func (s *DockerSuite) TestLogsAPIFollowEmptyOutput(c *check.C) {
	name := "logs_test"
	t0 := time.Now()
	dockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "sleep", "10")

	_, body, err := request.Get(fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&stderr=1&tail=all", name))
	t1 := time.Now()
	assert.NilError(c, err)
	body.Close()
	elapsed := t1.Sub(t0).Seconds()
	if elapsed > 20.0 {
		c.Fatalf("HTTP response was not immediate (elapsed %.1fs)", elapsed)
	}
}

func (s *DockerSuite) TestLogsAPIContainerNotFound(c *check.C) {
	name := "nonExistentContainer"
	resp, _, err := request.Get(fmt.Sprintf("/containers/%s/logs?follow=1&stdout=1&stderr=1&tail=all", name))
	assert.NilError(c, err)
	assert.Equal(c, resp.StatusCode, http.StatusNotFound)
}

func (s *DockerSuite) TestLogsAPIUntilFutureFollow(c *check.C) {
	testRequires(c, DaemonIsLinux)
	name := "logsuntilfuturefollow"
	dockerCmd(c, "run", "-d", "--name", name, "busybox", "/bin/sh", "-c", "while true; do date +%s; sleep 1; done")
	assert.NilError(c, waitRun(name))

	untilSecs := 5
	untilDur, err := time.ParseDuration(fmt.Sprintf("%ds", untilSecs))
	assert.NilError(c, err)
	until := daemonTime(c).Add(untilDur)

	client, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	cfg := types.ContainerLogsOptions{Until: until.Format(time.RFC3339Nano), Follow: true, ShowStdout: true, Timestamps: true}
	reader, err := client.ContainerLogs(context.Background(), name, cfg)
	assert.NilError(c, err)

	type logOut struct {
		out string
		err error
	}

	chLog := make(chan logOut)

	go func() {
		bufReader := bufio.NewReader(reader)
		defer reader.Close()
		for i := 0; i < untilSecs; i++ {
			out, _, err := bufReader.ReadLine()
			if err != nil {
				if err == io.EOF {
					return
				}
				chLog <- logOut{"", err}
				return
			}

			chLog <- logOut{strings.TrimSpace(string(out)), err}
		}
	}()

	for i := 0; i < untilSecs; i++ {
		select {
		case l := <-chLog:
			assert.NilError(c, l.err)
			i, err := strconv.ParseInt(strings.Split(l.out, " ")[1], 10, 64)
			assert.NilError(c, err)
			assert.Assert(c, time.Unix(i, 0).UnixNano() <= until.UnixNano())
		case <-time.After(20 * time.Second):
			c.Fatal("timeout waiting for logs to exit")
		}
	}
}

func (s *DockerSuite) TestLogsAPIUntil(c *check.C) {
	testRequires(c, MinimumAPIVersion("1.34"))
	name := "logsuntil"
	dockerCmd(c, "run", "--name", name, "busybox", "/bin/sh", "-c", "for i in $(seq 1 3); do echo log$i; sleep 1; done")

	client, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	extractBody := func(c *check.C, cfg types.ContainerLogsOptions) []string {
		reader, err := client.ContainerLogs(context.Background(), name, cfg)
		assert.NilError(c, err)

		actualStdout := new(bytes.Buffer)
		actualStderr := ioutil.Discard
		_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
		assert.NilError(c, err)

		return strings.Split(actualStdout.String(), "\n")
	}

	// Get timestamp of second log line
	allLogs := extractBody(c, types.ContainerLogsOptions{Timestamps: true, ShowStdout: true})
	assert.Assert(c, len(allLogs) >= 3)

	t, err := time.Parse(time.RFC3339Nano, strings.Split(allLogs[1], " ")[0])
	assert.NilError(c, err)
	until := t.Format(time.RFC3339Nano)

	// Get logs until the timestamp of second line, i.e. first two lines
	logs := extractBody(c, types.ContainerLogsOptions{Timestamps: true, ShowStdout: true, Until: until})

	// Ensure log lines after cut-off are excluded
	logsString := strings.Join(logs, "\n")
	assert.Assert(c, !strings.Contains(logsString, "log3"), "unexpected log message returned, until=%v", until)
}

func (s *DockerSuite) TestLogsAPIUntilDefaultValue(c *check.C) {
	name := "logsuntildefaultval"
	dockerCmd(c, "run", "--name", name, "busybox", "/bin/sh", "-c", "for i in $(seq 1 3); do echo log$i; done")

	client, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		c.Fatal(err)
	}

	extractBody := func(c *check.C, cfg types.ContainerLogsOptions) []string {
		reader, err := client.ContainerLogs(context.Background(), name, cfg)
		assert.NilError(c, err)

		actualStdout := new(bytes.Buffer)
		actualStderr := ioutil.Discard
		_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
		assert.NilError(c, err)

		return strings.Split(actualStdout.String(), "\n")
	}

	// Get timestamp of second log line
	allLogs := extractBody(c, types.ContainerLogsOptions{Timestamps: true, ShowStdout: true})

	// Test with default value specified and parameter omitted
	defaultLogs := extractBody(c, types.ContainerLogsOptions{Timestamps: true, ShowStdout: true, Until: "0"})
	assert.DeepEqual(c, defaultLogs, allLogs)
}

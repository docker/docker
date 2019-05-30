package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/test/request"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/go-check/check"
	"github.com/pkg/errors"
	"golang.org/x/net/websocket"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func (s *DockerSuite) TestGetContainersAttachWebsocket(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-dit", "busybox", "cat")

	rwc, err := request.SockConn(time.Duration(10*time.Second), request.DaemonHost())
	assert.NilError(c, err)

	cleanedContainerID := strings.TrimSpace(out)
	config, err := websocket.NewConfig(
		"/containers/"+cleanedContainerID+"/attach/ws?stream=1&stdin=1&stdout=1&stderr=1",
		"http://localhost",
	)
	assert.NilError(c, err)

	ws, err := websocket.NewClient(config, rwc)
	assert.NilError(c, err)
	defer ws.Close()

	expected := []byte("hello")
	actual := make([]byte, len(expected))

	outChan := make(chan error)
	go func() {
		_, err := io.ReadFull(ws, actual)
		outChan <- err
		close(outChan)
	}()

	inChan := make(chan error)
	go func() {
		_, err := ws.Write(expected)
		inChan <- err
		close(inChan)
	}()

	select {
	case err := <-inChan:
		assert.NilError(c, err)
	case <-time.After(5 * time.Second):
		c.Fatal("Timeout writing to ws")
	}

	select {
	case err := <-outChan:
		assert.NilError(c, err)
	case <-time.After(5 * time.Second):
		c.Fatal("Timeout reading from ws")
	}

	assert.Assert(c, is.DeepEqual(actual, expected), "Websocket didn't return the expected data")
}

// regression gh14320
func (s *DockerSuite) TestPostContainersAttachContainerNotFound(c *check.C) {
	resp, _, err := request.Post("/containers/doesnotexist/attach")
	assert.NilError(c, err)
	// connection will shutdown, err should be "persistent connection closed"
	assert.Equal(c, resp.StatusCode, http.StatusNotFound)
	content, err := request.ReadBody(resp.Body)
	assert.NilError(c, err)
	expected := "No such container: doesnotexist\r\n"
	assert.Equal(c, string(content), expected)
}

func (s *DockerSuite) TestGetContainersWsAttachContainerNotFound(c *check.C) {
	res, body, err := request.Get("/containers/doesnotexist/attach/ws")
	assert.Equal(c, res.StatusCode, http.StatusNotFound)
	assert.NilError(c, err)
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	expected := "No such container: doesnotexist"
	assert.Assert(c, strings.Contains(getErrorMessage(c, b), expected))
}

func (s *DockerSuite) TestPostContainersAttach(c *check.C) {
	testRequires(c, DaemonIsLinux)

	expectSuccess := func(conn net.Conn, br *bufio.Reader, stream string, tty bool) {
		defer conn.Close()
		expected := []byte("success")
		_, err := conn.Write(expected)
		assert.NilError(c, err)

		conn.SetReadDeadline(time.Now().Add(time.Second))
		lenHeader := 0
		if !tty {
			lenHeader = 8
		}
		actual := make([]byte, len(expected)+lenHeader)
		_, err = io.ReadFull(br, actual)
		assert.NilError(c, err)
		if !tty {
			fdMap := map[string]byte{
				"stdin":  0,
				"stdout": 1,
				"stderr": 2,
			}
			assert.Equal(c, actual[0], fdMap[stream])
		}
		assert.Assert(c, is.DeepEqual(actual[lenHeader:], expected), "Attach didn't return the expected data from %s", stream)
	}

	expectTimeout := func(conn net.Conn, br *bufio.Reader, stream string) {
		defer conn.Close()
		_, err := conn.Write([]byte{'t'})
		assert.NilError(c, err)

		conn.SetReadDeadline(time.Now().Add(time.Second))
		actual := make([]byte, 1)
		_, err = io.ReadFull(br, actual)
		opErr, ok := err.(*net.OpError)
		assert.Assert(c, ok, "Error is expected to be *net.OpError, got %v", err)
		assert.Assert(c, opErr.Timeout(), "Read from %s is expected to timeout", stream)
	}

	// Create a container that only emits stdout.
	cid, _ := dockerCmd(c, "run", "-di", "busybox", "cat")
	cid = strings.TrimSpace(cid)
	// Attach to the container's stdout stream.
	conn, br, err := sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stdout=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	// Check if the data from stdout can be received.
	expectSuccess(conn, br, "stdout", false)
	// Attach to the container's stderr stream.
	conn, br, err = sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stderr=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	// Since the container only emits stdout, attaching to stderr should return nothing.
	expectTimeout(conn, br, "stdout")

	// Test the similar functions of the stderr stream.
	cid, _ = dockerCmd(c, "run", "-di", "busybox", "/bin/sh", "-c", "cat >&2")
	cid = strings.TrimSpace(cid)
	conn, br, err = sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stderr=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	expectSuccess(conn, br, "stderr", false)
	conn, br, err = sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stdout=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	expectTimeout(conn, br, "stderr")

	// Test with tty.
	cid, _ = dockerCmd(c, "run", "-dit", "busybox", "/bin/sh", "-c", "cat >&2")
	cid = strings.TrimSpace(cid)
	// Attach to stdout only.
	conn, br, err = sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stdout=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	expectSuccess(conn, br, "stdout", true)

	// Attach without stdout stream.
	conn, br, err = sockRequestHijack("POST", "/containers/"+cid+"/attach?stream=1&stdin=1&stderr=1", nil, "text/plain", request.DaemonHost())
	assert.NilError(c, err)
	// Nothing should be received because both the stdout and stderr of the container will be
	// sent to the client as stdout when tty is enabled.
	expectTimeout(conn, br, "stdout")

	// Test the client API
	client, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer client.Close()

	cid, _ = dockerCmd(c, "run", "-di", "busybox", "/bin/sh", "-c", "echo hello; cat")
	cid = strings.TrimSpace(cid)

	// Make sure we don't see "hello" if Logs is false
	attachOpts := types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Logs:   false,
	}

	resp, err := client.ContainerAttach(context.Background(), cid, attachOpts)
	assert.NilError(c, err)
	expectSuccess(resp.Conn, resp.Reader, "stdout", false)

	// Make sure we do see "hello" if Logs is true
	attachOpts.Logs = true
	resp, err = client.ContainerAttach(context.Background(), cid, attachOpts)
	assert.NilError(c, err)

	defer resp.Conn.Close()
	resp.Conn.SetReadDeadline(time.Now().Add(time.Second))

	_, err = resp.Conn.Write([]byte("success"))
	assert.NilError(c, err)

	var outBuf, errBuf bytes.Buffer
	_, err = stdcopy.StdCopy(&outBuf, &errBuf, resp.Reader)
	if err != nil && errors.Cause(err).(net.Error).Timeout() {
		// ignore the timeout error as it is expected
		err = nil
	}
	assert.NilError(c, err)
	assert.Equal(c, errBuf.String(), "")
	assert.Equal(c, outBuf.String(), "hello\nsuccess")
}

// SockRequestHijack creates a connection to specified host (with method, contenttype, …) and returns a hijacked connection
// and the output as a `bufio.Reader`
func sockRequestHijack(method, endpoint string, data io.Reader, ct string, daemon string, modifiers ...func(*http.Request)) (net.Conn, *bufio.Reader, error) {
	req, client, err := newRequestClient(method, endpoint, data, ct, daemon, modifiers...)
	if err != nil {
		return nil, nil, err
	}

	client.Do(req)
	conn, br := client.Hijack()
	return conn, br, nil
}

// FIXME(vdemeester) httputil.ClientConn is deprecated, use http.Client instead (closer to actual client)
// Deprecated: Use New instead of NewRequestClient
// Deprecated: use request.Do (or Get, Delete, Post) instead
func newRequestClient(method, endpoint string, data io.Reader, ct, daemon string, modifiers ...func(*http.Request)) (*http.Request, *httputil.ClientConn, error) {
	c, err := request.SockConn(time.Duration(10*time.Second), daemon)
	if err != nil {
		return nil, nil, fmt.Errorf("could not dial docker daemon: %v", err)
	}

	client := httputil.NewClientConn(c, nil)

	req, err := http.NewRequest(method, endpoint, data)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("could not create new request: %v", err)
	}

	for _, opt := range modifiers {
		opt(req)
	}

	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	return req, client, nil
}

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestExecCreateError(t *testing.T) {
	client, err := New(
		WithMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	)
	assert.NilError(t, err)

	_, err = client.ExecCreate(t.Context(), "container_id", ExecCreateOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))

	_, err = client.ExecCreate(t.Context(), "", ExecCreateOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))

	_, err = client.ExecCreate(t.Context(), "    ", ExecCreateOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))
}

// TestExecCreateConnectionError verifies that connection errors occurring
// during API-version negotiation are not shadowed by API-version errors.
//
// Regression test for https://github.com/docker/cli/issues/4890
func TestExecCreateConnectionError(t *testing.T) {
	client, err := New(WithHost("tcp://no-such-host.invalid"))
	assert.NilError(t, err)

	_, err = client.ExecCreate(t.Context(), "container_id", ExecCreateOptions{})
	assert.Check(t, is.ErrorType(err, IsErrConnectionFailed))
}

func TestExecCreate(t *testing.T) {
	const expectedURL = "/containers/container_id/exec"

	expectedReq := container.ExecCreateRequest{
		User:         "user",
		Privileged:   true,
		Tty:          true,
		ConsoleSize:  &[2]uint{100, 200},
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		DetachKeys:   "ctrl-p,ctrl-q",
		Env:          []string{"FOO=bar"},
		WorkingDir:   "/app",
		Cmd:          []string{"sh", "-c", "echo hello"},
	}

	var actualReq container.ExecCreateRequest
	client, err := New(
		WithMockClient(func(req *http.Request) (*http.Response, error) {
			if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
				return nil, err
			}
			if err := json.NewDecoder(req.Body).Decode(&actualReq); err != nil {
				return nil, err
			}
			return mockJSONResponse(http.StatusOK, nil, container.ExecCreateResponse{
				ID: "exec_id",
			})(req)
		}),
	)
	assert.NilError(t, err)

	res, err := client.ExecCreate(t.Context(), "container_id", ExecCreateOptions{
		User:         "user",
		Privileged:   true,
		TTY:          true,
		ConsoleSize:  ConsoleSize{Height: 100, Width: 200},
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		DetachKeys:   "ctrl-p,ctrl-q",
		Env:          []string{"FOO=bar"},
		WorkingDir:   "/app",
		Cmd:          []string{"sh", "-c", "echo hello"},
	})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ID, "exec_id"))
	assert.Check(t, is.DeepEqual(actualReq, expectedReq))
}

func TestExecStartError(t *testing.T) {
	client, err := New(
		WithMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	)
	assert.NilError(t, err)

	_, err = client.ExecStart(t.Context(), "nothing", ExecStartOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
}

func TestExecStart(t *testing.T) {
	const expectedURL = "/exec/exec_id/start"
	client, err := New(
		WithMockClient(func(req *http.Request) (*http.Response, error) {
			if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
				return nil, err
			}
			if err := req.ParseForm(); err != nil {
				return nil, err
			}
			request := &container.ExecStartRequest{}
			if err := json.NewDecoder(req.Body).Decode(request); err != nil {
				return nil, err
			}
			if request.Tty || !request.Detach {
				return nil, fmt.Errorf("expected ExecStartOptions{Detach:true,Tty:false}, got %v", request)
			}
			return mockResponse(http.StatusOK, nil, "")(req)
		}),
	)
	assert.NilError(t, err)

	_, err = client.ExecStart(t.Context(), "exec_id", ExecStartOptions{
		Detach: true,
		TTY:    false,
	})
	assert.NilError(t, err)
}

func TestExecStartConsoleSize(t *testing.T) {
	tests := []struct {
		doc     string
		options ExecStartOptions
		expErr  string
		expReq  container.ExecStartRequest
	}{
		{
			doc: "without TTY",
			options: ExecStartOptions{
				Detach:      true,
				TTY:         false,
				ConsoleSize: ConsoleSize{Height: 100, Width: 200},
			},
			expErr: "console size is only supported when TTY is enabled",
		},
		{
			doc: "with TTY",
			options: ExecStartOptions{
				Detach:      true,
				TTY:         true,
				ConsoleSize: ConsoleSize{Height: 100, Width: 200},
			},
			expReq: container.ExecStartRequest{
				Detach:      true,
				Tty:         true,
				ConsoleSize: &[2]uint{100, 200},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			var actualReq container.ExecStartRequest
			client, err := New(
				WithMockClient(func(req *http.Request) (*http.Response, error) {
					if tc.expErr != "" {
						return nil, errors.New("should not have made API request")
					}
					if err := json.NewDecoder(req.Body).Decode(&actualReq); err != nil {
						return nil, err
					}

					return mockJSONResponse(http.StatusOK, nil, ExecStartResult{})(req)
				}),
			)
			assert.NilError(t, err)

			_, err = client.ExecStart(t.Context(), "exec_id", tc.options)
			if tc.expErr != "" {
				assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
				assert.Check(t, is.ErrorContains(err, tc.expErr))
				assert.Check(t, is.DeepEqual(actualReq, tc.expReq))
			} else {
				assert.NilError(t, err)
				assert.Check(t, is.DeepEqual(actualReq, tc.expReq))
			}
		})
	}
}

func TestExecInspectError(t *testing.T) {
	client, err := New(
		WithMockClient(errorMock(http.StatusInternalServerError, "Server error")),
	)
	assert.NilError(t, err)

	_, err = client.ExecInspect(t.Context(), "nothing", ExecInspectOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
}

func TestExecInspect(t *testing.T) {
	const expectedURL = "/exec/exec_id/json"
	client, err := New(
		WithMockClient(func(req *http.Request) (*http.Response, error) {
			if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
				return nil, err
			}
			return mockJSONResponse(http.StatusOK, nil, container.ExecInspectResponse{
				ID:          "exec_id",
				ContainerID: "container_id",
			})(req)
		}),
	)
	assert.NilError(t, err)

	inspect, err := client.ExecInspect(t.Context(), "exec_id", ExecInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.ID, "exec_id"))
	assert.Check(t, is.Equal(inspect.ContainerID, "container_id"))
}

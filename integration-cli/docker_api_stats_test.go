package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func (s *DockerAPISuite) TestAPIStatsStoppedContainerInGoroutines(c *testing.T) {
	out := cli.DockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "echo 1").Stdout()
	id := strings.TrimSpace(out)

	getGoRoutines := func() int {
		_, body, err := request.Get(testutil.GetContext(c), "/info")
		assert.NilError(c, err)
		info := system.Info{}
		err = json.NewDecoder(body).Decode(&info)
		assert.NilError(c, err)
		_ = body.Close()
		return info.NGoroutines
	}

	// When the HTTP connection is closed, the number of goroutines should not increase.
	routines := getGoRoutines()
	_, body, err := request.Get(testutil.GetContext(c), "/containers/"+id+"/stats")
	assert.NilError(c, err)
	_ = body.Close()

	t := time.After(30 * time.Second)
	for {
		select {
		case <-t:
			assert.Assert(c, getGoRoutines() <= routines)
			return
		default:
			if n := getGoRoutines(); n <= routines {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (s *DockerAPISuite) TestAPIStatsNoStreamConnectedContainers(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	id1 := runSleepingContainer(c)
	cli.WaitRun(c, id1)

	id2 := runSleepingContainer(c, "--net", "container:"+id1)
	cli.WaitRun(c, id2)

	// We expect an immediate response; use a timeout to avoid hanging.
	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 10*time.Second)
	defer cancel()

	resp, body, err := request.Get(ctx, "/containers/"+id2+"/stats?stream=false&one-shot=true")
	assert.NilError(c, err)
	defer func() { _ = body.Close() }()

	assert.Check(c, is.Equal(resp.StatusCode, http.StatusOK), "invalid StatusCode %v", resp.StatusCode)
	assert.Check(c, is.Equal(resp.Header.Get("Content-Type"), "application/json"), "invalid 'Content-Type' %v", resp.Header.Get("Content-Type"))

	var v container.StatsResponse
	dec := json.NewDecoder(body)
	assert.NilError(c, dec.Decode(&v))
	assert.Check(c, is.Equal(v.ID, id2))
	err = dec.Decode(&v)
	assert.Check(c, is.ErrorIs(err, io.EOF), "expected only a single result")
}

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func (s *DockerAPISuite) TestAPIStatsNoStreamGetCpu(c *testing.T) {
	skip.If(c, RuntimeIsWindowsContainerd(), "FIXME: Broken on Windows + containerd combination")
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")
	out := cli.DockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "while true;usleep 100; do echo 'Hello'; done").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)
	resp, body, err := request.Get(testutil.GetContext(c), fmt.Sprintf("/containers/%s/stats?stream=false", id))
	assert.NilError(c, err)
	assert.Equal(c, resp.StatusCode, http.StatusOK)
	assert.Equal(c, resp.Header.Get("Content-Type"), "application/json")
	assert.Equal(c, resp.Header.Get("Content-Type"), "application/json")

	var v container.StatsResponse
	err = json.NewDecoder(body).Decode(&v)
	assert.NilError(c, err)
	_ = body.Close()

	cpuPercent := 0.0

	if testEnv.DaemonInfo.OSType != "windows" {
		cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage)
		systemDelta := float64(v.CPUStats.SystemUsage - v.PreCPUStats.SystemUsage)
		cpuPercent = (cpuDelta / systemDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100.0
	} else {
		// Max number of 100ns intervals between the previous time read and now
		possIntervals := uint64(v.Read.Sub(v.PreRead).Nanoseconds()) // Start with number of ns intervals
		possIntervals /= 100                                         // Convert to number of 100ns intervals
		possIntervals *= uint64(v.NumProcs)                          // Multiple by the number of processors

		// Intervals used
		intervalsUsed := v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage

		// Percentage avoiding divide-by-zero
		if possIntervals > 0 {
			cpuPercent = float64(intervalsUsed) / float64(possIntervals) * 100.0
		}
	}

	assert.Assert(c, cpuPercent != 0.0, "docker stats with no-stream get cpu usage failed: was %v", cpuPercent)
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

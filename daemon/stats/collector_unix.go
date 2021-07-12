// +build !windows

package stats // import "github.com/docker/docker/daemon/stats"

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	// The value comes from `C.sysconf(C._SC_CLK_TCK)`, and
	// on Linux it's a constant which is safe to be hard coded,
	// so we can avoid using cgo here. For details, see:
	// https://github.com/containerd/cgroups/pull/12
	clockTicksPerSecond  uint64 = 100
	nanoSecondsPerSecond        = 1e9
	nanoSecondsPerTick          = nanoSecondsPerSecond / clockTicksPerSecond
)

// getSystemCPUUsage returns the host system's cpu usage in
// nanoseconds. An error is returned if the format of the underlying
// file does not match.
//
// Uses /proc/stat defined by POSIX. Looks for the cpu
// statistics line and then sums up the first seven fields
// provided. See `man 5 proc` for details on specific field
// information.
func (s *Collector) getSystemCPUUsage() (uint64, error) {
	procStatFile, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer procStatFile.Close()

	return s.cpuNanoSeconds(procStatFile)
}

// cpuNanoSeconds calculates and returns CPU time in nanoseconds
// given a file in the same format as /proc/stat
func (s *Collector) cpuNanoSeconds(procStatFile io.Reader) (uint64, error) {
	defer s.bufReader.Reset(nil)
	s.bufReader.Reset(procStatFile)

	for {
		line, err := s.bufReader.ReadString('\n')
		if err != nil {
			break
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "cpu":
			if len(parts) < 8 {
				return 0, fmt.Errorf("invalid number of cpu fields")
			}
			var totalClockTicks uint64
			for _, i := range parts[1:8] {
				v, err := strconv.ParseUint(i, 10, 64)
				if err != nil {
					return 0, fmt.Errorf("unable to convert value %s to int: %s", i, err)
				}
				totalClockTicks += v
			}

			// This may result in a uint64 overflow which will result in an incorrect number
			// being reported. However, since all CPU calculations are currently done
			// in relation to one another, they will be correct after both points being
			// compared have overflowed.
			return totalClockTicks * nanoSecondsPerTick, nil
		}
	}

	return 0, fmt.Errorf("invalid format while trying to parse proc stat file")
}

func (s *Collector) getNumberOnlineCPUs() (uint32, error) {
	var cpuset unix.CPUSet
	err := unix.SchedGetaffinity(0, &cpuset)
	if err != nil {
		return 0, err
	}
	return uint32(cpuset.Count()), nil
}

package specconv // import "github.com/docker/docker/rootless/specconv"

import (
	"io/ioutil"
	"strconv"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// ToRootless converts spec to be compatible with "rootless" runc.
// * Remove cgroups (will be supported in separate PR when delegation permission is configured)
// * Fix up OOMScoreAdj
func ToRootless(spec *specs.Spec) error {
	return toRootless(spec, getCurrentOOMScoreAdj())
}

func getCurrentOOMScoreAdj() int {
	b, err := ioutil.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		return 0
	}
	i, err := strconv.Atoi(string(b))
	if err != nil {
		return 0
	}
	return i
}

func toRootless(spec *specs.Spec, currentOOMScoreAdj int) error {
	// Remove cgroup settings.
	spec.Linux.Resources = nil
	spec.Linux.CgroupsPath = ""

	if spec.Process.OOMScoreAdj != nil && *spec.Process.OOMScoreAdj < currentOOMScoreAdj {
		*spec.Process.OOMScoreAdj = currentOOMScoreAdj
	}
	return nil
}

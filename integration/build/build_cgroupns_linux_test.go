package build // import "github.com/docker/docker/integration/build"

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration/internal/requirement"
	"github.com/docker/docker/internal/test/daemon"
	"github.com/docker/docker/internal/test/fakecontext"
	"github.com/docker/docker/pkg/jsonmessage"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

// Finds the output of `readlink /proc/<pid>/ns/cgroup` in build output
func getCgroupFromBuildOutput(buildOutput io.Reader) (string, error) {
	const prefix = "cgroup:"

	dec := json.NewDecoder(buildOutput)
	for {
		m := jsonmessage.JSONMessage{}
		err := dec.Decode(&m)
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if ix := strings.Index(m.Stream, prefix); ix == 0 {
			return strings.TrimSpace(m.Stream), nil
		}
	}
}

// Runs a docker build against a daemon with the given cgroup namespace default value.
// Returns the container cgroup and daemon cgroup.
func testBuildWithCgroupNs(t *testing.T, daemonNsMode string) (string, string) {
	d := daemon.New(t, daemon.WithDefaultCgroupNamespaceMode(daemonNsMode))
	d.StartWithBusybox(t)
	defer d.Stop(t)

	dockerfile := `
		FROM busybox
		RUN readlink /proc/self/ns/cgroup
	`
	ctx := context.Background()
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	client := d.NewClientT(t)
	resp, err := client.ImageBuild(ctx,
		source.AsTarReader(t),
		types.ImageBuildOptions{
			Remove:      true,
			ForceRemove: true,
			Tags:        []string{"buildcgroupns"},
		})
	assert.NilError(t, err)
	defer resp.Body.Close()

	containerCgroup, err := getCgroupFromBuildOutput(resp.Body)
	assert.NilError(t, err)
	daemonCgroup := d.CgroupNamespace(t)

	return containerCgroup, daemonCgroup
}

func TestCgroupNamespacesBuild(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to private cgroup namespaces, containers launched
	// should be in their own private cgroup namespace by default
	containerCgroup, daemonCgroup := testBuildWithCgroupNs(t, "private")
	assert.Assert(t, daemonCgroup != containerCgroup)
}

func TestCgroupNamespacesBuildDaemonHostMode(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !requirement.CgroupNamespacesEnabled())

	// When the daemon defaults to host cgroup namespaces, containers
	// launched should not be inside their own cgroup namespaces
	containerCgroup, daemonCgroup := testBuildWithCgroupNs(t, "host")
	assert.Assert(t, daemonCgroup == containerCgroup)
}

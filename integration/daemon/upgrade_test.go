package daemon

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fixtures/load"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// dockerdPreviousBinaryEnv is the environment variable holding the path of the
// dockerd binary of the previous stable release to upgrade from.
const dockerdPreviousBinaryEnv = "DOCKERD_PREVIOUS_BINARY"

// TestUpgradeFromPreviousDaemon creates state with the previous stable dockerd,
// verifies that the state is intact after restarting that same daemon, and then
// starts the dockerd under test against the same data-root to verify the state
// once more. It covers state that is carried from one Engine version to another,
// which tests that create and consume state with a single daemon version cannot.
func TestUpgradeFromPreviousDaemon(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux")
	skip.If(t, testEnv.IsRootless(), "the previous daemon is not set up for rootless mode")
	prevBinary := os.Getenv(dockerdPreviousBinaryEnv)
	skip.If(t, prevBinary == "", "no previous dockerd: set "+dockerdPreviousBinaryEnv+" to its path")

	ctx := testutil.StartSpan(baseContext, t)

	const (
		ctrName    = "upgrade-ctr"
		volName    = "upgrade-vol"
		netName    = "upgrade-net"
		volTarget  = "/vol"
		markerFile = volTarget + "/marker"
		markerData = "written by the previous daemon"
	)
	daemonArgs := []string{"--iptables=false", "--ip6tables=false"}

	d := daemon.New(t, daemon.WithDockerdBinary(prevBinary))
	defer d.Cleanup(t)
	defer d.Stop(t)

	// Start the previous stable dockerd and create state with it.
	d.Start(t, daemonArgs...)
	apiClient := d.NewClientT(t)
	prevVersion, err := apiClient.ServerVersion(ctx, client.ServerVersionOptions{})
	assert.NilError(t, err)
	t.Logf("upgrading from dockerd %s", prevVersion.Version)

	assert.NilError(t, load.FrozenImagesLinux(ctx, apiClient, "busybox:latest"))

	_, err = apiClient.VolumeCreate(ctx, client.VolumeCreateOptions{Name: volName})
	assert.NilError(t, err)
	network.CreateNoError(ctx, t, apiClient, netName, network.WithDriver("bridge"))

	ctrID := container.Run(ctx, t, apiClient,
		container.WithName(ctrName),
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithNetworkMode(netName),
		container.WithMount(mount.Mount{Type: mount.TypeVolume, Source: volName, Target: volTarget}),
	)

	res, err := container.Exec(ctx, apiClient, ctrID, []string{"sh", "-c", "echo -n " + markerData + " > " + markerFile})
	assert.NilError(t, err)
	res.AssertSuccess(t)

	_, err = apiClient.ContainerStop(ctx, ctrID, client.ContainerStopOptions{})
	assert.NilError(t, err)

	// The state as described by the daemon that wrote it; the daemon reading it
	// back after a restart, and the daemon under test after the upgrade, must
	// describe the same state.
	want := readState(ctx, t, apiClient, ctrID, volName, netName)

	// Restart the same daemon to establish a baseline.
	d.Restart(t, daemonArgs...)
	apiClient = d.NewClientT(t)
	assert.DeepEqual(t, readState(ctx, t, apiClient, ctrID, volName, netName), want)
	assert.Equal(t, readMarker(ctx, t, apiClient, "baseline", volName, volTarget, markerFile), markerData)

	// Start the daemon under test against the same data-root.
	d.Stop(t)
	d.UseBinary(t, daemon.DefaultDockerdBinary)
	d.Start(t, daemonArgs...)
	apiClient = d.NewClientT(t)
	version, err := apiClient.ServerVersion(ctx, client.ServerVersionOptions{})
	assert.NilError(t, err)
	t.Logf("upgraded to dockerd %s", version.Version)

	assert.DeepEqual(t, readState(ctx, t, apiClient, ctrID, volName, netName), want)
	assert.Equal(t, readMarker(ctx, t, apiClient, "upgraded", volName, volTarget, markerFile), markerData)

	// The container created by the previous daemon must still start.
	_, err = apiClient.ContainerStart(ctx, ctrID, client.ContainerStartOptions{})
	assert.NilError(t, err)
	poll.WaitOn(t, container.RunningStateFlagIs(ctx, apiClient, ctrID, true))
}

// state holds the parts of the daemon's state that must survive an upgrade.
// Fields that are allowed to differ between daemon versions (such as the daemon
// version itself, or paths under the daemon's exec-root) are not included.
type state struct {
	DaemonID    string
	Images      int
	ContainerNm string
	ImageRef    string
	Cmd         []string
	Mounts      []string
	Networks    []string
	VolumeMount string
	NetworkID   string
}

func readState(ctx context.Context, t *testing.T, apiClient client.APIClient, ctrID, volName, netName string) state {
	t.Helper()

	info, err := apiClient.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	inspect, err := apiClient.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	ctr := inspect.Container

	vol, err := apiClient.VolumeInspect(ctx, volName, client.VolumeInspectOptions{})
	assert.NilError(t, err)

	nw, err := apiClient.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	assert.NilError(t, err)

	st := state{
		DaemonID:    info.Info.ID,
		Images:      info.Info.Images,
		ContainerNm: ctr.Name,
		ImageRef:    ctr.Config.Image,
		Cmd:         ctr.Config.Cmd,
		VolumeMount: vol.Volume.Mountpoint,
		NetworkID:   nw.Network.ID,
	}
	for _, m := range ctr.Mounts {
		st.Mounts = append(st.Mounts, m.Name+":"+m.Destination)
	}
	for nwName := range ctr.NetworkSettings.Networks {
		st.Networks = append(st.Networks, nwName)
	}
	slices.Sort(st.Mounts)
	slices.Sort(st.Networks)
	return st
}

// readMarker reads the marker file from the volume with a throw-away container.
func readMarker(ctx context.Context, t *testing.T, apiClient client.APIClient, name, volName, volTarget, markerFile string) string {
	t.Helper()

	res := container.RunAttach(ctx, t, apiClient,
		container.WithName("upgrade-read-marker-"+name),
		container.WithImage("busybox:latest"),
		container.WithCmd("cat", markerFile),
		container.WithMount(mount.Mount{Type: mount.TypeVolume, Source: volName, Target: volTarget}),
	)
	assert.Equal(t, res.ExitCode, 0, res.Stderr.String())
	defer container.Remove(ctx, t, apiClient, res.ContainerID, client.ContainerRemoveOptions{Force: true})
	return res.Stdout.String()
}

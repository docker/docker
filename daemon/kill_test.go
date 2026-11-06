package daemon

import (
	"context"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"gotest.tools/v3/assert"
)

type notFoundKillTask struct {
	libcontainerdtypes.Task
	deleteCalled chan struct{}
}

type killContextKey struct{}

type contextKillTask struct {
	libcontainerdtypes.Task
	killContext chan context.Context
}

func (t *contextKillTask) Pid() uint32 {
	return 1
}

func (t *contextKillTask) Kill(ctx context.Context, _ syscall.Signal) error {
	t.killContext <- ctx
	return nil
}

func (t *notFoundKillTask) Pid() uint32 {
	return 1
}

func (t *notFoundKillTask) Kill(context.Context, syscall.Signal) error {
	return cerrdefs.ErrNotFound
}

func (t *notFoundKillTask) Delete(ctx context.Context) (*containerd.ExitStatus, error) {
	close(t.deleteCalled)
	<-ctx.Done()
	return nil, ctx.Err()
}

type noopKillTask struct {
	libcontainerdtypes.Task
}

func (t *noopKillTask) Pid() uint32 {
	return 1
}

func (t *noopKillTask) Kill(context.Context, syscall.Signal) error {
	return nil
}

// TestKillWithSignalDoesNotMarkManuallyStoppedForNonStopSignal covers a real-world
// pattern: a reverse proxy (docker-gen, nginx-proxy, etc.) sends a non-terminating
// signal like SIGHUP via `docker kill` to trigger a config reload. That must not be
// treated the same as a user running `docker stop` -- the container is still
// running, and unless-stopped's restart policy relies on HasBeenManuallyStopped to
// tell the two apart. Images without a STOPSIGNAL (Config.StopSignal == "") are
// the common case and must behave the same way.
func TestKillWithSignalDoesNotMarkManuallyStoppedForNonStopSignal(t *testing.T) {
	for _, stopSignal := range []string{"", "SIGTERM", "SIGQUIT"} {
		t.Run("StopSignal="+stopSignal, func(t *testing.T) {
			task := &noopKillTask{}
			ctr := container.NewBaseContainer(t.Name(), t.TempDir())
			ctr.Config = &containertypes.Config{StopSignal: stopSignal}
			ctr.HostConfig = &containertypes.HostConfig{}
			ctr.Lock()
			ctr.State.SetRunning(nil, task, time.Now())
			ctr.Unlock()

			daemon := &Daemon{
				EventsService: events.New(),
			}

			err := daemon.killWithSignal(t.Context(), ctr, syscall.SIGHUP)
			assert.NilError(t, err)
			assert.Equal(t, ctr.HasBeenManuallyStopped, false)
		})
	}
}

// TestKillWithSignalMarksManuallyStoppedForRealStopSignal is the flip side of
// TestKillWithSignalDoesNotMarkManuallyStoppedForNonStopSignal: the container's
// actual stop signal (the configured one, or SIGTERM when none is configured)
// and SIGKILL must still be treated as a real stop, so unless-stopped correctly
// does not auto-restart it afterward.
func TestKillWithSignalMarksManuallyStoppedForRealStopSignal(t *testing.T) {
	for _, tc := range []struct {
		stopSignal string
		signal     syscall.Signal
	}{
		{stopSignal: "", signal: syscall.SIGTERM},
		{stopSignal: "", signal: syscall.SIGKILL},
		{stopSignal: "SIGTERM", signal: syscall.SIGTERM},
		{stopSignal: "SIGQUIT", signal: syscall.SIGQUIT},
		{stopSignal: "SIGQUIT", signal: syscall.SIGKILL},
	} {
		t.Run("StopSignal="+tc.stopSignal+"/"+tc.signal.String(), func(t *testing.T) {
			task := &noopKillTask{}
			ctr := container.NewBaseContainer(t.Name(), t.TempDir())
			ctr.Config = &containertypes.Config{StopSignal: tc.stopSignal}
			ctr.HostConfig = &containertypes.HostConfig{}
			ctr.Lock()
			ctr.State.SetRunning(nil, task, time.Now())
			ctr.Unlock()

			containersReplica, err := container.NewViewDB()
			assert.NilError(t, err)
			assert.NilError(t, containersReplica.Save(ctr))

			daemon := &Daemon{
				EventsService:     events.New(),
				containersReplica: containersReplica,
			}

			err = daemon.killWithSignal(t.Context(), ctr, tc.signal)
			assert.NilError(t, err)
			assert.Equal(t, ctr.HasBeenManuallyStopped, true)
		})
	}
}

func TestKillWithSignalWaitsIndefinitelyForDelayedExit(t *testing.T) {
	task := &notFoundKillTask{deleteCalled: make(chan struct{})}
	ctr := container.NewBaseContainer(t.Name(), t.TempDir())
	stopTimeout := -1
	ctr.Config = &containertypes.Config{StopTimeout: &stopTimeout}
	ctr.HostConfig = &containertypes.HostConfig{}
	ctr.Lock()
	ctr.State.SetRunning(nil, task, time.Now())
	ctr.Unlock()

	daemon := &Daemon{
		EventsService: events.New(),
		shutdown:      true,
	}

	err := daemon.killWithSignal(t.Context(), ctr, syscall.SIGKILL)
	assert.NilError(t, err)

	select {
	case <-task.deleteCalled:
		t.Fatal("fallback exit handling ran before the delayed exit event")
	case <-time.After(100 * time.Millisecond):
	}

	ctr.Lock()
	ctr.State.SetStopped(&container.ExitStatus{})
	ctr.Unlock()
}

func TestKillWithSignalDetachesContext(t *testing.T) {
	task := &contextKillTask{killContext: make(chan context.Context, 1)}
	ctr := container.NewBaseContainer(t.Name(), t.TempDir())
	ctr.Config = &containertypes.Config{}
	ctr.HostConfig = &containertypes.HostConfig{}
	ctr.Lock()
	ctr.State.SetRunning(nil, task, time.Now())
	ctr.Unlock()

	daemon := &Daemon{
		EventsService: events.New(),
		shutdown:      true,
	}

	const contextValue = "value"
	ctx := context.WithValue(t.Context(), killContextKey{}, contextValue)
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	err := daemon.killWithSignal(ctx, ctr, syscall.SIGKILL)
	assert.NilError(t, err)

	killCtx := <-task.killContext
	assert.NilError(t, killCtx.Err())
	assert.Equal(t, killCtx.Value(killContextKey{}), contextValue)
}

package daemon

import (
	"context"
	"runtime"
	"syscall"
	"testing"
	"testing/synctest"
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

type delayedExitKillTask struct {
	libcontainerdtypes.Task
	kill func()
}

func (*delayedExitKillTask) Pid() uint32 {
	// No OS process is involved, including on the direct-kill fallback path.
	return 0
}

func (task *delayedExitKillTask) Kill(context.Context, syscall.Signal) error {
	task.kill()
	return nil
}

func TestKillWaitTimeout(t *testing.T) {
	defaultWait := 10 * time.Second
	if runtime.GOOS == "windows" {
		defaultWait = 75 * time.Second
	}
	longTimeout := int(defaultWait/time.Second) + 20
	shortTimeout, zeroTimeout, infiniteTimeout := 1, 0, -1
	for _, tc := range []struct {
		name           string
		stopTimeout    *int
		defaultTimeout int
		exitAfter      time.Duration
		wantDuration   time.Duration
		wantError      bool
	}{
		{name: "prompt exit", exitAfter: time.Second, wantDuration: time.Second},
		{name: "delayed exit", stopTimeout: &longTimeout, exitAfter: defaultWait + 5*time.Second, wantDuration: defaultWait + 5*time.Second},
		{name: "daemon default", defaultTimeout: longTimeout, exitAfter: defaultWait + 5*time.Second, wantDuration: defaultWait + 5*time.Second},
		{name: "bounded wait", stopTimeout: &longTimeout, wantDuration: time.Duration(longTimeout)*time.Second + 2*time.Second, wantError: true},
		{name: "default unchanged", wantDuration: defaultWait + 2*time.Second, wantError: true},
		{name: "short grace period", stopTimeout: &shortTimeout, wantDuration: defaultWait + 2*time.Second, wantError: true},
		{name: "zero grace period", stopTimeout: &zeroTimeout, wantDuration: defaultWait + 2*time.Second, wantError: true},
		{name: "infinite grace period", stopTimeout: &infiniteTimeout, wantDuration: defaultWait + 2*time.Second, wantError: true},
		{name: "container overrides daemon", stopTimeout: &shortTimeout, defaultTimeout: longTimeout, wantDuration: defaultWait + 2*time.Second, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctr := container.NewBaseContainer(t.Name(), t.TempDir())
				ctr.Config = &containertypes.Config{StopTimeout: tc.stopTimeout}
				ctr.HostConfig = &containertypes.HostConfig{}
				var exitTimer *time.Timer
				defer func() {
					if exitTimer != nil {
						exitTimer.Stop()
					}
				}()
				task := &delayedExitKillTask{kill: func() {
					if tc.exitAfter > 0 {
						exitTimer = time.AfterFunc(tc.exitAfter, func() {
							ctr.Lock()
							ctr.State.SetStopped(&container.ExitStatus{ExitCode: 137})
							ctr.Unlock()
						})
					}
				}}
				ctr.State.SetRunning(nil, task, time.Now())
				daemon := &Daemon{EventsService: events.New(), shutdown: true}
				cfg := &configStore{}
				cfg.DefaultStopTimeout = tc.defaultTimeout
				daemon.configStore.Store(cfg)
				started := time.Now()
				err := daemon.kill(t.Context(), ctr)
				if tc.wantError {
					assert.Error(t, err, "tried to kill container, but did not receive an exit event")
				} else {
					assert.NilError(t, err)
				}
				assert.Equal(t, ctr.State.IsRunning(), tc.wantError)
				assert.Equal(t, time.Since(started), tc.wantDuration)
			})
		})
	}
}

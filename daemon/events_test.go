package daemon

import (
	"testing"
	"time"

	gogotypes "github.com/gogo/protobuf/types"
	containertypes "github.com/moby/moby/api/types/container"
	eventtypes "github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	filters "github.com/moby/moby/v2/daemon/internal/filters"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestLogContainerEventCopyLabels(t *testing.T) {
	e := events.New()
	_, l, cancel := e.Subscribe()
	defer cancel()

	ctr := &container.Container{
		ID:   "container_id",
		Name: "container_name",
		Config: &containertypes.Config{
			Image: "image_name",
			Labels: map[string]string{
				"node": "1",
				"os":   "alpine",
			},
		},
	}
	daemon := &Daemon{
		EventsService: e,
	}
	daemon.LogContainerEvent(ctr, eventtypes.ActionCreate)

	if _, mutated := ctr.Config.Labels["image"]; mutated {
		t.Fatalf("Expected to not mutate the container labels, got %q", ctr.Config.Labels)
	}

	validateTestAttributes(t, l, map[string]string{
		"node": "1",
		"os":   "alpine",
	})
}

func TestLogContainerEventWithAttributes(t *testing.T) {
	e := events.New()
	_, l, cancel := e.Subscribe()
	defer cancel()

	ctr := &container.Container{
		ID:   "container_id",
		Name: "container_name",
		Config: &containertypes.Config{
			Labels: map[string]string{
				"node": "1",
				"os":   "alpine",
			},
		},
	}
	daemon := &Daemon{
		EventsService: e,
	}
	daemon.LogContainerEventWithAttributes(ctr, eventtypes.ActionCreate, map[string]string{
		"node": "2",
		"foo":  "bar",
	})

	validateTestAttributes(t, l, map[string]string{
		"node": "1",
		"foo":  "bar",
	})
}

func validateTestAttributes(t *testing.T, l chan any, expectedAttributesToTest map[string]string) {
	select {
	case ev := <-l:
		event, ok := ev.(eventtypes.Message)
		if !ok {
			t.Fatalf("Unexpected event message: %q", ev)
		}
		for key, expected := range expectedAttributesToTest {
			actual, ok := event.Actor.Attributes[key]
			if !ok || actual != expected {
				t.Fatalf("Expected value for key %s to be %s, but was %s (event:%v)", key, expected, actual, event)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LogEvent test timed out")
	}
}

func TestEventTimestamp(t *testing.T) {
	now := time.Now()
	createdAt := &gogotypes.Timestamp{Seconds: now.Unix(), Nanos: int32(now.Nanosecond())}
	updatedAt := &gogotypes.Timestamp{Seconds: now.Add(time.Hour).Unix(), Nanos: int32(now.Add(time.Hour).Nanosecond())}

	tests := []struct {
		doc    string
		meta   swarmapi.Meta
		action swarmapi.WatchActionKind
		check  func(t *testing.T, result time.Time)
	}{
		{
			doc:    "Create action uses CreatedAt timestamp",
			meta:   swarmapi.Meta{CreatedAt: createdAt},
			action: swarmapi.WatchActionKindCreate,
			check: func(t *testing.T, result time.Time) {
				if result.Unix() != now.Unix() {
					t.Errorf("expected CreatedAt timestamp, got %v", result)
				}
			},
		},
		{
			doc:    "Update action uses UpdatedAt timestamp",
			meta:   swarmapi.Meta{UpdatedAt: updatedAt},
			action: swarmapi.WatchActionKindUpdate,
			check: func(t *testing.T, result time.Time) {
				if result.Unix() != now.Add(time.Hour).Unix() {
					t.Errorf("expected UpdatedAt timestamp, got %v", result)
				}
			},
		},
		{
			doc:    "Remove action uses current time",
			meta:   swarmapi.Meta{},
			action: swarmapi.WatchActionKindRemove,
			check: func(t *testing.T, result time.Time) {
				if result.IsZero() {
					t.Error("expected non-zero timestamp for Remove action")
				}
			},
		},
		{
			doc:    "Unknown action returns valid timestamp",
			meta:   swarmapi.Meta{},
			action: swarmapi.WatchActionKindUnknown,
			check: func(t *testing.T, result time.Time) {
				if result.IsZero() {
					t.Error("expected non-zero timestamp for Unknown action")
				}
			},
		},
		{
			doc:    "Invalid action falls back to current time",
			meta:   swarmapi.Meta{},
			action: swarmapi.WatchActionKind(123456789),
			check: func(t *testing.T, result time.Time) {
				if result.IsZero() {
					t.Error("expected non-zero timestamp for invalid action")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			result := eventTimestamp(tc.meta, tc.action)
			tc.check(t, result)
		})
	}
}

func TestExecEventsExecType(t *testing.T) {
	d := &Daemon{
		containers:    container.NewMemoryStore(),
		execCommands:  container.NewExecStore(),
		EventsService: events.New(),
	}

	_, l, cancel := d.EventsService.Subscribe()
	defer cancel()

	ctr := &container.Container{
		ID:           "test_container_id",
		Name:         "/test_container",
		Config:       &containertypes.Config{Image: "test_image"},
		ExecCommands: container.NewExecStore(),
		State:        &container.State{Running: true},
	}
	d.containers.Add(ctr.ID, ctr)

	// 1. Standard exec create event contains execType="exec"
	execID, err := d.ContainerExecCreate(ctr.ID, &containertypes.ExecCreateRequest{
		Cmd: []string{"echo", "hello"},
	})
	assert.NilError(t, err)

	select {
	case ev := <-l:
		msg, ok := ev.(eventtypes.Message)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(string(msg.Action), string(eventtypes.ActionExecCreate)+": echo hello"))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execType"], container.ExecTypeDefault))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execID"], execID))
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for exec_create event")
	}

	// 2. Standard exec exit (exec_die) event contains execType="exec"
	err = d.ProcessEvent(ctr.ID, libcontainerdtypes.EventExit, libcontainerdtypes.EventInfo{
		ProcessID: execID,
		ExitCode:  0,
	})
	assert.NilError(t, err)

	var stdDieEv eventtypes.Message
	select {
	case ev := <-l:
		msg, ok := ev.(eventtypes.Message)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(msg.Action, eventtypes.ActionExecDie))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execType"], container.ExecTypeDefault))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execID"], execID))
		assert.Check(t, is.Equal(msg.Actor.Attributes["exitCode"], "0"))
		stdDieEv = msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for standard exec_die event")
	}

	// 3. Healthcheck exec exit (exec_die) event contains execType="healthcheck"
	hcExec := container.NewExecConfig(ctr)
	hcExec.ExecType = container.ExecTypeHealthcheck
	hcExec.Entrypoint = "healthcheck.sh"
	d.registerExecCommand(ctr, hcExec)

	d.LogContainerEventWithAttributes(ctr, eventtypes.Action(string(eventtypes.ActionExecCreate)+": "+hcExec.Entrypoint), map[string]string{
		"execID":   hcExec.ID,
		"execType": hcExec.ExecType,
	})

	select {
	case ev := <-l:
		msg, ok := ev.(eventtypes.Message)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(msg.Actor.Attributes["execType"], container.ExecTypeHealthcheck))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execID"], hcExec.ID))
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for healthcheck exec_create event")
	}

	err = d.ProcessEvent(ctr.ID, libcontainerdtypes.EventExit, libcontainerdtypes.EventInfo{
		ProcessID: hcExec.ID,
		ExitCode:  1,
	})
	assert.NilError(t, err)

	var hcDieEv eventtypes.Message
	select {
	case ev := <-l:
		msg, ok := ev.(eventtypes.Message)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(msg.Action, eventtypes.ActionExecDie))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execType"], container.ExecTypeHealthcheck))
		assert.Check(t, is.Equal(msg.Actor.Attributes["execID"], hcExec.ID))
		assert.Check(t, is.Equal(msg.Actor.Attributes["exitCode"], "1"))
		hcDieEv = msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for healthcheck exec_die event")
	}

	// 4. Verify event filtering by label=execType
	filterHealthcheck := events.NewFilter(filters.NewArgs(filters.Arg("label", "execType=healthcheck")))
	assert.Check(t, filterHealthcheck.Include(hcDieEv))
	assert.Check(t, !filterHealthcheck.Include(stdDieEv))

	filterDefaultExec := events.NewFilter(filters.NewArgs(filters.Arg("label", "execType=exec")))
	assert.Check(t, filterDefaultExec.Include(stdDieEv))
	assert.Check(t, !filterDefaultExec.Include(hcDieEv))
}

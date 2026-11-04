package daemon

import (
	"testing"
	"time"

	gogotypes "github.com/gogo/protobuf/types"
	containertypes "github.com/moby/moby/api/types/container"
	eventtypes "github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	swarmapi "github.com/moby/swarmkit/v2/api"
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

func TestExecEventAttributes(t *testing.T) {
	execConfig := &container.ExecConfig{
		ID:       "exec_id",
		ExecType: "healthcheck",
	}

	attrs := execEventAttributes(execConfig)
	if got := attrs["execType"]; got != "healthcheck" {
		t.Fatalf("expected execType=healthcheck, got %q", got)
	}
	if got := attrs["execID"]; got != "exec_id" {
		t.Fatalf("expected execID=exec_id, got %q", got)
	}

	userExec := &container.ExecConfig{ID: "user_exec_id"}
	userAttrs := execEventAttributes(userExec)
	if got := userAttrs["execType"]; got != "exec" {
		t.Fatalf("expected default execType=exec, got %q", got)
	}
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

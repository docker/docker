package container

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/v2/daemon"
	"github.com/moby/swarmkit/v2/api"
)

// TestWaitNodeAttachment tests that the waitNodeAttachment method successfully
// blocks until the required node attachment becomes available.
func TestWaitNodeAttachment(t *testing.T) {
	emptyDaemon := &daemon.Daemon{}

	// the daemon creates an attachment store as an object, which means it's
	// initialized to an empty store by default. get that attachment store here
	// and add some attachments to it
	attachmentStore := emptyDaemon.GetAttachmentStore()

	// create a set of attachments to put into the attachment store
	attachments := map[string]string{
		"network1": "10.1.2.3/24",
	}

	// this shouldn't fail, but check it anyway just in case
	err := attachmentStore.ResetAttachments(attachments)
	if err != nil {
		t.Fatalf("error resetting attachments: %v", err)
	}

	// create a containerConfig to put in the adapter. we don't need the task,
	// actually; only the networkAttachments are needed.
	container := &containerConfig{
		task: nil,
		networks: map[string]*api.Network{
			// network1 is already present in the attachment store.
			"network1": {
				ID: "network1",
				DriverState: &api.Driver{
					Name: "overlay",
				},
			},
			// network2 is not yet present in the attachment store, and we
			// should block while waiting for it.
			"network2": {
				ID: "network2",
				DriverState: &api.Driver{
					Name: "overlay",
				},
			},
			// localnetwork is not and will never be in the attachment store,
			// but we should not block on it, because it is not an overlay
			// network
			"localnetwork": {
				ID: "localnetwork",
				DriverState: &api.Driver{
					Name: "bridge",
				},
			},
		},
	}

	// we don't create an adapter using the newContainerAdapter package,
	// because it does a bunch of checks and validations. instead, create one
	// "from scratch" so we only have the fields we need.
	adapter := &containerAdapter{
		backend:   emptyDaemon,
		container: container,
	}

	// create a context to do call the method with
	ctx := t.Context()

	// create a channel to allow the goroutine that we run the method call in
	// to signal that it's done.
	doneChan := make(chan struct{})

	// store the error return value of waitNodeAttachments in this variable
	var waitNodeAttachmentsErr error
	// NOTE(dperny): be careful running goroutines in test code. if a test
	// terminates with ie t.Fatalf or a failed requirement, runtime.Goexit gets
	// called, which does run defers but does not clean up child goroutines.
	// we defer canceling the context here, which should stop this goroutine
	// from leaking
	go func() {
		waitNodeAttachmentsErr = adapter.waitNodeAttachments(ctx)
		// signal that we've completed
		close(doneChan)
	}()

	// wait 200ms to allow the waitNodeAttachments call to spin for a bit
	time.Sleep(200 * time.Millisecond)
	select {
	case <-doneChan:
		if waitNodeAttachmentsErr == nil {
			t.Fatalf("waitNodeAttachments exited early with no error")
		} else {
			t.Fatalf(
				"waitNodeAttachments exited early with an error: %v",
				waitNodeAttachmentsErr,
			)
		}
	default:
		// allow falling through; this is the desired case
	}

	// now update the node attachments to include another network attachment
	attachments["network2"] = "10.3.4.5/24"
	err = attachmentStore.ResetAttachments(attachments)
	if err != nil {
		t.Fatalf("error resetting attachments: %v", err)
	}

	// now wait 200 ms for waitNodeAttachments to pick up the change
	time.Sleep(200 * time.Millisecond)

	// and check that waitNodeAttachments has exited with no error
	select {
	case <-doneChan:
		if waitNodeAttachmentsErr != nil {
			t.Fatalf(
				"waitNodeAttachments returned an error: %v",
				waitNodeAttachmentsErr,
			)
		}
	default:
		t.Fatalf("waitNodeAttachments did not exit yet, but should have")
	}
}

func TestCheckBindMountCreation(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing source", true: "create source"}[create], func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "missing")
			task := &api.Task{Spec: api.TaskSpec{Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{Mounts: []api.Mount{{
					Type: api.MountTypeBind, Source: source, Target: "/target",
					BindOptions: &api.Mount_BindOptions{CreateMountpoint: create},
				}}},
			}}}
			adapter := &containerAdapter{container: &containerConfig{task: task}}
			err := adapter.checkMounts()
			if (err == nil) != create {
				t.Fatalf("CreateMountpoint=%v: checkMounts() = %v", create, err)
			}
		})
	}
}

package volumeenforcer

import (
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state/store"
)

// VolumeEnforcer is a component, styled off of the ConstraintEnforcer, that
// watches for updates to Volumes, and shuts down tasks if those Volumes are
// being drained.
type VolumeEnforcer struct {
	store    *store.MemoryStore
	stopChan chan struct{}
	doneChan chan struct{}
}

func New(s *store.MemoryStore) *VolumeEnforcer {
	return &VolumeEnforcer{
		store:    s,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

func (ve *VolumeEnforcer) Run() {
	defer close(ve.doneChan)

	var volumes []*api.Volume
	watcher, cancelWatch, _ := store.ViewAndWatch(ve.store, func(tx store.ReadTx) error {
		var err error
		volumes, err = store.FindVolumes(tx, store.All)
		return err
	}, api.EventUpdateVolume{})
	defer cancelWatch()

	for _, volume := range volumes {
		ve.rejectNoncompliantTasks(volume)
	}

	for {
		select {
		case event := <-watcher:
			v := event.(api.EventUpdateVolume).Volume
			ve.rejectNoncompliantTasks(v)
		case <-ve.stopChan:
			return
		}
	}

}

func (ve *VolumeEnforcer) Stop() {
	close(ve.stopChan)
	<-ve.doneChan
}

func (ve *VolumeEnforcer) rejectNoncompliantTasks(v *api.Volume) {
	if v.Spec.Availability != api.VolumeAvailabilityDrain {
		return
	}

	var volumeTasks []*api.Task

	ve.store.View(func(tx store.ReadTx) {
		// ignore the error, it only happens if you pass an invalid find by
		volumeTasks, _ = store.FindTasks(tx, store.ByVolumeAttachment(v.ID))
	})
	if len(volumeTasks) != 0 {
		err := ve.store.Batch(func(batch *store.Batch) error {
			for _, t := range volumeTasks {
				// skip any tasks we know are already shut down or shutting
				// down. Do this before we open the transaction. This saves us
				// copying volumeTasks while still avoiding unnecessary
				// transactions. we will still need to check again once we
				// start the transaction against the latest version of the
				// task.
				if t.DesiredState > api.TaskStateCompleted || t.Status.State >= api.TaskStateCompleted {
					continue
				}

				err := batch.Update(func(tx store.Tx) error {
					t = store.GetTask(tx, t.ID)
					// another check for task liveness.
					if t == nil || t.DesiredState > api.TaskStateCompleted || t.Status.State >= api.TaskStateCompleted {
						return nil
					}

					// as documented in the ConstraintEnforcer:
					//
					// We set the observed state to
					// REJECTED, rather than the desired
					// state. Desired state is owned by the
					// orchestrator, and setting it directly
					// will bypass actions such as
					// restarting the task on another node
					// (if applicable).
					t.Status.State = api.TaskStateRejected
					t.Status.Message = "task rejected by volume enforcer"
					t.Status.Err = "attached to volume which is being drained"
					return store.UpdateTask(tx, t)
				})
				if err != nil {
					log.L.WithField("module", "volumeenforcer").WithError(err).Errorf("failed to shut down task %s", t.ID)
				}
			}
			return nil
		})

		if err != nil {
			log.L.WithField("module", "volumeenforcer").WithError(err).Errorf("failed to shut down tasks for volume %s", v.ID)
		}
	}
}

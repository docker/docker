package constraintenforcer

import (
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/genericresource"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/constraint"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/protobuf/ptypes"
)

// ConstraintEnforcer watches for updates to nodes and shuts down tasks that no
// longer satisfy scheduling constraints or resource limits.
type ConstraintEnforcer struct {
	store    *store.MemoryStore
	stopChan chan struct{}
	doneChan chan struct{}
}

// New creates a new ConstraintEnforcer.
func New(store *store.MemoryStore) *ConstraintEnforcer {
	return &ConstraintEnforcer{
		store:    store,
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

// Run is the ConstraintEnforcer's main loop.
func (ce *ConstraintEnforcer) Run() {
	defer close(ce.doneChan)

	watcher, cancelWatch := state.Watch(ce.store.WatchQueue(), api.EventUpdateNode{})
	defer cancelWatch()

	var (
		nodes []*api.Node
		err   error
	)
	ce.store.View(func(readTx store.ReadTx) {
		nodes, err = store.FindNodes(readTx, store.All)
	})
	if err != nil {
		log.L.WithError(err).Error("failed to check nodes for noncompliant tasks")
	} else {
		for _, node := range nodes {
			ce.rejectNoncompliantTasks(node)
		}
	}

	for {
		select {
		case event := <-watcher:
			node := event.(api.EventUpdateNode).Node
			ce.rejectNoncompliantTasks(node)
		case <-ce.stopChan:
			return
		}
	}
}

func (ce *ConstraintEnforcer) rejectNoncompliantTasks(node *api.Node) {
	// If the availability is "drain", the orchestrator will
	// shut down all tasks.
	// If the availability is "pause", we shouldn't touch
	// the tasks on this node.
	if node.Spec.Availability != api.NodeAvailabilityActive {
		return
	}

	var (
		tasks []*api.Task
		err   error
	)

	services := map[string]*api.Service{}
	ce.store.View(func(tx store.ReadTx) {
		tasks, err = store.FindTasks(tx, store.ByNodeID(node.ID))
		if err != nil {
			return
		}

		// Deduplicate service IDs using the services map. It's okay for the
		// values to be nil for now, we will look them up from the store next.
		for _, task := range tasks {
			services[task.ServiceID] = nil
		}

		for serviceID := range services {
			services[serviceID] = store.GetService(tx, serviceID)
		}
	})

	if err != nil {
		log.L.WithError(err).Errorf("failed to list tasks for node ID %s", node.ID)
	}

	available := &api.Resources{}
	var fakeStore []*api.GenericResource

	if node.Description != nil && node.Description.Resources != nil {
		available = node.Description.Resources.Copy()
	}

	removeTasks := make(map[string]*api.Task)

	// TODO(aaronl): The set of tasks removed will be
	// nondeterministic because it depends on the order of
	// the slice returned from FindTasks. We could do
	// a separate pass over the tasks for each type of
	// resource, and sort by the size of the reservation
	// to remove the most resource-intensive tasks.
loop:
	for _, t := range tasks {
		if t.DesiredState < api.TaskStateAssigned || t.DesiredState > api.TaskStateRunning {
			continue
		}

		// Ensure that the node still satisfies placement constraints.
		// NOTE: If the task is associacted with a service then we must use the
		// constraints from the current service spec rather than the
		// constraints from the task spec because they may be outdated. This
		// will happen if the service was previously updated in a way which
		// only changes the placement constraints and the node matched the
		// placement constraints both before and after that update. In the case
		// of such updates, the tasks are not considered "dirty" and are not
		// restarted but it will mean that the task spec's placement
		// constraints are outdated. Consider this example:
		// - A service is created with no constraints and a task is scheduled
		//   to a node.
		// - The node is updated to add a label, this doesn't affect the task
		//   on that node because it has no constraints.
		// - The service is updated to add a node label constraint which
		//   matches the label which was just added to the node. The updater
		//   does not shut down the task because the only the constraints have
		//   changed and the node still matches the updated constraints.
		// - The node is updated to remove the node label. The node no longer
		//   satisfies the placement constraints of the service, so the task
		//   should be shutdown. However, the task's spec still has the
		//   original and outdated constraints (that are still satisfied by
		//   the node). If we used those original constraints then the task
		//   would incorrectly not be removed. This is why the constraints
		//   from the service spec should be used instead.
		var placement *api.Placement
		if service := services[t.ServiceID]; service != nil {
			// This task is associated with a service, so we use the service's
			// current placement constraints.
			placement = service.Spec.Task.Placement
		} else {
			// This task is not associated with a service (or the service no
			// longer exists), so we use the placement constraints from the
			// original task spec.
			placement = t.Spec.Placement
		}
		if placement != nil && len(placement.Constraints) > 0 {
			constraints, _ := constraint.Parse(placement.Constraints)
			if !constraint.NodeMatches(constraints, node) {
				removeTasks[t.ID] = t
				continue
			}
		}

		// Ensure that the task assigned to the node
		// still satisfies the resource limits.
		if t.Spec.Resources != nil && t.Spec.Resources.Reservations != nil {
			if t.Spec.Resources.Reservations.MemoryBytes > available.MemoryBytes {
				removeTasks[t.ID] = t
				continue
			}
			if t.Spec.Resources.Reservations.NanoCPUs > available.NanoCPUs {
				removeTasks[t.ID] = t
				continue
			}
			for _, ta := range t.AssignedGenericResources {
				// Type change or no longer available
				if genericresource.HasResource(ta, available.Generic) {
					removeTasks[t.ID] = t
					break loop
				}
			}

			available.MemoryBytes -= t.Spec.Resources.Reservations.MemoryBytes
			available.NanoCPUs -= t.Spec.Resources.Reservations.NanoCPUs

			genericresource.ClaimResources(&available.Generic,
				&fakeStore, t.AssignedGenericResources)
		}
	}

	if len(removeTasks) != 0 {
		err := ce.store.Batch(func(batch *store.Batch) error {
			for _, t := range removeTasks {
				err := batch.Update(func(tx store.Tx) error {
					t = store.GetTask(tx, t.ID)
					if t == nil || t.DesiredState > api.TaskStateRunning {
						return nil
					}

					// We set the observed state to
					// REJECTED, rather than the desired
					// state. Desired state is owned by the
					// orchestrator, and setting it directly
					// will bypass actions such as
					// restarting the task on another node
					// (if applicable).
					t.Status.State = api.TaskStateRejected
					t.Status.Message = "task rejected by constraint enforcer"
					t.Status.Err = "assigned node no longer meets constraints"
					t.Status.Timestamp = ptypes.MustTimestampProto(time.Now())
					return store.UpdateTask(tx, t)
				})
				if err != nil {
					log.L.WithError(err).Errorf("failed to shut down task %s", t.ID)
				}
			}
			return nil
		})

		if err != nil {
			log.L.WithError(err).Errorf("failed to shut down tasks")
		}
	}
}

// Stop stops the ConstraintEnforcer and waits for the main loop to exit.
func (ce *ConstraintEnforcer) Stop() {
	close(ce.stopChan)
	<-ce.doneChan
}

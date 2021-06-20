package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/genericresource"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/protobuf/ptypes"
)

const (
	// monitorFailures is the lookback period for counting failures of
	// a task to determine if a node is faulty for a particular service.
	monitorFailures = 5 * time.Minute

	// maxFailures is the number of failures within monitorFailures that
	// triggers downweighting of a node in the sorting function.
	maxFailures = 5
)

type schedulingDecision struct {
	old *api.Task
	new *api.Task
}

// Scheduler assigns tasks to nodes.
type Scheduler struct {
	store           *store.MemoryStore
	unassignedTasks map[string]*api.Task
	// pendingPreassignedTasks already have NodeID, need resource validation
	pendingPreassignedTasks map[string]*api.Task
	// preassignedTasks tracks tasks that were preassigned, including those
	// past the pending state.
	preassignedTasks map[string]struct{}
	nodeSet          nodeSet
	allTasks         map[string]*api.Task
	pipeline         *Pipeline
	volumes          *volumeSet

	// stopOnce is a sync.Once used to ensure that Stop is idempotent
	stopOnce sync.Once
	// stopChan signals to the state machine to stop running
	stopChan chan struct{}
	// doneChan is closed when the state machine terminates
	doneChan chan struct{}
}

// New creates a new scheduler.
func New(store *store.MemoryStore) *Scheduler {
	return &Scheduler{
		store:                   store,
		unassignedTasks:         make(map[string]*api.Task),
		pendingPreassignedTasks: make(map[string]*api.Task),
		preassignedTasks:        make(map[string]struct{}),
		allTasks:                make(map[string]*api.Task),
		stopChan:                make(chan struct{}),
		doneChan:                make(chan struct{}),
		pipeline:                NewPipeline(),
		volumes:                 newVolumeSet(),
	}
}

func (s *Scheduler) setupTasksList(tx store.ReadTx) error {
	// add all volumes that are ready to the volumeSet
	volumes, err := store.FindVolumes(tx, store.All)
	if err != nil {
		return err
	}

	for _, volume := range volumes {
		// only add volumes that have been created, meaning they have a
		// VolumeID.
		if volume.VolumeInfo != nil && volume.VolumeInfo.VolumeID != "" {
			s.volumes.addOrUpdateVolume(volume)
		}
	}

	tasks, err := store.FindTasks(tx, store.All)
	if err != nil {
		return err
	}

	tasksByNode := make(map[string]map[string]*api.Task)
	for _, t := range tasks {
		// Ignore all tasks that have not reached PENDING
		// state and tasks that no longer consume resources.
		if t.Status.State < api.TaskStatePending || t.Status.State > api.TaskStateRunning {
			continue
		}

		// Also ignore tasks that have not yet been assigned but desired state
		// is beyond TaskStateCompleted. This can happen if you update, delete
		// or scale down a service before its tasks were assigned.
		if t.Status.State == api.TaskStatePending && t.DesiredState > api.TaskStateCompleted {
			continue
		}

		s.allTasks[t.ID] = t
		if t.NodeID == "" {
			s.enqueue(t)
			continue
		}
		// preassigned tasks need to validate resource requirement on corresponding node
		if t.Status.State == api.TaskStatePending {
			s.preassignedTasks[t.ID] = struct{}{}
			s.pendingPreassignedTasks[t.ID] = t
			continue
		}

		// track the volumes in use by the task
		s.volumes.reserveTaskVolumes(t)

		if tasksByNode[t.NodeID] == nil {
			tasksByNode[t.NodeID] = make(map[string]*api.Task)
		}
		tasksByNode[t.NodeID][t.ID] = t
	}

	return s.buildNodeSet(tx, tasksByNode)
}

// Run is the scheduler event loop.
func (s *Scheduler) Run(pctx context.Context) error {
	ctx := log.WithModule(pctx, "scheduler")
	defer close(s.doneChan)

	s.pipeline.AddFilter(&VolumesFilter{vs: s.volumes})

	updates, cancel, err := store.ViewAndWatch(s.store, s.setupTasksList)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("snapshot store update failed")
		return err
	}
	defer cancel()

	// Validate resource for tasks from preassigned tasks
	// do this before other tasks because preassigned tasks like
	// global service should start before other tasks
	s.processPreassignedTasks(ctx)

	// Queue all unassigned tasks before processing changes.
	s.tick(ctx)

	const (
		// commitDebounceGap is the amount of time to wait between
		// commit events to debounce them.
		commitDebounceGap = 50 * time.Millisecond
		// maxLatency is a time limit on the debouncing.
		maxLatency = time.Second
	)
	var (
		debouncingStarted     time.Time
		commitDebounceTimer   *time.Timer
		commitDebounceTimeout <-chan time.Time
	)

	tickRequired := false

	schedule := func() {
		if len(s.pendingPreassignedTasks) > 0 {
			s.processPreassignedTasks(ctx)
		}
		if tickRequired {
			s.tick(ctx)
			tickRequired = false
		}
	}

	// Watch for changes.
	for {
		select {
		case event := <-updates:
			switch v := event.(type) {
			case api.EventCreateTask:
				if s.createTask(ctx, v.Task) {
					tickRequired = true
				}
			case api.EventUpdateTask:
				if s.updateTask(ctx, v.Task) {
					tickRequired = true
				}
			case api.EventDeleteTask:
				if s.deleteTask(v.Task) {
					// deleting tasks may free up node resource, pending tasks should be re-evaluated.
					tickRequired = true
				}
			case api.EventCreateNode:
				s.createOrUpdateNode(v.Node)
				tickRequired = true
			case api.EventUpdateNode:
				s.createOrUpdateNode(v.Node)
				tickRequired = true
			case api.EventDeleteNode:
				s.nodeSet.remove(v.Node.ID)
			case api.EventUpdateVolume:
				// there is no need for a EventCreateVolume case, because
				// volumes are not ready to use until they've passed through
				// the volume manager and been created with the plugin
				//
				// as such, only addOrUpdateVolume if the VolumeInfo exists and
				// has a nonempty VolumeID
				if v.Volume.VolumeInfo != nil && v.Volume.VolumeInfo.VolumeID != "" {
					// TODO(dperny): verify that updating volumes doesn't break
					// scheduling
					log.G(ctx).WithField("volume.id", v.Volume.ID).Debug("updated volume")
					s.volumes.addOrUpdateVolume(v.Volume)
					tickRequired = true
				}
			case state.EventCommit:
				if commitDebounceTimer != nil {
					if time.Since(debouncingStarted) > maxLatency {
						commitDebounceTimer.Stop()
						commitDebounceTimer = nil
						commitDebounceTimeout = nil
						schedule()
					} else {
						commitDebounceTimer.Reset(commitDebounceGap)
					}
				} else {
					commitDebounceTimer = time.NewTimer(commitDebounceGap)
					commitDebounceTimeout = commitDebounceTimer.C
					debouncingStarted = time.Now()
				}
			}
		case <-commitDebounceTimeout:
			schedule()
			commitDebounceTimer = nil
			commitDebounceTimeout = nil
		case <-s.stopChan:
			return nil
		}
	}
}

// Stop causes the scheduler event loop to stop running.
func (s *Scheduler) Stop() {
	// ensure stop is called only once. this helps in some test cases.
	s.stopOnce.Do(func() {
		close(s.stopChan)
	})
	<-s.doneChan
}

// enqueue queues a task for scheduling.
func (s *Scheduler) enqueue(t *api.Task) {
	s.unassignedTasks[t.ID] = t
}

func (s *Scheduler) createTask(ctx context.Context, t *api.Task) bool {
	// Ignore all tasks that have not reached PENDING
	// state, and tasks that no longer consume resources.
	if t.Status.State < api.TaskStatePending || t.Status.State > api.TaskStateRunning {
		return false
	}

	s.allTasks[t.ID] = t
	if t.NodeID == "" {
		// unassigned task
		s.enqueue(t)
		return true
	}

	if t.Status.State == api.TaskStatePending {
		s.preassignedTasks[t.ID] = struct{}{}
		s.pendingPreassignedTasks[t.ID] = t
		// preassigned tasks do not contribute to running tasks count
		return false
	}

	nodeInfo, err := s.nodeSet.nodeInfo(t.NodeID)
	if err == nil && nodeInfo.addTask(t) {
		s.nodeSet.updateNode(nodeInfo)
	}

	return false
}

func (s *Scheduler) updateTask(ctx context.Context, t *api.Task) bool {
	// Ignore all tasks that have not reached PENDING
	// state.
	if t.Status.State < api.TaskStatePending {
		return false
	}

	oldTask := s.allTasks[t.ID]

	// Ignore all tasks that have not reached Pending
	// state, and tasks that no longer consume resources.
	if t.Status.State > api.TaskStateRunning {
		if oldTask == nil {
			return false
		}

		if t.Status.State != oldTask.Status.State &&
			(t.Status.State == api.TaskStateFailed || t.Status.State == api.TaskStateRejected) {
			// Keep track of task failures, so other nodes can be preferred
			// for scheduling this service if it looks like the service is
			// failing in a loop on this node. However, skip this for
			// preassigned tasks, because the scheduler does not choose
			// which nodes those run on.
			if _, wasPreassigned := s.preassignedTasks[t.ID]; !wasPreassigned {
				nodeInfo, err := s.nodeSet.nodeInfo(t.NodeID)
				if err == nil {
					nodeInfo.taskFailed(ctx, t)
					s.nodeSet.updateNode(nodeInfo)
				}
			}
		}

		s.deleteTask(oldTask)

		return true
	}

	if t.NodeID == "" {
		// unassigned task
		if oldTask != nil {
			s.deleteTask(oldTask)
		}
		s.allTasks[t.ID] = t
		s.enqueue(t)
		return true
	}

	if t.Status.State == api.TaskStatePending {
		if oldTask != nil {
			s.deleteTask(oldTask)
		}
		s.preassignedTasks[t.ID] = struct{}{}
		s.allTasks[t.ID] = t
		s.pendingPreassignedTasks[t.ID] = t
		// preassigned tasks do not contribute to running tasks count
		return false
	}

	s.allTasks[t.ID] = t
	nodeInfo, err := s.nodeSet.nodeInfo(t.NodeID)
	if err == nil && nodeInfo.addTask(t) {
		s.nodeSet.updateNode(nodeInfo)
	}

	return false
}

func (s *Scheduler) deleteTask(t *api.Task) bool {
	delete(s.allTasks, t.ID)
	delete(s.preassignedTasks, t.ID)
	delete(s.pendingPreassignedTasks, t.ID)

	// remove the task volume reservations as well, if any
	for _, attachment := range t.Volumes {
		s.volumes.releaseVolume(attachment.ID, t.ID)
	}

	nodeInfo, err := s.nodeSet.nodeInfo(t.NodeID)
	if err == nil && nodeInfo.removeTask(t) {
		s.nodeSet.updateNode(nodeInfo)
		return true
	}
	return false
}

func (s *Scheduler) createOrUpdateNode(n *api.Node) {
	nodeInfo, nodeInfoErr := s.nodeSet.nodeInfo(n.ID)
	var resources *api.Resources
	if n.Description != nil && n.Description.Resources != nil {
		resources = n.Description.Resources.Copy()
		// reconcile resources by looping over all tasks in this node
		if nodeInfoErr == nil {
			for _, task := range nodeInfo.Tasks {
				reservations := taskReservations(task.Spec)

				resources.MemoryBytes -= reservations.MemoryBytes
				resources.NanoCPUs -= reservations.NanoCPUs

				genericresource.ConsumeNodeResources(&resources.Generic,
					task.AssignedGenericResources)
			}
		}
	} else {
		resources = &api.Resources{}
	}

	if nodeInfoErr != nil {
		nodeInfo = newNodeInfo(n, nil, *resources)
	} else {
		nodeInfo.Node = n
		nodeInfo.AvailableResources = resources
	}
	s.nodeSet.addOrUpdateNode(nodeInfo)
}

func (s *Scheduler) processPreassignedTasks(ctx context.Context) {
	schedulingDecisions := make(map[string]schedulingDecision, len(s.pendingPreassignedTasks))
	for _, t := range s.pendingPreassignedTasks {
		newT := s.taskFitNode(ctx, t, t.NodeID)
		if newT == nil {
			continue
		}
		schedulingDecisions[t.ID] = schedulingDecision{old: t, new: newT}
	}

	successful, failed := s.applySchedulingDecisions(ctx, schedulingDecisions)

	for _, decision := range successful {
		if decision.new.Status.State == api.TaskStateAssigned {
			delete(s.pendingPreassignedTasks, decision.old.ID)
		}
	}
	for _, decision := range failed {
		s.allTasks[decision.old.ID] = decision.old
		nodeInfo, err := s.nodeSet.nodeInfo(decision.new.NodeID)
		if err == nil && nodeInfo.removeTask(decision.new) {
			s.nodeSet.updateNode(nodeInfo)
		}

		for _, va := range decision.new.Volumes {
			s.volumes.releaseVolume(va.ID, decision.new.ID)
		}
	}
}

// tick attempts to schedule the queue.
func (s *Scheduler) tick(ctx context.Context) {
	type commonSpecKey struct {
		serviceID   string
		specVersion api.Version
	}
	tasksByCommonSpec := make(map[commonSpecKey]map[string]*api.Task)
	var oneOffTasks []*api.Task
	schedulingDecisions := make(map[string]schedulingDecision, len(s.unassignedTasks))

	for taskID, t := range s.unassignedTasks {
		if t == nil || t.NodeID != "" {
			// task deleted or already assigned
			delete(s.unassignedTasks, taskID)
			continue
		}

		// Group tasks with common specs
		if t.SpecVersion != nil {
			taskGroupKey := commonSpecKey{
				serviceID:   t.ServiceID,
				specVersion: *t.SpecVersion,
			}

			if tasksByCommonSpec[taskGroupKey] == nil {
				tasksByCommonSpec[taskGroupKey] = make(map[string]*api.Task)
			}
			tasksByCommonSpec[taskGroupKey][taskID] = t
		} else {
			// This task doesn't have a spec version. We have to
			// schedule it as a one-off.
			oneOffTasks = append(oneOffTasks, t)
		}
		delete(s.unassignedTasks, taskID)
	}

	for _, taskGroup := range tasksByCommonSpec {
		s.scheduleTaskGroup(ctx, taskGroup, schedulingDecisions)
	}
	for _, t := range oneOffTasks {
		s.scheduleTaskGroup(ctx, map[string]*api.Task{t.ID: t}, schedulingDecisions)
	}

	_, failed := s.applySchedulingDecisions(ctx, schedulingDecisions)
	for _, decision := range failed {
		s.allTasks[decision.old.ID] = decision.old

		nodeInfo, err := s.nodeSet.nodeInfo(decision.new.NodeID)
		if err == nil && nodeInfo.removeTask(decision.new) {
			s.nodeSet.updateNode(nodeInfo)
		}

		// release the volumes we tried to use
		for _, va := range decision.new.Volumes {
			s.volumes.releaseVolume(va.ID, decision.new.ID)
		}

		// enqueue task for next scheduling attempt
		s.enqueue(decision.old)
	}
}

func (s *Scheduler) applySchedulingDecisions(ctx context.Context, schedulingDecisions map[string]schedulingDecision) (successful, failed []schedulingDecision) {
	if len(schedulingDecisions) == 0 {
		return
	}

	successful = make([]schedulingDecision, 0, len(schedulingDecisions))

	// Apply changes to master store
	err := s.store.Batch(func(batch *store.Batch) error {
		for len(schedulingDecisions) > 0 {
			err := batch.Update(func(tx store.Tx) error {
				// Update exactly one task inside this Update
				// callback.
			taskLoop:
				for taskID, decision := range schedulingDecisions {
					delete(schedulingDecisions, taskID)

					t := store.GetTask(tx, taskID)
					if t == nil {
						// Task no longer exists
						s.deleteTask(decision.new)
						continue
					}

					if t.Status.State == decision.new.Status.State &&
						t.Status.Message == decision.new.Status.Message &&
						t.Status.Err == decision.new.Status.Err {
						// No changes, ignore
						continue
					}

					if t.Status.State >= api.TaskStateAssigned {
						nodeInfo, err := s.nodeSet.nodeInfo(decision.new.NodeID)
						if err != nil {
							failed = append(failed, decision)
							continue
						}
						node := store.GetNode(tx, decision.new.NodeID)
						if node == nil || node.Meta.Version != nodeInfo.Meta.Version {
							// node is out of date
							failed = append(failed, decision)
							continue
						}
					}

					volumes := []*api.Volume{}
					for _, va := range decision.new.Volumes {
						v := store.GetVolume(tx, va.ID)
						if v == nil {
							log.G(ctx).Debugf(
								"scheduler failed to update task %s because volume %s could not be found",
								taskID,
								va.ID,
							)
							failed = append(failed, decision)
							continue taskLoop
						}

						// it's ok if the copy of the Volume we scheduled off
						// of is out of date, because the Scheduler is the only
						// component which add new uses of a particular Volume,
						// which means that in most cases, no update to the
						// volume could conflict with the copy the Scheduler
						// used to make decisions.
						//
						// the exception is that the VolumeAvailability could
						// have been changed. both Pause and Drain
						// availabilities mean the Volume should not be
						// scheduled, and so we call off our attempt to commit
						// this scheduling decision. this is the only field we
						// must check for conflicts.
						//
						// this is, additionally, the reason that a Volume must
						// be set to Drain before it can be deleted. it stops
						// us from having to worry about any other field when
						// attempting to use the Volume.
						if v.Spec.Availability != api.VolumeAvailabilityActive {
							log.G(ctx).Debugf(
								"scheduler failed to update task %s because volume %s has availability %s",
								taskID, v.ID, v.Spec.Availability.String(),
							)
							failed = append(failed, decision)
							continue taskLoop
						}

						alreadyPublished := false
						for _, ps := range v.PublishStatus {
							if ps.NodeID == decision.new.NodeID {
								alreadyPublished = true
								break
							}
						}
						if !alreadyPublished {
							v.PublishStatus = append(
								v.PublishStatus,
								&api.VolumePublishStatus{
									NodeID: decision.new.NodeID,
									State:  api.VolumePublishStatus_PENDING_PUBLISH,
								},
							)
							volumes = append(volumes, v)
						}
					}

					if err := store.UpdateTask(tx, decision.new); err != nil {
						log.G(ctx).Debugf("scheduler failed to update task %s; will retry", taskID)
						failed = append(failed, decision)
						continue
					}
					for _, v := range volumes {
						if err := store.UpdateVolume(tx, v); err != nil {
							// TODO(dperny): handle the case of a partial
							// update?
							log.G(ctx).WithError(err).Debugf(
								"scheduler failed to update task %v; volume %v could not be updated",
								taskID, v.ID,
							)
							failed = append(failed, decision)
							continue taskLoop
						}
					}
					successful = append(successful, decision)
					return nil
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		// finally, every time we make new scheduling decisions, take the
		// opportunity to release volumes.
		return batch.Update(func(tx store.Tx) error {
			return s.volumes.freeVolumes(tx)
		})
	})

	if err != nil {
		log.G(ctx).WithError(err).Error("scheduler tick transaction failed")
		failed = append(failed, successful...)
		successful = nil
	}
	return
}

// taskFitNode checks if a node has enough resources to accommodate a task.
func (s *Scheduler) taskFitNode(ctx context.Context, t *api.Task, nodeID string) *api.Task {
	nodeInfo, err := s.nodeSet.nodeInfo(nodeID)
	if err != nil {
		// node does not exist in set (it may have been deleted)
		return nil
	}
	newT := *t
	s.pipeline.SetTask(t)
	if !s.pipeline.Process(&nodeInfo) {
		// this node cannot accommodate this task
		newT.Status.Timestamp = ptypes.MustTimestampProto(time.Now())
		newT.Status.Err = s.pipeline.Explain()
		s.allTasks[t.ID] = &newT

		return &newT
	}

	// before doing all of the updating logic, get the volume attachments
	// for the task on this node. this should always succeed, because we
	// should already have filtered nodes based on volume availability, but
	// just in case we missed something and it doesn't, we have an error
	// case.
	attachments, err := s.volumes.chooseTaskVolumes(t, &nodeInfo)
	if err != nil {
		newT.Status.Timestamp = ptypes.MustTimestampProto(time.Now())
		newT.Status.Err = err.Error()
		s.allTasks[t.ID] = &newT

		return &newT
	}

	newT.Volumes = attachments

	newT.Status = api.TaskStatus{
		State:     api.TaskStateAssigned,
		Timestamp: ptypes.MustTimestampProto(time.Now()),
		Message:   "scheduler confirmed task can run on preassigned node",
	}
	s.allTasks[t.ID] = &newT

	if nodeInfo.addTask(&newT) {
		s.nodeSet.updateNode(nodeInfo)
	}
	return &newT
}

// scheduleTaskGroup schedules a batch of tasks that are part of the same
// service and share the same version of the spec.
func (s *Scheduler) scheduleTaskGroup(ctx context.Context, taskGroup map[string]*api.Task, schedulingDecisions map[string]schedulingDecision) {
	// Pick at task at random from taskGroup to use for constraint
	// evaluation. It doesn't matter which one we pick because all the
	// tasks in the group are equal in terms of the fields the constraint
	// filters consider.
	var t *api.Task
	for _, t = range taskGroup {
		break
	}

	s.pipeline.SetTask(t)

	now := time.Now()

	nodeLess := func(a *NodeInfo, b *NodeInfo) bool {
		// If either node has at least maxFailures recent failures,
		// that's the deciding factor.
		recentFailuresA := a.countRecentFailures(now, t)
		recentFailuresB := b.countRecentFailures(now, t)

		if recentFailuresA >= maxFailures || recentFailuresB >= maxFailures {
			if recentFailuresA > recentFailuresB {
				return false
			}
			if recentFailuresB > recentFailuresA {
				return true
			}
		}

		tasksByServiceA := a.ActiveTasksCountByService[t.ServiceID]
		tasksByServiceB := b.ActiveTasksCountByService[t.ServiceID]

		if tasksByServiceA < tasksByServiceB {
			return true
		}
		if tasksByServiceA > tasksByServiceB {
			return false
		}

		// Total number of tasks breaks ties.
		return a.ActiveTasksCount < b.ActiveTasksCount
	}

	var prefs []*api.PlacementPreference
	if t.Spec.Placement != nil {
		prefs = t.Spec.Placement.Preferences
	}

	tree := s.nodeSet.tree(t.ServiceID, prefs, len(taskGroup), s.pipeline.Process, nodeLess)

	s.scheduleNTasksOnSubtree(ctx, len(taskGroup), taskGroup, &tree, schedulingDecisions, nodeLess)
	if len(taskGroup) != 0 {
		s.noSuitableNode(ctx, taskGroup, schedulingDecisions)
	}
}

// scheduleNTasksOnSubtree schedules a set of tasks with identical constraints
// onto a set of nodes, taking into account placement preferences.
//
// placement preferences are used to create a tree such that every branch
// represents one subset of nodes across which tasks should be spread.
//
// because of this tree structure, scheduleNTasksOnSubtree is a recursive
// function. If there are subtrees of the current tree, then we recurse. if we
// are at a leaf node, past which there are no subtrees, then we try to
// schedule a proportional number of tasks to the nodes of that branch.
//
// - n is the number of tasks being scheduled on this subtree
// - taskGroup is a set of tasks to schedule, taking the form of a map from the
//   task ID to the task object.
// - tree is the decision tree we're scheduling on. this is, effectively, the
//   set of nodes that meet scheduling constraints. these nodes are arranged
//   into a tree so that placement preferences can be taken into account when
//   spreading tasks across nodes.
// - schedulingDecisions is a set of the scheduling decisions already made for
//   this tree
// - nodeLess is a comparator that chooses which of the two nodes is preferable
//   to schedule on.
func (s *Scheduler) scheduleNTasksOnSubtree(ctx context.Context, n int, taskGroup map[string]*api.Task, tree *decisionTree, schedulingDecisions map[string]schedulingDecision, nodeLess func(a *NodeInfo, b *NodeInfo) bool) int {
	if tree.next == nil {
		nodes := tree.orderedNodes(s.pipeline.Process, nodeLess)
		if len(nodes) == 0 {
			return 0
		}

		return s.scheduleNTasksOnNodes(ctx, n, taskGroup, nodes, schedulingDecisions, nodeLess)
	}

	// Walk the tree and figure out how the tasks should be split at each
	// level.
	tasksScheduled := 0
	tasksInUsableBranches := tree.tasks
	var noRoom map[*decisionTree]struct{}

	// Try to make branches even until either all branches are
	// full, or all tasks have been scheduled.
	for tasksScheduled != n && len(noRoom) != len(tree.next) {
		desiredTasksPerBranch := (tasksInUsableBranches + n - tasksScheduled) / (len(tree.next) - len(noRoom))
		remainder := (tasksInUsableBranches + n - tasksScheduled) % (len(tree.next) - len(noRoom))

		for _, subtree := range tree.next {
			if noRoom != nil {
				if _, ok := noRoom[subtree]; ok {
					continue
				}
			}
			subtreeTasks := subtree.tasks
			if subtreeTasks < desiredTasksPerBranch || (subtreeTasks == desiredTasksPerBranch && remainder > 0) {
				tasksToAssign := desiredTasksPerBranch - subtreeTasks
				if remainder > 0 {
					tasksToAssign++
				}
				res := s.scheduleNTasksOnSubtree(ctx, tasksToAssign, taskGroup, subtree, schedulingDecisions, nodeLess)
				if res < tasksToAssign {
					if noRoom == nil {
						noRoom = make(map[*decisionTree]struct{})
					}
					noRoom[subtree] = struct{}{}
					tasksInUsableBranches -= subtreeTasks
				} else if remainder > 0 {
					remainder--
				}
				tasksScheduled += res
			}
		}
	}

	return tasksScheduled
}

// scheduleNTasksOnNodes schedules some number of tasks on the set of provided
// nodes. The number of tasks being scheduled may be less than the total number
// of tasks, as the Nodes may be one branch of a tree used to spread tasks.
//
// returns the number of tasks actually scheduled to these nodes. this may be
// fewer than the number of tasks desired to be scheduled, if there are
// insufficient nodes to meet resource constraints.
//
// - n is the number of tasks desired to be scheduled to this set of nodes
// - taskGroup is the tasks desired to be scheduled, in the form of a map from
//   task ID to task object. this argument is mutated; tasks which have been
//   scheduled are removed from the map.
// - nodes is the set of nodes to schedule to
// - schedulingDecisions is the set of scheduling decisions that have been made
//   thus far, in the form of a map from task ID to the decision made.
// - nodeLess is a simple comparator that chooses which of two nodes would be
//   preferable to schedule on.
func (s *Scheduler) scheduleNTasksOnNodes(ctx context.Context, n int, taskGroup map[string]*api.Task, nodes []NodeInfo, schedulingDecisions map[string]schedulingDecision, nodeLess func(a *NodeInfo, b *NodeInfo) bool) int {
	tasksScheduled := 0
	failedConstraints := make(map[int]bool) // key is index in nodes slice
	nodeIter := 0
	nodeCount := len(nodes)
	for taskID, t := range taskGroup {
		// Skip tasks which were already scheduled because they ended
		// up in two groups at once.
		if _, exists := schedulingDecisions[taskID]; exists {
			continue
		}

		node := &nodes[nodeIter%nodeCount]
		// before doing all of the updating logic, get the volume attachments
		// for the task on this node. this should always succeed, because we
		// should already have filtered nodes based on volume availability, but
		// just in case we missed something and it doesn't, we have an error
		// case.
		attachments, err := s.volumes.chooseTaskVolumes(t, node)
		if err != nil {
			// TODO(dperny) if there's an error, then what? i'm frankly not
			// sure.
			log.G(ctx).WithField("task.id", t.ID).WithError(err).Error("could not find task volumes")
		}

		log.G(ctx).WithField("task.id", t.ID).Debugf("assigning to node %s", node.ID)
		// she turned me into a newT!
		newT := *t
		newT.Volumes = attachments
		newT.NodeID = node.ID
		s.volumes.reserveTaskVolumes(&newT)
		newT.Status = api.TaskStatus{
			State:     api.TaskStateAssigned,
			Timestamp: ptypes.MustTimestampProto(time.Now()),
			Message:   "scheduler assigned task to node",
		}
		s.allTasks[t.ID] = &newT

		// in each iteration of this loop, the node we choose will always be
		// one which meets constraints. at the end of each iteration, we
		// re-process nodes, allowing us to remove nodes which no longer meet
		// resource constraints.
		nodeInfo, err := s.nodeSet.nodeInfo(node.ID)
		if err == nil && nodeInfo.addTask(&newT) {
			s.nodeSet.updateNode(nodeInfo)
			nodes[nodeIter%nodeCount] = nodeInfo
		}

		schedulingDecisions[taskID] = schedulingDecision{old: t, new: &newT}
		delete(taskGroup, taskID)
		tasksScheduled++
		if tasksScheduled == n {
			return tasksScheduled
		}

		if nodeIter+1 < nodeCount {
			// First pass fills the nodes until they have the same
			// number of tasks from this service.
			nextNode := nodes[(nodeIter+1)%nodeCount]
			if nodeLess(&nextNode, &nodeInfo) {
				nodeIter++
			}
		} else {
			// In later passes, we just assign one task at a time
			// to each node that still meets the constraints.
			nodeIter++
		}

		origNodeIter := nodeIter
		for failedConstraints[nodeIter%nodeCount] || !s.pipeline.Process(&nodes[nodeIter%nodeCount]) {
			failedConstraints[nodeIter%nodeCount] = true
			nodeIter++
			if nodeIter-origNodeIter == nodeCount {
				// None of the nodes meet the constraints anymore.
				return tasksScheduled
			}
		}
	}

	return tasksScheduled
}

// noSuitableNode checks unassigned tasks and make sure they have an existing service in the store before
// updating the task status and adding it back to: schedulingDecisions, unassignedTasks and allTasks
func (s *Scheduler) noSuitableNode(ctx context.Context, taskGroup map[string]*api.Task, schedulingDecisions map[string]schedulingDecision) {
	explanation := s.pipeline.Explain()
	for _, t := range taskGroup {
		var service *api.Service
		s.store.View(func(tx store.ReadTx) {
			service = store.GetService(tx, t.ServiceID)
		})
		if service == nil {
			log.G(ctx).WithField("task.id", t.ID).Debug("removing task from the scheduler")
			continue
		}

		log.G(ctx).WithField("task.id", t.ID).Debug("no suitable node available for task")

		newT := *t
		newT.Status.Timestamp = ptypes.MustTimestampProto(time.Now())
		sv := service.SpecVersion
		tv := newT.SpecVersion
		if sv != nil && tv != nil && sv.Index > tv.Index {
			log.G(ctx).WithField("task.id", t.ID).Debug(
				"task belongs to old revision of service",
			)
			if t.Status.State == api.TaskStatePending && t.DesiredState >= api.TaskStateShutdown {
				log.G(ctx).WithField("task.id", t.ID).Debug(
					"task is desired shutdown, scheduler will go ahead and do so",
				)
				newT.Status.State = api.TaskStateShutdown
				newT.Status.Err = ""
			}
		} else {
			if explanation != "" {
				newT.Status.Err = "no suitable node (" + explanation + ")"
			} else {
				newT.Status.Err = "no suitable node"
			}

			// re-enqueue a task that should still be attempted
			s.enqueue(&newT)
		}

		s.allTasks[t.ID] = &newT
		schedulingDecisions[t.ID] = schedulingDecision{old: t, new: &newT}
	}
}

func (s *Scheduler) buildNodeSet(tx store.ReadTx, tasksByNode map[string]map[string]*api.Task) error {
	nodes, err := store.FindNodes(tx, store.All)
	if err != nil {
		return err
	}

	s.nodeSet.alloc(len(nodes))

	for _, n := range nodes {
		var resources api.Resources
		if n.Description != nil && n.Description.Resources != nil {
			resources = *n.Description.Resources
		}
		s.nodeSet.addOrUpdateNode(newNodeInfo(n, tasksByNode[n.ID], resources))
	}

	return nil
}

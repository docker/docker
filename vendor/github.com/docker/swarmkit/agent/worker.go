package agent

import (
	"context"
	"sync"

	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/watch"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

// Worker implements the core task management logic and persistence. It
// coordinates the set of assignments with the executor.
type Worker interface {
	// Init prepares the worker for task assignment.
	Init(ctx context.Context) error

	// Close performs worker cleanup when no longer needed.
	//
	// It is not safe to call any worker function after that.
	Close()

	// Assign assigns a complete set of tasks and configs/secrets/volumes to a
	// worker. Any items not included in this set will be removed.
	Assign(ctx context.Context, assignments []*api.AssignmentChange) error

	// Updates updates an incremental set of tasks or configs/secrets/volumes of
	// the worker. Any items not included either in added or removed will
	// remain untouched.
	Update(ctx context.Context, assignments []*api.AssignmentChange) error

	// Listen to updates about tasks controlled by the worker. When first
	// called, the reporter will receive all updates for all tasks controlled
	// by the worker.
	//
	// The listener will be removed if the context is cancelled.
	Listen(ctx context.Context, reporter Reporter)

	// Report resends the status of all tasks controlled by this worker.
	Report(ctx context.Context, reporter StatusReporter)

	// Subscribe to log messages matching the subscription.
	Subscribe(ctx context.Context, subscription *api.SubscriptionMessage) error

	// Wait blocks until all task managers have closed
	Wait(ctx context.Context) error
}

// statusReporterKey protects removal map from panic.
type statusReporterKey struct {
	Reporter
}

type worker struct {
	db                *bolt.DB
	executor          exec.Executor
	publisher         exec.LogPublisher
	listeners         map[*statusReporterKey]struct{}
	taskevents        *watch.Queue
	publisherProvider exec.LogPublisherProvider

	taskManagers map[string]*taskManager
	mu           sync.RWMutex

	closed  bool
	closers sync.WaitGroup // keeps track of active closers
}

func newWorker(db *bolt.DB, executor exec.Executor, publisherProvider exec.LogPublisherProvider) *worker {
	return &worker{
		db:                db,
		executor:          executor,
		publisherProvider: publisherProvider,
		taskevents:        watch.NewQueue(),
		listeners:         make(map[*statusReporterKey]struct{}),
		taskManagers:      make(map[string]*taskManager),
	}
}

// Init prepares the worker for assignments.
func (w *worker) Init(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	ctx = log.WithModule(ctx, "worker")

	// TODO(stevvooe): Start task cleanup process.

	// read the tasks from the database and start any task managers that may be needed.
	return w.db.Update(func(tx *bolt.Tx) error {
		return WalkTasks(tx, func(task *api.Task) error {
			if !TaskAssigned(tx, task.ID) {
				// NOTE(stevvooe): If tasks can survive worker restart, we need
				// to startup the controller and ensure they are removed. For
				// now, we can simply remove them from the database.
				if err := DeleteTask(tx, task.ID); err != nil {
					log.G(ctx).WithError(err).Errorf("error removing task %v", task.ID)
				}
				return nil
			}

			status, err := GetTaskStatus(tx, task.ID)
			if err != nil {
				log.G(ctx).WithError(err).Error("unable to read tasks status")
				return nil
			}

			task.Status = *status // merges the status into the task, ensuring we start at the right point.
			return w.startTask(ctx, tx, task)
		})
	})
}

// Close performs worker cleanup when no longer needed.
func (w *worker) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()

	w.taskevents.Close()
}

// Assign assigns a full set of tasks, configs, and secrets to the worker.
// Any tasks not previously known will be started. Any tasks that are in the task set
// and already running will be updated, if possible. Any tasks currently running on
// the worker outside the task set will be terminated.
// Anything not in the set of assignments will be removed.
func (w *worker) Assign(ctx context.Context, assignments []*api.AssignmentChange) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrClosed
	}

	log.G(ctx).WithFields(logrus.Fields{
		"len(assignments)": len(assignments),
	}).Debug("(*worker).Assign")

	// Need to update dependencies before tasks

	err := reconcileSecrets(ctx, w, assignments, true)
	if err != nil {
		return err
	}

	err = reconcileConfigs(ctx, w, assignments, true)
	if err != nil {
		return err
	}

	err = reconcileTaskState(ctx, w, assignments, true)
	if err != nil {
		return err
	}

	return reconcileVolumes(ctx, w, assignments)
}

// Update updates the set of tasks, configs, and secrets for the worker.
// Tasks in the added set will be added to the worker, and tasks in the removed set
// will be removed from the worker
// Secrets in the added set will be added to the worker, and secrets in the removed set
// will be removed from the worker.
// Configs in the added set will be added to the worker, and configs in the removed set
// will be removed from the worker.
func (w *worker) Update(ctx context.Context, assignments []*api.AssignmentChange) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrClosed
	}

	log.G(ctx).WithFields(logrus.Fields{
		"len(assignments)": len(assignments),
	}).Debug("(*worker).Update")

	err := reconcileSecrets(ctx, w, assignments, false)
	if err != nil {
		return err
	}

	err = reconcileConfigs(ctx, w, assignments, false)
	if err != nil {
		return err
	}

	err = reconcileTaskState(ctx, w, assignments, false)
	if err != nil {
		return err
	}

	return reconcileVolumes(ctx, w, assignments)
}

func reconcileTaskState(ctx context.Context, w *worker, assignments []*api.AssignmentChange, fullSnapshot bool) error {
	var (
		updatedTasks []*api.Task
		removedTasks []*api.Task
	)
	for _, a := range assignments {
		if t := a.Assignment.GetTask(); t != nil {
			switch a.Action {
			case api.AssignmentChange_AssignmentActionUpdate:
				updatedTasks = append(updatedTasks, t)
			case api.AssignmentChange_AssignmentActionRemove:
				removedTasks = append(removedTasks, t)
			}
		}
	}

	log.G(ctx).WithFields(logrus.Fields{
		"len(updatedTasks)": len(updatedTasks),
		"len(removedTasks)": len(removedTasks),
	}).Debug("(*worker).reconcileTaskState")

	tx, err := w.db.Begin(true)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed starting transaction against task database")
		return err
	}
	defer tx.Rollback()

	assigned := map[string]struct{}{}

	for _, task := range updatedTasks {
		log.G(ctx).WithFields(
			logrus.Fields{
				"task.id":           task.ID,
				"task.desiredstate": task.DesiredState}).Debug("assigned")
		if err := PutTask(tx, task); err != nil {
			return err
		}

		if err := SetTaskAssignment(tx, task.ID, true); err != nil {
			return err
		}

		if mgr, ok := w.taskManagers[task.ID]; ok {
			if err := mgr.Update(ctx, task); err != nil && err != ErrClosed {
				log.G(ctx).WithError(err).Error("failed updating assigned task")
			}
		} else {
			// we may have still seen the task, let's grab the status from
			// storage and replace it with our status, if we have it.
			status, err := GetTaskStatus(tx, task.ID)
			if err != nil {
				if err != errTaskUnknown {
					return err
				}

				// never seen before, register the provided status
				if err := PutTaskStatus(tx, task.ID, &task.Status); err != nil {
					return err
				}
			} else {
				task.Status = *status
			}
			w.startTask(ctx, tx, task)
		}

		assigned[task.ID] = struct{}{}
	}

	closeManager := func(tm *taskManager) {
		go func(tm *taskManager) {
			defer w.closers.Done()
			// when a task is no longer assigned, we shutdown the task manager
			if err := tm.Close(); err != nil {
				log.G(ctx).WithError(err).Error("error closing task manager")
			}
		}(tm)

		// make an attempt at removing. this is best effort. any errors will be
		// retried by the reaper later.
		if err := tm.ctlr.Remove(ctx); err != nil {
			log.G(ctx).WithError(err).WithField("task.id", tm.task.ID).Error("remove task failed")
		}

		if err := tm.ctlr.Close(); err != nil {
			log.G(ctx).WithError(err).Error("error closing controller")
		}
	}

	removeTaskAssignment := func(taskID string) error {
		ctx := log.WithLogger(ctx, log.G(ctx).WithField("task.id", taskID))
		// if a task is no longer assigned, then we do not have to keep track
		// of it. a task will only be unassigned when it is deleted on the
		// manager. instead of SetTaskAssginment to true, we'll just remove the
		// task now.
		if err := DeleteTask(tx, taskID); err != nil {
			log.G(ctx).WithError(err).Error("error removing de-assigned task")
			return err
		}
		return nil
	}

	// If this was a complete set of assignments, we're going to remove all the remaining
	// tasks.
	if fullSnapshot {
		for id, tm := range w.taskManagers {
			if _, ok := assigned[id]; ok {
				continue
			}

			err := removeTaskAssignment(id)
			if err == nil {
				delete(w.taskManagers, id)
				go closeManager(tm)
			}
		}
	} else {
		// If this was an incremental set of assignments, we're going to remove only the tasks
		// in the removed set
		for _, task := range removedTasks {
			err := removeTaskAssignment(task.ID)
			if err != nil {
				continue
			}

			tm, ok := w.taskManagers[task.ID]
			if ok {
				delete(w.taskManagers, task.ID)
				go closeManager(tm)
			}
		}
	}

	return tx.Commit()
}

func reconcileSecrets(ctx context.Context, w *worker, assignments []*api.AssignmentChange, fullSnapshot bool) error {
	var (
		updatedSecrets []api.Secret
		removedSecrets []string
	)
	for _, a := range assignments {
		if s := a.Assignment.GetSecret(); s != nil {
			switch a.Action {
			case api.AssignmentChange_AssignmentActionUpdate:
				updatedSecrets = append(updatedSecrets, *s)
			case api.AssignmentChange_AssignmentActionRemove:
				removedSecrets = append(removedSecrets, s.ID)
			}

		}
	}

	secretsProvider, ok := w.executor.(exec.SecretsProvider)
	if !ok {
		if len(updatedSecrets) != 0 || len(removedSecrets) != 0 {
			log.G(ctx).Warn("secrets update ignored; executor does not support secrets")
		}
		return nil
	}

	secrets := secretsProvider.Secrets()

	log.G(ctx).WithFields(logrus.Fields{
		"len(updatedSecrets)": len(updatedSecrets),
		"len(removedSecrets)": len(removedSecrets),
	}).Debug("(*worker).reconcileSecrets")

	// If this was a complete set of secrets, we're going to clear the secrets map and add all of them
	if fullSnapshot {
		secrets.Reset()
	} else {
		secrets.Remove(removedSecrets)
	}
	secrets.Add(updatedSecrets...)

	return nil
}

func reconcileConfigs(ctx context.Context, w *worker, assignments []*api.AssignmentChange, fullSnapshot bool) error {
	var (
		updatedConfigs []api.Config
		removedConfigs []string
	)
	for _, a := range assignments {
		if r := a.Assignment.GetConfig(); r != nil {
			switch a.Action {
			case api.AssignmentChange_AssignmentActionUpdate:
				updatedConfigs = append(updatedConfigs, *r)
			case api.AssignmentChange_AssignmentActionRemove:
				removedConfigs = append(removedConfigs, r.ID)
			}

		}
	}

	configsProvider, ok := w.executor.(exec.ConfigsProvider)
	if !ok {
		if len(updatedConfigs) != 0 || len(removedConfigs) != 0 {
			log.G(ctx).Warn("configs update ignored; executor does not support configs")
		}
		return nil
	}

	configs := configsProvider.Configs()

	log.G(ctx).WithFields(logrus.Fields{
		"len(updatedConfigs)": len(updatedConfigs),
		"len(removedConfigs)": len(removedConfigs),
	}).Debug("(*worker).reconcileConfigs")

	// If this was a complete set of configs, we're going to clear the configs map and add all of them
	if fullSnapshot {
		configs.Reset()
	} else {
		configs.Remove(removedConfigs)
	}
	configs.Add(updatedConfigs...)

	return nil
}

// reconcileVolumes reconciles the CSI volumes on this node. It does not need
// fullSnapshot like other reconcile functions because volumes are non-trivial
// and are never reset.
func reconcileVolumes(ctx context.Context, w *worker, assignments []*api.AssignmentChange) error {
	var (
		updatedVolumes []api.VolumeAssignment
		removedVolumes []api.VolumeAssignment
	)
	for _, a := range assignments {
		if r := a.Assignment.GetVolume(); r != nil {
			switch a.Action {
			case api.AssignmentChange_AssignmentActionUpdate:
				updatedVolumes = append(updatedVolumes, *r)
			case api.AssignmentChange_AssignmentActionRemove:
				removedVolumes = append(removedVolumes, *r)
			}

		}
	}

	volumesProvider, ok := w.executor.(exec.VolumesProvider)
	if !ok {
		if len(updatedVolumes) != 0 || len(removedVolumes) != 0 {
			log.G(ctx).Warn("volumes update ignored; executor does not support volumes")
		}
		return nil
	}

	volumes := volumesProvider.Volumes()

	log.G(ctx).WithFields(logrus.Fields{
		"len(updatedVolumes)": len(updatedVolumes),
		"len(removedVolumes)": len(removedVolumes),
	}).Debug("(*worker).reconcileVolumes")

	volumes.Remove(removedVolumes, func(id string) {
		w.mu.RLock()
		defer w.mu.RUnlock()

		for key := range w.listeners {
			if err := key.Reporter.ReportVolumeUnpublished(ctx, id); err != nil {
				log.G(ctx).WithError(err).Errorf("failed reporting volume unpublished for reporter %v", key.Reporter)
			}
		}
	})
	volumes.Add(updatedVolumes...)

	return nil
}

func (w *worker) Listen(ctx context.Context, reporter Reporter) {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := &statusReporterKey{reporter}
	w.listeners[key] = struct{}{}

	go func() {
		<-ctx.Done()
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.listeners, key) // remove the listener if the context is closed.
	}()

	// report the current statuses to the new listener
	w.reportAllStatuses(ctx, reporter)
}

func (w *worker) Report(ctx context.Context, reporter StatusReporter) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.reportAllStatuses(ctx, reporter)
}

func (w *worker) reportAllStatuses(ctx context.Context, reporter StatusReporter) {
	if err := w.db.View(func(tx *bolt.Tx) error {
		return WalkTaskStatus(tx, func(id string, status *api.TaskStatus) error {
			return reporter.UpdateTaskStatus(ctx, id, status)
		})
	}); err != nil {
		log.G(ctx).WithError(err).Errorf("failed reporting initial statuses")
	}
}

func (w *worker) startTask(ctx context.Context, tx *bolt.Tx, task *api.Task) error {
	_, err := w.taskManager(ctx, tx, task) // side-effect taskManager creation.

	if err != nil {
		log.G(ctx).WithError(err).Error("failed to start taskManager")
		// we ignore this error: it gets reported in the taskStatus within
		// `newTaskManager`. We log it here and move on. If their is an
		// attempted restart, the lack of taskManager will have this retry
		// again.
		return nil
	}

	// only publish if controller resolution was successful.
	w.taskevents.Publish(task.Copy())
	return nil
}

func (w *worker) taskManager(ctx context.Context, tx *bolt.Tx, task *api.Task) (*taskManager, error) {
	if tm, ok := w.taskManagers[task.ID]; ok {
		return tm, nil
	}

	tm, err := w.newTaskManager(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	w.taskManagers[task.ID] = tm
	// keep track of active tasks
	w.closers.Add(1)
	return tm, nil
}

func (w *worker) newTaskManager(ctx context.Context, tx *bolt.Tx, task *api.Task) (*taskManager, error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(logrus.Fields{
		"task.id":    task.ID,
		"service.id": task.ServiceID,
	}))

	ctlr, status, err := exec.Resolve(ctx, task, w.executor)
	if err := w.updateTaskStatus(ctx, tx, task.ID, status); err != nil {
		log.G(ctx).WithError(err).Error("error updating task status after controller resolution")
	}

	if err != nil {
		log.G(ctx).WithError(err).Error("controller resolution failed")
		return nil, err
	}

	return newTaskManager(ctx, task, ctlr, statusReporterFunc(func(ctx context.Context, taskID string, status *api.TaskStatus) error {
		w.mu.RLock()
		defer w.mu.RUnlock()

		return w.db.Update(func(tx *bolt.Tx) error {
			return w.updateTaskStatus(ctx, tx, taskID, status)
		})
	})), nil
}

// updateTaskStatus reports statuses to listeners, read lock must be held.
func (w *worker) updateTaskStatus(ctx context.Context, tx *bolt.Tx, taskID string, status *api.TaskStatus) error {
	if err := PutTaskStatus(tx, taskID, status); err != nil {
		// we shouldn't fail to put a task status. however, there exists the
		// possibility of a race in which we try to put a task status after the
		// task has been deleted. because this whole contraption is a careful
		// dance of too-tightly-coupled concurrent parts, fixing tht race is
		// fraught with hazards. instead, we'll recognize that it can occur,
		// log the error, and then ignore it.
		if err == errTaskUnknown {
			// log at info level. debug logging in docker is already really
			// verbose, so many people disable it. the race that causes this
			// behavior should be very rare, but if it occurs, we should know
			// about it, because if there is some case where it is _not_ rare,
			// then knowing about it will go a long way toward debugging.
			log.G(ctx).Info("attempted to update status for a task that has been removed")
			return nil
		}
		log.G(ctx).WithError(err).Error("failed writing status to disk")
		return err
	}

	// broadcast the task status out.
	for key := range w.listeners {
		if err := key.Reporter.UpdateTaskStatus(ctx, taskID, status); err != nil {
			log.G(ctx).WithError(err).Errorf("failed updating status for reporter %v", key.Reporter)
		}
	}

	return nil
}

// Subscribe to log messages matching the subscription.
func (w *worker) Subscribe(ctx context.Context, subscription *api.SubscriptionMessage) error {
	log.G(ctx).Debugf("Received subscription %s (selector: %v)", subscription.ID, subscription.Selector)

	publisher, cancel, err := w.publisherProvider.Publisher(ctx, subscription.ID)
	if err != nil {
		return err
	}
	// Send a close once we're done
	defer cancel()

	match := func(t *api.Task) bool {
		// TODO(aluzzardi): Consider using maps to limit the iterations.
		for _, tid := range subscription.Selector.TaskIDs {
			if t.ID == tid {
				return true
			}
		}

		for _, sid := range subscription.Selector.ServiceIDs {
			if t.ServiceID == sid {
				return true
			}
		}

		for _, nid := range subscription.Selector.NodeIDs {
			if t.NodeID == nid {
				return true
			}
		}

		return false
	}

	wg := sync.WaitGroup{}
	w.mu.Lock()
	for _, tm := range w.taskManagers {
		if match(tm.task) {
			wg.Add(1)
			go func(tm *taskManager) {
				defer wg.Done()
				tm.Logs(ctx, *subscription.Options, publisher)
			}(tm)
		}
	}
	w.mu.Unlock()

	// If follow mode is disabled, wait for the current set of matched tasks
	// to finish publishing logs, then close the subscription by returning.
	if subscription.Options == nil || !subscription.Options.Follow {
		waitCh := make(chan struct{})
		go func() {
			defer close(waitCh)
			wg.Wait()
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCh:
			return nil
		}
	}

	// In follow mode, watch for new tasks. Don't close the subscription
	// until it's cancelled.
	ch, cancel := w.taskevents.Watch()
	defer cancel()
	for {
		select {
		case v := <-ch:
			task := v.(*api.Task)
			if match(task) {
				w.mu.RLock()
				tm, ok := w.taskManagers[task.ID]
				w.mu.RUnlock()
				if !ok {
					continue
				}

				go tm.Logs(ctx, *subscription.Options, publisher)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *worker) Wait(ctx context.Context) error {
	ch := make(chan struct{})
	go func() {
		w.closers.Wait()
		close(ch)
	}()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

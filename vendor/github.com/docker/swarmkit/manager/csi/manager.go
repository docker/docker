package csi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/docker/go-events"
	"github.com/sirupsen/logrus"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/volumequeue"
)

type Manager struct {
	store *store.MemoryStore
	// provider is the SecretProvider which allows retrieving secrets. Used
	// when creating new Plugin objects.
	provider SecretProvider

	// newPlugin is a function which returns an object implementing the Plugin
	// interface. It allows us to swap out the implementation of plugins while
	// unit-testing the Manager
	newPlugin func(config *api.CSIConfig_Plugin, provider SecretProvider) Plugin

	// synchronization for starting and stopping the Manager
	startOnce sync.Once

	stopChan chan struct{}
	stopOnce sync.Once
	doneChan chan struct{}

	cluster *api.Cluster
	plugins map[string]Plugin

	pendingVolumes *volumequeue.VolumeQueue
}

func NewManager(s *store.MemoryStore) *Manager {
	return &Manager{
		store:          s,
		stopChan:       make(chan struct{}),
		doneChan:       make(chan struct{}),
		newPlugin:      NewPlugin,
		plugins:        map[string]Plugin{},
		provider:       NewSecretProvider(s),
		pendingVolumes: volumequeue.NewVolumeQueue(),
	}
}

// Run runs the manager. The provided context is used as the parent for all RPC
// calls made to the CSI plugins. Canceling this context will cancel those RPC
// calls by the nature of contexts, but this is not the preferred way to stop
// the Manager. Instead, Stop should be called, which cause all RPC calls to be
// canceled anyway. The context is also used to get the logging context for the
// Manager.
func (vm *Manager) Run(ctx context.Context) {
	vm.startOnce.Do(func() {
		vm.run(ctx)
	})
}

// run performs the actual meat of the run operation.
//
// the argument is called pctx because it's the parent context, from which we
// immediately resolve a new child context.
func (vm *Manager) run(pctx context.Context) {
	defer close(vm.doneChan)
	ctx, ctxCancel := context.WithCancel(
		log.WithModule(pctx, "csi/manager"),
	)
	defer ctxCancel()

	watch, cancel, err := store.ViewAndWatch(vm.store, func(tx store.ReadTx) error {
		cluster, err := store.FindClusters(tx, store.ByName(store.DefaultClusterName))
		if err != nil {
			return err
		}
		vm.cluster = cluster[0]
		return nil
	})
	if err != nil {
		log.G(ctx).WithError(err).Error("error in store view and watch")
		return
	}
	defer cancel()

	vm.init(ctx)

	// run a goroutine which periodically processes incoming volumes. the
	// handle function will trigger processing every time new events come in
	// by writing to the channel

	doneProc := make(chan struct{})
	go func() {
		for {
			id, attempt := vm.pendingVolumes.Wait()
			// this case occurs when the stop method has been called on
			// pendingVolumes. stop is called on pendingVolumes when Stop is
			// called on the CSI manager.
			if id == "" && attempt == 0 {
				break
			}
			// TODO(dperny): we can launch some number of workers and process
			// more than one volume at a time, if desired.
			vm.processVolume(ctx, id, attempt)
		}

		// closing doneProc signals that this routine has exited, and allows
		// the main Run routine to exit.
		close(doneProc)
	}()

	// defer read from doneProc. doneProc is closed in the goroutine above,
	// and this defer will block until then. Because defers are executed as a
	// stack, this in turn blocks the final defer (closing doneChan) from
	// running. Ultimately, this prevents Stop from returning until the above
	// goroutine is closed.
	defer func() {
		<-doneProc
	}()

	for {
		select {
		case ev := <-watch:
			vm.handleEvent(ev)
		case <-vm.stopChan:
			vm.pendingVolumes.Stop()
			return
		}
	}
}

// processVolumes encapuslates the logic for processing pending Volumes.
func (vm *Manager) processVolume(ctx context.Context, id string, attempt uint) {
	// set up log fields for a derrived context to pass to handleVolume.
	dctx := log.WithFields(ctx, logrus.Fields{
		"volume.id": id,
		"attempt":   attempt,
	})

	err := vm.handleVolume(dctx, id)
	// TODO(dperny): differentiate between retryable and non-retryable
	// errors.
	if err != nil {
		log.G(dctx).WithError(err).Info("error handling volume")
		vm.pendingVolumes.Enqueue(id, attempt+1)
	}
}

// init does one-time setup work for the Manager, like creating all of
// the Plugins and initializing the local state of the component.
func (vm *Manager) init(ctx context.Context) {
	vm.updatePlugins()

	var (
		nodes   []*api.Node
		volumes []*api.Volume
	)
	vm.store.View(func(tx store.ReadTx) {
		var err error
		nodes, err = store.FindNodes(tx, store.All)
		if err != nil {
			// this should *never happen*. Find only returns errors if the find
			// by is invalid.
			log.G(ctx).WithError(err).Error("error finding nodes")
		}
		volumes, err = store.FindVolumes(tx, store.All)
		if err != nil {
			// likewise, should never happen.
			log.G(ctx).WithError(err).Error("error finding volumes")
		}
	})

	for _, node := range nodes {
		vm.handleNode(node)
	}

	// on initialization, we enqueue all of the Volumes. The easiest way to
	// know if a Volume needs some work performed is to just pass it through
	// the VolumeManager. If it doesn't need any work, then we will quickly
	// skip by it. Otherwise, the needed work will be performed.
	for _, volume := range volumes {
		vm.enqueueVolume(volume.ID)
	}
}

func (vm *Manager) updatePlugins() {
	// activePlugins is a set of plugin names that are currently in the cluster
	// spec. this lets remove from the vm.plugins map any plugins that are
	// no longer in use.
	activePlugins := map[string]struct{}{}

	if vm.cluster != nil {
		for _, plugin := range vm.cluster.Spec.CSIConfig.Plugins {
			// it's exceedingly unlikely that plugin could ever be nil but
			// better this than segfault
			if plugin != nil {
				if _, ok := vm.plugins[plugin.Name]; !ok {
					vm.plugins[plugin.Name] = vm.newPlugin(plugin, vm.provider)
				}
				activePlugins[plugin.Name] = struct{}{}
			}
		}
	}

	// remove any plugins that are no longer in use.
	for pluginName := range vm.plugins {
		if _, ok := activePlugins[pluginName]; !ok {
			delete(vm.plugins, pluginName)
		}
	}
}

func (vm *Manager) Stop() {
	vm.stopOnce.Do(func() {
		close(vm.stopChan)
	})

	<-vm.doneChan
}

func (vm *Manager) handleEvent(ev events.Event) {
	switch e := ev.(type) {
	case api.EventUpdateCluster:
		// TODO(dperny): verify that the Cluster in this event can never be nil
		if e.Cluster != nil {
			vm.cluster = e.Cluster
			vm.updatePlugins()
		}
	case api.EventCreateVolume:
		vm.enqueueVolume(e.Volume.ID)
	case api.EventUpdateVolume:
		vm.enqueueVolume(e.Volume.ID)
	case api.EventCreateNode:
		vm.handleNode(e.Node)
	case api.EventUpdateNode:
		// for updates, we're only adding the node to every plugin. if the node
		// no longer reports CSIInfo for a specific plugin, we will just leave
		// the stale data in the plugin. this should not have any adverse
		// effect, because the memory impact is small, and this operation
		// should not be frequent. this may change as the code for volumes
		// becomes more polished.
		vm.handleNode(e.Node)
	case api.EventDeleteNode:
		vm.handleNodeRemove(e.Node.ID)
	}
}

func (vm *Manager) createVolume(ctx context.Context, v *api.Volume) error {
	l := log.G(ctx).WithField("volume.id", v.ID).WithField("driver", v.Spec.Driver.Name)
	l.Info("creating volume")

	p, ok := vm.plugins[v.Spec.Driver.Name]
	if !ok {
		l.Errorf("volume creation failed: driver %s not found", v.Spec.Driver.Name)
		return errors.New("TODO")
	}

	info, err := p.CreateVolume(ctx, v)
	if err != nil {
		l.WithError(err).Error("volume create failed")
		return err
	}

	err = vm.store.Update(func(tx store.Tx) error {
		v2 := store.GetVolume(tx, v.ID)
		// the volume should never be missing. I don't know of even any race
		// condition that could result in this behavior. nevertheless, it's
		// better to do this than to segfault.
		if v2 == nil {
			return nil
		}

		v2.VolumeInfo = info

		return store.UpdateVolume(tx, v2)
	})
	if err != nil {
		l.WithError(err).Error("committing created volume to store failed")
	}
	return err
}

// enqueueVolume enqueues a new volume event, placing the Volume ID into
// pendingVolumes to be processed. Because enqueueVolume is only called in
// response to a new Volume update event, not for a retry, the retry number is
// always reset to 0.
func (vm *Manager) enqueueVolume(id string) {
	vm.pendingVolumes.Enqueue(id, 0)
}

// handleVolume processes a Volume. It determines if any relevant update has
// occurred, and does the required work to handle that update if so.
//
// returns an error if handling the volume failed and needs to be retried.
//
// even if an error is returned, the store may still be updated.
func (vm *Manager) handleVolume(ctx context.Context, id string) error {
	var volume *api.Volume
	vm.store.View(func(tx store.ReadTx) {
		volume = store.GetVolume(tx, id)
	})
	if volume == nil {
		// if the volume no longer exists, there is nothing to do, nothing to
		// retry, and no relevant error.
		return nil
	}

	if volume.VolumeInfo == nil {
		return vm.createVolume(ctx, volume)
	}

	if volume.PendingDelete {
		return vm.deleteVolume(ctx, volume)
	}

	updated := false
	// TODO(dperny): it's just pointers, but copying the entire PublishStatus
	// on each update might be intensive.

	// we take a copy of the PublishStatus slice, because if we succeed in an
	// unpublish operation, we will delete that status from PublishStatus.
	statuses := make([]*api.VolumePublishStatus, len(volume.PublishStatus))
	copy(statuses, volume.PublishStatus)

	// failedPublishOrUnpublish is a slice of nodes where publish or unpublish
	// operations failed. Publishing or unpublishing a volume can succeed or
	// fail in part. If any failures occur, we will add the node ID of the
	// publish operation that failed to this slice. Then, at the end of this
	// function, after we update the store, if there are any failed operations,
	// we will still return an error.
	failedPublishOrUnpublish := []string{}

	// adjustIndex is the number of entries deleted from volume.PublishStatus.
	// when we're deleting entries from volume.PublishStatus, the index of the
	// entry in statuses will no longer match the index of the same entry in
	// volume.PublishStatus. we subtract adjustIndex from i to get the index
	// where the entry is found after taking into account the deleted entries.
	adjustIndex := 0

	for i, status := range statuses {
		switch status.State {
		case api.VolumePublishStatus_PENDING_PUBLISH:
			plug := vm.plugins[volume.Spec.Driver.Name]
			publishContext, err := plug.PublishVolume(ctx, volume, status.NodeID)
			if err == nil {
				status.State = api.VolumePublishStatus_PUBLISHED
				status.PublishContext = publishContext
				status.Message = ""
			} else {
				status.Message = fmt.Sprintf("error publishing volume: %v", err)
				failedPublishOrUnpublish = append(failedPublishOrUnpublish, status.NodeID)
			}
			updated = true
		case api.VolumePublishStatus_PENDING_UNPUBLISH:
			plug := vm.plugins[volume.Spec.Driver.Name]
			err := plug.UnpublishVolume(ctx, volume, status.NodeID)
			if err == nil {
				// if there is no error with unpublishing, then we delete the
				// status from the statuses slice.
				j := i - adjustIndex
				volume.PublishStatus = append(volume.PublishStatus[:j], volume.PublishStatus[j+1:]...)
				adjustIndex++
			} else {
				status.Message = fmt.Sprintf("error unpublishing volume: %v", err)
				failedPublishOrUnpublish = append(failedPublishOrUnpublish, status.NodeID)
			}

			updated = true
		}
	}

	if updated {
		if err := vm.store.Update(func(tx store.Tx) error {
			// the publish status is now authoritative. read-update-write the
			// volume object.
			v := store.GetVolume(tx, volume.ID)
			if v == nil {
				// volume should never be deleted with pending publishes. if
				// this does occur somehow, then we will just ignore it, rather
				// than crashing.
				return nil
			}

			v.PublishStatus = volume.PublishStatus
			return store.UpdateVolume(tx, v)
		}); err != nil {
			return err
		}
	}

	if len(failedPublishOrUnpublish) > 0 {
		return fmt.Errorf("error publishing or unpublishing to some nodes: %v", failedPublishOrUnpublish)
	}
	return nil
}

// handleNode handles one node event
func (vm *Manager) handleNode(n *api.Node) {
	if n.Description == nil {
		return
	}
	// we just call AddNode on every update. Because it's just a map
	// assignment, this is probably faster than checking if something changed.
	for _, info := range n.Description.CSIInfo {
		p, ok := vm.plugins[info.PluginName]
		if !ok {
			// TODO(dperny): log something
			continue
		}
		p.AddNode(n.ID, info.NodeID)
	}
}

// handleNodeRemove handles a node delete event
func (vm *Manager) handleNodeRemove(nodeID string) {
	// we just call RemoveNode on every plugin, because it's probably quicker
	// than checking if the node was using that plugin.
	for _, plugin := range vm.plugins {
		plugin.RemoveNode(nodeID)
	}
}

func (vm *Manager) deleteVolume(ctx context.Context, v *api.Volume) error {
	// TODO(dperny): handle missing plugin
	plug := vm.plugins[v.Spec.Driver.Name]
	err := plug.DeleteVolume(ctx, v)
	if err != nil {
		return err
	}

	// TODO(dperny): handle update error
	return vm.store.Update(func(tx store.Tx) error {
		return store.DeleteVolume(tx, v.ID)
	})
}

package scheduler

import (
	"fmt"
	"strings"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/manager/state/store"
)

// the scheduler package does double duty -- in addition to choosing nodes, it
// must also choose volumes. this is because volumes are fungible, and can be
// scheduled to several nodes, and used by several tasks. we should endeavor to
// spread tasks across volumes, like we spread nodes. on the positive side,
// unlike nodes, volumes are not heirarchical. that is, we don't need to
// spread across multiple levels of a tree, only a flat set.

// volumeSet is the set of all volumes currently managed
type volumeSet struct {
	// volumes is a mapping of volume IDs to volumeInfo
	volumes map[string]volumeInfo
	// byGroup is a mapping from a volume group name to a set of volumes in
	// that group
	byGroup map[string]map[string]struct{}
	// byName is a mapping of volume names to swarmkit volume IDs.
	byName map[string]string
}

// volumeUsage contains information about the usage of a Volume by a specific
// task.
type volumeUsage struct {
	nodeID   string
	readOnly bool
}

// volumeInfo contains scheduler information about a given volume
type volumeInfo struct {
	volume *api.Volume
	tasks  map[string]volumeUsage
	// nodes is a set of nodes a volume is in use on. it maps a node ID to a
	// reference count for how many tasks are using the volume on that node.
	nodes map[string]int
}

func newVolumeSet() *volumeSet {
	return &volumeSet{
		volumes: map[string]volumeInfo{},
		byGroup: map[string]map[string]struct{}{},
		byName:  map[string]string{},
	}
}

func (vs *volumeSet) getVolume(id string) *api.Volume {
	// getVolume returns the volume object for the given ID as stored in the
	// volumeSet, or nil if none exists
	return vs.volumes[id].volume
}

func (vs *volumeSet) addOrUpdateVolume(v *api.Volume) {
	if info, ok := vs.volumes[v.ID]; !ok {
		vs.volumes[v.ID] = volumeInfo{
			volume: v,
			nodes:  map[string]int{},
			tasks:  map[string]volumeUsage{},
		}
	} else {
		// if the volume already exists in the set, then only update the volume
		// object, not the tasks map.
		info.volume = v
	}

	if set, ok := vs.byGroup[v.Spec.Group]; ok {
		set[v.ID] = struct{}{}
	} else {
		vs.byGroup[v.Spec.Group] = map[string]struct{}{v.ID: {}}
	}
	vs.byName[v.Spec.Annotations.Name] = v.ID
}

func (vs *volumeSet) removeVolume(volumeID string) {
	if info, ok := vs.volumes[volumeID]; ok {
		// if the volume exists in the set, look up its group ID and remove it
		// from the byGroup mapping as well
		group := info.volume.Spec.Group
		delete(vs.byGroup[group], volumeID)
		delete(vs.volumes, volumeID)
		delete(vs.byName, info.volume.Spec.Annotations.Name)
	}
}

// chooseTaskVolumes selects a set of VolumeAttachments for the task on the
// given node. it expects that the node was already validated to have the
// necessary volumes, but it will return an error if a full set of volumes is
// not available.
func (vs *volumeSet) chooseTaskVolumes(task *api.Task, nodeInfo *NodeInfo) ([]*api.VolumeAttachment, error) {
	volumes := []*api.VolumeAttachment{}

	// we'll reserve volumes in this loop, but release all of our reservations
	// before we finish. the caller will need to call reserveTaskVolumes after
	// calling this function
	// TODO(dperny): this is probably not optimal
	defer func() {
		for _, volume := range volumes {
			vs.releaseVolume(volume.ID, task.ID)
		}
	}()

	// TODO(dperny): handle non-container tasks
	c := task.Spec.GetContainer()
	if c == nil {
		return nil, nil
	}
	for _, mount := range task.Spec.GetContainer().Mounts {
		if mount.Type == api.MountTypeCSI {
			candidate := vs.isVolumeAvailableOnNode(&mount, nodeInfo)
			if candidate == "" {
				// TODO(dperny): return structured error types, instead of
				// error strings
				return nil, fmt.Errorf("cannot find volume to satisfy mount with source %v", mount.Source)
			}
			vs.reserveVolume(candidate, task.ID, nodeInfo.Node.ID, mount.ReadOnly)
			volumes = append(volumes, &api.VolumeAttachment{
				ID:     candidate,
				Source: mount.Source,
				Target: mount.Target,
			})
		}
	}

	return volumes, nil
}

// reserveTaskVolumes identifies all volumes currently in use on a task and
// marks them in the volumeSet as in use.
func (vs *volumeSet) reserveTaskVolumes(task *api.Task) {
	for _, va := range task.Volumes {
		// we shouldn't need to handle non-container tasks because those tasks
		// won't have any entries in task.Volumes.
		for _, mount := range task.Spec.GetContainer().Mounts {
			if mount.Source == va.Source && mount.Target == va.Target {
				vs.reserveVolume(va.ID, task.ID, task.NodeID, mount.ReadOnly)
			}
		}
	}
}

func (vs *volumeSet) reserveVolume(volumeID, taskID, nodeID string, readOnly bool) {
	info, ok := vs.volumes[volumeID]
	if !ok {
		// TODO(dperny): don't just return nothing.
		return
	}

	info.tasks[taskID] = volumeUsage{nodeID: nodeID, readOnly: readOnly}
	// increment the reference count for this node.
	info.nodes[nodeID] = info.nodes[nodeID] + 1
}

func (vs *volumeSet) releaseVolume(volumeID, taskID string) {
	info, ok := vs.volumes[volumeID]
	if !ok {
		// if the volume isn't in the set, no action to take.
		return
	}

	// decrement the reference count for this task's node
	usage, ok := info.tasks[taskID]
	if ok {
		// this is probably an unnecessarily high level of caution, but make
		// sure we don't go below zero on node count.
		if c := info.nodes[usage.nodeID]; c > 0 {
			info.nodes[usage.nodeID] = c - 1
		}
		delete(info.tasks, taskID)
	}
}

// freeVolumes finds volumes that are no longer in use on some nodes, and
// updates them to be unpublished from those nodes.
//
// TODO(dperny): this is messy and has a lot of overhead. it should be reworked
// to something more streamlined.
func (vs *volumeSet) freeVolumes(tx store.Tx) error {
	for volumeID, info := range vs.volumes {
		v := store.GetVolume(tx, volumeID)
		if v == nil {
			continue
		}

		changed := false
		for _, status := range v.PublishStatus {
			if info.nodes[status.NodeID] == 0 && status.State == api.VolumePublishStatus_PUBLISHED {
				status.State = api.VolumePublishStatus_PENDING_NODE_UNPUBLISH
				changed = true
			}
		}
		if changed {
			if err := store.UpdateVolume(tx, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// isVolumeAvailableOnNode checks if a volume satisfying the given mount is
// available on the given node.
//
// Returns the ID of the volume available, or an empty string if no such volume
// is found.
func (vs *volumeSet) isVolumeAvailableOnNode(mount *api.Mount, node *NodeInfo) string {
	source := mount.Source
	// first, discern whether we're looking for a group or a volume
	// try trimming off the "group:" prefix. if the resulting string is
	// different from the input string (meaning something has been trimmed),
	// then this volume is actually a volume group.
	if group := strings.TrimPrefix(source, "group:"); group != source {
		ids, ok := vs.byGroup[group]
		// if there are no volumes of this group specified, then no volume
		// meets the moutn criteria.
		if !ok {
			return ""
		}

		// iterate through all ids in the group, checking if any one meets the
		// spec.
		for id := range ids {
			if vs.checkVolume(id, node, mount.ReadOnly) {
				return id
			}
		}
		return ""
	}

	// if it's not a group, it's a name. resolve the volume name to its ID
	id, ok := vs.byName[source]
	if !ok || !vs.checkVolume(id, node, mount.ReadOnly) {
		return ""
	}
	return id
}

// checkVolume checks if an individual volume with the given ID can be placed
// on the given node.
func (vs *volumeSet) checkVolume(id string, info *NodeInfo, readOnly bool) bool {
	vi := vs.volumes[id]
	// first, check if the volume's availability is even Active. If not. no
	// reason to bother with anything further.
	if vi.volume != nil && vi.volume.Spec.Availability != api.VolumeAvailabilityActive {
		return false
	}

	// get the node topology for this volume
	var top *api.Topology
	// get the topology for this volume's driver on this node
	for _, info := range info.Description.CSIInfo {
		if info.PluginName == vi.volume.Spec.Driver.Name {
			top = info.AccessibleTopology
			break
		}
	}

	// check if the volume is available on this node. a volume's
	// availability on a node depends on its accessible topology, how it's
	// already being used, and how this task intends to use it.

	if vi.volume.Spec.AccessMode.Scope == api.VolumeScopeSingleNode {
		// if the volume is not in use on this node already, then it can't
		// be used here.
		for _, usage := range vi.tasks {
			if usage.nodeID != info.ID {
				return false
			}
		}
	}

	// even if the volume is currently on this node, or it has multi-node
	// access, the volume sharing needs to be compatible.
	switch vi.volume.Spec.AccessMode.Sharing {
	case api.VolumeSharingNone:
		// if the volume sharing is none, then the volume cannot be
		// used by another task
		if len(vi.tasks) > 0 {
			return false
		}
	case api.VolumeSharingOneWriter:
		// if the mount is not ReadOnly, and the volume has a writer, then
		// we this volume does not work.
		if !readOnly && hasWriter(vi) {
			return false
		}
	case api.VolumeSharingReadOnly:
		// if the volume sharing is read-only, then the Mount must also
		// be read-only
		if !readOnly {
			return false
		}
	}

	// then, do the quick check of whether this volume is in the topology.  if
	// the volume has an AccessibleTopology, and it does not lie within the
	// node's topology, then this volume won't fit.
	if !IsInTopology(top, vi.volume.VolumeInfo.AccessibleTopology) {
		return false
	}

	return true
}

// hasWriter is a helper function that returns true if at least one task is
// using this volume not in read-only mode.
func hasWriter(info volumeInfo) bool {
	for _, usage := range info.tasks {
		if !usage.readOnly {
			return true
		}
	}
	return false
}

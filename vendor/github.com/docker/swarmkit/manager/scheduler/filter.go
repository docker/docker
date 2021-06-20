package scheduler

import (
	"fmt"
	"strings"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/genericresource"
	"github.com/docker/swarmkit/manager/constraint"
)

// Filter checks whether the given task can run on the given node.
// A filter may only operate
type Filter interface {
	// SetTask returns true when the filter is enabled for a given task
	// and assigns the task to the filter. It returns false if the filter
	// isn't applicable to this task.  For instance, a constraints filter
	// would return `false` if the task doesn't contain any constraints.
	SetTask(*api.Task) bool

	// Check returns true if the task assigned by SetTask can be scheduled
	// into the given node. This function should not be called if SetTask
	// returned false.
	Check(*NodeInfo) bool

	// Explain what a failure of this filter means
	Explain(nodes int) string
}

// ReadyFilter checks that the node is ready to schedule tasks.
type ReadyFilter struct {
}

// SetTask returns true when the filter is enabled for a given task.
func (f *ReadyFilter) SetTask(_ *api.Task) bool {
	return true
}

// Check returns true if the task can be scheduled into the given node.
func (f *ReadyFilter) Check(n *NodeInfo) bool {
	return n.Status.State == api.NodeStatus_READY &&
		n.Spec.Availability == api.NodeAvailabilityActive
}

// Explain returns an explanation of a failure.
func (f *ReadyFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "1 node not available for new tasks"
	}
	return fmt.Sprintf("%d nodes not available for new tasks", nodes)
}

// ResourceFilter checks that the node has enough resources available to run
// the task.
type ResourceFilter struct {
	reservations *api.Resources
}

// SetTask returns true when the filter is enabled for a given task.
func (f *ResourceFilter) SetTask(t *api.Task) bool {
	r := t.Spec.Resources
	if r == nil || r.Reservations == nil {
		return false
	}

	res := r.Reservations
	if res.NanoCPUs == 0 && res.MemoryBytes == 0 && len(res.Generic) == 0 {
		return false
	}

	f.reservations = r.Reservations
	return true
}

// Check returns true if the task can be scheduled into the given node.
func (f *ResourceFilter) Check(n *NodeInfo) bool {
	if f.reservations.NanoCPUs > n.AvailableResources.NanoCPUs {
		return false
	}

	if f.reservations.MemoryBytes > n.AvailableResources.MemoryBytes {
		return false
	}

	for _, v := range f.reservations.Generic {
		enough, err := genericresource.HasEnough(n.AvailableResources.Generic, v)
		if err != nil || !enough {
			return false
		}
	}

	return true
}

// Explain returns an explanation of a failure.
func (f *ResourceFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "insufficient resources on 1 node"
	}
	return fmt.Sprintf("insufficient resources on %d nodes", nodes)
}

// PluginFilter checks that the node has a specific volume plugin installed
type PluginFilter struct {
	t *api.Task
}

func referencesVolumePlugin(mount api.Mount) bool {
	return mount.Type == api.MountTypeVolume &&
		mount.VolumeOptions != nil &&
		mount.VolumeOptions.DriverConfig != nil &&
		mount.VolumeOptions.DriverConfig.Name != "" &&
		mount.VolumeOptions.DriverConfig.Name != "local"

}

// SetTask returns true when the filter is enabled for a given task.
func (f *PluginFilter) SetTask(t *api.Task) bool {
	c := t.Spec.GetContainer()

	var volumeTemplates bool
	if c != nil {
		for _, mount := range c.Mounts {
			if referencesVolumePlugin(mount) {
				volumeTemplates = true
				break
			}
		}
	}

	if (c != nil && volumeTemplates) || len(t.Networks) > 0 || t.Spec.LogDriver != nil {
		f.t = t
		return true
	}

	return false
}

// Check returns true if the task can be scheduled into the given node.
// TODO(amitshukla): investigate storing Plugins as a map so it can be easily probed
func (f *PluginFilter) Check(n *NodeInfo) bool {
	if n.Description == nil || n.Description.Engine == nil {
		// If the node is not running Engine, plugins are not
		// supported.
		return true
	}

	// Get list of plugins on the node
	nodePlugins := n.Description.Engine.Plugins

	// Check if all volume plugins required by task are installed on node
	container := f.t.Spec.GetContainer()
	if container != nil {
		for _, mount := range container.Mounts {
			if referencesVolumePlugin(mount) {
				if _, exists := f.pluginExistsOnNode("Volume", mount.VolumeOptions.DriverConfig.Name, nodePlugins); !exists {
					return false
				}
			}
		}
	}

	// Check if all network plugins required by task are installed on node
	for _, tn := range f.t.Networks {
		if tn.Network != nil && tn.Network.DriverState != nil && tn.Network.DriverState.Name != "" {
			if _, exists := f.pluginExistsOnNode("Network", tn.Network.DriverState.Name, nodePlugins); !exists {
				return false
			}
		}
	}

	// It's possible that the LogDriver object does not carry a name, just some
	// configuration options. In that case, the plugin filter shouldn't fail to
	// schedule the task
	if f.t.Spec.LogDriver != nil && f.t.Spec.LogDriver.Name != "none" && f.t.Spec.LogDriver.Name != "" {
		// If there are no log driver types in the list at all, most likely this is
		// an older daemon that did not report this information. In this case don't filter
		if typeFound, exists := f.pluginExistsOnNode("Log", f.t.Spec.LogDriver.Name, nodePlugins); !exists && typeFound {
			return false
		}
	}
	return true
}

// pluginExistsOnNode returns true if the (pluginName, pluginType) pair is present in nodePlugins
func (f *PluginFilter) pluginExistsOnNode(pluginType string, pluginName string, nodePlugins []api.PluginDescription) (bool, bool) {
	var typeFound bool

	for _, np := range nodePlugins {
		if pluginType != np.Type {
			continue
		}
		typeFound = true

		if pluginName == np.Name {
			return true, true
		}
		// This does not use the reference package to avoid the
		// overhead of parsing references as part of the scheduling
		// loop. This is okay only because plugin names are a very
		// strict subset of the reference grammar that is always
		// name:tag.
		if strings.HasPrefix(np.Name, pluginName) && np.Name[len(pluginName):] == ":latest" {
			return true, true
		}
	}
	return typeFound, false
}

// Explain returns an explanation of a failure.
func (f *PluginFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "missing plugin on 1 node"
	}
	return fmt.Sprintf("missing plugin on %d nodes", nodes)
}

// ConstraintFilter selects only nodes that match certain labels.
type ConstraintFilter struct {
	constraints []constraint.Constraint
}

// SetTask returns true when the filter is enable for a given task.
func (f *ConstraintFilter) SetTask(t *api.Task) bool {
	if t.Spec.Placement == nil || len(t.Spec.Placement.Constraints) == 0 {
		return false
	}

	constraints, err := constraint.Parse(t.Spec.Placement.Constraints)
	if err != nil {
		// constraints have been validated at controlapi
		// if in any case it finds an error here, treat this task
		// as constraint filter disabled.
		return false
	}
	f.constraints = constraints
	return true
}

// Check returns true if the task's constraint is supported by the given node.
func (f *ConstraintFilter) Check(n *NodeInfo) bool {
	return constraint.NodeMatches(f.constraints, n.Node)
}

// Explain returns an explanation of a failure.
func (f *ConstraintFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "scheduling constraints not satisfied on 1 node"
	}
	return fmt.Sprintf("scheduling constraints not satisfied on %d nodes", nodes)
}

// PlatformFilter selects only nodes that run the required platform.
type PlatformFilter struct {
	supportedPlatforms []*api.Platform
}

// SetTask returns true when the filter is enabled for a given task.
func (f *PlatformFilter) SetTask(t *api.Task) bool {
	placement := t.Spec.Placement
	if placement != nil {
		// copy the platform information
		f.supportedPlatforms = placement.Platforms
		if len(placement.Platforms) > 0 {
			return true
		}
	}
	return false
}

// Check returns true if the task can be scheduled into the given node.
func (f *PlatformFilter) Check(n *NodeInfo) bool {
	// if the supportedPlatforms field is empty, then either it wasn't
	// provided or there are no constraints
	if len(f.supportedPlatforms) == 0 {
		return true
	}
	// check if the platform for the node is supported
	if n.Description != nil {
		if nodePlatform := n.Description.Platform; nodePlatform != nil {
			for _, p := range f.supportedPlatforms {
				if f.platformEqual(*p, *nodePlatform) {
					return true
				}
			}
		}
	}
	return false
}

func (f *PlatformFilter) platformEqual(imgPlatform, nodePlatform api.Platform) bool {
	// normalize "x86_64" architectures to "amd64"
	if imgPlatform.Architecture == "x86_64" {
		imgPlatform.Architecture = "amd64"
	}
	if nodePlatform.Architecture == "x86_64" {
		nodePlatform.Architecture = "amd64"
	}

	// normalize "aarch64" architectures to "arm64"
	if imgPlatform.Architecture == "aarch64" {
		imgPlatform.Architecture = "arm64"
	}
	if nodePlatform.Architecture == "aarch64" {
		nodePlatform.Architecture = "arm64"
	}

	if (imgPlatform.Architecture == "" || imgPlatform.Architecture == nodePlatform.Architecture) && (imgPlatform.OS == "" || imgPlatform.OS == nodePlatform.OS) {
		return true
	}
	return false
}

// Explain returns an explanation of a failure.
func (f *PlatformFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "unsupported platform on 1 node"
	}
	return fmt.Sprintf("unsupported platform on %d nodes", nodes)
}

// HostPortFilter checks that the node has a specific port available.
type HostPortFilter struct {
	t *api.Task
}

// SetTask returns true when the filter is enabled for a given task.
func (f *HostPortFilter) SetTask(t *api.Task) bool {
	if t.Endpoint != nil {
		for _, port := range t.Endpoint.Ports {
			if port.PublishMode == api.PublishModeHost && port.PublishedPort != 0 {
				f.t = t
				return true
			}
		}
	}

	return false
}

// Check returns true if the task can be scheduled into the given node.
func (f *HostPortFilter) Check(n *NodeInfo) bool {
	for _, port := range f.t.Endpoint.Ports {
		if port.PublishMode == api.PublishModeHost && port.PublishedPort != 0 {
			portSpec := hostPortSpec{protocol: port.Protocol, publishedPort: port.PublishedPort}
			if _, ok := n.usedHostPorts[portSpec]; ok {
				return false
			}
		}
	}

	return true
}

// Explain returns an explanation of a failure.
func (f *HostPortFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "host-mode port already in use on 1 node"
	}
	return fmt.Sprintf("host-mode port already in use on %d nodes", nodes)
}

// MaxReplicasFilter selects only nodes that does not exceed max replicas per node.
type MaxReplicasFilter struct {
	t *api.Task
}

// SetTask returns true when max replicas per node filter > 0 for a given task.
func (f *MaxReplicasFilter) SetTask(t *api.Task) bool {
	if t.Spec.Placement != nil && t.Spec.Placement.MaxReplicas > 0 {
		f.t = t
		return true
	}

	return false
}

// Check returns true if there is less active (assigned or pre-assigned) tasks for this service on current node than set to MaxReplicas limit
func (f *MaxReplicasFilter) Check(n *NodeInfo) bool {
	return uint64(n.ActiveTasksCountByService[f.t.ServiceID]) < f.t.Spec.Placement.MaxReplicas
}

// Explain returns an explanation of a failure.
func (f *MaxReplicasFilter) Explain(nodes int) string {
	return "max replicas per node limit exceed"
}

type VolumesFilter struct {
	vs *volumeSet
	t  *api.Task

	// requestedVolumes is a set of volumes requested by the task. This can
	// include either volume names or volume groups. Volume groups, as in the
	// Mount.Source field, are prefixed with "group:"
	requestedVolumes []*api.Mount
}

func (f *VolumesFilter) SetTask(t *api.Task) bool {
	// if there is no volume Manager, skip this filter always
	if f.vs == nil {
		return false
	}
	f.t = t
	// reset requestedVolumes every time we set a task, so we don't
	// accidentally append to the last task's set of requested volumes.
	f.requestedVolumes = []*api.Mount{}

	// t should never be nil, but we should ensure that it is not just in case
	// we make mistakes in the future.
	if t == nil {
		return false
	}

	c := t.Spec.GetContainer()
	if c == nil {
		return false
	}

	// hasCSI will be set true if one of the mounts is a CSI-type mount.
	hasCSI := false
	for _, mount := range c.Mounts {
		if mount.Type == api.MountTypeCSI {
			hasCSI = true
			f.requestedVolumes = append(f.requestedVolumes, &mount)
		}
	}
	return hasCSI
}

func (f *VolumesFilter) Check(nodeInfo *NodeInfo) bool {
	for _, mount := range f.requestedVolumes {
		if f.vs.isVolumeAvailableOnNode(mount, nodeInfo) != "" {
			return true
		}
	}

	return false
}

func (f *VolumesFilter) Explain(nodes int) string {
	if nodes == 1 {
		return "cannot fulfill requested CSI volume mounts on 1 node"
	}
	return fmt.Sprintf(
		"cannot fulfill requested CSI volume mounts on %d nodes", nodes,
	)
}

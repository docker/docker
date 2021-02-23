package store

import "github.com/docker/swarmkit/api"

// By is an interface type passed to Find methods. Implementations must be
// defined in this package.
type By interface {
	// isBy allows this interface to only be satisfied by certain internal
	// types.
	isBy()
}

type byAll struct{}

func (a byAll) isBy() {
}

// All is an argument that can be passed to find to list all items in the
// set.
var All byAll

type byNamePrefix string

func (b byNamePrefix) isBy() {
}

// ByNamePrefix creates an object to pass to Find to select by query.
func ByNamePrefix(namePrefix string) By {
	return byNamePrefix(namePrefix)
}

type byIDPrefix string

func (b byIDPrefix) isBy() {
}

// ByIDPrefix creates an object to pass to Find to select by query.
func ByIDPrefix(idPrefix string) By {
	return byIDPrefix(idPrefix)
}

type byName string

func (b byName) isBy() {
}

// ByName creates an object to pass to Find to select by name.
func ByName(name string) By {
	return byName(name)
}

type byService string

func (b byService) isBy() {
}

type byRuntime string

func (b byRuntime) isBy() {
}

// ByRuntime creates an object to pass to Find to select by runtime.
func ByRuntime(runtime string) By {
	return byRuntime(runtime)
}

// ByServiceID creates an object to pass to Find to select by service.
func ByServiceID(serviceID string) By {
	return byService(serviceID)
}

type byNode string

func (b byNode) isBy() {
}

// ByNodeID creates an object to pass to Find to select by node.
func ByNodeID(nodeID string) By {
	return byNode(nodeID)
}

type bySlot struct {
	serviceID string
	slot      uint64
}

func (b bySlot) isBy() {
}

// BySlot creates an object to pass to Find to select by slot.
func BySlot(serviceID string, slot uint64) By {
	return bySlot{serviceID: serviceID, slot: slot}
}

type byDesiredState api.TaskState

func (b byDesiredState) isBy() {
}

// ByDesiredState creates an object to pass to Find to select by desired state.
func ByDesiredState(state api.TaskState) By {
	return byDesiredState(state)
}

type byTaskState api.TaskState

func (b byTaskState) isBy() {
}

// ByTaskState creates an object to pass to Find to select by task state.
func ByTaskState(state api.TaskState) By {
	return byTaskState(state)
}

type byRole api.NodeRole

func (b byRole) isBy() {
}

// ByRole creates an object to pass to Find to select by role.
func ByRole(role api.NodeRole) By {
	return byRole(role)
}

type byMembership api.NodeSpec_Membership

func (b byMembership) isBy() {
}

// ByMembership creates an object to pass to Find to select by Membership.
func ByMembership(membership api.NodeSpec_Membership) By {
	return byMembership(membership)
}

type byReferencedNetworkID string

func (b byReferencedNetworkID) isBy() {
}

// ByReferencedNetworkID creates an object to pass to Find to search for a
// service or task that references a network with the given ID.
func ByReferencedNetworkID(networkID string) By {
	return byReferencedNetworkID(networkID)
}

type byReferencedSecretID string

func (b byReferencedSecretID) isBy() {
}

// ByReferencedSecretID creates an object to pass to Find to search for a
// service or task that references a secret with the given ID.
func ByReferencedSecretID(secretID string) By {
	return byReferencedSecretID(secretID)
}

type byReferencedConfigID string

func (b byReferencedConfigID) isBy() {
}

// ByReferencedConfigID creates an object to pass to Find to search for a
// service or task that references a config with the given ID.
func ByReferencedConfigID(configID string) By {
	return byReferencedConfigID(configID)
}

type byVolumeAttachment string

func (b byVolumeAttachment) isBy() {}

// ByVolumeAttachment creates an object to pass to Find to search for a Task
// that has been assigned the given ID.
func ByVolumeAttachment(volumeID string) By {
	return byVolumeAttachment(volumeID)
}

type byKind string

func (b byKind) isBy() {
}

// ByKind creates an object to pass to Find to search for a Resource of a
// particular kind.
func ByKind(kind string) By {
	return byKind(kind)
}

type byCustom struct {
	objType string
	index   string
	value   string
}

func (b byCustom) isBy() {
}

// ByCustom creates an object to pass to Find to search a custom index.
func ByCustom(objType, index, value string) By {
	return byCustom{
		objType: objType,
		index:   index,
		value:   value,
	}
}

type byCustomPrefix struct {
	objType string
	index   string
	value   string
}

func (b byCustomPrefix) isBy() {
}

// ByCustomPrefix creates an object to pass to Find to search a custom index by
// a value prefix.
func ByCustomPrefix(objType, index, value string) By {
	return byCustomPrefix{
		objType: objType,
		index:   index,
		value:   value,
	}
}

// ByVolumeGroup creates an object to pass to Find to search for volumes
// belonging to a particular group.
func ByVolumeGroup(group string) By {
	return byVolumeGroup(group)
}

type byVolumeGroup string

func (b byVolumeGroup) isBy() {
}

// ByDriver creates an object to pass to Find to search for objects using a
// specific driver.
func ByDriver(driver string) By {
	return byDriver(driver)
}

type byDriver string

func (b byDriver) isBy() {
}

package swarm // import "github.com/docker/docker/api/types/swarm"

import (
	"github.com/docker/docker/api/types/mount"
)

// Volume represents a swarm Volume object, which is backed by a CSI storage
// plugin.
type Volume struct {
	ID string
	Meta
	Spec VolumeSpec `json:",omitempty"`

	// PublishStatus contains the status of the Volume as it pertains to its
	// publishing on Nodes.
	PublishStatus []*VolumePublishStatus `json:",omitempty"`

	// VolumeInfo contains information about the global status of the volume.
	VolumeInfo *VolumeInfo `json:",omitempty"`
}

// VolumeSpec defines a swarm Volume.
type VolumeSpec struct {
	Annotations

	// Group defines the volume group of this volume. Volumes belonging to the
	// same group can be referred to by group name when creating Services. This
	// allows any volume from the group to be used for the Service.
	Group string `json:",omitempty"`

	// Driver defines the CSI plugin to use for this volume. The Options field
	// of the Driver is passed to the CSI CreateVolumeRequest as the
	// "parameters" field. The Driver must be specified, as there is no default
	// CSI plugin.
	Driver *mount.Driver `json:",omitempty"`

	// AccessMode defines the access mode of the volume.
	AccessMode *VolumeAccessMode `json:",omitempty"`

	// Secrets represents Swarm secrets to use when communicating with the
	// CSI plugin.
	Secrets []VolumeSecret `json:",omitempty"`

	// AccessibilityRequirements specifies where a volume must be accessible
	// from.
	//
	// This field must be empty if the plugin does not support
	// VOLUME_ACCESSIBILITY_CONSTRAINTS capabilities. If it is present but the
	// plugin does not support it, volume will not be created.
	//
	// If AccessibilityRequirements is empty, but the plugin does support
	// VOLUME_ACCESSIBILITY_CONSTRAINTS, then Swarmkit will assume the entire
	// cluster is a valid target for the volume.
	AccessibilityRequirements *TopologyRequirement `json:",omitempty"`

	// CapacityRange is the capacity this volume should be created with. If
	// nil, the plugin will decide the capacity.
	CapacityRange *CapacityRange `json:",omitempty"`

	// Availability is the Volume's desired availability. Analogous to Node
	// Availability, this allows the user to take volumes offline in order to
	// update or delete them.
	Availability VolumeAvailability `json:",omitempty"`
}

// VolumePublishState represents the state of a Volume as it pertains to its
// use on a particular Node.
type VolumePublishState string

const (
	// VolumePendingPublish indicates that the volume should be published on
	// this node, but the call to ControllerPublishVolume has not been
	// successfully completed yet and the result recorded by swarmkit.
	VolumePendingPublish VolumePublishState = "pending publish"

	// VolumePublished means the volume is published successfully to the node.
	VolumePublished VolumePublishState = "published"

	// VolumePendingNodeUnpublish indicates that the Volume should be
	// unpublished on the Node, and we're waiting for confirmation that it has
	// done so.  After the Node has confirmed that the Volume has been
	// unpublished, the state will move to VolumePendingUnpublish.
	VolumePendingNodeUnpublish VolumePublishState = "pending node unpublish"

	// VolumePendingUnpublish means the volume is still published to the node
	// by the controller, awaiting the operation to unpublish it.
	VolumePendingUnpublish VolumePublishState = "pending controller unpublish"
)

// VolumePublishStatus represents the status of the volume as published to an
// individual node
type VolumePublishStatus struct {
	// NodeID is the ID of the swarm node this Volume is published to.
	NodeID string `json:",omitempty"`

	// State is the publish state of the volume.
	State VolumePublishState `json:",omitempty"`

	// PublishContext is the PublishContext returned by the CSI plugin when
	// a volume is published.
	PublishContext map[string]string `json:",omitempty"`
}

// VolumeInfo contains information about the Volume as a whole as provided by
// the CSI storage plugin.
type VolumeInfo struct {
	// CapacityBytes is the capacity of the volume in bytes. A value of 0
	// indicates that the capacity is unknown.
	CapacityBytes int `json:",omitempty"`

	// VolumeContext is the context originating from the CSI storage plugin
	// when the Volume is created.
	VolumeContext map[string]string `json:",omitempty"`

	// VolumeID is the ID of the Volume as seen by the CSI storage plugin. This
	// is distinct from the Volume's Swarm ID, which is the ID used by all of
	// the Docker Engine to refer to the Volume. If this field is blank, then
	// the Volume has not been successfully created yet.
	VolumeID string `json:",omitempty"`

	// AccessibleTopolgoy is the topology this volume is actually accessible
	// from.
	AccessibleTopology []Topology `json:",omitempty"`
}

// VolumeScope defines the Scope of a CSI Volume. This is how many nodes a
// Volume can be accessed simultaneously on.
type VolumeScope string

const (
	// VolumeScopeSingleNode indicates the volume can be accessed on one node
	// at a time.
	VolumeScopeSingleNode VolumeScope = "single"

	// VolumeScopeMultiNode indicates the volume can be accessed on many nodes
	// at the same time.
	VolumeScopeMultiNode VolumeScope = "multi"
)

// VolumeSharing defines the Sharing of a CSI Volume. This is how Tasks using a
// Volume at the same time can use it.
type VolumeSharing string

const (
	// VolumeSharingNone indicates that only one Task may use the Volume at a
	// time.
	VolumeSharingNone VolumeSharing = "none"

	// VolumeSharingReadOnly indicates that the Volume may be shared by any
	// number of Tasks, but they must be read-only.
	VolumeSharingReadOnly VolumeSharing = "readonly"

	// VolumeSharingOneWriter indicates that the Volume may be shared by any
	// number of Tasks, but all after the first must be read-only.
	VolumeSharingOneWriter VolumeSharing = "onewriter"

	// VolumeSharingAll means that the Volume may be shared by any number of
	// Tasks, as readers or writers.
	VolumeSharingAll VolumeSharing = "all"
)

// VolumeAccessMode defines how a Volume can be used across Tasks and across
// Nodes.
type VolumeAccessMode struct {
	// Scope defines how the Volume can be used across different Nodes.
	Scope VolumeScope `json:",omitempty"`

	// Sharing indicates how the Volume can be used by different Tasks.
	Sharing VolumeSharing `json:",omitempty"`
}

// VolumeSecret represents a Swarm Secret value that must be passed to the CSI
// storage plugin when operating on this Volume. It represents one key-value
// pair of possibly many.
type VolumeSecret struct {
	// Key represents the key of the key-value pair.
	Key string `json:",omitempty"`

	// Secret represents the swarmkit Secret object from which to read data.
	// The Secret data is used as the value of the key-value pair. This can be
	// a Secret name or ID.
	Secret string `json:",omitempty"`
}

// CapacityRange describes the minimum and maximum capacity a volume should be
// created with
type CapacityRange struct {
	// RequiredBytes specifies that a volume must be at least this big. The
	// value of 0 indicates an unspecified minimum.
	RequiredBytes uint64

	// LimitBytes specifies that a volume must not be bigger than this. The
	// value of 0 indicates an unspecified maximum
	LimitBytes uint64
}

// VolumeAvailability specifies the availability of the volume.
type VolumeAvailability string

const (
	// VolumeAvailabilityActive indicates that the volume is active and fully
	// schedulable on the cluster.
	VolumeAvailabilityActive VolumeAvailability = "active"

	// VolumeAvailabilityPause indicates that no new workloads should use the
	// volume, but existing workloads can continue to use it.
	VolumeAvailabilityPause VolumeAvailability = "pause"

	// VolumeAvailabilityDrain indicates that all workloads using this volume
	// should be rescheduled, and the volume unpublished from all nodes.
	VolumeAvailabilityDrain VolumeAvailability = "drain"
)

// TopologyRequirement expresses the user's requirements for a volume's
// accessible topology.
type TopologyRequirement struct {
	// Requisite specifices the list of topologies that the volume must be
	// accessible from.
	//
	// Taken verbatim from the CSI Spec:
	//
	// Specifies the list of topologies the provisioned volume MUST be
	// accessible from.
	// This field is OPTIONAL. If TopologyRequirement is specified either
	// requisite or preferred or both MUST be specified.
	//
	// If requisite is specified, the provisioned volume MUST be
	// accessible from at least one of the requisite topologies.
	//
	// Given
	//   x = number of topologies provisioned volume is accessible from
	//   n = number of requisite topologies
	// The CO MUST ensure n >= 1. The SP MUST ensure x >= 1
	// If x==n, then the SP MUST make the provisioned volume available to
	// all topologies from the list of requisite topologies. If it is
	// unable to do so, the SP MUST fail the CreateVolume call.
	// For example, if a volume should be accessible from a single zone,
	// and requisite =
	//   {"region": "R1", "zone": "Z2"}
	// then the provisioned volume MUST be accessible from the "region"
	// "R1" and the "zone" "Z2".
	// Similarly, if a volume should be accessible from two zones, and
	// requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"}
	// then the provisioned volume MUST be accessible from the "region"
	// "R1" and both "zone" "Z2" and "zone" "Z3".
	//
	// If x<n, then the SP SHALL choose x unique topologies from the list
	// of requisite topologies. If it is unable to do so, the SP MUST fail
	// the CreateVolume call.
	// For example, if a volume should be accessible from a single zone,
	// and requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"}
	// then the SP may choose to make the provisioned volume available in
	// either the "zone" "Z2" or the "zone" "Z3" in the "region" "R1".
	// Similarly, if a volume should be accessible from two zones, and
	// requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"},
	//   {"region": "R1", "zone": "Z4"}
	// then the provisioned volume MUST be accessible from any combination
	// of two unique topologies: e.g. "R1/Z2" and "R1/Z3", or "R1/Z2" and
	//  "R1/Z4", or "R1/Z3" and "R1/Z4".
	//
	// If x>n, then the SP MUST make the provisioned volume available from
	// all topologies from the list of requisite topologies and MAY choose
	// the remaining x-n unique topologies from the list of all possible
	// topologies. If it is unable to do so, the SP MUST fail the
	// CreateVolume call.
	// For example, if a volume should be accessible from two zones, and
	// requisite =
	//   {"region": "R1", "zone": "Z2"}
	// then the provisioned volume MUST be accessible from the "region"
	// "R1" and the "zone" "Z2" and the SP may select the second zone
	// independently, e.g. "R1/Z4".
	Requisite []Topology `json:",omitempty"`

	// Preferred specifies the list of topologies the volume would be preferred
	// in.
	//
	// Taken from the CSI spec:
	//
	// Specifies the list of topologies the CO would prefer the volume to
	// be provisioned in.
	//
	// This field is OPTIONAL. If TopologyRequirement is specified either
	// requisite or preferred or both MUST be specified.
	//
	// An SP MUST attempt to make the provisioned volume available using
	// the preferred topologies in order from first to last.
	//
	// If requisite is specified, all topologies in preferred list MUST
	// also be present in the list of requisite topologies.
	//
	// If the SP is unable to to make the provisioned volume available
	// from any of the preferred topologies, the SP MAY choose a topology
	// from the list of requisite topologies.
	// If the list of requisite topologies is not specified, then the SP
	// MAY choose from the list of all possible topologies.
	// If the list of requisite topologies is specified and the SP is
	// unable to to make the provisioned volume available from any of the
	// requisite topologies it MUST fail the CreateVolume call.
	//
	// Example 1:
	// Given a volume should be accessible from a single zone, and
	// requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"}
	// preferred =
	//   {"region": "R1", "zone": "Z3"}
	// then the the SP SHOULD first attempt to make the provisioned volume
	// available from "zone" "Z3" in the "region" "R1" and fall back to
	// "zone" "Z2" in the "region" "R1" if that is not possible.
	//
	// Example 2:
	// Given a volume should be accessible from a single zone, and
	// requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"},
	//   {"region": "R1", "zone": "Z4"},
	//   {"region": "R1", "zone": "Z5"}
	// preferred =
	//   {"region": "R1", "zone": "Z4"},
	//   {"region": "R1", "zone": "Z2"}
	// then the the SP SHOULD first attempt to make the provisioned volume
	// accessible from "zone" "Z4" in the "region" "R1" and fall back to
	// "zone" "Z2" in the "region" "R1" if that is not possible. If that
	// is not possible, the SP may choose between either the "zone"
	// "Z3" or "Z5" in the "region" "R1".
	//
	// Example 3:
	// Given a volume should be accessible from TWO zones (because an
	// opaque parameter in CreateVolumeRequest, for example, specifies
	// the volume is accessible from two zones, aka synchronously
	// replicated), and
	// requisite =
	//   {"region": "R1", "zone": "Z2"},
	//   {"region": "R1", "zone": "Z3"},
	//   {"region": "R1", "zone": "Z4"},
	//   {"region": "R1", "zone": "Z5"}
	// preferred =
	//   {"region": "R1", "zone": "Z5"},
	//   {"region": "R1", "zone": "Z3"}
	// then the the SP SHOULD first attempt to make the provisioned volume
	// accessible from the combination of the two "zones" "Z5" and "Z3" in
	// the "region" "R1". If that's not possible, it should fall back to
	// a combination of "Z5" and other possibilities from the list of
	// requisite. If that's not possible, it should fall back  to a
	// combination of "Z3" and other possibilities from the list of
	// requisite. If that's not possible, it should fall back  to a
	// combination of other possibilities from the list of requisite.
	Preferred []Topology `json:",omitempty"`
}

// Topology is a map of topological domains to topological segments.
//
// This description is taken verbatim from the CSI Spec:
//
// A topological domain is a sub-division of a cluster, like "region",
// "zone", "rack", etc.
// A topological segment is a specific instance of a topological domain,
// like "zone3", "rack3", etc.
// For example {"com.company/zone": "Z1", "com.company/rack": "R3"}
// Valid keys have two segments: an OPTIONAL prefix and name, separated
// by a slash (/), for example: "com.company.example/zone".
// The key name segment is REQUIRED. The prefix is OPTIONAL.
// The key name MUST be 63 characters or less, begin and end with an
// alphanumeric character ([a-z0-9A-Z]), and contain only dashes (-),
// underscores (_), dots (.), or alphanumerics in between, for example
// "zone".
// The key prefix MUST be 63 characters or less, begin and end with a
// lower-case alphanumeric character ([a-z0-9]), contain only
// dashes (-), dots (.), or lower-case alphanumerics in between, and
// follow domain name notation format
// (https://tools.ietf.org/html/rfc1035#section-2.3.1).
// The key prefix SHOULD include the plugin's host company name and/or
// the plugin name, to minimize the possibility of collisions with keys
// from other plugins.
// If a key prefix is specified, it MUST be identical across all
// topology keys returned by the SP (across all RPCs).
// Keys MUST be case-insensitive. Meaning the keys "Zone" and "zone"
// MUST not both exist.
// Each value (topological segment) MUST contain 1 or more strings.
// Each string MUST be 63 characters or less and begin and end with an
// alphanumeric character with '-', '_', '.', or alphanumerics in
// between.
type Topology struct {
	Segments map[string]string `json:",omitempty"`
}

// VolumeAttachment contains the associating a Volume to a Task.
type VolumeAttachment struct {
	// ID is the Swarmkit ID of the VOlume. This is not the CSI VolumeId.
	ID string `json:",omitempty"`

	// Source, together with Target, indicates the Mount, as specified in the
	// ContainerSpec, that this volume fulfills.
	Source string `json:",omitempty"`

	// Target, together with Source, indicates the Mount, as specified
	// in the ContainerSpec, that this volume fulfills.
	Target string `json:",omitempty"`
}

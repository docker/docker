package csi

// The `csi` package contains code for managing Swarmkit Cluster Volumes,
// which are powered by CSI drivers.
//
// This package stands separately from other manager components because of the
// unique nature of volumes. Volumes need to be allocated before they can be
// used, but the availability of a volume also imposes a scheduling constraint
// on the node. Further, the CSI lifecycle requires many different RPC calls at
// many points in the volume's life, which brings it out of the purview of any
// one component.
//
// In an ideal world, this package would live wholely within the allocator
// package, but the allocator is very fragile, and modifying it is more trouble
// than it's worth.

// Volume Lifecycle in Swarm
//
// Creation
//
// When a volume is created, the first thing the allocator does is contact the
// relevant CSI plugin in order to ensure that the volume is created, and to
// retrieve the associated volume ID. Volumes are always created when the
// swarmkit object is created, as opposed to being created when demanded by a
// Service.
//
// Assignment
//
// After a volume has been created, it may be used by one or more Tasks.

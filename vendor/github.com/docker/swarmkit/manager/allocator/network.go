package allocator

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/go-events"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/allocator/cnmallocator"
	"github.com/docker/swarmkit/manager/allocator/networkallocator"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/protobuf/ptypes"
	"github.com/pkg/errors"
)

const (
	// Network allocator Voter ID for task allocation vote.
	networkVoter           = "network"
	allocatedStatusMessage = "pending task scheduling"
)

var (
	// ErrNoIngress is returned when no ingress network is found in store
	ErrNoIngress = errors.New("no ingress network found")
	errNoChanges = errors.New("task unchanged")

	retryInterval = 5 * time.Minute
)

// Network context information which is used throughout the network allocation code.
type networkContext struct {
	ingressNetwork *api.Network
	// Instance of the low-level network allocator which performs
	// the actual network allocation.
	nwkAllocator networkallocator.NetworkAllocator

	// A set of tasks which are ready to be allocated as a batch. This is
	// distinct from "unallocatedTasks" which are tasks that failed to
	// allocate on the first try, being held for a future retry.
	pendingTasks map[string]*api.Task

	// A set of unallocated tasks which will be revisited if any thing
	// changes in system state that might help task allocation.
	unallocatedTasks map[string]*api.Task

	// A set of unallocated services which will be revisited if
	// any thing changes in system state that might help service
	// allocation.
	unallocatedServices map[string]*api.Service

	// A set of unallocated networks which will be revisited if
	// any thing changes in system state that might help network
	// allocation.
	unallocatedNetworks map[string]*api.Network

	// lastRetry is the last timestamp when unallocated
	// tasks/services/networks were retried.
	lastRetry time.Time

	// somethingWasDeallocated indicates that we just deallocated at
	// least one service/task/network, so we should retry failed
	// allocations (in we are experiencing IP exhaustion and an IP was
	// released).
	somethingWasDeallocated bool
}

func (a *Allocator) doNetworkInit(ctx context.Context) (err error) {
	var netConfig *cnmallocator.NetworkConfig
	// There are two ways user can invoke swarm init
	// with default address pool & vxlan port  or with only vxlan port
	// hence we need two different way to construct netconfig
	if a.networkConfig != nil {
		if a.networkConfig.DefaultAddrPool != nil {
			netConfig = &cnmallocator.NetworkConfig{
				DefaultAddrPool: a.networkConfig.DefaultAddrPool,
				SubnetSize:      a.networkConfig.SubnetSize,
				VXLANUDPPort:    a.networkConfig.VXLANUDPPort,
			}
		} else if a.networkConfig.VXLANUDPPort != 0 {
			netConfig = &cnmallocator.NetworkConfig{
				DefaultAddrPool: nil,
				SubnetSize:      0,
				VXLANUDPPort:    a.networkConfig.VXLANUDPPort,
			}
		}
	}

	na, err := cnmallocator.New(a.pluginGetter, netConfig)
	if err != nil {
		return err
	}

	nc := &networkContext{
		nwkAllocator:        na,
		pendingTasks:        make(map[string]*api.Task),
		unallocatedTasks:    make(map[string]*api.Task),
		unallocatedServices: make(map[string]*api.Service),
		unallocatedNetworks: make(map[string]*api.Network),
		lastRetry:           time.Now(),
	}
	a.netCtx = nc
	defer func() {
		// Clear a.netCtx if initialization was unsuccessful.
		if err != nil {
			a.netCtx = nil
		}
	}()

	// Ingress network is now created at cluster's first time creation.
	// Check if we have the ingress network. If found, make sure it is
	// allocated, before reading all network objects for allocation.
	// If not found, it means it was removed by user, nothing to do here.
	ingressNetwork, err := GetIngressNetwork(a.store)
	switch err {
	case nil:
		// Try to complete ingress network allocation before anything else so
		// that the we can get the preferred subnet for ingress network.
		nc.ingressNetwork = ingressNetwork
		if !na.IsAllocated(nc.ingressNetwork) {
			if err := a.allocateNetwork(ctx, nc.ingressNetwork); err != nil {
				log.G(ctx).WithError(err).Error("failed allocating ingress network during init")
			} else if err := a.store.Batch(func(batch *store.Batch) error {
				if err := a.commitAllocatedNetwork(ctx, batch, nc.ingressNetwork); err != nil {
					log.G(ctx).WithError(err).Error("failed committing allocation of ingress network during init")
				}
				return nil
			}); err != nil {
				log.G(ctx).WithError(err).Error("failed committing allocation of ingress network during init")
			}
		}
	case ErrNoIngress:
		// Ingress network is not present in store, It means user removed it
		// and did not create a new one.
	default:
		return errors.Wrap(err, "failure while looking for ingress network during init")
	}

	// First, allocate (read it as restore) objects likes network,nodes,serives
	// and tasks that were already allocated. Then go on the allocate objects
	// that are in raft and were previously not allocated. The reason being, during
	// restore, we  make sure that we populate the allocated states of
	// the objects in the raft onto our in memory state.
	if err := a.allocateNetworks(ctx, true); err != nil {
		return err
	}

	if err := a.allocateNodes(ctx, true); err != nil {
		return err
	}

	if err := a.allocateServices(ctx, true); err != nil {
		return err
	}
	if err := a.allocateTasks(ctx, true); err != nil {
		return err
	}
	// Now allocate objects that were not previously allocated
	// but were present in the raft.
	if err := a.allocateNetworks(ctx, false); err != nil {
		return err
	}

	if err := a.allocateNodes(ctx, false); err != nil {
		return err
	}

	if err := a.allocateServices(ctx, false); err != nil {
		return err
	}
	return a.allocateTasks(ctx, false)
}

func (a *Allocator) doNetworkAlloc(ctx context.Context, ev events.Event) {
	nc := a.netCtx

	switch v := ev.(type) {
	case api.EventCreateNetwork:
		n := v.Network.Copy()
		if nc.nwkAllocator.IsAllocated(n) {
			break
		}
		if IsIngressNetwork(n) && nc.ingressNetwork != nil {
			log.G(ctx).Errorf("Cannot allocate ingress network %s (%s) because another ingress network is already present: %s (%s)",
				n.ID, n.Spec.Annotations.Name, nc.ingressNetwork.ID, nc.ingressNetwork.Spec.Annotations.Name)
			break
		}

		if err := a.allocateNetwork(ctx, n); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed allocation for network %s", n.ID)
			break
		}

		if err := a.store.Batch(func(batch *store.Batch) error {
			return a.commitAllocatedNetwork(ctx, batch, n)
		}); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to commit allocation for network %s", n.ID)
		}
		if IsIngressNetwork(n) {
			nc.ingressNetwork = n
		}
	case api.EventDeleteNetwork:
		n := v.Network.Copy()

		if IsIngressNetwork(n) && nc.ingressNetwork != nil && nc.ingressNetwork.ID == n.ID {
			nc.ingressNetwork = nil
		}

		if err := a.deallocateNodeAttachments(ctx, n.ID); err != nil {
			log.G(ctx).WithError(err).Error(err)
		}

		// The assumption here is that all dependent objects
		// have been cleaned up when we are here so the only
		// thing that needs to happen is free the network
		// resources.
		if err := nc.nwkAllocator.Deallocate(n); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed during network free for network %s", n.ID)
		} else {
			nc.somethingWasDeallocated = true
		}

		delete(nc.unallocatedNetworks, n.ID)
	case api.EventCreateService:
		var s *api.Service
		a.store.View(func(tx store.ReadTx) {
			s = store.GetService(tx, v.Service.ID)
		})

		if s == nil {
			break
		}

		if nc.nwkAllocator.IsServiceAllocated(s) {
			break
		}

		if err := a.allocateService(ctx, s, false); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed allocation for service %s", s.ID)
			break
		}

		if err := a.store.Batch(func(batch *store.Batch) error {
			return a.commitAllocatedService(ctx, batch, s)
		}); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to commit allocation for service %s", s.ID)
		}
	case api.EventUpdateService:
		// We may have already allocated this service. If a create or
		// update event is older than the current version in the store,
		// we run the risk of allocating the service a second time.
		// Only operate on the latest version of the service.
		var s *api.Service
		a.store.View(func(tx store.ReadTx) {
			s = store.GetService(tx, v.Service.ID)
		})

		if s == nil {
			break
		}

		if nc.nwkAllocator.IsServiceAllocated(s) {
			if !nc.nwkAllocator.HostPublishPortsNeedUpdate(s) {
				break
			}
			updatePortsInHostPublishMode(s)
		} else {
			if err := a.allocateService(ctx, s, false); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed allocation during update of service %s", s.ID)
				break
			}
		}

		if err := a.store.Batch(func(batch *store.Batch) error {
			return a.commitAllocatedService(ctx, batch, s)
		}); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to commit allocation during update for service %s", s.ID)
			nc.unallocatedServices[s.ID] = s
		} else {
			delete(nc.unallocatedServices, s.ID)
		}
	case api.EventDeleteService:
		s := v.Service.Copy()

		if err := nc.nwkAllocator.DeallocateService(s); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed deallocation during delete of service %s", s.ID)
		} else {
			nc.somethingWasDeallocated = true
		}

		// Remove it from unallocatedServices just in case
		// it's still there.
		delete(nc.unallocatedServices, s.ID)
	case api.EventCreateNode, api.EventUpdateNode, api.EventDeleteNode:
		a.doNodeAlloc(ctx, ev)
	case api.EventCreateTask, api.EventUpdateTask, api.EventDeleteTask:
		a.doTaskAlloc(ctx, ev)
	case state.EventCommit:
		a.procTasksNetwork(ctx, false)

		if time.Since(nc.lastRetry) > retryInterval || nc.somethingWasDeallocated {
			a.procUnallocatedNetworks(ctx)
			a.procUnallocatedServices(ctx)
			a.procTasksNetwork(ctx, true)
			nc.lastRetry = time.Now()
			nc.somethingWasDeallocated = false
		}

		// Any left over tasks are moved to the unallocated set
		for _, t := range nc.pendingTasks {
			nc.unallocatedTasks[t.ID] = t
		}
		nc.pendingTasks = make(map[string]*api.Task)
	}
}

func (a *Allocator) doNodeAlloc(ctx context.Context, ev events.Event) {
	var (
		isDelete bool
		node     *api.Node
	)

	// We may have already allocated this node. If a create or update
	// event is older than the current version in the store, we run the
	// risk of allocating the node a second time. Only operate on the
	// latest version of the node.
	switch v := ev.(type) {
	case api.EventCreateNode:
		a.store.View(func(tx store.ReadTx) {
			node = store.GetNode(tx, v.Node.ID)
		})
	case api.EventUpdateNode:
		a.store.View(func(tx store.ReadTx) {
			node = store.GetNode(tx, v.Node.ID)
		})
	case api.EventDeleteNode:
		isDelete = true
		node = v.Node.Copy()
	}

	if node == nil {
		return
	}

	nc := a.netCtx

	if isDelete {
		if err := a.deallocateNode(node); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed freeing network resources for node %s", node.ID)
		} else {
			nc.somethingWasDeallocated = true
		}
	} else {
		// if this isn't a delete, we should try reallocating the node. if this
		// is a creation, then the node will be allocated only for ingress.
		if err := a.reallocateNode(ctx, node.ID); err != nil {
			log.G(ctx).WithError(err).Errorf(
				"error reallocating network resources for node %v", node.ID,
			)
		}
	}
}

func isOverlayNetwork(n *api.Network) bool {
	if n.DriverState != nil && n.DriverState.Name == "overlay" {
		return true
	}

	if n.Spec.DriverConfig != nil && n.Spec.DriverConfig.Name == "overlay" {
		return true
	}

	return false
}

func (a *Allocator) getAllocatedNetworks() ([]*api.Network, error) {
	var (
		err               error
		nc                = a.netCtx
		na                = nc.nwkAllocator
		allocatedNetworks []*api.Network
	)

	// Find allocated networks
	var networks []*api.Network
	a.store.View(func(tx store.ReadTx) {
		networks, err = store.FindNetworks(tx, store.All)
	})

	if err != nil {
		return nil, errors.Wrap(err, "error listing all networks in store while trying to allocate during init")
	}

	for _, n := range networks {

		if isOverlayNetwork(n) && na.IsAllocated(n) {
			allocatedNetworks = append(allocatedNetworks, n)
		}
	}

	return allocatedNetworks, nil
}

// getNodeNetworks returns all networks that should be allocated for a node
func (a *Allocator) getNodeNetworks(nodeID string) ([]*api.Network, error) {
	var (
		// no need to initialize networks. we only append to it, and appending
		// to a nil slice is valid. this has the added bonus of making this nil
		// if we return an error
		networks []*api.Network
		err      error
	)
	a.store.View(func(tx store.ReadTx) {
		// get all tasks currently assigned to this node. it's no big deal if
		// the tasks change in the meantime, there's no race to clean up
		// unneeded network attachments on a node.
		var tasks []*api.Task
		tasks, err = store.FindTasks(tx, store.ByNodeID(nodeID))
		if err != nil {
			return
		}
		// we need to keep track of network IDs that we've already added to the
		// list of networks we're going to return. we could do
		// map[string]*api.Network and then convert to []*api.Network and
		// return that, but it seems cleaner to have a separate set and list.
		networkIDs := map[string]struct{}{}
		for _, task := range tasks {
			// we don't need to check if a task is before the Assigned state.
			// the only way we have a task with a NodeID that isn't yet in
			// Assigned is if it's a global service task. this check is not
			// necessary:
			// if task.Status.State < api.TaskStateAssigned {
			//     continue
			// }
			if task.Status.State > api.TaskStateRunning {
				// we don't need to have network attachments for a task that's
				// already in a terminal state
				continue
			}

			// now go through the task's network attachments and find all of
			// the networks
			for _, attachment := range task.Networks {
				// if the network is an overlay network, and the network ID is
				// not yet in the set of network IDs, then add it to the set
				// and add the network to the list of networks we'll be
				// returning
				if _, ok := networkIDs[attachment.Network.ID]; isOverlayNetwork(attachment.Network) && !ok {
					networkIDs[attachment.Network.ID] = struct{}{}
					// we don't need to worry about retrieving the network from
					// the store, because the network in the attachment is an
					// identical copy of the network in the store.
					networks = append(networks, attachment.Network)
				}
			}
		}
	})

	// finally, we need the ingress network if one exists.
	if a.netCtx != nil && a.netCtx.ingressNetwork != nil {
		networks = append(networks, a.netCtx.ingressNetwork)
	}

	return networks, err
}

func (a *Allocator) allocateNodes(ctx context.Context, existingAddressesOnly bool) error {
	// Allocate nodes in the store so far before we process watched events.
	var (
		allocatedNodes []*api.Node
		nodes          []*api.Node
		err            error
	)

	a.store.View(func(tx store.ReadTx) {
		nodes, err = store.FindNodes(tx, store.All)
	})
	if err != nil {
		return errors.Wrap(err, "error listing all nodes in store while trying to allocate network resources")
	}

	for _, node := range nodes {
		networks, err := a.getNodeNetworks(node.ID)
		if err != nil {
			return errors.Wrap(err, "error getting all networks needed by node")
		}
		isAllocated := a.allocateNode(ctx, node, existingAddressesOnly, networks)
		if isAllocated {
			allocatedNodes = append(allocatedNodes, node)
		}
	}

	if err := a.store.Batch(func(batch *store.Batch) error {
		for _, node := range allocatedNodes {
			if err := a.commitAllocatedNode(ctx, batch, node); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to commit allocation of network resources for node %s", node.ID)
			}
		}
		return nil
	}); err != nil {
		log.G(ctx).WithError(err).Error("Failed to commit allocation of network resources for nodes")
	}

	return nil
}

func (a *Allocator) deallocateNodes(ctx context.Context) error {
	var (
		nodes []*api.Node
		nc    = a.netCtx
		err   error
	)

	a.store.View(func(tx store.ReadTx) {
		nodes, err = store.FindNodes(tx, store.All)
	})
	if err != nil {
		return fmt.Errorf("error listing all nodes in store while trying to free network resources")
	}

	for _, node := range nodes {
		if err := a.deallocateNode(node); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed freeing network resources for node %s", node.ID)
		} else {
			nc.somethingWasDeallocated = true
		}
		if err := a.store.Batch(func(batch *store.Batch) error {
			return a.commitAllocatedNode(ctx, batch, node)
		}); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to commit deallocation of network resources for node %s", node.ID)
		}
	}

	return nil
}

func (a *Allocator) deallocateNodeAttachments(ctx context.Context, nid string) error {
	var (
		nodes []*api.Node
		nc    = a.netCtx
		err   error
	)

	a.store.View(func(tx store.ReadTx) {
		nodes, err = store.FindNodes(tx, store.All)
	})
	if err != nil {
		return fmt.Errorf("error listing all nodes in store while trying to free network resources")
	}

	for _, node := range nodes {

		var networkAttachment *api.NetworkAttachment
		var naIndex int
		for index, na := range node.Attachments {
			if na.Network.ID == nid {
				networkAttachment = na
				naIndex = index
				break
			}
		}

		if networkAttachment == nil {
			log.G(ctx).Errorf("Failed to find network %s on node %s", nid, node.ID)
			continue
		}

		if nc.nwkAllocator.IsAttachmentAllocated(node, networkAttachment) {
			if err := nc.nwkAllocator.DeallocateAttachment(node, networkAttachment); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to commit deallocation of network resources for node %s", node.ID)
			} else {

				// Delete the lbattachment
				node.Attachments[naIndex] = node.Attachments[len(node.Attachments)-1]
				node.Attachments[len(node.Attachments)-1] = nil
				node.Attachments = node.Attachments[:len(node.Attachments)-1]

				if err := a.store.Batch(func(batch *store.Batch) error {
					return a.commitAllocatedNode(ctx, batch, node)
				}); err != nil {
					log.G(ctx).WithError(err).Errorf("Failed to commit deallocation of network resources for node %s", node.ID)
				}

			}
		}

	}
	return nil
}

func (a *Allocator) deallocateNode(node *api.Node) error {
	var (
		nc = a.netCtx
	)

	for _, na := range node.Attachments {
		if nc.nwkAllocator.IsAttachmentAllocated(node, na) {
			if err := nc.nwkAllocator.DeallocateAttachment(node, na); err != nil {
				return err
			}
		}
	}

	node.Attachments = nil

	return nil
}

// allocateNetworks allocates (restores) networks in the store so far before we process
// watched events. existingOnly flags is set to true to specify if only allocated
// networks need to be restored.
func (a *Allocator) allocateNetworks(ctx context.Context, existingOnly bool) error {
	var (
		nc       = a.netCtx
		networks []*api.Network
		err      error
	)
	a.store.View(func(tx store.ReadTx) {
		networks, err = store.FindNetworks(tx, store.All)
	})
	if err != nil {
		return errors.Wrap(err, "error listing all networks in store while trying to allocate during init")
	}

	var allocatedNetworks []*api.Network
	for _, n := range networks {
		if nc.nwkAllocator.IsAllocated(n) {
			continue
		}
		// Network is considered allocated only if the DriverState and IPAM are NOT nil.
		// During initial restore (existingOnly being true), check the network state in
		// raft store. If it is allocated, then restore the same in the in memory allocator
		// state. If it is not allocated, then skip allocating the network at this step.
		// This is to avoid allocating  an in-use network IP, subnet pool or vxlan id to
		// another network.
		if existingOnly &&
			(n.DriverState == nil ||
				n.IPAM == nil) {
			continue
		}

		if err := a.allocateNetwork(ctx, n); err != nil {
			log.G(ctx).WithField("existingOnly", existingOnly).WithError(err).Errorf("failed allocating network %s during init", n.ID)
			continue
		}
		allocatedNetworks = append(allocatedNetworks, n)
	}

	if err := a.store.Batch(func(batch *store.Batch) error {
		for _, n := range allocatedNetworks {
			if err := a.commitAllocatedNetwork(ctx, batch, n); err != nil {
				log.G(ctx).WithError(err).Errorf("failed committing allocation of network %s during init", n.ID)
			}
		}
		return nil
	}); err != nil {
		log.G(ctx).WithError(err).Error("failed committing allocation of networks during init")
	}

	return nil
}

// allocateServices allocates services in the store so far before we process
// watched events.
func (a *Allocator) allocateServices(ctx context.Context, existingAddressesOnly bool) error {
	var (
		nc       = a.netCtx
		services []*api.Service
		err      error
	)
	a.store.View(func(tx store.ReadTx) {
		services, err = store.FindServices(tx, store.All)
	})
	if err != nil {
		return errors.Wrap(err, "error listing all services in store while trying to allocate during init")
	}

	var allocatedServices []*api.Service
	for _, s := range services {
		if nc.nwkAllocator.IsServiceAllocated(s, networkallocator.OnInit) {
			continue
		}
		if existingAddressesOnly &&
			(s.Endpoint == nil ||
				len(s.Endpoint.VirtualIPs) == 0) {
			continue
		}

		if err := a.allocateService(ctx, s, existingAddressesOnly); err != nil {
			log.G(ctx).WithField("existingAddressesOnly", existingAddressesOnly).WithError(err).Errorf("failed allocating service %s during init", s.ID)
			continue
		}
		allocatedServices = append(allocatedServices, s)
	}

	if err := a.store.Batch(func(batch *store.Batch) error {
		for _, s := range allocatedServices {
			if err := a.commitAllocatedService(ctx, batch, s); err != nil {
				log.G(ctx).WithError(err).Errorf("failed committing allocation of service %s during init", s.ID)
			}
		}
		return nil
	}); err != nil {
		for _, s := range allocatedServices {
			log.G(ctx).WithError(err).Errorf("failed committing allocation of service %v during init", s.GetID())
		}
	}

	return nil
}

// allocateTasks allocates tasks in the store so far before we started watching.
func (a *Allocator) allocateTasks(ctx context.Context, existingAddressesOnly bool) error {
	var (
		nc             = a.netCtx
		tasks          []*api.Task
		allocatedTasks []*api.Task
		err            error
	)
	a.store.View(func(tx store.ReadTx) {
		tasks, err = store.FindTasks(tx, store.All)
	})
	if err != nil {
		return errors.Wrap(err, "error listing all tasks in store while trying to allocate during init")
	}

	logger := log.G(ctx).WithField("method", "(*Allocator).allocateTasks")

	for _, t := range tasks {
		if t.Status.State > api.TaskStateRunning {
			logger.Debugf("task %v is in allocated state: %v", t.GetID(), t.Status.State)
			continue
		}

		if existingAddressesOnly {
			hasAddresses := false
			for _, nAttach := range t.Networks {
				if len(nAttach.Addresses) != 0 {
					hasAddresses = true
					break
				}
			}
			if !hasAddresses {
				logger.Debugf("task %v has no attached addresses", t.GetID())
				continue
			}
		}

		var s *api.Service
		if t.ServiceID != "" {
			a.store.View(func(tx store.ReadTx) {
				s = store.GetService(tx, t.ServiceID)
			})
		}

		// Populate network attachments in the task
		// based on service spec.
		a.taskCreateNetworkAttachments(t, s)

		if taskReadyForNetworkVote(t, s, nc) {
			if t.Status.State >= api.TaskStatePending {
				logger.Debugf("task %v is in allocated state: %v", t.GetID(), t.Status.State)
				continue
			}

			if a.taskAllocateVote(networkVoter, t.ID) {
				// If the task is not attached to any network, network
				// allocators job is done. Immediately cast a vote so
				// that the task can be moved to the PENDING state as
				// soon as possible.
				updateTaskStatus(t, api.TaskStatePending, allocatedStatusMessage)
				allocatedTasks = append(allocatedTasks, t)
				logger.Debugf("allocated task %v, state update %v", t.GetID(), api.TaskStatePending)
			}
			continue
		}

		err := a.allocateTask(ctx, t)
		if err == nil {
			allocatedTasks = append(allocatedTasks, t)
		} else if err != errNoChanges {
			logger.WithError(err).Errorf("failed allocating task %s during init", t.ID)
			nc.unallocatedTasks[t.ID] = t
		}
	}

	if err := a.store.Batch(func(batch *store.Batch) error {
		for _, t := range allocatedTasks {
			if err := a.commitAllocatedTask(ctx, batch, t); err != nil {
				logger.WithError(err).Errorf("failed committing allocation of task %s during init", t.ID)
			}
		}

		return nil
	}); err != nil {
		for _, t := range allocatedTasks {
			logger.WithError(err).Errorf("failed committing allocation of task %v during init", t.GetID())
		}
	}

	return nil
}

// taskReadyForNetworkVote checks if the task is ready for a network
// vote to move it to PENDING state.
func taskReadyForNetworkVote(t *api.Task, s *api.Service, nc *networkContext) bool {
	// Task is ready for vote if the following is true:
	//
	// Task has no network attached or networks attached but all
	// of them allocated AND Task's service has no endpoint or
	// network configured or service endpoints have been
	// allocated.
	return (len(t.Networks) == 0 || nc.nwkAllocator.IsTaskAllocated(t)) &&
		(s == nil || nc.nwkAllocator.IsServiceAllocated(s))
}

func taskUpdateNetworks(t *api.Task, networks []*api.NetworkAttachment) {
	networksCopy := make([]*api.NetworkAttachment, 0, len(networks))
	for _, n := range networks {
		networksCopy = append(networksCopy, n.Copy())
	}

	t.Networks = networksCopy
}

func taskUpdateEndpoint(t *api.Task, endpoint *api.Endpoint) {
	t.Endpoint = endpoint.Copy()
}

// IsIngressNetworkNeeded checks whether the service requires the routing-mesh
func IsIngressNetworkNeeded(s *api.Service) bool {
	return networkallocator.IsIngressNetworkNeeded(s)
}

func (a *Allocator) taskCreateNetworkAttachments(t *api.Task, s *api.Service) {
	// If task network attachments have already been filled in no
	// need to do anything else.
	if len(t.Networks) != 0 {
		return
	}

	var networks []*api.NetworkAttachment
	if IsIngressNetworkNeeded(s) && a.netCtx.ingressNetwork != nil {
		networks = append(networks, &api.NetworkAttachment{Network: a.netCtx.ingressNetwork})
	}

	a.store.View(func(tx store.ReadTx) {
		// Always prefer NetworkAttachmentConfig in the TaskSpec
		specNetworks := t.Spec.Networks
		if len(specNetworks) == 0 && s != nil && len(s.Spec.Networks) != 0 {
			specNetworks = s.Spec.Networks
		}

		for _, na := range specNetworks {
			n := store.GetNetwork(tx, na.Target)
			if n == nil {
				continue
			}

			attachment := api.NetworkAttachment{Network: n}
			attachment.Aliases = append(attachment.Aliases, na.Aliases...)
			attachment.Addresses = append(attachment.Addresses, na.Addresses...)
			attachment.DriverAttachmentOpts = na.DriverAttachmentOpts
			networks = append(networks, &attachment)
		}
	})

	taskUpdateNetworks(t, networks)
}

func (a *Allocator) doTaskAlloc(ctx context.Context, ev events.Event) {
	var (
		isDelete bool
		t        *api.Task
	)

	logger := log.G(ctx).WithField("method", "(*Allocator).doTaskAlloc")

	// We may have already allocated this task. If a create or update
	// event is older than the current version in the store, we run the
	// risk of allocating the task a second time. Only operate on the
	// latest version of the task.
	switch v := ev.(type) {
	case api.EventCreateTask:
		a.store.View(func(tx store.ReadTx) {
			t = store.GetTask(tx, v.Task.ID)
		})
	case api.EventUpdateTask:
		a.store.View(func(tx store.ReadTx) {
			t = store.GetTask(tx, v.Task.ID)
		})
	case api.EventDeleteTask:
		isDelete = true
		t = v.Task.Copy()
	}

	if t == nil {
		return
	}

	nc := a.netCtx

	// If the task has stopped running then we should free the network
	// resources associated with the task right away.
	if t.Status.State > api.TaskStateRunning || isDelete {
		if nc.nwkAllocator.IsTaskAllocated(t) {
			if err := nc.nwkAllocator.DeallocateTask(t); err != nil {
				logger.WithError(err).Errorf("Failed freeing network resources for task %s", t.ID)
			} else {
				nc.somethingWasDeallocated = true
			}
		}

		// if we're deallocating the task, we also might need to deallocate the
		// node's network attachment, if this is the last task on the node that
		// needs it. we can do that by doing the same dance to reallocate a
		// node
		if err := a.reallocateNode(ctx, t.NodeID); err != nil {
			logger.WithError(err).Errorf("error reallocating node %v", t.NodeID)
		}

		// Cleanup any task references that might exist
		delete(nc.pendingTasks, t.ID)
		delete(nc.unallocatedTasks, t.ID)

		return
	}

	// if the task has a node ID, we should allocate an attachment for the node
	// this happens if the task is in any non-terminal state.
	if t.NodeID != "" && t.Status.State <= api.TaskStateRunning {
		if err := a.reallocateNode(ctx, t.NodeID); err != nil {
			// TODO(dperny): not entire sure what the error handling flow here
			// should be... for now, just log and keep going
			logger.WithError(err).Errorf("error reallocating node %v", t.NodeID)
		}
	}

	// If we are already in allocated state, there is
	// absolutely nothing else to do.
	if t.Status.State >= api.TaskStatePending {
		logger.Debugf("Task %s is already in allocated state %v", t.ID, t.Status.State)
		delete(nc.pendingTasks, t.ID)
		delete(nc.unallocatedTasks, t.ID)
		return
	}

	var s *api.Service
	if t.ServiceID != "" {
		a.store.View(func(tx store.ReadTx) {
			s = store.GetService(tx, t.ServiceID)
		})
		if s == nil {
			// If the task is running it is not normal to
			// not be able to find the associated
			// service. If the task is not running (task
			// is either dead or the desired state is set
			// to dead) then the service may not be
			// available in store. But we still need to
			// cleanup network resources associated with
			// the task.
			if t.Status.State <= api.TaskStateRunning && !isDelete {
				log.G(ctx).Errorf("Event %T: Failed to get service %s for task %s state %s: could not find service %s", ev, t.ServiceID, t.ID, t.Status.State, t.ServiceID)
				return
			}
		}
	}

	// Populate network attachments in the task
	// based on service spec.
	a.taskCreateNetworkAttachments(t, s)

	nc.pendingTasks[t.ID] = t
	log.G(ctx).Debugf("task %v was marked pending allocation", t.ID)
}

// allocateNode takes a context, a node, whether or not new allocations should
// be made, and the networks to allocate. it then makes sure an attachment is
// allocated for every network in the provided networks, allocating new
// attachments if existingAddressesOnly is false. it return true if something
// new was allocated or something was removed, or false otherwise.
//
// additionally, allocateNode will remove and free any attachments for networks
// not in the set of networks passed in.
func (a *Allocator) allocateNode(ctx context.Context, node *api.Node, existingAddressesOnly bool, networks []*api.Network) bool {
	var allocated bool

	nc := a.netCtx

	var nwIDs = make(map[string]struct{}, len(networks))

	// go through all of the networks we've passed in
	for _, network := range networks {
		nwIDs[network.ID] = struct{}{}

		// for each one, create space for an attachment. then, search through
		// all of the attachments already on the node. if the attachment
		// exists, then copy it to the node. if not, we'll allocate it below.
		var lbAttachment *api.NetworkAttachment
		for _, na := range node.Attachments {
			if na.Network != nil && na.Network.ID == network.ID {
				lbAttachment = na
				break
			}
		}

		if lbAttachment != nil {
			if nc.nwkAllocator.IsAttachmentAllocated(node, lbAttachment) {
				continue
			}
		}

		if lbAttachment == nil {
			// if we're restoring state, we should not add an attachment here.
			if existingAddressesOnly {
				continue
			}
			lbAttachment = &api.NetworkAttachment{}
			node.Attachments = append(node.Attachments, lbAttachment)
		}

		if existingAddressesOnly && len(lbAttachment.Addresses) == 0 {
			continue
		}

		lbAttachment.Network = network.Copy()
		if err := a.netCtx.nwkAllocator.AllocateAttachment(node, lbAttachment); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to allocate network resources for node %s", node.ID)
			// TODO: Should we add a unallocatedNode and retry allocating resources like we do for network, tasks, services?
			// right now, we will only retry allocating network resources for the node when the node is updated.
			continue
		}

		allocated = true
	}

	// if we're only initializing existing addresses, we should stop here and
	// not deallocate anything
	if existingAddressesOnly {
		return allocated
	}

	// now that we've allocated everything new, we have to remove things that
	// do not belong. we have to do this last because we can easily roll back
	// attachments we've allocated if something goes wrong by freeing them, but
	// we can't roll back deallocating attachments by reacquiring them.

	// we're using a trick to filter without allocating see the official go
	// wiki on github:
	// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
	attachments := node.Attachments[:0]
	for _, attach := range node.Attachments {
		if _, ok := nwIDs[attach.Network.ID]; ok {
			// attachment belongs to one of the networks, so keep it
			attachments = append(attachments, attach)
		} else {
			// free the attachment and remove it from the node's attachments by
			// re-slicing
			if err := a.netCtx.nwkAllocator.DeallocateAttachment(node, attach); err != nil {
				// if deallocation fails, there's nothing we can do besides log
				// an error and keep going
				log.G(ctx).WithError(err).Errorf(
					"error deallocating attachment for network %v on node %v",
					attach.Network.ID, node.ID,
				)
			}
			// strictly speaking, nothing was allocated, but something was
			// deallocated and that counts.
			allocated = true
			// also, set the somethingWasDeallocated flag so the allocator
			// knows that it can now try again.
			a.netCtx.somethingWasDeallocated = true
		}
	}
	node.Attachments = attachments

	return allocated
}

func (a *Allocator) reallocateNode(ctx context.Context, nodeID string) error {
	var (
		node *api.Node
	)
	a.store.View(func(tx store.ReadTx) {
		node = store.GetNode(tx, nodeID)
	})
	if node == nil {
		return errors.Errorf("node %v cannot be found", nodeID)
	}

	networks, err := a.getNodeNetworks(node.ID)
	if err != nil {
		return errors.Wrapf(err, "error getting networks for node %v", nodeID)
	}
	if a.allocateNode(ctx, node, false, networks) {
		// if something was allocated, commit the node
		if err := a.store.Batch(func(batch *store.Batch) error {
			return a.commitAllocatedNode(ctx, batch, node)
		}); err != nil {
			return errors.Wrapf(err, "error committing allocation for node %v", nodeID)
		}
	}
	return nil
}

func (a *Allocator) commitAllocatedNode(ctx context.Context, batch *store.Batch, node *api.Node) error {
	if err := batch.Update(func(tx store.Tx) error {
		err := store.UpdateNode(tx, node)

		if err == store.ErrSequenceConflict {
			storeNode := store.GetNode(tx, node.ID)
			storeNode.Attachments = node.Attachments
			err = store.UpdateNode(tx, storeNode)
		}

		return errors.Wrapf(err, "failed updating state in store transaction for node %s", node.ID)
	}); err != nil {
		if err := a.deallocateNode(node); err != nil {
			log.G(ctx).WithError(err).Errorf("failed rolling back allocation of node %s", node.ID)
		}

		return err
	}

	return nil
}

// This function prepares the service object for being updated when the change regards
// the published ports in host mode: It resets the runtime state ports (s.Endpoint.Ports)
// to the current ingress mode runtime state ports plus the newly configured publish mode ports,
// so that the service allocation invoked on this new service object will trigger the deallocation
// of any old publish mode port and allocation of any new one.
func updatePortsInHostPublishMode(s *api.Service) {
	// First, remove all host-mode ports from s.Endpoint.Ports
	if s.Endpoint != nil {
		var portConfigs []*api.PortConfig
		for _, portConfig := range s.Endpoint.Ports {
			if portConfig.PublishMode != api.PublishModeHost {
				portConfigs = append(portConfigs, portConfig)
			}
		}
		s.Endpoint.Ports = portConfigs
	}

	// Add back all host-mode ports
	if s.Spec.Endpoint != nil {
		if s.Endpoint == nil {
			s.Endpoint = &api.Endpoint{}
		}
		for _, portConfig := range s.Spec.Endpoint.Ports {
			if portConfig.PublishMode == api.PublishModeHost {
				s.Endpoint.Ports = append(s.Endpoint.Ports, portConfig.Copy())
			}
		}
	}
	s.Endpoint.Spec = s.Spec.Endpoint.Copy()
}

// allocateService takes care to align the desired state with the spec passed
// the last parameter is true only during restart when the data is read from raft
// and used to build internal state
func (a *Allocator) allocateService(ctx context.Context, s *api.Service, existingAddressesOnly bool) error {
	nc := a.netCtx

	if s.Spec.Endpoint != nil {
		// service has user-defined endpoint
		if s.Endpoint == nil {
			// service currently has no allocated endpoint, need allocated.
			s.Endpoint = &api.Endpoint{
				Spec: s.Spec.Endpoint.Copy(),
			}
		}

		// The service is trying to expose ports to the external
		// world. Automatically attach the service to the ingress
		// network only if it is not already done.
		if IsIngressNetworkNeeded(s) {
			if nc.ingressNetwork == nil {
				return fmt.Errorf("ingress network is missing")
			}
			var found bool
			for _, vip := range s.Endpoint.VirtualIPs {
				if vip.NetworkID == nc.ingressNetwork.ID {
					found = true
					break
				}
			}

			if !found {
				s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs,
					&api.Endpoint_VirtualIP{NetworkID: nc.ingressNetwork.ID})
			}
		}
	} else if s.Endpoint != nil && !existingAddressesOnly {
		// if we are in the restart phase there is no reason to try to deallocate anything because the state
		// is not there
		// service has no user-defined endpoints while has already allocated network resources,
		// need deallocated.
		if err := nc.nwkAllocator.DeallocateService(s); err != nil {
			return err
		}
		nc.somethingWasDeallocated = true
	}

	if err := nc.nwkAllocator.AllocateService(s); err != nil {
		nc.unallocatedServices[s.ID] = s
		return err
	}

	// If the service doesn't expose ports any more and if we have
	// any lingering virtual IP references for ingress network
	// clean them up here.
	if !IsIngressNetworkNeeded(s) && nc.ingressNetwork != nil {
		if s.Endpoint != nil {
			for i, vip := range s.Endpoint.VirtualIPs {
				if vip.NetworkID == nc.ingressNetwork.ID {
					n := len(s.Endpoint.VirtualIPs)
					s.Endpoint.VirtualIPs[i], s.Endpoint.VirtualIPs[n-1] = s.Endpoint.VirtualIPs[n-1], nil
					s.Endpoint.VirtualIPs = s.Endpoint.VirtualIPs[:n-1]
					break
				}
			}
		}
	}
	return nil
}

func (a *Allocator) commitAllocatedService(ctx context.Context, batch *store.Batch, s *api.Service) error {
	if err := batch.Update(func(tx store.Tx) error {
		err := store.UpdateService(tx, s)

		if err == store.ErrSequenceConflict {
			storeService := store.GetService(tx, s.ID)
			storeService.Endpoint = s.Endpoint
			err = store.UpdateService(tx, storeService)
		}

		return errors.Wrapf(err, "failed updating state in store transaction for service %s", s.ID)
	}); err != nil {
		if err := a.netCtx.nwkAllocator.DeallocateService(s); err != nil {
			log.G(ctx).WithError(err).Errorf("failed rolling back allocation of service %s", s.ID)
		}

		return err
	}

	return nil
}

func (a *Allocator) allocateNetwork(ctx context.Context, n *api.Network) error {
	nc := a.netCtx

	if err := nc.nwkAllocator.Allocate(n); err != nil {
		nc.unallocatedNetworks[n.ID] = n
		return err
	}

	return nil
}

func (a *Allocator) commitAllocatedNetwork(ctx context.Context, batch *store.Batch, n *api.Network) error {
	if err := batch.Update(func(tx store.Tx) error {
		if err := store.UpdateNetwork(tx, n); err != nil {
			return errors.Wrapf(err, "failed updating state in store transaction for network %s", n.ID)
		}
		return nil
	}); err != nil {
		if err := a.netCtx.nwkAllocator.Deallocate(n); err != nil {
			log.G(ctx).WithError(err).Errorf("failed rolling back allocation of network %s", n.ID)
		}

		return err
	}

	return nil
}

func (a *Allocator) allocateTask(ctx context.Context, t *api.Task) (err error) {
	taskUpdated := false
	nc := a.netCtx

	logger := log.G(ctx).WithField("method", "(*Allocator).allocateTask")

	// We might be here even if a task allocation has already
	// happened but wasn't successfully committed to store. In such
	// cases skip allocation and go straight ahead to updating the
	// store.
	if !nc.nwkAllocator.IsTaskAllocated(t) {
		a.store.View(func(tx store.ReadTx) {
			if t.ServiceID != "" {
				s := store.GetService(tx, t.ServiceID)
				if s == nil {
					err = fmt.Errorf("could not find service %s for task %s", t.ServiceID, t.GetID())
					return
				}

				if !nc.nwkAllocator.IsServiceAllocated(s) {
					err = fmt.Errorf("service %s to which task %s belongs has pending allocations", s.ID, t.ID)
					return
				}

				if s.Endpoint != nil {
					taskUpdateEndpoint(t, s.Endpoint)
					taskUpdated = true
				}
			}

			for _, na := range t.Networks {
				n := store.GetNetwork(tx, na.Network.ID)
				if n == nil {
					err = fmt.Errorf("failed to retrieve network %s while allocating task %s", na.Network.ID, t.ID)
					return
				}

				if !nc.nwkAllocator.IsAllocated(n) {
					err = fmt.Errorf("network %s attached to task %s not allocated yet", n.ID, t.ID)
					return
				}

				na.Network = n
			}

			if err = nc.nwkAllocator.AllocateTask(t); err != nil {
				return
			}
			if nc.nwkAllocator.IsTaskAllocated(t) {
				taskUpdated = true
			}
		})

		if err != nil {
			return err
		}
	}

	// Update the network allocations and moving to
	// PENDING state on top of the latest store state.
	if a.taskAllocateVote(networkVoter, t.ID) {
		if t.Status.State < api.TaskStatePending {
			updateTaskStatus(t, api.TaskStatePending, allocatedStatusMessage)
			logger.Debugf("allocated task %v, state update %v", t.GetID(), api.TaskStatePending)
			taskUpdated = true
		} else {
			logger.Debugf("task %v, already in allocated state %v", t.GetID(), t.Status.State)
		}
	}

	if !taskUpdated {
		return errNoChanges
	}

	return nil
}

func (a *Allocator) commitAllocatedTask(ctx context.Context, batch *store.Batch, t *api.Task) error {
	retError := batch.Update(func(tx store.Tx) error {
		err := store.UpdateTask(tx, t)

		if err == store.ErrSequenceConflict {
			storeTask := store.GetTask(tx, t.ID)
			taskUpdateNetworks(storeTask, t.Networks)
			taskUpdateEndpoint(storeTask, t.Endpoint)
			if storeTask.Status.State < api.TaskStatePending {
				storeTask.Status = t.Status
			}
			err = store.UpdateTask(tx, storeTask)
		}

		return errors.Wrapf(err, "failed updating state in store transaction for task %s", t.ID)
	})

	if retError == nil {
		log.G(ctx).Debugf("committed allocated task %v, state update %v", t.GetID(), t.Status)
	}

	return retError
}

func (a *Allocator) procUnallocatedNetworks(ctx context.Context) {
	nc := a.netCtx
	var allocatedNetworks []*api.Network
	for _, n := range nc.unallocatedNetworks {
		if !nc.nwkAllocator.IsAllocated(n) {
			if err := a.allocateNetwork(ctx, n); err != nil {
				log.G(ctx).WithError(err).Debugf("Failed allocation of unallocated network %s", n.ID)
				continue
			}
			allocatedNetworks = append(allocatedNetworks, n)
		}
	}

	if len(allocatedNetworks) == 0 {
		return
	}

	err := a.store.Batch(func(batch *store.Batch) error {
		for _, n := range allocatedNetworks {
			if err := a.commitAllocatedNetwork(ctx, batch, n); err != nil {
				log.G(ctx).WithError(err).Debugf("Failed to commit allocation of unallocated network %s", n.ID)
				continue
			}
			delete(nc.unallocatedNetworks, n.ID)
		}
		return nil
	})

	if err != nil {
		log.G(ctx).WithError(err).Error("Failed to commit allocation of unallocated networks")
		// We optimistically removed these from nc.unallocatedNetworks
		// above in anticipation of successfully committing the batch,
		// but since the transaction has failed, we requeue them here.
		for _, n := range allocatedNetworks {
			nc.unallocatedNetworks[n.ID] = n
		}
	}
}

func (a *Allocator) procUnallocatedServices(ctx context.Context) {
	nc := a.netCtx
	var allocatedServices []*api.Service
	for _, s := range nc.unallocatedServices {
		if !nc.nwkAllocator.IsServiceAllocated(s) {
			if err := a.allocateService(ctx, s, false); err != nil {
				log.G(ctx).WithError(err).Debugf("Failed allocation of unallocated service %s", s.ID)
				continue
			}
			allocatedServices = append(allocatedServices, s)
		}
	}

	if len(allocatedServices) == 0 {
		return
	}

	err := a.store.Batch(func(batch *store.Batch) error {
		for _, s := range allocatedServices {
			if err := a.commitAllocatedService(ctx, batch, s); err != nil {
				log.G(ctx).WithError(err).Debugf("Failed to commit allocation of unallocated service %s", s.ID)
				continue
			}
			delete(nc.unallocatedServices, s.ID)
		}
		return nil
	})

	if err != nil {
		log.G(ctx).WithError(err).Error("Failed to commit allocation of unallocated services")
		// We optimistically removed these from nc.unallocatedServices
		// above in anticipation of successfully committing the batch,
		// but since the transaction has failed, we requeue them here.
		for _, s := range allocatedServices {
			nc.unallocatedServices[s.ID] = s
		}
	}
}

func (a *Allocator) procTasksNetwork(ctx context.Context, onRetry bool) {
	nc := a.netCtx
	quiet := false
	toAllocate := nc.pendingTasks
	if onRetry {
		toAllocate = nc.unallocatedTasks
		quiet = true
	}
	allocatedTasks := make([]*api.Task, 0, len(toAllocate))

	for _, t := range toAllocate {

		if err := a.allocateTask(ctx, t); err == nil {
			allocatedTasks = append(allocatedTasks, t)
		} else if err != errNoChanges {
			if quiet {
				log.G(ctx).WithError(err).Debug("task allocation failure")
			} else {
				log.G(ctx).WithError(err).Error("task allocation failure")
			}
		}
	}

	if len(allocatedTasks) == 0 {
		return
	}

	err := a.store.Batch(func(batch *store.Batch) error {
		for _, t := range allocatedTasks {
			err := a.commitAllocatedTask(ctx, batch, t)
			if err != nil {
				log.G(ctx).WithField("method", "(*Allocator).procTasksNetwork").WithError(err).Errorf("allocation commit failure for task %s", t.GetID())
				continue
			}
			delete(toAllocate, t.ID)
		}

		return nil
	})

	if err != nil {
		log.G(ctx).WithError(err).Error("failed a store batch operation while processing tasks")
		// We optimistically removed these from toAllocate above in
		// anticipation of successfully committing the batch, but since
		// the transaction has failed, we requeue them here.
		for _, t := range allocatedTasks {
			toAllocate[t.ID] = t
		}
	}
}

// IsBuiltInNetworkDriver returns whether the passed driver is an internal network driver
func IsBuiltInNetworkDriver(name string) bool {
	return cnmallocator.IsBuiltInDriver(name)
}

// PredefinedNetworks returns the list of predefined network structures for a given network model
func PredefinedNetworks() []networkallocator.PredefinedNetworkData {
	return cnmallocator.PredefinedNetworks()
}

// updateTaskStatus sets TaskStatus and updates timestamp.
func updateTaskStatus(t *api.Task, newStatus api.TaskState, message string) {
	t.Status = api.TaskStatus{
		State:     newStatus,
		Message:   message,
		Timestamp: ptypes.MustTimestampProto(time.Now()),
	}
}

// IsIngressNetwork returns whether the passed network is an ingress network.
func IsIngressNetwork(nw *api.Network) bool {
	return networkallocator.IsIngressNetwork(nw)
}

// GetIngressNetwork fetches the ingress network from store.
// ErrNoIngress will be returned if the ingress network is not present,
// nil otherwise. In case of any other failure in accessing the store,
// the respective error will be reported as is.
func GetIngressNetwork(s *store.MemoryStore) (*api.Network, error) {
	var (
		networks []*api.Network
		err      error
	)
	s.View(func(tx store.ReadTx) {
		networks, err = store.FindNetworks(tx, store.All)
	})
	if err != nil {
		return nil, err
	}
	for _, n := range networks {
		if IsIngressNetwork(n) {
			return n, nil
		}
	}
	return nil, ErrNoIngress
}

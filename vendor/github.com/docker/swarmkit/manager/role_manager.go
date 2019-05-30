package manager

import (
	"context"
	"time"

	"code.cloudfoundry.org/clock"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/state/raft"
	"github.com/docker/swarmkit/manager/state/raft/membership"
	"github.com/docker/swarmkit/manager/state/store"
)

const (
	// roleReconcileInterval is how often to retry removing a node, if a reconciliation or
	// removal failed
	roleReconcileInterval = 5 * time.Second

	// removalTimeout is how long to wait before a raft member removal fails to be applied
	// to the store
	removalTimeout = 5 * time.Second
)

// roleManager reconciles the raft member list with desired role changes.
type roleManager struct {
	ctx    context.Context
	cancel func()

	store    *store.MemoryStore
	raft     *raft.Node
	doneChan chan struct{}

	// pendingReconciliation contains changed nodes that have not yet been reconciled in
	// the raft member list.
	pendingReconciliation map[string]*api.Node

	// pendingRemoval contains the IDs of nodes that have been deleted - if these correspond
	// to members in the raft cluster, those members need to be removed from raft
	pendingRemoval map[string]struct{}

	// leave this nil except for tests which need to inject a fake time source
	clocksource clock.Clock
}

// newRoleManager creates a new roleManager.
func newRoleManager(store *store.MemoryStore, raftNode *raft.Node) *roleManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &roleManager{
		ctx:                   ctx,
		cancel:                cancel,
		store:                 store,
		raft:                  raftNode,
		doneChan:              make(chan struct{}),
		pendingReconciliation: make(map[string]*api.Node),
		pendingRemoval:        make(map[string]struct{}),
	}
}

// getTicker returns a ticker based on the configured clock source
func (rm *roleManager) getTicker(interval time.Duration) clock.Ticker {
	if rm.clocksource == nil {
		return clock.NewClock().NewTicker(interval)
	}
	return rm.clocksource.NewTicker(interval)

}

// Run is roleManager's main loop.  On startup, it looks at every node object in the cluster and
// attempts to reconcile the raft member list with all the nodes' desired roles.  If any nodes
// need to be demoted or promoted, it will add them to a reconciliation queue, and if any raft
// members' node have been deleted, it will add them to a removal queue.

// These queues are processed immediately, and any nodes that failed to be processed are
// processed again in the next reconciliation interval, so that nodes will hopefully eventually
// be reconciled.  As node updates come in, any promotions or demotions are also added to the
// reconciliation queue and reconciled.  As node removals come in, they are added to the removal
// queue to be removed from the raft cluster.

// Removal from a raft cluster is idempotent (and it's the only raft cluster change that will occur
// during reconciliation or removal), so it's fine if a node is in both the removal and reconciliation
// queues.

// The ctx param is only used for logging.
func (rm *roleManager) Run(ctx context.Context) {
	defer close(rm.doneChan)

	var (
		nodes []*api.Node

		// ticker and tickerCh are used to time the reconciliation interval, which will
		// periodically attempt to re-reconcile nodes that failed to reconcile the first
		// time through
		ticker   clock.Ticker
		tickerCh <-chan time.Time
	)

	watcher, cancelWatch, err := store.ViewAndWatch(rm.store,
		func(readTx store.ReadTx) error {
			var err error
			nodes, err = store.FindNodes(readTx, store.All)
			return err
		},
		api.EventUpdateNode{},
		api.EventDeleteNode{})
	defer cancelWatch()

	if err != nil {
		log.G(ctx).WithError(err).Error("failed to check nodes for role changes")
	} else {
		// Assume all raft members have been deleted from the cluster, until the node list
		// tells us otherwise.  We can make this assumption because the node object must
		// exist first before the raft member object.

		// Background life-cycle for a manager: it joins the cluster, getting a new TLS
		// certificate. To get a TLS certificate, it makes an RPC call to the CA server,
		// which on successful join adds its information to the cluster node list and
		// eventually generates a TLS certificate for it. Once it has a TLS certificate,
		// it can contact the other nodes, and makes an RPC call to request to join the
		// raft cluster.  The node it contacts will add the node to the raft membership.
		for _, member := range rm.raft.GetMemberlist() {
			rm.pendingRemoval[member.NodeID] = struct{}{}
		}
		for _, node := range nodes {
			// if the node exists, we don't want it removed from the raft membership cluster
			// necessarily
			delete(rm.pendingRemoval, node.ID)

			// reconcile each existing node
			rm.pendingReconciliation[node.ID] = node
			rm.reconcileRole(ctx, node)
		}
		for nodeID := range rm.pendingRemoval {
			rm.evictRemovedNode(ctx, nodeID)
		}
		// If any reconciliations or member removals failed, we want to try again, so
		// make sure that we start the ticker so we can try again and again every
		// roleReconciliationInterval seconds until the queues are both empty.
		if len(rm.pendingReconciliation) != 0 || len(rm.pendingRemoval) != 0 {
			ticker = rm.getTicker(roleReconcileInterval)
			tickerCh = ticker.C()
		}
	}

	for {
		select {
		case event := <-watcher:
			switch ev := event.(type) {
			case api.EventUpdateNode:
				rm.pendingReconciliation[ev.Node.ID] = ev.Node
				rm.reconcileRole(ctx, ev.Node)
			case api.EventDeleteNode:
				rm.pendingRemoval[ev.Node.ID] = struct{}{}
				rm.evictRemovedNode(ctx, ev.Node.ID)
			}
			// If any reconciliations or member removals failed, we want to try again, so
			// make sure that we start the ticker so we can try again and again every
			// roleReconciliationInterval seconds until the queues are both empty.
			if (len(rm.pendingReconciliation) != 0 || len(rm.pendingRemoval) != 0) && ticker == nil {
				ticker = rm.getTicker(roleReconcileInterval)
				tickerCh = ticker.C()
			}
		case <-tickerCh:
			for _, node := range rm.pendingReconciliation {
				rm.reconcileRole(ctx, node)
			}
			for nodeID := range rm.pendingRemoval {
				rm.evictRemovedNode(ctx, nodeID)
			}
			if len(rm.pendingReconciliation) == 0 && len(rm.pendingRemoval) == 0 {
				ticker.Stop()
				ticker = nil
				tickerCh = nil
			}
		case <-rm.ctx.Done():
			if ticker != nil {
				ticker.Stop()
			}
			return
		}
	}
}

// evictRemovedNode evicts a removed node from the raft cluster membership.  This is to cover an edge case in which
// a node might have been removed, but somehow the role was not reconciled (possibly a demotion and a removal happened
// in rapid succession before the raft membership configuration went through).
func (rm *roleManager) evictRemovedNode(ctx context.Context, nodeID string) {
	// Check if the member still exists in the membership
	member := rm.raft.GetMemberByNodeID(nodeID)
	if member != nil {
		// We first try to remove the raft node from the raft cluster.  On the next tick, if the node
		// has been removed from the cluster membership, we then delete it from the removed list
		rm.removeMember(ctx, member)
		return
	}
	delete(rm.pendingRemoval, nodeID)
}

// removeMember removes a member from the raft cluster membership
func (rm *roleManager) removeMember(ctx context.Context, member *membership.Member) {
	// Quorum safeguard - quorum should have been checked before a node was allowed to be demoted, but if in the
	// intervening time some other node disconnected, removing this node would result in a loss of cluster quorum.
	// We leave it
	if !rm.raft.CanRemoveMember(member.RaftID) {
		// TODO(aaronl): Retry later
		log.G(ctx).Debugf("can't demote node %s at this time: removing member from raft would result in a loss of quorum", member.NodeID)
		return
	}

	rmCtx, rmCancel := context.WithTimeout(rm.ctx, removalTimeout)
	defer rmCancel()

	if member.RaftID == rm.raft.Config.ID {
		// Don't use rmCtx, because we expect to lose
		// leadership, which will cancel this context.
		log.G(ctx).Info("demoted; transferring leadership")
		err := rm.raft.TransferLeadership(context.Background())
		if err == nil {
			return
		}
		log.G(ctx).WithError(err).Info("failed to transfer leadership")
	}
	if err := rm.raft.RemoveMember(rmCtx, member.RaftID); err != nil {
		// TODO(aaronl): Retry later
		log.G(ctx).WithError(err).Debugf("can't demote node %s at this time", member.NodeID)
	}
}

// reconcileRole looks at the desired role for a node, and if it is being demoted or promoted, updates the
// node role accordingly.   If the node is being demoted, it also removes the node from the raft cluster membership.
func (rm *roleManager) reconcileRole(ctx context.Context, node *api.Node) {
	if node.Role == node.Spec.DesiredRole {
		// Nothing to do.
		delete(rm.pendingReconciliation, node.ID)
		return
	}

	// Promotion can proceed right away.
	if node.Spec.DesiredRole == api.NodeRoleManager && node.Role == api.NodeRoleWorker {
		err := rm.store.Update(func(tx store.Tx) error {
			updatedNode := store.GetNode(tx, node.ID)
			if updatedNode == nil || updatedNode.Spec.DesiredRole != node.Spec.DesiredRole || updatedNode.Role != node.Role {
				return nil
			}
			updatedNode.Role = api.NodeRoleManager
			return store.UpdateNode(tx, updatedNode)
		})
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to promote node %s", node.ID)
		} else {
			delete(rm.pendingReconciliation, node.ID)
		}
	} else if node.Spec.DesiredRole == api.NodeRoleWorker && node.Role == api.NodeRoleManager {
		// Check for node in memberlist
		member := rm.raft.GetMemberByNodeID(node.ID)
		if member != nil {
			// We first try to remove the raft node from the raft cluster.  On the next tick, if the node
			// has been removed from the cluster membership, we then update the store to reflect the fact
			// that it has been successfully demoted, and if that works, remove it from the pending list.
			rm.removeMember(ctx, member)
			return
		}

		err := rm.store.Update(func(tx store.Tx) error {
			updatedNode := store.GetNode(tx, node.ID)
			if updatedNode == nil || updatedNode.Spec.DesiredRole != node.Spec.DesiredRole || updatedNode.Role != node.Role {
				return nil
			}
			updatedNode.Role = api.NodeRoleWorker

			return store.UpdateNode(tx, updatedNode)
		})
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to demote node %s", node.ID)
		} else {
			delete(rm.pendingReconciliation, node.ID)
		}
	}
}

// Stop stops the roleManager and waits for the main loop to exit.
func (rm *roleManager) Stop() {
	rm.cancel()
	<-rm.doneChan
}

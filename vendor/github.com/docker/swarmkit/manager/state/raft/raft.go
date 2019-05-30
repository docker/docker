package raft

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/clock"
	"github.com/coreos/etcd/pkg/idutil"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/go-events"
	"github.com/docker/go-metrics"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/ca"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/raftselector"
	"github.com/docker/swarmkit/manager/state"
	"github.com/docker/swarmkit/manager/state/raft/membership"
	"github.com/docker/swarmkit/manager/state/raft/storage"
	"github.com/docker/swarmkit/manager/state/raft/transport"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/watch"
	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	// ErrNoRaftMember is thrown when the node is not yet part of a raft cluster
	ErrNoRaftMember = errors.New("raft: node is not yet part of a raft cluster")
	// ErrConfChangeRefused is returned when there is an issue with the configuration change
	ErrConfChangeRefused = errors.New("raft: propose configuration change refused")
	// ErrApplyNotSpecified is returned during the creation of a raft node when no apply method was provided
	ErrApplyNotSpecified = errors.New("raft: apply method was not specified")
	// ErrSetHardState is returned when the node fails to set the hard state
	ErrSetHardState = errors.New("raft: failed to set the hard state for log append entry")
	// ErrStopped is returned when an operation was submitted but the node was stopped in the meantime
	ErrStopped = errors.New("raft: failed to process the request: node is stopped")
	// ErrLostLeadership is returned when an operation was submitted but the node lost leader status before it became committed
	ErrLostLeadership = errors.New("raft: failed to process the request: node lost leader status")
	// ErrRequestTooLarge is returned when a raft internal message is too large to be sent
	ErrRequestTooLarge = errors.New("raft: raft message is too large and can't be sent")
	// ErrCannotRemoveMember is thrown when we try to remove a member from the cluster but this would result in a loss of quorum
	ErrCannotRemoveMember = errors.New("raft: member cannot be removed, because removing it may result in loss of quorum")
	// ErrNoClusterLeader is thrown when the cluster has no elected leader
	ErrNoClusterLeader = errors.New("raft: no elected cluster leader")
	// ErrMemberUnknown is sent in response to a message from an
	// unrecognized peer.
	ErrMemberUnknown = errors.New("raft: member unknown")

	// work around lint
	lostQuorumMessage = "The swarm does not have a leader. It's possible that too few managers are online. Make sure more than half of the managers are online."
	errLostQuorum     = errors.New(lostQuorumMessage)

	// Timer to capture ProposeValue() latency.
	proposeLatencyTimer metrics.Timer
)

// LeadershipState indicates whether the node is a leader or follower.
type LeadershipState int

const (
	// IsLeader indicates that the node is a raft leader.
	IsLeader LeadershipState = iota
	// IsFollower indicates that the node is a raft follower.
	IsFollower

	// lostQuorumTimeout is the number of ticks that can elapse with no
	// leader before LeaderConn starts returning an error right away.
	lostQuorumTimeout = 10
)

// EncryptionKeys are the current and, if necessary, pending DEKs with which to
// encrypt raft data
type EncryptionKeys struct {
	CurrentDEK []byte
	PendingDEK []byte
}

// EncryptionKeyRotator is an interface to find out if any keys need rotating.
type EncryptionKeyRotator interface {
	GetKeys() EncryptionKeys
	UpdateKeys(EncryptionKeys) error
	NeedsRotation() bool
	RotationNotify() chan struct{}
}

// Node represents the Raft Node useful
// configuration.
type Node struct {
	raftNode  raft.Node
	cluster   *membership.Cluster
	transport *transport.Transport

	raftStore           *raft.MemoryStorage
	memoryStore         *store.MemoryStore
	Config              *raft.Config
	opts                NodeOptions
	reqIDGen            *idutil.Generator
	wait                *wait
	campaignWhenAble    bool
	signalledLeadership uint32
	isMember            uint32
	bootstrapMembers    []*api.RaftMember

	// waitProp waits for all the proposals to be terminated before
	// shutting down the node.
	waitProp sync.WaitGroup

	confState       raftpb.ConfState
	appliedIndex    uint64
	snapshotMeta    raftpb.SnapshotMetadata
	writtenWALIndex uint64

	ticker clock.Ticker
	doneCh chan struct{}
	// RemovedFromRaft notifies about node deletion from raft cluster
	RemovedFromRaft chan struct{}
	cancelFunc      func()
	// removeRaftCh notifies about node deletion from raft cluster
	removeRaftCh        chan struct{}
	removeRaftOnce      sync.Once
	leadershipBroadcast *watch.Queue

	// used to coordinate shutdown
	// Lock should be used only in stop(), all other functions should use RLock.
	stopMu sync.RWMutex
	// used for membership management checks
	membershipLock sync.Mutex
	// synchronizes access to n.opts.Addr, and makes sure the address is not
	// updated concurrently with JoinAndStart.
	addrLock sync.Mutex

	snapshotInProgress chan raftpb.SnapshotMetadata
	asyncTasks         sync.WaitGroup

	// stopped chan is used for notifying grpc handlers that raft node going
	// to stop.
	stopped chan struct{}

	raftLogger     *storage.EncryptedRaftLogger
	keyRotator     EncryptionKeyRotator
	rotationQueued bool
	clearData      bool

	// waitForAppliedIndex stores the index of the last log that was written using
	// an raft DEK during a raft DEK rotation, so that we won't finish a rotation until
	// a snapshot covering that index has been written encrypted with the new raft DEK
	waitForAppliedIndex uint64
	ticksWithNoLeader   uint32
}

// NodeOptions provides node-level options.
type NodeOptions struct {
	// ID is the node's ID, from its certificate's CN field.
	ID string
	// Addr is the address of this node's listener
	Addr string
	// ForceNewCluster defines if we have to force a new cluster
	// because we are recovering from a backup data directory.
	ForceNewCluster bool
	// JoinAddr is the cluster to join. May be an empty string to create
	// a standalone cluster.
	JoinAddr string
	// ForceJoin tells us to join even if already part of a cluster.
	ForceJoin bool
	// Config is the raft config.
	Config *raft.Config
	// StateDir is the directory to store durable state.
	StateDir string
	// TickInterval interval is the time interval between raft ticks.
	TickInterval time.Duration
	// ClockSource is a Clock interface to use as a time base.
	// Leave this nil except for tests that are designed not to run in real
	// time.
	ClockSource clock.Clock
	// SendTimeout is the timeout on the sending messages to other raft
	// nodes. Leave this as 0 to get the default value.
	SendTimeout    time.Duration
	TLSCredentials credentials.TransportCredentials
	KeyRotator     EncryptionKeyRotator
	// DisableStackDump prevents Run from dumping goroutine stacks when the
	// store becomes stuck.
	DisableStackDump bool

	// FIPS specifies whether the raft encryption should be FIPS compliant
	FIPS bool
}

func init() {
	rand.Seed(time.Now().UnixNano())
	ns := metrics.NewNamespace("swarm", "raft", nil)
	proposeLatencyTimer = ns.NewTimer("transaction_latency", "Raft transaction latency.")
	metrics.Register(ns)
}

// NewNode generates a new Raft node
func NewNode(opts NodeOptions) *Node {
	cfg := opts.Config
	if cfg == nil {
		cfg = DefaultNodeConfig()
	}
	if opts.TickInterval == 0 {
		opts.TickInterval = time.Second
	}
	if opts.SendTimeout == 0 {
		opts.SendTimeout = 2 * time.Second
	}

	raftStore := raft.NewMemoryStorage()

	n := &Node{
		cluster:   membership.NewCluster(),
		raftStore: raftStore,
		opts:      opts,
		Config: &raft.Config{
			ElectionTick:    cfg.ElectionTick,
			HeartbeatTick:   cfg.HeartbeatTick,
			Storage:         raftStore,
			MaxSizePerMsg:   cfg.MaxSizePerMsg,
			MaxInflightMsgs: cfg.MaxInflightMsgs,
			Logger:          cfg.Logger,
			CheckQuorum:     cfg.CheckQuorum,
		},
		doneCh:              make(chan struct{}),
		RemovedFromRaft:     make(chan struct{}),
		stopped:             make(chan struct{}),
		leadershipBroadcast: watch.NewQueue(),
		keyRotator:          opts.KeyRotator,
	}
	n.memoryStore = store.NewMemoryStore(n)

	if opts.ClockSource == nil {
		n.ticker = clock.NewClock().NewTicker(opts.TickInterval)
	} else {
		n.ticker = opts.ClockSource.NewTicker(opts.TickInterval)
	}

	n.reqIDGen = idutil.NewGenerator(uint16(n.Config.ID), time.Now())
	n.wait = newWait()

	n.cancelFunc = func(n *Node) func() {
		var cancelOnce sync.Once
		return func() {
			cancelOnce.Do(func() {
				close(n.stopped)
			})
		}
	}(n)

	return n
}

// IsIDRemoved reports if member with id was removed from cluster.
// Part of transport.Raft interface.
func (n *Node) IsIDRemoved(id uint64) bool {
	return n.cluster.IsIDRemoved(id)
}

// NodeRemoved signals that node was removed from cluster and should stop.
// Part of transport.Raft interface.
func (n *Node) NodeRemoved() {
	n.removeRaftOnce.Do(func() {
		atomic.StoreUint32(&n.isMember, 0)
		close(n.RemovedFromRaft)
	})
}

// ReportSnapshot reports snapshot status to underlying raft node.
// Part of transport.Raft interface.
func (n *Node) ReportSnapshot(id uint64, status raft.SnapshotStatus) {
	n.raftNode.ReportSnapshot(id, status)
}

// ReportUnreachable reports to underlying raft node that member with id is
// unreachable.
// Part of transport.Raft interface.
func (n *Node) ReportUnreachable(id uint64) {
	n.raftNode.ReportUnreachable(id)
}

// SetAddr provides the raft node's address. This can be used in cases where
// opts.Addr was not provided to NewNode, for example when a port was not bound
// until after the raft node was created.
func (n *Node) SetAddr(ctx context.Context, addr string) error {
	n.addrLock.Lock()
	defer n.addrLock.Unlock()

	n.opts.Addr = addr

	if !n.IsMember() {
		return nil
	}

	newRaftMember := &api.RaftMember{
		RaftID: n.Config.ID,
		NodeID: n.opts.ID,
		Addr:   addr,
	}
	if err := n.cluster.UpdateMember(n.Config.ID, newRaftMember); err != nil {
		return err
	}

	// If the raft node is running, submit a configuration change
	// with the new address.

	// TODO(aaronl): Currently, this node must be the leader to
	// submit this configuration change. This works for the initial
	// use cases (single-node cluster late binding ports, or calling
	// SetAddr before joining a cluster). In the future, we may want
	// to support having a follower proactively change its remote
	// address.

	leadershipCh, cancelWatch := n.SubscribeLeadership()
	defer cancelWatch()

	ctx, cancelCtx := n.WithContext(ctx)
	defer cancelCtx()

	isLeader := atomic.LoadUint32(&n.signalledLeadership) == 1
	for !isLeader {
		select {
		case leadershipChange := <-leadershipCh:
			if leadershipChange == IsLeader {
				isLeader = true
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return n.updateNodeBlocking(ctx, n.Config.ID, addr)
}

// WithContext returns context which is cancelled when parent context cancelled
// or node is stopped.
func (n *Node) WithContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		select {
		case <-ctx.Done():
		case <-n.stopped:
			cancel()
		}
	}()
	return ctx, cancel
}

func (n *Node) initTransport() {
	transportConfig := &transport.Config{
		HeartbeatInterval: time.Duration(n.Config.ElectionTick) * n.opts.TickInterval,
		SendTimeout:       n.opts.SendTimeout,
		Credentials:       n.opts.TLSCredentials,
		Raft:              n,
	}
	n.transport = transport.New(transportConfig)
}

// JoinAndStart joins and starts the raft server
func (n *Node) JoinAndStart(ctx context.Context) (err error) {
	ctx, cancel := n.WithContext(ctx)
	defer func() {
		cancel()
		if err != nil {
			n.stopMu.Lock()
			// to shutdown transport
			n.cancelFunc()
			n.stopMu.Unlock()
			n.done()
		} else {
			atomic.StoreUint32(&n.isMember, 1)
		}
	}()

	loadAndStartErr := n.loadAndStart(ctx, n.opts.ForceNewCluster)
	if loadAndStartErr != nil && loadAndStartErr != storage.ErrNoWAL {
		return loadAndStartErr
	}

	snapshot, err := n.raftStore.Snapshot()
	// Snapshot never returns an error
	if err != nil {
		panic("could not get snapshot of raft store")
	}

	n.confState = snapshot.Metadata.ConfState
	n.appliedIndex = snapshot.Metadata.Index
	n.snapshotMeta = snapshot.Metadata
	n.writtenWALIndex, _ = n.raftStore.LastIndex() // lastIndex always returns nil as an error

	n.addrLock.Lock()
	defer n.addrLock.Unlock()

	// override the module field entirely, since etcd/raft is not exactly a submodule
	n.Config.Logger = log.G(ctx).WithField("module", "raft")

	// restore from snapshot
	if loadAndStartErr == nil {
		if n.opts.JoinAddr != "" && n.opts.ForceJoin {
			if err := n.joinCluster(ctx); err != nil {
				return errors.Wrap(err, "failed to rejoin cluster")
			}
		}
		n.campaignWhenAble = true
		n.initTransport()
		n.raftNode = raft.RestartNode(n.Config)
		return nil
	}

	if n.opts.JoinAddr == "" {
		// First member in the cluster, self-assign ID
		n.Config.ID = uint64(rand.Int63()) + 1
		peer, err := n.newRaftLogs(n.opts.ID)
		if err != nil {
			return err
		}
		n.campaignWhenAble = true
		n.initTransport()
		n.raftNode = raft.StartNode(n.Config, []raft.Peer{peer})
		return nil
	}

	// join to existing cluster

	if err := n.joinCluster(ctx); err != nil {
		return err
	}

	if _, err := n.newRaftLogs(n.opts.ID); err != nil {
		return err
	}

	n.initTransport()
	n.raftNode = raft.StartNode(n.Config, nil)

	return nil
}

func (n *Node) joinCluster(ctx context.Context) error {
	if n.opts.Addr == "" {
		return errors.New("attempted to join raft cluster without knowing own address")
	}

	conn, err := dial(n.opts.JoinAddr, "tcp", n.opts.TLSCredentials, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := api.NewRaftMembershipClient(conn)

	joinCtx, joinCancel := context.WithTimeout(ctx, n.reqTimeout())
	defer joinCancel()
	resp, err := client.Join(joinCtx, &api.JoinRequest{
		Addr: n.opts.Addr,
	})
	if err != nil {
		return err
	}

	n.Config.ID = resp.RaftID
	n.bootstrapMembers = resp.Members
	return nil
}

// DefaultNodeConfig returns the default config for a
// raft node that can be modified and customized
func DefaultNodeConfig() *raft.Config {
	return &raft.Config{
		HeartbeatTick: 1,
		// Recommended value in etcd/raft is 10 x (HeartbeatTick).
		// Lower values were seen to have caused instability because of
		// frequent leader elections when running on flakey networks.
		ElectionTick:    10,
		MaxSizePerMsg:   math.MaxUint16,
		MaxInflightMsgs: 256,
		Logger:          log.L,
		CheckQuorum:     true,
	}
}

// DefaultRaftConfig returns a default api.RaftConfig.
func DefaultRaftConfig() api.RaftConfig {
	return api.RaftConfig{
		KeepOldSnapshots:           0,
		SnapshotInterval:           10000,
		LogEntriesForSlowFollowers: 500,
		// Recommended value in etcd/raft is 10 x (HeartbeatTick).
		// Lower values were seen to have caused instability because of
		// frequent leader elections when running on flakey networks.
		HeartbeatTick: 1,
		ElectionTick:  10,
	}
}

// MemoryStore returns the memory store that is kept in sync with the raft log.
func (n *Node) MemoryStore() *store.MemoryStore {
	return n.memoryStore
}

func (n *Node) done() {
	n.cluster.Clear()

	n.ticker.Stop()
	n.leadershipBroadcast.Close()
	n.cluster.PeersBroadcast.Close()
	n.memoryStore.Close()
	if n.transport != nil {
		n.transport.Stop()
	}

	close(n.doneCh)
}

// ClearData tells the raft node to delete its WALs, snapshots, and keys on
// shutdown.
func (n *Node) ClearData() {
	n.clearData = true
}

// Run is the main loop for a Raft node, it goes along the state machine,
// acting on the messages received from other Raft nodes in the cluster.
//
// Before running the main loop, it first starts the raft node based on saved
// cluster state. If no saved state exists, it starts a single-node cluster.
func (n *Node) Run(ctx context.Context) error {
	ctx = log.WithLogger(ctx, logrus.WithField("raft_id", fmt.Sprintf("%x", n.Config.ID)))
	ctx, cancel := context.WithCancel(ctx)

	for _, node := range n.bootstrapMembers {
		if err := n.registerNode(node); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to register member %x", node.RaftID)
		}
	}

	defer func() {
		cancel()
		n.stop(ctx)
		if n.clearData {
			// Delete WAL and snapshots, since they are no longer
			// usable.
			if err := n.raftLogger.Clear(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to move wal after node removal")
			}
			// clear out the DEKs
			if err := n.keyRotator.UpdateKeys(EncryptionKeys{}); err != nil {
				log.G(ctx).WithError(err).Error("could not remove DEKs")
			}
		}
		n.done()
	}()

	// Flag that indicates if this manager node is *currently* the raft leader.
	wasLeader := false
	transferLeadershipLimit := rate.NewLimiter(rate.Every(time.Minute), 1)

	for {
		select {
		case <-n.ticker.C():
			n.raftNode.Tick()

			if n.leader() == raft.None {
				atomic.AddUint32(&n.ticksWithNoLeader, 1)
			} else {
				atomic.StoreUint32(&n.ticksWithNoLeader, 0)
			}
		case rd := <-n.raftNode.Ready():
			raftConfig := n.getCurrentRaftConfig()

			// Save entries to storage
			if err := n.saveToStorage(ctx, &raftConfig, rd.HardState, rd.Entries, rd.Snapshot); err != nil {
				return errors.Wrap(err, "failed to save entries to storage")
			}

			// If the memory store lock has been held for too long,
			// transferring leadership is an easy way to break out of it.
			if wasLeader &&
				(rd.SoftState == nil || rd.SoftState.RaftState == raft.StateLeader) &&
				n.memoryStore.Wedged() &&
				transferLeadershipLimit.Allow() {
				log.G(ctx).Error("Attempting to transfer leadership")
				if !n.opts.DisableStackDump {
					signal.DumpStacks("")
				}
				transferee, err := n.transport.LongestActive()
				if err != nil {
					log.G(ctx).WithError(err).Error("failed to get longest-active member")
				} else {
					log.G(ctx).Error("data store lock held too long - transferring leadership")
					n.raftNode.TransferLeadership(ctx, n.Config.ID, transferee)
				}
			}

			for _, msg := range rd.Messages {
				// Send raft messages to peers
				if err := n.transport.Send(msg); err != nil {
					log.G(ctx).WithError(err).Error("failed to send message to member")
				}
			}

			// Apply snapshot to memory store. The snapshot
			// was applied to the raft store in
			// saveToStorage.
			if !raft.IsEmptySnap(rd.Snapshot) {
				// Load the snapshot data into the store
				if err := n.restoreFromSnapshot(ctx, rd.Snapshot.Data); err != nil {
					log.G(ctx).WithError(err).Error("failed to restore cluster from snapshot")
				}
				n.appliedIndex = rd.Snapshot.Metadata.Index
				n.snapshotMeta = rd.Snapshot.Metadata
				n.confState = rd.Snapshot.Metadata.ConfState
			}

			// If we cease to be the leader, we must cancel any
			// proposals that are currently waiting for a quorum to
			// acknowledge them. It is still possible for these to
			// become committed, but if that happens we will apply
			// them as any follower would.

			// It is important that we cancel these proposals before
			// calling processCommitted, so processCommitted does
			// not deadlock.

			if rd.SoftState != nil {
				if wasLeader && rd.SoftState.RaftState != raft.StateLeader {
					wasLeader = false
					log.G(ctx).Error("soft state changed, node no longer a leader, resetting and cancelling all waits")

					if atomic.LoadUint32(&n.signalledLeadership) == 1 {
						atomic.StoreUint32(&n.signalledLeadership, 0)
						n.leadershipBroadcast.Publish(IsFollower)
					}

					// It is important that we set n.signalledLeadership to 0
					// before calling n.wait.cancelAll. When a new raft
					// request is registered, it checks n.signalledLeadership
					// afterwards, and cancels the registration if it is 0.
					// If cancelAll was called first, this call might run
					// before the new request registers, but
					// signalledLeadership would be set after the check.
					// Setting signalledLeadership before calling cancelAll
					// ensures that if a new request is registered during
					// this transition, it will either be cancelled by
					// cancelAll, or by its own check of signalledLeadership.
					n.wait.cancelAll()
				} else if !wasLeader && rd.SoftState.RaftState == raft.StateLeader {
					// Node just became a leader.
					wasLeader = true
				}
			}

			// Process committed entries
			for _, entry := range rd.CommittedEntries {
				if err := n.processCommitted(ctx, entry); err != nil {
					log.G(ctx).WithError(err).Error("failed to process committed entries")
				}
			}

			// in case the previous attempt to update the key failed
			n.maybeMarkRotationFinished(ctx)

			// Trigger a snapshot every once in awhile
			if n.snapshotInProgress == nil &&
				(n.needsSnapshot(ctx) || raftConfig.SnapshotInterval > 0 &&
					n.appliedIndex-n.snapshotMeta.Index >= raftConfig.SnapshotInterval) {
				n.triggerSnapshot(ctx, raftConfig)
			}

			if wasLeader && atomic.LoadUint32(&n.signalledLeadership) != 1 {
				// If all the entries in the log have become
				// committed, broadcast our leadership status.
				if n.caughtUp() {
					atomic.StoreUint32(&n.signalledLeadership, 1)
					n.leadershipBroadcast.Publish(IsLeader)
				}
			}

			// Advance the state machine
			n.raftNode.Advance()

			// On the first startup, or if we are the only
			// registered member after restoring from the state,
			// campaign to be the leader.
			if n.campaignWhenAble {
				members := n.cluster.Members()
				if len(members) >= 1 {
					n.campaignWhenAble = false
				}
				if len(members) == 1 && members[n.Config.ID] != nil {
					n.raftNode.Campaign(ctx)
				}
			}

		case snapshotMeta := <-n.snapshotInProgress:
			raftConfig := n.getCurrentRaftConfig()
			if snapshotMeta.Index > n.snapshotMeta.Index {
				n.snapshotMeta = snapshotMeta
				if err := n.raftLogger.GC(snapshotMeta.Index, snapshotMeta.Term, raftConfig.KeepOldSnapshots); err != nil {
					log.G(ctx).WithError(err).Error("failed to clean up old snapshots and WALs")
				}
			}
			n.snapshotInProgress = nil
			n.maybeMarkRotationFinished(ctx)
			if n.rotationQueued && n.needsSnapshot(ctx) {
				// there was a key rotation that took place before while the snapshot
				// was in progress - we have to take another snapshot and encrypt with the new key
				n.rotationQueued = false
				n.triggerSnapshot(ctx, raftConfig)
			}
		case <-n.keyRotator.RotationNotify():
			// There are 2 separate checks:  rotationQueued, and n.needsSnapshot().
			// We set rotationQueued so that when we are notified of a rotation, we try to
			// do a snapshot as soon as possible.  However, if there is an error while doing
			// the snapshot, we don't want to hammer the node attempting to do snapshots over
			// and over.  So if doing a snapshot fails, wait until the next entry comes in to
			// try again.
			switch {
			case n.snapshotInProgress != nil:
				n.rotationQueued = true
			case n.needsSnapshot(ctx):
				n.triggerSnapshot(ctx, n.getCurrentRaftConfig())
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (n *Node) restoreFromSnapshot(ctx context.Context, data []byte) error {
	snapCluster, err := n.clusterSnapshot(data)
	if err != nil {
		return err
	}

	oldMembers := n.cluster.Members()

	for _, member := range snapCluster.Members {
		delete(oldMembers, member.RaftID)
	}

	for _, removedMember := range snapCluster.Removed {
		n.cluster.RemoveMember(removedMember)
		n.transport.RemovePeer(removedMember)
		delete(oldMembers, removedMember)
	}

	for id, member := range oldMembers {
		n.cluster.ClearMember(id)
		if err := n.transport.RemovePeer(member.RaftID); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to remove peer %x from transport", member.RaftID)
		}
	}
	for _, node := range snapCluster.Members {
		if err := n.registerNode(&api.RaftMember{RaftID: node.RaftID, NodeID: node.NodeID, Addr: node.Addr}); err != nil {
			log.G(ctx).WithError(err).Error("failed to register node from snapshot")
		}
	}
	return nil
}

func (n *Node) needsSnapshot(ctx context.Context) bool {
	if n.waitForAppliedIndex == 0 && n.keyRotator.NeedsRotation() {
		keys := n.keyRotator.GetKeys()
		if keys.PendingDEK != nil {
			n.raftLogger.RotateEncryptionKey(keys.PendingDEK)
			// we want to wait for the last index written with the old DEK to be committed, else a snapshot taken
			// may have an index less than the index of a WAL written with an old DEK.  We want the next snapshot
			// written with the new key to supercede any WAL written with an old DEK.
			n.waitForAppliedIndex = n.writtenWALIndex
			// if there is already a snapshot at this index or higher, bump the wait index up to 1 higher than the current
			// snapshot index, because the rotation cannot be completed until the next snapshot
			if n.waitForAppliedIndex <= n.snapshotMeta.Index {
				n.waitForAppliedIndex = n.snapshotMeta.Index + 1
			}
			log.G(ctx).Debugf(
				"beginning raft DEK rotation - last indices written with the old key are (snapshot: %d, WAL: %d) - waiting for snapshot of index %d to be written before rotation can be completed", n.snapshotMeta.Index, n.writtenWALIndex, n.waitForAppliedIndex)
		}
	}

	result := n.waitForAppliedIndex > 0 && n.waitForAppliedIndex <= n.appliedIndex
	if result {
		log.G(ctx).Debugf(
			"a snapshot at index %d is needed in order to complete raft DEK rotation - a snapshot with index >= %d can now be triggered",
			n.waitForAppliedIndex, n.appliedIndex)
	}
	return result
}

func (n *Node) maybeMarkRotationFinished(ctx context.Context) {
	if n.waitForAppliedIndex > 0 && n.waitForAppliedIndex <= n.snapshotMeta.Index {
		// this means we tried to rotate - so finish the rotation
		if err := n.keyRotator.UpdateKeys(EncryptionKeys{CurrentDEK: n.raftLogger.EncryptionKey}); err != nil {
			log.G(ctx).WithError(err).Error("failed to update encryption keys after a successful rotation")
		} else {
			log.G(ctx).Debugf(
				"a snapshot with index %d is available, which completes the DEK rotation requiring a snapshot of at least index %d - throwing away DEK and older snapshots encrypted with the old key",
				n.snapshotMeta.Index, n.waitForAppliedIndex)
			n.waitForAppliedIndex = 0

			if err := n.raftLogger.GC(n.snapshotMeta.Index, n.snapshotMeta.Term, 0); err != nil {
				log.G(ctx).WithError(err).Error("failed to remove old snapshots and WALs that were written with the previous raft DEK")
			}
		}
	}
}

func (n *Node) getCurrentRaftConfig() api.RaftConfig {
	raftConfig := DefaultRaftConfig()
	n.memoryStore.View(func(readTx store.ReadTx) {
		clusters, err := store.FindClusters(readTx, store.ByName(store.DefaultClusterName))
		if err == nil && len(clusters) == 1 {
			raftConfig = clusters[0].Spec.Raft
		}
	})
	return raftConfig
}

// Cancel interrupts all ongoing proposals, and prevents new ones from
// starting. This is useful for the shutdown sequence because it allows
// the manager to shut down raft-dependent services that might otherwise
// block on shutdown if quorum isn't met. Then the raft node can be completely
// shut down once no more code is using it.
func (n *Node) Cancel() {
	n.cancelFunc()
}

// Done returns channel which is closed when raft node is fully stopped.
func (n *Node) Done() <-chan struct{} {
	return n.doneCh
}

func (n *Node) stop(ctx context.Context) {
	n.stopMu.Lock()
	defer n.stopMu.Unlock()

	n.Cancel()
	n.waitProp.Wait()
	n.asyncTasks.Wait()

	n.raftNode.Stop()
	n.ticker.Stop()
	n.raftLogger.Close(ctx)
	atomic.StoreUint32(&n.isMember, 0)
	// TODO(stevvooe): Handle ctx.Done()
}

// isLeader checks if we are the leader or not, without the protection of lock
func (n *Node) isLeader() bool {
	if !n.IsMember() {
		return false
	}

	if n.Status().Lead == n.Config.ID {
		return true
	}
	return false
}

// IsLeader checks if we are the leader or not, with the protection of lock
func (n *Node) IsLeader() bool {
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	return n.isLeader()
}

// leader returns the id of the leader, without the protection of lock and
// membership check, so it's caller task.
func (n *Node) leader() uint64 {
	return n.Status().Lead
}

// Leader returns the id of the leader, with the protection of lock
func (n *Node) Leader() (uint64, error) {
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if !n.IsMember() {
		return raft.None, ErrNoRaftMember
	}
	leader := n.leader()
	if leader == raft.None {
		return raft.None, ErrNoClusterLeader
	}

	return leader, nil
}

// ReadyForProposals returns true if the node has broadcasted a message
// saying that it has become the leader. This means it is ready to accept
// proposals.
func (n *Node) ReadyForProposals() bool {
	return atomic.LoadUint32(&n.signalledLeadership) == 1
}

func (n *Node) caughtUp() bool {
	// obnoxious function that always returns a nil error
	lastIndex, _ := n.raftStore.LastIndex()
	return n.appliedIndex >= lastIndex
}

// Join asks to a member of the raft to propose
// a configuration change and add us as a member thus
// beginning the log replication process. This method
// is called from an aspiring member to an existing member
func (n *Node) Join(ctx context.Context, req *api.JoinRequest) (*api.JoinResponse, error) {
	nodeInfo, err := ca.RemoteNode(ctx)
	if err != nil {
		return nil, err
	}

	fields := logrus.Fields{
		"node.id": nodeInfo.NodeID,
		"method":  "(*Node).Join",
		"raft_id": fmt.Sprintf("%x", n.Config.ID),
	}
	if nodeInfo.ForwardedBy != nil {
		fields["forwarder.id"] = nodeInfo.ForwardedBy.NodeID
	}
	log := log.G(ctx).WithFields(fields)
	log.Debug("")

	// can't stop the raft node while an async RPC is in progress
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	n.membershipLock.Lock()
	defer n.membershipLock.Unlock()

	if !n.IsMember() {
		return nil, status.Errorf(codes.FailedPrecondition, "%s", ErrNoRaftMember.Error())
	}

	if !n.isLeader() {
		return nil, status.Errorf(codes.FailedPrecondition, "%s", ErrLostLeadership.Error())
	}

	remoteAddr := req.Addr

	// If the joining node sent an address like 0.0.0.0:4242, automatically
	// determine its actual address based on the GRPC connection. This
	// avoids the need for a prospective member to know its own address.

	requestHost, requestPort, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid address %s in raft join request", remoteAddr)
	}

	requestIP := net.ParseIP(requestHost)
	if requestIP != nil && requestIP.IsUnspecified() {
		remoteHost, _, err := net.SplitHostPort(nodeInfo.RemoteAddr)
		if err != nil {
			return nil, err
		}
		remoteAddr = net.JoinHostPort(remoteHost, requestPort)
	}

	// We do not bother submitting a configuration change for the
	// new member if we can't contact it back using its address
	if err := n.checkHealth(ctx, remoteAddr, 5*time.Second); err != nil {
		return nil, err
	}

	// If the peer is already a member of the cluster, we will only update
	// its information, not add it as a new member. Adding it again would
	// cause the quorum to be computed incorrectly.
	for _, m := range n.cluster.Members() {
		if m.NodeID == nodeInfo.NodeID {
			if remoteAddr == m.Addr {
				return n.joinResponse(m.RaftID), nil
			}
			updatedRaftMember := &api.RaftMember{
				RaftID: m.RaftID,
				NodeID: m.NodeID,
				Addr:   remoteAddr,
			}
			if err := n.cluster.UpdateMember(m.RaftID, updatedRaftMember); err != nil {
				return nil, err
			}

			if err := n.updateNodeBlocking(ctx, m.RaftID, remoteAddr); err != nil {
				log.WithError(err).Error("failed to update node address")
				return nil, err
			}

			log.Info("updated node address")
			return n.joinResponse(m.RaftID), nil
		}
	}

	// Find a unique ID for the joining member.
	var raftID uint64
	for {
		raftID = uint64(rand.Int63()) + 1
		if n.cluster.GetMember(raftID) == nil && !n.cluster.IsIDRemoved(raftID) {
			break
		}
	}

	err = n.addMember(ctx, remoteAddr, raftID, nodeInfo.NodeID)
	if err != nil {
		log.WithError(err).Errorf("failed to add member %x", raftID)
		return nil, err
	}

	log.Debug("node joined")

	return n.joinResponse(raftID), nil
}

func (n *Node) joinResponse(raftID uint64) *api.JoinResponse {
	var nodes []*api.RaftMember
	for _, node := range n.cluster.Members() {
		nodes = append(nodes, &api.RaftMember{
			RaftID: node.RaftID,
			NodeID: node.NodeID,
			Addr:   node.Addr,
		})
	}

	return &api.JoinResponse{Members: nodes, RaftID: raftID}
}

// checkHealth tries to contact an aspiring member through its advertised address
// and checks if its raft server is running.
func (n *Node) checkHealth(ctx context.Context, addr string, timeout time.Duration) error {
	conn, err := dial(addr, "tcp", n.opts.TLSCredentials, timeout)
	if err != nil {
		return err
	}

	defer conn.Close()

	if timeout != 0 {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = tctx
	}

	healthClient := api.NewHealthClient(conn)
	resp, err := healthClient.Check(ctx, &api.HealthCheckRequest{Service: "Raft"})
	if err != nil {
		return errors.Wrap(err, "could not connect to prospective new cluster member using its advertised address")
	}
	if resp.Status != api.HealthCheckResponse_SERVING {
		return fmt.Errorf("health check returned status %s", resp.Status.String())
	}

	return nil
}

// addMember submits a configuration change to add a new member on the raft cluster.
func (n *Node) addMember(ctx context.Context, addr string, raftID uint64, nodeID string) error {
	node := api.RaftMember{
		RaftID: raftID,
		NodeID: nodeID,
		Addr:   addr,
	}

	meta, err := node.Marshal()
	if err != nil {
		return err
	}

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  raftID,
		Context: meta,
	}

	// Wait for a raft round to process the configuration change
	return n.configure(ctx, cc)
}

// updateNodeBlocking runs synchronous job to update node address in whole cluster.
func (n *Node) updateNodeBlocking(ctx context.Context, id uint64, addr string) error {
	m := n.cluster.GetMember(id)
	if m == nil {
		return errors.Errorf("member %x is not found for update", id)
	}
	node := api.RaftMember{
		RaftID: m.RaftID,
		NodeID: m.NodeID,
		Addr:   addr,
	}

	meta, err := node.Marshal()
	if err != nil {
		return err
	}

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeUpdateNode,
		NodeID:  id,
		Context: meta,
	}

	// Wait for a raft round to process the configuration change
	return n.configure(ctx, cc)
}

// UpdateNode submits a configuration change to change a member's address.
func (n *Node) UpdateNode(id uint64, addr string) {
	ctx, cancel := n.WithContext(context.Background())
	defer cancel()
	// spawn updating info in raft in background to unblock transport
	go func() {
		if err := n.updateNodeBlocking(ctx, id, addr); err != nil {
			log.G(ctx).WithFields(logrus.Fields{"raft_id": n.Config.ID, "update_id": id}).WithError(err).Error("failed to update member address in cluster")
		}
	}()
}

// Leave asks to a member of the raft to remove
// us from the raft cluster. This method is called
// from a member who is willing to leave its raft
// membership to an active member of the raft
func (n *Node) Leave(ctx context.Context, req *api.LeaveRequest) (*api.LeaveResponse, error) {
	if req.Node == nil {
		return nil, status.Errorf(codes.InvalidArgument, "no node information provided")
	}

	nodeInfo, err := ca.RemoteNode(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := n.WithContext(ctx)
	defer cancel()

	fields := logrus.Fields{
		"node.id": nodeInfo.NodeID,
		"method":  "(*Node).Leave",
		"raft_id": fmt.Sprintf("%x", n.Config.ID),
	}
	if nodeInfo.ForwardedBy != nil {
		fields["forwarder.id"] = nodeInfo.ForwardedBy.NodeID
	}
	log.G(ctx).WithFields(fields).Debug("")

	if err := n.removeMember(ctx, req.Node.RaftID); err != nil {
		return nil, err
	}

	return &api.LeaveResponse{}, nil
}

// CanRemoveMember checks if a member can be removed from
// the context of the current node.
func (n *Node) CanRemoveMember(id uint64) bool {
	members := n.cluster.Members()
	nreachable := 0 // reachable managers after removal

	for _, m := range members {
		if m.RaftID == id {
			continue
		}

		// Local node from where the remove is issued
		if m.RaftID == n.Config.ID {
			nreachable++
			continue
		}

		if n.transport.Active(m.RaftID) {
			nreachable++
		}
	}

	nquorum := (len(members)-1)/2 + 1

	return nreachable >= nquorum
}

func (n *Node) removeMember(ctx context.Context, id uint64) error {
	// can't stop the raft node while an async RPC is in progress
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if !n.IsMember() {
		return ErrNoRaftMember
	}

	if !n.isLeader() {
		return ErrLostLeadership
	}

	n.membershipLock.Lock()
	defer n.membershipLock.Unlock()
	if !n.CanRemoveMember(id) {
		return ErrCannotRemoveMember
	}

	cc := raftpb.ConfChange{
		ID:      id,
		Type:    raftpb.ConfChangeRemoveNode,
		NodeID:  id,
		Context: []byte(""),
	}
	return n.configure(ctx, cc)
}

// TransferLeadership attempts to transfer leadership to a different node,
// and wait for the transfer to happen.
func (n *Node) TransferLeadership(ctx context.Context) error {
	ctx, cancelTransfer := context.WithTimeout(ctx, n.reqTimeout())
	defer cancelTransfer()

	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if !n.IsMember() {
		return ErrNoRaftMember
	}

	if !n.isLeader() {
		return ErrLostLeadership
	}

	transferee, err := n.transport.LongestActive()
	if err != nil {
		return errors.Wrap(err, "failed to get longest-active member")
	}
	start := time.Now()
	n.raftNode.TransferLeadership(ctx, n.Config.ID, transferee)
	ticker := time.NewTicker(n.opts.TickInterval / 10)
	defer ticker.Stop()
	var leader uint64
	for {
		leader = n.leader()
		if leader != raft.None && leader != n.Config.ID {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	log.G(ctx).Infof("raft: transfer leadership %x -> %x finished in %v", n.Config.ID, leader, time.Since(start))
	return nil
}

// RemoveMember submits a configuration change to remove a member from the raft cluster
// after checking if the operation would not result in a loss of quorum.
func (n *Node) RemoveMember(ctx context.Context, id uint64) error {
	ctx, cancel := n.WithContext(ctx)
	defer cancel()
	return n.removeMember(ctx, id)
}

// processRaftMessageLogger is used to lazily create a logger for
// ProcessRaftMessage. Usually nothing will be logged, so it is useful to avoid
// formatting strings and allocating a logger when it won't be used.
func (n *Node) processRaftMessageLogger(ctx context.Context, msg *api.ProcessRaftMessageRequest) *logrus.Entry {
	fields := logrus.Fields{
		"method": "(*Node).ProcessRaftMessage",
	}

	if n.IsMember() {
		fields["raft_id"] = fmt.Sprintf("%x", n.Config.ID)
	}

	if msg != nil && msg.Message != nil {
		fields["from"] = fmt.Sprintf("%x", msg.Message.From)
	}

	return log.G(ctx).WithFields(fields)
}

func (n *Node) reportNewAddress(ctx context.Context, id uint64) error {
	// too early
	if !n.IsMember() {
		return nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	oldAddr, err := n.transport.PeerAddr(id)
	if err != nil {
		return err
	}
	if oldAddr == "" {
		// Don't know the address of the peer yet, so can't report an
		// update.
		return nil
	}
	newHost, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return err
	}
	_, officialPort, err := net.SplitHostPort(oldAddr)
	if err != nil {
		return err
	}
	newAddr := net.JoinHostPort(newHost, officialPort)
	return n.transport.UpdatePeerAddr(id, newAddr)
}

// StreamRaftMessage is the server endpoint for streaming Raft messages.
// It accepts a stream of raft messages to be processed on this raft member,
// returning a StreamRaftMessageResponse when processing of the streamed
// messages is complete.
// It is called from the Raft leader, which uses it to stream messages
// to this raft member.
// A single stream corresponds to a single raft message,
// which may be disassembled and streamed by the sender
// as individual messages. Therefore, each of the messages
// received by the stream will have the same raft message type and index.
// Currently, only messages of type raftpb.MsgSnap can be disassembled, sent
// and received on the stream.
func (n *Node) StreamRaftMessage(stream api.Raft_StreamRaftMessageServer) error {
	// recvdMsg is the current messasge received from the stream.
	// assembledMessage is where the data from recvdMsg is appended to.
	var recvdMsg, assembledMessage *api.StreamRaftMessageRequest
	var err error

	// First message index.
	var raftMsgIndex uint64

	for {
		recvdMsg, err = stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			log.G(stream.Context()).WithError(err).Error("error while reading from stream")
			return err
		}

		// Initialized the message to be used for assembling
		// the raft message.
		if assembledMessage == nil {
			// For all message types except raftpb.MsgSnap,
			// we don't expect more than a single message
			// on the stream so we'll get an EOF on the next Recv()
			// and go on to process the received message.
			assembledMessage = recvdMsg
			raftMsgIndex = recvdMsg.Message.Index
			continue
		}

		// Verify raft message index.
		if recvdMsg.Message.Index != raftMsgIndex {
			errMsg := fmt.Sprintf("Raft message chunk with index %d is different from the previously received raft message index %d",
				recvdMsg.Message.Index, raftMsgIndex)
			log.G(stream.Context()).Errorf(errMsg)
			return status.Errorf(codes.InvalidArgument, "%s", errMsg)
		}

		// Verify that multiple message received on a stream
		// can only be of type raftpb.MsgSnap.
		if recvdMsg.Message.Type != raftpb.MsgSnap {
			errMsg := fmt.Sprintf("Raft message chunk is not of type %d",
				raftpb.MsgSnap)
			log.G(stream.Context()).Errorf(errMsg)
			return status.Errorf(codes.InvalidArgument, "%s", errMsg)
		}

		// Append the received snapshot data.
		assembledMessage.Message.Snapshot.Data = append(assembledMessage.Message.Snapshot.Data, recvdMsg.Message.Snapshot.Data...)
	}

	// We should have the complete snapshot. Verify and process.
	if err == io.EOF {
		_, err = n.ProcessRaftMessage(stream.Context(), &api.ProcessRaftMessageRequest{Message: assembledMessage.Message})
		if err == nil {
			// Translate the response of ProcessRaftMessage() from
			// ProcessRaftMessageResponse to StreamRaftMessageResponse if needed.
			return stream.SendAndClose(&api.StreamRaftMessageResponse{})
		}
	}

	return err
}

// ProcessRaftMessage calls 'Step' which advances the
// raft state machine with the provided message on the
// receiving node
func (n *Node) ProcessRaftMessage(ctx context.Context, msg *api.ProcessRaftMessageRequest) (*api.ProcessRaftMessageResponse, error) {
	if msg == nil || msg.Message == nil {
		n.processRaftMessageLogger(ctx, msg).Debug("received empty message")
		return &api.ProcessRaftMessageResponse{}, nil
	}

	// Don't process the message if this comes from
	// a node in the remove set
	if n.cluster.IsIDRemoved(msg.Message.From) {
		n.processRaftMessageLogger(ctx, msg).Debug("received message from removed member")
		return nil, status.Errorf(codes.NotFound, "%s", membership.ErrMemberRemoved.Error())
	}

	ctx, cancel := n.WithContext(ctx)
	defer cancel()

	// TODO(aaronl): Address changes are temporarily disabled.
	// See https://github.com/docker/docker/issues/30455.
	// This should be reenabled in the future with additional
	// safeguards (perhaps storing multiple addresses per node).
	//if err := n.reportNewAddress(ctx, msg.Message.From); err != nil {
	//	log.G(ctx).WithError(err).Errorf("failed to report new address of %x to transport", msg.Message.From)
	//}

	// Reject vote requests from unreachable peers
	if msg.Message.Type == raftpb.MsgVote {
		member := n.cluster.GetMember(msg.Message.From)
		if member == nil {
			n.processRaftMessageLogger(ctx, msg).Debug("received message from unknown member")
			return &api.ProcessRaftMessageResponse{}, nil
		}

		if err := n.transport.HealthCheck(ctx, msg.Message.From); err != nil {
			n.processRaftMessageLogger(ctx, msg).WithError(err).Debug("member which sent vote request failed health check")
			return &api.ProcessRaftMessageResponse{}, nil
		}
	}

	if msg.Message.Type == raftpb.MsgProp {
		// We don't accept forwarded proposals. Our
		// current architecture depends on only the leader
		// making proposals, so in-flight proposals can be
		// guaranteed not to conflict.
		n.processRaftMessageLogger(ctx, msg).Debug("dropped forwarded proposal")
		return &api.ProcessRaftMessageResponse{}, nil
	}

	// can't stop the raft node while an async RPC is in progress
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if n.IsMember() {
		if msg.Message.To != n.Config.ID {
			n.processRaftMessageLogger(ctx, msg).Errorf("received message intended for raft_id %x", msg.Message.To)
			return &api.ProcessRaftMessageResponse{}, nil
		}

		if err := n.raftNode.Step(ctx, *msg.Message); err != nil {
			n.processRaftMessageLogger(ctx, msg).WithError(err).Debug("raft Step failed")
		}
	}

	return &api.ProcessRaftMessageResponse{}, nil
}

// ResolveAddress returns the address reaching for a given node ID.
func (n *Node) ResolveAddress(ctx context.Context, msg *api.ResolveAddressRequest) (*api.ResolveAddressResponse, error) {
	if !n.IsMember() {
		return nil, ErrNoRaftMember
	}

	nodeInfo, err := ca.RemoteNode(ctx)
	if err != nil {
		return nil, err
	}

	fields := logrus.Fields{
		"node.id": nodeInfo.NodeID,
		"method":  "(*Node).ResolveAddress",
		"raft_id": fmt.Sprintf("%x", n.Config.ID),
	}
	if nodeInfo.ForwardedBy != nil {
		fields["forwarder.id"] = nodeInfo.ForwardedBy.NodeID
	}
	log.G(ctx).WithFields(fields).Debug("")

	member := n.cluster.GetMember(msg.RaftID)
	if member == nil {
		return nil, status.Errorf(codes.NotFound, "member %x not found", msg.RaftID)
	}
	return &api.ResolveAddressResponse{Addr: member.Addr}, nil
}

func (n *Node) getLeaderConn() (*grpc.ClientConn, error) {
	leader, err := n.Leader()
	if err != nil {
		return nil, err
	}

	if leader == n.Config.ID {
		return nil, raftselector.ErrIsLeader
	}
	conn, err := n.transport.PeerConn(leader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get connection to leader")
	}
	return conn, nil
}

// LeaderConn returns current connection to cluster leader or raftselector.ErrIsLeader
// if current machine is leader.
func (n *Node) LeaderConn(ctx context.Context) (*grpc.ClientConn, error) {
	cc, err := n.getLeaderConn()
	if err == nil {
		return cc, nil
	}
	if err == raftselector.ErrIsLeader {
		return nil, err
	}
	if atomic.LoadUint32(&n.ticksWithNoLeader) > lostQuorumTimeout {
		return nil, errLostQuorum
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cc, err := n.getLeaderConn()
			if err == nil {
				return cc, nil
			}
			if err == raftselector.ErrIsLeader {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// registerNode registers a new node on the cluster memberlist
func (n *Node) registerNode(node *api.RaftMember) error {
	if n.cluster.IsIDRemoved(node.RaftID) {
		return nil
	}

	member := &membership.Member{}

	existingMember := n.cluster.GetMember(node.RaftID)
	if existingMember != nil {
		// Member already exists

		// If the address is different from what we thought it was,
		// update it. This can happen if we just joined a cluster
		// and are adding ourself now with the remotely-reachable
		// address.
		if existingMember.Addr != node.Addr {
			if node.RaftID != n.Config.ID {
				if err := n.transport.UpdatePeer(node.RaftID, node.Addr); err != nil {
					return err
				}
			}
			member.RaftMember = node
			n.cluster.AddMember(member)
		}

		return nil
	}

	// Avoid opening a connection to the local node
	if node.RaftID != n.Config.ID {
		if err := n.transport.AddPeer(node.RaftID, node.Addr); err != nil {
			return err
		}
	}

	member.RaftMember = node
	err := n.cluster.AddMember(member)
	if err != nil {
		if rerr := n.transport.RemovePeer(node.RaftID); rerr != nil {
			return errors.Wrapf(rerr, "failed to remove peer after error %v", err)
		}
		return err
	}

	return nil
}

// ProposeValue calls Propose on the underlying raft library(etcd/raft) and waits
// on the commit log action before returning a result
func (n *Node) ProposeValue(ctx context.Context, storeAction []api.StoreAction, cb func()) error {
	defer metrics.StartTimer(proposeLatencyTimer)()
	ctx, cancel := n.WithContext(ctx)
	defer cancel()
	_, err := n.processInternalRaftRequest(ctx, &api.InternalRaftRequest{Action: storeAction}, cb)

	return err
}

// GetVersion returns the sequence information for the current raft round.
func (n *Node) GetVersion() *api.Version {
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if !n.IsMember() {
		return nil
	}

	status := n.Status()
	return &api.Version{Index: status.Commit}
}

// ChangesBetween returns the changes starting after "from", up to and
// including "to". If these changes are not available because the log
// has been compacted, an error will be returned.
func (n *Node) ChangesBetween(from, to api.Version) ([]state.Change, error) {
	n.stopMu.RLock()
	defer n.stopMu.RUnlock()

	if from.Index > to.Index {
		return nil, errors.New("versions are out of order")
	}

	if !n.IsMember() {
		return nil, ErrNoRaftMember
	}

	// never returns error
	last, _ := n.raftStore.LastIndex()

	if to.Index > last {
		return nil, errors.New("last version is out of bounds")
	}

	pbs, err := n.raftStore.Entries(from.Index+1, to.Index+1, math.MaxUint64)
	if err != nil {
		return nil, err
	}

	var changes []state.Change
	for _, pb := range pbs {
		if pb.Type != raftpb.EntryNormal || pb.Data == nil {
			continue
		}
		r := &api.InternalRaftRequest{}
		err := proto.Unmarshal(pb.Data, r)
		if err != nil {
			return nil, errors.Wrap(err, "error umarshalling internal raft request")
		}

		if r.Action != nil {
			changes = append(changes, state.Change{StoreActions: r.Action, Version: api.Version{Index: pb.Index}})
		}
	}

	return changes, nil
}

// SubscribePeers subscribes to peer updates in cluster. It sends always full
// list of peers.
func (n *Node) SubscribePeers() (q chan events.Event, cancel func()) {
	return n.cluster.PeersBroadcast.Watch()
}

// GetMemberlist returns the current list of raft members in the cluster.
func (n *Node) GetMemberlist() map[uint64]*api.RaftMember {
	memberlist := make(map[uint64]*api.RaftMember)
	members := n.cluster.Members()
	leaderID, err := n.Leader()
	if err != nil {
		leaderID = raft.None
	}

	for id, member := range members {
		reachability := api.RaftMemberStatus_REACHABLE
		leader := false

		if member.RaftID != n.Config.ID {
			if !n.transport.Active(member.RaftID) {
				reachability = api.RaftMemberStatus_UNREACHABLE
			}
		}

		if member.RaftID == leaderID {
			leader = true
		}

		memberlist[id] = &api.RaftMember{
			RaftID: member.RaftID,
			NodeID: member.NodeID,
			Addr:   member.Addr,
			Status: api.RaftMemberStatus{
				Leader:       leader,
				Reachability: reachability,
			},
		}
	}

	return memberlist
}

// Status returns status of underlying etcd.Node.
func (n *Node) Status() raft.Status {
	return n.raftNode.Status()
}

// GetMemberByNodeID returns member information based
// on its generic Node ID.
func (n *Node) GetMemberByNodeID(nodeID string) *membership.Member {
	members := n.cluster.Members()
	for _, member := range members {
		if member.NodeID == nodeID {
			return member
		}
	}
	return nil
}

// GetNodeIDByRaftID returns the generic Node ID of a member given its raft ID.
// It returns ErrMemberUnknown if the raft ID is unknown.
func (n *Node) GetNodeIDByRaftID(raftID uint64) (string, error) {
	if member, ok := n.cluster.Members()[raftID]; ok {
		return member.NodeID, nil
	}
	// this is the only possible error value that should be returned; the
	// manager code depends on this. if you need to add more errors later, make
	// sure that you update the callers of this method accordingly
	return "", ErrMemberUnknown
}

// IsMember checks if the raft node has effectively joined
// a cluster of existing members.
func (n *Node) IsMember() bool {
	return atomic.LoadUint32(&n.isMember) == 1
}

// Saves a log entry to our Store
func (n *Node) saveToStorage(
	ctx context.Context,
	raftConfig *api.RaftConfig,
	hardState raftpb.HardState,
	entries []raftpb.Entry,
	snapshot raftpb.Snapshot,
) (err error) {

	if !raft.IsEmptySnap(snapshot) {
		if err := n.raftLogger.SaveSnapshot(snapshot); err != nil {
			return errors.Wrap(err, "failed to save snapshot")
		}
		if err := n.raftLogger.GC(snapshot.Metadata.Index, snapshot.Metadata.Term, raftConfig.KeepOldSnapshots); err != nil {
			log.G(ctx).WithError(err).Error("unable to clean old snapshots and WALs")
		}
		if err = n.raftStore.ApplySnapshot(snapshot); err != nil {
			return errors.Wrap(err, "failed to apply snapshot on raft node")
		}
	}

	if err := n.raftLogger.SaveEntries(hardState, entries); err != nil {
		return errors.Wrap(err, "failed to save raft log entries")
	}

	if len(entries) > 0 {
		lastIndex := entries[len(entries)-1].Index
		if lastIndex > n.writtenWALIndex {
			n.writtenWALIndex = lastIndex
		}
	}

	if err = n.raftStore.Append(entries); err != nil {
		return errors.Wrap(err, "failed to append raft log entries")
	}

	return nil
}

// processInternalRaftRequest proposes a value to be appended to the raft log.
// It calls Propose() on etcd/raft, which calls back into the raft FSM,
// which then sends a message to each of the participating nodes
// in the raft group to apply a log entry and then waits for it to be applied
// on this node. It will block until the this node:
// 1. Gets the necessary replies back from the participating nodes and also performs the commit itself, or
// 2. There is an error, or
// 3. Until the raft node finalizes all the proposals on node shutdown.
func (n *Node) processInternalRaftRequest(ctx context.Context, r *api.InternalRaftRequest, cb func()) (proto.Message, error) {
	n.stopMu.RLock()
	if !n.IsMember() {
		n.stopMu.RUnlock()
		return nil, ErrStopped
	}
	n.waitProp.Add(1)
	defer n.waitProp.Done()
	n.stopMu.RUnlock()

	r.ID = n.reqIDGen.Next()

	// This must be derived from the context which is cancelled by stop()
	// to avoid a deadlock on shutdown.
	waitCtx, cancel := context.WithCancel(ctx)

	ch := n.wait.register(r.ID, cb, cancel)

	// Do this check after calling register to avoid a race.
	if atomic.LoadUint32(&n.signalledLeadership) != 1 {
		log.G(ctx).Error("node is no longer leader, aborting propose")
		n.wait.cancel(r.ID)
		return nil, ErrLostLeadership
	}

	data, err := r.Marshal()
	if err != nil {
		n.wait.cancel(r.ID)
		return nil, err
	}

	if len(data) > store.MaxTransactionBytes {
		n.wait.cancel(r.ID)
		return nil, ErrRequestTooLarge
	}

	err = n.raftNode.Propose(waitCtx, data)
	if err != nil {
		n.wait.cancel(r.ID)
		return nil, err
	}

	select {
	case x, ok := <-ch:
		if !ok {
			// Wait notification channel was closed. This should only happen if the wait was cancelled.
			log.G(ctx).Error("wait cancelled")
			if atomic.LoadUint32(&n.signalledLeadership) == 1 {
				log.G(ctx).Error("wait cancelled but node is still a leader")
			}
			return nil, ErrLostLeadership
		}
		return x.(proto.Message), nil
	case <-waitCtx.Done():
		n.wait.cancel(r.ID)
		// If we can read from the channel, wait item was triggered. Otherwise it was cancelled.
		x, ok := <-ch
		if !ok {
			log.G(ctx).WithError(waitCtx.Err()).Error("wait context cancelled")
			if atomic.LoadUint32(&n.signalledLeadership) == 1 {
				log.G(ctx).Error("wait context cancelled but node is still a leader")
			}
			return nil, ErrLostLeadership
		}
		return x.(proto.Message), nil
	case <-ctx.Done():
		n.wait.cancel(r.ID)
		// if channel is closed, wait item was canceled, otherwise it was triggered
		x, ok := <-ch
		if !ok {
			return nil, ctx.Err()
		}
		return x.(proto.Message), nil
	}
}

// configure sends a configuration change through consensus and
// then waits for it to be applied to the server. It will block
// until the change is performed or there is an error.
func (n *Node) configure(ctx context.Context, cc raftpb.ConfChange) error {
	cc.ID = n.reqIDGen.Next()

	ctx, cancel := context.WithCancel(ctx)
	ch := n.wait.register(cc.ID, nil, cancel)

	if err := n.raftNode.ProposeConfChange(ctx, cc); err != nil {
		n.wait.cancel(cc.ID)
		return err
	}

	select {
	case x := <-ch:
		if err, ok := x.(error); ok {
			return err
		}
		if x != nil {
			log.G(ctx).Panic("raft: configuration change error, return type should always be error")
		}
		return nil
	case <-ctx.Done():
		n.wait.cancel(cc.ID)
		return ctx.Err()
	}
}

func (n *Node) processCommitted(ctx context.Context, entry raftpb.Entry) error {
	// Process a normal entry
	if entry.Type == raftpb.EntryNormal && entry.Data != nil {
		if err := n.processEntry(ctx, entry); err != nil {
			return err
		}
	}

	// Process a configuration change (add/remove node)
	if entry.Type == raftpb.EntryConfChange {
		n.processConfChange(ctx, entry)
	}

	n.appliedIndex = entry.Index
	return nil
}

func (n *Node) processEntry(ctx context.Context, entry raftpb.Entry) error {
	r := &api.InternalRaftRequest{}
	err := proto.Unmarshal(entry.Data, r)
	if err != nil {
		return err
	}

	if !n.wait.trigger(r.ID, r) {
		// There was no wait on this ID, meaning we don't have a
		// transaction in progress that would be committed to the
		// memory store by the "trigger" call. This could mean that:
		// 1. Startup is in progress, and the raft WAL is being parsed,
		// processed and applied to the store, or
		// 2. Either a different node wrote this to raft,
		// or we wrote it before losing the leader
		// position and cancelling the transaction. This entry still needs
		// to be committed since other nodes have already committed it.
		// Create a new transaction to commit this entry.

		// It should not be possible for processInternalRaftRequest
		// to be running in this situation, but out of caution we
		// cancel any current invocations to avoid a deadlock.
		// TODO(anshul) This call is likely redundant, remove after consideration.
		n.wait.cancelAll()

		err := n.memoryStore.ApplyStoreActions(r.Action)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to apply actions from raft")
		}
	}
	return nil
}

func (n *Node) processConfChange(ctx context.Context, entry raftpb.Entry) {
	var (
		err error
		cc  raftpb.ConfChange
	)

	if err := proto.Unmarshal(entry.Data, &cc); err != nil {
		n.wait.trigger(cc.ID, err)
	}

	if err := n.cluster.ValidateConfigurationChange(cc); err != nil {
		n.wait.trigger(cc.ID, err)
	}

	switch cc.Type {
	case raftpb.ConfChangeAddNode:
		err = n.applyAddNode(cc)
	case raftpb.ConfChangeUpdateNode:
		err = n.applyUpdateNode(ctx, cc)
	case raftpb.ConfChangeRemoveNode:
		err = n.applyRemoveNode(ctx, cc)
	}

	if err != nil {
		n.wait.trigger(cc.ID, err)
	}

	n.confState = *n.raftNode.ApplyConfChange(cc)
	n.wait.trigger(cc.ID, nil)
}

// applyAddNode is called when we receive a ConfChange
// from a member in the raft cluster, this adds a new
// node to the existing raft cluster
func (n *Node) applyAddNode(cc raftpb.ConfChange) error {
	member := &api.RaftMember{}
	err := proto.Unmarshal(cc.Context, member)
	if err != nil {
		return err
	}

	// ID must be non zero
	if member.RaftID == 0 {
		return nil
	}

	return n.registerNode(member)
}

// applyUpdateNode is called when we receive a ConfChange from a member in the
// raft cluster which update the address of an existing node.
func (n *Node) applyUpdateNode(ctx context.Context, cc raftpb.ConfChange) error {
	newMember := &api.RaftMember{}
	err := proto.Unmarshal(cc.Context, newMember)
	if err != nil {
		return err
	}

	if newMember.RaftID == n.Config.ID {
		return nil
	}
	if err := n.transport.UpdatePeer(newMember.RaftID, newMember.Addr); err != nil {
		return err
	}
	return n.cluster.UpdateMember(newMember.RaftID, newMember)
}

// applyRemoveNode is called when we receive a ConfChange
// from a member in the raft cluster, this removes a node
// from the existing raft cluster
func (n *Node) applyRemoveNode(ctx context.Context, cc raftpb.ConfChange) (err error) {
	// If the node from where the remove is issued is
	// a follower and the leader steps down, Campaign
	// to be the leader.

	if cc.NodeID == n.leader() && !n.isLeader() {
		if err = n.raftNode.Campaign(ctx); err != nil {
			return err
		}
	}

	if cc.NodeID == n.Config.ID {
		// wait for the commit ack to be sent before closing connection
		n.asyncTasks.Wait()

		n.NodeRemoved()
	} else if err := n.transport.RemovePeer(cc.NodeID); err != nil {
		return err
	}

	return n.cluster.RemoveMember(cc.NodeID)
}

// SubscribeLeadership returns channel to which events about leadership change
// will be sent in form of raft.LeadershipState. Also cancel func is returned -
// it should be called when listener is no longer interested in events.
func (n *Node) SubscribeLeadership() (q chan events.Event, cancel func()) {
	return n.leadershipBroadcast.Watch()
}

// createConfigChangeEnts creates a series of Raft entries (i.e.
// EntryConfChange) to remove the set of given IDs from the cluster. The ID
// `self` is _not_ removed, even if present in the set.
// If `self` is not inside the given ids, it creates a Raft entry to add a
// default member with the given `self`.
func createConfigChangeEnts(ids []uint64, self uint64, term, index uint64) []raftpb.Entry {
	var ents []raftpb.Entry
	next := index + 1
	found := false
	for _, id := range ids {
		if id == self {
			found = true
			continue
		}
		cc := &raftpb.ConfChange{
			Type:   raftpb.ConfChangeRemoveNode,
			NodeID: id,
		}
		data, err := cc.Marshal()
		if err != nil {
			log.L.WithError(err).Panic("marshal configuration change should never fail")
		}
		e := raftpb.Entry{
			Type:  raftpb.EntryConfChange,
			Data:  data,
			Term:  term,
			Index: next,
		}
		ents = append(ents, e)
		next++
	}
	if !found {
		node := &api.RaftMember{RaftID: self}
		meta, err := node.Marshal()
		if err != nil {
			log.L.WithError(err).Panic("marshal member should never fail")
		}
		cc := &raftpb.ConfChange{
			Type:    raftpb.ConfChangeAddNode,
			NodeID:  self,
			Context: meta,
		}
		data, err := cc.Marshal()
		if err != nil {
			log.L.WithError(err).Panic("marshal configuration change should never fail")
		}
		e := raftpb.Entry{
			Type:  raftpb.EntryConfChange,
			Data:  data,
			Term:  term,
			Index: next,
		}
		ents = append(ents, e)
	}
	return ents
}

// getIDs returns an ordered set of IDs included in the given snapshot and
// the entries. The given snapshot/entries can contain two kinds of
// ID-related entry:
// - ConfChangeAddNode, in which case the contained ID will be added into the set.
// - ConfChangeRemoveNode, in which case the contained ID will be removed from the set.
func getIDs(snap *raftpb.Snapshot, ents []raftpb.Entry) []uint64 {
	ids := make(map[uint64]struct{})
	if snap != nil {
		for _, id := range snap.Metadata.ConfState.Nodes {
			ids[id] = struct{}{}
		}
	}
	for _, e := range ents {
		if e.Type != raftpb.EntryConfChange {
			continue
		}
		if snap != nil && e.Index < snap.Metadata.Index {
			continue
		}
		var cc raftpb.ConfChange
		if err := cc.Unmarshal(e.Data); err != nil {
			log.L.WithError(err).Panic("unmarshal configuration change should never fail")
		}
		switch cc.Type {
		case raftpb.ConfChangeAddNode:
			ids[cc.NodeID] = struct{}{}
		case raftpb.ConfChangeRemoveNode:
			delete(ids, cc.NodeID)
		case raftpb.ConfChangeUpdateNode:
			// do nothing
		default:
			log.L.Panic("ConfChange Type should be either ConfChangeAddNode, or ConfChangeRemoveNode, or ConfChangeUpdateNode!")
		}
	}
	var sids []uint64
	for id := range ids {
		sids = append(sids, id)
	}
	return sids
}

func (n *Node) reqTimeout() time.Duration {
	return 5*time.Second + 2*time.Duration(n.Config.ElectionTick)*n.opts.TickInterval
}

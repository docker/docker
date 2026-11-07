package networkdb

//go:generate protoc -I=. -I=../../../vendor/ --gogofaster_out=import_path=github.com/moby/moby/v2/daemon/libnetwork/networkdb:. networkdb.proto

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	"github.com/docker/go-events"
	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/hashicorp/memberlist"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/lamport"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

const (
	byTable int = 1 + iota
	byNetwork
)

// NetworkDB instance drives the networkdb cluster and acts the broker
// for cluster-scoped and network-scoped gossip and watches.
type NetworkDB struct {
	// The clocks MUST be the first things
	// in this struct due to Golang issue #599.

	// Global lamport clock for node network attach events.
	networkClock lamport.Clock

	// Global lamport clock for table events.
	tableClock lamport.Clock

	sync.RWMutex

	// NetworkDB configuration.
	config *Config

	// All the tree index (byTable, byNetwork) that we maintain
	// the db.
	indexes map[int]*iradix.Tree[*entry]

	// Memberlist we use to drive the cluster.
	memberlist *memberlist.Memberlist

	// List of all peer nodes in the cluster not-limited to any
	// network.
	nodes nodeMap

	// An approximation of len(nodes) that can be accessed without
	// synchronization.
	estNodes atomic.Int32

	// List of all peer nodes which have failed. The attachments and table
	// entries of a failed node are remembered, hidden rather than dropped,
	// and put back in place if the node returns before it is reaped.
	failedNodes nodeMap

	// A multi-dimensional map of network/node attachments for peer nodes.
	// The first key is a node name and the second key is a network ID for
	// the network that node is participating in. Attachments of a failed
	// node are kept until it is reaped, so its table events keep being
	// accepted and its peer-list membership can be restored if it returns.
	networks map[string]map[string]*network

	// A map of this node's network attachments.
	thisNodeNetworks map[string]*thisNodeNetwork

	// The active peers participating in a given network, which gossip and
	// bulk sync pick peers from, keyed by network ID. A node is taken out
	// when it fails or leaves, and put back if it returns.
	networkNodes map[string][]string

	// A table of subscriptions for every node from which we are waiting for
	// a bulk-sync ack.
	bulkSyncAckTbl map[string][]bulkSyncSubscription

	// A count of the number of times we have timed out waiting for a bulk
	// sync ack from any peer node.
	bulkSyncAckTimeouts atomic.Uint64

	// Broadcast queue for network event gossip.
	networkBroadcasts *memberlist.TransmitLimitedQueue

	// Broadcast queue for node event gossip.
	nodeBroadcasts *memberlist.TransmitLimitedQueue

	// A central context to stop all go routines running on
	// behalf of the NetworkDB instance.
	ctx       context.Context
	cancelCtx context.CancelFunc

	// A central broadcaster for all local watchers watching table
	// events.
	broadcaster *events.Broadcaster

	// Reference to the memberlist's keyring to add & remove keys
	keyring *memberlist.Keyring

	// lastStatsTimestamp is the last timestamp when the stats got printed
	lastStatsTimestamp time.Time

	// lastHealthTimestamp is the last timestamp when the health score got printed
	lastHealthTimestamp time.Time

	rngMu sync.Mutex
	rng   *rand.Rand
}

// PeerInfo represents the peer (gossip cluster) nodes of a network
type PeerInfo struct {
	Name string
	IP   netip.Addr
}

// PeerClusterInfo represents the peer (gossip cluster) nodes
type PeerClusterInfo struct {
	PeerInfo
}

type node struct {
	memberlist.Node
	ltime lamport.Time
	// Number of hours left before the reaper removes the node
	reapTime time.Duration
}

// network describes the node/network attachment.
type network struct {
	// Lamport time for the latest state of the entry.
	ltime lamport.Time

	// Node leave is in progress.
	leaving bool

	// Number of seconds still left before a deleted network entry gets
	// removed from networkDB
	reapTime time.Duration
}

// networkEventType returns the [NetworkEvent_Type] which announces an
// attachment that is, or is not, in the process of leaving.
func networkEventType(leaving bool) NetworkEvent_Type {
	if leaving {
		return NetworkEventTypeLeave
	}
	return NetworkEventTypeJoin
}

// thisNodeNetwork describes a network attachment on the local node.
type thisNodeNetwork struct {
	network

	// Gets set to true after the first bulk sync happens
	inSync bool

	// The broadcast queue for this network's table event gossip
	// for entries owned by this node.
	tableBroadcasts *memberlist.TransmitLimitedQueue

	// The broadcast queue for this network's table event gossip
	// relayed from other nodes.
	//
	// Messages in this queue are broadcasted when there is space available
	// in the gossip packet after filling it with tableBroadcast messages.
	// Relayed messages are broadcasted at a lower priority than messages
	// originating from this node to ensure that local messages are always
	// broadcasted in a timely manner, irrespective of how many messages
	// from other nodes are queued for rebroadcasting.
	tableRebroadcasts *memberlist.TransmitLimitedQueue

	// Number of gossip messages sent related to this network during the last stats collection period
	qMessagesSent atomic.Int64

	// Number of entries on the network. This value is the sum of all the entries of all the tables of a specific network.
	// Its use is for statistics purposes. It keep tracks of database size and is printed per network every StatsPrintPeriod
	// interval
	entriesNumber atomic.Int64

	// An approximation of len(nDB.networkNodes[nid]) that can be accessed
	// without synchronization.
	networkNodes atomic.Int32
}

// Config represents the configuration of the networkdb instance and
// can be passed by the caller.
type Config struct {
	// NodeID is the node unique identifier of the node when is part of the cluster
	NodeID string

	// Hostname is the node hostname.
	Hostname string

	// BindAddr is the IP on which networkdb listens. It can be
	// 0.0.0.0 to listen on all addresses on the host.
	BindAddr string

	// AdvertiseAddr is the node's IP address that we advertise for
	// cluster communication.
	AdvertiseAddr string

	// BindPort is the local node's port to which we bind to for
	// cluster communication. Zero leaves the choice to memberlist, so it is
	// not the port this node ends up gossiping on: for that, ask memberlist
	// what it advertises.
	BindPort int

	// Keys to be added to the Keyring of the memberlist. Key at index
	// 0 is the primary key
	Keys [][]byte

	// PacketBufferSize is the maximum number of bytes that memberlist will
	// put in a packet (this will be for UDP packets by default with a NetTransport).
	// A safe value for this is typically 1400 bytes (which is the default). However,
	// depending on your network's MTU (Maximum Transmission Unit) you may
	// be able to increase this to get more content into each gossip packet.
	PacketBufferSize int

	// reapEntryInterval duration of a deleted entry before being garbage collected
	reapEntryInterval time.Duration

	// reapNetworkInterval duration of a deleted network before being garbage collected
	// NOTE this MUST always be higher than reapEntryInterval
	reapNetworkInterval time.Duration

	// rejoinClusterDuration represents retryJoin timeout used by rejoinClusterBootStrap.
	// Default is 10sec.
	rejoinClusterDuration time.Duration

	// rejoinClusterInterval represents interval on which rejoinClusterBootStrap runs.
	// Default is 60sec.
	rejoinClusterInterval time.Duration

	// transport, when non-nil, is gossiped over in place of the UDP and TCP
	// sockets memberlist would otherwise open for itself. BindAddr,
	// AdvertiseAddr and BindPort are then the transport's business, not
	// memberlist's, and must agree with what it advertises.
	//
	// Only set by tests, to substitute an in-memory network.
	transport memberlist.Transport

	// tableEventObserver, when non-nil, is called each time a table event
	// received from a peer is applied to the local store, reporting the node
	// which applied it and whether it arrived by bulk sync rather than by
	// gossip. It lets a test attribute convergence to the mechanism which
	// actually delivered the last missing entry, instead of inferring it from
	// the elapsed time.
	//
	// The receiving node is reported because one observer is shared by a whole
	// cluster: without it, an entry delivered to any node by bulk sync is
	// indistinguishable from one delivered to the node under observation.
	//
	// It is called with the NetworkDB write lock held, so it must not block
	// and must not call back into NetworkDB.
	//
	// Only set by tests.
	tableEventObserver func(nodeID, networkID, tableName, key string, viaBulkSync bool)

	// rngSeed, when non-nil, seeds the RNG which picks gossip and bulk-sync
	// peers, in place of a seed read from crypto/rand, so that a test can fix
	// what is otherwise drawn from a source it can neither control nor record.
	//
	// It does not make a cluster's gossip reproducible. Which peer a node picks
	// also depends on which peers it has heard of by the time it draws, and that
	// follows from goroutine scheduling; memberlist draws from the math/rand
	// global for its own shuffling and probe timing, and offers no way to inject
	// an RNG. Seeding removes one uncontrolled input of several.
	//
	// Note this is the seed, not the RNG: every instance still gets its own
	// stream, so nodes do not make identical choices.
	//
	// Only set by tests.
	rngSeed *[32]byte

	// BootstrapPeers reports the addresses this node should try to rejoin the
	// cluster through, or nil if none are known. It is consulted every time a
	// rejoin is considered, so a caller which can name the current peers keeps
	// this node from bootstrapping off addresses which have since gone away.
	//
	// It is called from the rejoin timer with no NetworkDB lock held, so it
	// must be safe for concurrent use, and it must not block for long.
	//
	// When nil this node never bootstraps its way back. Join still gets it
	// into the cluster, and reconnectNode goes on retrying the peers it saw
	// fail, so a node which falls out can still return by itself -- but once
	// reapDeadNode has forgotten those peers there is nothing left to rejoin
	// through.
	BootstrapPeers func() []string

	// StatsPrintPeriod the period to use to print queue stats
	// Default is 5min
	StatsPrintPeriod time.Duration

	// HealthPrintPeriod the period to use to print the health score
	// Default is 1min
	HealthPrintPeriod time.Duration
}

// entry defines a table entry
type entry struct {
	// node from which this entry was learned.
	node string

	// Lamport time for the most recent update to the entry
	ltime lamport.Time

	// Opaque value store in the entry
	value []byte

	// Deleting the entry is in progress. All entries linger in
	// the cluster for certain amount of time after deletion.
	deleting bool

	// Time still left before the entry gets removed from networkDB. Set
	// for deleted entries lingering as tombstones, and for entries hidden
	// because their owner failed.
	reapTime time.Duration
}

// bulkSyncSubscription is a subscription to the bulk-sync progress for a peer node.
// Done is closed when a bulk sync from the node is received with an LTime
// greater than the subscription's LTime.
type bulkSyncSubscription struct {
	LTime lamport.Time
	Done  chan<- struct{}
}

// DefaultConfig returns a NetworkDB config with default values
func DefaultConfig() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		NodeID:                stringid.TruncateID(stringid.GenerateRandomID()),
		Hostname:              hostname,
		BindAddr:              "0.0.0.0",
		PacketBufferSize:      1400,
		StatsPrintPeriod:      5 * time.Minute,
		HealthPrintPeriod:     1 * time.Minute,
		reapEntryInterval:     30 * time.Minute,
		rejoinClusterDuration: 10 * time.Second,
		rejoinClusterInterval: 60 * time.Second,
	}
}

// New creates a new instance of NetworkDB using the Config passed by
// the caller.
func New(c *Config) (*NetworkDB, error) {
	nDB := newNetworkDB(c)
	log.G(context.TODO()).Infof("New memberlist node - Node:%v will use memberlist nodeID:%v with config:%+v", c.Hostname, c.NodeID, c)
	if err := nDB.clusterInit(); err != nil {
		return nil, err
	}

	return nDB, nil
}

func newNetworkDB(c *Config) *NetworkDB {
	// The garbage collection logic for entries leverage the presence of the network.
	// For this reason the expiration time of the network is put slightly higher than the entry expiration so that
	// there is at least 5 extra cycle to make sure that all the entries are properly deleted before deleting the network.
	c.reapNetworkInterval = c.reapEntryInterval + 5*reapPeriod

	var rngSeed [32]byte
	if c.rngSeed != nil {
		rngSeed = *c.rngSeed
	} else {
		_, _ = cryptorand.Read(rngSeed[:]) // Documented never to return an error
	}

	return &NetworkDB{
		config: c,
		indexes: map[int]*iradix.Tree[*entry]{
			byTable:   iradix.New[*entry](),
			byNetwork: iradix.New[*entry](),
		},
		networks:         make(map[string]map[string]*network),
		thisNodeNetworks: make(map[string]*thisNodeNetwork),
		nodes:            make(map[string]*node),
		failedNodes:      make(map[string]*node),
		networkNodes:     make(map[string][]string),
		bulkSyncAckTbl:   make(map[string][]bulkSyncSubscription),
		broadcaster:      events.NewBroadcaster(),
		rng:              rand.New(rand.NewChaCha8(rngSeed)), //gosec:disable G404 -- not used in a security sensitive context
	}
}

// Join joins this NetworkDB instance with a list of peer NetworkDB
// instances passed by the caller in the form of addr:port
func (nDB *NetworkDB) Join(members []string) error {
	return nDB.clusterJoin(members)
}

// Close destroys this NetworkDB instance by leave the cluster,
// stopping timers, canceling goroutines etc.
func (nDB *NetworkDB) Close() {
	if err := nDB.clusterLeave(); err != nil {
		log.G(context.TODO()).Errorf("%v(%v) Could not close DB: %v", nDB.config.Hostname, nDB.config.NodeID, err)
	}

	// Avoid (*Broadcaster).run goroutine leak
	nDB.broadcaster.Close()
}

// ClusterPeers returns all the gossip cluster peers.
func (nDB *NetworkDB) ClusterPeers() []PeerInfo {
	nDB.RLock()
	defer nDB.RUnlock()
	peers := make([]PeerInfo, 0, len(nDB.nodes))
	for _, node := range nDB.nodes {
		ip, _ := netip.AddrFromSlice(node.Node.Addr)
		peers = append(peers, PeerInfo{
			Name: node.Name,
			IP:   ip.Unmap(),
		})
	}
	return peers
}

// Peers returns the gossip peers for a given network.
func (nDB *NetworkDB) Peers(nid string) []PeerInfo {
	nDB.RLock()
	defer nDB.RUnlock()
	peers := make([]PeerInfo, 0, len(nDB.networkNodes[nid]))
	for _, nodeName := range nDB.networkNodes[nid] {
		if node, ok := nDB.nodes[nodeName]; ok {
			ip, _ := netip.AddrFromSlice(node.Node.Addr)
			peers = append(peers, PeerInfo{
				Name: node.Name,
				IP:   ip.Unmap(),
			})
		} else {
			// Added for testing purposes, this condition should never happen else mean that the network list
			// is out of sync with the node list
			peers = append(peers, PeerInfo{Name: nodeName})
		}
	}
	return peers
}

// GetEntry retrieves the value of a table entry in a given (network,
// table, key) tuple
func (nDB *NetworkDB) GetEntry(tname, nid, key string) ([]byte, error) {
	nDB.RLock()
	defer nDB.RUnlock()
	v, err := nDB.getEntry(tname, nid, key)
	if err != nil {
		return nil, err
	}
	if v != nil && v.deleting {
		return nil, types.NotFoundErrorf("entry in table %s network id %s and key %s deleted and pending garbage collection", tname, nid, key)
	}
	if nDB.isNodeFailed(v.node) {
		return nil, types.NotFoundErrorf("entry in table %s network id %s and key %s is owned by a failed node and hidden until it returns", tname, nid, key)
	}

	// note: this panics if a nil entry was stored in the table; after
	// discussion, we decided to not gracefully handle this situation as
	// this would be an unexpected situation;
	// see https://github.com/moby/moby/pull/48157#discussion_r1674428635
	return v.value, nil
}

func (nDB *NetworkDB) getEntry(tname, nid, key string) (*entry, error) {
	e, ok := nDB.indexes[byTable].Get(fmt.Appendf(nil, "/%s/%s/%s", tname, nid, key))
	if !ok {
		return nil, types.NotFoundErrorf("could not get entry in table %s with network id %s and key %s", tname, nid, key)
	}

	return e, nil
}

// isNodeFailed reports whether the node is a peer which memberlist has
// declared failed. The state of a failed node is remembered, but hidden from
// watchers and reads, until the node either comes back or is reaped.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) isNodeFailed(nodeName string) bool {
	_, failed := nDB.failedNodes[nodeName]
	return failed
}

// failedNodeSet returns the names of the currently failed nodes as a set,
// for filtering entries after the lock has been released.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) failedNodeSet() map[string]bool {
	if len(nDB.failedNodes) == 0 {
		return nil
	}
	failed := make(map[string]bool, len(nDB.failedNodes))
	for name := range nDB.failedNodes {
		failed[name] = true
	}
	return failed
}

// CreateEntry creates a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster. It is an error to create an
// entry for the same tuple for which there is already an existing
// entry, unless that entry is a tombstone or is hidden because its owner
// is a failed node.
func (nDB *NetworkDB) CreateEntry(tname, nid, key string, value []byte) error {
	nDB.Lock()
	oldEntry, err := nDB.getEntry(tname, nid, key)
	if err == nil && !oldEntry.deleting && !nDB.isNodeFailed(oldEntry.node) {
		nDB.Unlock()
		return fmt.Errorf("cannot create entry in table %s with network id %s and key %s, already exists", tname, nid, key)
	}

	entry := &entry{
		ltime: nDB.tableClock.Increment(),
		node:  nDB.config.NodeID,
		value: value,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeCreate, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send create event for table %s, %v", tname, err)
	}

	return nil
}

// UpdateEntry updates a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster. It is an error to update a
// non-existent entry, including an entry that is a tombstone or is
// hidden because its owner is a failed node.
func (nDB *NetworkDB) UpdateEntry(tname, nid, key string, value []byte) error {
	nDB.Lock()
	oldEntry, err := nDB.getEntry(tname, nid, key)
	if err != nil || oldEntry.deleting || nDB.isNodeFailed(oldEntry.node) {
		nDB.Unlock()
		return fmt.Errorf("cannot update entry as the entry in table %s with network id %s and key %s does not exist", tname, nid, key)
	}

	entry := &entry{
		ltime: nDB.tableClock.Increment(),
		node:  nDB.config.NodeID,
		value: value,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeUpdate, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send table update event: %v", err)
	}

	return nil
}

// TableElem elem
type TableElem struct {
	Value []byte
	owner string
}

// GetTableByNetwork walks the networkdb by the give table and network id and
// returns a map of keys and values
func (nDB *NetworkDB) GetTableByNetwork(tname, nid string) map[string]*TableElem {
	nDB.RLock()
	root := nDB.indexes[byTable].Root()
	failed := nDB.failedNodeSet()
	nDB.RUnlock()
	entries := make(map[string]*TableElem)
	root.WalkPrefix(fmt.Appendf(nil, "/%s/%s", tname, nid), func(k []byte, v *entry) bool {
		if v.deleting || failed[v.node] {
			return false
		}
		key := string(k)
		key = key[strings.LastIndex(key, "/")+1:]
		entries[key] = &TableElem{Value: v.value, owner: v.node}
		return false
	})
	return entries
}

// DeleteEntry deletes a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster.
func (nDB *NetworkDB) DeleteEntry(tname, nid, key string) error {
	nDB.Lock()
	oldEntry, err := nDB.getEntry(tname, nid, key)
	if err != nil || oldEntry.deleting || nDB.isNodeFailed(oldEntry.node) {
		nDB.Unlock()
		return fmt.Errorf("cannot delete entry %s with network id %s and key %s "+
			"does not exist or is already being deleted", tname, nid, key)
	}

	entry := &entry{
		ltime:    nDB.tableClock.Increment(),
		node:     nDB.config.NodeID,
		value:    oldEntry.value,
		deleting: true,
		reapTime: nDB.config.reapEntryInterval,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeDelete, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send table delete event: %v", err)
	}

	return nil
}

// isNodeAttached reports whether the node has a non-leaving attachment for
// the network. A failed node still counts if its attachment was remembered.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) isNodeAttached(nodeName, nid string) bool {
	n, ok := nDB.networks[nodeName][nid]
	return ok && !n.leaving
}

// deleteNodeFromNetworks stops the node from being a peer of any network and
// forgets which ones it was attached to.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) deleteNodeFromNetworks(deletedNode string) {
	nDB.suspendNodeInNetworks(deletedNode)
	delete(nDB.networks, deletedNode)
}

// suspendNodeInNetworks stops the node from being a peer of any network,
// while keeping its attachments for restoreNodeInNetworks.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) suspendNodeInNetworks(nodeName string) {
	for nid := range nDB.networkNodes {
		nDB.deleteNetworkNode(nid, nodeName)
	}
}

// restoreNodeInNetworks makes the node a peer again of the networks it is
// attached to.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) restoreNodeInNetworks(nodeName string) {
	for nid, n := range nDB.networks[nodeName] {
		if n.leaving {
			continue
		}
		nDB.addNetworkNode(nid, nodeName)
	}
}

// suspendNodeTableEntries hides all table entries owned by node from
// watchers and reads while keeping them in the store, so that the freshest
// state possible is already in place if the node comes back. The entries
// age out on the entry reap timer.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) suspendNodeTableEntries(node string) {
	nDB.indexes[byTable].Root().Walk(func(path []byte, e *entry) bool {
		if e.node != node || e.deleting {
			return false
		}
		params := strings.Split(string(path[1:]), "/")
		tName, nwID, key := params[0], params[1], params[2]

		e.reapTime = nDB.config.reapEntryInterval
		nDB.broadcaster.Write(WatchEvent{
			Table:     tName,
			NetworkID: nwID,
			Key:       key,
			Prev:      e.value,
		})
		return false
	})
}

// restoreNodeTableEntries makes the table entries which were remembered
// across a failure of node visible again. What is published is the freshest
// state this node remembered; a peer which holds newer relayed state
// delivers it as a regular update afterwards.
// Caller should hold the NetworkDB lock while calling this.
func (nDB *NetworkDB) restoreNodeTableEntries(node string) {
	nDB.indexes[byTable].Root().Walk(func(path []byte, e *entry) bool {
		if e.node != node || e.deleting {
			return false
		}
		params := strings.Split(string(path[1:]), "/")
		tName, nwID, key := params[0], params[1], params[2]

		e.reapTime = 0
		nDB.broadcaster.Write(WatchEvent{
			Table:     tName,
			NetworkID: nwID,
			Key:       key,
			Value:     e.value,
		})
		return false
	})
}

// deleteNodeNetworkEntries deletes all table entries for a network owned by
// node from the local store.
func (nDB *NetworkDB) deleteNodeNetworkEntries(nid, node string) {
	hidden := nDB.isNodeFailed(node)
	nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid),
		func(path []byte, oldEntry *entry) bool {
			// Do nothing if the entry is owned by a remote node that is not leaving the network
			// because the event is triggered for a node that does not own this entry.
			if oldEntry.node != node {
				return false
			}
			params := strings.Split(string(path[1:]), "/")
			nwID, tName, key := params[0], params[1], params[2]

			nDB.deleteEntry(nwID, tName, key)

			// Notify to the upper layer only entries not already marked for
			// deletion, and not already hidden because their owner is failed.
			if !oldEntry.deleting && !hidden {
				nDB.broadcaster.Write(WatchEvent{
					Table:     tName,
					NetworkID: nwID,
					Key:       key,
					Prev:      oldEntry.value,
				})
			}
			return false
		})
}

// deleteNodeTableEntries deletes all table entries owned by node from the local
// store, across all networks. It returns the events for the entries which were
// visible, for the caller to broadcast if their removal is to be observed.
func (nDB *NetworkDB) deleteNodeTableEntries(node string) []WatchEvent {
	var deleted []WatchEvent
	nDB.indexes[byTable].Root().Walk(func(path []byte, e *entry) bool {
		if e.node != node {
			return false
		}

		params := strings.Split(string(path[1:]), "/")
		tName, nwID, key := params[0], params[1], params[2]

		nDB.deleteEntry(nwID, tName, key)

		if !e.deleting {
			deleted = append(deleted, WatchEvent{
				Table:     tName,
				NetworkID: nwID,
				Key:       key,
				Prev:      e.value,
			})
		}
		return false
	})
	return deleted
}

// WalkTable walks a single table in NetworkDB and invokes the passed
// function for each entry in the table passing the network, key,
// value. The walk stops if the passed function returns a true.
// Entries which are hidden because their owner is failed are reported as
// deleted.
func (nDB *NetworkDB) WalkTable(tname string, fn func(string, string, []byte, bool) bool) error {
	nDB.RLock()
	root := nDB.indexes[byTable].Root()
	failed := nDB.failedNodeSet()
	nDB.RUnlock()
	root.WalkPrefix([]byte("/"+tname), func(path []byte, v *entry) bool {
		params := strings.Split(string(path[1:]), "/")
		nid := params[1]
		key := params[2]
		deleted := v.deleting || failed[v.node]
		return fn(nid, key, v.value, deleted)
	})

	return nil
}

// JoinNetwork joins this node to a given network and propagates this
// event across the cluster. This triggers this node joining the
// sub-cluster of this network and participates in the network-scoped
// gossip and bulk sync for this network.
func (nDB *NetworkDB) JoinNetwork(nid string) error {
	ltime := nDB.networkClock.Increment()

	nDB.Lock()
	n, ok := nDB.thisNodeNetworks[nid]
	if ok {
		if !n.leaving {
			nDB.Unlock()
			return fmt.Errorf("networkdb: network %s is already joined", nid)
		}
		n.network = network{ltime: ltime}
		n.inSync = false
	} else {
		n = &thisNodeNetwork{
			network: network{ltime: ltime},
			tableBroadcasts: &memberlist.TransmitLimitedQueue{
				RetransmitMult: 4,
			},
			tableRebroadcasts: &memberlist.TransmitLimitedQueue{
				RetransmitMult: 4,
			},
		}
		numNodes := func() int { return int(n.networkNodes.Load()) }
		n.tableBroadcasts.NumNodes = numNodes
		n.tableRebroadcasts.NumNodes = numNodes
	}
	nDB.addNetworkNode(nid, nDB.config.NodeID)

	if err := nDB.sendNetworkEvent(nid, NetworkEventTypeJoin, ltime); err != nil {
		nDB.Unlock()
		return fmt.Errorf("failed to send join network event for %s: %v", nid, err)
	}

	networkNodes := nDB.networkNodes[nid]
	n.networkNodes.Store(int32(len(networkNodes)))
	nDB.thisNodeNetworks[nid] = n
	nDB.Unlock()

	log.G(context.TODO()).Debugf("%v(%v): joined network %s", nDB.config.Hostname, nDB.config.NodeID, nid)
	if _, err := nDB.bulkSync(networkNodes, true); err != nil {
		log.G(context.TODO()).Errorf("Error bulk syncing while joining network %s: %v", nid, err)
	}

	// Mark the network as being synced
	// note this is a best effort, we are not checking the result of the bulk sync
	nDB.Lock()
	n.inSync = true
	nDB.Unlock()

	return nil
}

// LeaveNetwork leaves this node from a given network and propagates
// this event across the cluster. This triggers this node leaving the
// sub-cluster of this network and as a result will no longer
// participate in the network-scoped gossip and bulk sync for this
// network. Also remove all the table entries for this network from
// networkdb
func (nDB *NetworkDB) LeaveNetwork(nid string) error {
	ltime := nDB.networkClock.Increment()
	if err := nDB.sendNetworkEvent(nid, NetworkEventTypeLeave, ltime); err != nil {
		return fmt.Errorf("failed to send leave network event for %s: %v", nid, err)
	}

	nDB.Lock()
	defer nDB.Unlock()

	// Remove myself from the list of the nodes participating to the network
	nDB.deleteNetworkNode(nid, nDB.config.NodeID)

	// Mark all the local entries for deletion
	// so that if we rejoin the network
	// before another node has received the network-leave notification,
	// the old entries owned by us will still be purged as expected.
	// Delete all the remote entries from our local store
	// without leaving any tombstone.
	// This ensures that we will accept the CREATE events
	// for entries owned by remote nodes
	// if we later rejoin the network.
	nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, oldEntry *entry) bool {
		owned := oldEntry.node == nDB.config.NodeID
		if owned && oldEntry.deleting {
			return false
		}

		params := strings.Split(string(path[1:]), "/")
		nwID, tName, key := params[0], params[1], params[2]
		if owned {
			newEntry := &entry{
				ltime:    nDB.tableClock.Increment(),
				node:     oldEntry.node,
				value:    oldEntry.value,
				deleting: true,
				reapTime: nDB.config.reapEntryInterval,
			}
			nDB.createOrUpdateEntry(nwID, tName, key, newEntry)
		} else {
			nDB.deleteEntry(nwID, tName, key)
		}
		// Do not notify watchers of entries already hidden from them
		// because their owner is failed.
		hidden := nDB.isNodeFailed(oldEntry.node)
		if !oldEntry.deleting && !hidden {
			nDB.broadcaster.Write(WatchEvent{
				Table:     tName,
				NetworkID: nwID,
				Key:       key,
				Prev:      oldEntry.value,
			})
		}
		return false
	})

	n, ok := nDB.thisNodeNetworks[nid]
	if !ok {
		return fmt.Errorf("could not find network %s while trying to leave", nid)
	}

	log.G(context.TODO()).Debugf("%v(%v): leaving network %s", nDB.config.Hostname, nDB.config.NodeID, nid)
	n.ltime = ltime
	n.reapTime = nDB.config.reapNetworkInterval
	n.leaving = true
	return nil
}

// addNetworkNode adds the node to the list of nodes which participate
// in the passed network only if it is not already present. Caller
// should hold the NetworkDB lock while calling this
func (nDB *NetworkDB) addNetworkNode(nid string, nodeName string) {
	if slices.Contains(nDB.networkNodes[nid], nodeName) {
		return
	}

	nDB.networkNodes[nid] = append(nDB.networkNodes[nid], nodeName)
	if n, ok := nDB.thisNodeNetworks[nid]; ok {
		n.networkNodes.Store(int32(len(nDB.networkNodes[nid])))
	}
}

// Deletes the node from the list of nodes which participate in the
// passed network. Caller should hold the NetworkDB lock while calling
// this
func (nDB *NetworkDB) deleteNetworkNode(nid string, nodeName string) {
	nodes, ok := nDB.networkNodes[nid]
	if !ok || len(nodes) == 0 {
		return
	}
	newNodes := make([]string, 0, len(nodes)-1)
	for _, name := range nodes {
		if name == nodeName {
			continue
		}
		newNodes = append(newNodes, name)
	}
	nDB.networkNodes[nid] = newNodes
	if n, ok := nDB.thisNodeNetworks[nid]; ok {
		n.networkNodes.Store(int32(len(newNodes)))
	}
}

// findCommonNetworks find the networks that both this node and the
// passed node have joined.
func (nDB *NetworkDB) findCommonNetworks(nodeName string) []string {
	nDB.RLock()
	defer nDB.RUnlock()

	var networks []string
	for nid := range nDB.thisNodeNetworks {
		if n, ok := nDB.networks[nodeName][nid]; ok {
			if !n.leaving {
				networks = append(networks, nid)
			}
		}
	}

	return networks
}

func (nDB *NetworkDB) updateLocalNetworkTime() {
	nDB.Lock()
	defer nDB.Unlock()

	ltime := nDB.networkClock.Increment()
	for _, n := range nDB.thisNodeNetworks {
		n.ltime = ltime
	}
}

// createOrUpdateEntry this function handles the creation or update of entries into the local
// tree store. It is also used to keep in sync the entries number of the network (all tables are aggregated)
func (nDB *NetworkDB) createOrUpdateEntry(nid, tname, key string, v *entry) (okTable bool, okNetwork bool) {
	nDB.indexes[byTable], _, okTable = nDB.indexes[byTable].Insert(fmt.Appendf(nil, "/%s/%s/%s", tname, nid, key), v)
	nDB.indexes[byNetwork], _, okNetwork = nDB.indexes[byNetwork].Insert(fmt.Appendf(nil, "/%s/%s/%s", nid, tname, key), v)
	if !okNetwork {
		// Add only if it is an insert not an update
		n, ok := nDB.thisNodeNetworks[nid]
		if ok {
			n.entriesNumber.Add(1)
		}
	}
	return okTable, okNetwork
}

// deleteEntry this function handles the deletion of entries into the local tree store.
// It is also used to keep in sync the entries number of the network (all tables are aggregated)
func (nDB *NetworkDB) deleteEntry(nid, tname, key string) (okTable bool, okNetwork bool) {
	nDB.indexes[byTable], _, okTable = nDB.indexes[byTable].Delete(fmt.Appendf(nil, "/%s/%s/%s", tname, nid, key))
	nDB.indexes[byNetwork], _, okNetwork = nDB.indexes[byNetwork].Delete(fmt.Appendf(nil, "/%s/%s/%s", nid, tname, key))
	if okNetwork {
		// Remove only if the delete is successful
		n, ok := nDB.thisNodeNetworks[nid]
		if ok {
			n.entriesNumber.Add(-1)
		}
	}
	return okTable, okNetwork
}

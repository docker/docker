package networkdb

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	golog "log"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/hashicorp/memberlist"
)

const (
	reapPeriod       = 5 * time.Second
	retryInterval    = 1 * time.Second
	nodeReapInterval = 24 * time.Hour
	nodeReapPeriod   = 2 * time.Hour
	// considering a cluster with > 20 nodes and a drain speed of 100 msg/s
	// the following is roughly 1 minute
	maxQueueLenBroadcastOnSync = 500
)

type logWriter struct{}

func (l *logWriter) Write(p []byte) (int, error) {
	str := string(p)
	str = strings.TrimSuffix(str, "\n")

	switch {
	case strings.HasPrefix(str, "[WARN] "):
		str = strings.TrimPrefix(str, "[WARN] ")
		log.G(context.TODO()).Warn(str)
	case strings.HasPrefix(str, "[DEBUG] "):
		str = strings.TrimPrefix(str, "[DEBUG] ")
		log.G(context.TODO()).Debug(str)
	case strings.HasPrefix(str, "[INFO] "):
		str = strings.TrimPrefix(str, "[INFO] ")
		log.G(context.TODO()).Info(str)
	case strings.HasPrefix(str, "[ERR] "):
		str = strings.TrimPrefix(str, "[ERR] ")
		log.G(context.TODO()).Warn(str)
	}

	return len(p), nil
}

// SetKey adds a new key to the key ring
func (nDB *NetworkDB) SetKey(key []byte) {
	log.G(context.TODO()).Debugf("Adding key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for _, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			return
		}
	}
	nDB.config.Keys = append(nDB.config.Keys, key)
	if nDB.keyring != nil {
		nDB.keyring.AddKey(key)
	}
	logEncKeys(context.TODO(), key)
}

// SetPrimaryKey sets the given key as the primary key. This should have
// been added apriori through SetKey
func (nDB *NetworkDB) SetPrimaryKey(key []byte) {
	log.G(context.TODO()).Debugf("Primary Key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for _, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			if nDB.keyring != nil {
				nDB.keyring.UseKey(dbKey)
			}
			break
		}
	}
}

// RemoveKey removes a key from the key ring. The key being removed
// can't be the primary key
func (nDB *NetworkDB) RemoveKey(key []byte) {
	log.G(context.TODO()).Debugf("Remove Key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for i, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			nDB.config.Keys = append(nDB.config.Keys[:i], nDB.config.Keys[i+1:]...)
			if nDB.keyring != nil {
				nDB.keyring.RemoveKey(dbKey)
			}
			break
		}
	}
}

func (nDB *NetworkDB) clusterInit() error {
	nDB.lastStatsTimestamp = time.Now()
	nDB.lastHealthTimestamp = nDB.lastStatsTimestamp

	config := memberlist.DefaultLANConfig()
	config.Name = nDB.config.NodeID
	config.BindAddr = nDB.config.BindAddr
	config.AdvertiseAddr = nDB.config.AdvertiseAddr
	config.UDPBufferSize = nDB.config.PacketBufferSize
	// nil unless a test has substituted its own network, in which case
	// memberlist opens no sockets of its own.
	config.Transport = nDB.config.transport

	if nDB.config.BindPort != 0 {
		config.BindPort = nDB.config.BindPort
		config.AdvertisePort = nDB.config.BindPort
	}

	config.ProtocolVersion = memberlist.ProtocolVersion2Compatible
	config.Delegate = &delegate{nDB: nDB}
	config.Events = &eventDelegate{nDB: nDB}
	// custom logger that does not add time or date, so they are not
	// duplicated by logrus
	config.Logger = golog.New(&logWriter{}, "", 0)

	var err error
	if len(nDB.config.Keys) > 0 {
		for i, key := range nDB.config.Keys {
			log.G(context.TODO()).Debugf("Encryption key %d: %.5s", i+1, hex.EncodeToString(key))
		}
		logEncKeys(context.TODO(), nDB.config.Keys...)
		nDB.keyring, err = memberlist.NewKeyring(nDB.config.Keys, nDB.config.Keys[0])
		if err != nil {
			return err
		}
		config.Keyring = nDB.keyring
	}

	nDB.networkBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       nDB.estNumNodes,
		RetransmitMult: config.RetransmitMult,
	}

	nDB.nodeBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       nDB.estNumNodes,
		RetransmitMult: config.RetransmitMult,
	}

	mlist, err := memberlist.Create(config)
	if err != nil {
		return fmt.Errorf("failed to create memberlist: %v", err)
	}

	nDB.ctx, nDB.cancelCtx = context.WithCancel(context.Background())
	nDB.memberlist = mlist

	for _, trigger := range []struct {
		interval time.Duration
		fn       func()
	}{
		{reapPeriod, nDB.reapState},
		{config.GossipInterval, nDB.gossip},
		{config.PushPullInterval, nDB.bulkSyncTables},
		{retryInterval, nDB.reconnectNode},
		{nodeReapPeriod, nDB.reapDeadNode},
		{nDB.config.rejoinClusterInterval, nDB.rejoinClusterBootStrap},
	} {
		nDB.goTriggerFunc(nDB.ctx.Done(), trigger.interval, trigger.fn)
	}

	return nil
}

// retryJoin joins members, retrying every retryInterval until it succeeds or
// ctx is done. The first attempt is made straight away: waiting out a tick
// first would make the whole call a no-op whenever ctx expires within
// retryInterval, and it is the caller who chooses that budget.
func (nDB *NetworkDB) retryJoin(ctx context.Context, members []string) {
	t := time.NewTicker(retryInterval)
	defer t.Stop()

	for ctx.Err() == nil {
		if _, err := nDB.memberlist.Join(members); err != nil {
			log.G(ctx).Errorf("Failed to join memberlist %s on retry: %v", members, err)
		} else if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
			log.G(ctx).Errorf("failed to send node join on retry: %v", err)
		} else {
			return
		}

		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (nDB *NetworkDB) clusterJoin(members []string) error {
	mlist := nDB.memberlist

	if _, err := mlist.Join(members); err != nil {
		// No explicit retryJoin here: rejoinClusterBootStrap runs every
		// nDB.config.rejoinClusterInterval and will retryJoin for
		// nDB.config.rejoinClusterDuration. Note that it works from
		// Config.BootstrapPeers, not from members, so it only picks this
		// join up again if the caller also names these addresses there --
		// and a NetworkDB configured without BootstrapPeers never retries.
		return fmt.Errorf("could not join node to memberlist: %v", err)
	}

	if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
		return fmt.Errorf("failed to send node join: %v", err)
	}

	return nil
}

func (nDB *NetworkDB) clusterLeave() error {
	mlist := nDB.memberlist

	if err := nDB.sendNodeEvent(NodeEventTypeLeave); err != nil {
		log.G(context.TODO()).WithError(err).Error("failed to send node leave event")
	}

	if err := mlist.Leave(time.Second); err != nil {
		log.G(context.TODO()).WithError(err).Error("failed to broadcast memberlist leave message")
	}

	// cancel the context
	nDB.cancelCtx()

	return mlist.Shutdown()
}

// goTriggerFunc starts a background goroutine which calls f every interval
// until ch is closed. The first call is additionally delayed by a random
// stagger to avoid synchronizing with other nodes.
func (nDB *NetworkDB) goTriggerFunc(ch <-chan struct{}, interval time.Duration, f func()) {
	nDB.rngMu.Lock()
	randStagger := time.Duration(nDB.rng.Int64N(int64(interval))) // #nosec G404 -- use of math/rand/v2 is fine for this purpose.
	nDB.rngMu.Unlock()
	go func() {
		select {
		case <-time.After(randStagger):
		case <-ch:
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				f()
			case <-ch:
				return
			}
		}
	}()
}

func (nDB *NetworkDB) reapDeadNode() {
	nDB.Lock()
	defer nDB.Unlock()
	for id, n := range nDB.failedNodes {
		if n.reapTime > nodeReapPeriod {
			n.reapTime -= nodeReapPeriod
			continue
		}
		log.G(context.TODO()).Debugf("Garbage collect node %v", n.Name)
		// The node is not coming back: drop the attachments remembered for
		// it along with any entries reapTableEntries has not aged out yet.
		nDB.deleteNodeFromNetworks(n.Name)
		nDB.deleteNodeTableEntries(n.Name)
		delete(nDB.failedNodes, id)
	}
}

// bootstrapPeersToRejoin is the subset of peers which are not currently in the
// cluster, and so are worth rejoining. Ourselves is counted as in it: a
// bootstrap address which is our own is one we have no way to rejoin and no
// reason to, rather than an absent peer.
//
// An address which names no port is read as being on the port we gossip on,
// and not config.BindPort: that is only a request, left zero by every caller
// which is happy with memberlist's default. Addresses are compared unmapped,
// because memberlist knows its peers by whatever form their address arrived
// in, so a 4-in-6 address has to match the same address written as IPv4.
func (nDB *NetworkDB) bootstrapPeersToRejoin(peers []string) []string {
	nDB.RLock()
	defer nDB.RUnlock()

	myself, ok := nDB.nodes[nDB.config.NodeID]
	if !ok {
		log.G(context.TODO()).Warnf("rejoinClusterBootstrap unable to find local node info using ID:%v", nDB.config.NodeID)
		return nil
	}
	gossipPort := myself.Port

	joined := make(map[netip.AddrPort]struct{}, len(nDB.nodes))
	for _, node := range nDB.nodes {
		nodeIP, _ := netip.AddrFromSlice(node.Addr)
		joined[netip.AddrPortFrom(nodeIP.Unmap(), node.Port)] = struct{}{}
	}

	rejoin := make([]string, 0, len(peers))
	for _, bootIP := range peers {
		// bootstrap IPs are usually IP:port from the Join
		bootstrapIP, err := netip.ParseAddrPort(bootIP)
		if err != nil {
			// try to parse it as an IP without port
			// Note this seems to be the case for swarm that do not specify any port
			addr, err := netip.ParseAddr(bootIP)
			if err != nil {
				continue
			}
			// A peer is assumed to gossip on the same port we do.
			bootstrapIP = netip.AddrPortFrom(addr, gossipPort)
		}
		if _, ok := joined[netip.AddrPortFrom(bootstrapIP.Addr().Unmap(), bootstrapIP.Port())]; ok {
			// This bootstrap node is already part of the cluster, so it
			// needs no rejoining. The others still might.
			continue
		}
		rejoin = append(rejoin, bootIP)
	}
	return rejoin
}

// rejoinClusterBootStrap is called periodically to check if all bootStrap nodes are active in the cluster,
// if not, call the cluster join to merge 2 separate clusters that are formed when all managers
// stopped/started at the same time
//
// Each bootstrap address is considered on its own: being attached to one of
// them says nothing about the others, and memberlist.Join reports an error only
// when every address it was given fails, so a rejoin which reached part of the
// list still returns success. Only the addresses which are not already peers
// are worth rejoining, and they stay worth rejoining for as long as they are
// absent.
//
// That matters most for a partition which outlives node reaping. reconnectNode
// retries the peers it saw fail, but reapDeadNode forgets them after
// nodeReapInterval, leaving this function as the only thing which can still
// merge the two halves. With a bootstrap node left on each side, each half is
// attached to one bootstrap address throughout, so it has to keep trying the
// ones it cannot see or the split outlives the network fault which caused it.
func (nDB *NetworkDB) rejoinClusterBootStrap() {
	// Ask who the bootstrap nodes are before taking the lock: the answer comes
	// from the caller, whose own locks must not be taken beneath ours.
	if nDB.config.BootstrapPeers == nil {
		return
	}
	peers := nDB.config.BootstrapPeers()
	if len(peers) == 0 {
		return
	}

	bootStrapIPs := nDB.bootstrapPeersToRejoin(peers)
	if len(bootStrapIPs) == 0 {
		// Every bootstrap node is already a peer, or none of them parsed.
		// memberlist.Join reports success on an empty list, so without this
		// retryJoin would announce a join it never made.
		log.G(context.TODO()).Debug("rejoinClusterBootStrap has no bootstrap node to rejoin")
		return
	}
	// Some of the bootStrap nodes are not in the cluster, call memberlist join
	log.G(context.TODO()).Debugf("rejoinClusterBootStrap, calling cluster join with bootStrap %v", bootStrapIPs)
	ctx, cancel := context.WithTimeout(nDB.ctx, nDB.config.rejoinClusterDuration)
	defer cancel()
	nDB.retryJoin(ctx, bootStrapIPs)
}

func (nDB *NetworkDB) reconnectNode() {
	nDB.RLock()
	if len(nDB.failedNodes) == 0 {
		nDB.RUnlock()
		return
	}

	nodes := make([]*node, 0, len(nDB.failedNodes))
	for _, n := range nDB.failedNodes {
		nodes = append(nodes, n)
	}
	nDB.RUnlock()

	// Built by ranging failedNodes, so unsorted the same offset names a
	// different node on each run.
	slices.SortFunc(nodes, func(a, b *node) int { return strings.Compare(a.Name, b.Name) })

	nDB.rngMu.Lock()
	offset := nDB.rng.IntN(len(nodes))
	nDB.rngMu.Unlock()
	node := nodes[offset]
	addr := net.UDPAddr{IP: node.Addr, Port: int(node.Port)}

	if _, err := nDB.memberlist.Join([]string{addr.String()}); err != nil {
		return
	}

	if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
		return
	}

	log.G(context.TODO()).Debugf("Initiating bulk sync with node %s after reconnect", node.Name)
	nDB.bulkSync([]string{node.Name}, true)
}

// For timing the entry deletion in the reaper APIs that doesn't use monotonic clock
// source (time.Now, Sub etc.) should be avoided. Hence we use reapTime in every
// entry which is set initially to reapInterval and decremented by reapPeriod every time
// the reaper runs. NOTE reapTime is only written while holding the NetworkDB write
// lock — by the reapers and by the paths which suspend and restore the entries of a
// failed node — and only read while holding at least the read lock.
func (nDB *NetworkDB) reapState() {
	// The reapTableEntries leverage the presence of the network so garbage collect entries first
	nDB.reapTableEntries()
	nDB.reapNetworks()
}

func (nDB *NetworkDB) reapNetworks() {
	nDB.Lock()
	for id, n := range nDB.thisNodeNetworks {
		if n.leaving {
			if n.reapTime <= 0 {
				delete(nDB.thisNodeNetworks, id)
				continue
			}
			n.reapTime -= reapPeriod
		}
	}
	for _, nn := range nDB.networks {
		for id, n := range nn {
			if n.leaving {
				if n.reapTime <= 0 {
					delete(nn, id)
					continue
				}
				n.reapTime -= reapPeriod
			}
		}
	}
	nDB.Unlock()
}

func (nDB *NetworkDB) reapTableEntries() {
	var nodeNetworks []string
	// This is best effort, if the list of network changes will be picked up in the next cycle
	nDB.RLock()
	for nid := range nDB.thisNodeNetworks {
		nodeNetworks = append(nodeNetworks, nid)
	}
	nDB.RUnlock()

	cycleStart := time.Now()
	// In order to avoid blocking the database for a long time, apply the garbage collection logic by network
	// The lock is taken at the beginning of the cycle and the deletion is inline
	for _, nid := range nodeNetworks {
		nDB.Lock()
		nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, v *entry) bool {
			// timeCompensation compensate in case the lock took some time to be released
			timeCompensation := time.Since(cycleStart)
			// Entries remembered for a failed owner age out like tombstones,
			// so that they cannot be restored after whatever would delete
			// them expired.
			if !v.deleting && !nDB.isNodeFailed(v.node) {
				return false
			}

			// In this check we are adding an extra 1 second to guarantee that when the number is truncated to int32 to fit the packet
			// for the tableEvent the number is always strictly > 1 and never 0
			if v.reapTime > reapPeriod+timeCompensation+time.Second {
				v.reapTime -= reapPeriod + timeCompensation
				return false
			}

			params := strings.Split(string(path[1:]), "/")
			nwID, tName, key := params[0], params[1], params[2]
			okTable, okNetwork := nDB.deleteEntry(nwID, tName, key)
			if !okTable {
				log.G(context.TODO()).Errorf("Table tree delete failed, entry with key:%s does not exist in the table:%s network:%s", key, tName, nwID)
			}
			if !okNetwork {
				log.G(context.TODO()).Errorf("Network tree delete failed, entry with key:%s does not exist in the network:%s table:%s", key, nwID, tName)
			}

			return false
		})
		nDB.Unlock()
	}
}

func (nDB *NetworkDB) gossip() {
	networkNodes := make(map[string][]string)
	nDB.RLock()
	for nid := range nDB.thisNodeNetworks {
		networkNodes[nid] = nDB.networkNodes[nid]
	}
	printStats := time.Since(nDB.lastStatsTimestamp) >= nDB.config.StatsPrintPeriod
	printHealth := time.Since(nDB.lastHealthTimestamp) >= nDB.config.HealthPrintPeriod
	nDB.RUnlock()

	if printHealth {
		healthScore := nDB.memberlist.GetHealthScore()
		if healthScore != 0 {
			log.G(context.TODO()).Warnf("NetworkDB stats %v(%v) - healthscore:%d (connectivity issues)", nDB.config.Hostname, nDB.config.NodeID, healthScore)
		}
		nDB.lastHealthTimestamp = time.Now()
	}

	// Sorted: the draws mRandomNodes makes below are consumed in visitation
	// order, so ranging the map would hand a given network a different draw on
	// each run.
	for _, nid := range slices.Sorted(maps.Keys(networkNodes)) {
		nodes := networkNodes[nid]
		mNodes := nDB.mRandomNodes(3, nodes)
		bytesAvail := nDB.config.PacketBufferSize - compoundHeaderOverhead

		nDB.RLock()
		network, ok := nDB.thisNodeNetworks[nid]
		// Read leaving while still holding the lock: it is mutated under
		// the write lock by (*NetworkDB).LeaveNetwork.
		leaving := ok && network.leaving
		nDB.RUnlock()
		if !ok || network == nil {
			// It is normal for the network to be removed
			// between the time we collect the network
			// attachments of this node and processing
			// them here.
			continue
		}

		msgs := getBroadcasts(compoundOverhead, bytesAvail, network.tableBroadcasts, network.tableRebroadcasts)
		// Collect stats and print the queue info, note this code is here also to have a view of the queues empty
		network.qMessagesSent.Add(int64(len(msgs)))
		if printStats {
			msent := network.qMessagesSent.Swap(0)
			log.G(context.TODO()).Infof("NetworkDB stats %v(%v) - netID:%s leaving:%t netPeers:%d entries:%d Queue qLen:%d+%d netMsg/s:%d",
				nDB.config.Hostname, nDB.config.NodeID,
				nid, leaving, network.tableBroadcasts.NumNodes(), network.entriesNumber.Load(),
				network.tableBroadcasts.NumQueued(), network.tableRebroadcasts.NumQueued(),
				msent/int64((nDB.config.StatsPrintPeriod/time.Second)))
		}

		if len(msgs) == 0 {
			continue
		}

		// Create a compound message
		compound := makeCompoundMessage(msgs)

		for _, node := range mNodes {
			nDB.RLock()
			mnode := nDB.nodes[node]
			nDB.RUnlock()

			if mnode == nil {
				// The node stopped being an active peer since it was
				// picked, therefore skip it.
				continue
			}

			// Send the compound message
			if err := nDB.memberlist.SendBestEffort(&mnode.Node, compound); err != nil {
				log.G(context.TODO()).Errorf("Failed to send gossip to %s: %s", mnode.Addr, err)
			}
		}
	}
	// Reset the stats
	if printStats {
		nDB.lastStatsTimestamp = time.Now()
	}
}

func (nDB *NetworkDB) bulkSyncTables() {
	var networks []string
	nDB.RLock()
	for nid, network := range nDB.thisNodeNetworks {
		if network.leaving {
			continue
		}
		networks = append(networks, nid)
	}
	nDB.RUnlock()
	// Sorted for a canonical starting point -- ranging thisNodeNetworks would
	// otherwise hand each network a different bulk-sync draw on each run -- and
	// then permuted, because the order this loop visits networks in has to keep
	// varying between cycles.
	//
	// bulkSync returns every network it found in common with the peer it chose,
	// and the loop below strikes all of them off. A network which is always
	// visited after one it overlaps with is therefore always struck off before it
	// selects a peer of its own, and never syncs with the members it does not
	// share that first network with. Two groups overlapping on one network can
	// then stay partitioned for good: anti-entropy never crosses between them.
	slices.Sort(networks)
	nDB.rngMu.Lock()
	nDB.rng.Shuffle(len(networks), func(i, j int) {
		networks[i], networks[j] = networks[j], networks[i]
	})
	nDB.rngMu.Unlock()

	for len(networks) != 0 {
		nid := networks[0]
		networks = networks[1:]

		nDB.RLock()
		nodes := nDB.networkNodes[nid]
		nDB.RUnlock()

		// No peer nodes on this network. Move on.
		if len(nodes) == 0 {
			continue
		}

		completed, err := nDB.bulkSync(nodes, false)
		if err != nil {
			log.G(context.TODO()).Errorf("periodic bulk sync failure for network %s: %v", nid, err)
			continue
		}

		// Remove all the networks for which we have
		// successfully completed bulk sync in this iteration.
		updatedNetworks := make([]string, 0, len(networks))
		for _, nid := range networks {
			if !slices.Contains(completed, nid) {
				updatedNetworks = append(updatedNetworks, nid)
			}
		}

		networks = updatedNetworks
	}
}

func (nDB *NetworkDB) bulkSync(nodes []string, all bool) ([]string, error) {
	if !all {
		// Get 2 random nodes. 2nd node will be tried if the bulk sync to
		// 1st node fails.
		nodes = nDB.mRandomNodes(2, nodes)
	}

	if len(nodes) == 0 {
		return nil, nil
	}

	var err error
	var networks []string
	var success bool
	for _, node := range nodes {
		if node == nDB.config.NodeID {
			continue
		}
		log.G(context.TODO()).Debugf("%v(%v): Initiating bulk sync with node %v", nDB.config.Hostname, nDB.config.NodeID, node)
		networks = nDB.findCommonNetworks(node)
		err = nDB.bulkSyncNode(networks, node, true)
		if err != nil {
			err = fmt.Errorf("bulk sync to node %s failed: %v", node, err)
			log.G(context.TODO()).Warn(err.Error())
		} else {
			// bulk sync succeeded
			success = true
			// if its periodic bulksync stop after the first successful sync
			if !all {
				break
			}
		}
	}

	if success {
		// if at least one node sync succeeded
		return networks, nil
	}

	return nil, err
}

// Bulk sync all the table entries belonging to a set of networks to a
// single peer node. It can be unsolicited or can be in response to an
// unsolicited bulk sync
func (nDB *NetworkDB) bulkSyncNode(networks []string, node string, unsolicited bool) (retErr error) {
	var msgs [][]byte

	var unsolMsg string
	if unsolicited {
		unsolMsg = "unsolicited"
	}

	log.G(context.TODO()).Debugf("%v(%v): Initiating %s bulk sync for networks %v with node %s",
		nDB.config.Hostname, nDB.config.NodeID, unsolMsg, networks, node)

	nDB.RLock()
	mnode := nDB.nodes[node]
	if mnode == nil {
		// The node is not an active peer: it either failed since it was
		// picked from a peer list, or it sent us a bulk sync while
		// memberlist considers it failed. Sync at the address last known
		// for it anyway: it evidently could reach us, and it is waiting for
		// our reply if the bulk sync we are answering was unsolicited.
		mnode = nDB.failedNodes[node]
	}
	if mnode == nil {
		nDB.RUnlock()
		// Nothing is known about the node, so it cannot be reached. Report
		// it so that the caller does not count this as a completed sync.
		return fmt.Errorf("cannot bulk sync with unknown node %s", node)
	}

	// Carry the attachment view for the networks being synced, ahead of the
	// entries which depend on it, for any peer which can take it.
	if mnode.canReceiveNetworkEvents() {
		msgs = nDB.appendNetworkEvents(msgs, networks, node)
	}

	for _, nid := range networks {
		nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, v *entry) bool {
			eType := TableEventTypeCreate
			if v.deleting {
				eType = TableEventTypeDelete
			}

			params := strings.Split(string(path[1:]), "/")
			tEvent := TableEvent{
				Type:      eType,
				LTime:     v.ltime,
				NodeName:  v.node,
				NetworkID: nid,
				TableName: params[1],
				Key:       params[2],
				Value:     v.value,
				// The duration in second is a float that below would be truncated
				ResidualReapTime: int32(v.reapTime.Seconds()),
			}

			msg, err := encodeMessage(MessageTypeTableEvent, &tEvent)
			if err != nil {
				log.G(context.TODO()).WithError(err).Errorf("Encode failure during bulk sync: %#v", tEvent)
				return false
			}

			msgs = append(msgs, msg)
			return false
		})
	}
	nDB.RUnlock()

	// Create a compound message
	compound := makeCompoundMessage(msgs)

	bsm := BulkSyncMessage{
		LTime:       nDB.tableClock.Increment(),
		Unsolicited: unsolicited,
		NodeName:    nDB.config.NodeID,
		Networks:    networks,
		Payload:     compound,
	}

	buf, err := encodeMessage(MessageTypeBulkSync, &bsm)
	if err != nil {
		return fmt.Errorf("failed to encode bulk sync message: %v", err)
	}

	// Wait on a response only if we are sending an unsolicited bulk sync,
	// as only unsolicited bulk syncs trigger a response.
	if unsolicited {
		nDB.Lock()
		ch := make(chan struct{})
		nDB.bulkSyncAckTbl[node] = append(nDB.bulkSyncAckTbl[node], bulkSyncSubscription{
			LTime: bsm.LTime,
			Done:  ch,
		})
		nDB.Unlock()

		unsubscribe := func() {
			nDB.Lock()
			defer nDB.Unlock()
			subscriptions := nDB.bulkSyncAckTbl[node]
			for i, sub := range subscriptions {
				if sub.Done == ch {
					subscriptions = slices.Delete(subscriptions, i, i+1)
					break
				}
			}
			if len(subscriptions) > 0 {
				nDB.bulkSyncAckTbl[node] = subscriptions
			} else {
				delete(nDB.bulkSyncAckTbl, node)
			}
		}

		defer func() {
			if retErr != nil {
				unsubscribe()
				return
			}

			startTime := time.Now()
			t := time.NewTimer(30 * time.Second)
			select {
			case <-t.C:
				log.G(context.TODO()).Errorf("Bulk sync to node %s timed out", node)
				nDB.bulkSyncAckTimeouts.Add(1)
				unsubscribe()
			case <-ch:
				log.G(context.TODO()).Debugf("%v(%v): Bulk sync to node %s took %s", nDB.config.Hostname, nDB.config.NodeID, node, time.Since(startTime))
			}
			t.Stop()
		}()
	}

	err = nDB.memberlist.SendReliable(&mnode.Node, buf)
	if err != nil {
		return fmt.Errorf("failed to send a TCP message during bulk sync: %v", err)
	}
	return nil
}

// mRandomNodes is used to select up to m random nodes. It is possible
// that less than m nodes are returned.
func (nDB *NetworkDB) mRandomNodes(m int, nodes []string) []string {
	mNodes := make([]string, 0, max(0, len(nodes)-1))
	for _, node := range nodes {
		if node == nDB.config.NodeID {
			// Skip myself
			continue
		}
		mNodes = append(mNodes, node)
	}

	// networkNodes is in the order attachments were learned, which follows
	// gossip arrival and so differs between nodes and between runs. The draws
	// below select by index, so without a total order the same draw picks a
	// different peer. Sorting a copy -- mNodes already is one -- makes peer
	// selection a function of the RNG alone. Both callers reach the RNG through
	// here, so this is the one place it has to happen.
	slices.Sort(mNodes)

	if len(mNodes) < m {
		nDB.rngMu.Lock()
		nDB.rng.Shuffle(len(mNodes), func(i, j int) {
			mNodes[i], mNodes[j] = mNodes[j], mNodes[i]
		})
		nDB.rngMu.Unlock()
		return mNodes
	}

	nDB.rngMu.Lock()
	perm := nDB.rng.Perm(len(mNodes))
	nDB.rngMu.Unlock()

	sample := make([]string, 0, m)
	for _, idx := range perm[:m] {
		sample = append(sample, mNodes[idx])
	}

	return sample
}

// appendNetworkEvents appends the attachment view for networks: which nodes this
// one believes participate in each of them. Call with nDB's read lock held.
//
// handleTableEvent will not accept an entry from a node it does not believe
// participates in the network, and attachments are otherwise disseminated only
// by one-shot NetworkEvent broadcasts with a bounded number of retransmits. A
// node which misses those has no way back: a bulk sync hands it the entries
// every round and it rejects every one of them, for as long as its view stays
// stale. Sending the attachments those entries are predicated on makes the sync
// self-sufficient, so anti-entropy converges the membership it needs rather
// than assuming it.
//
// The volume is bounded by the membership of the networks being synced, not by
// cluster history: changeNodeState drops a peer's attachments as soon as it goes
// failed or left, and reapNetworks collects those marked leaving, so nDB.networks
// holds live peers plus a short tail of departures.
//
// Departures are included on purpose. A node which missed a Leave is the mirror
// image of the case above -- it holds a departed owner in networkNodes and keeps
// accepting and serving its entries -- and handleNetworkEvent's Leave path is
// what clears that. Filtering on !n.leaving here would trade one stale view for
// the other.
//
// These are ordinary network events, so handleNetworkEvent applies its usual
// Lamport-time check and a receiver which is already up to date does nothing.
func (nDB *NetworkDB) appendNetworkEvents(msgs [][]byte, networks []string, node string) [][]byte {
	for _, nid := range networks {
		if n, ok := nDB.thisNodeNetworks[nid]; ok {
			msgs = appendNetworkEventMsg(msgs, nDB.config.NodeID, nid, n.network)
		}
	}
	for owner, nws := range nDB.networks {
		if owner == node {
			// The peer is authoritative for its own attachments, so
			// handleNetworkEvent drops anything we tell it about itself. Skipping
			// it is not just a saving: findCommonNetworks only returns networks
			// this node records the peer as attached to, so without this every
			// network in the sync would carry one event guaranteed to be
			// discarded -- after the receiver had taken the write lock to
			// discard it.
			continue
		}
		for _, nid := range networks {
			if n, ok := nws[nid]; ok {
				msgs = appendNetworkEventMsg(msgs, owner, nid, *n)
			}
		}
	}
	return msgs
}

// appendNetworkEventMsg appends an encoded NetworkEvent describing one node's
// attachment to a network. Used to make a bulk sync carry the membership its
// table entries are predicated on.
func appendNetworkEventMsg(msgs [][]byte, nodeName, nid string, n network) [][]byte {
	nEvent := NetworkEvent{
		Type:      networkEventType(n.leaving),
		LTime:     n.ltime,
		NodeName:  nodeName,
		NetworkID: nid,
	}

	msg, err := encodeMessage(MessageTypeNetworkEvent, &nEvent)
	if err != nil {
		log.G(context.TODO()).WithError(err).Errorf("Encode failure during bulk sync: %#v", nEvent)
		return msgs
	}
	return append(msgs, msg)
}

package networkdb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	rnd "math/rand"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/sirupsen/logrus"
)

const (
	reapPeriod            = 5 * time.Second
	rejoinClusterDuration = 10 * time.Second
	rejoinInterval        = 60 * time.Second
	retryInterval         = 1 * time.Second
	nodeReapInterval      = 24 * time.Hour
	nodeReapPeriod        = 2 * time.Hour
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
		logrus.Warn(str)
	case strings.HasPrefix(str, "[DEBUG] "):
		str = strings.TrimPrefix(str, "[DEBUG] ")
		logrus.Debug(str)
	case strings.HasPrefix(str, "[INFO] "):
		str = strings.TrimPrefix(str, "[INFO] ")
		logrus.Info(str)
	case strings.HasPrefix(str, "[ERR] "):
		str = strings.TrimPrefix(str, "[ERR] ")
		logrus.Warn(str)
	}

	return len(p), nil
}

// SetKey adds a new key to the key ring
func (nDB *NetworkDB) SetKey(key []byte) {
	logrus.Debugf("Adding key %.5s", hex.EncodeToString(key))
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
}

// SetPrimaryKey sets the given key as the primary key. This should have
// been added apriori through SetKey
func (nDB *NetworkDB) SetPrimaryKey(key []byte) {
	logrus.Debugf("Primary Key %.5s", hex.EncodeToString(key))
	nDB.RLock()
	defer nDB.RUnlock()
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
	logrus.Debugf("Remove Key %.5s", hex.EncodeToString(key))
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

	if nDB.config.BindPort != 0 {
		config.BindPort = nDB.config.BindPort
	}

	config.ProtocolVersion = memberlist.ProtocolVersion2Compatible
	config.Delegate = &delegate{nDB: nDB}
	config.Events = &eventDelegate{nDB: nDB}
	// custom logger that does not add time or date, so they are not
	// duplicated by logrus
	config.Logger = log.New(&logWriter{}, "", 0)

	var err error
	if len(nDB.config.Keys) > 0 {
		for i, key := range nDB.config.Keys {
			logrus.Debugf("Encryption key %d: %.5s", i+1, hex.EncodeToString(key))
		}
		nDB.keyring, err = memberlist.NewKeyring(nDB.config.Keys, nDB.config.Keys[0])
		if err != nil {
			return err
		}
		config.Keyring = nDB.keyring
	}

	nDB.networkBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes: func() int {
			nDB.RLock()
			num := len(nDB.nodes)
			nDB.RUnlock()
			return num
		},
		RetransmitMult: config.RetransmitMult,
	}

	nDB.nodeBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes: func() int {
			nDB.RLock()
			num := len(nDB.nodes)
			nDB.RUnlock()
			return num
		},
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
		{rejoinInterval, nDB.rejoinClusterBootStrap},
	} {
		t := time.NewTicker(trigger.interval)
		go nDB.triggerFunc(trigger.interval, t.C, trigger.fn)
		nDB.tickers = append(nDB.tickers, t)
	}

	return nil
}

func (nDB *NetworkDB) retryJoin(ctx context.Context, members []string) {
	t := time.NewTicker(retryInterval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if _, err := nDB.memberlist.Join(members); err != nil {
				logrus.Errorf("Failed to join memberlist %s on retry: %v", members, err)
				continue
			}
			if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
				logrus.Errorf("failed to send node join on retry: %v", err)
				continue
			}
			return
		case <-ctx.Done():
			return
		}
	}

}

func (nDB *NetworkDB) clusterJoin(members []string) error {
	mlist := nDB.memberlist

	if _, err := mlist.Join(members); err != nil {
		// In case of failure, we no longer need to explicitly call retryJoin.
		// rejoinClusterBootStrap, which runs every minute, will retryJoin for 10sec
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
		logrus.Errorf("failed to send node leave: %v", err)
	}

	if err := mlist.Leave(time.Second); err != nil {
		return err
	}

	// cancel the context
	nDB.cancelCtx()

	for _, t := range nDB.tickers {
		t.Stop()
	}

	return mlist.Shutdown()
}

func (nDB *NetworkDB) triggerFunc(stagger time.Duration, C <-chan time.Time, f func()) {
	// Use a random stagger to avoid synchronizing
	randStagger := time.Duration(uint64(rnd.Int63()) % uint64(stagger))
	select {
	case <-time.After(randStagger):
	case <-nDB.ctx.Done():
		return
	}
	for {
		select {
		case <-C:
			f()
		case <-nDB.ctx.Done():
			return
		}
	}
}

func (nDB *NetworkDB) reapDeadNode() {
	nDB.Lock()
	defer nDB.Unlock()
	for _, nodeMap := range []map[string]*node{
		nDB.failedNodes,
		nDB.leftNodes,
	} {
		for id, n := range nodeMap {
			if n.reapTime > nodeReapPeriod {
				n.reapTime -= nodeReapPeriod
				continue
			}
			logrus.Debugf("Garbage collect node %v", n.Name)
			delete(nodeMap, id)
		}
	}
}

// rejoinClusterBootStrap is called periodically to check if all bootStrap nodes are active in the cluster,
// if not, call the cluster join to merge 2 separate clusters that are formed when all managers
// stopped/started at the same time
func (nDB *NetworkDB) rejoinClusterBootStrap() {
	nDB.RLock()
	if len(nDB.bootStrapIP) == 0 {
		nDB.RUnlock()
		return
	}

	myself, ok := nDB.nodes[nDB.config.NodeID]
	if !ok {
		nDB.RUnlock()
		logrus.Warnf("rejoinClusterBootstrap unable to find local node info using ID:%v", nDB.config.NodeID)
		return
	}
	bootStrapIPs := make([]string, 0, len(nDB.bootStrapIP))
	for _, bootIP := range nDB.bootStrapIP {
		// botostrap IPs are usually IP:port from the Join
		var bootstrapIP net.IP
		ipStr, _, err := net.SplitHostPort(bootIP)
		if err != nil {
			// try to parse it as an IP with port
			// Note this seems to be the case for swarm that do not specify any port
			ipStr = bootIP
		}
		bootstrapIP = net.ParseIP(ipStr)
		if bootstrapIP != nil {
			for _, node := range nDB.nodes {
				if node.Addr.Equal(bootstrapIP) && !node.Addr.Equal(myself.Addr) {
					// One of the bootstrap nodes (and not myself) is part of the cluster, return
					nDB.RUnlock()
					return
				}
			}
			bootStrapIPs = append(bootStrapIPs, bootIP)
		}
	}
	nDB.RUnlock()
	if len(bootStrapIPs) == 0 {
		// this will also avoid to call the Join with an empty list erasing the current bootstrap ip list
		logrus.Debug("rejoinClusterBootStrap did not find any valid IP")
		return
	}
	// None of the bootStrap nodes are in the cluster, call memberlist join
	logrus.Debugf("rejoinClusterBootStrap, calling cluster join with bootStrap %v", bootStrapIPs)
	ctx, cancel := context.WithTimeout(nDB.ctx, rejoinClusterDuration)
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

	node := nodes[randomOffset(len(nodes))]
	addr := net.UDPAddr{IP: node.Addr, Port: int(node.Port)}

	if _, err := nDB.memberlist.Join([]string{addr.String()}); err != nil {
		return
	}

	if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
		return
	}

	logrus.Debugf("Initiating bulk sync with node %s after reconnect", node.Name)
	nDB.bulkSync([]string{node.Name}, true)
}

// For timing the entry deletion in the reaper APIs that doesn't use monotonic clock
// source (time.Now, Sub etc.) should be avoided. Hence we use reapTime in every
// entry which is set initially to reapInterval and decremented by reapPeriod every time
// the reaper runs. NOTE nDB.reapTableEntries updates the reapTime with a readlock. This
// is safe as long as no other concurrent path touches the reapTime field.
func (nDB *NetworkDB) reapState() {
	// The reapTableEntries leverage the presence of the network so garbage collect entries first
	nDB.reapTableEntries()
	nDB.reapNetworks()
}

func (nDB *NetworkDB) reapNetworks() {
	nDB.Lock()
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
	for nid := range nDB.networks[nDB.config.NodeID] {
		nodeNetworks = append(nodeNetworks, nid)
	}
	nDB.RUnlock()

	cycleStart := time.Now()
	// In order to avoid blocking the database for a long time, apply the garbage collection logic by network
	// The lock is taken at the beginning of the cycle and the deletion is inline
	for _, nid := range nodeNetworks {
		nDB.Lock()
		nDB.indexes[byNetwork].WalkPrefix(fmt.Sprintf("/%s", nid), func(path string, v interface{}) bool {
			// timeCompensation compensate in case the lock took some time to be released
			timeCompensation := time.Since(cycleStart)
			entry, ok := v.(*entry)
			if !ok || !entry.deleting {
				return false
			}

			// In this check we are adding an extra 1 second to guarantee that when the number is truncated to int32 to fit the packet
			// for the tableEvent the number is always strictly > 1 and never 0
			if entry.reapTime > reapPeriod+timeCompensation+time.Second {
				entry.reapTime -= reapPeriod + timeCompensation
				return false
			}

			params := strings.Split(path[1:], "/")
			nid := params[0]
			tname := params[1]
			key := params[2]

			okTable, okNetwork := nDB.deleteEntry(nid, tname, key)
			if !okTable {
				logrus.Errorf("Table tree delete failed, entry with key:%s does not exists in the table:%s network:%s", key, tname, nid)
			}
			if !okNetwork {
				logrus.Errorf("Network tree delete failed, entry with key:%s does not exists in the network:%s table:%s", key, nid, tname)
			}

			return false
		})
		nDB.Unlock()
	}
}

func (nDB *NetworkDB) gossip() {
	networkNodes := make(map[string][]string)
	nDB.RLock()
	thisNodeNetworks := nDB.networks[nDB.config.NodeID]
	for nid := range thisNodeNetworks {
		networkNodes[nid] = nDB.networkNodes[nid]
	}
	printStats := time.Since(nDB.lastStatsTimestamp) >= nDB.config.StatsPrintPeriod
	printHealth := time.Since(nDB.lastHealthTimestamp) >= nDB.config.HealthPrintPeriod
	nDB.RUnlock()

	if printHealth {
		healthScore := nDB.memberlist.GetHealthScore()
		if healthScore != 0 {
			logrus.Warnf("NetworkDB stats %v(%v) - healthscore:%d (connectivity issues)", nDB.config.Hostname, nDB.config.NodeID, healthScore)
		}
		nDB.lastHealthTimestamp = time.Now()
	}

	for nid, nodes := range networkNodes {
		mNodes := nDB.mRandomNodes(3, nodes)
		bytesAvail := nDB.config.PacketBufferSize - compoundHeaderOverhead

		nDB.RLock()
		network, ok := thisNodeNetworks[nid]
		nDB.RUnlock()
		if !ok || network == nil {
			// It is normal for the network to be removed
			// between the time we collect the network
			// attachments of this node and processing
			// them here.
			continue
		}

		broadcastQ := network.tableBroadcasts

		if broadcastQ == nil {
			logrus.Errorf("Invalid broadcastQ encountered while gossiping for network %s", nid)
			continue
		}

		msgs := broadcastQ.GetBroadcasts(compoundOverhead, bytesAvail)
		// Collect stats and print the queue info, note this code is here also to have a view of the queues empty
		network.qMessagesSent += len(msgs)
		if printStats {
			logrus.Infof("NetworkDB stats %v(%v) - netID:%s leaving:%t netPeers:%d entries:%d Queue qLen:%d netMsg/s:%d",
				nDB.config.Hostname, nDB.config.NodeID,
				nid, network.leaving, broadcastQ.NumNodes(), network.entriesNumber, broadcastQ.NumQueued(),
				network.qMessagesSent/int((nDB.config.StatsPrintPeriod/time.Second)))
			network.qMessagesSent = 0
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
				break
			}

			// Send the compound message
			if err := nDB.memberlist.SendBestEffort(&mnode.Node, compound); err != nil {
				logrus.Errorf("Failed to send gossip to %s: %s", mnode.Addr, err)
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
	for nid, network := range nDB.networks[nDB.config.NodeID] {
		if network.leaving {
			continue
		}
		networks = append(networks, nid)
	}
	nDB.RUnlock()

	for {
		if len(networks) == 0 {
			break
		}

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
			logrus.Errorf("periodic bulk sync failure for network %s: %v", nid, err)
			continue
		}

		// Remove all the networks for which we have
		// successfully completed bulk sync in this iteration.
		updatedNetworks := make([]string, 0, len(networks))
		for _, nid := range networks {
			var found bool
			for _, completedNid := range completed {
				if nid == completedNid {
					found = true
					break
				}
			}

			if !found {
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
		logrus.Debugf("%v(%v): Initiating bulk sync with node %v", nDB.config.Hostname, nDB.config.NodeID, node)
		networks = nDB.findCommonNetworks(node)
		err = nDB.bulkSyncNode(networks, node, true)
		if err != nil {
			err = fmt.Errorf("bulk sync to node %s failed: %v", node, err)
			logrus.Warn(err.Error())
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
func (nDB *NetworkDB) bulkSyncNode(networks []string, node string, unsolicited bool) error {
	var msgs [][]byte

	var unsolMsg string
	if unsolicited {
		unsolMsg = "unsolicited"
	}

	logrus.Debugf("%v(%v): Initiating %s bulk sync for networks %v with node %s",
		nDB.config.Hostname, nDB.config.NodeID, unsolMsg, networks, node)

	nDB.RLock()
	mnode := nDB.nodes[node]
	if mnode == nil {
		nDB.RUnlock()
		return nil
	}

	for _, nid := range networks {
		nDB.indexes[byNetwork].WalkPrefix(fmt.Sprintf("/%s", nid), func(path string, v interface{}) bool {
			entry, ok := v.(*entry)
			if !ok {
				return false
			}

			eType := TableEventTypeCreate
			if entry.deleting {
				eType = TableEventTypeDelete
			}

			params := strings.Split(path[1:], "/")
			tEvent := TableEvent{
				Type:      eType,
				LTime:     entry.ltime,
				NodeName:  entry.node,
				NetworkID: nid,
				TableName: params[1],
				Key:       params[2],
				Value:     entry.value,
				// The duration in second is a float that below would be truncated
				ResidualReapTime: int32(entry.reapTime.Seconds()),
			}

			msg, err := encodeMessage(MessageTypeTableEvent, &tEvent)
			if err != nil {
				logrus.Errorf("Encode failure during bulk sync: %#v", tEvent)
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
		LTime:       nDB.tableClock.Time(),
		Unsolicited: unsolicited,
		NodeName:    nDB.config.NodeID,
		Networks:    networks,
		Payload:     compound,
	}

	buf, err := encodeMessage(MessageTypeBulkSync, &bsm)
	if err != nil {
		return fmt.Errorf("failed to encode bulk sync message: %v", err)
	}

	nDB.Lock()
	ch := make(chan struct{})
	nDB.bulkSyncAckTbl[node] = ch
	nDB.Unlock()

	err = nDB.memberlist.SendReliable(&mnode.Node, buf)
	if err != nil {
		nDB.Lock()
		delete(nDB.bulkSyncAckTbl, node)
		nDB.Unlock()

		return fmt.Errorf("failed to send a TCP message during bulk sync: %v", err)
	}

	// Wait on a response only if it is unsolicited.
	if unsolicited {
		startTime := time.Now()
		t := time.NewTimer(30 * time.Second)
		select {
		case <-t.C:
			logrus.Errorf("Bulk sync to node %s timed out", node)
		case <-ch:
			logrus.Debugf("%v(%v): Bulk sync to node %s took %s", nDB.config.Hostname, nDB.config.NodeID, node, time.Since(startTime))
		}
		t.Stop()
	}

	return nil
}

// Returns a random offset between 0 and n
func randomOffset(n int) int {
	if n == 0 {
		return 0
	}

	val, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		logrus.Errorf("Failed to get a random offset: %v", err)
		return 0
	}

	return int(val.Int64())
}

// mRandomNodes is used to select up to m random nodes. It is possible
// that less than m nodes are returned.
func (nDB *NetworkDB) mRandomNodes(m int, nodes []string) []string {
	n := len(nodes)
	mNodes := make([]string, 0, m)
OUTER:
	// Probe up to 3*n times, with large n this is not necessary
	// since k << n, but with small n we want search to be
	// exhaustive
	for i := 0; i < 3*n && len(mNodes) < m; i++ {
		// Get random node
		idx := randomOffset(n)
		node := nodes[idx]

		if node == nDB.config.NodeID {
			continue
		}

		// Check if we have this node already
		for j := 0; j < len(mNodes); j++ {
			if node == mNodes[j] {
				continue OUTER
			}
		}

		// Append the node
		mNodes = append(mNodes, node)
	}

	return mNodes
}

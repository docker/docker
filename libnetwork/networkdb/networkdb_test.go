package networkdb

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/go-events"
	"github.com/hashicorp/memberlist"
	"github.com/sirupsen/logrus"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"

	// this takes care of the incontainer flag
	_ "github.com/docker/docker/libnetwork/testutils"
)

var dbPort int32 = 10000

func TestMain(m *testing.M) {
	ioutil.WriteFile("/proc/sys/net/ipv6/conf/lo/disable_ipv6", []byte{'0', '\n'}, 0644)
	logrus.SetLevel(logrus.ErrorLevel)
	os.Exit(m.Run())
}

func launchNode(t *testing.T, conf Config) *NetworkDB {
	t.Helper()
	db, err := New(&conf)
	assert.NilError(t, err)
	return db
}

func createNetworkDBInstances(t *testing.T, num int, namePrefix string, conf *Config) []*NetworkDB {
	t.Helper()
	var dbs []*NetworkDB
	for i := 0; i < num; i++ {
		localConfig := *conf
		localConfig.Hostname = fmt.Sprintf("%s%d", namePrefix, i+1)
		localConfig.NodeID = stringid.TruncateID(stringid.GenerateRandomID())
		localConfig.BindPort = int(atomic.AddInt32(&dbPort, 1))
		db := launchNode(t, localConfig)
		if i != 0 {
			assert.Check(t, db.Join([]string{fmt.Sprintf("localhost:%d", db.config.BindPort-1)}))
		}

		dbs = append(dbs, db)
	}

	// Wait till the cluster creation is successful
	check := func(t poll.LogT) poll.Result {
		// Check that the cluster is properly created
		for i := 0; i < num; i++ {
			if num != len(dbs[i].ClusterPeers()) {
				return poll.Continue("%s:Waiting for cluser peers to be established", dbs[i].config.Hostname)
			}
		}
		return poll.Success()
	}
	poll.WaitOn(t, check, poll.WithDelay(2*time.Second), poll.WithTimeout(20*time.Second))

	return dbs
}

func closeNetworkDBInstances(t *testing.T, dbs []*NetworkDB) {
	t.Helper()
	log.Print("Closing DB instances...")
	for _, db := range dbs {
		db.Close()
	}
}

func (db *NetworkDB) verifyNodeExistence(t *testing.T, node string, present bool) {
	t.Helper()
	for i := 0; i < 80; i++ {
		db.RLock()
		_, ok := db.nodes[node]
		db.RUnlock()
		if present && ok {
			return
		}

		if !present && !ok {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("%v(%v): Node existence verification for node %s failed", db.config.Hostname, db.config.NodeID, node)
}

func (db *NetworkDB) verifyNetworkExistence(t *testing.T, node string, id string, present bool) {
	t.Helper()
	for i := 0; i < 80; i++ {
		db.RLock()
		nn, nnok := db.networks[node]
		db.RUnlock()
		if nnok {
			n, ok := nn[id]
			if present && ok {
				return
			}

			if !present &&
				((ok && n.leaving) ||
					!ok) {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Error("Network existence verification failed")
}

func (db *NetworkDB) verifyEntryExistence(t *testing.T, tname, nid, key, value string, present bool) {
	t.Helper()
	n := 80
	for i := 0; i < n; i++ {
		entry, err := db.getEntry(tname, nid, key)
		if present && err == nil && string(entry.value) == value {
			return
		}

		if !present &&
			((err == nil && entry.deleting) ||
				(err != nil)) {
			return
		}

		if i == n-1 && !present && err != nil {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("Entry existence verification test failed for %v(%v)", db.config.Hostname, db.config.NodeID)
}

func testWatch(t *testing.T, ch chan events.Event, ev interface{}, tname, nid, key, value string) {
	t.Helper()
	select {
	case rcvdEv := <-ch:
		assert.Check(t, is.Equal(fmt.Sprintf("%T", rcvdEv), fmt.Sprintf("%T", ev)))
		switch typ := rcvdEv.(type) {
		case CreateEvent:
			assert.Check(t, is.Equal(tname, typ.Table))
			assert.Check(t, is.Equal(nid, typ.NetworkID))
			assert.Check(t, is.Equal(key, typ.Key))
			assert.Check(t, is.Equal(value, string(typ.Value)))
		case UpdateEvent:
			assert.Check(t, is.Equal(tname, typ.Table))
			assert.Check(t, is.Equal(nid, typ.NetworkID))
			assert.Check(t, is.Equal(key, typ.Key))
			assert.Check(t, is.Equal(value, string(typ.Value)))
		case DeleteEvent:
			assert.Check(t, is.Equal(tname, typ.Table))
			assert.Check(t, is.Equal(nid, typ.NetworkID))
			assert.Check(t, is.Equal(key, typ.Key))
		}
	case <-time.After(time.Second):
		t.Fail()
		return
	}
}

func TestNetworkDBSimple(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())
	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBJoinLeaveNetwork(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", true)

	err = dbs[0].LeaveNetwork("network1")
	assert.NilError(t, err)

	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", false)
	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBJoinLeaveNetworks(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	n := 10
	for i := 1; i <= n; i++ {
		err := dbs[0].JoinNetwork(fmt.Sprintf("network0%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		err := dbs[1].JoinNetwork(fmt.Sprintf("network1%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, fmt.Sprintf("network0%d", i), true)
	}

	for i := 1; i <= n; i++ {
		dbs[0].verifyNetworkExistence(t, dbs[1].config.NodeID, fmt.Sprintf("network1%d", i), true)
	}

	for i := 1; i <= n; i++ {
		err := dbs[0].LeaveNetwork(fmt.Sprintf("network0%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		err := dbs[1].LeaveNetwork(fmt.Sprintf("network1%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, fmt.Sprintf("network0%d", i), false)
	}

	for i := 1; i <= n; i++ {
		dbs[0].verifyNetworkExistence(t, dbs[1].config.NodeID, fmt.Sprintf("network1%d", i), false)
	}

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBCRUDTableEntry(t *testing.T) {
	dbs := createNetworkDBInstances(t, 3, "node", DefaultConfig())

	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", true)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[0].CreateEntry("test_table", "network1", "test_key", []byte("test_value"))
	assert.NilError(t, err)

	dbs[1].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_value", true)
	dbs[2].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_value", false)

	err = dbs[0].UpdateEntry("test_table", "network1", "test_key", []byte("test_updated_value"))
	assert.NilError(t, err)

	dbs[1].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_updated_value", true)

	err = dbs[0].DeleteEntry("test_table", "network1", "test_key")
	assert.NilError(t, err)

	dbs[1].verifyEntryExistence(t, "test_table", "network1", "test_key", "", false)

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBCRUDTableEntries(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", true)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[0].verifyNetworkExistence(t, dbs[1].config.NodeID, "network1", true)

	n := 10
	for i := 1; i <= n; i++ {
		err = dbs[0].CreateEntry("test_table", "network1",
			fmt.Sprintf("test_key0%d", i),
			[]byte(fmt.Sprintf("test_value0%d", i)))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		err = dbs[1].CreateEntry("test_table", "network1",
			fmt.Sprintf("test_key1%d", i),
			[]byte(fmt.Sprintf("test_value1%d", i)))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[0].verifyEntryExistence(t, "test_table", "network1",
			fmt.Sprintf("test_key1%d", i),
			fmt.Sprintf("test_value1%d", i), true)
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[1].verifyEntryExistence(t, "test_table", "network1",
			fmt.Sprintf("test_key0%d", i),
			fmt.Sprintf("test_value0%d", i), true)
		assert.NilError(t, err)
	}

	// Verify deletes
	for i := 1; i <= n; i++ {
		err = dbs[0].DeleteEntry("test_table", "network1",
			fmt.Sprintf("test_key0%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		err = dbs[1].DeleteEntry("test_table", "network1",
			fmt.Sprintf("test_key1%d", i))
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[0].verifyEntryExistence(t, "test_table", "network1",
			fmt.Sprintf("test_key1%d", i), "", false)
		assert.NilError(t, err)
	}

	for i := 1; i <= n; i++ {
		dbs[1].verifyEntryExistence(t, "test_table", "network1",
			fmt.Sprintf("test_key0%d", i), "", false)
		assert.NilError(t, err)
	}

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBNodeLeave(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[0].CreateEntry("test_table", "network1", "test_key", []byte("test_value"))
	assert.NilError(t, err)

	dbs[1].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_value", true)

	dbs[0].Close()
	dbs[1].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_value", false)
	dbs[1].Close()
}

func TestNetworkDBWatch(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())
	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	ch, cancel := dbs[1].Watch("", "", "")

	err = dbs[0].CreateEntry("test_table", "network1", "test_key", []byte("test_value"))
	assert.NilError(t, err)

	testWatch(t, ch.C, CreateEvent{}, "test_table", "network1", "test_key", "test_value")

	err = dbs[0].UpdateEntry("test_table", "network1", "test_key", []byte("test_updated_value"))
	assert.NilError(t, err)

	testWatch(t, ch.C, UpdateEvent{}, "test_table", "network1", "test_key", "test_updated_value")

	err = dbs[0].DeleteEntry("test_table", "network1", "test_key")
	assert.NilError(t, err)

	testWatch(t, ch.C, DeleteEvent{}, "test_table", "network1", "test_key", "")

	cancel()
	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBBulkSync(t *testing.T) {
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", true)

	n := 1000
	for i := 1; i <= n; i++ {
		err = dbs[0].CreateEntry("test_table", "network1",
			fmt.Sprintf("test_key0%d", i),
			[]byte(fmt.Sprintf("test_value0%d", i)))
		assert.NilError(t, err)
	}

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	dbs[0].verifyNetworkExistence(t, dbs[1].config.NodeID, "network1", true)

	for i := 1; i <= n; i++ {
		dbs[1].verifyEntryExistence(t, "test_table", "network1",
			fmt.Sprintf("test_key0%d", i),
			fmt.Sprintf("test_value0%d", i), true)
		assert.NilError(t, err)
	}

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBCRUDMediumCluster(t *testing.T) {
	n := 5

	dbs := createNetworkDBInstances(t, n, "node", DefaultConfig())

	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}

			dbs[i].verifyNodeExistence(t, dbs[j].config.NodeID, true)
		}
	}

	for i := 0; i < n; i++ {
		err := dbs[i].JoinNetwork("network1")
		assert.NilError(t, err)
	}

	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			dbs[i].verifyNetworkExistence(t, dbs[j].config.NodeID, "network1", true)
		}
	}

	err := dbs[0].CreateEntry("test_table", "network1", "test_key", []byte("test_value"))
	assert.NilError(t, err)

	for i := 1; i < n; i++ {
		dbs[i].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_value", true)
	}

	err = dbs[0].UpdateEntry("test_table", "network1", "test_key", []byte("test_updated_value"))
	assert.NilError(t, err)

	for i := 1; i < n; i++ {
		dbs[i].verifyEntryExistence(t, "test_table", "network1", "test_key", "test_updated_value", true)
	}

	err = dbs[0].DeleteEntry("test_table", "network1", "test_key")
	assert.NilError(t, err)

	for i := 1; i < n; i++ {
		dbs[i].verifyEntryExistence(t, "test_table", "network1", "test_key", "", false)
	}

	for i := 1; i < n; i++ {
		_, err = dbs[i].GetEntry("test_table", "network1", "test_key")
		assert.Check(t, is.ErrorContains(err, ""))
		assert.Check(t, is.Contains(err.Error(), "deleted and pending garbage collection"), err)
	}

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBNodeJoinLeaveIteration(t *testing.T) {
	maxRetry := 5
	dbs := createNetworkDBInstances(t, 2, "node", DefaultConfig())

	// Single node Join/Leave
	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	if len(dbs[0].networkNodes["network1"]) != 1 {
		t.Fatalf("The networkNodes list has to have be 1 instead of %d", len(dbs[0].networkNodes["network1"]))
	}

	err = dbs[0].LeaveNetwork("network1")
	assert.NilError(t, err)

	if len(dbs[0].networkNodes["network1"]) != 0 {
		t.Fatalf("The networkNodes list has to have be 0 instead of %d", len(dbs[0].networkNodes["network1"]))
	}

	// Multiple nodes Join/Leave
	err = dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	// Wait for the propagation on db[0]
	dbs[0].verifyNetworkExistence(t, dbs[1].config.NodeID, "network1", true)
	if len(dbs[0].networkNodes["network1"]) != 2 {
		t.Fatalf("The networkNodes list has to have be 2 instead of %d - %v", len(dbs[0].networkNodes["network1"]), dbs[0].networkNodes["network1"])
	}
	if n, ok := dbs[0].networks[dbs[0].config.NodeID]["network1"]; !ok || n.leaving {
		t.Fatalf("The network should not be marked as leaving:%t", n.leaving)
	}

	// Wait for the propagation on db[1]
	dbs[1].verifyNetworkExistence(t, dbs[0].config.NodeID, "network1", true)
	if len(dbs[1].networkNodes["network1"]) != 2 {
		t.Fatalf("The networkNodes list has to have be 2 instead of %d - %v", len(dbs[1].networkNodes["network1"]), dbs[1].networkNodes["network1"])
	}
	if n, ok := dbs[1].networks[dbs[1].config.NodeID]["network1"]; !ok || n.leaving {
		t.Fatalf("The network should not be marked as leaving:%t", n.leaving)
	}

	// Try a quick leave/join
	err = dbs[0].LeaveNetwork("network1")
	assert.NilError(t, err)
	err = dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	for i := 0; i < maxRetry; i++ {
		if len(dbs[0].networkNodes["network1"]) == 2 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if len(dbs[0].networkNodes["network1"]) != 2 {
		t.Fatalf("The networkNodes list has to have be 2 instead of %d - %v", len(dbs[0].networkNodes["network1"]), dbs[0].networkNodes["network1"])
	}

	for i := 0; i < maxRetry; i++ {
		if len(dbs[1].networkNodes["network1"]) == 2 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if len(dbs[1].networkNodes["network1"]) != 2 {
		t.Fatalf("The networkNodes list has to have be 2 instead of %d - %v", len(dbs[1].networkNodes["network1"]), dbs[1].networkNodes["network1"])
	}

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBGarbageCollection(t *testing.T) {
	keysWriteDelete := 5
	config := DefaultConfig()
	config.reapEntryInterval = 30 * time.Second
	config.StatsPrintPeriod = 15 * time.Second

	dbs := createNetworkDBInstances(t, 3, "node", config)

	// 2 Nodes join network
	err := dbs[0].JoinNetwork("network1")
	assert.NilError(t, err)

	err = dbs[1].JoinNetwork("network1")
	assert.NilError(t, err)

	for i := 0; i < keysWriteDelete; i++ {
		err = dbs[i%2].CreateEntry("testTable", "network1", "key-"+strconv.Itoa(i), []byte("value"))
		assert.NilError(t, err)
	}
	time.Sleep(time.Second)
	for i := 0; i < keysWriteDelete; i++ {
		err = dbs[i%2].DeleteEntry("testTable", "network1", "key-"+strconv.Itoa(i))
		assert.NilError(t, err)
	}
	for i := 0; i < 2; i++ {
		assert.Check(t, is.Equal(keysWriteDelete, dbs[i].networks[dbs[i].config.NodeID]["network1"].entriesNumber), "entries number should match")
	}

	// from this point the timer for the garbage collection started, wait 5 seconds and then join a new node
	time.Sleep(5 * time.Second)

	err = dbs[2].JoinNetwork("network1")
	assert.NilError(t, err)
	for i := 0; i < 3; i++ {
		assert.Check(t, is.Equal(keysWriteDelete, dbs[i].networks[dbs[i].config.NodeID]["network1"].entriesNumber), "entries number should match")
	}
	// at this point the entries should had been all deleted
	time.Sleep(30 * time.Second)
	for i := 0; i < 3; i++ {
		assert.Check(t, is.Equal(0, dbs[i].networks[dbs[i].config.NodeID]["network1"].entriesNumber), "entries should had been garbage collected")
	}

	// make sure that entries are not coming back
	time.Sleep(15 * time.Second)
	for i := 0; i < 3; i++ {
		assert.Check(t, is.Equal(0, dbs[i].networks[dbs[i].config.NodeID]["network1"].entriesNumber), "entries should had been garbage collected")
	}

	closeNetworkDBInstances(t, dbs)
}

func TestFindNode(t *testing.T) {
	dbs := createNetworkDBInstances(t, 1, "node", DefaultConfig())

	dbs[0].nodes["active"] = &node{Node: memberlist.Node{Name: "active"}}
	dbs[0].failedNodes["failed"] = &node{Node: memberlist.Node{Name: "failed"}}
	dbs[0].leftNodes["left"] = &node{Node: memberlist.Node{Name: "left"}}

	// active nodes is 2 because the testing node is in the list
	assert.Check(t, is.Len(dbs[0].nodes, 2))
	assert.Check(t, is.Len(dbs[0].failedNodes, 1))
	assert.Check(t, is.Len(dbs[0].leftNodes, 1))

	n, currState, m := dbs[0].findNode("active")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal("active", n.Name))
	assert.Check(t, is.Equal(nodeActiveState, currState))
	assert.Check(t, m != nil)
	// delete the entry manually
	delete(m, "active")

	// test if can be still find
	n, currState, m = dbs[0].findNode("active")
	assert.Check(t, is.Nil(n))
	assert.Check(t, is.Equal(nodeNotFound, currState))
	assert.Check(t, is.Nil(m))

	n, currState, m = dbs[0].findNode("failed")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal("failed", n.Name))
	assert.Check(t, is.Equal(nodeFailedState, currState))
	assert.Check(t, m != nil)

	// find and remove
	n, currState, m = dbs[0].findNode("left")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal("left", n.Name))
	assert.Check(t, is.Equal(nodeLeftState, currState))
	assert.Check(t, m != nil)
	delete(m, "left")

	n, currState, m = dbs[0].findNode("left")
	assert.Check(t, is.Nil(n))
	assert.Check(t, is.Equal(nodeNotFound, currState))
	assert.Check(t, is.Nil(m))

	closeNetworkDBInstances(t, dbs)
}

func TestChangeNodeState(t *testing.T) {
	dbs := createNetworkDBInstances(t, 1, "node", DefaultConfig())

	dbs[0].nodes["node1"] = &node{Node: memberlist.Node{Name: "node1"}}
	dbs[0].nodes["node2"] = &node{Node: memberlist.Node{Name: "node2"}}
	dbs[0].nodes["node3"] = &node{Node: memberlist.Node{Name: "node3"}}

	// active nodes is 4 because the testing node is in the list
	assert.Check(t, is.Len(dbs[0].nodes, 4))

	n, currState, m := dbs[0].findNode("node1")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeActiveState, currState))
	assert.Check(t, is.Equal("node1", n.Name))
	assert.Check(t, m != nil)

	// node1 to failed
	dbs[0].changeNodeState("node1", nodeFailedState)

	n, currState, m = dbs[0].findNode("node1")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeFailedState, currState))
	assert.Check(t, is.Equal("node1", n.Name))
	assert.Check(t, m != nil)
	assert.Check(t, time.Duration(0) != n.reapTime)

	// node1 back to active
	dbs[0].changeNodeState("node1", nodeActiveState)

	n, currState, m = dbs[0].findNode("node1")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeActiveState, currState))
	assert.Check(t, is.Equal("node1", n.Name))
	assert.Check(t, m != nil)
	assert.Check(t, is.Equal(time.Duration(0), n.reapTime))

	// node1 to left
	dbs[0].changeNodeState("node1", nodeLeftState)
	dbs[0].changeNodeState("node2", nodeLeftState)
	dbs[0].changeNodeState("node3", nodeLeftState)

	n, currState, m = dbs[0].findNode("node1")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeLeftState, currState))
	assert.Check(t, is.Equal("node1", n.Name))
	assert.Check(t, m != nil)
	assert.Check(t, time.Duration(0) != n.reapTime)

	n, currState, m = dbs[0].findNode("node2")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeLeftState, currState))
	assert.Check(t, is.Equal("node2", n.Name))
	assert.Check(t, m != nil)
	assert.Check(t, time.Duration(0) != n.reapTime)

	n, currState, m = dbs[0].findNode("node3")
	assert.Check(t, n != nil)
	assert.Check(t, is.Equal(nodeLeftState, currState))
	assert.Check(t, is.Equal("node3", n.Name))
	assert.Check(t, m != nil)
	assert.Check(t, time.Duration(0) != n.reapTime)

	// active nodes is 1 because the testing node is in the list
	assert.Check(t, is.Len(dbs[0].nodes, 1))
	assert.Check(t, is.Len(dbs[0].failedNodes, 0))
	assert.Check(t, is.Len(dbs[0].leftNodes, 3))

	closeNetworkDBInstances(t, dbs)
}

func TestNodeReincarnation(t *testing.T) {
	dbs := createNetworkDBInstances(t, 1, "node", DefaultConfig())

	dbs[0].nodes["node1"] = &node{Node: memberlist.Node{Name: "node1", Addr: net.ParseIP("192.168.1.1")}}
	dbs[0].leftNodes["node2"] = &node{Node: memberlist.Node{Name: "node2", Addr: net.ParseIP("192.168.1.2")}}
	dbs[0].failedNodes["node3"] = &node{Node: memberlist.Node{Name: "node3", Addr: net.ParseIP("192.168.1.3")}}

	// active nodes is 2 because the testing node is in the list
	assert.Check(t, is.Len(dbs[0].nodes, 2))
	assert.Check(t, is.Len(dbs[0].failedNodes, 1))
	assert.Check(t, is.Len(dbs[0].leftNodes, 1))

	b := dbs[0].purgeReincarnation(&memberlist.Node{Name: "node4", Addr: net.ParseIP("192.168.1.1")})
	assert.Check(t, b)
	dbs[0].nodes["node4"] = &node{Node: memberlist.Node{Name: "node4", Addr: net.ParseIP("192.168.1.1")}}

	b = dbs[0].purgeReincarnation(&memberlist.Node{Name: "node5", Addr: net.ParseIP("192.168.1.2")})
	assert.Check(t, b)
	dbs[0].nodes["node5"] = &node{Node: memberlist.Node{Name: "node5", Addr: net.ParseIP("192.168.1.1")}}

	b = dbs[0].purgeReincarnation(&memberlist.Node{Name: "node6", Addr: net.ParseIP("192.168.1.3")})
	assert.Check(t, b)
	dbs[0].nodes["node6"] = &node{Node: memberlist.Node{Name: "node6", Addr: net.ParseIP("192.168.1.1")}}

	b = dbs[0].purgeReincarnation(&memberlist.Node{Name: "node6", Addr: net.ParseIP("192.168.1.10")})
	assert.Check(t, !b)

	// active nodes is 1 because the testing node is in the list
	assert.Check(t, is.Len(dbs[0].nodes, 4))
	assert.Check(t, is.Len(dbs[0].failedNodes, 0))
	assert.Check(t, is.Len(dbs[0].leftNodes, 3))

	closeNetworkDBInstances(t, dbs)
}

func TestParallelCreate(t *testing.T) {
	dbs := createNetworkDBInstances(t, 1, "node", DefaultConfig())

	startCh := make(chan int)
	doneCh := make(chan error)
	var success int32
	for i := 0; i < 20; i++ {
		go func() {
			<-startCh
			err := dbs[0].CreateEntry("testTable", "testNetwork", "key", []byte("value"))
			if err == nil {
				atomic.AddInt32(&success, 1)
			}
			doneCh <- err
		}()
	}

	close(startCh)

	for i := 0; i < 20; i++ {
		<-doneCh
	}
	close(doneCh)
	// Only 1 write should have succeeded
	assert.Check(t, is.Equal(int32(1), success))

	closeNetworkDBInstances(t, dbs)
}

func TestParallelDelete(t *testing.T) {
	dbs := createNetworkDBInstances(t, 1, "node", DefaultConfig())

	err := dbs[0].CreateEntry("testTable", "testNetwork", "key", []byte("value"))
	assert.NilError(t, err)

	startCh := make(chan int)
	doneCh := make(chan error)
	var success int32
	for i := 0; i < 20; i++ {
		go func() {
			<-startCh
			err := dbs[0].DeleteEntry("testTable", "testNetwork", "key")
			if err == nil {
				atomic.AddInt32(&success, 1)
			}
			doneCh <- err
		}()
	}

	close(startCh)

	for i := 0; i < 20; i++ {
		<-doneCh
	}
	close(doneCh)
	// Only 1 write should have succeeded
	assert.Check(t, is.Equal(int32(1), success))

	closeNetworkDBInstances(t, dbs)
}

func TestNetworkDBIslands(t *testing.T) {
	pollTimeout := func() time.Duration {
		const defaultTimeout = 120 * time.Second
		dl, ok := t.Deadline()
		if !ok {
			return defaultTimeout
		}
		if d := time.Until(dl); d <= defaultTimeout {
			return d
		}
		return defaultTimeout
	}

	logrus.SetLevel(logrus.DebugLevel)
	conf := DefaultConfig()
	// Shorten durations to speed up test execution.
	conf.rejoinClusterDuration = conf.rejoinClusterDuration / 10
	conf.rejoinClusterInterval = conf.rejoinClusterInterval / 10
	dbs := createNetworkDBInstances(t, 5, "node", conf)

	// Get the node IP used currently
	node := dbs[0].nodes[dbs[0].config.NodeID]
	baseIPStr := node.Addr.String()
	// Node 0,1,2 are going to be the 3 bootstrap nodes
	members := []string{fmt.Sprintf("%s:%d", baseIPStr, dbs[0].config.BindPort),
		fmt.Sprintf("%s:%d", baseIPStr, dbs[1].config.BindPort),
		fmt.Sprintf("%s:%d", baseIPStr, dbs[2].config.BindPort)}
	// Rejoining will update the list of the bootstrap members
	for i := 3; i < 5; i++ {
		t.Logf("Re-joining: %d", i)
		assert.Check(t, dbs[i].Join(members))
	}

	// Now the 3 bootstrap nodes will cleanly leave, and will be properly removed from the other 2 nodes
	for i := 0; i < 3; i++ {
		logrus.Infof("node %d leaving", i)
		dbs[i].Close()
	}

	checkDBs := make(map[string]*NetworkDB)
	for i := 3; i < 5; i++ {
		db := dbs[i]
		checkDBs[db.config.Hostname] = db
	}

	// Give some time to let the system propagate the messages and free up the ports
	check := func(t poll.LogT) poll.Result {
		// Verify that the nodes are actually all gone and marked appropiately
		for name, db := range checkDBs {
			db.RLock()
			if (len(db.leftNodes) != 3) || (len(db.failedNodes) != 0) {
				for name := range db.leftNodes {
					t.Logf("%s: Node %s left", db.config.Hostname, name)
				}
				for name := range db.failedNodes {
					t.Logf("%s: Node %s failed", db.config.Hostname, name)
				}
				db.RUnlock()
				return poll.Continue("%s:Waiting for all nodes to cleanly leave, left: %d, failed nodes: %d", name, len(db.leftNodes), len(db.failedNodes))
			}
			db.RUnlock()
			t.Logf("%s: OK", name)
			delete(checkDBs, name)
		}
		return poll.Success()
	}
	poll.WaitOn(t, check, poll.WithDelay(time.Second), poll.WithTimeout(pollTimeout()))

	// Spawn again the first 3 nodes with different names but same IP:port
	for i := 0; i < 3; i++ {
		logrus.Infof("node %d coming back", i)
		dbs[i].config.NodeID = stringid.TruncateID(stringid.GenerateRandomID())
		dbs[i] = launchNode(t, *dbs[i].config)
	}

	// Give some time for the reconnect routine to run, it runs every 6s.
	check = func(t poll.LogT) poll.Result {
		// Verify that the cluster is again all connected. Note that the 3 previous node did not do any join
		for i := 0; i < 5; i++ {
			db := dbs[i]
			db.RLock()
			if len(db.nodes) != 5 {
				db.RUnlock()
				return poll.Continue("%s:Waiting to connect to all nodes", dbs[i].config.Hostname)
			}
			if len(db.failedNodes) != 0 {
				db.RUnlock()
				return poll.Continue("%s:Waiting for 0 failedNodes", dbs[i].config.Hostname)
			}
			if i < 3 {
				// nodes from 0 to 3 has no left nodes
				if len(db.leftNodes) != 0 {
					db.RUnlock()
					return poll.Continue("%s:Waiting to have no leftNodes", dbs[i].config.Hostname)
				}
			} else {
				// nodes from 4 to 5 has the 3 previous left nodes
				if len(db.leftNodes) != 3 {
					db.RUnlock()
					return poll.Continue("%s:Waiting to have 3 leftNodes", dbs[i].config.Hostname)
				}
			}
			db.RUnlock()
		}
		return poll.Success()
	}
	poll.WaitOn(t, check, poll.WithDelay(time.Second), poll.WithTimeout(pollTimeout()))
	closeNetworkDBInstances(t, dbs)
}

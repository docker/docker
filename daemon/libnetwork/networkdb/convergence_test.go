package networkdb

import (
	"encoding/csv"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/lamport"
	"gotest.tools/v3/assert"
)

// Convergence latency measurement.
//
// This is the counterpart to TestNetworkDBAlwaysConverges. That test explores
// interleavings and makes no timing claims; this one gives up interleaving
// entirely in exchange for a clean measurement:
//
//   - The cluster is quiesced before the clock starts, so nothing is in flight
//     that the measurement did not cause.
//   - Exactly one perturbation is applied, of a known kind, so every sample is
//     attributable rather than being the aggregate of a random action mix.
//   - The target is a Lamport time, not a value. A write at ltime L has reached
//     a node once that node holds the key at ltime >= L. That is immune to a
//     later write superseding the value, and unlike value equality it stays
//     well defined if anything else is in flight.
//   - Convergence is watched from the last mutation onwards rather than after
//     the fact, so no sample is left-censored.
//
// The measurements are virtual time from inside a [testing/synctest] bubble.
// They isolate the algorithm from machine load and are comparable across runs
// and machines, but they account for none of the network RTT, syscall or
// serialization cost a real cluster pays. They are a floor for any wall-clock
// figure, never the figure itself.
//
// A corollary worth keeping in mind when reading a zero: virtual time only
// advances on a timer, so work which completes without waiting for one costs
// nothing here however much real work it does. A synchronous bulk sync over the
// in-memory network is instantaneous in virtual time and would not be on a real
// one. What a zero means is "no timer had to fire", which is a genuine and
// useful property -- it is the difference between propagating on the next gossip
// tick and propagating right now -- but it is not "no time passed".

const (
	// Measurement granularity. Every sample is biased upwards by at most this
	// much. The convergence check is a single key lookup per node, so a fine
	// step is affordable.
	latencyStep = time.Millisecond

	// A deliberately loose ceiling, so that this test also serves as a
	// liveness assertion. It is well above the ~30s anti-entropy sweep which
	// dominates the slow tail, and is not a claim about expected latency.
	latencyCeiling = 3 * time.Minute

	// Budget for reaching a quiet cluster before the clock starts.
	settleStep    = 10 * time.Millisecond
	settleTimeout = 90 * time.Second
)

var latencyClusterSizes = []int{2, 3, 5, 10, 25}

// TestNetworkDBConvergenceLatency measures how long one change takes to reach
// every node, broken down by cluster size and by the kind of change.
//
// Each run records one sample per (perturbation, cluster size). Results are
// appended to testdata/convergence_virtual-<timestamp>-<pid>.csv, one file per
// invocation; collect a distribution by asking for more runs:
//
//	go test ./daemon/libnetwork/networkdb -run TestNetworkDBConvergenceLatency \
//	    -count=50 -timeout=30m
func TestNetworkDBConvergenceLatency(t *testing.T) {
	requireSynctest(t)
	for _, p := range perturbations {
		for _, nodes := range latencyClusterSizes {
			if nodes < p.minNodes {
				continue
			}
			t.Run(fmt.Sprintf("%s/nodes=%d", p.name, nodes), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p.do(t, nodes)
				})
			})
		}
	}
}

// perturbation is one kind of change whose propagation latency is measured.
//
// apply performs the change on the cluster and returns a predicate reporting
// whether a given node has caught up with it yet.
type perturbation struct {
	name     string
	minNodes int

	// key is the table key the perturbation acts on, used to attribute the
	// delivery mechanism. Empty for perturbations which write no entry.
	key string

	// holdBackLast keeps the last node out of the initial join, for
	// perturbations which have it join as part of the measurement.
	holdBackLast bool

	// seedOwner, when non-nil, creates a "seed" entry on the node it selects,
	// settled before the clock starts, for perturbations which need something
	// to act on. Which node owns it matters: a node leaving a network purges
	// only the entries it owns.
	seedOwner func(*memCluster) *NetworkDB

	apply func(t *testing.T, c *memCluster, nw string) (converged func(*NetworkDB) bool)
}

func firstNode(c *memCluster) *NetworkDB { return c.dbs[0] }
func lastNode(c *memCluster) *NetworkDB  { return c.dbs[len(c.dbs)-1] }

var perturbations = []perturbation{
	{
		name:     "create-entry",
		minNodes: 2,
		key:      "k",
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			origin := c.dbs[0]
			assert.NilError(t, origin.CreateEntry(tableUnderTest, nw, "k", []byte("v")))
			ltime := entryLTime(t, origin, nw, "k")
			return entryReached(nw, "k", ltime, false)
		},
	},
	{
		name:      "update-entry",
		minNodes:  2,
		key:       "seed",
		seedOwner: firstNode,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			origin := c.dbs[0]
			assert.NilError(t, origin.UpdateEntry(tableUnderTest, nw, "seed", []byte("v2")))
			ltime := entryLTime(t, origin, nw, "seed")
			return entryReached(nw, "seed", ltime, false)
		},
	},
	{
		name:      "delete-entry",
		minNodes:  2,
		key:       "seed",
		seedOwner: firstNode,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			origin := c.dbs[0]
			assert.NilError(t, origin.DeleteEntry(tableUnderTest, nw, "seed"))
			ltime := entryLTime(t, origin, nw, "seed")
			return entryReached(nw, "seed", ltime, true)
		},
	},
	{
		name:         "join-network",
		minNodes:     3,
		holdBackLast: true,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			// The last node sat out the initial join; have it join now.
			joiner := c.dbs[len(c.dbs)-1]
			assert.NilError(t, joiner.JoinNetwork(nw))
			return func(db *NetworkDB) bool {
				return participates(db, nw, joiner.config.NodeID)
			}
		},
	},
	{
		// Join a network and write to it in the same breath, without letting
		// the join propagate first.
		//
		// The hazard being probed is that handleTableEvent tests nodePresent
		// before applying anything, so a peer which has not yet learned this
		// node participates in nw rejects its table events outright, leaving
		// the entry for the next anti-entropy sweep an order of magnitude
		// later.
		//
		// The rejection is real, not hypothetical: instrumenting the
		// nodePresent branch counts this write rejected twice at three nodes
		// and twelve times at twenty-five, where a plain create is rejected
		// none. Nothing sequences the join ahead of it -- the join goes to the
		// cluster-wide networkBroadcasts, drained by memberlist's own gossip,
		// and the write to the per-network tableBroadcasts, drained by
		// NetworkDB's gossip() to a different set of peers. What recovers it
		// is retransmission: tableBroadcasts carries RetransmitMult 4, so a
		// later copy lands once the join has propagated, and the entry arrives
		// by gossip one round slower than a plain create rather than falling
		// through to anti-entropy.
		//
		// Kept for that recovery path. A change which stopped retransmitting
		// rejected table events would show up here as a jump to tens of
		// seconds, and in no other perturbation.
		//
		// minNodes is 3, not 2, because holdBackLast leaves one fewer node in
		// the network: at two nodes exactly one has joined, and settle's
		// agreement check is then satisfied by that node's own synchronous
		// JoinNetwork before anything has propagated. The measurement would be
		// of join propagation from a standing start, not of the hazard above.
		name:         "write-after-join",
		minNodes:     3,
		key:          "k",
		holdBackLast: true,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			writer := c.dbs[len(c.dbs)-1]
			assert.NilError(t, writer.JoinNetwork(nw))
			assert.NilError(t, writer.CreateEntry(tableUnderTest, nw, "k", []byte("v")))
			ltime := entryLTime(t, writer, nw, "k")
			return entryReached(nw, "k", ltime, false)
		},
	},
	{
		// Join a network which already holds entries. The joiner has to acquire
		// that existing state, and JoinNetwork bulk syncs for exactly this
		// reason -- but only against the peers it already believes participate.
		// Whether that set is populated at the moment of the join decides
		// whether the entries arrive at once or only on the next periodic
		// anti-entropy sweep, tens of seconds later.
		name:         "join-populated-network",
		minNodes:     3,
		key:          "seed",
		holdBackLast: true,
		seedOwner:    firstNode,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			joiner := c.dbs[len(c.dbs)-1]
			ltime := entryLTime(t, c.dbs[0], nw, "seed")
			assert.NilError(t, joiner.JoinNetwork(nw))
			return entryReached(nw, "seed", ltime, false)
		},
	},
	{
		// Leaving a network is two jobs, and the entry purge is the expensive
		// one: LeaveNetwork tombstones every entry the leaver owns and each peer
		// hard-deletes them on the network-leave event. The seed is therefore
		// owned by the leaver -- seeded from any other node, the purge would
		// walk nothing and this would measure membership propagation alone.
		name:      "leave-network",
		minNodes:  3,
		key:       "seed",
		seedOwner: lastNode,
		apply: func(t *testing.T, c *memCluster, nw string) func(*NetworkDB) bool {
			leaver := c.dbs[len(c.dbs)-1]
			assert.NilError(t, leaver.LeaveNetwork(nw))
			// LeaveNetwork drops the leaver from its own networkNodes under the
			// write lock before returning, so the membership half already holds
			// for the leaver itself and needs no special case. The entry half is
			// asymmetric -- peers hard-delete, the leaver keeps a tombstone --
			// so ask only that the entry is no longer live, which covers both.
			return func(db *NetworkDB) bool {
				if participates(db, nw, leaver.config.NodeID) {
					return false
				}
				e, ok := lookupEntry(db, nw, "seed")
				return !ok || e.deleting
			}
		},
	},
}

// do runs a single sample: build a quiet cluster, perturb it once, and time how
// long the change takes to reach every node.
func (p perturbation) do(t *testing.T, nodes int) {
	const nw = "nw0"

	var obs mechanismObserver
	conf := DefaultConfig()
	conf.tableEventObserver = obs.record

	c := newMemCluster(t, nodes, "node", conf, 0)

	// Every node joins the network, except the one a perturbation which joins as
	// part of the measurement needs to keep out of it.
	joiners := c.dbs
	if p.holdBackLast {
		joiners = c.dbs[:len(c.dbs)-1]
	}
	for _, db := range joiners {
		assert.NilError(t, db.JoinNetwork(nw))
	}
	if p.seedOwner != nil {
		assert.NilError(t, p.seedOwner(c).CreateEntry(tableUnderTest, nw, "seed", []byte("v1")))
	}

	// Let all of that settle, so the measurement below only sees traffic it
	// caused itself.
	wantMembers := make([]string, 0, len(joiners))
	for _, db := range joiners {
		wantMembers = append(wantMembers, db.config.NodeID)
	}
	c.settle(t, nw, wantMembers)
	obs.reset()
	c.mn.resetDropCount()

	// Start the clock before applying the change, not after. Some of these
	// APIs propagate synchronously -- JoinNetwork bulk syncs before it returns
	// -- and timing from after the call would hide that work entirely, the same
	// way measuring after the fact hid it in the property test.
	start := time.Now()
	converged := p.apply(t, c, nw)

	var polls int
	deadline := start.Add(latencyCeiling)
	for {
		synctest.Wait()
		polls++
		if allConverged(c.dbs, converged) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s on %d nodes did not converge within %v of virtual time; %d nodes still behind",
				p.name, nodes, latencyCeiling, countBehind(c.dbs, converged))
		}
		time.Sleep(latencyStep)
	}
	elapsed := time.Since(start)

	// A drop means memPacketBuffer was too shallow and this sample was inflated
	// by retransmits a real cluster would not have needed. Fail rather than
	// quietly record a contaminated measurement.
	assert.Equal(t, c.mn.dropCount(), int64(0), "in-memory network dropped datagrams; measurement is not usable")

	gossip, bulkSync := obs.deliveriesOf(p.key, c.dbs)
	t.Logf("%s on %d nodes converged in %v of virtual time (%d polls, %d by gossip, %d by bulk sync)",
		p.name, nodes, elapsed, polls, gossip, bulkSync)
	recordSample(t, sample{
		Perturbation: p.name,
		Nodes:        nodes,
		Converged:    len(c.dbs),
		Elapsed:      elapsed,
		Polls:        polls,
		Gossip:       gossip,
		BulkSync:     bulkSync,
	})
}

// allConverged reports whether every node has caught up. It runs on every poll
// of the measuring loop, so it short-circuits on the first node still behind
// rather than counting them all.
func allConverged(dbs []*NetworkDB, converged func(*NetworkDB) bool) bool {
	return !slices.ContainsFunc(dbs, func(db *NetworkDB) bool { return !converged(db) })
}

// countBehind is for the failure message, which wants the number rather than
// the fact.
func countBehind(dbs []*NetworkDB, converged func(*NetworkDB) bool) int {
	var n int
	for _, db := range dbs {
		if !converged(db) {
			n++
		}
	}
	return n
}

// settle drives the cluster to a quiet, agreed state before a measurement
// begins: every node which has joined nw agrees that exactly wantMembers
// participate in it, and holds identical table contents.
//
// Getting this condition right matters more than it looks. A weaker check that
// merely compares the *size* of each node's participant set passes immediately
// after the joins are issued, while every node still lists only itself -- and
// then the measurement which follows is really timing join propagation as well
// as the perturbation. Worse, until a node knows the writer participates in the
// network, handleTableEvent rejects its table events outright.
func (c *memCluster) settle(t *testing.T, nw string, wantMembers []string) {
	t.Helper()

	agreed, err := c.agreed(nw, wantMembers)
	assert.NilError(t, err)

	start := time.Now()
	for {
		synctest.Wait()
		if agreed() {
			return
		}
		if time.Since(start) >= settleTimeout {
			t.Fatalf("cluster of %d did not settle within %v of virtual time:\n%s",
				len(c.dbs), settleTimeout, c.dumpMembership(nw))
		}
		time.Sleep(settleStep)
	}
}

// keyState is what a node believes about one table key, reduced to the parts
// which decide whether it has caught up: its Lamport time and whether it is a
// tombstone. The value itself is redundant -- it is a function of the ltime.
type keyState struct {
	ltime    lamport.Time
	deleting bool
}

// setOf collects s into a set, so membership can be compared without regard to
// order and without the caller having to sort anything.
func setOf[T comparable](s []T) map[T]struct{} {
	m := make(map[T]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

// agreed returns a predicate reporting whether every node in the cluster agrees
// that exactly the nodes in want participate in nw, and whether every node which
// has joined nw holds the same table contents. It is a constructor rather than
// the check itself so that want is converted once, not on each of the many calls
// a polling loop makes.
//
// The two halves have deliberately different scopes: membership is asked of
// every node because a node outside nw still needs it (see below), while table
// contents are only meaningful for nodes actually in the network.
//
// want is compared as a set, so its order does not matter. networkNodes is in
// arrival order, which differs per node, so comparing the two as sequences would
// mean sorting both -- and a caller who passed an unsorted want would get a
// predicate that silently never holds. Taking a slice and converting here keeps
// that impossible to get wrong from the outside.
//
// A duplicate in want would collapse into the same set and leave the predicate
// looking satisfiable when it describes a cluster that cannot exist, so it is
// reported rather than absorbed. A duplicate in networkNodes still collapses
// silently, which is the right split: that would be a NetworkDB bug,
// addNetworkNode already guards against it, and this is a quiescence predicate
// rather than an assertion about NetworkDB's invariants.
//
// "Joined" here means the same thing it means in dumpMembership: present and not
// leaving. Presence alone is not enough for the table half, because LeaveNetwork
// tombstones the entries the leaver owns and drops everyone else's while keeping
// the thisNodeNetworks entry for reapNetworkInterval -- so a departing node's
// table cannot match its peers' and never will again.
func (c *memCluster) agreed(nw string, want []string) (func() bool, error) {
	wantSet := setOf(want)
	if len(wantSet) != len(want) {
		return nil, fmt.Errorf("want lists %d node IDs but only %d are distinct: %v",
			len(want), len(wantSet), want)
	}

	return func() bool {
		// Declared per call, not captured: a reference table held across calls
		// would compare this poll's state against a snapshot taken polls ago.
		var (
			ref     map[string]keyState
			haveRef bool
		)
		for _, db := range c.dbs {
			db.RLock()
			n, joined := db.thisNodeNetworks[nw]
			joined = joined && !n.leaving
			members := setOf(db.networkNodes[nw])
			db.RUnlock()
			// Membership is required of every node, joined or not. A node
			// outside nw still tracks who participates in it, and JoinNetwork
			// picks its bulk-sync peers out of that list -- so a node held back
			// from the initial join has to know the incumbents before it joins,
			// or its join-time bulk sync finds no peers and whatever it should
			// have picked up waits for the next anti-entropy sweep instead.
			// That is the difference join-populated-network exists to measure,
			// between roughly zero and tens of seconds, so it cannot be left to
			// whether the settle loop happened to run long enough.
			if !maps.Equal(members, wantSet) {
				return false
			}
			if !joined {
				continue
			}
			got := keyStates(db, nw)
			if !haveRef {
				ref, haveRef = got, true
				continue
			}
			if !maps.Equal(got, ref) {
				return false
			}
		}
		return true
	}, nil
}

// keyStates collects one node's view of every key in nw.
//
// It walks the byTable index directly rather than going through WalkTable. The
// radix root is immutable, so one snapshot taken under the lock gives a
// consistent view of the whole table; WalkTable hands the callback only the
// value and the tombstone flag, so getting each key's ltime through it meant a
// second lookup per key -- one lock acquisition each, against a *later*
// snapshot than the walk enumerated. On the churn path that is tens of
// thousands of locked lookups per call, for a torn view.
func keyStates(db *NetworkDB, nw string) map[string]keyState {
	db.RLock()
	root := db.indexes[byTable].Root()
	db.RUnlock()

	prefix := "/" + tableUnderTest + "/" + nw + "/"
	out := make(map[string]keyState)
	root.WalkPrefix([]byte(prefix), func(path []byte, e *entry) bool {
		out[string(path[len(prefix):])] = keyState{ltime: e.ltime, deleting: e.deleting}
		return false
	})
	return out
}

// dumpKeysMax bounds the per-node key listing in a failure dump. The churn test
// leaves up to churnMaxEntries junk keys on each of up to 25 nodes, and printing
// them all turns the one message a human reads into megabytes they cannot.
const dumpKeysMax = 8

func (c *memCluster) dumpMembership(nw string) string {
	var b strings.Builder
	for _, db := range c.dbs {
		db.RLock()
		joined := false
		if n, ok := db.thisNodeNetworks[nw]; ok {
			joined = !n.leaving
		}
		members := slices.Sorted(slices.Values(db.networkNodes[nw]))
		db.RUnlock()
		fmt.Fprintf(&b, "  %s (%s) joined=%v members=%v keys=%s\n",
			db.config.Hostname, db.config.NodeID, joined, members, summariseKeys(keyStates(db, nw)))
	}
	return b.String()
}

// summariseKeys renders at most dumpKeysMax keys, in sorted order, and says how
// many it left out.
func summariseKeys(ks map[string]keyState) string {
	keys := slices.Sorted(maps.Keys(ks))
	var b strings.Builder
	fmt.Fprintf(&b, "%d[", len(keys))
	for i, k := range keys {
		if i == dumpKeysMax {
			fmt.Fprintf(&b, " ...+%d", len(keys)-dumpKeysMax)
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:%v", k, ks[k])
	}
	b.WriteByte(']')
	return b.String()
}

// lookupEntry reads one entry, including its Lamport time, from a node's local
// store. Unlike [NetworkDB.GetEntry] it does not hide entries pending deletion,
// because a delete's propagation is exactly what one perturbation measures.
func lookupEntry(db *NetworkDB, nw, key string) (entry, bool) {
	db.RLock()
	defer db.RUnlock()
	e, err := db.getEntry(tableUnderTest, nw, key)
	if err != nil || e == nil {
		return entry{}, false
	}
	return *e, true
}

// entryReached returns a predicate reporting whether a node has caught up with a
// write to nw/key at Lamport time ltime. deleting is what the entry should look
// like once it has: a tombstone for a delete, a live value for anything else.
func entryReached(nw, key string, ltime lamport.Time, deleting bool) func(*NetworkDB) bool {
	return func(db *NetworkDB) bool {
		e, ok := lookupEntry(db, nw, key)
		return ok && e.ltime >= ltime && e.deleting == deleting
	}
}

func entryLTime(t *testing.T, db *NetworkDB, nw, key string) lamport.Time {
	t.Helper()
	e, ok := lookupEntry(db, nw, key)
	assert.Assert(t, ok, "originating node has no entry for %s/%s", nw, key)
	return e.ltime
}

func participates(db *NetworkDB, nw, nodeID string) bool {
	db.RLock()
	defer db.RUnlock()
	return slices.Contains(db.networkNodes[nw], nodeID)
}

// mechanismObserver tallies how table events arrived, per key, so a sample can
// be labelled with the mechanism that actually delivered the change rather than
// having it inferred from the elapsed time.
//
// The tally has to be per key. Under background churn the cluster is delivering
// plenty of unrelated entries, some of them by bulk sync, so a whole-cluster
// tally would report "bulksync" for every sample regardless of how the tracked
// write itself travelled.
type mechanismObserver struct {
	mu       sync.Mutex
	gossip   map[delivery]int
	bulkSync map[delivery]int
}

// delivery identifies one node's receipt of one key.
type delivery struct {
	nodeID string
	key    string
}

// record satisfies Config.tableEventObserver. It is called with the NetworkDB
// write lock held, so it does no more than count.
func (o *mechanismObserver) record(nodeID, _, _, key string, viaBulkSync bool) {
	d := delivery{nodeID: nodeID, key: key}
	o.mu.Lock()
	if viaBulkSync {
		if o.bulkSync == nil {
			o.bulkSync = map[delivery]int{}
		}
		o.bulkSync[d]++
	} else {
		if o.gossip == nil {
			o.gossip = map[delivery]int{}
		}
		o.gossip[d]++
	}
	o.mu.Unlock()
}

func (o *mechanismObserver) reset() {
	o.mu.Lock()
	o.gossip, o.bulkSync = nil, nil
	o.mu.Unlock()
}

// deliveriesOf counts how key reached the given nodes: how many applied it from
// a gossip packet, and how many from a bulk sync stream. Both are zero when the
// change is not a table event at all, as a network join or leave is.
//
// Two counts rather than one label, because "arrived by bulk sync" does not mean
// "arrived by the anti-entropy sweep". JoinNetwork bulk syncs before it returns,
// and bulkSyncNode ships the whole byNetwork index, so a node rejoining a network
// pushes everything it holds at its peers -- deliveries an order of magnitude
// sooner than the ~30s sweep, and indistinguishable from it on the receiving
// side, where this observer runs. Collapsing to a single label reported
// "bulksync" for 117 of 125 churn samples, at 181ms to 2.2s, which is not the
// mode such a label is read as meaning. Counts state what was actually seen and
// leave the inference to whoever reads the CSV.
//
// Restricting the question to a set of nodes still matters under churn: a node
// which leaves and rejoins re-acquires the whole table as a matter of course, so
// counting its own deliveries says nothing about how the nodes being measured
// came by the entry.
func (o *mechanismObserver) deliveriesOf(key string, dbs []*NetworkDB) (gossip, bulkSync int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, db := range dbs {
		d := delivery{nodeID: db.config.NodeID, key: key}
		gossip += o.gossip[d]
		bulkSync += o.bulkSync[d]
	}
	return gossip, bulkSync
}

type sample struct {
	Perturbation string
	// Nodes is the cluster size; Converged is how many of them the sample
	// waited for. They differ when a perturbation excludes a node from the
	// target, as the churn measurement excludes its flapper -- without the
	// second column a churn sample is silently a weaker measurement than the
	// quiet sample it sits next to.
	Nodes     int
	Converged int
	Elapsed   time.Duration
	Polls     int
	// Deliveries of the tracked key to the nodes the sample waited for, split
	// by how each arrived. Both zero for a change which is not a table event.
	Gossip   int
	BulkSync int
}

// runStamp distinguishes one test binary invocation from the next, in the style
// of rapid's fail files: a wall-clock timestamp and a pid.
//
// It has to be captured here, at package initialization, rather than where it is
// used. recordSample runs inside a [testing/synctest] bubble, and time.Now()
// there reads the bubble's fake clock -- which starts at the same instant in
// every bubble, so it cannot tell two runs apart, or even two samples.
var runStamp = fmt.Sprintf("%s-%d", time.Now().Format("20060102150405"), os.Getpid())

// recordSample appends one observation to a CSV on disk for later analysis.
//
// The file is named for the fact that these are virtual-time measurements: they
// are comparable with each other and not with anything measured on a real
// cluster. It is also named for the run which produced it, so that samples taken
// against different builds of NetworkDB do not accumulate into one
// undifferentiated pool: every row within a file comes from a single invocation,
// and `go test -count=N` collects a distribution into one file rather than
// interleaving N indistinguishable sets of rows.
func recordSample(t *testing.T, s sample) {
	t.Helper()

	if err := os.Mkdir("testdata", 0o755); err != nil && !os.IsExist(err) {
		t.Logf("Could not record sample: creating testdata directory: %v", err)
		return
	}
	f, err := os.OpenFile("testdata/convergence_virtual-"+runStamp+".csv", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Logf("Could not record sample: opening file: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("Could not record sample: closing file: %v", err)
		}
	}()
	w := csv.NewWriter(f)
	if st, err := f.Stat(); err != nil {
		t.Logf("Could not record sample: stat: %v", err)
		return
	} else if st.Size() == 0 {
		if err := w.Write([]string{
			"Perturbation", "Nodes", "Converged", "VirtualConvergence(ns)",
			"Polls", "GossipDeliveries", "BulkSyncDeliveries",
		}); err != nil {
			t.Logf("Could not record sample: writing header: %v", err)
			return
		}
	}
	if err := w.Write([]string{
		s.Perturbation,
		strconv.Itoa(s.Nodes),
		strconv.Itoa(s.Converged),
		strconv.FormatInt(int64(s.Elapsed), 10),
		strconv.Itoa(s.Polls),
		strconv.Itoa(s.Gossip),
		strconv.Itoa(s.BulkSync),
	}); err != nil {
		t.Logf("Could not record sample: writing row: %v", err)
		return
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Logf("Could not record sample: flushing row: %v", err)
	}
}

// Convergence under churn.
//
// TestNetworkDBConvergenceLatency measures a quiet cluster, which is the right
// baseline but not the condition under which NetworkDB spends its life. It also
// never reaches the slow mode of the distribution: with nothing else in flight,
// gossip essentially always delivers, and the anti-entropy sweep behind it never
// gets a job to do. The churn-driven property test hits that mode in about 7% of
// runs, so the mechanism is reachable -- just not from a standing start.
//
// This variant keeps everything that makes the measurement trustworthy -- one
// tracked write, a Lamport-time target, the clock started before the change, the
// mechanism recorded rather than inferred -- and adds continuous background
// traffic for the duration: junk writes from rotating nodes, and one node
// repeatedly leaving and rejoining the network.
//
// Convergence is required of the nodes which do not flap. A node outside the
// network legitimately does not need the entry, so including the flapper would
// make the target depend on where in its cycle it happened to be.

var churnClusterSizes = []int{5, 10, 25}

const (
	// One churn step is applied every churnInterval of virtual time for as long
	// as the measurement runs.
	churnInterval = 20 * time.Millisecond

	// Junk entries written per step, from a rotating source node. This is what
	// puts the tracked write in a queue rather than alone in a gossip packet.
	churnWritesPerStep = 8

	// Total junk entries, after which the writes stop and only the membership
	// flapping continues. Bounds the CPU a slow sample can cost: a run which
	// falls through to anti-entropy polls for tens of seconds of virtual time.
	churnMaxEntries = 2000

	// Steps between membership flaps. Leaving and rejoining forces the peer to
	// re-acquire state, which is expensive on a large cluster, so it happens
	// much less often than the writes.
	churnStepsPerFlap = 5
)

// TestNetworkDBConvergenceUnderChurn measures how long one write takes to reach
// every stable node while the cluster is busy.
//
// Results land in the same CSV as TestNetworkDBConvergenceLatency, under
// perturbation names suffixed "-under-churn", so the two conditions can be
// compared directly.
func TestNetworkDBConvergenceUnderChurn(t *testing.T) {
	requireSynctest(t)
	for _, nodes := range churnClusterSizes {
		t.Run(fmt.Sprintf("nodes=%d", nodes), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				measureUnderChurn(t, nodes)
			})
		})
	}
}

func measureUnderChurn(t *testing.T, nodes int) {
	const (
		nw         = "nw0"
		trackedKey = "tracked"
	)

	var obs mechanismObserver
	conf := DefaultConfig()
	conf.tableEventObserver = obs.record

	c := newMemCluster(t, nodes, "node", conf, 0)

	for _, db := range c.dbs {
		assert.NilError(t, db.JoinNetwork(nw))
	}
	wantMembers := make([]string, 0, len(c.dbs))
	for _, db := range c.dbs {
		wantMembers = append(wantMembers, db.config.NodeID)
	}
	c.settle(t, nw, wantMembers)
	obs.reset()
	c.mn.resetDropCount()

	// The tracked write comes from the first node, which takes no part in the
	// churn. An entry is only accepted from a node the receiver believes
	// participates in the network, so a writer that flapped would make the
	// target unreachable rather than merely late -- a hang, not a measurement.
	writer := c.dbs[0]
	flapper := c.dbs[len(c.dbs)-1]
	stable := c.dbs[:len(c.dbs)-1]
	junkSources := c.dbs[1 : len(c.dbs)-1]

	// The flapper starts out joined, along with everyone else.
	ch := &churnGenerator{nw: nw, flapper: flapper, junkSources: junkSources, joined: true}

	start := time.Now()
	assert.NilError(t, writer.CreateEntry(tableUnderTest, nw, trackedKey, []byte("v")))
	ltime := entryLTime(t, writer, nw, trackedKey)
	converged := entryReached(nw, trackedKey, ltime, false)

	var polls int
	nextChurn := start
	deadline := start.Add(latencyCeiling)
	for {
		synctest.Wait()
		polls++
		if allConverged(stable, converged) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("tracked write on %d nodes did not reach all %d stable nodes within %v of virtual time; %d behind, after %d churn steps:\n%s",
				nodes, len(stable), latencyCeiling, countBehind(stable, converged), ch.steps, c.dumpMembership(nw))
		}
		if !time.Now().Before(nextChurn) {
			ch.step()
			nextChurn = time.Now().Add(churnInterval)
		}
		time.Sleep(latencyStep)
	}
	elapsed := time.Since(start)

	assert.Equal(t, c.mn.dropCount(), int64(0), "in-memory network dropped datagrams; measurement is not usable")

	gossip, bulkSync := obs.deliveriesOf(trackedKey, stable)
	t.Logf("tracked write on %d nodes reached %d stable nodes in %v of virtual time (%d polls, %d churn steps, %d junk entries, %d by gossip, %d by bulk sync)",
		nodes, len(stable), elapsed, polls, ch.steps, ch.written, gossip, bulkSync)
	recordSample(t, sample{
		Perturbation: "create-entry-under-churn",
		Nodes:        nodes,
		// Only the non-flapping nodes are required to converge; recording the
		// cluster size alone would make this look like a like-for-like sample
		// against the quiet measurements, which wait for every node.
		Converged: len(stable),
		Elapsed:   elapsed,
		Polls:     polls,
		Gossip:    gossip,
		BulkSync:  bulkSync,
	})
}

// churnGenerator produces background traffic on a network: junk entries from
// rotating source nodes, and one node leaving and rejoining.
//
// It is driven from the measuring goroutine rather than its own, so that churn
// is interleaved with the convergence checks at a known rate instead of racing
// them. Errors are ignored throughout: this is noise, and a leave or join can
// legitimately fail if the node is already in the state asked for.
type churnGenerator struct {
	nw          string
	flapper     *NetworkDB
	junkSources []*NetworkDB

	steps   int
	written int
	joined  bool // whether the flapper is currently in the network
}

func (g *churnGenerator) step() {
	g.steps++

	if g.written < churnMaxEntries && len(g.junkSources) > 0 {
		src := g.junkSources[g.steps%len(g.junkSources)]
		for i := range churnWritesPerStep {
			key := fmt.Sprintf("junk-%d-%d", g.steps, i)
			if err := src.CreateEntry(tableUnderTest, g.nw, key, []byte("junk")); err == nil {
				g.written++
			}
		}
	}

	if g.steps%churnStepsPerFlap != 0 {
		return
	}
	if g.joined {
		_ = g.flapper.LeaveNetwork(g.nw)
	} else {
		_ = g.flapper.JoinNetwork(g.nw)
	}
	g.joined = !g.joined
}

// TestNetworkDBBulkSyncCarriesAttachments pins the invariant that a bulk sync
// carries the network attachments its table entries are predicated on, and
// carries them first.
//
// handleTableEvent rejects an entry from a node it does not record as
// participating in the network, and that record is otherwise disseminated only
// by a one-shot NetworkEvent broadcast with a bounded retransmit budget. A node
// which misses that broadcast rejects the owner's entries indefinitely --
// including the ones every bulk sync hands it -- unless the sync repairs the
// membership too.
//
// Driving exactly one bulkSyncNode is what makes this a test of ordering rather
// than merely of presence. handleCompound processes a compound message's parts
// in order, so attachments appended after the entries would leave those entries
// rejected on this sync and accepted only on a later one -- which a test that
// waited for eventual convergence would pass without noticing.
func TestNetworkDBBulkSyncCarriesAttachments(t *testing.T) {
	requireSynctest(t)
	synctest.Test(t, func(t *testing.T) {
		const (
			nw  = "nw0"
			key = "k0"
		)

		c := newMemCluster(t, 2, "node", DefaultConfig(), 0)
		a, b := c.dbs[0], c.dbs[1]
		aID, bID := a.config.NodeID, b.config.NodeID

		for _, db := range c.dbs {
			assert.NilError(t, db.JoinNetwork(nw))
		}
		c.settle(t, nw, []string{aID, bID})

		// Make a forget that b participates in nw, leaving it exactly as
		// deleteNodeFromNetworks does: the attachment record gone and the owner
		// out of the per-network peer list. Dropping only the peer list would
		// not reproduce anything reachable -- a would still hold an attachment
		// record no staler than the repair, and handleNetworkEvent would
		// discard the repair as stale.
		a.Lock()
		delete(a.networks[bID], nw)
		a.deleteNetworkNode(nw, bID)
		a.Unlock()

		// b writes after the divergence. The clock is not advanced from here
		// on, so no gossip round, push/pull or periodic sync runs: the entry
		// has exactly one way to reach a, and it is the sync below. Advancing
		// the clock instead would make this flaky, since memberlist staggers
		// the first push/pull uniformly over PushPullInterval and that would
		// sometimes repair a on its own.
		assert.NilError(t, b.CreateEntry(tableUnderTest, nw, key, []byte("v0")))
		synctest.Wait()
		_, err := a.GetEntry(tableUnderTest, nw, key)
		assert.Check(t, err != nil, "a already holds the entry, so this test would pass without proving anything")

		// Solicited, not unsolicited: an unsolicited sync makes a answer with
		// one of its own, and bulkSyncNode then blocks on the ack.
		assert.NilError(t, b.bulkSyncNode([]string{nw}, aID, false))
		synctest.Wait()

		got, err := a.GetEntry(tableUnderTest, nw, key)
		assert.NilError(t, err, "a rejected the bulk-synced entry: the sync did not carry b's attachment ahead of it")
		assert.Equal(t, string(got), "v0")
	})
}

// TestNetworkDBBulkSyncSkipsAttachmentsForOldPeers is the counterpart to the
// test above. A daemon which does not advertise Lamport-time-aware invalidation
// must not be sent network events in a bulk sync: it would queue relays it can
// then evict out of order, gossiping a stale attachment. It keeps the
// convergence gap the sync would have closed until it is upgraded, which is the
// conservative side to err on during a rolling upgrade.
func TestNetworkDBBulkSyncSkipsAttachmentsForOldPeers(t *testing.T) {
	requireSynctest(t)
	synctest.Test(t, func(t *testing.T) {
		const (
			nw  = "nw0"
			key = "k0"
		)

		c := newMemCluster(t, 2, "node", DefaultConfig(), 0)
		a, b := c.dbs[0], c.dbs[1]
		aID, bID := a.config.NodeID, b.config.NodeID

		for _, db := range c.dbs {
			assert.NilError(t, db.JoinNetwork(nw))
		}
		c.settle(t, nw, []string{aID, bID})

		// Let b see a as a daemon from before node metadata carried a version.
		b.Lock()
		b.nodes[aID].Meta = nil
		b.Unlock()

		a.Lock()
		delete(a.networks[bID], nw)
		a.deleteNetworkNode(nw, bID)
		a.Unlock()

		assert.NilError(t, b.CreateEntry(tableUnderTest, nw, key, []byte("v0")))
		synctest.Wait()
		assert.NilError(t, b.bulkSyncNode([]string{nw}, aID, false))
		synctest.Wait()

		_, err := a.GetEntry(tableUnderTest, nw, key)
		assert.Check(t, err != nil,
			"a accepted the entry, so the sync carried attachments to a peer which cannot safely take them")
	})
}

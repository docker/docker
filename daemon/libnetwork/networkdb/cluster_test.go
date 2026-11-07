package networkdb

import (
	"context"
	"iter"
	"maps"
	"math"
	"math/bits"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"pgregory.net/rapid"
)

func TestTriggerFuncStagger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		epoch := time.Now()

		ndb := &NetworkDB{
			rng: rand.New(new(rand.PCG)),
		}
		var calls []time.Duration
		ndb.goTriggerFunc(t.Context().Done(), time.Second, func() {
			calls = append(calls, time.Since(epoch))
		})

		time.Sleep(10 * time.Second)
		synctest.Wait()

		assert.Check(t, is.Len(calls, 9))
		if len(calls) > 0 {
			assert.Check(t, calls[0] > time.Second, "first call was not staggered: called after %v delay", calls[0])
			assert.Check(t, calls[0] < 2*time.Second, "first call was staggered by more than interval: called after %v delay", calls[0])
		}

		for i := 1; i < len(calls); i++ {
			delta := calls[i] - calls[i-1]
			jitter := delta - time.Second
			if jitter < 0 {
				jitter = -jitter
			}
			assert.Check(t, jitter < time.Millisecond, "calls were not spaced by interval: call %d was %v after call %d, want ~1s", i, delta, i-1)
		}
	})
}

// TestBulkSyncTablesCrossesOverlappingNetworks guards the order bulkSyncTables
// visits networks in.
//
// bulkSync returns every network it found in common with the peer it picked, and
// bulkSyncTables strikes all of them off its worklist. So a network which is
// always visited after one it overlaps with is always struck off before it picks
// a peer of its own, and only ever syncs with the members it shares that earlier
// network with. Two groups overlapping on a single network stay partitioned:
// gossip which misses an entry is never repaired by anti-entropy.
//
// Here nodes 0 and 1 share a-left, nodes 2 and 3 share b-right, and all four
// share z-shared, which sorts last. Visiting networks in a fixed order leaves the
// entry stranded in the first group.
func TestBulkSyncTablesCrossesOverlappingNetworks(t *testing.T) {
	requireSynctest(t)
	synctest.Test(t, func(t *testing.T) {
		c := newMemCluster(t, 4, "node", DefaultConfig())
		members := func(indices ...int) []string {
			out := make([]string, 0, len(indices))
			for _, i := range indices {
				out = append(out, c.dbs[i].config.NodeID)
			}
			return out
		}
		for _, i := range []int{0, 1} {
			assert.NilError(t, c.dbs[i].JoinNetwork("a-left"))
		}
		for _, i := range []int{2, 3} {
			assert.NilError(t, c.dbs[i].JoinNetwork("b-right"))
		}
		for _, db := range c.dbs {
			assert.NilError(t, db.JoinNetwork("z-shared"))
		}
		c.settle(t, "a-left", members(0, 1))
		c.settle(t, "b-right", members(2, 3))
		c.settle(t, "z-shared", members(0, 1, 2, 3))

		assert.NilError(t, c.dbs[0].CreateEntry(tableUnderTest, "z-shared", "key", []byte("value")))
		// Enough rounds that every node has visited z-shared first several times
		// over; one round is not enough even when the order does vary.
		for range 20 {
			for _, db := range c.dbs {
				db.bulkSyncTables()
			}
			synctest.Wait()
		}
		for i, db := range c.dbs {
			_, err := db.GetEntry(tableUnderTest, "z-shared", "key")
			assert.NilError(t, err, "node %d never received the entry", i)
		}
	})
}

func TestMRandomNodes(t *testing.T) {
	cfg := DefaultConfig()
	// The easiest way to ensure that we don't accidentally generate node
	// IDs that match the local one is to include runes that the generator
	// will never emit.
	cfg.NodeID = "_thisnode"
	uut := newNetworkDB(cfg)

	t.Run("EmptySlice", func(t *testing.T) {
		sample := uut.mRandomNodes(3, nil)
		assert.Check(t, is.Len(sample, 0))
	})

	t.Run("OnlyLocalNode", func(t *testing.T) {
		sample := uut.mRandomNodes(3, []string{cfg.NodeID})
		assert.Check(t, is.Len(sample, 0))
	})

	gen := rapid.Custom(func(t *rapid.T) []string {
		s := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{10}`), 0, 100, rapid.ID).Draw(t, "node-names")
		insertPoint := rapid.IntRange(0, len(s)).Draw(t, "insertPoint")
		return slices.Insert(s, insertPoint, cfg.NodeID)
	})

	rapid.Check(t, func(t *rapid.T) {
		nodes := gen.Draw(t, "nodes")
		m := rapid.IntRange(0, len(nodes)).Draw(t, "m")

		takeSample := func() []string {
			sample := uut.mRandomNodes(m, nodes)
			assert.Check(t, is.Len(sample, min(m, len(nodes)-1)))
			assert.Check(t, is.Equal(slices.Index(sample, cfg.NodeID), -1), "sample contains local node ID\n%v", sample)
			assertUniqueElements(t, sample)
			return sample
		}

		p := kpermutations(uint64(len(nodes)-1), uint64(m))
		switch {
		case p <= 1:
			// Only one permutation is possible, so cannot test randomness.
			// Assert the other properties by taking a few samples.
			for range 100 {
				_ = takeSample()
			}
			return
		case p <= 10:
			// With a small number of possible k-permutations, we
			// can feasibly test how many samples it takes to get
			// all of them.
			seen := make(map[string]bool)
			var i int
			for i = range 10000 {
				sample := takeSample()
				seen[strings.Join(sample, ",")] = true
				if len(seen) == int(p) {
					break
				}
			}
			assert.Check(t, is.Len(seen, int(p)), "did not see all %d permutations after %d trials", p, i+1)
			t.Logf("saw all %d permutations after %d samples", p, i+1)
		default:
			uniques := 0
			sample1 := takeSample()
			for range 10 {
				sample2 := takeSample()
				if !slices.Equal(sample1, sample2) {
					uniques++
				}
			}
			assert.Check(t, uniques > 0, "mRandomNodes returned the same sample multiple times")
		}

		// We are testing randomness so statistical outliers are
		// occasionally expected even when the probability
		// distribution is uniform. Run multiple trials to make
		// test flakes unlikely in practice.
		extremes := 0
		for range 10 {
			counts := make(map[string]int)
			for _, n := range nodes {
				if n != cfg.NodeID {
					counts[n] = 0
				}
			}
			const samples = 10000
			for range samples {
				for _, n := range uut.mRandomNodes(m, nodes) {
					counts[n]++
				}
			}
			// Adding multiple samples together should yield a normal distribution
			// if the samples are unbiased.
			mean, stdev, minv, maxv := distributionStats(maps.Values(counts))
			if minv < mean-4*stdev || maxv > mean+4*stdev {
				extremes++
				t.Logf("Mean: %f, StdDev: %f, Min: %f, Max: %f", mean, stdev, minv, maxv)
			}
		}
		assert.Check(t, extremes <= 2, "outliers in distribution: %d/10 trials, expected <2/10", extremes)
	})
}

func assertUniqueElements[S ~[]E, E comparable](t rapid.TB, s S) {
	t.Helper()
	counts := make(map[E]int)
	for _, e := range s {
		counts[e]++
	}
	for e, c := range counts {
		assert.Equal(t, c, 1, "element %v appears more than once in the slice", e)
	}
}

// kpermutations returns P(n,k), the number of permutations of k elements chosen
// from a set of size n. The calculation is saturating: if the result is larger than
// can be represented by a uint64, math.MaxUint64 is returned.
func kpermutations(n, k uint64) uint64 {
	if k > n {
		return 0
	}
	if k == 0 || n == 0 {
		return 1
	}
	p := uint64(1)
	for i := range k {
		var hi uint64
		hi, p = bits.Mul64(p, n-i)
		if hi != 0 {
			return math.MaxUint64
		}
	}
	return p
}

// distributionStats computes mean, population standard deviation, min, and max
// over the values yielded by the iterator.
func distributionStats(vals iter.Seq[int]) (mean, stdev, minv, maxv float64) {
	var sum, sumSq float64
	var n int
	minv = math.MaxFloat64
	maxv = -math.MaxFloat64
	for v := range vals {
		f := float64(v)
		sum += f
		sumSq += f * f
		minv = min(minv, f)
		maxv = max(maxv, f)
		n++
	}
	if n == 0 {
		return 0, 0, 0, 0
	}
	mean = sum / float64(n)
	stdev = math.Sqrt(sumSq/float64(n) - mean*mean)
	return mean, stdev, minv, maxv
}

// TestBootstrapPeersToRejoin covers how a bootstrap address is read before it
// is compared with the cluster's peers. Both readings are load-bearing and
// neither is exercised by the tests which drive a real cluster: those name
// their bootstrap nodes as ip:port and set BindPort explicitly, so the address
// arrives in the one form which needs no interpretation.
func TestBootstrapPeersToRejoin(t *testing.T) {
	// A node gossiping on 7946 with config.BindPort left zero, which is what
	// the daemon does: it never sets BindPort, so memberlist picks its own
	// default and the port we gossip on is not the one in the config.
	db := newNetworkDB(DefaultConfig())
	defer db.broadcaster.Close()
	assert.Assert(t, is.Equal(db.config.BindPort, 0), "the point of the test is that BindPort is unset")

	db.config.NodeID = "self"
	// net.ParseIP yields the 4-in-6 form, which is how memberlist holds an
	// IPv4 peer address.
	db.nodes["self"] = &node{Node: memberlist.Node{Name: "self", Addr: net.ParseIP("192.0.2.1"), Port: 7946}}
	db.nodes["peer"] = &node{Node: memberlist.Node{Name: "peer", Addr: net.ParseIP("192.0.2.2"), Port: 7946}}

	for _, tc := range []struct {
		name  string
		peers []string
		want  []string
	}{
		{name: "none known", peers: nil, want: []string{}},
		{name: "peer by ip:port", peers: []string{"192.0.2.2:7946"}, want: []string{}},
		// Read on the port we gossip on. Read on config.BindPort instead and
		// this is 192.0.2.2:0, which matches nothing and is rejoined forever.
		{name: "peer without a port", peers: []string{"192.0.2.2"}, want: []string{}},
		// The peer side is unmapped before it is compared, so the bootstrap
		// side has to be too, or these never match.
		{name: "peer as 4-in-6 with port", peers: []string{"[::ffff:192.0.2.2]:7946"}, want: []string{}},
		{name: "peer as 4-in-6 without a port", peers: []string{"::ffff:192.0.2.2"}, want: []string{}},
		{name: "ourselves", peers: []string{"192.0.2.1"}, want: []string{}},
		{name: "unparseable", peers: []string{"not-an-address"}, want: []string{}},
		{name: "an absent bootstrap node", peers: []string{"192.0.2.9:7946"}, want: []string{"192.0.2.9:7946"}},
		{name: "a peer on another port", peers: []string{"192.0.2.2:9999"}, want: []string{"192.0.2.2:9999"}},
		// Being attached to one bootstrap node says nothing about the others.
		{
			name:  "only the ones which are absent",
			peers: []string{"192.0.2.2", "192.0.2.9:7946", "192.0.2.1"},
			want:  []string{"192.0.2.9:7946"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Check(t, is.DeepEqual(db.bootstrapPeersToRejoin(tc.peers), tc.want))
		})
	}
}

// TestRetryJoinAttemptsBeforeTicking pins that retryJoin makes its first
// attempt straight away. Waiting out a tick first makes the whole call a no-op
// for any caller whose budget is shorter than retryInterval, and no test which
// drives a real cluster would notice: they all allow a budget several ticks
// long.
func TestRetryJoinAttemptsBeforeTicking(t *testing.T) {
	conf := DefaultConfig()
	conf.BindAddr = "127.0.0.1"
	conf.AdvertiseAddr = conf.BindAddr

	launch := func(name string) *NetworkDB {
		t.Helper()
		c := *conf
		c.Hostname = name
		c.NodeID = stringid.TruncateID(stringid.GenerateRandomID())
		c.BindPort = int(dbPort.Add(1))
		return launchNode(t, c)
	}
	// Deliberately not joined to each other: retryJoin is the only thing which
	// puts them in touch.
	a, b := launch("node1"), launch("node2")
	defer closeNetworkDBInstances(t, []*NetworkDB{a, b})

	ctx, cancel := context.WithTimeout(t.Context(), retryInterval/10)
	defer cancel()
	a.retryJoin(ctx, []string{net.JoinHostPort(b.config.AdvertiseAddr, strconv.Itoa(b.config.BindPort))})

	poll.WaitOn(t, func(t poll.LogT) poll.Result {
		a.RLock()
		defer a.RUnlock()
		if _, ok := a.nodes[b.config.NodeID]; ok {
			return poll.Success()
		}
		return poll.Continue("waiting for the join made inside the budget to have taken")
	}, poll.WithDelay(100*time.Millisecond), poll.WithTimeout(30*time.Second))
}

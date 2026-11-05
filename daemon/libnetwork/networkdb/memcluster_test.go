package networkdb

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing/synctest"
	"time"

	"github.com/containerd/log"
	"github.com/hashicorp/memberlist"
	"gotest.tools/v3/assert"
)

// Helpers for running a cluster of NetworkDB instances inside a
// [testing/synctest] bubble, gossiping over the in-memory network implemented at
// the bottom of this file rather than over real sockets.
//
// A goroutine parked on a socket is not *durably* blocked, so a bubble
// containing real network I/O never quiesces and its clock never advances.
// Substituting the transport makes the whole cluster timer-driven, which buys
// two things: waits cost nothing, and [synctest.Wait] becomes an exact "the
// cluster has finished reacting to everything it already knows" signal.
//
// Everything in this file may only be called from inside a bubble.

// tableUnderTest is the single NetworkDB table these tests read and write.
const tableUnderTest = "some_table"

// Virtual-time budgets. These are spent from the bubble's fake clock, so a
// generous budget costs nothing but the CPU of the checks it performs.
const (
	formationStep    = 100 * time.Millisecond
	formationTimeout = 60 * time.Second

	// shutdownDrain has to exceed the longest wait Close() cannot cut short.
	// That is bulkSyncNode's ack wait: a flat 30s timer with no ctx case
	// (cluster.go), so cancelCtx does not touch it, and bulkSyncTables works
	// through its networks serially, so one latched tick can arm it once per
	// network -- 5 in TestNetworkDBAlwaysConverges, hence 150s. At exactly 30s
	// the budget covered one network and nothing else, and a sync armed at or
	// after the moment the teardown starts counting outlived it, leaving a
	// goroutine parked when the bubble ended: a deadlock panic that takes the
	// whole test binary with it.
	shutdownDrain = 3 * time.Minute
)

// memCluster is a cluster of NetworkDB instances on a private in-memory network.
type memCluster struct {
	dbs []*NetworkDB
	mn  *memNetwork
}

// staggerStream separates a cluster's launch-offset draws from its per-node
// gossip streams, which newMemCluster fans out of the last eight bytes of the
// same seed. "stagger." as bytes.
const staggerStream = 0x737461676765722e

// newMemCluster brings up num NetworkDB instances wired to a private in-memory
// network and waits for every node to see every other. The cluster is torn down
// by a [testing.T.Cleanup] registered here, so callers need no teardown of their
// own.
//
// stagger bounds how far apart their creation is spread, and so how far apart
// the phases of their periodic work fall; zero creates them together, in phase.
//
// Owning the teardown matters, rather than leaving a deferred one to each
// caller. launchNode and the formation wait below are both fatal, and a fatal
// inside a bubble runtime.Goexit()s the bubble's root goroutine -- at which
// point a caller's defer has not been reached yet, because this function has not
// returned. The nodes already launched would still be running when the bubble
// ends, which synctest reports as a deadlock panic: it kills the whole test
// binary and buries the diagnostic those two were trying to print. Cleanups run
// on the Goexit and, in a bubble, run inside it -- synctest.Test invokes the
// property through tRunner, and rapid.SyncTest calls t.cleanup() from a defer
// within its bubble -- so the teardown can still use the fake clock.
//
// conf is used as a template; the per-node identity and address fields are
// filled in here. If conf.tableEventObserver is set it is shared by every node,
// so an observer must be safe for concurrent use.
//
// Log output is capped at warn for the cluster's lifetime. TestMain runs the
// package at debug, which a socket-based test with a handful of nodes can
// afford; a bubble cannot. NetworkDB logs a line per peer pair at info, so
// cluster formation alone is O(n^2), and these tests build hundreds of clusters
// of up to 25 nodes. Left at debug they bury a failing subtest under tens of
// thousands of lines from the ones that passed.
func newMemCluster(t TestingT, num int, namePrefix string, conf *Config, stagger time.Duration) *memCluster {
	t.Helper()

	if logLevel := log.GetLevel(); logLevel > log.WarnLevel {
		_ = log.SetLevel(log.WarnLevel)
		t.Cleanup(func() { _ = log.SetLevel(logLevel) })
	}

	c := &memCluster{mn: &memNetwork{}}

	t.Cleanup(func() {
		closeNetworkDBInstances(t, c.dbs)
		// [NetworkDB.Close] stops the cluster but does not join
		// everything it stops: some goroutines are parked in a bounded
		// wait -- a broadcast ack (5s), a probe RTT, a stream timeout
		// (10s) -- which has to lapse before they notice the shutdown
		// and return. The bubble ends the moment the test function
		// returns and reports any goroutine still running as a
		// deadlock, so the budget here has to exceed the longest such
		// wait. It is spent from the fake clock, so a generous one is
		// free: nothing is runnable, and the sleep returns at once.
		time.Sleep(shutdownDrain)
		synctest.Wait()
	})

	// Configs first, and sequentially: memNetwork hands out addresses from a
	// counter, so allocating them concurrently would leave a node's address
	// decided by the scheduler and stop a recorded failure from replaying.
	configs := make([]Config, num)
	for i := range num {
		tr := c.mn.NewTransport()
		addr := tr.AddrPort()

		localConfig := *conf
		localConfig.Hostname = fmt.Sprintf("%s%d", namePrefix, i+1)
		// Derived from the index, not stringid.GenerateRandomID, which reads
		// crypto/rand: identical clusters across runs make a failure dump
		// diffable against the next run's, and stop node identity from being
		// one more thing a replay cannot reproduce. Twelve characters, matching
		// what TruncateID would have produced, and never all-digits.
		localConfig.NodeID = fmt.Sprintf("node%08d", i+1)
		if conf.rngSeed != nil {
			// One stream per node, all derived from the template's seed, so a
			// single recorded value reproduces the whole cluster's gossip
			// without every node making identical choices.
			seed := *conf.rngSeed
			binary.LittleEndian.PutUint64(seed[24:], uint64(i))
			localConfig.rngSeed = &seed
		}
		localConfig.transport = tr
		localConfig.BindAddr = addr.Addr().String()
		localConfig.AdvertiseAddr = localConfig.BindAddr
		localConfig.BindPort = int(addr.Port())

		configs[i] = localConfig
	}

	// Then bring every node up from its own goroutine, each after the offset the
	// caller asked for. Creation is the only place this can be done: Create
	// calls memberlist's schedule(), which builds the probe and gossip tickers,
	// and clusterInit starts NetworkDB's own triggers, so the instant a node is
	// created fixes the phase of every periodic task it will ever run. Nothing
	// afterwards moves that phase -- memberlist's randStagger cannot, since
	// schedule() builds the ticker before the goroutine which reads it, and a
	// stagger shorter than the interval never even skips a tick. A harness which
	// wants those phases spread has to space the creations itself, and must not
	// leave the spacing to whatever Join happens to cost.
	//
	// Spacing them here, before any Join, costs nothing but clock: a node with no
	// peers has nobody to gossip with while it waits its turn. The offsets come
	// from their own stream off the template seed -- eight bytes nothing else
	// derives from -- so spreading the launches leaves every node's gossip choices
	// exactly as they would have been, and the arrangement follows from the seed
	// rather than having to be recorded a node at a time.
	offsets := make([]time.Duration, num)
	if stagger > 0 {
		var seed [32]byte
		if conf.rngSeed != nil {
			seed = *conf.rngSeed
		} else {
			_, _ = cryptorand.Read(seed[:]) // Documented never to return an error
		}
		binary.LittleEndian.PutUint64(seed[16:], staggerStream)
		rng := rand.New(rand.NewChaCha8(seed)) //gosec:disable G404 -- not used in a security sensitive context
		for i := range offsets {
			offsets[i] = time.Duration(rng.Int64N(int64(stagger)))
		}
	}

	dbs := make([]*NetworkDB, num)
	errs := make([]error, num)
	var wg sync.WaitGroup
	for i := range num {
		wg.Go(func() {
			if offsets[i] > 0 {
				time.Sleep(offsets[i])
			}
			dbs[i], errs[i] = New(&configs[i])
		})
	}
	wg.Wait()
	// Register whatever came up before reporting any failure, so the cleanup
	// above closes it either way. The reporting happens here and not inside the
	// goroutines because t.Fatal has to run on the test's own goroutine.
	for _, db := range dbs {
		if db != nil {
			c.dbs = append(c.dbs, db)
		}
	}
	for _, err := range errs {
		assert.NilError(t, err)
	}

	for i, db := range c.dbs {
		if i > 0 {
			// Seed the newcomer with every node already up, not just the
			// previous one. Join push/pulls with each seed synchronously, so
			// all of them learn the newcomer before Join returns and formation
			// never waits on an epidemic to spread it. That is not the same as
			// costing no clock -- a join spends a few hundred milliseconds of
			// it, and forming nineteen nodes some seconds in total -- but what
			// it costs does not turn on a race.
			//
			// Chain-joining left the newcomer known only to its one seed, and
			// its spread to everyone else rode memberlist's alive broadcast:
			// a one-shot epidemic whose budget is
			// RetransmitMult*ceil(log10(N+1)) transmissions, spent one per
			// recipient, so gossiping to GossipNodes peers costs that many at
			// once. At N=9 the budget is 4 against 8 peers -- the worst ratio
			// in the range these tests use, since the ceil(log10) step to 8
			// lands at N=10 -- and roughly 10% of formations lost the race and
			// fell back to memberlist's push/pull anti-entropy: one random
			// peer per node per 30s, several rounds to spread from a single
			// origin, 15-60s of virtual time. That tail sat right on
			// formationTimeout, so about 1 run in 171 timed out here.
			seeds := make([]string, 0, i)
			for _, prev := range c.dbs[:i] {
				seeds = append(seeds, net.JoinHostPort(prev.config.AdvertiseAddr, strconv.Itoa(prev.config.BindPort)))
			}
			assert.Check(t, db.Join(seeds))
		}
	}

	// Formation is synchronous now, so this should pass on its first look. It
	// stays as a guard: if a future change makes membership depend on gossip
	// again, this reports it here rather than as a puzzling failure later.
	incomplete := func(db *NetworkDB) bool { return len(db.ClusterPeers()) != len(c.dbs) }
	start := time.Now()
	for {
		// Wait before looking, so a check never races the gossip it is
		// waiting on, and so the cluster is always quiesced on return.
		synctest.Wait()
		if !slices.ContainsFunc(c.dbs, incomplete) {
			break
		}
		if time.Since(start) >= formationTimeout {
			var b strings.Builder
			for _, db := range c.dbs {
				fmt.Fprintf(&b, "  %s (%s): %d of %d peers\n",
					db.config.Hostname, db.config.NodeID, len(db.ClusterPeers()), len(c.dbs))
			}
			t.Fatalf("cluster of %d did not form within %v of virtual time:\n%s", len(c.dbs), formationTimeout, b.String())
		}
		time.Sleep(formationStep)
	}
	return c
}

// memNetwork is the in-memory stand-in for the UDP + TCP network that memberlist
// nodes gossip over, built from channels and [net.Pipe] for the reason given at
// the top of this file.
//
// memberlist ships its own [memberlist.MockTransport], which is not usable here
// for three reasons that this type fixes:
//
//   - Its sends block until the peer's handler picks the message up, so a cycle
//     of nodes gossiping at each other can deadlock. Real UDP never blocks the
//     sender, it drops; so does this.
//   - Its Shutdown is a no-op, leaving a closed node reachable. Delivering to a
//     channel nobody will ever read again is a goroutine leak outside a bubble
//     and a hard deadlock inside one.
//   - Its address maps are read and written without synchronization.
type memNetwork struct {
	mu    sync.Mutex
	nodes map[netip.AddrPort]*memTransport
	all   []*memTransport // every transport ever created, for accounting
	next  uint32          // host part of the next address to hand out
}

// memNetworkPort is the port every node on a memNetwork listens on. Nodes are
// distinguished by address, one fictional host each, as they would be in a real
// cluster.
const memNetworkPort = 7946

// packet and stream channel depths. Both are far larger than the number of
// messages a healthy cluster has in flight at once, so the drop path below is
// only reached if something is genuinely wedged.
const (
	memPacketBuffer = 512
	memStreamBuffer = 64
)

// NewTransport returns a transport with a freshly allocated address, wired up to
// every other transport on the same memNetwork.
func (mn *memNetwork) NewTransport() *memTransport {
	mn.mu.Lock()
	defer mn.mu.Unlock()

	mn.next++
	if mn.next > 0xfffe {
		panic("memnet: out of addresses")
	}
	t := &memTransport{
		mn:       mn,
		addr:     netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, byte(mn.next >> 8), byte(mn.next)}), memNetworkPort),
		packetCh: make(chan *memberlist.Packet, memPacketBuffer),
		streamCh: make(chan net.Conn, memStreamBuffer),
		shutdown: make(chan struct{}),
	}
	if mn.nodes == nil {
		mn.nodes = make(map[netip.AddrPort]*memTransport)
	}
	mn.nodes[t.addr] = t
	mn.all = append(mn.all, t)
	return t
}

func (mn *memNetwork) lookup(addr string) *memTransport {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil
	}
	mn.mu.Lock()
	defer mn.mu.Unlock()
	return mn.nodes[ap]
}

func (mn *memNetwork) deregister(addr netip.AddrPort) {
	mn.mu.Lock()
	defer mn.mu.Unlock()
	delete(mn.nodes, addr)
}

// memAddr adapts a [netip.AddrPort] to [net.Addr]. Its String form is the usual
// "ip:port", so anything which parses a peer address off the wire still works.
type memAddr netip.AddrPort

func (a memAddr) Network() string { return "memnet" }
func (a memAddr) String() string  { return netip.AddrPort(a).String() }

// memTransport is one node's attachment to a memNetwork.
type memTransport struct {
	mn       *memNetwork
	addr     netip.AddrPort
	packetCh chan *memberlist.Packet
	streamCh chan net.Conn

	shutdown  chan struct{}
	closeOnce sync.Once

	dropped atomic.Int64
}

var _ memberlist.NodeAwareTransport = (*memTransport)(nil)

// AddrPort returns the address this transport is reachable at.
func (t *memTransport) AddrPort() netip.AddrPort { return t.addr }

func (t *memTransport) FinalAdvertiseAddr(string, int) (net.IP, int, error) {
	return net.IP(t.addr.Addr().AsSlice()), int(t.addr.Port()), nil
}

func (t *memTransport) WriteTo(b []byte, addr string) (time.Time, error) {
	return t.WriteToAddress(b, memberlist.Address{Addr: addr})
}

func (t *memTransport) WriteToAddress(b []byte, a memberlist.Address) (time.Time, error) {
	now := time.Now()

	dst := t.mn.lookup(a.Addr)
	if dst == nil {
		// A datagram addressed to a node which is not listening is
		// silently discarded, with nothing reported to the sender. Do
		// not return an error here: memberlist would treat it as a
		// send failure, which a real UDP socket would never observe.
		return now, nil
	}
	select {
	case <-dst.shutdown:
		return now, nil
	case <-t.shutdown:
		// This node's own transport is down. memberlist shuts the transport
		// down before it stops its own handlers, so a send already in flight
		// can reach here afterwards; the real transport has closed its UDP
		// sockets by then and the write fails. Fail here too, or a closed node
		// goes on originating fresh probes against live peers whose acks can
		// no longer reach it.
		//
		// This does not silence teardown's "UDP probes failed, network may be
		// misconfigured" warnings, and should not: a real node logs them too,
		// for probes already awaiting an ack when Shutdown runs.
		// NetTransport.Shutdown closes listeners only, and its
		// DialAddressTimeout is a bare dialer.Dial, so the UDP ack is lost
		// while the TCP fallback still connects. What this removes is the
		// probes a departed node would otherwise keep starting, which a real
		// one cannot.
		//
		// Deliberately not a *net.OpError: memberlist's failedRemote treats a
		// udp/write OpError as the *peer* having failed and runs the indirect
		// probe and suspicion machinery, which is the opposite of what a node
		// on its way out should provoke. A plain error is a local failure, so
		// probeNode logs and returns.
		return now, fmt.Errorf("memnet: write on shut-down transport %s", t.addr)
	default:
	}

	// memberlist pools the buffers it hands us, and a broadcast passes the
	// same buffer to every recipient, so the receiver needs its own copy.
	pkt := &memberlist.Packet{
		Buf:       bytes.Clone(b),
		From:      memAddr(t.addr),
		Timestamp: now,
	}
	select {
	case dst.packetCh <- pkt:
	default:
		dst.dropped.Add(1)
		// Receive buffer full. Drop it, as a kernel socket buffer
		// would, rather than blocking the sender and risking a cycle of
		// nodes each waiting on the other.
	}
	return now, nil
}

func (t *memTransport) PacketCh() <-chan *memberlist.Packet { return t.packetCh }

func (t *memTransport) DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return t.DialAddressTimeout(memberlist.Address{Addr: addr}, timeout)
}

func (t *memTransport) DialAddressTimeout(a memberlist.Address, timeout time.Duration) (net.Conn, error) {
	dst := t.mn.lookup(a.Addr)
	if dst == nil {
		return nil, fmt.Errorf("memnet: dial %s: connection refused", a.String())
	}

	local, remote := net.Pipe()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case dst.streamCh <- remote:
		// The peer may have shut down between the lookup above and this send.
		// Both this case and the one below are then ready at once and the
		// select picks at random, so half the time the conn is queued anyway --
		// into a channel whose one-shot drain has already run and whose
		// streamListen has already returned. Nothing would ever read or close
		// it, and the caller is memberlist's sendUserMsg, the one stream path
		// which sets no deadline, so its write parks forever and the bubble
		// deadlocks. Re-check and hand back a failure rather than a conn nobody
		// is listening on. If shutdown instead begins just after this check,
		// its drain still runs after ours was queued and closes it.
		select {
		case <-dst.shutdown:
		default:
			return local, nil
		}
	case <-dst.shutdown:
	case <-t.shutdown:
	case <-timer.C:
	}
	_ = local.Close()
	_ = remote.Close()
	return nil, fmt.Errorf("memnet: dial %s: connection refused or timed out", a.String())
}

func (t *memTransport) StreamCh() <-chan net.Conn { return t.streamCh }

func (t *memTransport) Shutdown() error {
	t.closeOnce.Do(func() {
		// Stop accepting first, so no peer can queue anything new, then
		// hang up on whatever is already queued but not yet accepted.
		// Otherwise the dialer at the far end of the pipe stays blocked
		// on a write that will never be read -- a deadlocked bubble.
		t.mn.deregister(t.addr)
		close(t.shutdown)
		for {
			select {
			case c := <-t.streamCh:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

// dropCount reports how many datagrams this network discarded for want of
// receive buffer, across every node, including nodes since shut down.
//
// A healthy run drops nothing. A non-zero count means memPacketBuffer was too
// shallow for the load, and any convergence time measured over that run has
// been inflated by retransmits that a real cluster would not have needed --
// which is worth knowing, because it is otherwise indistinguishable from
// NetworkDB genuinely falling back to its anti-entropy sweep.
func (mn *memNetwork) dropCount() int64 {
	mn.mu.Lock()
	defer mn.mu.Unlock()
	var n int64
	for _, t := range mn.all {
		n += t.dropped.Load()
	}
	return n
}

// resetDropCount zeroes the tally, so that a measurement can assert on drops
// which happened during the measurement rather than on every drop since the
// cluster was created. Cluster formation and the pre-measurement settle are
// the noisiest parts of a run and cannot contaminate a sample taken after
// settle has seen every node agree.
func (mn *memNetwork) resetDropCount() {
	mn.mu.Lock()
	defer mn.mu.Unlock()
	for _, t := range mn.all {
		t.dropped.Store(0)
	}
}

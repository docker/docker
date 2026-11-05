package networkdb

import (
	"encoding/binary"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gotest.tools/v3/assert"
	"pgregory.net/rapid"
)

// Virtual-time budget for the convergence check. Once the state machine has
// stopped mutating, convergence is terminal, so the step only decides how
// promptly the test notices -- it is not a measurement resolution. This test
// makes no timing claims; see TestNetworkDBConvergenceLatency for those.
const (
	convergenceStep    = 100 * time.Millisecond
	convergenceTimeout = 2 * time.Minute
)

// convergenceAttempts is how many times a scenario is executed before it is
// taken to hold. See TestNetworkDBAlwaysConverges for when to raise it.
var convergenceAttempts = flag.Int("networkdb.convergence-attempts", 0,
	"executions of each scenario in the NetworkDB convergence tests; raise to reproduce or minimize a failure. 0, the default, means once when searching, or whatever a scenario file asks for")

// TestNetworkDBAlwaysConverges drives a cluster of NetworkDB instances with a
// random sequence of joins, leaves and table writes, then asserts that every
// node's view of the table ends up identical.
//
// This test exists to explore interleavings, and deliberately never quiesces
// the cluster between actions: gossip is still in flight when the next mutation
// lands, which is where the interesting bugs are. That is also precisely why it
// cannot measure convergence latency -- by the time the property function gets
// to look, most runs have already converged during the waits the state machine
// draws, so any interval it could report would be an upper bound rather
// than a measurement. TestNetworkDBConvergenceLatency trades interleaving for
// precision and measures it properly.
//
// The whole property runs inside a [testing/synctest] bubble, gossiping over an
// in-memory network; see memcluster_test.go for why and how.
//
// # Reproducing a failure
//
// A failure prints the scenario it drew as a plan, in the form parsePlan reads
// back. Whether replaying that plan fails again is a separate question:
// convergence depends on the order gossip interleaves in, that order comes from
// goroutine scheduling, and nothing here controls scheduling. A scenario that
// failed once can pass the next hundred runs, which is what rapid reports as
// "flaky test, can not reproduce a failure".
//
// -networkdb.convergence-attempts is the lever for that, in all three modes
// below. It executes the scenario that many times -- a fresh cluster and a
// different gossip stream each time -- and fails if any execution does, turning
// a failure that shows up one run in thirty into one that shows up in nearly
// every run.
//
// Quickest is to save the printed plan to a file and replay it on its own, with
// no draw buffer, seed or failfile in the way. A plan can also be trimmed by
// hand, which is often faster than asking rapid to minimize it:
//
//	go test ./daemon/libnetwork/networkdb/ -run TestNetworkDBReplayScenario \
//	    -networkdb.convergence-plan=/tmp/plan.txt -networkdb.convergence-attempts=250
//
// A relative path there is resolved against the package directory, since that is
// where go test runs the binary, so an absolute one saves confusion.
//
// -networkdb.convergence-minimize reduces a scenario and measures what the
// results would cost to catch, ready to be saved under testdata/scenarios. It
// takes "diagnose" for the smallest scenario which still fails, or "optimize"
// for the one cheapest to catch, which is rarely the same thing. See
// minimizePlan.
//
// Replaying rapid's own failfile runs the same scenario back through the
// property, which is worth doing when the plan is not the suspect:
//
//	go test ./daemon/libnetwork/networkdb/ -run TestNetworkDBAlwaysConverges \
//	    -rapid.failfile=<failfile> -networkdb.convergence-attempts=250
//
// To have rapid minimize a scenario, run its seed instead. A failfile is only
// ever replayed, never minimized, and rapid picks up any failfile under
// testdata/rapid/ whether or not -rapid.failfile names one -- so the file has to
// be moved aside for the seed to get a look in:
//
//	mv testdata/rapid/TestNetworkDBAlwaysConverges/*.fail /tmp/
//	go test ./daemon/libnetwork/networkdb/ -run TestNetworkDBAlwaysConverges \
//	    -rapid.seed=<seed> -networkdb.convergence-attempts=250 -rapid.shrinktime=10m
//
// Set attempts high enough that a failing scenario fails on nearly every batch,
// not merely most of them. rapid gets a single batch to confirm a failure before
// it starts minimizing, and judges each candidate on a single batch after that,
// reading any batch that passes as the bug being absent. The logged "attempt N of
// M" is the guide: at attempt 29 of 50, one execution finds that scenario about
// 3% of the time, so 50 attempts leaves roughly a one-in-five chance that the
// confirmation batch passes and the failure is written off as flaky. Minimizing
// is also the expensive mode, because rapid spends the whole attempt budget on
// every scenario that passes.
//
// Searching wants the opposite of repetition -- for a fixed execution budget,
// distinct scenarios find more bugs than repeats of one -- so the default is 1.
//
// Once a scenario is minimized and understood, commit it under
// testdata/scenarios so it keeps being run; see TestNetworkDBScenarios. The
// search is not guaranteed to draw it again.
func TestNetworkDBAlwaysConverges(t *testing.T) {
	requireSynctest(t)
	rapid.Check(t, testConvergence)
}

func testConvergence(t *rapid.T) {
	numNodes := rapid.IntRange(2, 25).Draw(t, "numNodes")
	numNetworks := rapid.IntRange(1, 5).Draw(t, "numNetworks")

	// Draw the gossip seed rather than letting each node read crypto/rand, so
	// that it is recorded and shrunk like any other draw instead of being
	// invented. It does not pin the interleaving -- memberlist's own randomness
	// is unseedable and synctest orders the clock, not the scheduler -- but it
	// takes one uncontrolled input out of the picture. A fixed seed would be
	// worse than either: every run would gossip alike, and the point of this
	// test is to explore that space.
	seed := rapid.Uint64().Draw(t, "rngSeed")

	// Draw the whole scenario before executing any of it. Every draw an action
	// makes resolves against the model alone -- which node has joined what,
	// which keys are taken -- and never against the cluster, so the scenario is
	// a pure function of rapid's draw buffer and can be executed as many times
	// as we like. Drawing and executing in one pass, as this test used to, ties
	// the two together: a second execution consumes fresh draws and so runs a
	// different scenario, which is what leaves a flaky failure unreproducible.
	// Drawn once per scenario, not per action: see networkDBFSM.record for why
	// the whole range from burst to spread is worth covering.
	maxDelay := rapid.IntRange(0, 500).Draw(t, "maxDelay")

	// Drawn once per scenario for the same reason as maxDelay: both ends are
	// worth covering, and which end a scenario sits at is a property of the
	// scenario. One gossip interval is the whole useful range -- phase is taken
	// modulo the interval, so a larger bound only spreads the launches further
	// apart without spreading the phases any wider.
	stagger := time.Duration(rapid.IntRange(0, 200).Draw(t, "stagger")) * time.Millisecond

	fsm := newNetworkDBFSM(numNodes, numNetworks, maxDelay)
	t.Repeat(rapid.StateMachineActions(fsm))
	p := &plan{nodes: numNodes, seed: seed, stagger: stagger, actions: fsm.plan}

	t.Logf("Scenario, replayable with -networkdb.convergence-plan:\n%s", p)

	attempts := max(*convergenceAttempts, 1)
	for attempt := range attempts {
		// One bubble per attempt, rather than one bubble running every attempt.
		// newMemCluster registers its teardown with t.Cleanup, and rapid runs
		// those cleanups at the end of the bubble they were registered in, so a
		// shared bubble would keep every attempt's nodes alive until the last
		// one finished -- and a bubble reports a goroutine still running when it
		// ends as a deadlock. Separate bubbles also give each attempt the same
		// virtual-time origin, which keeps their output comparable.
		rapid.SyncTest(t, func(t *rapid.T) {
			executePlan(t, p, attempt, attempts)
		})
	}
}

func (p *plan) clone() *plan {
	c := *p
	c.actions = slices.Clone(p.actions)
	return &c
}

// runPlanOnce executes p against a fresh cluster and reports whether it
// converged: "" if it did, a diff explaining the state it was left in if it did
// not. It must be called inside a [testing/synctest] bubble.
//
// It stops short of calling the answer a test failure, which is executePlan's
// job, because the minimizer asks this question hundreds of times and a run that
// does not converge is the answer it is hoping for.
func runPlanOnce(t TestingT, p *plan, attempt int) outcome {
	t.Helper()

	state, err := p.model()
	assert.NilError(t, err)

	conf := DefaultConfig()
	seed := p.attemptSeed(attempt)
	conf.rngSeed = &seed
	c := newMemCluster(t, p.nodes, "node", conf, p.stagger)

	for _, a := range p.actions {
		a.apply(t, c.dbs)
	}

	want := wantTableState(state, c.dbs)
	diff, got := awaitTableState(c.dbs, want)
	var o outcome
	if diff != "" {
		o.diff = diff + "\n\n" + dumpTables(c.dbs)
		o.networks, o.entries = diverged(want, got)
	}

	if drops := c.mn.dropCount(); drops != 0 {
		// Not a correctness failure -- gossip has to tolerate loss -- but it
		// means memPacketBuffer was too shallow for this load and the run
		// needed more retransmits than a real cluster would have.
		t.Logf("in-memory network dropped %d datagrams for want of receive buffer", drops)
	}
	return o
}

// executePlan runs p once and fails the test if the cluster does not converge.
// It must be called inside a [testing/synctest] bubble.
func executePlan(t TestingT, p *plan, attempt, attempts int) {
	t.Helper()

	o := runPlanOnce(t, p, attempt)
	if o.diff == "" {
		return
	}
	t.Logf("Attempt %d of %d did not converge.\n%v", attempt+1, attempts, o.diff)

	// Deliberately carries no detail. rapid compares failures by their message
	// text: to decide whether a replay reproduced the failure it was asked to
	// minimize, and to decide whether a candidate scenario still fails. A diff or
	// a table dump in here varies with the interleaving, so one bug would report
	// a different failure every run -- which rapid reads as a flaky test and
	// declines to minimize. The detail goes to the log above instead, where
	// nothing compares it.
	//
	// The cost is that every non-convergence looks alike, so minimizing may land
	// on a different scenario than the one that failed. Any scenario that fails
	// to converge is a valid witness for this property, so that is a fair trade.
	t.Errorf("NetworkDB state did not converge within %v of virtual time", convergenceTimeout)
}

// tableStateCmp is shared by the check which ends the wait below and the diff
// which explains a timeout, so the two cannot disagree: on different options the
// wait could end on state the diff calls equal, and print an empty diff.
//
// A network a node has joined but holds no entries for is an empty map on one
// side and, depending on how it was built, nil on the other. That is not a
// difference worth failing on -- what matters is which networks a node lists and
// which keys it holds -- so equate them.
var tableStateCmp = cmp.Options{cmpopts.EquateEmpty()}

// awaitTableState blocks until every node's view of the table under test matches
// want, and returns "" once it does. If that has not happened within
// convergenceTimeout of virtual time it gives up and returns a diff.
func awaitTableState(dbs []*NetworkDB, want tableState) (string, tableState) {
	deadline := time.Now().Add(convergenceTimeout)
	for {
		synctest.Wait()
		got := snapshotTableState(dbs)
		if cmp.Equal(want, got, tableStateCmp) {
			return "", got
		}
		if !time.Now().Before(deadline) {
			return cmp.Diff(want, got, tableStateCmp), got
		}
		time.Sleep(convergenceStep)
	}
}

// outcome is what one execution of a scenario came to.
type outcome struct {
	// diff explains what the cluster was left disagreeing about, and is empty
	// if it converged.
	diff string
	// networks are those some node did not converge on. Reduction uses this to
	// propose dropping the actions of networks which did converge: they are
	// scattered through a scenario, so removing them as a group is a move the
	// block ladder cannot make in one step.
	networks map[string]bool
	// entries are the "network/key" pairs some node did not converge on, for
	// the same purpose one level down.
	entries map[string]bool
}

// diverged reports where got fell short of want. It compares the two states
// rather than reading the printed diff, which is for people.
func diverged(want, got tableState) (networks, entries map[string]bool) {
	networks, entries = map[string]bool{}, map[string]bool{}
	for node, wantNws := range want {
		gotNws := got[node]
		for nw, wantKeys := range wantNws {
			gotKeys, joined := gotNws[nw]
			if !joined {
				networks[nw] = true
				continue
			}
			for k, v := range wantKeys {
				if gotKeys[k] != v {
					networks[nw] = true
					entries[nw+"/"+k] = true
				}
			}
		}
	}
	// A network a node holds but should not is a divergence too.
	for node, gotNws := range got {
		for nw := range gotNws {
			if _, ok := want[node][nw]; !ok {
				networks[nw] = true
			}
		}
	}
	return networks, entries
}

// tableState is every node's view of every network's entries in the table under
// test: node ID -> network -> key -> value.
type tableState map[string]map[string]map[string]string

func snapshotTableState(dbs []*NetworkDB) tableState {
	st := make(tableState, len(dbs))
	for _, nDB := range dbs {
		node := make(map[string]map[string]string)
		nDB.RLock()
		for k, nw := range nDB.thisNodeNetworks {
			if !nw.leaving {
				node[k] = make(map[string]string)
			}
		}
		nDB.RUnlock()
		nDB.WalkTable(tableUnderTest, func(network, key string, value []byte, deleting bool) bool {
			if deleting {
				return false
			}
			if node[network] == nil {
				node[network] = make(map[string]string)
			}
			node[network][key] = string(value)
			return false
		})
		st[nDB.config.NodeID] = node
	}
	return st
}

func dumpTables(dbs []*NetworkDB) string {
	dumps := make([]string, len(dbs))
	for i, nDB := range dbs {
		dumps[i] = fmt.Sprintf("Node #%d (%s):\n%v", i, nDB.config.NodeID, nDB.DebugDumpTable(tableUnderTest))
	}
	return strings.Join(dumps, "\n\n")
}

// plan is a scenario: how many nodes to run it on, the seed their gossip derives
// from, and the actions to perform in order.
//
// It is what this test reproduces. Printing it in a form parsePlan reads back
// means a failure can be replayed without rapid -- see
// TestNetworkDBReplayScenario -- and cut down by hand.
type plan struct {
	nodes int
	seed  uint64

	// stagger bounds how far apart the nodes are created, which is the only
	// thing that spreads the phase of the cluster's periodic work. Zero creates
	// them all at one virtual instant and leaves every node's gossip and probe
	// ticker in phase for the whole run -- memberlist's schedule() fixes a
	// ticker's phase when the node is created, and the randStagger in its
	// triggerFunc cannot move it, since the ticker is built before the
	// goroutine which reads it and the stagger is shorter than the interval.
	// A real cluster cannot reach that state, its nodes having started at
	// arbitrary times, so it is worth reaching deliberately rather than by
	// accident, and worth being able to leave.
	//
	// Each node's offset is drawn uniformly from [0, stagger) off the attempt
	// seed, so the arrangement replays with the rest of the scenario instead of
	// having to be recorded a node at a time.
	stagger time.Duration

	actions []action

	// attempts is how many times this scenario should be executed before it is
	// taken to hold, or 0 to leave that to -networkdb.convergence-attempts. A
	// committed scenario sets it, because how often a scenario fails is a
	// property of the bug it pins and varies by orders of magnitude between
	// them; see TestNetworkDBScenarios.
	attempts int
}

// attemptSeed is the gossip seed for one execution of p. The drawn seed goes in
// the first eight bytes and the attempt in the next eight, so that repeat
// executions of one plan gossip differently while staying a function of the
// plan; newMemCluster fans a per-node stream out of the last eight.
func (p *plan) attemptSeed(attempt int) [32]byte {
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], p.seed)
	binary.LittleEndian.PutUint64(seed[8:], uint64(attempt))
	return seed
}

// model folds p's actions into what each node ends up owning: node -> joined
// network -> key -> value. It reports the first action inconsistent with the
// ones before it, which is what a hand-trimmed plan is likely to get wrong.
func (p *plan) model() ([]map[string]map[string]string, error) {
	state := make([]map[string]map[string]string, p.nodes)
	for i := range state {
		state[i] = make(map[string]map[string]string)
	}
	for i, a := range p.actions {
		if err := applyToModel(state, a); err != nil {
			return nil, fmt.Errorf("action %d, %s: %w", i+1, strings.TrimSpace(a.String()), err)
		}
	}
	return state, nil
}

// wantTableState is the state every node should hold once the cluster has
// converged: for each network, the union of the entries owned by every node
// which has joined it.
func wantTableState(state []map[string]map[string]string, dbs []*NetworkDB) tableState {
	converged := make(map[string]map[string]string)
	for _, st := range state {
		for network, entries := range st {
			if converged[network] == nil {
				converged[network] = make(map[string]string)
			}
			maps.Copy(converged[network], entries)
		}
	}
	want := make(tableState, len(state))
	for i, st := range state {
		exp := make(map[string]map[string]string)
		for k := range st {
			exp[k] = converged[k]
		}
		want[dbs[i].config.NodeID] = exp
	}
	return want
}

// applyToModel folds a into state, or reports why it cannot be. It is the one
// place the meaning of an action is written down: networkDBFSM folds each action
// it draws through here, and plan.model folds a parsed plan through the same
// path, so a replayed plan cannot disagree with the drawn one about what should
// converge.
func applyToModel(state []map[string]map[string]string, a action) error {
	if a.nodeidx < 0 || a.nodeidx >= len(state) {
		return fmt.Errorf("node #%d is out of range for a %d-node cluster", a.nodeidx, len(state))
	}
	if a.kind == actionJoinNetwork {
		if _, ok := state[a.nodeidx][a.network]; ok {
			return fmt.Errorf("node #%d has already joined %s", a.nodeidx, a.network)
		}
		state[a.nodeidx][a.network] = make(map[string]string)
		return nil
	}
	entries, joined := state[a.nodeidx][a.network]
	if !joined {
		return fmt.Errorf("node #%d has not joined %s", a.nodeidx, a.network)
	}
	_, owned := entries[a.key]
	switch a.kind {
	case actionLeaveNetwork:
		delete(state[a.nodeidx], a.network)
	case actionCreateEntry:
		if owned {
			return fmt.Errorf("node #%d already owns %s in %s", a.nodeidx, a.key, a.network)
		}
		entries[a.key] = a.value
	case actionUpdateEntry:
		if !owned {
			return fmt.Errorf("node #%d does not own %s in %s", a.nodeidx, a.key, a.network)
		}
		entries[a.key] = a.value
	case actionDeleteEntry:
		if !owned {
			return fmt.Errorf("node #%d does not own %s in %s", a.nodeidx, a.key, a.network)
		}
		delete(entries, a.key)
	default:
		// actionJoinNetwork returns above, being the one kind which must not
		// have joined already. Anything else is a kind this model was never
		// taught, which is a bug here rather than a plan worth reporting on.
		return fmt.Errorf("no model for action kind %v", a.kind)
	}
	return nil
}

// actionKind is one of the mutations networkDBFSM can draw.
type actionKind int

const (
	actionJoinNetwork actionKind = iota
	actionLeaveNetwork
	actionCreateEntry
	actionUpdateEntry
	actionDeleteEntry
)

var actionKindNames = [...]string{"JoinNetwork", "LeaveNetwork", "CreateEntry", "UpdateEntry", "DeleteEntry"}

func (k actionKind) String() string {
	if k < 0 || int(k) >= len(actionKindNames) {
		return fmt.Sprintf("actionKind(%d)", int(k))
	}
	return actionKindNames[k]
}

// action is a single step of a scenario, resolved against the model at the point
// it was drawn and so executable without drawing anything further.
type action struct {
	kind    actionKind
	nodeidx int
	network string
	key     string
	value   string
	// delay is how long to wait before performing the action. Waiting after
	// would be the same thing minus a case, since the convergence check which
	// follows the last action waits anyway.
	delay time.Duration
}

// String prints the action, preceded by how long to wait before it in a column
// of fixed width, so that a run of them lines up as a timeline. An action which
// waits for nothing is padded to the same width and so carries leading blanks;
// [plan.model] trims them where it names an action in an error.
func (a action) String() string {
	var s string
	switch a.kind {
	case actionCreateEntry, actionUpdateEntry:
		s = fmt.Sprintf("#%d:%v(%s, %s=%s)", a.nodeidx, a.kind, a.network, a.key, a.value)
	case actionDeleteEntry:
		s = fmt.Sprintf("#%d:%v(%s, %s)", a.nodeidx, a.kind, a.network, a.key)
	default:
		s = fmt.Sprintf("#%d:%v(%s)", a.nodeidx, a.kind, a.network)
	}
	delay := ""
	if a.delay > 0 {
		delay = "+" + a.delay.String()
	}
	return fmt.Sprintf("%*s %s", delayColumn, delay, s)
}

// apply performs a against a running cluster. Failure messages carry only what
// the plan determines, never anything derived from the interleaving; see the
// t.Errorf in executePlan for why that matters.
func (a action) apply(t TestingT, dbs []*NetworkDB) {
	t.Helper()
	if a.delay > 0 {
		time.Sleep(a.delay)
	}
	switch a.kind {
	case actionJoinNetwork:
		if err := dbs[a.nodeidx].JoinNetwork(a.network); err != nil {
			t.Errorf("Node %v failed to join network %s: %v", a.nodeidx, a.network, err)
		}
	case actionLeaveNetwork:
		if err := dbs[a.nodeidx].LeaveNetwork(a.network); err != nil {
			t.Errorf("Node %v failed to leave network %s: %v", a.nodeidx, a.network, err)
		}
	case actionCreateEntry:
		if err := dbs[a.nodeidx].CreateEntry(tableUnderTest, a.network, a.key, []byte(a.value)); err != nil {
			t.Errorf("Node %v failed to create entry %s=%s in network %s: %v", a.nodeidx, a.key, a.value, a.network, err)
		}
	case actionUpdateEntry:
		if err := dbs[a.nodeidx].UpdateEntry(tableUnderTest, a.network, a.key, []byte(a.value)); err != nil {
			t.Errorf("Node %v failed to update entry %s=%s in network %s: %v", a.nodeidx, a.key, a.value, a.network, err)
		}
	case actionDeleteEntry:
		if err := dbs[a.nodeidx].DeleteEntry(tableUnderTest, a.network, a.key); err != nil {
			t.Errorf("Node %v failed to delete entry %s in network %s: %v", a.nodeidx, a.key, a.network, err)
		}
	}
}

// networkDBFSM is a [rapid.StateMachine] providing the set of actions available
// for rapid to draw a NetworkDB scenario from. See also
// [rapid.StateMachineActions] and [rapid.Repeat].
//
// Its actions draw and record; they do not touch a cluster. Everything a draw
// depends on lives in the model below, which is what lets the recorded plan be
// executed more than once. executePlan executes it.
type networkDBFSM struct {
	networks []string // list of networks which can be joined
	// node -> joined-network -> key -> value
	state []map[string]map[string]string

	// Remember entry keys that have been used before to avoid trying to
	// create colliding keys. Due to how quickly the FSM runs, it is
	// possible for a node to not have learned that the previous generation
	// of the key was deleted before we try to create it again.
	// network -> key -> true
	keysUsed map[string]map[string]bool

	plan []action

	// maxDelay bounds how long any one action waits before it happens, in
	// milliseconds; see record.
	maxDelay int
}

func newNetworkDBFSM(numNodes, numNetworks, maxDelay int) *networkDBFSM {
	u := &networkDBFSM{
		maxDelay: maxDelay,
		state:    make([]map[string]map[string]string, numNodes),
		keysUsed: make(map[string]map[string]bool),
	}
	for i := range u.state {
		u.state[i] = make(map[string]map[string]string)
	}
	for i := range numNetworks {
		nw := "nw" + strconv.Itoa(i)
		u.networks = append(u.networks, nw)
		u.keysUsed[nw] = make(map[string]bool)
	}
	return u
}

// record draws how long to wait before a, folds it into the model, and appends it
// to the plan being drawn.
//
// The wait is a single draw between zero and u.maxDelay, zero meaning the action
// happens immediately after the one before it. Only the bound is per scenario;
// the waits under it vary action by action across the whole of that range. So
// what a scenario settles is how tightly packed it can be, not how tightly packed
// any stretch of it is. Waiting as an action of its own could not do that: it
// would be a fixed fraction of the set, putting every scenario at the same
// middling spacing.
//
// Both ends matter, because they exercise different code: a burst of actions at
// one virtual instant overruns memberlist's broadcast path and leaves the cluster
// to converge by push/pull anti-entropy tens of seconds later, where a spread
// scenario converges by gossip as it goes.
//
// Leaning on rapid's bias is deliberate. Its integer draws favour small
// magnitudes in absolute terms -- of 4000 draws from [0,500], 11% came out zero
// and 40% under ten, and widening the range to [0,10000] barely moved either --
// so drawing the delay directly gives frequent tight clumping under a near-flat
// tail of longer waits, and a maxDelay of zero, which the scenario-level draw
// also lands on about a tenth of the time, gives a scenario that is all burst.
// Comparing a biased draw against a threshold cannot do this: a nominal
// one-in-five delayed every second action when measured.
//
// Drawing the wait as a number rather than as an action of its own also puts it
// inside what rapid searches: shrinking narrows a delay toward zero, and zero is
// removal.
//
// applyToModel can only reject an action this state machine had no business
// drawing, since every action method picks from what the model already permits. So
// a rejection is a bug here rather than anything a run can provoke, and panicking
// says so: rapid turns it into a failure with a traceback.
func (u *networkDBFSM) record(t *rapid.T, a action) {
	if u.maxDelay > 0 {
		a.delay = time.Duration(rapid.IntRange(0, u.maxDelay).Draw(t, "delay")) * time.Millisecond
	}
	if err := applyToModel(u.state, a); err != nil {
		panic(fmt.Sprintf("state machine drew an action its own model rejects: %v", err))
	}
	u.plan = append(u.plan, a)
}

func (u *networkDBFSM) Check(t *rapid.T) {
	// This method is required to implement the [rapid.StateMachine]
	// interface. But there is nothing to check stepwise: the actions only
	// draw, and what they are drawing a plan for is an eventually consistent
	// system. The checks happen once the plan is executed.
}

func (u *networkDBFSM) JoinNetwork(t *rapid.T) {
	// Pick a node that has not joined all networks...
	var nodes []int
	for i, s := range u.state {
		if len(s) < len(u.networks) {
			nodes = append(nodes, i)
		}
	}
	if len(nodes) == 0 {
		t.Skip("All nodes are already joined to all networks")
	}
	nodeidx := rapid.SampledFrom(nodes).Draw(t, "node")

	// ... and a network to join.
	networks := slices.DeleteFunc(slices.Clone(u.networks), func(n string) bool {
		_, ok := u.state[nodeidx][n]
		return ok
	})
	nw := rapid.SampledFrom(networks).Draw(t, "network")

	u.record(t, action{kind: actionJoinNetwork, nodeidx: nodeidx, network: nw})
}

// drawJoinedNodeAndNetwork returns a random node that has joined at least one
// network, and one of the networks it has joined.
func (u *networkDBFSM) drawJoinedNodeAndNetwork(t *rapid.T) (nodeidx int, nw string) {
	var nodes []int
	for i, s := range u.state {
		if len(s) > 0 {
			nodes = append(nodes, i)
		}
	}
	if len(nodes) == 0 {
		t.Skip("No node is joined to any network")
	}
	nodeidx = rapid.SampledFrom(nodes).Draw(t, "node")

	// Sorted, not merely collected. SampledFrom records the index it chose,
	// and map iteration order is randomised per run, so an unsorted candidate
	// list makes that index resolve to a different element on replay. The
	// model then diverges, later candidate lists differ in size, draws consume
	// different widths, and the rest of the stream desynchronises -- which is
	// why an unsorted list defeats both failfile replay and shrinking.
	nw = rapid.SampledFrom(slices.Sorted(maps.Keys(u.state[nodeidx]))).Draw(t, "network")
	return nodeidx, nw
}

func (u *networkDBFSM) LeaveNetwork(t *rapid.T) {
	nodeidx, nw := u.drawJoinedNodeAndNetwork(t)
	u.record(t, action{kind: actionLeaveNetwork, nodeidx: nodeidx, network: nw})
}

func (u *networkDBFSM) CreateEntry(t *rapid.T) {
	nodeidx, nw := u.drawJoinedNodeAndNetwork(t)
	key := rapid.StringMatching(`[a-z]{3,25}`).
		Filter(func(s string) bool { return !u.keysUsed[nw][s] }).
		Draw(t, "key")
	value := rapid.StringMatching(`[a-z]{5,20}`).Draw(t, "value")

	u.keysUsed[nw][key] = true
	u.record(t, action{kind: actionCreateEntry, nodeidx: nodeidx, network: nw, key: key, value: value})
}

// drawOwnedDBKey returns a random key in nw owned by the node at nodeidx.
func (u *networkDBFSM) drawOwnedDBKey(t *rapid.T, nodeidx int, nw string) string {
	keys := slices.Sorted(maps.Keys(u.state[nodeidx][nw])) // sorted: see drawJoinedNodeAndNetwork
	if len(keys) == 0 {
		t.Skipf("Node %v owns no entries in network %s", nodeidx, nw)
		panic("unreachable")
	}
	return rapid.SampledFrom(keys).Draw(t, "key")
}

func (u *networkDBFSM) UpdateEntry(t *rapid.T) {
	nodeidx, nw := u.drawJoinedNodeAndNetwork(t)
	key := u.drawOwnedDBKey(t, nodeidx, nw)
	value := rapid.StringMatching(`[a-z]{5,20}`).Draw(t, "value")

	u.record(t, action{kind: actionUpdateEntry, nodeidx: nodeidx, network: nw, key: key, value: value})
}

func (u *networkDBFSM) DeleteEntry(t *rapid.T) {
	nodeidx, nw := u.drawJoinedNodeAndNetwork(t)
	key := u.drawOwnedDBKey(t, nodeidx, nw)

	u.record(t, action{kind: actionDeleteEntry, nodeidx: nodeidx, network: nw, key: key})
}

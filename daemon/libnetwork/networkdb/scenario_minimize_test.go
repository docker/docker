package networkdb

import (
	"flag"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// convergenceMeasure prices a scenario without reducing it.
var convergenceMeasure = flag.Bool("networkdb.convergence-measure", false,
	"report how often the scenario given by -networkdb.convergence-plan fails and what one execution of it costs, without reducing it")

// convergenceMinimize turns TestNetworkDBReplayScenario into a reducer, and says
// which of the two things a reduced scenario can be good for to aim at.
var convergenceMinimize = flag.String("networkdb.convergence-minimize", "",
	`reduce the scenario given by -networkdb.convergence-plan: "diagnose" for the smallest scenario which still fails, "optimize" for the one cheapest to catch`)

// probe is what running a scenario up to a budget came to.
type probe struct {
	// at is the 1-based execution which did not converge, or 0 if none did.
	at  int
	out outcome
	// converged counts the executions which did converge, and convergedFor is
	// the wall time they took. Only those are timed: an execution which fails
	// sits out the whole convergence timeout, which costs far more real time
	// than one which does not, and it is the passing cost that says what a run
	// against fixed code will cost.
	converged    int
	convergedFor time.Duration
}

// firstFailure runs p up to budget times, stopping at the first execution which
// does not converge. A plan the model rejects does not run at all: an incoherent
// scenario is not a smaller failing one.
func firstFailure(t *testing.T, p *plan, budget int) probe {
	t.Helper()

	if _, err := p.model(); err != nil {
		return probe{}
	}
	var pr probe
	for attempt := range budget {
		var o outcome
		start := time.Now()
		synctest.Test(t, func(t *testing.T) {
			o = runPlanOnce(t, p, attempt)
		})
		if o.diff != "" {
			pr.at, pr.out = attempt+1, o
			return pr
		}
		pr.converged++
		pr.convergedFor += time.Since(start)
	}
	return pr
}

// The two things a reduced scenario can be good for. They pull in opposite
// directions, so which one is being aimed at has to be said out loud.
const (
	// minimizeDiagnose wants the smallest scenario which still fails, so that
	// what is left is the bug and nothing else. How often it fails does not
	// matter: whoever is reading it will run it under a debugger as often as
	// they like.
	minimizeDiagnose = "diagnose"
	// minimizeOptimize wants the scenario cheapest to catch, which is what a
	// committed regression test should be. That is not usually the smallest one.
	minimizeOptimize = "optimize"
)

// catchFactor is how many multiples of a scenario's mean-executions-to-failure
// give a 99% chance of catching it: solving 1-(1-p)^n = 0.99 for small p gives
// n = ln(0.01)/ln(1-p), or about 4.6/p. The 3/p which gives 95% leaves one run in
// twenty-seven passing while the bug is present, which was seen happening to a
// scenario in testdata at that budget -- too loose for a regression test whose
// whole job is to fail.
const catchFactor = 4.6

// reportMeasurement prints how often p fails and what an execution of it costs.
//
// It is the same measurement reportPath makes of each point along a reduction,
// offered on its own because those two numbers are what a committed scenario's
// attempts field is set from, and what choosing between two scenarios turns on.
//
// Doing it here rather than by hand matters more than it sounds. The obvious
// approach from outside -- strip whatever entry the failure names, time the rest
// as a scenario which passes -- measures the wrong thing twice over: removing
// those entries lightens the workload, so the timing understates the real
// scenario, and a failure need not name the same entry twice, in which case no
// amount of stripping produces something which passes at all. measure sidesteps
// both by timing only the executions which converged, within runs of the
// scenario itself.
func reportMeasurement(t *testing.T, p *plan) {
	t.Helper()

	const trials = 12
	perFailure, perExec, timed, at, used := measure(t, p, p.attemptCount(), trials, 3)
	if perFailure == 0 {
		// Every execution converged, so the cost is the one number this could
		// measure -- and the one a committed scenario is still sized in once its
		// bug is fixed.
		t.Logf("Did not fail in %d trials of %d executions, so it is rarer than about 1 in %d. "+
			"An execution costs %v.",
			trials, used, trials*used, perExec.Round(time.Microsecond))
		return
	}

	// A trial which reached its budget without failing is written as "-" rather
	// than as the zero measure returns for it: printed among the others it reads
	// as a failure on the first execution, which is the opposite of what it means.
	shown := make([]string, len(at))
	censored := 0
	for i, n := range at {
		if n == 0 {
			shown[i] = "-"
			censored++
			continue
		}
		shown[i] = strconv.Itoa(n)
	}
	survived := ""
	if censored > 0 {
		survived = fmt.Sprintf(", %d of them reaching %d without failing", censored, used)
	}
	if timed == 0 {
		t.Logf("Failed on execution [%s] over %d trials: about 1 in %.0f. No execution converged, "+
			"so what one costs -- and so what keeping this would cost a run -- went unmeasured.",
			strings.Join(shown, " "), trials, perFailure)
		return
	}
	t.Logf("Failed on execution [%s] over %d trials%s: about 1 in %.0f, %v an execution. "+
		"The %.0f attempts which give a 99%% chance of catching it would cost %v a run; "+
		"%.0f would give 95%%, for %v.",
		strings.Join(shown, " "), trials, survived, perFailure, perExec.Round(time.Microsecond),
		catchFactor*perFailure,
		time.Duration(catchFactor*perFailure*float64(perExec)).Round(time.Millisecond),
		3*perFailure, time.Duration(3*perFailure*float64(perExec)).Round(time.Millisecond))
}

// minimizePlan reduces p and prints what the reduction passed through, ready to
// be saved as a file under testdata/scenarios.
//
// Reduction removes actions which leave the bug reachable, but often reachable
// less often, so its path runs from a scenario which fails frequently and costs
// a lot per execution to one which fails rarely and costs little. Neither end is
// automatically the one to want. The smallest is the clearest to read; the
// cheapest to catch is somewhere in the middle, and only visible if the rate is
// measured along the way. reportPath does that measuring and mode decides which
// answer is offered.
//
// The verdict on each candidate is probabilistic, and errs in the safe
// direction: a candidate whose failure did not show up in the executions it was
// given is kept rather than dropped, so the result is a scenario which does
// fail, not necessarily the smallest one. Raising
// -networkdb.convergence-attempts reduces further and costs proportionally
// more, because every candidate which does not reproduce is paid for in full.
//
// Reduction takes a while: budget an hour for a large scenario, and pass
// -timeout to match.
func minimizePlan(t *testing.T, p *plan, mode string) {
	t.Helper()

	attempts := p.attemptCount()
	pr := firstFailure(t, p, attempts)
	if pr.at == 0 {
		t.Fatalf("the scenario given did not fail in %d executions, so there is nothing to reduce; "+
			"raise -networkdb.convergence-attempts if it fails rarely", attempts)
	}
	t.Logf("Reducing %d nodes and %d actions for %s, judging each candidate on %d executions.",
		p.nodes, len(p.actions), mode, attempts)

	path := []*plan{p.clone()}

	// What the failure said comes first: it can drop scattered actions in one
	// move, which is what the ladder below is worst at.
	if pruneToDiverged(t, p, attempts, pr.out) {
		path = append(path, p.clone())
	}

	// Whole blocks next: a scenario recorded from a search usually carries long
	// stretches which have nothing to do with the failure, and removing them one
	// action at a time pays for every one of them separately.
	for n := 8; n >= 1; n /= 2 {
		for i := 0; i < len(p.actions); {
			if i+n > len(p.actions) {
				i++
				continue
			}
			cand := *p
			cand.actions = slices.Concat(p.actions[:i], p.actions[i+n:])
			if firstFailure(t, &cand, attempts).at > 0 {
				p.actions = cand.actions
				path = append(path, p.clone())
				t.Logf("  dropped %d action(s) at %d, %d left", n, i, len(p.actions))
				continue
			}
			i++
		}
		if compactCluster(t, p, attempts) {
			path = append(path, p.clone())
		}
	}

	reportPath(t, path, attempts, mode)
}

// pruneToDiverged proposes dropping the actions which what the failure said
// cannot implicate: first the networks the cluster did converge on, then, if
// that holds, the entries it converged on within a network it did not.
//
// This is worth doing before the block ladder because it is a move the ladder
// cannot make. A converged network's actions are scattered through the scenario
// rather than contiguous, so removing them as a run is impossible and removing
// them one at a time costs a verification apiece -- and every one of those has to
// reproduce, so with a probabilistic verdict a long chain is far likelier to
// stall part-way than a single jump is to fail.
//
// It is a proposal and not a deduction. A network which converged in the
// execution we happened to watch still carried gossip, still competed for the
// broadcast budget and still shaped the timing, so dropping it can perfectly
// well take the failure with it. That is why the candidate is verified like any
// other and abandoned if it does not reproduce.
func pruneToDiverged(t *testing.T, p *plan, attempts int, o outcome) bool {
	t.Helper()

	keep := func(why string, wanted func(action) bool) bool {
		cand := *p
		cand.actions = slices.DeleteFunc(slices.Clone(p.actions), func(a action) bool {
			return !wanted(a)
		})
		dropped := len(p.actions) - len(cand.actions)
		if dropped == 0 {
			return false
		}
		if firstFailure(t, &cand, attempts).at == 0 {
			t.Logf("  keeping %d action(s) for %s: dropping them lost the failure", dropped, why)
			return false
		}
		p.actions = cand.actions
		t.Logf("  dropped %d action(s) for %s, %d left", dropped, why, len(p.actions))
		return true
	}

	changed := false
	if len(o.networks) > 0 {
		changed = keep("networks which converged", func(a action) bool {
			return o.networks[a.network]
		}) || changed
	}
	if len(o.entries) > 0 {
		changed = keep("entries which converged", func(a action) bool {
			switch a.kind {
			case actionJoinNetwork, actionLeaveNetwork:
				return true // membership shapes everything else
			default:
				return o.entries[a.network+"/"+a.key]
			}
		}) || changed
	}
	return changed
}

// compactCluster renumbers p onto the nodes which actually act, then looks for
// the smallest cluster which still fails.
//
// It searches upwards from the number of acting nodes rather than simply using
// it, because the size of the cluster is itself load-bearing: memberlist gives a
// broadcast a budget of RetransmitMult*ceil(log10(N+1)) transmissions against
// N-1 peers, so a scenario can need bystanders to fail at all.
// entry-after-leave-rejoin.scenario does, and says so.
func compactCluster(t *testing.T, p *plan, attempts int) bool {
	t.Helper()

	used := make(map[int]bool)
	for _, a := range p.actions {
		used[a.nodeidx] = true
	}
	order := slices.Sorted(maps.Keys(used))
	remap := make(map[int]int, len(order))
	for i, old := range order {
		remap[old] = i
	}
	renumbered := make([]action, len(p.actions))
	for i, a := range p.actions {
		a.nodeidx = remap[a.nodeidx]
		renumbered[i] = a
	}

	for n := max(len(order), 2); n < p.nodes; n++ {
		cand := *p
		cand.nodes, cand.actions = n, renumbered
		if firstFailure(t, &cand, attempts).at > 0 {
			t.Logf("  cluster %d -> %d nodes", p.nodes, n)
			p.nodes, p.actions = n, renumbered
			return true
		}
	}
	return false
}

// measure estimates how often p fails and what one execution of it costs.
//
// Trials which reach the budget without failing are the whole difficulty. They
// still carry information -- a scenario which survived a budget is rarer than one
// which did not -- so the rate is failures over every execution run, censored
// trials included. Averaging only the trials which failed would read a single
// failure at execution 34 out of eight trials of 40 as "one in 34" when it is
// nearer one in 300, and understate the attempts needed by a factor of ten.
//
// A perFailure of 0 means nothing failed, so the scenario is rarer than the
// trials could measure. A timed of 0 means nothing converged, so perExec is not
// a measurement of anything and the scenario cannot be priced.
//
// rounds bounds how far the budget may grow: each round quadruples it, so three
// rounds spend 21 times the budget on a scenario which never fails. Pass 1 where
// the answer only has to be a comparison rather than a rate.
func measure(t *testing.T, p *plan, budget, trials, rounds int) (perFailure float64, perExec time.Duration, timed int, at []int, used int) {
	t.Helper()

	// Grow the budget until enough failures show up to estimate a rate. The
	// whole point of the path is to compare a scenario which fails often against
	// one which hardly ever does, and a budget which suits the first cannot
	// measure the second. Without this the rarest points come back uncosted, and
	// optimize quietly settles for whichever it could measure -- which is the
	// unreduced scenario it started from, the one reduction was meant to improve
	// on.
	for round := 0; ; round++ {
		executions, failures := 0, 0
		converged, convergedFor := 0, time.Duration(0)
		at = at[:0]
		for range trials {
			pr := firstFailure(t, p, budget)
			at = append(at, pr.at)
			converged += pr.converged
			convergedFor += pr.convergedFor
			if pr.at == 0 {
				executions += budget
				continue
			}
			executions += pr.at
			failures++
		}
		// Wall time of the executions which converged, which is what a run
		// against fixed code pays for every one of its attempts. Timing the
		// failing ones too would overstate it, and overstate it most for the
		// scenarios which fail often -- the very ones worth keeping. A scenario
		// where none converged leaves nothing to average, and timed says so
		// rather than letting a zero stand in for a cost.
		timed = converged
		if timed > 0 {
			perExec = convergedFor / time.Duration(timed)
		}
		if failures >= 2 || round == rounds-1 {
			if failures == 0 {
				return 0, perExec, timed, at, budget
			}
			return float64(executions) / float64(failures), perExec, timed, at, budget
		}
		budget *= 4
	}
}

// spreadInTime tries the scenario with its actions further apart in time and
// returns the cheapest to catch, which may be the one it was given.
//
// Timing is part of what a scenario costs, not just how many actions it has.
// Firing them all at one virtual instant overruns memberlist's broadcast path,
// and the cluster then converges only once push/pull anti-entropy catches up,
// some 27s of virtual time later, with every node's timers firing throughout.
// The same actions 100ms apart can converge immediately and skip all of it.
//
// Reduction cannot find this. Dropping actions only ever makes a scenario
// cheaper by making it rarer, whereas spacing them out leaves what it pins
// alone. It does change the interleaving, which is the very thing these bugs
// turn on, so a spacing which thins the failure out costs more than it saves.
// That is the usual outcome, not a corner case: respacing one scenario here
// halved what an execution cost and still lost, because it thinned the failures
// from 1 in 69 to 1 in 194. Each spacing is judged on measured cost for that
// reason, and declining is the expected answer.
//
// Only offered for optimize: a scenario spread out is longer to read, and
// diagnosis wants the bug and nothing else.
func spreadInTime(t *testing.T, p *plan, budget, trials int, cost time.Duration) (*plan, time.Duration) {
	t.Helper()

	best, bestCost := p, cost
	for _, gap := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
		cand := p.clone()
		for i := range cand.actions {
			cand.actions[i].delay = gap
		}

		// One round only. This has to answer "cheaper than what we have?", not
		// "how rare is it?", and a spacing which loses the failure is the usual
		// answer -- so growing the budget to price something already known to be
		// worse spends most of the run establishing it. At 60 executions and 53ms
		// apiece that is the difference between seconds and six minutes a
		// spacing, and there are three of them.
		perFailure, perExec, timed, _, _ := measure(t, cand, budget, trials, 1)
		if perFailure == 0 {
			t.Logf("  %v apart: too rare to measure, keeping what we had", gap)
			continue
		}
		if timed == 0 {
			t.Logf("  %v apart: never converged, so it cannot be priced; keeping what we had", gap)
			continue
		}
		c := time.Duration(catchFactor * perFailure * float64(perExec))
		t.Logf("  %v apart: 1 in %.0f, %v an execution, %v a run",
			gap, perFailure, perExec.Round(time.Microsecond), c.Round(time.Millisecond))
		if c < bestCost {
			cand.attempts = int(catchFactor * perFailure)
			best, bestCost = cand, c
		}
	}
	return best, bestCost
}

// samplePath picks at most k scenarios spread along the reduction, always
// including where it started and where it ended up.
func samplePath(path []*plan, k int) []*plan {
	if len(path) <= k {
		return path
	}
	out := make([]*plan, 0, k)
	for i := range k {
		out = append(out, path[i*(len(path)-1)/(k-1)])
	}
	return out
}

// reportPath measures scenarios from along the reduction and prints what each
// would cost to catch, then offers the one which suits mode.
//
// The point of measuring more than the end is that reduction trades one property
// for the other: it starts from a scenario which fails often and costs a lot to
// run, and ends at one which is cheap to run and hardly ever fails. Cost to
// catch is the product of the two, so it falls and then rises, and the best
// scenario for a regression test is somewhere in the middle. Reading the
// smallest as the best is the mistake this exists to prevent -- for
// entry-after-leave-rejoin.scenario it means 6 nodes and 12 actions needing over
// 3000 executions, over the 9 nodes and 25 actions which need 92.
func reportPath(t *testing.T, path []*plan, budget int, mode string) {
	t.Helper()

	const trials = 5
	pts := samplePath(path, 4)

	var b strings.Builder
	fmt.Fprintf(&b, "Reduction path, %d trials a point, from %d executions each and grown as needed:\n", trials, budget)
	fmt.Fprintf(&b, "  %-6s %-8s %-16s %-10s %s\n", "nodes", "actions", "fails about", "per exec", "cost at 99%")

	rates := make([]float64, len(pts))
	best, bestCost := -1, time.Duration(0)
	for i, q := range pts {
		perFailure, perExec, timed, _, _ := measure(t, q, budget, trials, 3)
		rates[i] = perFailure
		rate, per, cost := "rarer than measured", "-", "-"
		if perFailure > 0 {
			rate = fmt.Sprintf("1 in %.0f", perFailure)
		}
		// Ranking compares cost, so a point with no converged execution to price
		// has to sit the comparison out: its cost is unknown, not zero, and a
		// zero would beat every real measurement here.
		if perFailure > 0 && timed > 0 {
			c := time.Duration(catchFactor * perFailure * float64(perExec))
			per = perExec.Round(time.Microsecond).String()
			cost = c.Round(time.Millisecond).String()
			// Points come in reduction order, so a later one is the smaller
			// scenario. Take it unless it is clearly dearer: per-execution
			// timings vary between runs, so a narrow win is not a real one, and
			// of two scenarios costing about the same the smaller is the better
			// to keep.
			if best < 0 || c < time.Duration(float64(bestCost)*1.25) {
				best, bestCost = i, c
			}
		}
		fmt.Fprintf(&b, "  %-6d %-8d %-16s %-10s %s\n",
			q.nodes, len(q.actions), rate, per, cost)
	}
	t.Log(b.String())

	switch mode {
	case minimizeDiagnose:
		q := pts[len(pts)-1]
		t.Logf("Smallest scenario which still fails, %d nodes and %d actions:\n%s",
			q.nodes, len(q.actions), q)
	case minimizeOptimize:
		if best < 0 {
			t.Logf("No scenario on the path failed often enough to measure; " +
				"re-run with a larger -networkdb.convergence-attempts.")
			return
		}
		q := pts[best]
		// The attempts a committed scenario needs, from the rate already
		// measured above rather than a second sample which would disagree with
		// the table just printed.
		q.attempts = int(catchFactor * rates[best])

		t.Logf("Trying the same actions spaced further apart:")
		q, bestCost = spreadInTime(t, q, budget, trials, bestCost)

		t.Logf("Cheapest to catch, %v a run, %d nodes and %d actions:\n%s",
			bestCost.Round(time.Millisecond), q.nodes, len(q.actions), q)
	}
}

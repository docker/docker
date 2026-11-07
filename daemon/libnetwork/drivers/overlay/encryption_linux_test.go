//go:build linux

package overlay

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay/overlayutils"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// mangleOutputRules returns the rules programmed into the mangle table's OUTPUT
// chain, in the order they are evaluated. The chain policy line is omitted so
// that an empty chain yields an empty slice.
func mangleOutputRules(t *testing.T, iptable *iptables.IPTable) []string {
	t.Helper()
	return chainRules(t, iptable, iptables.Mangle, "OUTPUT")
}

// chainRules returns the rules programmed into a chain of an iptables table,
// in the order they are evaluated. The chain policy line is omitted so that an
// empty chain yields an empty slice.
func chainRules(t *testing.T, iptable *iptables.IPTable, table iptables.Table, chain string) []string {
	t.Helper()
	out, err := iptable.Raw("-t", string(table), "-S", chain)
	assert.NilError(t, err)
	var rules []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(l, "-A ") {
			rules = append(rules, l)
		}
	}
	return rules
}

// indexOfRule returns the index of the first rule containing substr, or -1.
func indexOfRule(rules []string, substr string) int {
	return slices.IndexFunc(rules, func(r string) bool {
		return strings.Contains(r, substr)
	})
}

// clearRuleMatch uniquely identifies the mark-stripping rule: it is the only
// rule which *matches* on the encryption mark. Do not match on
// "--socket-exists", which also appears in the marking rule as the inverted
// "! --socket-exists".
const clearRuleMatch = "-m mark --mark"

// vniBytecode returns the xt_bpf bytecode which matches vni, as rendered into a
// rule by iptables. It uniquely identifies the marking rule for a VNI.
func vniBytecode(vni uint32) string {
	return marshalXTBPF(vniMatchBPF(vni))
}

// legacyMangleRule returns the marking rule as programmed by daemons predating
// the fix: it marks any VXLAN datagram with the given VNI for encryption,
// without discriminating between datagrams sent by the kernel and datagrams
// forged by a local process.
func legacyMangleRule(vni uint32) []string {
	return append(matchVXLAN(overlayutils.VXLANUDPPort(), vni),
		"-j", "MARK", "--set-mark", strconv.FormatUint(mark, 10))
}

func seedLegacyMangleRule(t *testing.T, iptable *iptables.IPTable, vni uint32) {
	t.Helper()
	args := append([]string{"-t", string(iptables.Mangle), string(iptables.Append), "OUTPUT"},
		legacyMangleRule(vni)...)
	assert.NilError(t, iptable.RawCombinedOutput(args...),
		"failed to seed legacy mangle rule (is xt_bpf available?)")
}

// TestProgramMangleReplacesLegacyRule asserts that programming the marking rule
// for a VNI removes the unrestricted rule which a pre-fix daemon would have left
// behind for the same VNI, rather than leaving both in place.
func TestProgramMangleReplacesLegacyRule(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := &driver{advertiseAddress: netip.MustParseAddr("192.0.2.1")}
	iptable := iptables.GetIptable(iptables.IPv4)

	const vni = 4096
	seedLegacyMangleRule(t, iptable, vni)
	assert.Assert(t, is.Len(mangleOutputRules(t, iptable), 1))

	assert.NilError(t, d.programMangle(vni, true))

	rules := mangleOutputRules(t, iptable)
	assert.Assert(t, is.Len(rules, 1), "expected the legacy rule to be replaced, not duplicated:\n%s",
		strings.Join(rules, "\n"))
	assert.Check(t, strings.Contains(rules[0], vniBytecode(vni)), "wrong VNI: %s", rules[0])
	assert.Check(t, strings.Contains(rules[0], "! --socket-exists"),
		"marking rule must skip datagrams associated with a socket: %s", rules[0])
}

// TestMangleMarkSpoofProtectionOrdering asserts that the rule which strips the
// encryption mark from socket-associated packets is evaluated *after* any rule
// which sets that mark.
//
// Daemons predating this fix appended mangle rules which mark VXLAN datagrams
// for encryption without discriminating between authentic datagrams and
// datagrams forged by a local process. Such a rule survives an upgrade for any
// VNI which is never reprogrammed on the node, so the mark-stripping rule has to
// be evaluated after it in order to neutralize it. That is why
// programMangleMarkSpoofProtection appends its rule instead of inserting it at
// the head of the chain: an inserted rule would run *before* the stale rule and
// would silently stop neutralizing it.
func TestMangleMarkSpoofProtectionOrdering(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := &driver{advertiseAddress: netip.MustParseAddr("192.0.2.1")}
	iptable := iptables.GetIptable(iptables.IPv4)

	// Seed an unrestricted marking rule for a VNI which the driver never
	// programs, so nothing removes it.
	const staleVNI = 4096
	seedLegacyMangleRule(t, iptable, staleVNI)

	assert.NilError(t, d.programMangleMarkSpoofProtection(true))

	rules := mangleOutputRules(t, iptable)
	staleIdx := indexOfRule(rules, vniBytecode(staleVNI))
	clearIdx := indexOfRule(rules, clearRuleMatch)
	assert.Assert(t, staleIdx >= 0, "stale marking rule not found in:\n%s", strings.Join(rules, "\n"))
	assert.Assert(t, clearIdx >= 0, "mark-stripping rule not found in:\n%s", strings.Join(rules, "\n"))
	assert.Check(t, staleIdx < clearIdx,
		"mark-stripping rule (index %d) must be evaluated after the stale marking rule (index %d):\n%s",
		clearIdx, staleIdx, strings.Join(rules, "\n"))
}

// TestProgramMangleLifecycle walks the rules through install and removal for two
// VNIs, and pins the relative ordering of the marking and mark-stripping rules.
func TestProgramMangleLifecycle(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := &driver{advertiseAddress: netip.MustParseAddr("192.0.2.1")}
	iptable := iptables.GetIptable(iptables.IPv4)

	const vniA, vniB = 4096, 4097

	// First encrypted network on the node: the marking rule is programmed
	// when the subnet is set up in the network sandbox, ...
	assert.NilError(t, d.programMangle(vniA, true))
	// ... and the mark-stripping rule when the first peer is discovered.
	assert.NilError(t, d.programMangleMarkSpoofProtection(true))

	rules := mangleOutputRules(t, iptable)
	assert.Assert(t, is.Len(rules, 2), "%s", strings.Join(rules, "\n"))
	assert.Check(t, is.Equal(indexOfRule(rules, vniBytecode(vniA)), 0), "%s", strings.Join(rules, "\n"))
	assert.Check(t, is.Equal(indexOfRule(rules, clearRuleMatch), 1), "%s", strings.Join(rules, "\n"))

	// A second encrypted network appends its marking rule after the
	// mark-stripping rule. That is safe: the marking rule cannot mark a
	// datagram associated with a socket, so it can never re-mark a datagram
	// the mark-stripping rule has already cleared.
	assert.NilError(t, d.programMangle(vniB, true))
	rules = mangleOutputRules(t, iptable)
	assert.Assert(t, is.Len(rules, 3), "%s", strings.Join(rules, "\n"))
	assert.Check(t, indexOfRule(rules, clearRuleMatch) < indexOfRule(rules, vniBytecode(vniB)),
		"%s", strings.Join(rules, "\n"))

	// Removal leaves the chain as we found it.
	assert.NilError(t, d.programMangle(vniA, false))
	assert.NilError(t, d.programMangle(vniB, false))
	assert.NilError(t, d.programMangleMarkSpoofProtection(false))
	rules = mangleOutputRules(t, iptable)
	assert.Check(t, is.Len(rules, 0), "%s", strings.Join(rules, "\n"))
}

// TestProgramMangleIPv6Transport asserts that the rules are programmed into
// ip6tables, and not iptables, when the Swarm data plane is IPv6.
func TestProgramMangleIPv6Transport(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := &driver{advertiseAddress: netip.MustParseAddr("2001:db8::1")}

	const vni = 4096
	assert.NilError(t, d.programMangle(vni, true))
	assert.NilError(t, d.programMangleMarkSpoofProtection(true))

	rules6 := mangleOutputRules(t, iptables.GetIptable(iptables.IPv6))
	assert.Assert(t, is.Len(rules6, 2), "%s", strings.Join(rules6, "\n"))
	assert.Check(t, is.Equal(indexOfRule(rules6, vniBytecode(vni)), 0), "%s", strings.Join(rules6, "\n"))
	assert.Check(t, is.Equal(indexOfRule(rules6, clearRuleMatch), 1), "%s", strings.Join(rules6, "\n"))

	rules4 := mangleOutputRules(t, iptables.GetIptable(iptables.IPv4))
	assert.Check(t, is.Len(rules4, 0), "IPv4 mangle OUTPUT should be untouched:\n%s",
		strings.Join(rules4, "\n"))
}

func TestRestoreIptablesEncryptionRulesOnFirewalldReload(t *testing.T) {
	skip.If(t, nftables.Enabled(), "test is for the iptables backend")
	defer netnsutils.SetupTestOSContext(t)()

	const encVNI, clearVNI, notSetUpVNI = 4097, 4098, 4099
	d := newDriver(t.TempDir())
	d.advertiseAddress = netip.MustParseAddr("192.0.2.1")
	d.networks["enc"] = &network{id: "enc", driver: d, secure: true, subnets: []*subnet{
		{vni: encVNI, initDone: true},
		// A subnet which has not been set up.
		{vni: notSetUpVNI},
	}}
	d.networks["clear"] = &network{id: "clear", driver: d, subnets: []*subnet{{vni: clearVNI, initDone: true}}}

	ipt := iptables.GetIptable(iptables.IPv4)
	type ruleset struct{ MangleOutput, FilterInput []string }
	// The order of the rules does not matter as each rule in a chain
	// matches a disjoint set of packets.
	dump := func() ruleset {
		t.Helper()
		rs := ruleset{
			MangleOutput: chainRules(t, ipt, iptables.Mangle, "OUTPUT"),
			FilterInput:  chainRules(t, ipt, iptables.Filter, "INPUT"),
		}
		slices.Sort(rs.MangleOutput)
		slices.Sort(rs.FilterInput)
		return rs
	}
	noRules := dump()

	// Install the rules as initSubnet does for the subnet of the
	// secure network which has been set up.
	// Install and remove the rules for the other network's VNI, as for a
	// deleted secure network whose VNI was reassigned.
	ctx := t.Context()
	assert.NilError(t, d.programOverlayEncryptionFirewall(ctx, encVNI, true))
	assert.Assert(t, !cmp.Equal(dump(), noRules), "TEST BUG! No rules were installed for VNI %d", encVNI)
	assert.NilError(t, d.programOverlayEncryptionFirewall(ctx, clearVNI, true))
	assert.NilError(t, d.programOverlayEncryptionFirewall(ctx, clearVNI, false))

	noSpoofProtection := dump()

	// Install the local-spoof-protection rule as setupEncryption does for
	// the first remote peer, without programming any XFRM state.
	assert.NilError(t, d.programMangleMarkSpoofProtection(true))
	d.secMap[netip.MustParseAddr("192.0.2.2")] = encrNode{count: 1}

	want := dump()
	assert.Assert(t, !cmp.Equal(want, noSpoofProtection), "TEST BUG! The local-spoof-protection rule was not installed")

	// Flush the chains, as firewalld does on reload.
	_, err := ipt.Raw("-t", string(iptables.Mangle), "-F", "OUTPUT")
	assert.NilError(t, err)
	_, err = ipt.Raw("-t", string(iptables.Filter), "-F", "INPUT")
	assert.NilError(t, err)
	assert.Assert(t, !cmp.Equal(dump(), want), "TEST BUG! The iptables chains could not be flushed")

	d.restoreOverlayEncryptionFirewall(t.Context())
	assert.Check(t, is.DeepEqual(dump(), want),
		"the rules must be restored correctly after a firewalld reload")

	// A reload which did not delete the rules must not duplicate them.
	d.restoreOverlayEncryptionFirewall(t.Context())
	assert.Check(t, is.DeepEqual(dump(), want),
		"a reload which did not delete the rules must not duplicate them")
}

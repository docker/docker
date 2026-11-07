package libnetwork

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"
	"gotest.tools/v3/skip"
)

const (
	fwdChainName       = "FORWARD"
	dockerFwdChainName = "DOCKER-FORWARD"
	usrChainName       = userChain
)

func TestUserChain(t *testing.T) {
	skip.If(t, iptables.UsingFirewalld(), "firewalld is running in the host netns, it can't modify rules in the test's netns")

	const testName = "TestUserChain"
	iptable4 := iptables.GetIptable(iptables.IPv4)
	iptable6 := iptables.GetIptable(iptables.IPv6)

	tests := []struct {
		iptables bool
		append   bool // append other rules to FORWARD
	}{
		{
			iptables: true,
			append:   false,
		},
		{
			iptables: true,
			append:   true,
		},
		{
			iptables: false,
			append:   false,
		},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("iptables=%v,append=%v", tc.iptables, tc.append), func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t)()
			defer resetIptables(t)

			c, err := New(
				t.Context(),
				config.OptionBridgeConfig(bridge.Configuration{
					EnableIPTables:  tc.iptables,
					EnableIP6Tables: tc.iptables,
				}))
			assert.NilError(t, err)
			skip.If(t, nftables.Enabled(), "nftables is enabled, skipping iptables test")

			// init. condition
			golden.Assert(t, getRules(t, iptable4, fwdChainName),
				fmt.Sprintf("%s/iptables-%v_append-%v_fwdinit4.golden", testName, tc.iptables, tc.append))
			golden.Assert(t, getRules(t, iptable6, fwdChainName),
				fmt.Sprintf("%s/iptables-%v_append-%v_fwdinit6.golden", testName, tc.iptables, tc.append))
			if tc.iptables {
				golden.Assert(t, getRules(t, iptable4, dockerFwdChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_dockerfwdinit4.golden", testName, tc.iptables, tc.append))
				golden.Assert(t, getRules(t, iptable6, dockerFwdChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_dockerfwdinit6.golden", testName, tc.iptables, tc.append))
			} else {
				assert.Check(t, !iptables.GetIptable(iptables.IPv4).ExistChain(dockerFwdChainName, iptables.Filter),
					"Chain %s should not exist", dockerFwdChainName)
			}

			if tc.append {
				_, err := iptable4.Raw("-A", fwdChainName, "-j", "DROP")
				assert.Check(t, err)
				_, err = iptable6.Raw("-A", fwdChainName, "-j", "DROP")
				assert.Check(t, err)
			}
			c.setupUserChains()

			golden.Assert(t, getRules(t, iptable4, fwdChainName),
				fmt.Sprintf("%s/iptables-%v_append-%v_fwdafter4.golden", testName, tc.iptables, tc.append))
			golden.Assert(t, getRules(t, iptable6, fwdChainName),
				fmt.Sprintf("%s/iptables-%v_append-%v_fwdafter6.golden", testName, tc.iptables, tc.append))
			if tc.iptables {
				golden.Assert(t, getRules(t, iptable4, dockerFwdChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_dockerfwdafter4.golden", testName, tc.iptables, tc.append))
				golden.Assert(t, getRules(t, iptable6, dockerFwdChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_dockerfwdafter6.golden", testName, tc.iptables, tc.append))
			} else {
				assert.Check(t, !iptables.GetIptable(iptables.IPv4).ExistChain(dockerFwdChainName, iptables.Filter),
					"Chain %s should not exist", dockerFwdChainName)
			}

			if tc.iptables {
				golden.Assert(t, getRules(t, iptable4, usrChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_usrafter4.golden", testName, tc.iptables, tc.append))
				golden.Assert(t, getRules(t, iptable6, usrChainName),
					fmt.Sprintf("%s/iptables-%v_append-%v_usrafter6.golden", testName, tc.iptables, tc.append))
			} else {
				_, err := iptable4.Raw("-S", usrChainName)
				assert.Check(t, isMissingChainError(err), "ipv4 chain %v: created unexpectedly", usrChainName)
				_, err = iptable6.Raw("-S", usrChainName)
				assert.Check(t, isMissingChainError(err), "ipv6 chain %v: created unexpectedly", usrChainName)
			}
		})
	}
}

func getRules(t *testing.T, iptable *iptables.IPTable, chain string) string {
	t.Helper()
	output, err := iptable.Raw("-S", chain)
	assert.NilError(t, err, "chain %s: failed to get rules", chain)
	return string(output)
}

func resetIptables(t *testing.T) {
	t.Helper()

	for _, ipVer := range []iptables.IPVersion{iptables.IPv4, iptables.IPv6} {
		iptable := iptables.GetIptable(ipVer)

		_, err := iptable.Raw("-F", fwdChainName)
		assert.Check(t, err)
		_ = iptable.RemoveExistingChain(usrChainName, iptables.Filter)
	}
}

// isMissingChainError succeeds if err indicates that "iptables -S <chain>"
// could not find the requested chain.
//
// Before iptables v1.8.10, iptables-nft "-S <chain>" could report a
// non-existent chain as incompatible:
//
//	ip6tables v1.8.9 (nf_tables): chain `<chain>' in table `filter' is incompatible, use 'nft' tool.
//
// This was fixed in iptables v1.8.10. Debian 13 ships v1.8.11 and therefore
// returns the usual "No chain/target/match by that name" diagnostic.
//
// Fixed by: https://git.netfilter.org/iptables/commit/?id=82ccfb488eeac5507471099b9b4e6d136cc06e3b
func isMissingChainError(err error) is.Comparison {
	return func() is.Result {
		if err == nil {
			return is.ResultFailure("expected a missing-chain error, got nil")
		}
		if strings.Contains(err.Error(), "No chain/target/match by that name") ||
			strings.Contains(err.Error(), "incompatible, use 'nft' tool") {
			return is.ResultSuccess
		}
		return is.ResultFailure(fmt.Sprintf("expected a missing-chain error, got %v", err))
	}
}

//go:build !windows

package libnetwork

import (
	"net"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
)

func TestCleanupServiceDiscovery(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	c, err := New(t.Context(),
		config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
	assert.NilError(t, err)

	cleanup := func(n *Network) {
		if err := n.Delete(); err != nil {
			t.Error(err)
		}
	}
	n1, err := c.NewNetwork(t.Context(), "bridge", "net1", "", NetworkOptionEnableIPv4(true))
	assert.NilError(t, err)
	defer cleanup(n1)

	n2, err := c.NewNetwork(t.Context(), "bridge", "net2", "", NetworkOptionEnableIPv4(true))
	assert.NilError(t, err)
	defer cleanup(n2)

	n1.addSvcRecords("N1ep1", "service_test", "serviceID1", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")
	n1.addSvcRecords("N2ep2", "service_test", "serviceID2", net.ParseIP("192.168.0.2"), net.IP{}, true, "test")

	n2.addSvcRecords("N2ep1", "service_test", "serviceID1", net.ParseIP("192.168.1.1"), net.IP{}, true, "test")
	n2.addSvcRecords("N2ep2", "service_test", "serviceID2", net.ParseIP("192.168.1.2"), net.IP{}, true, "test")

	if len(c.svcRecords) != 2 {
		t.Fatalf("Service record not added correctly:%v", c.svcRecords)
	}

	// cleanup net1
	c.cleanupServiceDiscovery(n1.ID())

	if len(c.svcRecords) != 1 {
		t.Fatalf("Service record not cleaned correctly:%v", c.svcRecords)
	}

	c.cleanupServiceDiscovery("")

	if len(c.svcRecords) != 0 {
		t.Fatalf("Service record not cleaned correctly:%v", c.svcRecords)
	}
}

// TestServiceAliasRefCounting exercises the per-network alias ref-counting
// in addServiceBinding/rmServiceBinding. It verifies:
//  1. A VIP DNS record for an alias survives a rolling update as long as
//     any task on that network still claims it, and disappears once the
//     last task is removed.
//  2. Aliases are scoped per-network: configuring an alias on network A
//     does not publish a VIP DNS record for it on network B, or vice
//     versa.
func TestServiceAliasRefCounting(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	ctx := t.Context()

	c, err := New(ctx,
		config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
	assert.NilError(t, err)

	n1, err := c.NewNetwork(ctx, "bridge", "net1", "", NetworkOptionEnableIPv4(true))
	assert.NilError(t, err)
	defer func() { _ = n1.Delete() }()

	n2, err := c.NewNetwork(ctx, "bridge", "net2", "", NetworkOptionEnableIPv4(true))
	assert.NilError(t, err)
	defer func() { _ = n2.Delete() }()

	const (
		svcName = "svc"
		svcID   = "svcid"
	)
	vip1 := net.ParseIP("10.0.0.1")
	vip2 := net.ParseIP("10.0.0.2")

	resolves := func(t *testing.T, n *Network, name string) bool {
		t.Helper()
		ips, _ := n.ResolveName(ctx, name, types.IPv4)
		return len(ips) > 0
	}

	t.Run("rolling update preserves alias until last task leaves", func(t *testing.T) {
		// Old task with alias "old".
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n1.ID(), "ep-old", "ctr-old",
			vip1, nil, []string{"old"}, nil, net.ParseIP("172.20.0.10"), "test"))
		assert.Check(t, resolves(t, n1, "old"), "alias 'old' should resolve after first task")

		// New task with alias "new" arrives mid rolling-update.
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n1.ID(), "ep-new", "ctr-new",
			vip1, nil, []string{"new"}, nil, net.ParseIP("172.20.0.11"), "test"))
		assert.Check(t, resolves(t, n1, "old"), "'old' must persist while old task still references it")
		assert.Check(t, resolves(t, n1, "new"), "'new' must resolve once new task is bound")

		// Old task gone — only "new" should remain.
		assert.NilError(t, c.rmServiceBinding(svcName, svcID, n1.ID(), "ep-old", "ctr-old",
			vip1, nil, []string{"old"}, nil, net.ParseIP("172.20.0.10"), "test", true, true))
		assert.Check(t, !resolves(t, n1, "old"), "'old' must be gone once last referencing task is removed")
		assert.Check(t, resolves(t, n1, "new"), "'new' must still resolve")

		// New task gone — service should be empty.
		assert.NilError(t, c.rmServiceBinding(svcName, svcID, n1.ID(), "ep-new", "ctr-new",
			vip1, nil, []string{"new"}, nil, net.ParseIP("172.20.0.11"), "test", true, true))
		assert.Check(t, !resolves(t, n1, "new"))
	})

	t.Run("aliases are scoped per network", func(t *testing.T) {
		// Same service, different aliases per network.
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n1.ID(), "ep-net1", "ctr-1",
			vip1, nil, []string{"only-on-net1"}, nil, net.ParseIP("172.20.0.20"), "test"))
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n2.ID(), "ep-net2", "ctr-2",
			vip2, nil, []string{"only-on-net2"}, nil, net.ParseIP("172.21.0.20"), "test"))

		assert.Check(t, resolves(t, n1, "only-on-net1"), "alias should be on net1")
		assert.Check(t, !resolves(t, n2, "only-on-net1"), "alias must NOT leak to net2")
		assert.Check(t, resolves(t, n2, "only-on-net2"), "alias should be on net2")
		assert.Check(t, !resolves(t, n1, "only-on-net2"), "alias must NOT leak to net1")

		// Tear down — each network's alias should be released independently.
		assert.NilError(t, c.rmServiceBinding(svcName, svcID, n1.ID(), "ep-net1", "ctr-1",
			vip1, nil, []string{"only-on-net1"}, nil, net.ParseIP("172.20.0.20"), "test", true, true))
		assert.Check(t, !resolves(t, n1, "only-on-net1"))
		assert.Check(t, resolves(t, n2, "only-on-net2"), "removing net1 binding must not affect net2")

		assert.NilError(t, c.rmServiceBinding(svcName, svcID, n2.ID(), "ep-net2", "ctr-2",
			vip2, nil, []string{"only-on-net2"}, nil, net.ParseIP("172.21.0.20"), "test", true, true))
		assert.Check(t, !resolves(t, n2, "only-on-net2"))
	})

	t.Run("rebinding same endpoint with changed alias set", func(t *testing.T) {
		// First bind: alias=[a].
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n1.ID(), "ep-rebind", "ctr-rb",
			vip1, nil, []string{"a"}, nil, net.ParseIP("172.20.0.30"), "test"))
		assert.Check(t, resolves(t, n1, "a"))
		assert.Check(t, !resolves(t, n1, "b"))

		// Re-bind same eID with alias=[b]. Diff should drop "a" and add "b".
		assert.NilError(t, c.addServiceBinding(svcName, svcID, n1.ID(), "ep-rebind", "ctr-rb",
			vip1, nil, []string{"b"}, nil, net.ParseIP("172.20.0.30"), "test"))
		assert.Check(t, !resolves(t, n1, "a"), "'a' should be removed by diff")
		assert.Check(t, resolves(t, n1, "b"))

		assert.NilError(t, c.rmServiceBinding(svcName, svcID, n1.ID(), "ep-rebind", "ctr-rb",
			vip1, nil, []string{"b"}, nil, net.ParseIP("172.20.0.30"), "test", true, true))
		assert.Check(t, !resolves(t, n1, "b"))
	})
}

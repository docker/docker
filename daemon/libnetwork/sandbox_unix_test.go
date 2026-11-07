//go:build !windows

package libnetwork

import (
	"strconv"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/options"
	"github.com/moby/moby/v2/internal/testutil/libnetworkutils"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func getTestEnv(t *testing.T, opts ...[]NetworkOption) (*Controller, []*Network) {
	c, err := New(
		t.Context(),
		libnetworkutils.OptionTempNetnsDir(t),
		config.OptionBridgeConfig(bridge.Configuration{
			EnableIPForwarding: true,
		}),
		config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()),
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(opts) == 0 {
		return c, nil
	}

	nwList := make([]*Network, 0, len(opts))
	for i, opt := range opts {
		name := "test_nw_" + strconv.Itoa(i)
		newOptions := []NetworkOption{
			NetworkOptionGeneric(options.Generic{
				netlabel.GenericData: map[string]string{
					bridge.BridgeName: name,
				},
			}),
		}
		newOptions = append(newOptions, opt...)
		n, err := c.NewNetwork(t.Context(), bridge.NetworkType, name, "", newOptions...)
		if err != nil {
			t.Fatal(err)
		}

		nwList = append(nwList, n)
	}

	return c, nwList
}

func TestControllerGetSandbox(t *testing.T) {
	ctrlr, _ := getTestEnv(t)
	t.Run("invalid id", func(t *testing.T) {
		const cID = ""
		sb, err := ctrlr.GetSandbox(cID)
		assert.Check(t, cerrdefs.IsInvalidArgument(err), "expected a ErrInvalidParameter, got %[1]v (%[1]T)", err)
		assert.Check(t, is.Error(err, "invalid id: id is empty"))
		assert.Check(t, is.Nil(sb))
	})
	t.Run("not found", func(t *testing.T) {
		const cID = "container-id-with-no-sandbox"
		sb, err := ctrlr.GetSandbox(cID)
		assert.Check(t, cerrdefs.IsNotFound(err), "expected a ErrNotFound, got %[1]v (%[1]T)", err)
		assert.Check(t, is.Error(err, "network sandbox for container container-id-with-no-sandbox not found"))
		assert.Check(t, is.Nil(sb))
	})
	t.Run("existing sandbox", func(t *testing.T) {
		const cID = "test-container-id"
		expected, err := ctrlr.NewSandbox(t.Context(), cID)
		assert.Check(t, err)

		sb, err := ctrlr.GetSandbox(cID)
		assert.Check(t, err)
		assert.Check(t, is.Equal(sb.ContainerID(), cID))
		assert.Check(t, is.Equal(sb.ID(), expected.ID()))
		assert.Check(t, is.Equal(sb.Key(), expected.Key()))
		assert.Check(t, is.Equal(sb.ContainerID(), expected.ContainerID()))

		err = sb.Delete(t.Context())
		assert.Check(t, err)

		sb, err = ctrlr.GetSandbox(cID)
		assert.Check(t, cerrdefs.IsNotFound(err), "expected a ErrNotFound, got %[1]v (%[1]T)", err)
		assert.Check(t, is.Error(err, "network sandbox for container test-container-id not found"))
		assert.Check(t, is.Nil(sb))
	})
}

func TestSandboxAddEmpty(t *testing.T) {
	ctrlr, _ := getTestEnv(t)

	sbx, err := ctrlr.NewSandbox(t.Context(), "sandbox0")
	if err != nil {
		t.Fatal(err)
	}

	if err := sbx.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(ctrlr.sandboxes) != 0 {
		t.Fatalf("controller sandboxes is not empty. len = %d", len(ctrlr.sandboxes))
	}
}

// // If different priorities are specified, internal option and ipv6 addresses mustn't influence endpoint order
func TestSandboxAddMultiPrio(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	opts := [][]NetworkOption{
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionEnableIPv6(true),
			NetworkOptionIpam(defaultipam.DriverName, "", nil, []*IpamConf{{PreferredPool: "fe90::/64"}}, nil),
		},
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionInternalNetwork(),
		},
		{
			NetworkOptionEnableIPv4(true),
		},
	}

	ctrlr, nws := getTestEnv(t, opts...)

	sbx, err := ctrlr.NewSandbox(t.Context(), "sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	sid := sbx.ID()

	ep1, err := nws[0].CreateEndpoint(t.Context(), "ep1")
	if err != nil {
		t.Fatal(err)
	}
	ep2, err := nws[1].CreateEndpoint(t.Context(), "ep2")
	if err != nil {
		t.Fatal(err)
	}
	ep3, err := nws[2].CreateEndpoint(t.Context(), "ep3")
	if err != nil {
		t.Fatal(err)
	}

	if err := ep1.Join(t.Context(), sbx, JoinOptionPriority(1)); err != nil {
		t.Fatal(err)
	}

	if err := ep2.Join(t.Context(), sbx, JoinOptionPriority(2)); err != nil {
		t.Fatal(err)
	}

	if err := ep3.Join(t.Context(), sbx, JoinOptionPriority(3)); err != nil {
		t.Fatal(err)
	}

	if ctrlr.sandboxes[sid].endpoints[0].ID() != ep3.ID() {
		t.Fatal("Expected ep3 to be at the top of the heap. But did not find ep3 at the top of the heap")
	}

	if len(sbx.Endpoints()) != 3 {
		t.Fatal("Expected 3 endpoints to be connected to the sandbox.")
	}

	if err := ep3.Leave(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}
	if ctrlr.sandboxes[sid].endpoints[0].ID() != ep2.ID() {
		t.Fatal("Expected ep2 to be at the top of the heap after removing ep3. But did not find ep2 at the top of the heap")
	}

	if err := ep2.Leave(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}
	if ctrlr.sandboxes[sid].endpoints[0].ID() != ep1.ID() {
		t.Fatal("Expected ep1 to be at the top of the heap after removing ep2. But did not find ep1 at the top of the heap")
	}

	// Re-add ep3 back
	if err := ep3.Join(t.Context(), sbx, JoinOptionPriority(3)); err != nil {
		t.Fatal(err)
	}

	if ctrlr.sandboxes[sid].endpoints[0].ID() != ep3.ID() {
		t.Fatal("Expected ep3 to be at the top of the heap after adding ep3 back. But did not find ep3 at the top of the heap")
	}

	if err := sbx.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(ctrlr.sandboxes) != 0 {
		t.Fatalf("controller sandboxes is not empty. len = %d", len(ctrlr.sandboxes))
	}
}

func TestGatewayEndpointRespectsPriorityPerAddressFamily(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	ctx := t.Context()

	opts := [][]NetworkOption{
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionEnableIPv6(true),
			NetworkOptionIpam(defaultipam.DriverName, "",
				[]*IpamConf{{PreferredPool: "172.30.0.0/24", Gateway: "172.30.0.1"}},
				[]*IpamConf{{PreferredPool: "fe90::/64", Gateway: "fe90::1"}}, nil),
		},
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionIpam(defaultipam.DriverName, "",
				[]*IpamConf{{PreferredPool: "172.31.0.0/24", Gateway: "172.31.0.1"}},
				nil, nil),
		},
	}

	ctrlr, nws := getTestEnv(t, opts...)

	sbx, err := ctrlr.NewSandbox(ctx, "sandbox-prio-per-family")
	assert.NilError(t, err)

	epDual, err := nws[0].CreateEndpoint(ctx, "ep-dual")
	assert.NilError(t, err)
	epV4, err := nws[1].CreateEndpoint(ctx, "ep-v4")
	assert.NilError(t, err)

	err = epDual.Join(ctx, sbx)
	assert.NilError(t, err)
	err = epV4.Join(ctx, sbx, JoinOptionPriority(1000))
	assert.NilError(t, err)

	gwep4, gwep6 := sbx.getGatewayEndpoint()
	assert.Assert(t, gwep4 != nil)
	assert.Assert(t, gwep6 != nil)
	assert.Check(t, is.Equal(gwep4.ID(), epV4.ID()))
	assert.Check(t, is.Equal(gwep6.ID(), epDual.ID()))

	err = sbx.Delete(ctx)
	assert.NilError(t, err)
}

func TestSandboxAddSamePrio(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	opts := [][]NetworkOption{
		{
			NetworkOptionEnableIPv4(true),
		},
		{
			NetworkOptionEnableIPv4(true),
		},
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionEnableIPv6(true),
			NetworkOptionIpam(defaultipam.DriverName, "", nil, []*IpamConf{{PreferredPool: "fe90::/64"}}, nil),
		},
		{
			NetworkOptionEnableIPv4(true),
			NetworkOptionInternalNetwork(),
		},
	}

	ctrlr, nws := getTestEnv(t, opts...)

	sbx, err := ctrlr.NewSandbox(t.Context(), "sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	sid := sbx.ID()

	epNw1, err := nws[1].CreateEndpoint(t.Context(), "ep1")
	if err != nil {
		t.Fatal(err)
	}
	epIPv6, err := nws[2].CreateEndpoint(t.Context(), "ep2")
	if err != nil {
		t.Fatal(err)
	}

	epInternal, err := nws[3].CreateEndpoint(t.Context(), "ep3")
	if err != nil {
		t.Fatal(err)
	}

	epNw0, err := nws[0].CreateEndpoint(t.Context(), "ep4")
	if err != nil {
		t.Fatal(err)
	}

	if err := epNw1.Join(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	if err := epIPv6.Join(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	if err := epInternal.Join(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	if err := epNw0.Join(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	// order should now be: epIPv6, epNw0, epNw1, epInternal
	if len(sbx.Endpoints()) != 4 {
		t.Fatal("Expected 4 endpoints to be connected to the sandbox.")
	}

	// IPv6 has precedence over IPv4
	if ctrlr.sandboxes[sid].endpoints[0].ID() != epIPv6.ID() {
		t.Fatal("Expected epIPv6 to be at the top of the heap. But did not find epIPv6 at the top of the heap")
	}

	// internal network has lowest precedence
	if ctrlr.sandboxes[sid].endpoints[3].ID() != epInternal.ID() {
		t.Fatal("Expected epInternal to be at the bottom of the heap. But did not find epInternal at the bottom of the heap")
	}

	if err := epIPv6.Leave(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	// 'test_nw_0' has precedence over 'test_nw_1'
	if ctrlr.sandboxes[sid].endpoints[0].ID() != epNw0.ID() {
		t.Fatal("Expected epNw0 to be at the top of the heap after removing epIPv6. But did not find epNw0 at the top of the heap")
	}

	if err := epNw1.Leave(t.Context(), sbx); err != nil {
		t.Fatal(err)
	}

	if err := sbx.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(ctrlr.sandboxes) != 0 {
		t.Fatalf("controller containers is not empty. len = %d", len(ctrlr.sandboxes))
	}
}

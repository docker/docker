//go:build linux

package libnetwork

import (
	"context"
	"errors"
	"math"
	"net"
	"strconv"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/libnetworkutils"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// withEmptyPortConfigTbl swaps in a fresh ingress reference table for the
// duration of the test, since it's process-global state.
func withEmptyPortConfigTbl(t *testing.T) {
	t.Helper()

	portConfigMu.Lock()
	orig := portConfigTbl
	portConfigTbl = map[types.PublishedPort]int{}
	portConfigMu.Unlock()

	t.Cleanup(func() {
		portConfigMu.Lock()
		defer portConfigMu.Unlock()
		portConfigTbl = orig
	})
}

// TestPublishedPortIgnoresTargetPort pins down what the reference table counts:
// the host-port reservation, which says nothing about TargetPort. Two configs
// differing only in TargetPort therefore describe one reservation - if they
// didn't, each would take a first reference and both would try to reserve host
// port 8080.
func TestPublishedPortIgnoresTargetPort(t *testing.T) {
	a, err := publishedPort(&PortConfig{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80})
	assert.NilError(t, err)
	b, err := publishedPort(&PortConfig{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 8080})
	assert.NilError(t, err)

	assert.Check(t, is.Equal(a, b))
	assert.Check(t, is.Equal(a, types.PublishedPort{Proto: types.TCP, Port: 8080, HostPort: 8080, NoSelfConnect: true}))

	// A differing protocol is a different reservation.
	udp, err := publishedPort(&PortConfig{Protocol: ProtocolUDP, PublishedPort: 8080, TargetPort: 80})
	assert.NilError(t, err)
	assert.Check(t, a != udp)
}

func TestPublishedPortsRejectsOutOfRange(t *testing.T) {
	_, err := publishedPort(&PortConfig{Protocol: ProtocolTCP, PublishedPort: math.MaxUint16 + 1})
	assert.Check(t, is.ErrorContains(err, "out of range"))

	// The whole set is narrowed before any of it is returned, so a caller is never
	// left with a partial conversion to undo.
	pps, err := publishedPorts([]*PortConfig{
		{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80},
		{Protocol: ProtocolTCP, PublishedPort: math.MaxUint16 + 1},
	})
	assert.Check(t, is.ErrorContains(err, "out of range"))
	assert.Check(t, is.Len(pps, 0))
}

// TestIngressPortRefsAcrossGenerations covers the case that made the reference
// table's old key wrong. A service updated from `-p 8080:80` to `-p 8080:8080`
// has two bindings alive at once whose port configs differ only in TargetPort;
// they share one host-port reservation, so only the first may publish it and only
// the last release may unpublish it.
func TestIngressPortRefsAcrossGenerations(t *testing.T) {
	withEmptyPortConfigTbl(t)

	ppsA, err := publishedPorts([]*PortConfig{{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80}})
	assert.NilError(t, err)
	ppsB, err := publishedPorts([]*PortConfig{{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 8080}})
	assert.NilError(t, err)

	want := types.PublishedPort{Proto: types.TCP, Port: 8080, HostPort: 8080, NoSelfConnect: true}

	// The first generation publishes the reservation.
	assert.Check(t, is.DeepEqual(refIngressPorts(ppsA), []types.PublishedPort{want}))

	// The second finds it already held, so has nothing to plumb - rather than
	// trying to reserve a host port that is already taken and failing.
	assert.Check(t, is.Len(refIngressPorts(ppsB), 0))

	// The old generation draining must not unpublish a port the new one serves.
	assert.Check(t, is.Len(unrefIngressPorts(ppsA), 0))

	// Only the last reference going releases it.
	assert.Check(t, is.DeepEqual(unrefIngressPorts(ppsB), []types.PublishedPort{want}))
	assert.Check(t, is.Len(portConfigTbl, 0))
}

// TestIngressPortRefsRollbackBalanced covers the add path's rollback: it drops a
// reference for every port it took one for, not just the newly-referenced ones,
// so a reservation another service still holds keeps its count.
func TestIngressPortRefsRollbackBalanced(t *testing.T) {
	withEmptyPortConfigTbl(t)

	shared := types.PublishedPort{Proto: types.TCP, Port: 8080, HostPort: 8080}
	fresh := types.PublishedPort{Proto: types.TCP, Port: 9090, HostPort: 9090}

	// Another service already holds 8080.
	assert.Check(t, is.DeepEqual(refIngressPorts([]types.PublishedPort{shared}), []types.PublishedPort{shared}))

	pps := []types.PublishedPort{shared, fresh}
	assert.Check(t, is.DeepEqual(refIngressPorts(pps), []types.PublishedPort{fresh}))

	// Publishing fails, so the caller undoes every reference it took.
	unrefIngressPorts(pps)

	// The other service's reference must survive, and the failed port's must be gone.
	assert.Check(t, is.DeepEqual(portConfigTbl, map[types.PublishedPort]int{shared: 1}))
}

// TestAddIngressPortsRollsBackEveryReference checks that addIngressPorts hands
// the whole set back to unrefIngressPorts on a publish failure, not just the
// subset it was told to plumb. Rolling back only the subset leaks a reference
// on every port some other service already held, pinning a reservation that
// nothing will ever release.
func TestAddIngressPortsRollsBackEveryReference(t *testing.T) {
	withEmptyPortConfigTbl(t)

	shared := &PortConfig{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80}
	fresh := &PortConfig{Protocol: ProtocolTCP, PublishedPort: 9090, TargetPort: 90}
	sharedPP, err := publishedPort(shared)
	assert.NilError(t, err)

	// Another service already holds 8080, so it isn't among the ports the failing
	// publish is asked to plumb.
	assert.Assert(t, is.DeepEqual(refIngressPorts([]types.PublishedPort{sharedPP}), []types.PublishedPort{sharedPP}))

	// An endpoint that isn't joined to a sandbox can't publish anything, so
	// AddEphemeralPorts fails before it reaches a driver - no root, no live
	// network, no host state.
	gwEP := &Endpoint{network: &Network{controller: &Controller{}}}
	err = addIngressPorts(gwEP, []*PortConfig{shared, fresh})
	assert.Check(t, is.ErrorContains(err, "failed to program ingress ports"))

	// The other service's reference must survive, and the failed port's must be gone.
	assert.Check(t, is.DeepEqual(portConfigTbl, map[types.PublishedPort]int{sharedPP: 1}))
}

// TestUnrefIngressPortsIgnoresUnknown checks that dropping a reference nobody
// holds is a no-op rather than driving the count negative - which would make a
// later release of the same port look like it still had holders.
func TestUnrefIngressPortsIgnoresUnknown(t *testing.T) {
	withEmptyPortConfigTbl(t)

	pp := types.PublishedPort{Proto: types.TCP, Port: 8080, HostPort: 8080}
	assert.Check(t, is.Len(unrefIngressPorts([]types.PublishedPort{pp}), 0))
	assert.Check(t, is.Len(portConfigTbl, 0))
}

// TestLoadBalancerSandboxHasNoResolver checks that neither a network's
// load-balancer sandbox nor the ingress sandbox runs the embedded DNS
// resolver. Like overlay, the test driver doesn't give its endpoints a
// gateway, so the ingress sandbox also joins docker_gwbridge.
func TestLoadBalancerSandboxHasNoResolver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ingress bool
	}{
		{name: "network"},
		{name: "ingress", ingress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t)()
			c, err := New(t.Context(), libnetworkutils.OptionTempNetnsDir(t), config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
			assert.NilError(t, err)
			assert.NilError(t, c.drvRegistry.RegisterDriver(gatewaylessDriverName, gatewaylessDriver{},
				driverapi.Capability{DataScope: scope.Global}))

			n, err := c.NewNetwork(t.Context(), gatewaylessDriverName, "testlb", "",
				NetworkOptionEnableIPv4(true),
				NetworkOptionIpam(defaultipam.DriverName, "", []*IpamConf{{PreferredPool: "192.0.2.0/24"}}, nil, nil),
				NetworkOptionLBEndpoint(net.ParseIP("192.0.2.2")),
				NetworkOptionIngress(tc.ingress))
			assert.NilError(t, err)
			defer func() {
				assert.Check(t, n.Delete(NetworkDeleteOptionRemoveLB))
			}()

			sb, err := c.GetSandbox(n.lbSandboxName())
			assert.NilError(t, err)
			assert.Check(t, is.Equal(sb.getEndpointInGWNetwork() != nil, tc.ingress))
			assert.Check(t, sb.resolver == nil, "embedded DNS resolver started")
		})
	}
}

// TestIngressSandboxGatewayNoIPv6 checks that the ingress sandbox's
// docker_gwbridge endpoint gets no IPv6 address, even if docker_gwbridge has
// IPv6 enabled.
// The ingress sandbox only forwards IPv4, so host IPv6 traffic to published
// ports must reach it over IPv4.
func TestIngressSandboxGatewayNoIPv6(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	c, err := New(t.Context(), config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
	assert.NilError(t, err)
	defer c.Stop()
	assert.NilError(t, c.drvRegistry.RegisterDriver(gatewaylessDriverName, gatewaylessDriver{},
		driverapi.Capability{DataScope: scope.Global}))

	gwn, err := c.NewNetwork(t.Context(), "bridge", libnGWNetwork, "",
		NetworkOptionDriverOpts(map[string]string{bridge.BridgeName: libnGWNetwork}),
		NetworkOptionEnableIPv4(true),
		NetworkOptionEnableIPv6(true),
		NetworkOptionIpam(defaultipam.DriverName, "",
			[]*IpamConf{{PreferredPool: "172.30.0.0/16"}},
			[]*IpamConf{{PreferredPool: "fd00:30::/64"}}, nil))
	assert.NilError(t, err)
	defer func() {
		assert.Check(t, gwn.Delete())
	}()

	n, err := c.NewNetwork(t.Context(), gatewaylessDriverName, "testlb", "",
		NetworkOptionEnableIPv4(true),
		NetworkOptionIpam(defaultipam.DriverName, "", []*IpamConf{{PreferredPool: "192.0.2.0/24"}}, nil, nil),
		NetworkOptionLBEndpoint(net.ParseIP("192.0.2.2")),
		NetworkOptionIngress(true))
	assert.NilError(t, err)
	defer func() {
		assert.Check(t, n.Delete(NetworkDeleteOptionRemoveLB))
	}()

	sb, err := c.GetSandbox(n.lbSandboxName())
	assert.NilError(t, err)
	gwEP := sb.getEndpointInGWNetwork()
	assert.Assert(t, gwEP != nil)
	assert.Check(t, gwEP.Iface().Address() != nil)
	assert.Check(t, is.Nil(gwEP.Iface().AddressIPv6()))

	gw4, gw6 := sb.getGatewayEndpoint()
	assert.Check(t, gw4 == gwEP, "docker_gwbridge endpoint is not the IPv4 gateway")
	assert.Check(t, is.Nil(gw6))
}

const gatewaylessDriverName = "gatewayless"

// gatewaylessDriver is a global-scope network driver that accepts every
// request and does nothing.
type gatewaylessDriver struct{}

func (gatewaylessDriver) CreateNetwork(context.Context, string, map[string]any, driverapi.NetworkInfo, []driverapi.IPAMData, []driverapi.IPAMData) error {
	return nil
}

func (gatewaylessDriver) DeleteNetwork(string) error { return nil }

func (gatewaylessDriver) CreateEndpoint(context.Context, string, string, driverapi.InterfaceInfo, map[string]any) error {
	return nil
}

func (gatewaylessDriver) DeleteEndpoint(string, string) error { return nil }

func (gatewaylessDriver) EndpointOperInfo(string, string) (map[string]any, error) { return nil, nil }

func (gatewaylessDriver) Join(context.Context, string, string, string, driverapi.JoinInfo, map[string]any, map[string]any) error {
	return nil
}

func (gatewaylessDriver) Leave(string, string) error { return nil }

func (gatewaylessDriver) Type() string { return gatewaylessDriverName }

func (gatewaylessDriver) IsBuiltIn() bool { return false }

var errFake = errors.New("fake failure")

// lbWithBackends returns a loadBalancer carrying the given backends, and enough
// of a service behind it to be named in a log entry.
func lbWithBackends(backends ...*lbBackend) *loadBalancer {
	lb := &loadBalancer{
		backEnds: make(map[string]*lbBackend, len(backends)),
		service: &service{
			name:         "svc",
			id:           "svcid",
			ingressPorts: portConfigs{{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80}},
		},
	}
	for i, be := range backends {
		lb.backEnds["ep"+strconv.Itoa(i)] = be
	}
	return lb
}

// TestProgramService covers the serviceProgrammed latch on the way up: which
// combinations of data plane, backends and ingress ports leave a service recorded
// as fully plumbed. The latch decides whether a later backend event retries the
// setup and whether teardown unpublishes the ports, so both a latch set too early
// and one never set are bugs that outlive the event that caused them.
func TestProgramService(t *testing.T) {
	tests := []struct {
		name           string
		ingress        bool
		programmed     bool
		backends       []*lbBackend
		dataPlaneFails bool
		publishFails   bool
		wantPublished  bool
		wantProgrammed bool
	}{
		{
			name:           "publishes and latches on the first backend",
			ingress:        true,
			backends:       []*lbBackend{{}},
			wantPublished:  true,
			wantProgrammed: true,
		},
		{
			// The latch must not be set for a partially-plumbed service: the next
			// backend event has to retry the ingress ports too, not just the data
			// plane, which it only does while the latch is clear.
			name:           "data-plane failure leaves the latch clear",
			ingress:        true,
			backends:       []*lbBackend{{}},
			dataPlaneFails: true,
		},
		{
			// Same, one step later. Publishing is the last step precisely so that its
			// failure is the one the latch reflects.
			name:          "publish failure leaves the latch clear",
			ingress:       true,
			backends:      []*lbBackend{{}},
			publishFails:  true,
			wantPublished: true,
		},
		{
			// Publishing is once per service, not once per backend event. A second
			// publish would take a second reference to the same host-port reservation,
			// and the single unpublish at teardown would leave it held forever.
			name:           "already latched publishes nothing",
			ingress:        true,
			programmed:     true,
			backends:       []*lbBackend{{}},
			wantProgrammed: true,
		},
		{
			// Mid rolling-update the old generation can be left with every backend
			// deweighted. Latching then would mean the ports were never published for
			// the backend that eventually arrives.
			name:     "all backends deweighted does not latch",
			ingress:  true,
			backends: []*lbBackend{{disabled: true}},
		},
		{
			name:    "no backends does not latch",
			ingress: true,
		},
		{
			// Only an ingress network has host-published ports; the latch still records
			// that the data plane is up.
			name:           "non-ingress network latches without publishing",
			backends:       []*lbBackend{{}},
			wantProgrammed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lb := lbWithBackends(tc.backends...)
			lb.serviceProgrammed = tc.programmed

			var dataPlaneCalled, published bool
			lb.programService(tc.ingress,
				func() bool {
					dataPlaneCalled = true
					return !tc.dataPlaneFails
				},
				func() error {
					published = true
					if tc.publishFails {
						return errFake
					}
					return nil
				})

			// The data plane is restated on every backend event, whatever the latch
			// says: it's the ingress ports that are once-per-service, not the map or
			// IPVS entries a new backend has to appear in.
			assert.Check(t, dataPlaneCalled, "the data plane must be stated unconditionally")
			assert.Check(t, is.Equal(published, tc.wantPublished))
			assert.Check(t, is.Equal(lb.serviceProgrammed, tc.wantProgrammed))
		})
	}
}

// TestProgramServiceRetriesAfterPublishFailure is what a cleared latch buys: the
// service's next backend event attempts the whole setup again. A latch set before
// publishing had succeeded would leave the ports closed for the rest of the
// service's life, since nothing else ever retries them.
func TestProgramServiceRetriesAfterPublishFailure(t *testing.T) {
	lb := lbWithBackends(&lbBackend{})

	dataPlane := func() bool { return true }
	var publishes int
	publish := func() error {
		publishes++
		if publishes == 1 {
			return errFake
		}
		return nil
	}

	lb.programService(true, dataPlane, publish)
	assert.Check(t, !lb.serviceProgrammed, "a failed publish must not latch")

	lb.programService(true, dataPlane, publish)
	assert.Check(t, lb.serviceProgrammed)
	assert.Check(t, is.Equal(publishes, 2), "the retry must reach the ports")

	// And now that it has latched, further events leave the ports alone.
	lb.programService(true, dataPlane, publish)
	assert.Check(t, is.Equal(publishes, 2))
}

// TestProgramServiceLatchesWhenBackendsReturn covers the other reason the latch
// stays clear. A service with nothing to load-balance to publishes no ports, so
// its latch must be clear for the backend that eventually arrives to publish them
// - the case that makes the latch's independence from the live backend count load
// bearing rather than merely tidy.
func TestProgramServiceLatchesWhenBackendsReturn(t *testing.T) {
	be := &lbBackend{disabled: true}
	lb := lbWithBackends(be)

	dataPlane := func() bool { return true }
	var publishes int
	publish := func() error {
		publishes++
		return nil
	}

	lb.programService(true, dataPlane, publish)
	assert.Check(t, !lb.serviceProgrammed)
	assert.Check(t, is.Equal(publishes, 0))

	be.disabled = false
	lb.programService(true, dataPlane, publish)
	assert.Check(t, lb.serviceProgrammed)
	assert.Check(t, is.Equal(publishes, 1))
}

// TestUnprogramService covers the latch on the way down, where the stake is a
// host-port reservation shared with whatever other service or generation holds it.
func TestUnprogramService(t *testing.T) {
	tests := []struct {
		name            string
		ingress         bool
		programmed      bool
		unpublishFails  bool
		wantUnpublished bool
		wantProgrammed  bool
	}{
		{
			name:            "unpublishes and clears the latch",
			ingress:         true,
			programmed:      true,
			wantUnpublished: true,
		},
		{
			// The service's ports were never published, so unpublishing here would
			// drop a reference it never took - releasing a reservation that another
			// service, or another generation of this one, is still serving.
			name:    "never latched unpublishes nothing",
			ingress: true,
		},
		{
			// The latch is left set, which is inconsequential: rmLBBackend only reaches
			// here for the last backend of the service, by which point rmServiceBinding
			// has already detached this loadBalancer, so nothing reads the latch again.
			name:            "unpublish failure keeps the latch",
			ingress:         true,
			programmed:      true,
			unpublishFails:  true,
			wantUnpublished: true,
			wantProgrammed:  true,
		},
		{
			name:       "non-ingress network clears the latch without unpublishing",
			programmed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lb := lbWithBackends()
			lb.serviceProgrammed = tc.programmed

			var unpublished bool
			lb.unprogramService(tc.ingress, func() error {
				unpublished = true
				if tc.unpublishFails {
					return errFake
				}
				return nil
			})

			assert.Check(t, is.Equal(unpublished, tc.wantUnpublished))
			assert.Check(t, is.Equal(lb.serviceProgrammed, tc.wantProgrammed))
		})
	}
}

//go:build linux

package bridge

import (
	"errors"
	"os"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/drvregistry"
	"github.com/moby/moby/v2/daemon/libnetwork/portallocator"
	"github.com/moby/moby/v2/daemon/libnetwork/portmappers/nat"
	"github.com/moby/moby/v2/daemon/libnetwork/portmappers/routed"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/moby/moby/v2/internal/testutil/storeutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	ephTestNID = "ephnetwork"
	ephTestEID = "ephendpoint"
)

// ephTestEnv is a driver with one network and one already-joined endpoint,
// registered with each other so that the driver-level AddEphemeralPorts and
// DelEphemeralPorts entry points can be called the way libnetwork calls them.
type ephTestEnv struct {
	d  *driver
	n  *bridgeNetwork
	ep *bridgeEndpoint
	// stopProxyErr, when set, makes every proxy's stop function fail. That's the
	// cheapest way to make unmapPBs report an error while still having released
	// the bindings, which is the case DelEphemeralPorts has to commit through.
	stopProxyErr *bool
}

func newEphTestEnv(t *testing.T, pbm portBindingMode) *ephTestEnv {
	t.Helper()
	useStubFirewaller(t)

	stopProxyErr := false
	origStartProxy := startProxy
	startProxy = func(pb types.PortBinding, _ string, listenSock *os.File) (func() error, error) {
		return func() error {
			if stopProxyErr {
				return errors.New("cannot stop proxy")
			}
			return nil
		}, nil
	}
	t.Cleanup(func() { startProxy = origStartProxy })

	pms := &drvregistry.PortMappers{}
	assert.NilError(t, nat.Register(pms, nat.Config{}))
	assert.NilError(t, routed.Register(pms))

	d, err := newDriver(storeutils.NewTempStore(t), Configuration{
		EnableIPTables: true,
		EnableProxy:    true,
	}, pms)
	assert.NilError(t, err)

	n := &bridgeNetwork{
		id: ephTestNID,
		config: &networkConfiguration{
			BridgeName: "dummybridge",
			EnableIPv4: true,
		},
		bridge:    &bridgeInterface{},
		driver:    d,
		endpoints: map[string]*bridgeEndpoint{},
	}
	fwn, err := n.newFirewallerNetwork(t.Context())
	assert.NilError(t, err)
	n.firewallerNetwork = fwn

	d.networks[ephTestNID] = n

	ep := &bridgeEndpoint{
		id:               ephTestEID,
		nid:              ephTestNID,
		addr:             newIPNet(t, "192.168.60.2/24"),
		portBindingState: pbm,
	}
	n.endpoints[ephTestEID] = ep

	portallocator.Get().ReleaseAll()

	return &ephTestEnv{d: d, n: n, ep: ep, stopProxyErr: &stopProxyErr}
}

func pubPort(proto types.Protocol, port uint16) types.PublishedPort {
	return types.PublishedPort{Proto: proto, Port: port, HostPort: port}
}

// TestAddEphemeralPortsTracksSeparately checks that ports published on a joined
// endpoint land in ephemeralPortMapping and not in portMapping - the split exists
// so that only portMapping is written to the driver's datastore.
func TestAddEphemeralPortsTracksSeparately(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true, ipv4: true})

	err := env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)})
	assert.NilError(t, err)

	assert.Check(t, is.Len(env.ep.portMapping, 0), "join-time bindings must be untouched")
	assert.Assert(t, is.Len(env.ep.ephemeralPortMapping, 1))
	assert.Check(t, is.Equal(env.ep.ephemeralPortMapping[0].HostPort, uint16(8080)))
	assert.Check(t, is.Equal(env.ep.ephemeralPortMapping[0].Proto, types.TCP))
}

// TestAddEphemeralPortsNoSelfConnect checks that a published port's
// NoSelfConnect setting reaches the firewaller, which needs it to decide
// whether packets from the bridge are DNATed or left for the userland proxy.
func TestAddEphemeralPortsNoSelfConnect(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{ipv4: true})

	pp := pubPort(types.TCP, 8080)
	pp.NoSelfConnect = true
	err := env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID, []types.PublishedPort{pp})
	assert.NilError(t, err)

	fwPorts := env.n.firewallerNetwork.(*firewaller.StubFirewallerNetwork).Ports
	assert.Assert(t, is.Len(fwPorts, 1))
	assert.Check(t, fwPorts[0].NoSelfConnect)
}

// TestAddEphemeralPortsNoActiveMode documents a sharp edge rather than asserting
// it is desirable: with no NAT family active on the endpoint, addPortMappings
// generates no bindings and returns no error, so AddEphemeralPorts reports success
// having published nothing. Callers that latch on a nil return - as the Swarm
// ingress path does - will believe the ports are reachable.
func TestAddEphemeralPortsNoActiveMode(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true})

	err := env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)})
	assert.NilError(t, err, "reports success...")
	assert.Check(t, is.Len(env.ep.ephemeralPortMapping, 0), "...having published nothing")
}

// TestDelEphemeralPortsRemovesOnlyMatching checks that unpublishing one port
// leaves the endpoint's other published ports alone.
func TestDelEphemeralPortsRemovesOnlyMatching(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true, ipv4: true})

	assert.NilError(t, env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080), pubPort(types.TCP, 9090)}))
	assert.Assert(t, is.Len(env.ep.ephemeralPortMapping, 2))

	assert.NilError(t, env.d.DelEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)}))

	assert.Assert(t, is.Len(env.ep.ephemeralPortMapping, 1))
	assert.Check(t, is.Equal(env.ep.ephemeralPortMapping[0].HostPort, uint16(9090)))
}

// TestDelEphemeralPortsIgnoresUnknown checks that unpublishing a port that isn't
// published is a no-op rather than an error - the API documents that ports which
// aren't currently published are ignored.
func TestDelEphemeralPortsIgnoresUnknown(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true, ipv4: true})

	assert.NilError(t, env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)}))

	assert.NilError(t, env.d.DelEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 9999)}))
	assert.Check(t, is.Len(env.ep.ephemeralPortMapping, 1), "the published port must survive")
}

// TestDelEphemeralPortsCommitsOnUnmapError is the regression test for the
// refcount-restore bug: unmapPBs is best-effort, so by the time it reports an
// error the bindings have been released anyway. DelEphemeralPorts must therefore
// stop tracking them regardless, or the endpoint claims reservations that no
// longer exist and its owner re-takes references it should have dropped.
func TestDelEphemeralPortsCommitsOnUnmapError(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true, ipv4: true})

	assert.NilError(t, env.d.AddEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)}))
	assert.Assert(t, is.Len(env.ep.ephemeralPortMapping, 1))

	// Make the teardown report a failure.
	*env.stopProxyErr = true

	err := env.d.DelEphemeralPorts(t.Context(), ephTestNID, ephTestEID,
		[]types.PublishedPort{pubPort(types.TCP, 8080)})
	assert.Check(t, err != nil, "the teardown failure must be reported")
	assert.Check(t, is.Len(env.ep.ephemeralPortMapping, 0),
		"the binding must be dropped even so - it was released before the error")
}

// TestAddEphemeralPortsUnknownEndpoint checks the lookups the driver does before
// touching anything.
func TestAddEphemeralPortsUnknownEndpoint(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	env := newEphTestEnv(t, portBindingMode{routed: true, ipv4: true})
	ports := []types.PublishedPort{pubPort(types.TCP, 8080)}

	err := env.d.AddEphemeralPorts(t.Context(), ephTestNID, "nosuchep", ports)
	assert.Check(t, err != nil, "unknown endpoint")

	err = env.d.AddEphemeralPorts(t.Context(), "nosuchnetwork", ephTestEID, ports)
	assert.Check(t, err != nil, "unknown network")

	// An empty request is accepted and does nothing, so callers needn't special-case it.
	assert.NilError(t, env.d.AddEphemeralPorts(t.Context(), "nosuchnetwork", ephTestEID, nil))
	assert.NilError(t, env.d.DelEphemeralPorts(t.Context(), "nosuchnetwork", ephTestEID, nil))
}

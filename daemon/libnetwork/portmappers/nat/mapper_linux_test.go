package nat

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/portmapperapi"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// fakePortDriverClient stands in for the rootlesskit port driver. AddPort
// returns failErr for the first failN calls (simulating a host-namespace bind
// collision) and records the host port it was asked to bind on each call.
type fakePortDriverClient struct {
	childIP netip.Addr
	failN   int
	failErr error
	ports   []int
}

func (f *fakePortDriverClient) ChildHostIP(_ string, _ netip.Addr) netip.Addr {
	return f.childIP
}

func (f *fakePortDriverClient) AddPort(_ context.Context, _ string, _, _ netip.Addr, hostPort int) (func() error, error) {
	f.ports = append(f.ports, hostPort)
	if len(f.ports) <= f.failN {
		return nil, f.failErr
	}
	return func() error { return nil }, nil
}

// errAddrInUse mimics the error rootlesskit surfaces over IPC: the errno is
// flattened into the message rather than wrapped.
var errAddrInUse = errors.New("error while calling RootlessKit PortManager.AddPort(): listen tcp 0.0.0.0:0: bind: address already in use")

func TestMapPortsRetriesHostNamespaceCollision(t *testing.T) {
	f := &fakePortDriverClient{
		childIP: netip.MustParseAddr("127.0.0.1"),
		failN:   1,
		failErr: errAddrInUse,
	}
	pm := &PortMapper{pdc: f}
	// Dynamic host port (HostPort == 0), so a retry is allowed to pick a new one.
	cfg := []portmapperapi.PortBindingReq{{PortBinding: types.PortBinding{Proto: types.TCP, Port: 80, HostIP: net.IPv4zero}}}

	pbs, err := pm.MapPorts(t.Context(), cfg)
	assert.NilError(t, err)
	assert.Check(t, is.Len(pbs, 1))
	assert.Check(t, is.Len(f.ports, 2)) // failed once, retried once
	assert.Check(t, f.ports[0] != f.ports[1], "retry should pick a different host port")
	assert.NilError(t, pm.UnmapPorts(t.Context(), pbs))
}

func TestMapPortsNoRetryForFixedPort(t *testing.T) {
	// Grab a free port to use as a fixed (non-dynamic) host port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NilError(t, err)
	fixed := l.Addr().(*net.TCPAddr).Port
	assert.NilError(t, l.Close())

	f := &fakePortDriverClient{
		childIP: netip.MustParseAddr("127.0.0.1"),
		failN:   maxHostBindAttempts, // would always fail if retried
		failErr: errAddrInUse,
	}
	pm := &PortMapper{pdc: f}
	cfg := []portmapperapi.PortBindingReq{{PortBinding: types.PortBinding{
		Proto:       types.TCP,
		Port:        80,
		HostIP:      net.IPv4zero,
		HostPort:    uint16(fixed),
		HostPortEnd: uint16(fixed),
	}}}

	_, err = pm.MapPorts(t.Context(), cfg)
	assert.Check(t, is.ErrorContains(err, "address already in use"))
	assert.Check(t, is.Len(f.ports, 1), "a fixed host port must not be retried")
}

func TestBindHostPortsError(t *testing.T) {
	cfg := []portmapperapi.PortBindingReq{
		{
			PortBinding: types.PortBinding{
				Proto:       types.TCP,
				Port:        80,
				HostPort:    8080,
				HostPortEnd: 8080,
			},
		},
		{
			PortBinding: types.PortBinding{
				Proto:       types.TCP,
				Port:        80,
				HostPort:    8080,
				HostPortEnd: 8081,
			},
		},
	}
	pm := &PortMapper{}
	pbs, err := pm.MapPorts(t.Context(), cfg)
	assert.Check(t, is.Error(err, "port binding mismatch 80/tcp:8080-8080, 80/tcp:8080-8081"))
	assert.Check(t, is.Nil(pbs))
}

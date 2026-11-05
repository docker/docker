//go:build windows

package windows

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/storeutils"
	"gotest.tools/v3/assert"
)

func testNetwork(networkType string, t *testing.T) {
	d, err := newDriver(networkType, storeutils.NewTempStore(t), newContainerNamespaceCache())
	assert.NilError(t, err)
	bnw, _ := types.ParseCIDR("172.16.0.0/24")
	br, _ := types.ParseCIDR("172.16.0.1/16")

	netOption := make(map[string]any)
	networkOptions := map[string]string{
		NetworkName: "TestNetwork",
	}

	netOption[netlabel.GenericData] = networkOptions
	ipdList := []driverapi.IPAMData{
		{
			Pool:    bnw,
			Gateway: br,
		},
	}

	err = d.CreateNetwork(context.Background(), "dummy", netOption, nil, ipdList, nil)
	if err != nil {
		t.Fatalf("Failed to create bridge: %v", err)
	}
	defer func() {
		err = d.DeleteNetwork("dummy")
		if err != nil {
			t.Fatalf("Failed to create bridge: %v", err)
		}
	}()

	epOptions := make(map[string]any)
	te := &testEndpoint{}
	err = d.CreateEndpoint(t.Context(), "dummy", "ep1", te.Interface(), epOptions)
	if err != nil {
		t.Fatalf("Failed to create an endpoint : %s", err.Error())
	}

	err = d.DeleteEndpoint("dummy", "ep1")
	if err != nil {
		t.Fatalf("Failed to delete an endpoint : %s", err.Error())
	}
}

func TestNAT(t *testing.T) {
	t.Skip("Test does not work on CI and was never running to begin with")
	testNetwork("nat", t)
}

func TestTransparent(t *testing.T) {
	t.Skip("Test does not work on CI and was never running to begin with")
	testNetwork("transparent", t)
}

func TestFindContainerNamespaceID(t *testing.T) {
	tests := []struct {
		name          string
		containerID   string
		endpoints     []hcsshim.HNSEndpoint
		expected      string
		expectedError string
	}{
		{
			name:        "find namespace",
			containerID: "container",
			endpoints: []hcsshim.HNSEndpoint{
				{
					Namespace:        &hcsshim.Namespace{ID: "unrelated"},
					SharedContainers: []string{"other-container"},
				},
				{
					SharedContainers: []string{"container"},
				},
				{
					Namespace:        &hcsshim.Namespace{},
					SharedContainers: []string{"container"},
				},
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace"},
					SharedContainers: []string{"container"},
				},
			},
			expected: "namespace",
		},
		{
			name:        "same namespace on multiple endpoints",
			containerID: "container",
			endpoints: []hcsshim.HNSEndpoint{
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace"},
					SharedContainers: []string{"container"},
				},
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace"},
					SharedContainers: []string{"container"},
				},
			},
			expected: "namespace",
		},
		{
			name:        "different namespaces",
			containerID: "container",
			endpoints: []hcsshim.HNSEndpoint{
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace-1"},
					SharedContainers: []string{"container"},
				},
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace-2"},
					SharedContainers: []string{"container"},
				},
			},
			expectedError: "multiple network namespaces found",
		},
		{
			name:        "missing container",
			containerID: "missing",
			endpoints: []hcsshim.HNSEndpoint{
				{
					Namespace:        &hcsshim.Namespace{ID: "namespace"},
					SharedContainers: []string{"container"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			namespaceID, err := findContainerNamespaceID(tc.endpoints, tc.containerID)
			if tc.expectedError != "" {
				assert.ErrorContains(t, err, tc.expectedError)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, namespaceID, tc.expected)
		})
	}
}

type localDriverRegisterer struct {
	drivers map[string]*driver
}

func (r *localDriverRegisterer) RegisterDriver(name string, drv driverapi.Driver, _ driverapi.Capability) error {
	r.drivers[name] = drv.(*driver)
	return nil
}

func (*localDriverRegisterer) RegisterNetworkAllocator(string, driverapi.NetworkAllocator) error {
	return nil
}

func TestBuiltinDriversShareContainerNamespaceCache(t *testing.T) {
	registerer := &localDriverRegisterer{drivers: map[string]*driver{}}
	err := RegisterBuiltinLocalDrivers(registerer, storeutils.NewTempStore(t))
	assert.NilError(t, err)
	assert.Equal(t, len(registerer.drivers), len(builtinLocalDrivers))

	registerer.drivers["nat"].containerNamespaces.set("container", "namespace")
	assert.Equal(t, registerer.drivers["transparent"].containerNamespaces.get("container"), "namespace")
}

func TestLiveContainerNamespaceIDReplacesCachedNamespace(t *testing.T) {
	d := &driver{containerNamespaces: newContainerNamespaceCache()}
	d.containerNamespaces.set("container", "cached-namespace")

	namespaceID, err := d.liveContainerNamespaceID([]hcsshim.HNSEndpoint{
		{
			Namespace:        &hcsshim.Namespace{ID: "live-namespace"},
			SharedContainers: []string{"container"},
		},
	}, "container")
	assert.NilError(t, err)
	assert.Equal(t, namespaceID, "live-namespace")
	assert.Equal(t, d.containerNamespaces.get("container"), "live-namespace")
}

func TestCachedContainerNamespaceID(t *testing.T) {
	t.Run("cache miss", func(t *testing.T) {
		d := &driver{containerNamespaces: newContainerNamespaceCache()}
		_, err := d.cachedContainerNamespaceID("container", func(string) (*hcn.HostComputeNamespace, error) {
			t.Fatal("namespace lookup called for empty cache")
			return nil, nil
		})
		assert.ErrorContains(t, err, "failed to find network namespace")
	})

	t.Run("valid namespace", func(t *testing.T) {
		d := &driver{containerNamespaces: newContainerNamespaceCache()}
		d.containerNamespaces.set("container", "namespace")

		namespaceID, err := d.cachedContainerNamespaceID("container", func(id string) (*hcn.HostComputeNamespace, error) {
			assert.Equal(t, id, "namespace")
			return &hcn.HostComputeNamespace{}, nil
		})
		assert.NilError(t, err)
		assert.Equal(t, namespaceID, "namespace")
	})

	t.Run("namespace not found", func(t *testing.T) {
		d := &driver{containerNamespaces: newContainerNamespaceCache()}
		d.containerNamespaces.set("container", "namespace")

		_, err := d.cachedContainerNamespaceID("container", func(string) (*hcn.HostComputeNamespace, error) {
			return nil, hcn.NamespaceNotFoundError{NamespaceID: "namespace"}
		})
		assert.ErrorContains(t, err, "is no longer valid")
		assert.Equal(t, d.containerNamespaces.get("container"), "")
	})

	t.Run("transient error retains namespace for retry", func(t *testing.T) {
		d := &driver{containerNamespaces: newContainerNamespaceCache()}
		d.containerNamespaces.set("container", "namespace")

		_, err := d.cachedContainerNamespaceID("container", func(string) (*hcn.HostComputeNamespace, error) {
			return nil, errors.New("HCN service unavailable")
		})
		assert.ErrorContains(t, err, "failed to query cached network namespace")
		assert.Equal(t, d.containerNamespaces.get("container"), "namespace")

		namespaceID, err := d.cachedContainerNamespaceID("container", func(string) (*hcn.HostComputeNamespace, error) {
			return &hcn.HostComputeNamespace{}, nil
		})
		assert.NilError(t, err)
		assert.Equal(t, namespaceID, "namespace")
	})
}

func TestCacheAndRemoveNamespaceEndpoint(t *testing.T) {
	d := &driver{containerNamespaces: newContainerNamespaceCache()}
	removeErr := errors.New("transient remove error")

	err := d.cacheAndRemoveNamespaceEndpoint("container", "namespace", "endpoint", func(namespaceID, endpointID string) error {
		assert.Equal(t, namespaceID, "namespace")
		assert.Equal(t, endpointID, "endpoint")
		assert.Equal(t, d.containerNamespaces.get("container"), "namespace")
		return removeErr
	})
	assert.ErrorIs(t, err, removeErr)
	assert.Equal(t, d.containerNamespaces.get("container"), "namespace")
}

type testEndpoint struct {
	t                     *testing.T
	src                   string
	dst                   string
	address               string
	macAddress            string
	gateway               string
	disableGatewayService bool
}

func (test *testEndpoint) Interface() driverapi.InterfaceInfo {
	return test
}

func (test *testEndpoint) Address() *net.IPNet {
	if test.address == "" {
		return nil
	}
	nw, _ := types.ParseCIDR(test.address)
	return nw
}

func (test *testEndpoint) AddressIPv6() *net.IPNet {
	return nil
}

func (test *testEndpoint) MacAddress() net.HardwareAddr {
	if test.macAddress == "" {
		return nil
	}
	mac, _ := net.ParseMAC(test.macAddress)
	return mac
}

func (test *testEndpoint) SetMacAddress(mac net.HardwareAddr) error {
	if test.macAddress != "" {
		return types.ForbiddenErrorf("endpoint interface MAC address present (%s). Cannot be modified with %s.", test.macAddress, mac)
	}

	if mac == nil {
		return types.InvalidParameterErrorf("tried to set nil MAC address to endpoint interface")
	}
	test.macAddress = mac.String()
	return nil
}

func (test *testEndpoint) SetIPAddress(address *net.IPNet) error {
	if address.IP == nil {
		return types.InvalidParameterErrorf("tried to set nil IP address to endpoint interface")
	}

	test.address = address.String()
	return nil
}

func (test *testEndpoint) InterfaceName() driverapi.InterfaceNameInfo {
	return test
}

func (test *testEndpoint) SetGateway(ipv4 net.IP) error {
	return nil
}

func (test *testEndpoint) SetGatewayIPv6(ipv6 net.IP) error {
	return nil
}

func (test *testEndpoint) ForceGw4() {}
func (test *testEndpoint) ForceGw6() {}

func (test *testEndpoint) SetNames(_, _, _ string) error {
	return nil
}

func (test *testEndpoint) AddStaticRoute(destination *net.IPNet, routeType types.RouteType, nextHop net.IP) error {
	return nil
}

func (test *testEndpoint) DisableGatewayService() {
	test.disableGatewayService = true
}

func (test *testEndpoint) NetnsPath() string {
	return ""
}

func (test *testEndpoint) SetCreatedInContainer(bool) {
}

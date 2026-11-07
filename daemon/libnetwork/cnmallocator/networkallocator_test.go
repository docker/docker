package cnmallocator

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/manager/allocator/networkallocator"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func newNetworkAllocator(t *testing.T) networkallocator.NetworkAllocator {
	na, err := (&Provider{}).NewAllocator(nil)
	assert.Check(t, err)
	assert.Check(t, na != nil)
	return na
}

func TestNew(t *testing.T) {
	newNetworkAllocator(t)
}

func TestAllocateInvalidIPAM(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{
					Name: "invalidipam,",
				},
			},
		},
	}
	err := na.Allocate(n)
	assert.Check(t, is.ErrorContains(err, ""))
}

func TestAllocateInvalidDriver(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{
				Name: "invaliddriver",
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, is.ErrorContains(err, ""))
}

func TestNetworkDoubleAllocate(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)

	err = na.Allocate(n)
	assert.Check(t, is.ErrorContains(err, ""))
}

func TestAllocateEmptyConfig(t *testing.T) {
	na1 := newNetworkAllocator(t)
	na2 := newNetworkAllocator(t)
	n1 := &api.Network{
		ID: "testID1",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test1",
			},
		},
	}

	n2 := &api.Network{
		ID: "testID2",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test2",
			},
		},
	}

	err := na1.Allocate(n1)
	assert.Check(t, err)
	assert.Check(t, n1.IPAM.Configs != nil)
	assert.Check(t, is.Equal(len(n1.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n1.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n1.IPAM.Configs[0].Reserved), 0))

	_, subnet11, err := net.ParseCIDR(n1.IPAM.Configs[0].Subnet)
	assert.Check(t, err)

	gwip11 := net.ParseIP(n1.IPAM.Configs[0].Gateway)
	assert.Check(t, gwip11 != nil)

	err = na1.Allocate(n2)
	assert.Check(t, err)
	assert.Check(t, n2.IPAM.Configs != nil)
	assert.Check(t, is.Equal(len(n2.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n2.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n2.IPAM.Configs[0].Reserved), 0))

	_, subnet21, err := net.ParseCIDR(n2.IPAM.Configs[0].Subnet)
	assert.Check(t, err)

	gwip21 := net.ParseIP(n2.IPAM.Configs[0].Gateway)
	assert.Check(t, gwip21 != nil)

	// Allocate n1 ans n2 with another allocator instance but in
	// intentionally reverse order.
	err = na2.Allocate(n2)
	assert.Check(t, err)
	assert.Check(t, n2.IPAM.Configs != nil)
	assert.Check(t, is.Equal(len(n2.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n2.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n2.IPAM.Configs[0].Reserved), 0))

	_, subnet22, err := net.ParseCIDR(n2.IPAM.Configs[0].Subnet)
	assert.Check(t, err)
	assert.Check(t, is.DeepEqual(subnet21, subnet22))

	gwip22 := net.ParseIP(n2.IPAM.Configs[0].Gateway)
	assert.Check(t, is.DeepEqual(gwip21, gwip22))

	err = na2.Allocate(n1)
	assert.Check(t, err)
	assert.Check(t, n1.IPAM.Configs != nil)
	assert.Check(t, is.Equal(len(n1.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n1.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n1.IPAM.Configs[0].Reserved), 0))

	_, subnet12, err := net.ParseCIDR(n1.IPAM.Configs[0].Subnet)
	assert.Check(t, err)
	assert.Check(t, is.DeepEqual(subnet11, subnet12))

	gwip12 := net.ParseIP(n1.IPAM.Configs[0].Gateway)
	assert.Check(t, is.DeepEqual(gwip11, gwip12))
}

func TestAllocateWithOneSubnet(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet: "192.168.1.0/24",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(n.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n.IPAM.Configs[0].Reserved), 0))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Subnet, "192.168.1.0/24"))

	ip := net.ParseIP(n.IPAM.Configs[0].Gateway)
	assert.Check(t, ip != nil)
}

func TestAllocateWithOneSubnetGateway(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.1.0/24",
						Gateway: "192.168.1.1",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(n.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n.IPAM.Configs[0].Reserved), 0))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Subnet, "192.168.1.0/24"))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Gateway, "192.168.1.1"))
}

func TestAllocateWithOneSubnetInvalidGateway(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.1.0/24",
						Gateway: "192.168.2.1",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, is.ErrorContains(err, ""))
}

// TestAllocateWithSmallSubnet validates that /32 subnets don't produce an error,
// as /31 and /32 subnets are supported by docker daemon, starting with
// https://github.com/moby/moby/commit/3a938df4b570aad3bfb4d5342379582e872fc1a3,
func TestAllocateWithSmallSubnet(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet: "1.1.1.1/32",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)
}

func TestAllocateWithTwoSubnetsNoGateway(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet: "192.168.1.0/24",
					},
					{
						Subnet: "192.168.2.0/24",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(n.IPAM.Configs), 2))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n.IPAM.Configs[0].Reserved), 0))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Subnet, "192.168.1.0/24"))
	assert.Check(t, is.Equal(n.IPAM.Configs[1].Range, ""))
	assert.Check(t, is.Equal(len(n.IPAM.Configs[1].Reserved), 0))
	assert.Check(t, is.Equal(n.IPAM.Configs[1].Subnet, "192.168.2.0/24"))

	ip := net.ParseIP(n.IPAM.Configs[0].Gateway)
	assert.Check(t, ip != nil)
	ip = net.ParseIP(n.IPAM.Configs[1].Gateway)
	assert.Check(t, ip != nil)
}

func TestFree(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.1.0/24",
						Gateway: "192.168.1.1",
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)

	err = na.Deallocate(n)
	assert.Check(t, err)

	// Reallocate again to make sure it succeeds.
	err = na.Allocate(n)
	assert.Check(t, err)
}

func TestAllocateTaskFree(t *testing.T) {
	na1 := newNetworkAllocator(t)
	na2 := newNetworkAllocator(t)
	n1 := &api.Network{
		ID: "testID1",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test1",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.1.0/24",
						Gateway: "192.168.1.1",
					},
				},
			},
		},
	}

	n2 := &api.Network{
		ID: "testID2",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test2",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.2.0/24",
						Gateway: "192.168.2.1",
					},
				},
			},
		},
	}

	task1 := &api.Task{
		Networks: []*api.NetworkAttachment{
			{
				Network: n1,
			},
			{
				Network: n2,
			},
		},
	}

	task2 := &api.Task{
		Networks: []*api.NetworkAttachment{
			{
				Network: n1,
			},
			{
				Network: n2,
			},
		},
	}

	err := na1.Allocate(n1)
	assert.Check(t, err)

	err = na1.Allocate(n2)
	assert.Check(t, err)

	err = na1.AllocateTask(task1)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task1.Networks[0].Addresses), 1))
	assert.Check(t, is.Equal(len(task1.Networks[1].Addresses), 1))

	_, subnet1, _ := net.ParseCIDR("192.168.1.0/24")
	_, subnet2, _ := net.ParseCIDR("192.168.2.0/24")

	// variable coding: network/task/allocator
	ip111, _, err := net.ParseCIDR(task1.Networks[0].Addresses[0])
	assert.Check(t, err)

	ip211, _, err := net.ParseCIDR(task1.Networks[1].Addresses[0])
	assert.Check(t, err)

	assert.Check(t, is.Equal(subnet1.Contains(ip111), true))
	assert.Check(t, is.Equal(subnet2.Contains(ip211), true))

	err = na1.AllocateTask(task2)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task2.Networks[0].Addresses), 1))
	assert.Check(t, is.Equal(len(task2.Networks[1].Addresses), 1))

	ip121, _, err := net.ParseCIDR(task2.Networks[0].Addresses[0])
	assert.Check(t, err)

	ip221, _, err := net.ParseCIDR(task2.Networks[1].Addresses[0])
	assert.Check(t, err)

	assert.Check(t, is.Equal(subnet1.Contains(ip121), true))
	assert.Check(t, is.Equal(subnet2.Contains(ip221), true))

	// Now allocate the same the same tasks in a second allocator
	// but intentionally in reverse order.
	err = na2.Allocate(n1)
	assert.Check(t, err)

	err = na2.Allocate(n2)
	assert.Check(t, err)

	err = na2.AllocateTask(task2)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task2.Networks[0].Addresses), 1))
	assert.Check(t, is.Equal(len(task2.Networks[1].Addresses), 1))

	ip122, _, err := net.ParseCIDR(task2.Networks[0].Addresses[0])
	assert.Check(t, err)

	ip222, _, err := net.ParseCIDR(task2.Networks[1].Addresses[0])
	assert.Check(t, err)

	assert.Check(t, is.Equal(subnet1.Contains(ip122), true))
	assert.Check(t, is.Equal(subnet2.Contains(ip222), true))
	assert.Check(t, is.DeepEqual(ip121, ip122))
	assert.Check(t, is.DeepEqual(ip221, ip222))

	err = na2.AllocateTask(task1)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task1.Networks[0].Addresses), 1))
	assert.Check(t, is.Equal(len(task1.Networks[1].Addresses), 1))

	ip112, _, err := net.ParseCIDR(task1.Networks[0].Addresses[0])
	assert.Check(t, err)

	ip212, _, err := net.ParseCIDR(task1.Networks[1].Addresses[0])
	assert.Check(t, err)

	assert.Check(t, is.Equal(subnet1.Contains(ip112), true))
	assert.Check(t, is.Equal(subnet2.Contains(ip212), true))
	assert.Check(t, is.DeepEqual(ip111, ip112))
	assert.Check(t, is.DeepEqual(ip211, ip212))

	// Deallocate task
	err = na1.DeallocateTask(task1)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task1.Networks[0].Addresses), 0))
	assert.Check(t, is.Equal(len(task1.Networks[1].Addresses), 0))

	// Try allocation after free
	err = na1.AllocateTask(task1)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task1.Networks[0].Addresses), 1))
	assert.Check(t, is.Equal(len(task1.Networks[1].Addresses), 1))

	ip111, _, err = net.ParseCIDR(task1.Networks[0].Addresses[0])
	assert.Check(t, err)

	ip211, _, err = net.ParseCIDR(task1.Networks[1].Addresses[0])
	assert.Check(t, err)

	assert.Check(t, is.Equal(subnet1.Contains(ip111), true))
	assert.Check(t, is.Equal(subnet2.Contains(ip211), true))

	err = na1.DeallocateTask(task1)
	assert.Check(t, err)
	assert.Check(t, is.Equal(len(task1.Networks[0].Addresses), 0))
	assert.Check(t, is.Equal(len(task1.Networks[1].Addresses), 0))

	// Try to free endpoints on an already freed task
	err = na1.DeallocateTask(task1)
	assert.Check(t, err)
}

func TestAllocateService(t *testing.T) {
	na := newNetworkAllocator(t)
	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
		},
	}

	s := &api.Service{
		ID: "testID1",
		Spec: api.ServiceSpec{
			Task: api.TaskSpec{
				Networks: []*api.NetworkAttachmentConfig{
					{
						Target: "testID",
					},
				},
			},
			Endpoint: &api.EndpointSpec{
				Ports: []*api.PortConfig{
					{
						Name:       "http",
						TargetPort: 80,
					},
					{
						Name:       "https",
						TargetPort: 443,
					},
				},
			},
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)
	assert.Check(t, n.IPAM.Configs != nil)
	assert.Check(t, is.Equal(len(n.IPAM.Configs), 1))
	assert.Check(t, is.Equal(n.IPAM.Configs[0].Range, ""))
	assert.Check(t, is.Equal(len(n.IPAM.Configs[0].Reserved), 0))

	_, subnet, err := net.ParseCIDR(n.IPAM.Configs[0].Subnet)
	assert.Check(t, err)

	gwip := net.ParseIP(n.IPAM.Configs[0].Gateway)
	assert.Check(t, gwip != nil)

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, is.Len(s.Endpoint.Ports, 0)) // Network allocator is not responsible for allocating ports.

	assert.Check(t, is.Equal(1, len(s.Endpoint.VirtualIPs)))

	assert.Check(t, is.DeepEqual(s.Endpoint.Spec, s.Spec.Endpoint))

	ip, _, err := net.ParseCIDR(s.Endpoint.VirtualIPs[0].Addr)
	assert.Check(t, err)

	assert.Check(t, is.Equal(true, subnet.Contains(ip)))
}

func TestDeallocateServiceAllocateIngressMode(t *testing.T) {
	na := newNetworkAllocator(t)

	n := &api.Network{
		ID: "testNetID1",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			Ingress: true,
		},
	}

	err := na.Allocate(n)
	assert.Check(t, err)

	s := &api.Service{
		ID: "testID1",
		Spec: api.ServiceSpec{
			Endpoint: &api.EndpointSpec{
				Ports: []*api.PortConfig{
					{
						Name:          "some_tcp",
						TargetPort:    1234,
						PublishedPort: 1234,
						PublishMode:   api.PublishModeIngress,
					},
				},
			},
		},
		Endpoint: &api.Endpoint{},
	}

	s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs,
		&api.Endpoint_VirtualIP{NetworkID: n.ID})

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))

	err = na.DeallocateService(s)
	assert.Check(t, err)
	assert.Check(t, is.Len(s.Endpoint.Ports, 0))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 0))
	// Allocate again.
	s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs,
		&api.Endpoint_VirtualIP{NetworkID: n.ID})

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))
}

func TestServiceNetworkUpdate(t *testing.T) {
	na := newNetworkAllocator(t)

	n1 := &api.Network{
		ID: "testID1",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
		},
	}

	n2 := &api.Network{
		ID: "testID2",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test2",
			},
		},
	}

	// Allocate both networks
	err := na.Allocate(n1)
	assert.Check(t, err)

	err = na.Allocate(n2)
	assert.Check(t, err)

	// Attach a network to a service spec and allocate a service
	s := &api.Service{
		ID: "testID1",
		Spec: api.ServiceSpec{
			Task: api.TaskSpec{
				Networks: []*api.NetworkAttachmentConfig{
					{
						Target: "testID1",
					},
				},
			},
			Endpoint: &api.EndpointSpec{
				Mode: api.ResolutionModeVirtualIP,
			},
		},
	}

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, na.IsServiceAllocated(s))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))

	// Now update the same service with another network
	s.Spec.Task.Networks = append(s.Spec.Task.Networks, &api.NetworkAttachmentConfig{Target: "testID2"})

	assert.Check(t, !na.IsServiceAllocated(s))
	err = na.AllocateService(s)
	assert.Check(t, err)

	assert.Check(t, na.IsServiceAllocated(s))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 2))

	s.Spec.Task.Networks = s.Spec.Task.Networks[:1]

	// Check if service needs update and allocate with updated service spec
	assert.Check(t, !na.IsServiceAllocated(s))

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, na.IsServiceAllocated(s))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))

	s.Spec.Task.Networks = s.Spec.Task.Networks[:0]
	// Check if service needs update with all the networks removed and allocate with updated service spec
	assert.Check(t, !na.IsServiceAllocated(s))

	err = na.AllocateService(s)
	assert.Check(t, err)
	assert.Check(t, na.IsServiceAllocated(s))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 0))

	// Attach a network and allocate service
	s.Spec.Task.Networks = append(s.Spec.Task.Networks, &api.NetworkAttachmentConfig{Target: "testID2"})
	assert.Check(t, !na.IsServiceAllocated(s))

	err = na.AllocateService(s)
	assert.Check(t, err)

	assert.Check(t, na.IsServiceAllocated(s))
	assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))
}

type mockIpam struct {
	actualIpamOptions map[string]string
}

func (a *mockIpam) GetDefaultAddressSpaces() (string, string, error) {
	return "defaultAS", "defaultAS", nil
}

func (a *mockIpam) RequestPool(req ipamapi.PoolRequest) (ipamapi.AllocatedPool, error) {
	a.actualIpamOptions = req.Options

	poolCidr := netip.MustParsePrefix(req.Pool)
	return ipamapi.AllocatedPool{
		PoolID: "defaultAS/" + req.Pool,
		Pool:   poolCidr,
	}, nil
}

func (a *mockIpam) ReleasePool(poolID string) error {
	return nil
}

func (a *mockIpam) RequestAddress(poolID string, ip net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	return nil, nil, nil
}

func (a *mockIpam) ReleaseAddress(poolID string, ip net.IP) error {
	return nil
}

func (a *mockIpam) IsBuiltIn() bool {
	return true
}

func TestCorrectlyPassIPAMOptions(t *testing.T) {
	var err error
	expectedIpamOptions := map[string]string{"network-name": "freddie"}

	na := newNetworkAllocator(t)
	ipamDriver := &mockIpam{}

	err = na.(*cnmNetworkAllocator).ipamRegistry.RegisterIpamDriver("mockipam", ipamDriver)
	assert.Check(t, err)

	n := &api.Network{
		ID: "testID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "test",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{
					Name:    "mockipam",
					Options: expectedIpamOptions,
				},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "192.168.1.0/24",
						Gateway: "192.168.1.1",
					},
				},
			},
		},
	}
	err = na.Allocate(n)

	assert.Check(t, is.DeepEqual(expectedIpamOptions, ipamDriver.actualIpamOptions))
	assert.Check(t, err)
}

// fakeDriver is a global-scope network driver whose NetworkAllocate
// fails while err is set.
type fakeDriver struct {
	err error
}

func (d *fakeDriver) NetworkAllocate(string, map[string]string, []driverapi.IPAMData, []driverapi.IPAMData) (map[string]string, error) {
	return nil, d.err
}

func (d *fakeDriver) NetworkFree(string) error {
	return nil
}

func (d *fakeDriver) IsBuiltIn() bool {
	return true
}

const fakeDriverName = "fakedriver"

// driverFailures sets up the fake driver in an allocator so that the
// driver step of Allocate fails. Each returns a function which makes
// the driver step succeed.
var driverFailures = []struct {
	name  string
	setup func(t *testing.T, na *cnmNetworkAllocator) (fix func())
}{
	{
		name: "driver not found",
		setup: func(t *testing.T, na *cnmNetworkAllocator) func() {
			return func() {
				err := na.networkRegistry.RegisterNetworkAllocator(fakeDriverName, &fakeDriver{})
				assert.NilError(t, err)
			}
		},
	},
	{
		name: "driver state",
		setup: func(t *testing.T, na *cnmNetworkAllocator) func() {
			d := &fakeDriver{err: errors.New("injected failure")}
			err := na.networkRegistry.RegisterNetworkAllocator(fakeDriverName, d)
			assert.NilError(t, err)
			return func() { d.err = nil }
		},
	},
}

func newFakeDriverNetwork(id string) *api.Network {
	return &api.Network{
		ID: id,
		Spec: api.NetworkSpec{
			Annotations:  api.Annotations{Name: id},
			DriverConfig: &api.Driver{Name: fakeDriverName},
		},
	}
}

// recordedNetwork returns a network with the state recorded by the
// allocator of a previous leader.
func recordedNetwork(t *testing.T, id string) *api.Network {
	t.Helper()
	na := newNetworkAllocator(t).(*cnmNetworkAllocator)
	err := na.networkRegistry.RegisterNetworkAllocator(fakeDriverName, &fakeDriver{})
	assert.NilError(t, err)
	n := newFakeDriverNetwork(id)
	assert.NilError(t, na.Allocate(n))
	assert.Assert(t, is.Len(n.IPAM.Configs, 1))
	return n.Copy()
}

// newOverlayNetwork returns an overlay network with no recorded state.
// If subnet is not empty, the network spec requests that subnet.
func newOverlayNetwork(id, subnet string) *api.Network {
	n := &api.Network{
		ID: id,
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{Name: id},
		},
	}
	if subnet != "" {
		n.Spec.IPAM = &api.IPAMOptions{
			Configs: []*api.IPAMConfig{{Subnet: subnet}},
		}
	}
	return n
}

func TestAllocateRestoreKeepsPools(t *testing.T) {
	for _, tc := range driverFailures {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			fix := tc.setup(t, na)

			n := recordedNetwork(t, "recorded")
			recorded := n.IPAM.Configs[0].Copy()

			assert.Check(t, is.ErrorContains(na.Allocate(n), ""))
			assert.Check(t, !na.IsAllocated(n))

			// The recorded subnet stays reserved while the driver
			// step fails.
			other := newOverlayNetwork("other", "")
			assert.NilError(t, na.Allocate(other))
			assert.Check(t, other.IPAM.Configs[0].Subnet != recorded.Subnet)
			assert.Check(t, is.ErrorContains(na.Allocate(newOverlayNetwork("clash", recorded.Subnet)), ""))

			// A retry of the driver step allocates the network with
			// its recorded subnet.
			fix()
			assert.NilError(t, na.Allocate(n))
			assert.Check(t, na.IsAllocated(n))
			assert.Check(t, is.DeepEqual(n.IPAM.Configs, []*api.IPAMConfig{recorded}))

			// The retry reused the reserved pools, so one Deallocate
			// frees the subnet.
			assert.NilError(t, na.Deallocate(n))
			assert.NilError(t, na.Allocate(newOverlayNetwork("reuse", recorded.Subnet)))
		})
	}
}

func TestDeallocatePendingNetwork(t *testing.T) {
	for _, tc := range driverFailures {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			tc.setup(t, na)

			n := recordedNetwork(t, "recorded")
			subnet := n.IPAM.Configs[0].Subnet
			assert.Check(t, is.ErrorContains(na.Allocate(n), ""))

			assert.NilError(t, na.Deallocate(n))
			assert.Check(t, !na.IsAllocated(n))
			assert.NilError(t, na.Allocate(newOverlayNetwork("reuse", subnet)))
		})
	}
}

func TestAllocateRestoreReportsDriverError(t *testing.T) {
	na := newNetworkAllocator(t).(*cnmNetworkAllocator)
	n := recordedNetwork(t, "recorded")
	_, driverErr := na.resolveDriver(n)
	assert.Assert(t, driverErr != nil)

	// Another network holds the recorded subnet.
	assert.NilError(t, na.Allocate(newOverlayNetwork("other", n.IPAM.Configs[0].Subnet)))

	err := na.Allocate(n)
	assert.Check(t, is.ErrorContains(err, driverErr.Error()))
	assert.Check(t, is.ErrorContains(err, "Pool overlaps"))
}

func TestAllocateNewNetworkFailureReleasesPools(t *testing.T) {
	const subnet = "10.20.0.0/24"
	for _, tc := range driverFailures {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			tc.setup(t, na)

			n := newFakeDriverNetwork("new")
			n.Spec.IPAM = &api.IPAMOptions{
				Configs: []*api.IPAMConfig{{Subnet: subnet}},
			}
			// The allocator retries with the object which the failed
			// attempt updated.
			for range 2 {
				assert.Check(t, is.ErrorContains(na.Allocate(n), ""))
				assert.Check(t, !na.IsAllocated(n))

				probe := newOverlayNetwork("probe", subnet)
				assert.NilError(t, na.Allocate(probe))
				assert.NilError(t, na.Deallocate(probe))
			}
		})
	}
}

func TestAllocateRestoreRetryWithOtherCopy(t *testing.T) {
	for _, tc := range driverFailures {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			fix := tc.setup(t, na)

			// A record with no gateway is given one by the first
			// attempt.
			record := recordedNetwork(t, "recorded")
			record.IPAM.Configs[0].Gateway = ""
			first := record.Copy()
			assert.Check(t, is.ErrorContains(na.Allocate(first), ""))
			assert.Assert(t, first.IPAM.Configs[0].Gateway != "")

			// The retry uses the IPAM state of the reserved pools.
			fix()
			retry := record.Copy()
			assert.NilError(t, na.Allocate(retry))
			assert.Check(t, is.DeepEqual(retry.IPAM, first.IPAM))

			assert.NilError(t, na.Deallocate(retry))
			assert.NilError(t, na.Allocate(newOverlayNetwork("reuse", record.IPAM.Configs[0].Subnet)))
		})
	}
}

func TestAllocateRestoreNodeLocal(t *testing.T) {
	// The state recorded for a node-local network has no IPAM configs.
	recorded := func(driver string) *api.Network {
		n := newOverlayNetwork("local", "")
		n.Spec.DriverConfig = &api.Driver{Name: driver}
		n.DriverState = &api.Driver{Name: driver}
		n.IPAM = &api.IPAMOptions{Driver: &api.Driver{}}
		return n
	}
	na := newNetworkAllocator(t).(*cnmNetworkAllocator)

	// No pools are reserved for a network whose driver is not found.
	n := recorded(fakeDriverName)
	assert.Check(t, is.ErrorContains(na.Allocate(n), ""))
	assert.Check(t, is.Len(na.pendingNetworks, 0))

	n = recorded(localDrivers[0])
	assert.NilError(t, na.Allocate(n))
	assert.Check(t, is.Len(na.networks[n.ID].pools, 0))
	assert.Check(t, is.Len(n.IPAM.Configs, 0))
	assert.NilError(t, na.Deallocate(n))

	// The pools recorded for a network whose driver is now node-local
	// are released.
	const subnet = "10.20.0.0/24"
	n = recorded(localDrivers[0])
	n.IPAM.Configs = []*api.IPAMConfig{{Subnet: subnet, Gateway: "10.20.0.1"}}
	assert.NilError(t, na.Allocate(n))
	assert.Check(t, is.Len(na.pendingNetworks, 0))
	assert.Check(t, is.Len(na.networks[n.ID].pools, 0))
	assert.NilError(t, na.Allocate(newOverlayNetwork("reuse", subnet)))
}

// badGatewayIpam returns a gateway which does not parse, and records
// the pools it holds.
type badGatewayIpam struct {
	mockIpam
	pools map[string]struct{}
}

func (a *badGatewayIpam) RequestPool(req ipamapi.PoolRequest) (ipamapi.AllocatedPool, error) {
	alloc, err := a.mockIpam.RequestPool(req)
	alloc.Meta = map[string]string{netlabel.Gateway: "bogus"}
	a.pools[alloc.PoolID] = struct{}{}
	return alloc, err
}

func (a *badGatewayIpam) ReleasePool(poolID string) error {
	delete(a.pools, poolID)
	return nil
}

func TestAllocateBadGatewayReleasesPools(t *testing.T) {
	na := newNetworkAllocator(t).(*cnmNetworkAllocator)
	ipam := &badGatewayIpam{pools: make(map[string]struct{})}
	assert.NilError(t, na.ipamRegistry.RegisterIpamDriver("badgateway", ipam))

	n := newOverlayNetwork("badgateway", "192.168.1.0/24")
	n.Spec.IPAM.Driver = &api.Driver{Name: "badgateway"}
	assert.Check(t, is.ErrorContains(na.Allocate(n), "failed to parse gateway address"))
	assert.Check(t, is.Len(ipam.pools, 0))
}

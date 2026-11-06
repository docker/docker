package cnmallocator

import (
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
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

// TestDeallocateTaskAfterNetworkRemoval covers the sequence produced by
// `docker stack rm`: the overlay network of a stack is removed as soon as its
// tasks are marked for removal, i.e. before they have actually terminated.
// When such a task finally terminates, the allocator must still recognise it
// as allocated (otherwise the swarmkit allocator never calls DeallocateTask)
// and DeallocateTask must release the addresses the task holds on its
// remaining networks, notably the ingress network, which outlives every stack.
func TestDeallocateTaskAfterNetworkRemoval(t *testing.T) {
	na := newNetworkAllocator(t)

	ingress := &api.Network{
		ID: "ingressID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "ingress",
			},
			Ingress: true,
			IPAM: &api.IPAMOptions{
				Driver: &api.Driver{},
				Configs: []*api.IPAMConfig{
					{
						Subnet:  "10.0.0.0/29",
						Gateway: "10.0.0.1",
					},
				},
			},
		},
	}
	overlay := &api.Network{
		ID: "overlayID",
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: "overlay",
			},
		},
	}
	assert.NilError(t, na.Allocate(ingress))
	assert.NilError(t, na.Allocate(overlay))

	task := &api.Task{
		ID: "taskID",
		Networks: []*api.NetworkAttachment{
			{Network: ingress},
			{Network: overlay},
		},
	}
	assert.NilError(t, na.AllocateTask(task))
	assert.Check(t, na.IsTaskAllocated(task))
	assert.Check(t, is.Len(task.Networks[0].Addresses, 1))
	assert.Check(t, is.Len(task.Networks[1].Addresses, 1))
	ingressAddr := task.Networks[0].Addresses[0]

	// The overlay network is removed while the task is still shutting down.
	assert.NilError(t, na.Deallocate(overlay))

	// The task still holds an address on the ingress network, so it is
	// still allocated, and deallocating it must release that address and
	// clear the attachment to the removed network as well.
	assert.Check(t, na.IsTaskAllocated(task))
	assert.NilError(t, na.DeallocateTask(task))
	assert.Check(t, is.Len(task.Networks[0].Addresses, 0))
	assert.Check(t, is.Len(task.Networks[1].Addresses, 0))
	assert.Check(t, !na.IsTaskAllocated(task))

	// The ingress address must be available again.
	task2 := &api.Task{
		ID: "taskID2",
		Networks: []*api.NetworkAttachment{
			{Network: ingress, Addresses: []string{ingressAddr}},
		},
	}
	assert.NilError(t, na.AllocateTask(task2))
	assert.Check(t, is.Equal(task2.Networks[0].Addresses[0], ingressAddr))
}

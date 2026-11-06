package cnmallocator

import (
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
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

// strictReleaseIpam wraps an IPAM driver to record each release of an
// address which it did not give out, or which has been released already.
type strictReleaseIpam struct {
	ipamapi.Ipam
	held map[string]map[string]struct{} // pool ID -> address
	// strays lists the addresses released without being held.
	strays []string
}

func (a *strictReleaseIpam) RequestAddress(poolID string, ip net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	addr, data, err := a.Ipam.RequestAddress(poolID, ip, opts)
	if err == nil {
		if a.held[poolID] == nil {
			a.held[poolID] = map[string]struct{}{}
		}
		a.held[poolID][addr.IP.String()] = struct{}{}
	}
	return addr, data, err
}

func (a *strictReleaseIpam) ReleaseAddress(poolID string, ip net.IP) error {
	if _, ok := a.held[poolID][ip.String()]; !ok {
		a.strays = append(a.strays, fmt.Sprintf("%s in pool %q", ip, poolID))
	}
	delete(a.held[poolID], ip.String())
	return a.Ipam.ReleaseAddress(poolID, ip)
}

func (a *strictReleaseIpam) ReleasePool(poolID string) error {
	delete(a.held, poolID)
	return a.Ipam.ReleasePool(poolID)
}

// allocateSmallNetworks allocates a network with a /29 subnet, which has 5
// addresses to give out, for each of ids. The network with the ID "ingress"
// is the ingress network. The networks use the default IPAM driver, wrapped
// in the strictReleaseIpam it returns.
func allocateSmallNetworks(t *testing.T, na *cnmNetworkAllocator, ids ...string) (map[string]*api.Network, *strictReleaseIpam) {
	t.Helper()
	const ipamName = "strict-release"
	ipam, _ := na.ipamRegistry.IPAM(defaultipam.DriverName)
	assert.Assert(t, ipam != nil)
	strict := &strictReleaseIpam{Ipam: ipam, held: map[string]map[string]struct{}{}}
	assert.NilError(t, na.ipamRegistry.RegisterIpamDriver(ipamName, strict))

	networks := make(map[string]*api.Network, len(ids))
	for i, id := range ids {
		n := &api.Network{
			ID: id,
			Spec: api.NetworkSpec{
				Annotations:  api.Annotations{Name: id},
				DriverConfig: &api.Driver{},
				Ingress:      id == "ingress",
				IPAM: &api.IPAMOptions{
					Driver:  &api.Driver{Name: ipamName},
					Configs: []*api.IPAMConfig{{Subnet: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 9, byte(i), 0}), 29).String()}},
				},
			},
		}
		assert.NilError(t, na.Allocate(n))
		networks[id] = n
	}
	return networks, strict
}

func networkAttachments(ids ...string) []*api.NetworkAttachmentConfig {
	var nAttach []*api.NetworkAttachmentConfig
	for _, id := range ids {
		nAttach = append(nAttach, &api.NetworkAttachmentConfig{Target: id})
	}
	return nAttach
}

// TestAllocateServiceFailureReleasesVIPs checks that AllocateService releases
// every VIP of the service when it fails to allocate a VIP on one of the
// networks of the service, both those the service had and those the call
// allocated, and that it releases each address it holds once and no other.
func TestAllocateServiceFailureReleasesVIPs(t *testing.T) {
	// A VIP leaked on each attempt exhausts a network.
	const attempts = 6
	networkIDs := []string{"ov1", "ov2", "ov3", "ov4"}

	tests := []struct {
		name string
		// had lists the networks which the service has VIPs on before
		// the call which fails.
		had []string
		// freed lists the networks which are deallocated after the
		// service has VIPs on them, and allocated again after the call
		// which fails.
		freed []string
		// onGateway names a network which the service also has a VIP
		// on, at the gateway address of the network.
		onGateway string
		// published sets the service to publish a port on the ingress
		// network before the call which fails, which gives the service
		// a VIP with no address on the ingress network, as swarmkit
		// does.
		published bool
		// spec lists the networks of the service in the call which
		// fails. "unallocated" is a network which is not allocated.
		spec []string
		// err is the error of the call which fails, if it is not
		// "could not find local network state".
		err string
	}{
		{
			name: "new service",
			spec: []string{"ov1", "unallocated"},
		},
		{
			name: "unallocated network first",
			spec: []string{"unallocated", "ov1"},
		},
		{
			name: "networks added",
			had:  []string{"ov1"},
			spec: []string{"ov1", "ov2", "unallocated"},
		},
		{
			name: "networks added and removed",
			had:  []string{"ov3", "ov1"},
			spec: []string{"ov1", "ov2", "ov4", "unallocated"},
		},
		{
			name: "network removed",
			had:  []string{"ov2", "ov1"},
			spec: []string{"ov1", "unallocated"},
		},
		{
			// The call fails on the VIP on ov2, before it looks at the
			// VIP on ov3.
			name:  "network of a VIP freed",
			had:   []string{"ov1", "ov2", "ov3"},
			freed: []string{"ov2"},
			spec:  []string{"ov1", "ov2", "ov3", "ov4"},
		},
		{
			// IPAM holds the gateway address, so the call fails on
			// the VIP on ov2.
			name:      "VIP on a gateway address",
			had:       []string{"ov1"},
			onGateway: "ov2",
			spec:      []string{"ov1", "ov2", "ov3"},
			err:       "could not allocate VIP from IPAM",
		},
		{
			name:      "port published",
			had:       []string{"ov1"},
			published: true,
			spec:      []string{"ov1", "ov2", "unallocated"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			networks, ipam := allocateSmallNetworks(t, na, append(networkIDs, "ingress")...)

			for range attempts {
				s := &api.Service{ID: "svc1"}
				if len(tc.had) > 0 {
					s.Spec.Task.Networks = networkAttachments(tc.had...)
					assert.NilError(t, na.AllocateService(s))
					assert.Assert(t, is.Len(s.Endpoint.VirtualIPs, len(tc.had)))
				}
				for _, id := range tc.freed {
					assert.NilError(t, na.Deallocate(networks[id]))
				}
				if s.Endpoint == nil {
					s.Endpoint = &api.Endpoint{}
				}
				if tc.onGateway != "" {
					s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs, &api.Endpoint_VirtualIP{
						NetworkID: tc.onGateway,
						Addr:      networks[tc.onGateway].IPAM.Configs[0].Gateway + "/29",
					})
				}
				if tc.published {
					s.Spec.Endpoint = &api.EndpointSpec{
						Ports: []*api.PortConfig{{TargetPort: 80, PublishMode: api.PublishModeIngress}},
					}
					s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs, &api.Endpoint_VirtualIP{NetworkID: "ingress"})
				}

				s.Spec.Task.Networks = networkAttachments(tc.spec...)
				expErr := tc.err
				if expErr == "" {
					expErr = "could not find local network state"
				}
				assert.ErrorContains(t, na.AllocateService(s), expErr)
				assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 0))
				assert.Check(t, !na.IsServiceAllocated(s))
				assert.Assert(t, is.Len(ipam.strays, 0), "released addresses which were not held")
				for id := range networks {
					if nw := na.networks[id]; nw != nil {
						assert.Assert(t, is.Len(nw.endpoints, 0), "network %s", id)
					}
				}

				for _, id := range tc.freed {
					assert.NilError(t, na.Allocate(networks[id]))
				}
			}

			s := &api.Service{ID: "svc2"}
			s.Spec.Task.Networks = networkAttachments(networkIDs...)
			assert.NilError(t, na.AllocateService(s))
			assert.Check(t, is.Len(s.Endpoint.VirtualIPs, len(networkIDs)))
		})
	}
}

// TestAllocateServiceDuplicateNetwork checks that AllocateService allocates
// one VIP on a network which the spec of the service lists twice.
func TestAllocateServiceDuplicateNetwork(t *testing.T) {
	tests := []struct {
		name string
		// had lists the networks which the service has VIPs on before
		// its spec lists ov1 twice.
		had []string
	}{
		{name: "new service"},
		{name: "network replaced", had: []string{"ov2"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			na := newNetworkAllocator(t).(*cnmNetworkAllocator)
			allocateSmallNetworks(t, na, "ov1", "ov2")

			s := &api.Service{ID: "svc1"}
			if len(tc.had) > 0 {
				s.Spec.Task.Networks = networkAttachments(tc.had...)
				assert.NilError(t, na.AllocateService(s))
			}

			s.Spec.Task.Networks = networkAttachments("ov1", "ov1")
			assert.NilError(t, na.AllocateService(s))
			assert.Check(t, is.Len(s.Endpoint.VirtualIPs, 1))
			assert.Check(t, is.Len(na.networks["ov1"].endpoints, 1))
			assert.Check(t, is.Len(na.networks["ov2"].endpoints, 0))
			assert.Check(t, na.IsServiceAllocated(s))
		})
	}
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

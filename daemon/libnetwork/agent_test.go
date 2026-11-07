package libnetwork

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"github.com/gogo/protobuf/proto"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/moby/moby/v2/daemon/libnetwork/cluster"
	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/networkdb"
)

func TestEndpointEvent_EquivalentTo(t *testing.T) {
	assert.Check(t, (&endpointEvent{}).EquivalentTo(&endpointEvent{}))

	a := endpointEvent{
		EndpointRecord: EndpointRecord{
			Name:        "foo",
			ServiceName: "bar",
			ServiceID:   "baz",
			IngressPorts: []*PortConfig{
				{
					Protocol:   ProtocolTCP,
					TargetPort: 80,
				},
				{
					Name:          "dns",
					Protocol:      ProtocolUDP,
					TargetPort:    5353,
					PublishedPort: 53,
				},
			},
		},
		VirtualIP:  netip.MustParseAddr("10.0.0.42"),
		EndpointIP: netip.MustParseAddr("192.168.69.42"),
	}
	assert.Check(t, a.EquivalentTo(&a))

	reflexiveEquiv := func(a, b *endpointEvent) bool {
		t.Helper()
		assert.Check(t, a.EquivalentTo(b) == b.EquivalentTo(a), "reflexive equivalence")
		return a.EquivalentTo(b)
	}

	assert.Check(t, reflexiveEquiv(nil, nil), "nil should be equivalent to nil")
	assert.Check(t, !reflexiveEquiv(&a, nil), "non-nil should not be equivalent to nil")

	b := a
	b.ServiceDisabled = true
	assert.Check(t, reflexiveEquiv(&a, &b), "ServiceDisabled value should not matter")

	c := a
	c.IngressPorts = slices.Clone(a.IngressPorts)
	slices.Reverse(c.IngressPorts)
	assert.Check(t, reflexiveEquiv(&a, &c), "IngressPorts order should not matter")

	d := a
	d.IngressPorts = append(d.IngressPorts, a.IngressPorts[0])
	assert.Check(t, !reflexiveEquiv(&a, &d), "Differing number of copies of IngressPort entries should not be equivalent")
	d.IngressPorts = a.IngressPorts[:1]
	assert.Check(t, !reflexiveEquiv(&a, &d), "Removing an IngressPort entry should not be equivalent")

	e := a
	e.Aliases = []string{"alias1", "alias2"}
	assert.Check(t, !reflexiveEquiv(&a, &e), "Differing Aliases should not be equivalent")

	f := a
	f.TaskAliases = []string{"taskalias1", "taskalias2"}
	assert.Check(t, !reflexiveEquiv(&a, &f), "Adding TaskAliases should not be equivalent")
	g := a
	g.TaskAliases = []string{"taskalias2", "taskalias1"}
	assert.Check(t, reflexiveEquiv(&f, &g), "TaskAliases order should not matter")
	g.TaskAliases = g.TaskAliases[:1]
	assert.Check(t, !reflexiveEquiv(&f, &g), "Differing number of TaskAliases should not be equivalent")

	h := a
	h.EndpointIP = netip.MustParseAddr("192.168.69.43")
	assert.Check(t, !reflexiveEquiv(&a, &h), "Differing EndpointIP should not be equivalent")

	i := a
	i.VirtualIP = netip.MustParseAddr("10.0.0.69")
	assert.Check(t, !reflexiveEquiv(&a, &i), "Differing VirtualIP should not be equivalent")

	j := a
	j.ServiceID = "qux"
	assert.Check(t, !reflexiveEquiv(&a, &j), "Differing ServiceID should not be equivalent")

	k := a
	k.ServiceName = "quux"
	assert.Check(t, !reflexiveEquiv(&a, &k), "Differing ServiceName should not be equivalent")

	l := a
	l.Name = "aaaaa"
	assert.Check(t, !reflexiveEquiv(&a, &l), "Differing Name should not be equivalent")
}

type mockServiceBinder struct {
	actions []string
}

func (m *mockServiceBinder) addContainerNameResolution(nID, eID, containerName string, _ []string, ip net.IP, _ string) error {
	m.actions = append(m.actions, fmt.Sprintf("addContainerNameResolution(%v, %v, %v, %v)", nID, eID, containerName, ip))
	return nil
}

func (m *mockServiceBinder) delContainerNameResolution(nID, eID, containerName string, _ []string, ip net.IP, _ string) error {
	m.actions = append(m.actions, fmt.Sprintf("delContainerNameResolution(%v, %v, %v, %v)", nID, eID, containerName, ip))
	return nil
}

func (m *mockServiceBinder) addServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, _ []*PortConfig, _, _ []string, ip net.IP, _ string) error {
	m.actions = append(m.actions, fmt.Sprintf("addServiceBinding(%v, %v, %v, %v, %v, %v, %v)", svcName, svcID, nID, eID, containerName, vip, ip))
	return nil
}

func (m *mockServiceBinder) rmServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, _ []*PortConfig, _, _ []string, ip net.IP, _ string, deleteSvcRecords bool, fullRemove bool) error {
	m.actions = append(m.actions, fmt.Sprintf("rmServiceBinding(%v, %v, %v, %v, %v, %v, %v, deleteSvcRecords=%v, fullRemove=%v)", svcName, svcID, nID, eID, containerName, vip, ip, deleteSvcRecords, fullRemove))
	return nil
}

func TestHandleEPTableEvent(t *testing.T) {
	svc1 := EndpointRecord{
		Name:        "ep1",
		ServiceName: "svc1",
		ServiceID:   "id1",
		VirtualIP:   "10.0.0.1",
		EndpointIP:  "192.168.12.42",
	}
	svc1disabled := svc1
	svc1disabled.ServiceDisabled = true

	svc2 := EndpointRecord{
		Name:        "ep2",
		ServiceName: "svc2",
		ServiceID:   "id2",
		VirtualIP:   "10.0.0.2",
		EndpointIP:  "172.16.69.5",
	}
	svc2disabled := svc2
	svc2disabled.ServiceDisabled = true

	ctr1 := EndpointRecord{
		Name:       "ctr1",
		EndpointIP: "172.18.1.1",
	}
	ctr1disabled := ctr1
	ctr1disabled.ServiceDisabled = true

	ctr2 := EndpointRecord{
		Name:       "ctr2",
		EndpointIP: "172.18.1.2",
	}
	ctr2disabled := ctr2
	ctr2disabled.ServiceDisabled = true

	tests := []struct {
		name            string
		prev, ev        *EndpointRecord
		expectedActions []string
	}{
		{
			name: "Insert/Service/ServiceDisabled=false",
			ev:   &svc1,
			expectedActions: []string{
				"addServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42)",
			},
		},
		{
			name: "Insert/Service/ServiceDisabled=true",
			ev:   &svc1disabled,
		},
		{
			name: "Insert/Container/ServiceDisabled=false",
			ev:   &ctr1,
			expectedActions: []string{
				"addContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Insert/Container/ServiceDisabled=true",
			ev:   &ctr1disabled,
		},

		{
			name: "Update/Service/ServiceDisabled=ft",
			prev: &svc1,
			ev:   &svc1disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=false)",
			},
		},
		{
			name: "Update/Service/ServiceDisabled=tf",
			prev: &svc1disabled,
			ev:   &svc1,
			expectedActions: []string{
				"addServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42)",
			},
		},
		{
			name: "Update/Service/ServiceDisabled=ff",
			prev: &svc1disabled,
			ev:   &svc1disabled,
		},
		{
			name: "Update/Service/ServiceDisabled=tt",
			prev: &svc1,
			ev:   &svc1,
		},
		{
			name: "Update/Container/ServiceDisabled=ft",
			prev: &ctr1,
			ev:   &ctr1disabled,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Update/Container/ServiceDisabled=tf",
			prev: &ctr1disabled,
			ev:   &ctr1,
			expectedActions: []string{
				"addContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Update/Container/ServiceDisabled=ff",
			prev: &ctr1disabled,
			ev:   &ctr1disabled,
		},
		{
			name: "Update/Container/ServiceDisabled=tt",
			prev: &ctr1,
			ev:   &ctr1,
		},

		{
			name: "Delete/Service/ServiceDisabled=false",
			prev: &svc1,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},
		{
			name: "Delete/Service/ServiceDisabled=true",
			prev: &svc1disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},

		{
			name: "Delete/Container/ServiceDisabled=false",
			prev: &ctr1,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Delete/Container/ServiceDisabled=true",
			prev: &ctr1disabled,
		},

		{
			name: "Replace/From=Service/To=Service/ServiceDisabled=ff",
			prev: &svc1,
			ev:   &svc2,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
				"addServiceBinding(svc2, id2, network1, endpoint1, ep2, 10.0.0.2, 172.16.69.5)",
			},
		},
		{
			name: "Replace/From=Service/To=Service/ServiceDisabled=ft",
			prev: &svc1,
			ev:   &svc2disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},
		{
			name: "Replace/From=Service/To=Service/ServiceDisabled=tf",
			prev: &svc1disabled,
			ev:   &svc2,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
				"addServiceBinding(svc2, id2, network1, endpoint1, ep2, 10.0.0.2, 172.16.69.5)",
			},
		},
		{
			name: "Replace/From=Service/To=Service/ServiceDisabled=tt",
			prev: &svc1disabled,
			ev:   &svc2disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},
		{
			name: "Replace/From=Service/To=Container/ServiceDisabled=ff",
			prev: &svc1,
			ev:   &ctr2,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
				"addContainerNameResolution(network1, endpoint1, ctr2, 172.18.1.2)",
			},
		},
		{
			name: "Replace/From=Service/To=Container/ServiceDisabled=ft",
			prev: &svc1,
			ev:   &ctr2disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},
		{
			name: "Replace/From=Service/To=Container/ServiceDisabled=tf",
			prev: &svc1disabled,
			ev:   &ctr2,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
				"addContainerNameResolution(network1, endpoint1, ctr2, 172.18.1.2)",
			},
		},
		{
			name: "Replace/From=Service/To=Container/ServiceDisabled=tt",
			prev: &svc1disabled,
			ev:   &ctr2disabled,
			expectedActions: []string{
				"rmServiceBinding(svc1, id1, network1, endpoint1, ep1, 10.0.0.1, 192.168.12.42, deleteSvcRecords=true, fullRemove=true)",
			},
		},
		{
			name: "Replace/From=Container/To=Service/ServiceDisabled=ff",
			prev: &ctr1,
			ev:   &svc2,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
				"addServiceBinding(svc2, id2, network1, endpoint1, ep2, 10.0.0.2, 172.16.69.5)",
			},
		},
		{
			name: "Replace/From=Container/To=Service/ServiceDisabled=ft",
			prev: &ctr1,
			ev:   &svc2disabled,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Replace/From=Container/To=Service/ServiceDisabled=tf",
			prev: &ctr1disabled,
			ev:   &svc2,
			expectedActions: []string{
				"addServiceBinding(svc2, id2, network1, endpoint1, ep2, 10.0.0.2, 172.16.69.5)",
			},
		},
		{
			name: "Replace/From=Container/To=Service/ServiceDisabled=tt",
			prev: &ctr1disabled,
			ev:   &svc2disabled,
		},
		{
			name: "Replace/From=Container/To=Container/ServiceDisabled=ff",
			prev: &ctr1,
			ev:   &ctr2,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
				"addContainerNameResolution(network1, endpoint1, ctr2, 172.18.1.2)",
			},
		},
		{
			name: "Replace/From=Container/To=Container/ServiceDisabled=ft",
			prev: &ctr1,
			ev:   &ctr2disabled,
			expectedActions: []string{
				"delContainerNameResolution(network1, endpoint1, ctr1, 172.18.1.1)",
			},
		},
		{
			name: "Replace/From=Container/To=Container/ServiceDisabled=tf",
			prev: &ctr1disabled,
			ev:   &ctr2,
			expectedActions: []string{
				"addContainerNameResolution(network1, endpoint1, ctr2, 172.18.1.2)",
			},
		},
		{
			name: "Replace/From=Container/To=Container/ServiceDisabled=tt",
			prev: &ctr1disabled,
			ev:   &ctr2disabled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msb := &mockServiceBinder{}
			event := networkdb.WatchEvent{
				NetworkID: "network1",
				Key:       "endpoint1",
			}
			var err error
			if tt.prev != nil {
				event.Prev, err = proto.Marshal(tt.prev)
				assert.NilError(t, err)
			}
			if tt.ev != nil {
				event.Value, err = proto.Marshal(tt.ev)
				assert.NilError(t, err)
			}
			handleEpTableEvent(msb, event)
			assert.DeepEqual(t, tt.expectedActions, msb.actions)
		})
	}
}

// clusterProviderStub answers GetRemoteAddressList and nothing else: every
// other method of the interface is left to the nil embedded value, so a test
// which reaches one panics rather than quietly passing.
type clusterProviderStub struct {
	cluster.Provider
	mu      sync.Mutex
	remotes []string
	calls   int
}

func (p *clusterProviderStub) GetRemoteAddressList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return slices.Clone(p.remotes)
}

func TestRemoteAddrs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported []string
		want     []string
	}{
		{name: "none", reported: nil, want: []string{}},
		{name: "host and port", reported: []string{"10.0.0.1:2377"}, want: []string{"10.0.0.1"}},
		// The daemon's own providers report these, and SplitHostPort leaves
		// the empty string behind for them, which is not an address at all.
		{name: "no port", reported: []string{"10.0.0.1"}, want: []string{"10.0.0.1"}},
		{name: "ipv6", reported: []string{"[fd00::1]:2377"}, want: []string{"fd00::1"}},
		{name: "mixed", reported: []string{"10.0.0.1:2377", "10.0.0.2"}, want: []string{"10.0.0.1", "10.0.0.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Check(t, is.DeepEqual(remoteAddrs(&clusterProviderStub{remotes: tc.reported}), tc.want))
		})
	}
}

// TestRemoteAddrListIsLastKnownGood pins the half of the bootstrap-peer
// contract which is libnetwork's: the list is asked for afresh on every call
// and passed on as the provider reported it, with nothing cached, dropped or
// second-guessed here. A node which has lost the Swarm control plane therefore
// keeps bootstrapping off the managers it last saw, which is what lets gossip
// carry on without one.
//
// Swarmkit holds up the other half: its manager list lives in memory and on
// disk, a failed connection only re-weights an entry, and the one path which
// removes one runs off a dispatcher session message -- so an outage freezes
// the list rather than draining it.
func TestRemoteAddrListIsLastKnownGood(t *testing.T) {
	cp := &clusterProviderStub{remotes: []string{"10.0.0.1:2377", "10.0.0.2:2377"}}
	c := &Controller{cfg: &config.Config{ClusterProvider: cp}}

	want := []string{"10.0.0.1", "10.0.0.2"}
	for i := range 3 {
		assert.Check(t, is.DeepEqual(c.remoteAddrList(), want), "call %d", i)
	}
	assert.Check(t, is.Equal(cp.calls, 3), "the peers must be asked for every time, not snapshotted")

	// Once the node leaves the cluster the provider goes with it, and NetworkDB
	// is told there is nothing to bootstrap from rather than being handed a
	// list which can no longer be answered for.
	c.cfg.ClusterProvider = nil
	assert.Check(t, is.Nil(c.remoteAddrList()))
}

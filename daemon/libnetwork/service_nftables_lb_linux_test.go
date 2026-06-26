//go:build linux

package libnetwork

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/internal/iterutil"

	"github.com/google/go-cmp/cmp/cmpopts"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func ingressPort(published, target uint32) *PortConfig {
	return &PortConfig{Protocol: ProtocolTCP, PublishedPort: published, TargetPort: target}
}

// addrMap renders a map of []net.IP as a map of []string, with the IPs sorted.
// composeNFTLB promises no particular order - which backend lands in which
// numgen bucket is immaterial - so the ordering is imposed here rather than by
// the code under test. Strings also make a failure report addresses rather than
// byte slices.
func addrMap[K comparable](m map[K][]net.IP) map[K][]string {
	return maps.Collect(iterutil.Map2(maps.All(m), func(k K, v []net.IP) (K, []string) {
		return k, slices.Sorted(iterutil.Map(slices.Values(v), net.IP.String))
	}))
}

// TestComposeNFTLB covers how the contributions of coexisting loadBalancers are
// combined into the backend set that each VIP and each published port gets
// partitioned over.
func TestComposeNFTLB(t *testing.T) {
	vip := net.IPv4(10, 0, 0, 5)
	// Two generations of one service, as a published-port change rolls out. The old
	// tasks serve only 8080, the new ones 8080 and 9090, and both share the VIP.
	genA := serviceKey{id: "svc", ports: "8080:80/TCP"}
	genB := serviceKey{id: "svc", ports: "8080:80/TCP,9090:90/TCP"}

	tests := []struct {
		name      string
		contrib   map[serviceKey]nftLBContribution
		wantVIPs  map[string][]string
		wantPorts map[portKey][]string
	}{
		{
			// This is the reason the table has a single writer: a port that both
			// generations publish has to be partitioned once, over every backend
			// serving it. Composed independently they would emit overlapping
			// intervals for it, which nftables rejects.
			name: "shared port",
			contrib: map[serviceKey]nftLBContribution{
				genA: {
					vip:      vip,
					backends: []net.IP{net.IPv4(10, 0, 1, 3), net.IPv4(10, 0, 1, 4)},
					ports:    []*PortConfig{ingressPort(8080, 80)},
				},
				genB: {
					vip:      vip,
					backends: []net.IP{net.IPv4(10, 0, 1, 9)},
					ports:    []*PortConfig{ingressPort(8080, 80), ingressPort(9090, 90)},
				},
			},
			wantVIPs: map[string][]string{vip.String(): {"10.0.1.3", "10.0.1.4", "10.0.1.9"}},
			// 9090 reaches only the generation that publishes it. Routing it to an
			// old task would be a connection refused.
			wantPorts: map[portKey][]string{
				{proto: ProtocolTCP, port: 8080}: {"10.0.1.3", "10.0.1.4", "10.0.1.9"},
				{proto: ProtocolTCP, port: 9090}: {"10.0.1.9"},
			},
		},
		{
			// The terminal state of that rolling update: the old generation has no
			// backends left, so the caller has dropped its contribution, and the
			// survivor keeps the shared port to itself.
			name: "drained generation",
			contrib: map[serviceKey]nftLBContribution{
				genB: {
					vip:      vip,
					backends: []net.IP{net.IPv4(10, 0, 1, 9)},
					ports:    []*PortConfig{ingressPort(8080, 80), ingressPort(9090, 90)},
				},
			},
			wantVIPs: map[string][]string{vip.String(): {"10.0.1.9"}},
			wantPorts: map[portKey][]string{
				{proto: ProtocolTCP, port: 8080}: {"10.0.1.9"},
				{proto: ProtocolTCP, port: 9090}: {"10.0.1.9"},
			},
		},
		{
			// Every backend deweighted. The VIP and the port stay claimed - which is
			// what keeps the alias in place while the service exists - with nothing
			// to route to.
			name: "all backends deweighted",
			contrib: map[serviceKey]nftLBContribution{
				genA: {
					vip:   vip,
					ports: []*PortConfig{ingressPort(8080, 80)},
				},
			},
			wantVIPs:  map[string][]string{vip.String(): {}},
			wantPorts: map[portKey][]string{{proto: ProtocolTCP, port: 8080}: {}},
		},
		{
			// Distinct services share the load-balancer sandbox, and so its tables,
			// so they must keep separate VIPs and separate published ports.
			name: "distinct services",
			contrib: map[serviceKey]nftLBContribution{
				{id: "svc-a", ports: "8080:80/TCP"}: {
					vip:      net.IPv4(10, 0, 0, 5),
					backends: []net.IP{net.IPv4(10, 0, 1, 3)},
					ports:    []*PortConfig{ingressPort(8080, 80)},
				},
				{id: "svc-b", ports: "9090:90/TCP"}: {
					vip:      net.IPv4(10, 0, 0, 6),
					backends: []net.IP{net.IPv4(10, 0, 1, 7)},
					ports:    []*PortConfig{ingressPort(9090, 90)},
				},
			},
			wantVIPs: map[string][]string{
				"10.0.0.5": {"10.0.1.3"},
				"10.0.0.6": {"10.0.1.7"},
			},
			wantPorts: map[portKey][]string{
				{proto: ProtocolTCP, port: 8080}: {"10.0.1.3"},
				{proto: ProtocolTCP, port: 9090}: {"10.0.1.7"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vipBackends, portBackends := composeNFTLB(&nftLBState{contrib: tc.contrib})

			// EquateEmpty because a contribution with no backends records its keys
			// with a nil slice.
			assert.Check(t, is.DeepEqual(addrMap(vipBackends), tc.wantVIPs, cmpopts.EquateEmpty()))
			assert.Check(t, is.DeepEqual(addrMap(portBackends), tc.wantPorts, cmpopts.EquateEmpty()))
		})
	}
}

// cutLast slices s around the last instance of sep, like [strings.Cut] does
// around the first. Doing that by hand means repeating len(sep) at every call
// site, which is easy to get wrong when the separator changes.
//
// This is a stopgap for strings.CutLast, which arrives in Go 1.27. It matches
// that function's contract exactly, including returning s, "", false when sep is
// absent, so once the module's go directive reaches 1.27 (currently 1.26.3) this
// can be deleted and the call sites pointed at the standard library - a rename,
// with no behaviour to re-check.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// splitElements returns the elements' fully-qualified keys, and the values
// grouped by key prefix (the key without its interval).
//
// The keys are fully determined - they follow from the VIP or published port and
// the number of backends serving it - but which backend lands in which bucket is
// deliberately not, so a test must assert the keys exactly and the values only as
// a set. Asserting the pairing makes for a test that passes most of the time.
func splitElements(elems []nftables.MapElement) (keys []string, byPrefix map[string][]string) {
	byPrefix = map[string][]string{}
	for _, e := range elems {
		keys = append(keys, e.MapName+" "+e.Key)
		keyPrefix, _, _ := cutLast(e.Key, " . ")
		prefix := e.MapName + " " + keyPrefix
		byPrefix[prefix] = append(byPrefix[prefix], e.Value)
	}
	slices.Sort(keys)
	for _, vs := range byPrefix {
		slices.Sort(vs)
	}
	return keys, byPrefix
}

// TestNFTLBNATElementsSharedPort is the end of the chain that the service-id
// sweep bug broke: two generations of a service coexist during a published-port
// change, and the elements they produce must partition each key's bucket space
// exactly once. Overlapping intervals for one (proto, port) are rejected by
// nftables, so this asserts the actual keys rather than just the backend sets.
func TestNFTLBNATElementsSharedPort(t *testing.T) {
	vip := net.IPv4(10, 0, 0, 5)
	state := &nftLBState{contrib: map[serviceKey]nftLBContribution{
		{id: "svc", ports: "8080:80/TCP"}: {
			vip:      vip,
			backends: []net.IP{net.IPv4(10, 0, 1, 3)},
			ports:    []*PortConfig{ingressPort(8080, 80)},
		},
		{id: "svc", ports: "8080:80/TCP,9090:90/TCP"}: {
			vip:      vip,
			backends: []net.IP{net.IPv4(10, 0, 1, 9)},
			ports:    []*PortConfig{ingressPort(8080, 80), ingressPort(9090, 90)},
		},
	}}

	keys, byPrefix := splitElements(nftLBNATElements(composeNFTLB(state)))

	// Two backends serve the VIP and port 8080, so each gets half the bucket
	// space; only the new generation serves 9090, so it gets all of it. The halves
	// abut exactly - no gap, no overlap.
	assert.Check(t, is.DeepEqual(keys, []string{
		"nat-publish-port tcp . 8080 . 0-32767",
		"nat-publish-port tcp . 8080 . 32768-65535",
		"nat-publish-port tcp . 9090 . 0-65535",
		"nat-service-vip 10.0.0.5 . 0-32767",
		"nat-service-vip 10.0.0.5 . 32768-65535",
	}))

	// The shared port reaches both generations' backends; the new port reaches
	// only the generation that publishes it. Routing 9090 to an old task would be
	// a connection refused.
	assert.Check(t, is.DeepEqual(byPrefix, map[string][]string{
		"nat-service-vip 10.0.0.5":    {"10.0.1.3", "10.0.1.9"},
		"nat-publish-port tcp . 8080": {"10.0.1.3", "10.0.1.9"},
		"nat-publish-port tcp . 9090": {"10.0.1.9"},
	}))
}

// TestNFTLBNATElementsPartitionsWholeSpace checks the property nftables actually
// requires of an interval map: for every key prefix the intervals must tile
// [0, numgenModulus) with no gap and no overlap, whatever the backend count.
func TestNFTLBNATElementsPartitionsWholeSpace(t *testing.T) {
	for n := 1; n <= 9; n++ {
		t.Run(fmt.Sprintf("%d-backends", n), func(t *testing.T) {
			backends := make([]net.IP, 0, n)
			for i := range n {
				backends = append(backends, net.IPv4(10, 0, 1, byte(i+3)))
			}
			state := &nftLBState{contrib: map[serviceKey]nftLBContribution{
				{id: "svc", ports: "8080:80/TCP"}: {
					vip:      net.IPv4(10, 0, 0, 5),
					backends: backends,
					ports:    []*PortConfig{ingressPort(8080, 80)},
				},
			}}

			// Collect the intervals per map, in the order they were emitted for that
			// map, and check they tile the space.
			perMap := map[string][][2]int{}
			for _, e := range nftLBNATElements(composeNFTLB(state)) {
				var lo, hi int
				// The interval is the last dot-separated field of the key.
				_, iv, _ := cutLast(e.Key, " . ")
				_, err := fmt.Sscanf(iv, "%d-%d", &lo, &hi)
				assert.NilError(t, err, "parsing interval from key %q", e.Key)
				perMap[e.MapName] = append(perMap[e.MapName], [2]int{lo, hi})
			}

			assert.Check(t, is.Len(perMap, 2), "expected elements in both NAT maps")
			for mapName, ivs := range perMap {
				assert.Check(t, is.Len(ivs, n), "%s: one interval per backend", mapName)
				slices.SortFunc(ivs, func(a, b [2]int) int { return a[0] - b[0] })
				assert.Check(t, is.Equal(ivs[0][0], 0), "%s: must start at 0", mapName)
				assert.Check(t, is.Equal(ivs[len(ivs)-1][1], numgenModulus-1), "%s: must end at the modulus", mapName)
				for i := 1; i < len(ivs); i++ {
					assert.Check(t, is.Equal(ivs[i][0], ivs[i-1][1]+1),
						"%s: interval %d must abut the previous one", mapName, i)
				}
			}
		})
	}
}

// TestNFTLBNATElementsNoBackends checks that a service with every backend
// deweighted contributes no elements at all - rather than, say, an empty or
// reversed interval, which nftables would reject.
func TestNFTLBNATElementsNoBackends(t *testing.T) {
	state := &nftLBState{contrib: map[serviceKey]nftLBContribution{
		{id: "svc", ports: "8080:80/TCP"}: {
			vip:   net.IPv4(10, 0, 0, 5),
			ports: []*PortConfig{ingressPort(8080, 80)},
		},
	}}
	assert.Check(t, is.Len(nftLBNATElements(composeNFTLB(state)), 0))
}

// TestNFTLBNATElementsDistinctServices checks that two services sharing the
// load-balancer sandbox get separate keys throughout - they share the table, so a
// collision here would make one service's elements displace the other's.
func TestNFTLBNATElementsDistinctServices(t *testing.T) {
	state := &nftLBState{contrib: map[serviceKey]nftLBContribution{
		{id: "svc-a", ports: "8080:80/TCP"}: {
			vip:      net.IPv4(10, 0, 0, 5),
			backends: []net.IP{net.IPv4(10, 0, 1, 3)},
			ports:    []*PortConfig{ingressPort(8080, 80)},
		},
		{id: "svc-b", ports: "9090:90/UDP"}: {
			vip:      net.IPv4(10, 0, 0, 6),
			backends: []net.IP{net.IPv4(10, 0, 1, 7)},
			ports:    []*PortConfig{{Protocol: ProtocolUDP, PublishedPort: 9090, TargetPort: 90}},
		},
	}}

	elems := nftLBNATElements(composeNFTLB(state))
	// Each service has a single backend, so every key has exactly one possible
	// value and the elements can be compared as they are. Sorted because
	// nftLBNATElements makes no ordering promise.
	assert.Check(t, is.DeepEqual(elems, []nftables.MapElement{
		{MapName: natPublishPortMap, Key: "tcp . 8080 . 0-65535", Value: "10.0.1.3"},
		{MapName: natPublishPortMap, Key: "udp . 9090 . 0-65535", Value: "10.0.1.7"},
		{MapName: natServiceVipMap, Key: "10.0.0.5 . 0-65535", Value: "10.0.1.3"},
		{MapName: natServiceVipMap, Key: "10.0.0.6 . 0-65535", Value: "10.0.1.7"},
	}, cmpopts.SortSlices(func(a, b nftables.MapElement) int {
		if c := strings.Compare(a.MapName, b.MapName); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})))
}

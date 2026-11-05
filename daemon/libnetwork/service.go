package libnetwork

import (
	"fmt"
	"net"
	strings "strings"
	"sync"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/setmatrix"
)

var (
	// A global monotonic counter to assign firewall marks to
	// services.
	fwMarkCtr   uint32 = 256
	fwMarkCtrMu sync.Mutex
)

type portConfigs []*PortConfig

func (p portConfigs) String() string {
	var out strings.Builder
	for i, pc := range p {
		if i > 0 {
			out.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&out, "%d:%d/%s", pc.PublishedPort, pc.TargetPort, PortConfig_Protocol_name[int32(pc.Protocol)])
	}
	return out.String()
}

type serviceKey struct {
	id    string
	ports string
}

type service struct {
	name string // Service Name
	id   string // Service ID

	// Map of loadbalancers for the service one-per attached
	// network. It is keyed with network ID.
	loadBalancers map[string]*loadBalancer

	// List of ingress ports exposed by the service
	ingressPorts portConfigs

	// This maps tracks for each IP address the list of endpoints ID
	// associated with it. At stable state the endpoint ID expected is 1
	// but during transition and service change it is possible to have
	// temporary more than 1
	ipToEndpoint setmatrix.SetMatrix[string, string]

	deleted bool

	sync.Mutex
}

// assignIPToEndpoint inserts the mapping between the IP and the endpoint identifier
// returns true if the mapping was not present, false otherwise
// returns also the number of endpoints associated to the IP
func (s *service) assignIPToEndpoint(ip, eID string) (bool, int) {
	return s.ipToEndpoint.Insert(ip, eID)
}

// removeIPToEndpoint removes the mapping between the IP and the endpoint identifier
// returns true if the mapping was deleted, false otherwise
// returns also the number of endpoints associated to the IP
func (s *service) removeIPToEndpoint(ip, eID string) (bool, int) {
	return s.ipToEndpoint.Remove(ip, eID)
}

func (s *service) printIPToEndpoint(ip string) (string, bool) {
	return s.ipToEndpoint.String(ip)
}

type lbBackend struct {
	ip       net.IP
	disabled bool
	// aliases is the per-task service alias list this backend was registered
	// with. Stored so rmServiceBinding can decrement the matching counts in
	// service.aliasRefs even when the caller-supplied list has drifted (e.g.
	// during cleanupServiceBindings).
	aliases []string
}

type loadBalancer struct {
	vip    net.IP
	fwMark uint32

	// Map of backend IPs backing this loadbalancer on this
	// network. It is keyed with endpoint ID.
	backEnds map[string]*lbBackend

	// aliasRefs counts how many backends reference each service alias on
	// this network. A VIP DNS record for an alias is created on the 0→1
	// transition and removed on the 1→0 transition, so aliases survive
	// rolling updates as long as any old or new task still claims them.
	//
	// The map is keyed by alias. It lives on loadBalancer (one per network
	// the service is attached to) because a service can configure different
	// alias sets on each of its networks.
	aliasRefs map[string]int

	// Back pointer to service to which the loadbalancer belongs.
	service *service
	sync.Mutex

	osLoadBalancer
}

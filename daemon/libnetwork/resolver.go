package libnetwork

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	"github.com/miekg/dns"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// DNSBackend represents a backend DNS resolver used for DNS name
// resolution. All the queries to the resolver are forwarded to the
// backend resolver.
type DNSBackend interface {
	// ResolveName resolves a service name to an IPv4 or IPv6 address by searching
	// the networks the sandbox is connected to. The second return value will be
	// true if the name exists in docker domain, even if there are no addresses of
	// the required type. Such queries shouldn't be forwarded to external nameservers.
	ResolveName(ctx context.Context, name string, ipType types.IPFamily) ([]net.IP, bool)
	// ResolveIP returns the service name for the passed in IP. IP is in reverse dotted
	// notation; the format used for DNS PTR records
	ResolveIP(ctx context.Context, name string) string
	// ResolveService returns all the backend details about the containers or hosts
	// backing a service. Its purpose is to satisfy an SRV query
	ResolveService(ctx context.Context, name string) ([]*net.SRV, []net.IP)
	// ExecFunc allows a function to be executed in the context of the backend
	// on behalf of the resolver.
	ExecFunc(f func()) error
	// Ndots returns the ndots value from the backend's DNS options, or 0 if
	// the option is not set.
	Ndots() int
	// HandleQueryResp passes the name & IP from a response to the backend. backend
	// can use it to maintain any required state about the resolution
	HandleQueryResp(name string, ip net.IP)
}

const (
	dnsPort       = "53"
	ptrIPv4domain = ".in-addr.arpa."
	ptrIPv6domain = ".ip6.arpa."
	respTTL       = 600
	maxExtDNS     = 3 // max number of external servers to try
	extIOTimeout  = 4 * time.Second
	maxConcurrent = 1024
	logInterval   = 2 * time.Second
)

type extDNSEntry struct {
	IPStr        string
	port         uint16 // for testing
	HostLoopback bool
}

func (e extDNSEntry) String() string {
	if e.HostLoopback {
		return "host(" + e.IPStr + ")"
	}
	return e.IPStr
}

// Resolver is the embedded DNS server in Docker. It operates by listening on
// the container's loopback interface for DNS queries.
type Resolver struct {
	backend       DNSBackend
	extDNSList    [maxExtDNS]extDNSEntry // Ext servers to use when there's no entry in ipToExtDNS.
	ipToExtDNS    addrToExtDNSMap        // DNS query source IP -> ext servers.
	server        *dns.Server
	conn          *net.UDPConn
	tcpServer     *dns.Server
	tcpListen     *net.TCPListener
	err           error
	listenAddress netip.Addr
	proxyDNS      atomic.Bool
	startCh       chan struct{}
	logger        *log.Entry

	fwdSem      *semaphore.Weighted // Limit the number of concurrent external DNS requests in-flight
	logInterval rate.Sometimes      // Rate-limit logging about hitting the fwdSem limit
}

// NewResolver creates a new instance of the Resolver
func NewResolver(address string, proxyDNS bool, backend DNSBackend) *Resolver {
	r := &Resolver{
		backend:     backend,
		err:         errors.New("setup not done yet"),
		startCh:     make(chan struct{}, 1),
		fwdSem:      semaphore.NewWeighted(maxConcurrent),
		logInterval: rate.Sometimes{Interval: logInterval},
	}
	r.listenAddress, _ = netip.ParseAddr(address)
	r.proxyDNS.Store(proxyDNS)

	return r
}

type addrToExtDNSMap struct {
	mu   sync.Mutex
	eMap map[netip.Addr][maxExtDNS]extDNSEntry
}

func (am *addrToExtDNSMap) get(addr netip.Addr) ([maxExtDNS]extDNSEntry, bool) {
	am.mu.Lock()
	defer am.mu.Unlock()
	entries, ok := am.eMap[addr]
	return entries, ok
}

func (am *addrToExtDNSMap) set(addr netip.Addr, entries []extDNSEntry) {
	var e [maxExtDNS]extDNSEntry
	copy(e[:], entries)
	am.mu.Lock()
	defer am.mu.Unlock()
	if len(entries) > 0 {
		if am.eMap == nil {
			am.eMap = map[netip.Addr][maxExtDNS]extDNSEntry{}
		}
		am.eMap[addr] = e
	} else {
		delete(am.eMap, addr)
	}
}

func (r *Resolver) log(ctx context.Context) *log.Entry {
	if r.logger == nil {
		return log.G(ctx)
	}
	return r.logger
}

// SetupFunc returns the setup function that should be run in the container's
// network namespace.
func (r *Resolver) SetupFunc(port uint16) func() {
	return func() {
		var err error

		// DNS operates primarily on UDP
		r.conn, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(
			netip.AddrPortFrom(r.listenAddress, port)),
		)
		if err != nil {
			r.err = fmt.Errorf("error in opening name server socket %v", err)
			return
		}

		// Listen on a TCP as well
		r.tcpListen, err = net.ListenTCP("tcp", net.TCPAddrFromAddrPort(
			netip.AddrPortFrom(r.listenAddress, port)),
		)
		if err != nil {
			r.err = fmt.Errorf("error in opening name TCP server socket %v", err)
			return
		}
		r.err = nil
	}
}

// Start starts the name server for the container.
func (r *Resolver) Start() error {
	r.startCh <- struct{}{}
	defer func() { <-r.startCh }()

	// make sure the resolver has been setup before starting
	if r.err != nil {
		return r.err
	}

	if err := r.setupNAT(context.TODO()); err != nil {
		return fmt.Errorf("setting up DNAT/SNAT rules failed: %v", err)
	}

	s := &dns.Server{Handler: dns.HandlerFunc(r.serveDNS), PacketConn: r.conn}
	r.server = s
	go func() {
		if err := s.ActivateAndServe(); err != nil {
			r.log(context.TODO()).WithError(err).Error("[resolver] failed to start PacketConn DNS server")
		}
	}()

	tcpServer := &dns.Server{Handler: dns.HandlerFunc(r.serveDNS), Listener: r.tcpListen}
	r.tcpServer = tcpServer
	go func() {
		if err := tcpServer.ActivateAndServe(); err != nil {
			r.log(context.TODO()).WithError(err).Error("[resolver] failed to start TCP DNS server")
		}
	}()
	return nil
}

// Stop stops the name server for the container. A stopped resolver can be
// reused after running the SetupFunc again.
func (r *Resolver) Stop() {
	r.startCh <- struct{}{}
	defer func() { <-r.startCh }()

	if r.server != nil {
		r.server.Shutdown() //nolint:errcheck
	}
	if r.tcpServer != nil {
		r.tcpServer.Shutdown() //nolint:errcheck
	}
	r.conn = nil
	r.tcpServer = nil
	r.err = errors.New("setup not done yet")
	r.fwdSem = semaphore.NewWeighted(maxConcurrent)
}

// SetExtServers configures the external nameservers the resolver should use
// when forwarding queries, unless SetExtServersForSrc has configured servers
// for the DNS client making the request.
func (r *Resolver) SetExtServers(extDNS []extDNSEntry) {
	copy(r.extDNSList[:], r.filterExtServers(extDNS))
}

// SetForwardingPolicy re-configures the embedded DNS resolver to either enable or disable forwarding DNS queries to
// external servers.
func (r *Resolver) SetForwardingPolicy(policy bool) {
	r.proxyDNS.Store(policy)
}

// SetExtServersForSrc configures the external nameservers the resolver should
// use when forwarding queries from srcAddr. If set, these servers will be used
// in preference to servers set by SetExtServers. Supplying a nil or empty extDNS
// deletes nameservers for srcAddr.
func (r *Resolver) SetExtServersForSrc(srcAddr netip.Addr, extDNS []extDNSEntry) error {
	r.ipToExtDNS.set(srcAddr, r.filterExtServers(extDNS))
	return nil
}

// NameServer returns the IP of the DNS resolver for the containers.
func (r *Resolver) NameServer() netip.Addr {
	return r.listenAddress
}

// ResolverOptions returns resolv.conf options that should be set.
func (r *Resolver) ResolverOptions() []string {
	return []string{"ndots:0"}
}

// filterExtServers removes the resolver's own address from extDNS if present,
// and returns the result.
func (r *Resolver) filterExtServers(extDNS []extDNSEntry) []extDNSEntry {
	result := make([]extDNSEntry, 0, len(extDNS))
	for _, e := range extDNS {
		if !e.HostLoopback {
			if ra, _ := netip.ParseAddr(e.IPStr); ra == r.listenAddress {
				log.G(context.TODO()).Infof("[resolver] not using own address (%s) as an external DNS server",
					r.listenAddress)
				continue
			}
		}
		result = append(result, e)
	}
	return result
}

func createRespMsg(query *dns.Msg) *dns.Msg {
	resp := &dns.Msg{}
	resp.SetReply(query)
	resp.RecursionAvailable = true

	return resp
}

func (r *Resolver) handleMXQuery(ctx context.Context, query *dns.Msg) (*dns.Msg, error) {
	name := query.Question[0].Name
	addrv4, _ := r.backend.ResolveName(ctx, name, types.IPv4)
	addrv6, _ := r.backend.ResolveName(ctx, name, types.IPv6)

	if addrv4 == nil && addrv6 == nil {
		return nil, nil
	}

	// We were able to resolve the name. Respond with an empty list with
	// RcodeSuccess/NOERROR so that email clients can treat it as "implicit MX"
	// [RFC 5321 Section-5.1] and issue a Type A/AAAA query for the name.

	resp := createRespMsg(query)
	return resp, nil
}

func (r *Resolver) handleIPQuery(ctx context.Context, query *dns.Msg, ipType types.IPFamily) (*dns.Msg, error) {
	name := query.Question[0].Name
	addr, ok := r.backend.ResolveName(ctx, name, ipType)
	if !ok {
		return nil, nil
	}

	r.log(ctx).Debugf("[resolver] lookup for %s: IP %v", name, addr)

	resp := createRespMsg(query)
	rand.Shuffle(len(addr), func(i, j int) { // #nosec G404 -- use of math/rand/v2 is fine for this purpose.
		addr[i], addr[j] = addr[j], addr[i]
	})
	if ipType == types.IPv4 {
		for _, ip := range addr {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: respTTL},
				A:   ip,
			})
		}
	} else {
		for _, ip := range addr {
			resp.Answer = append(resp.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: respTTL},
				AAAA: ip,
			})
		}
	}
	return resp, nil
}

func (r *Resolver) handlePTRQuery(ctx context.Context, query *dns.Msg) (*dns.Msg, error) {
	ptr := query.Question[0].Name
	name, after, found := strings.Cut(ptr, ptrIPv4domain)
	if !found || after != "" {
		name, after, found = strings.Cut(ptr, ptrIPv6domain)
	}
	if !found || after != "" {
		// Not a known IPv4 or IPv6 PTR domain.
		// Maybe the external DNS servers know what to do with the query?
		return nil, nil
	}

	host := r.backend.ResolveIP(ctx, name)
	if host == "" {
		return nil, nil
	}

	r.log(ctx).Debugf("[resolver] lookup for IP %s: name %s", name, host)
	fqdn := dns.Fqdn(host)

	resp := createRespMsg(query)
	resp.Answer = append(resp.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: ptr, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: respTTL},
		Ptr: fqdn,
	})
	return resp, nil
}

func (r *Resolver) handleSRVQuery(ctx context.Context, query *dns.Msg) (*dns.Msg, error) {
	svc := query.Question[0].Name
	srv, ip := r.backend.ResolveService(ctx, svc)

	if len(srv) == 0 {
		return nil, nil
	}
	if len(srv) != len(ip) {
		return nil, fmt.Errorf("invalid reply for SRV query %s", svc)
	}

	resp := createRespMsg(query)

	for i, r := range srv {
		resp.Answer = append(resp.Answer, &dns.SRV{
			Hdr:    dns.RR_Header{Name: svc, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: respTTL},
			Port:   r.Port,
			Target: r.Target,
		})
		resp.Extra = append(resp.Extra, &dns.A{
			Hdr: dns.RR_Header{Name: r.Target, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: respTTL},
			A:   ip[i],
		})
	}
	return resp, nil
}

func (r *Resolver) serveDNS(w dns.ResponseWriter, query *dns.Msg) {
	if query == nil || len(query.Question) == 0 {
		return
	}

	queryName := query.Question[0].Name
	queryType := query.Question[0].Qtype

	ctx, span := otel.Tracer("").Start(context.Background(), "resolver.serveDNS", trace.WithAttributes(
		attribute.String("libnet.resolver.query.name", queryName),
		attribute.String("libnet.resolver.query.type", dns.TypeToString[queryType]),
	))
	defer span.End()

	var (
		resp *dns.Msg
		err  error
	)

	switch queryType {
	case dns.TypeA:
		resp, err = r.handleIPQuery(ctx, query, types.IPv4)
	case dns.TypeAAAA:
		resp, err = r.handleIPQuery(ctx, query, types.IPv6)
	case dns.TypeMX:
		resp, err = r.handleMXQuery(ctx, query)
	case dns.TypePTR:
		resp, err = r.handlePTRQuery(ctx, query)
	case dns.TypeSRV:
		resp, err = r.handleSRVQuery(ctx, query)
	default:
		r.log(ctx).Debugf("[resolver] query type %s is not supported by the embedded DNS and will be forwarded to external DNS", dns.TypeToString[queryType])
	}

	reply := func(msg *dns.Msg) {
		if err := w.WriteMsg(msg); err != nil {
			r.log(ctx).WithError(err).Error("[resolver] failed to write response")
			span.RecordError(err)
			span.SetStatus(codes.Error, "WriteMsg failed")
			// Make a best-effort attempt to send a failure response to the
			// client so it doesn't have to wait for a timeout if the failure
			// has to do with the content of msg rather than the connection.
			if msg.Rcode != dns.RcodeServerFailure {
				if err := w.WriteMsg(new(dns.Msg).SetRcode(query, dns.RcodeServerFailure)); err != nil {
					r.log(ctx).WithError(err).Error("[resolver] writing ServFail response also failed")
					span.RecordError(err)
				}
			}
		}
	}

	if err != nil {
		r.log(ctx).WithError(err).Errorf("[resolver] failed to handle query: %s (%s)", queryName, dns.TypeToString[queryType])
		reply(new(dns.Msg).SetRcode(query, dns.RcodeServerFailure))
		return
	}

	if resp != nil {
		// We are the authoritative DNS server for this request so it's
		// on us to truncate the response message to the size limit
		// negotiated by the client.
		maxSize := dns.MinMsgSize
		if w.LocalAddr().Network() == "tcp" {
			maxSize = dns.MaxMsgSize
		} else {
			if optRR := query.IsEdns0(); optRR != nil {
				if udpsize := int(optRR.UDPSize()); udpsize > maxSize {
					maxSize = udpsize
				}
			}
		}
		resp.Truncate(maxSize)
		span.AddEvent("found local record", trace.WithAttributes(
			attribute.String("libnet.resolver.resp", resp.String()),
		))
		reply(resp)
		return
	}

	// If the user sets ndots > 0 explicitly and the query is
	// in the root domain don't forward it out. We will return
	// failure and let the client retry with the search domain
	// attached.
	if (queryType == dns.TypeA || queryType == dns.TypeAAAA) && r.backend.Ndots() > 0 &&
		!strings.Contains(strings.TrimSuffix(queryName, "."), ".") {
		resp = createRespMsg(query)
	} else {
		resp = r.forwardExtDNS(ctx, w.LocalAddr().Network(), w.RemoteAddr(), query)
	}

	if resp == nil {
		// We were unable to get an answer from any of the upstream DNS
		// servers or the backend doesn't support proxying DNS requests.
		resp = new(dns.Msg).SetRcode(query, dns.RcodeServerFailure)
	}
	reply(resp)
}

const defaultPort = "53"

func (r *Resolver) dialExtDNS(proto string, server extDNSEntry) (net.Conn, error) {
	port := defaultPort
	if server.port != 0 {
		port = strconv.FormatUint(uint64(server.port), 10)
	}
	addr := net.JoinHostPort(server.IPStr, port)

	if server.HostLoopback {
		return net.DialTimeout(proto, addr, extIOTimeout)
	}

	var (
		extConn net.Conn
		dialErr error
	)
	err := r.backend.ExecFunc(func() {
		extConn, dialErr = net.DialTimeout(proto, addr, extIOTimeout)
	})
	if err != nil {
		return nil, err
	}
	if dialErr != nil {
		return nil, dialErr
	}

	return extConn, nil
}

func (r *Resolver) forwardExtDNS(ctx context.Context, proto string, remoteAddr net.Addr, query *dns.Msg) *dns.Msg {
	ctx, span := otel.Tracer("").Start(ctx, "resolver.forwardExtDNS")
	defer span.End()

	proxyDNS := r.proxyDNS.Load()
	for _, extDNS := range r.extDNS(netiputil.AddrPortFromNet(remoteAddr)) {
		if extDNS.IPStr == "" {
			break
		}
		// If proxyDNS is false, do not forward the request from the host's namespace
		// (don't access an external DNS server from an internal network). But, it is
		// safe to make the request from the container's network namespace - it'll fail
		// if the DNS server is not accessible, but the server may be on-net.
		if !proxyDNS && extDNS.HostLoopback {
			continue
		}

		// limits the number of outstanding concurrent queries.
		semAcqCtx, cancelSemAcqCtx := context.WithTimeout(ctx, extIOTimeout)
		err := r.fwdSem.Acquire(semAcqCtx, 1)
		cancelSemAcqCtx()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				r.logInterval.Do(func() {
					r.log(ctx).Errorf("[resolver] more than %v concurrent queries", maxConcurrent)
				})
			}
			return new(dns.Msg).SetRcode(query, dns.RcodeRefused)
		}
		resp := func() *dns.Msg {
			defer r.fwdSem.Release(1)
			return r.exchange(ctx, proto, extDNS, query)
		}()
		if resp == nil {
			continue
		}

		switch resp.Rcode {
		case dns.RcodeServerFailure, dns.RcodeRefused:
			// Server returned FAILURE: continue with the next external DNS server
			// Server returned REFUSED: this can be a transitional status, so continue with the next external DNS server
			r.log(ctx).Debugf("[resolver] external DNS %s:%s returned failure:\n%s", proto, extDNS.IPStr, resp)
			continue
		}
		answers := 0
		for _, rr := range resp.Answer {
			h := rr.Header()
			switch h.Rrtype {
			case dns.TypeA:
				answers++
				ip := rr.(*dns.A).A
				r.log(ctx).Debugf("[resolver] received A record %q for %q from %s:%s", ip, h.Name, proto, extDNS.IPStr)
				r.backend.HandleQueryResp(h.Name, ip)
			case dns.TypeAAAA:
				answers++
				ip := rr.(*dns.AAAA).AAAA
				r.log(ctx).Debugf("[resolver] received AAAA record %q for %q from %s:%s", ip, h.Name, proto, extDNS.IPStr)
				r.backend.HandleQueryResp(h.Name, ip)
			}
		}
		if len(resp.Answer) == 0 {
			r.log(ctx).Debugf("[resolver] external DNS %s:%s returned response with no answers:\n%s", proto, extDNS.IPStr, resp)
		}
		resp.Compress = true
		span.AddEvent("response from upstream server")
		return resp
	}

	span.AddEvent("no response from upstream servers")
	return nil
}

func (r *Resolver) extDNS(remoteAddr netip.AddrPort) []extDNSEntry {
	if res, ok := r.ipToExtDNS.get(remoteAddr.Addr()); ok {
		return res[:]
	}
	return r.extDNSList[:]
}

func (r *Resolver) exchange(ctx context.Context, proto string, extDNS extDNSEntry, query *dns.Msg) *dns.Msg {
	ctx, span := otel.Tracer("").Start(ctx, "resolver.exchange", trace.WithAttributes(
		attribute.String("libnet.resolver.upstream.proto", proto),
		attribute.String("libnet.resolver.upstream.address", extDNS.IPStr),
		attribute.Bool("libnet.resolver.upstream.host-loopback", extDNS.HostLoopback)))
	defer span.End()

	extConn, err := r.dialExtDNS(proto, extDNS)
	if err != nil {
		r.log(ctx).WithError(err).Warn("[resolver] connect failed")
		span.RecordError(err)
		span.SetStatus(codes.Error, "dialExtDNS failed")
		return nil
	}
	defer extConn.Close()

	logger := r.log(ctx).WithFields(log.Fields{
		"dns-server":  extConn.RemoteAddr().Network() + ":" + extConn.RemoteAddr().String(),
		"client-addr": extConn.LocalAddr().Network() + ":" + extConn.LocalAddr().String(),
		"question":    query.Question[0].String(),
	})
	logger.Debug("[resolver] forwarding query")

	resp, _, err := (&dns.Client{
		Timeout: extIOTimeout,
		// Following the robustness principle, make a best-effort
		// attempt to receive oversized response messages without
		// truncating them on our end to forward verbatim to the client.
		// Some DNS servers (e.g. Mikrotik RouterOS) don't support
		// EDNS(0) and may send replies over UDP longer than 512 bytes
		// regardless of what size limit, if any, was advertised in the
		// query message. Note that ExchangeWithConn will override this
		// value if it detects an EDNS OPT record in query so only
		// oversized replies to non-EDNS queries will benefit.
		UDPSize: dns.MaxMsgSize,
	}).ExchangeWithConn(query, &dns.Conn{Conn: extConn})
	if err != nil {
		logger.WithError(err).Error("[resolver] failed to query external DNS server")
		span.RecordError(err)
		span.SetStatus(codes.Error, "ExchangeWithConn failed")
		return nil
	}

	if resp == nil {
		// Should be impossible, so make noise if it happens anyway.
		logger.Error("[resolver] external DNS returned empty response")
		span.SetStatus(codes.Error, "External DNS returned empty response")
	}
	return resp
}

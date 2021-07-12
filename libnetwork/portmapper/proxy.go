package portmapper

import (
	"fmt"
	"io"
	"net"

	"github.com/ishidawataru/sctp"
)

type userlandProxy interface {
	Start() error
	Stop() error
}

// ipVersion refers to IP version - v4 or v6
type ipVersion string

const (
	// IPv4 is version 4
	ipv4 ipVersion = "4"
	// IPv4 is version 6
	ipv6 ipVersion = "6"
)

// dummyProxy just listen on some port, it is needed to prevent accidental
// port allocations on bound port, because without userland proxy we using
// iptables rules and not net.Listen
type dummyProxy struct {
	listener  io.Closer
	addr      net.Addr
	ipVersion ipVersion
}

func newDummyProxy(proto string, hostIP net.IP, hostPort int) (userlandProxy, error) {
	// detect version of hostIP to bind only to correct version
	version := ipv4
	if hostIP.To4() == nil {
		version = ipv6
	}
	switch proto {
	case "tcp":
		addr := &net.TCPAddr{IP: hostIP, Port: hostPort}
		return &dummyProxy{addr: addr, ipVersion: version}, nil
	case "udp":
		addr := &net.UDPAddr{IP: hostIP, Port: hostPort}
		return &dummyProxy{addr: addr, ipVersion: version}, nil
	case "sctp":
		addr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: hostIP}}, Port: hostPort}
		return &dummyProxy{addr: addr, ipVersion: version}, nil
	default:
		return nil, fmt.Errorf("Unknown addr type: %s", proto)
	}
}

func (p *dummyProxy) Start() error {
	switch addr := p.addr.(type) {
	case *net.TCPAddr:
		l, err := net.ListenTCP("tcp"+string(p.ipVersion), addr)
		if err != nil {
			return err
		}
		p.listener = l
	case *net.UDPAddr:
		l, err := net.ListenUDP("udp"+string(p.ipVersion), addr)
		if err != nil {
			return err
		}
		p.listener = l
	case *sctp.SCTPAddr:
		l, err := sctp.ListenSCTP("sctp"+string(p.ipVersion), addr)
		if err != nil {
			return err
		}
		p.listener = l
	default:
		return fmt.Errorf("Unknown addr type: %T", p.addr)
	}
	return nil
}

func (p *dummyProxy) Stop() error {
	if p.listener != nil {
		return p.listener.Close()
	}
	return nil
}

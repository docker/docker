package ipvs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// For Quick Reference IPVS related netlink message is described at the end of this file.
var (
	native     = nl.NativeEndian()
	ipvsFamily int
	ipvsOnce   sync.Once
)

// genlMsgHdrLen is the serialized size of genlMsgHdr. Keep this explicit
// rather than deriving it with unsafe.Sizeof so the wire format does not
// depend on the Go struct representation.
const genlMsgHdrLen = 4

type genlMsgHdr struct {
	cmd      uint8
	version  uint8
	reserved uint16
}

// ipvsFlagsLen is the serialized size of ipvsFlags. Keep this explicit
// rather than deriving it with unsafe.Sizeof so the wire format does not
// depend on the Go struct representation.
const ipvsFlagsLen = 8

type ipvsFlags struct {
	flags uint32
	mask  uint32
}

func (hdr *genlMsgHdr) Serialize() []byte {
	b := make([]byte, genlMsgHdrLen)
	b[0] = hdr.cmd
	b[1] = hdr.version
	native.PutUint16(b[2:], hdr.reserved)
	return b
}

func (*genlMsgHdr) Len() int {
	return genlMsgHdrLen
}

func (f *ipvsFlags) Serialize() []byte {
	b := make([]byte, ipvsFlagsLen)
	native.PutUint32(b[0:], f.flags)
	native.PutUint32(b[4:], f.mask)
	return b
}

func (*ipvsFlags) Len() int {
	return ipvsFlagsLen
}

func setup() {
	ipvsOnce.Do(func() {
		var err error
		if out, err := exec.Command("modprobe", "-va", "ip_vs").CombinedOutput(); err != nil {
			logrus.Warnf("Running modprobe ip_vs failed with message: `%s`, error: %v", strings.TrimSpace(string(out)), err)
		}

		ipvsFamily, err = getIPVSFamily()
		if err != nil {
			logrus.Error("Could not get ipvs family information from the kernel. It is possible that ipvs is not enabled in your kernel. Native loadbalancing will not work until this is fixed.")
		}
	})
}

func fillService(s *Service) nl.NetlinkRequestData {
	cmdAttr := nl.NewRtAttr(int(ipvsCmdAttrService), nil)
	cmdAttr.AddRtAttr(int(ipvsSvcAttrAddressFamily), nl.Uint16Attr(s.AddressFamily))
	if s.FWMark != 0 {
		cmdAttr.AddRtAttr(int(ipvsSvcAttrFWMark), nl.Uint32Attr(s.FWMark))
	} else {
		cmdAttr.AddRtAttr(int(ipvsSvcAttrProtocol), nl.Uint16Attr(s.Protocol))
		cmdAttr.AddRtAttr(int(ipvsSvcAttrAddress), rawIPData(s.Address))

		// Port needs to be in network byte order.
		var portBuf bytes.Buffer
		_ = binary.Write(&portBuf, binary.BigEndian, s.Port)
		cmdAttr.AddRtAttr(int(ipvsSvcAttrPort), portBuf.Bytes())
	}

	cmdAttr.AddRtAttr(int(ipvsSvcAttrSchedName), nl.ZeroTerminated(s.SchedName))
	if s.PEName != "" {
		cmdAttr.AddRtAttr(int(ipvsSvcAttrPEName), nl.ZeroTerminated(s.PEName))
	}
	f := &ipvsFlags{
		flags: s.Flags,
		mask:  0xFFFFFFFF,
	}
	cmdAttr.AddRtAttr(int(ipvsSvcAttrFlags), f.Serialize())
	cmdAttr.AddRtAttr(int(ipvsSvcAttrTimeout), nl.Uint32Attr(s.Timeout))
	cmdAttr.AddRtAttr(int(ipvsSvcAttrNetmask), nl.Uint32Attr(s.Netmask))
	return cmdAttr
}

func fillDestination(d *Destination) (nl.NetlinkRequestData, error) {
	if d.Weight < 0 || d.Weight > math.MaxInt32 {
		return nil, fmt.Errorf("destination weight out of range: %d", d.Weight)
	}
	cmdAttr := nl.NewRtAttr(int(ipvsCmdAttrDest), nil)

	cmdAttr.AddRtAttr(int(ipvsDestAttrAddress), rawIPData(d.Address))
	// Port needs to be in network byte order.
	var portBuf bytes.Buffer
	_ = binary.Write(&portBuf, binary.BigEndian, d.Port)
	cmdAttr.AddRtAttr(int(ipvsDestAttrPort), portBuf.Bytes())
	cmdAttr.AddRtAttr(int(ipvsDestAttrForwardingMethod), nl.Uint32Attr(d.ConnectionFlags&ConnFwdMask))
	cmdAttr.AddRtAttr(int(ipvsDestAttrWeight), nl.Uint32Attr(uint32(d.Weight)))
	cmdAttr.AddRtAttr(int(ipvsDestAttrUpperThreshold), nl.Uint32Attr(d.UpperThreshold))
	cmdAttr.AddRtAttr(int(ipvsDestAttrLowerThreshold), nl.Uint32Attr(d.LowerThreshold))

	return cmdAttr, nil
}

func (i *Handle) doCmdwithResponse(s *Service, d *Destination, cmd ipvsCmd) ([][]byte, error) {
	req := newIPVSRequest(cmd)
	req.Seq = atomic.AddUint32(&i.seq, 1)

	if s == nil {
		req.Flags |= syscall.NLM_F_DUMP                         // Flag to dump all messages
		req.AddData(nl.NewRtAttr(int(ipvsCmdAttrService), nil)) // Add a dummy attribute
	} else {
		req.AddData(fillService(s))
	}

	if d == nil {
		if cmd == ipvsCmdGetDest {
			req.Flags |= syscall.NLM_F_DUMP
		}
	} else {
		attr, err := fillDestination(d)
		if err != nil {
			return nil, err
		}
		req.AddData(attr)
	}

	res, err := execute(i.sock, req)
	if err != nil {
		return [][]byte{}, err
	}

	return res, nil
}

func (i *Handle) doCmd(s *Service, d *Destination, cmd ipvsCmd) error {
	_, err := i.doCmdwithResponse(s, d, cmd)

	return err
}

func getIPVSFamily() (int, error) {
	sock, err := nl.GetNetlinkSocketAt(netns.None(), netns.None(), syscall.NETLINK_GENERIC)
	if err != nil {
		return 0, err
	}
	defer sock.Close()

	req := newGenlRequest(genlCtrlID, genlCtrlCmdGetFamily)
	req.AddData(nl.NewRtAttr(genlCtrlAttrFamilyName, nl.ZeroTerminated("IPVS")))

	msgs, err := execute(sock, req)
	if err != nil {
		return 0, err
	}

	for _, m := range msgs {
		if len(m) < genlMsgHdrLen {
			return 0, errors.New("invalid generic netlink response: message too short")
		}

		attrs, err := nl.ParseRouteAttr(m[genlMsgHdrLen:])
		if err != nil {
			return 0, err
		}

		for _, attr := range attrs {
			if int(attr.Attr.Type) != genlCtrlAttrFamilyID {
				continue
			}
			if len(attr.Value) < 2 {
				return 0, errors.New("invalid generic netlink response: family ID too short")
			}
			return int(native.Uint16(attr.Value[:2])), nil
		}
	}

	return 0, errors.New("no family id in the netlink response")
}

func rawIPData(ip net.IP) []byte {
	family := nl.GetIPFamily(ip)
	if family == unix.AF_INET {
		return ip.To4()
	}
	return ip
}

func newIPVSRequest(cmd ipvsCmd) *nl.NetlinkRequest {
	return newGenlRequest(ipvsFamily, uint8(cmd))
}

func newGenlRequest(familyID int, cmd uint8) *nl.NetlinkRequest {
	req := nl.NewNetlinkRequest(familyID, syscall.NLM_F_ACK)
	req.AddData(&genlMsgHdr{cmd: cmd, version: 1})
	return req
}

func execute(s *nl.NetlinkSocket, req *nl.NetlinkRequest) ([][]byte, error) {
	if err := s.Send(req); err != nil {
		return nil, err
	}

	pid, err := s.GetPid()
	if err != nil {
		return nil, err
	}

	var res [][]byte

done:
	for {
		msgs, _, err := s.Receive()
		if err != nil {
			if s.GetFd() == -1 {
				return nil, errors.New("socket got closed on receive")
			}
			if err == syscall.EAGAIN {
				// timeout fired
				continue
			}
			return nil, err
		}
		for _, m := range msgs {
			if m.Header.Seq != req.Seq {
				continue
			}
			if m.Header.Pid != pid {
				return nil, fmt.Errorf("wrong pid %d, expected %d", m.Header.Pid, pid)
			}
			if m.Header.Type == syscall.NLMSG_DONE {
				break done
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				if len(m.Data) < 4 {
					return nil, errors.New("invalid netlink error response: message too short")
				}
				errno := native.Uint32(m.Data[:4])
				if errno == 0 {
					break done
				}
				return nil, syscall.Errno(-errno)
			}
			res = append(res, m.Data)
			if m.Header.Flags&syscall.NLM_F_MULTI == 0 {
				break done
			}
		}
	}
	return res, nil
}

func parseIP(ip []byte, family uint16) (net.IP, error) {
	switch family {
	case syscall.AF_INET:
		if len(ip) < net.IPv4len {
			return nil, fmt.Errorf("invalid IPv4 address: %d bytes", len(ip))
		}
		return ip[:net.IPv4len], nil
	case syscall.AF_INET6:
		if len(ip) < net.IPv6len {
			return nil, fmt.Errorf("invalid IPv6 address: %d bytes", len(ip))
		}
		return ip[:net.IPv6len], nil
	default:
		return nil, fmt.Errorf("unsupported address family: %d", family)
	}
}

// parseStats
func assembleStats(msg []byte) (SvcStats, error) {
	var s SvcStats

	attrs, err := nl.ParseRouteAttr(msg)
	if err != nil {
		return s, err
	}

	for _, attr := range attrs {
		switch ipvsStats(attr.Attr.Type) {
		case ipvsStatsUnspec:
			// unspecified
		case ipvsStatsConns:
			s.Connections = native.Uint32(attr.Value)
		case ipvsStatsPktsIn:
			s.PacketsIn = native.Uint32(attr.Value)
		case ipvsStatsPktsOut:
			s.PacketsOut = native.Uint32(attr.Value)
		case ipvsStatsBytesIn:
			s.BytesIn = native.Uint64(attr.Value)
		case ipvsStatsBytesOut:
			s.BytesOut = native.Uint64(attr.Value)
		case ipvsStatsCPS:
			s.CPS = native.Uint32(attr.Value)
		case ipvsStatsPPSIn:
			s.PPSIn = native.Uint32(attr.Value)
		case ipvsStatsPPSOut:
			s.PPSOut = native.Uint32(attr.Value)
		case ipvsStatsBPSIn:
			s.BPSIn = native.Uint32(attr.Value)
		case ipvsStatsBPSOut:
			s.BPSOut = native.Uint32(attr.Value)
		}
	}
	return s, nil
}

// assembleService assembles a services back from a hain of netlink attributes
func assembleService(attrs []syscall.NetlinkRouteAttr) (*Service, error) {
	var s Service
	var addressBytes []byte

	for _, attr := range attrs {
		switch ipvsSvcAttr(attr.Attr.Type) {
		case ipvsSvcAttrUnspec:
			// unspecified
		case ipvsSvcAttrAddressFamily:
			s.AddressFamily = native.Uint16(attr.Value)
		case ipvsSvcAttrProtocol:
			s.Protocol = native.Uint16(attr.Value)
		case ipvsSvcAttrAddress:
			addressBytes = attr.Value
		case ipvsSvcAttrPort:
			s.Port = binary.BigEndian.Uint16(attr.Value)
		case ipvsSvcAttrFWMark:
			s.FWMark = native.Uint32(attr.Value)
		case ipvsSvcAttrSchedName:
			s.SchedName = nl.BytesToString(attr.Value)
		case ipvsSvcAttrFlags:
			s.Flags = native.Uint32(attr.Value)
		case ipvsSvcAttrTimeout:
			s.Timeout = native.Uint32(attr.Value)
		case ipvsSvcAttrNetmask:
			s.Netmask = native.Uint32(attr.Value)
		case ipvsSvcAttrPEName:
			s.PEName = nl.BytesToString(attr.Value)
		case ipvsSvcAttrStats:
			stats, err := assembleStats(attr.Value)
			if err != nil {
				return nil, err
			}
			s.Stats = stats
		}
	}

	// parse Address after parse AddressFamily incase of parseIP error
	if addressBytes != nil {
		ip, err := parseIP(addressBytes, s.AddressFamily)
		if err != nil {
			return nil, err
		}
		s.Address = ip
	}

	return &s, nil
}

// parseService given a ipvs netlink response this function will respond with a valid service entry, an error otherwise
func (i *Handle) parseService(msg []byte) (*Service, error) {
	var s *Service

	// Remove General header for this message and parse the NetLink message
	if len(msg) < genlMsgHdrLen {
		return nil, errors.New("invalid generic netlink response: message too short")
	}
	NetLinkAttrs, err := nl.ParseRouteAttr(msg[genlMsgHdrLen:])
	if err != nil {
		return nil, err
	}
	if len(NetLinkAttrs) == 0 {
		return nil, errors.New("error no valid netlink message found while parsing service record")
	}

	// Now Parse and get IPVS related attributes messages packed in this message.
	ipvsAttrs, err := nl.ParseRouteAttr(NetLinkAttrs[0].Value)
	if err != nil {
		return nil, err
	}

	// Assemble all the IPVS related attribute messages and create a service record
	s, err = assembleService(ipvsAttrs)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// doGetServicesCmd a wrapper which could be used commonly for both GetServices() and GetService(*Service)
func (i *Handle) doGetServicesCmd(svc *Service) ([]*Service, error) {
	var res []*Service

	msgs, err := i.doCmdwithResponse(svc, nil, ipvsCmdGetService)
	if err != nil {
		return nil, err
	}

	for _, msg := range msgs {
		srv, err := i.parseService(msg)
		if err != nil {
			return nil, err
		}
		res = append(res, srv)
	}

	return res, nil
}

// doCmdWithoutAttr a simple wrapper of netlink socket execute command
func (i *Handle) doCmdWithoutAttr(cmd ipvsCmd) ([][]byte, error) {
	req := newIPVSRequest(cmd)
	req.Seq = atomic.AddUint32(&i.seq, 1)
	return execute(i.sock, req)
}

func assembleDestination(attrs []syscall.NetlinkRouteAttr) (*Destination, error) {
	var d Destination
	var addressBytes []byte

	for _, attr := range attrs {
		switch ipvsDestAttr(attr.Attr.Type) {
		case ipvsDestAttrUnspec:
			// unspecified
		case ipvsDestAttrAddressFamily:
			d.AddressFamily = native.Uint16(attr.Value)
		case ipvsDestAttrAddress:
			addressBytes = attr.Value
		case ipvsDestAttrPort:
			d.Port = binary.BigEndian.Uint16(attr.Value)
		case ipvsDestAttrForwardingMethod:
			d.ConnectionFlags = native.Uint32(attr.Value)
		case ipvsDestAttrWeight:
			d.Weight = int(native.Uint32(attr.Value))
		case ipvsDestAttrUpperThreshold:
			d.UpperThreshold = native.Uint32(attr.Value)
		case ipvsDestAttrLowerThreshold:
			d.LowerThreshold = native.Uint32(attr.Value)
		case ipvsDestAttrActiveConnections:
			d.ActiveConnections = int(native.Uint32(attr.Value))
		case ipvsDestAttrInactiveConnections:
			d.InactiveConnections = int(native.Uint32(attr.Value))
		case ipvsDestAttrPersistentConnections:
			d.PersistentConnections = int(native.Uint32(attr.Value))
		case ipvsDestAttrStats:
			stats, err := assembleStats(attr.Value)
			if err != nil {
				return nil, err
			}
			d.Stats = DstStats(stats)
		}
	}

	// in older kernels (< 3.18), the destination address family attribute doesn't exist so we must
	// assume it based on the destination address provided.
	if d.AddressFamily == 0 {
		// we can't check the address family using net stdlib because netlink returns
		// IPv4 addresses as the first 4 bytes in a []byte of length 16 where as
		// stdlib expects it as the last 4 bytes.
		addressFamily, err := getIPFamily(addressBytes)
		if err != nil {
			return nil, err
		}
		d.AddressFamily = addressFamily
	}

	// parse Address after parse AddressFamily incase of parseIP error
	if addressBytes != nil {
		ip, err := parseIP(addressBytes, d.AddressFamily)
		if err != nil {
			return nil, err
		}
		d.Address = ip
	}

	return &d, nil
}

// getIPFamily parses the IP family based on raw data from netlink.
// For AF_INET, netlink will set the first 4 bytes with trailing zeros
//
//	10.0.0.1 -> [10 0 0 1 0 0 0 0 0 0 0 0 0 0 0 0]
//
// For AF_INET6, the full 16 byte array is used:
//
//	2001:db8:3c4d:15::1a00 -> [32 1 13 184 60 77 0 21 0 0 0 0 0 0 26 0]
func getIPFamily(address []byte) (uint16, error) {
	if len(address) == 4 {
		return syscall.AF_INET, nil
	}

	if isZeros(address) {
		return 0, errors.New("could not parse IP family from address data")
	}

	// assume IPv4 if first 4 bytes are non-zero but rest of the data is trailing zeros
	if !isZeros(address[:4]) && isZeros(address[4:]) {
		return syscall.AF_INET, nil
	}

	return syscall.AF_INET6, nil
}

func isZeros(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

// parseDestination given a ipvs netlink response this function will respond with a valid destination entry, an error otherwise
func (i *Handle) parseDestination(msg []byte) (*Destination, error) {
	var dst *Destination

	// Remove General header for this message
	if len(msg) < genlMsgHdrLen {
		return nil, errors.New("invalid generic netlink response: message too short")
	}
	NetLinkAttrs, err := nl.ParseRouteAttr(msg[genlMsgHdrLen:])
	if err != nil {
		return nil, err
	}
	if len(NetLinkAttrs) == 0 {
		return nil, errors.New("error no valid netlink message found while parsing destination record")
	}

	// Now Parse and get IPVS related attributes messages packed in this message.
	ipvsAttrs, err := nl.ParseRouteAttr(NetLinkAttrs[0].Value)
	if err != nil {
		return nil, err
	}

	// Assemble netlink attributes and create a Destination record
	dst, err = assembleDestination(ipvsAttrs)
	if err != nil {
		return nil, err
	}

	return dst, nil
}

// doGetDestinationsCmd a wrapper function to be used by GetDestinations and GetDestination(d) apis
func (i *Handle) doGetDestinationsCmd(s *Service, d *Destination) ([]*Destination, error) {
	var res []*Destination

	msgs, err := i.doCmdwithResponse(s, d, ipvsCmdGetDest)
	if err != nil {
		return nil, err
	}

	for _, msg := range msgs {
		dest, err := i.parseDestination(msg)
		if err != nil {
			return res, err
		}
		res = append(res, dest)
	}
	return res, nil
}

// parseConfig given a ipvs netlink response this function will respond with a valid config entry, an error otherwise
func (i *Handle) parseConfig(msg []byte) (*Config, error) {
	var c Config

	// Remove General header for this message
	if len(msg) < genlMsgHdrLen {
		return nil, errors.New("invalid generic netlink response: message too short")
	}
	attrs, err := nl.ParseRouteAttr(msg[genlMsgHdrLen:])
	if err != nil {
		return nil, err
	}

	for _, attr := range attrs {
		switch ipvsCmdAttr(attr.Attr.Type) {
		case ipvsCmdAttrUnspec:
			// unspecified
		case ipvsCmdAttrTimeoutTCP:
			c.TimeoutTCP = time.Duration(native.Uint32(attr.Value)) * time.Second
		case ipvsCmdAttrTimeoutTCPFin:
			c.TimeoutTCPFin = time.Duration(native.Uint32(attr.Value)) * time.Second
		case ipvsCmdAttrTimeoutUDP:
			c.TimeoutUDP = time.Duration(native.Uint32(attr.Value)) * time.Second
		case ipvsCmdAttrService, ipvsCmdAttrDest, ipvsCmdAttrDaemon:
			// not implemented
		}
	}

	return &c, nil
}

// doGetConfigCmd a wrapper function to be used by GetConfig
func (i *Handle) doGetConfigCmd() (*Config, error) {
	msg, err := i.doCmdWithoutAttr(ipvsCmdGetConfig)
	if err != nil {
		return nil, err
	}

	res, err := i.parseConfig(msg[0])
	if err != nil {
		return res, err
	}
	return res, nil
}

// doSetConfigCmd a wrapper function to be used by SetConfig
func (i *Handle) doSetConfigCmd(c *Config) error {
	req := newIPVSRequest(ipvsCmdSetConfig)
	req.Seq = atomic.AddUint32(&i.seq, 1)

	req.AddData(nl.NewRtAttr(int(ipvsCmdAttrTimeoutTCP), nl.Uint32Attr(uint32(c.TimeoutTCP.Seconds()))))
	req.AddData(nl.NewRtAttr(int(ipvsCmdAttrTimeoutTCPFin), nl.Uint32Attr(uint32(c.TimeoutTCPFin.Seconds()))))
	req.AddData(nl.NewRtAttr(int(ipvsCmdAttrTimeoutUDP), nl.Uint32Attr(uint32(c.TimeoutUDP.Seconds()))))

	_, err := execute(i.sock, req)

	return err
}

// IPVS related netlink message format explained

/* EACH NETLINK MSG is of the below format, this is what we will receive from execute() api.
   If we have multiple netlink objects to process like GetServices() etc., execute() will
   supply an array of this below object

            NETLINK MSG
|-----------------------------------|
    0        1        2        3
|--------|--------|--------|--------| -
| CMD ID |  VER   |    RESERVED     | |==> General Message Header represented by genlMsgHdr
|-----------------------------------| -
|    ATTR LEN     |   ATTR TYPE     | |
|-----------------------------------| |
|                                   | |
|              VALUE                | |
|     []byte Array of IPVS MSG      | |==> Attribute Message represented by syscall.NetlinkRouteAttr
|        PADDED BY 4 BYTES          | |
|                                   | |
|-----------------------------------| -


 Once We strip genlMsgHdr from above NETLINK MSG, we should parse the VALUE.
 VALUE will have an array of netlink attributes (syscall.NetlinkRouteAttr) such that each attribute will
 represent a "Service" or "Destination" object's field.  If we assemble these attributes we can construct
 Service or Destination.

            IPVS MSG
|-----------------------------------|
     0        1        2        3
|--------|--------|--------|--------|
|    ATTR LEN     |    ATTR TYPE    |
|-----------------------------------|
|                                   |
|                                   |
| []byte IPVS ATTRIBUTE  BY 4 BYTES |
|                                   |
|                                   |
|-----------------------------------|
           NEXT ATTRIBUTE
|-----------------------------------|
|    ATTR LEN     |    ATTR TYPE    |
|-----------------------------------|
|                                   |
|                                   |
| []byte IPVS ATTRIBUTE  BY 4 BYTES |
|                                   |
|                                   |
|-----------------------------------|
           NEXT ATTRIBUTE
|-----------------------------------|
|    ATTR LEN     |    ATTR TYPE    |
|-----------------------------------|
|                                   |
|                                   |
| []byte IPVS ATTRIBUTE  BY 4 BYTES |
|                                   |
|                                   |
|-----------------------------------|

*/

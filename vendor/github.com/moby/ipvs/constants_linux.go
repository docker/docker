package ipvs

import "golang.org/x/sys/unix"

const (
	genlCtrlID = unix.GENL_ID_CTRL
)

// Generic netlink controller constants.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/genetlink.h#L36-L53
const (
	genlCtrlCmdUnspec    = unix.CTRL_CMD_UNSPEC
	genlCtrlCmdNewFamily = unix.CTRL_CMD_NEWFAMILY
	genlCtrlCmdDelFamily = unix.CTRL_CMD_DELFAMILY
	genlCtrlCmdGetFamily = unix.CTRL_CMD_GETFAMILY
)

// Generic netlink family attributes.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/genetlink.h#L57-L70
const (
	genlCtrlAttrUnspec     = unix.CTRL_ATTR_UNSPEC
	genlCtrlAttrFamilyID   = unix.CTRL_ATTR_FAMILY_ID
	genlCtrlAttrFamilyName = unix.CTRL_ATTR_FAMILY_NAME
)

// IPVS generic netlink commands.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L301-L329
type ipvsCmd uint8

const (
	ipvsCmdUnspec     ipvsCmd = 0  // IPVS_CMD_UNSPEC
	ipvsCmdNewService ipvsCmd = 1  // IPVS_CMD_NEW_SERVICE
	ipvsCmdSetService ipvsCmd = 2  // IPVS_CMD_SET_SERVICE
	ipvsCmdDelService ipvsCmd = 3  // IPVS_CMD_DEL_SERVICE
	ipvsCmdGetService ipvsCmd = 4  // IPVS_CMD_GET_SERVICE
	ipvsCmdNewDest    ipvsCmd = 5  // IPVS_CMD_NEW_DEST
	ipvsCmdSetDest    ipvsCmd = 6  // IPVS_CMD_SET_DEST
	ipvsCmdDelDest    ipvsCmd = 7  // IPVS_CMD_DEL_DEST
	ipvsCmdGetDest    ipvsCmd = 8  // IPVS_CMD_GET_DEST
	ipvsCmdNewDaemon  ipvsCmd = 9  // IPVS_CMD_NEW_DAEMON
	ipvsCmdDelDaemon  ipvsCmd = 10 // IPVS_CMD_DEL_DAEMON
	ipvsCmdGetDaemon  ipvsCmd = 11 // IPVS_CMD_GET_DAEMON
	ipvsCmdSetConfig  ipvsCmd = 12 // IPVS_CMD_SET_CONFIG
	ipvsCmdGetConfig  ipvsCmd = 13 // IPVS_CMD_GET_CONFIG
	ipvsCmdSetInfo    ipvsCmd = 14 // IPVS_CMD_SET_INFO
	ipvsCmdGetInfo    ipvsCmd = 15 // IPVS_CMD_GET_INFO
	ipvsCmdZero       ipvsCmd = 16 // IPVS_CMD_ZERO
	ipvsCmdFlush      ipvsCmd = 17 // IPVS_CMD_FLUSH
)

// IPVS generic netlink command attributes.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L333-L343
type ipvsCmdAttr int

const (
	ipvsCmdAttrUnspec        ipvsCmdAttr = 0 // IPVS_CMD_ATTR_UNSPEC
	ipvsCmdAttrService       ipvsCmdAttr = 1 // IPVS_CMD_ATTR_SERVICE
	ipvsCmdAttrDest          ipvsCmdAttr = 2 // IPVS_CMD_ATTR_DEST
	ipvsCmdAttrDaemon        ipvsCmdAttr = 3 // IPVS_CMD_ATTR_DAEMON
	ipvsCmdAttrTimeoutTCP    ipvsCmdAttr = 4 // IPVS_CMD_ATTR_TIMEOUT_TCP
	ipvsCmdAttrTimeoutTCPFin ipvsCmdAttr = 5 // IPVS_CMD_ATTR_TIMEOUT_TCP_FIN
	ipvsCmdAttrTimeoutUDP    ipvsCmdAttr = 6 // IPVS_CMD_ATTR_TIMEOUT_UDP
)

// IPVS service attributes, nested in IPVS_CMD_ATTR_SERVICE.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L347-L372
type ipvsSvcAttr int

const (
	ipvsSvcAttrUnspec        ipvsSvcAttr = 0  // IPVS_SVC_ATTR_UNSPEC
	ipvsSvcAttrAddressFamily ipvsSvcAttr = 1  // IPVS_SVC_ATTR_AF
	ipvsSvcAttrProtocol      ipvsSvcAttr = 2  // IPVS_SVC_ATTR_PROTOCOL
	ipvsSvcAttrAddress       ipvsSvcAttr = 3  // IPVS_SVC_ATTR_ADDR
	ipvsSvcAttrPort          ipvsSvcAttr = 4  // IPVS_SVC_ATTR_PORT
	ipvsSvcAttrFWMark        ipvsSvcAttr = 5  // IPVS_SVC_ATTR_FWMARK
	ipvsSvcAttrSchedName     ipvsSvcAttr = 6  // IPVS_SVC_ATTR_SCHED_NAME
	ipvsSvcAttrFlags         ipvsSvcAttr = 7  // IPVS_SVC_ATTR_FLAGS
	ipvsSvcAttrTimeout       ipvsSvcAttr = 8  // IPVS_SVC_ATTR_TIMEOUT
	ipvsSvcAttrNetmask       ipvsSvcAttr = 9  // IPVS_SVC_ATTR_NETMASK
	ipvsSvcAttrStats         ipvsSvcAttr = 10 // IPVS_SVC_ATTR_STATS
	ipvsSvcAttrPEName        ipvsSvcAttr = 11 // IPVS_SVC_ATTR_PE_NAME
)

// IPVS destination attributes, nested in IPVS_CMD_ATTR_DEST.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L376-L409
type ipvsDestAttr int

const (
	ipvsDestAttrUnspec                ipvsDestAttr = 0  // IPVS_DEST_ATTR_UNSPEC
	ipvsDestAttrAddress               ipvsDestAttr = 1  // IPVS_DEST_ATTR_ADDR
	ipvsDestAttrPort                  ipvsDestAttr = 2  // IPVS_DEST_ATTR_PORT
	ipvsDestAttrForwardingMethod      ipvsDestAttr = 3  // IPVS_DEST_ATTR_FWD_METHOD
	ipvsDestAttrWeight                ipvsDestAttr = 4  // IPVS_DEST_ATTR_WEIGHT
	ipvsDestAttrUpperThreshold        ipvsDestAttr = 5  // IPVS_DEST_ATTR_U_THRESH
	ipvsDestAttrLowerThreshold        ipvsDestAttr = 6  // IPVS_DEST_ATTR_L_THRESH
	ipvsDestAttrActiveConnections     ipvsDestAttr = 7  // IPVS_DEST_ATTR_ACTIVE_CONNS
	ipvsDestAttrInactiveConnections   ipvsDestAttr = 8  // IPVS_DEST_ATTR_INACT_CONNS
	ipvsDestAttrPersistentConnections ipvsDestAttr = 9  // IPVS_DEST_ATTR_PERSIST_CONNS
	ipvsDestAttrStats                 ipvsDestAttr = 10 // IPVS_DEST_ATTR_STATS
	ipvsDestAttrAddressFamily         ipvsDestAttr = 11 // IPVS_DEST_ATTR_ADDR_FAMILY
)

// IPVS statistics attributes.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L433-L454
type ipvsStats int

const (
	ipvsStatsUnspec   ipvsStats = 0  // IPVS_STATS_ATTR_UNSPEC
	ipvsStatsConns    ipvsStats = 1  // IPVS_STATS_ATTR_CONNS
	ipvsStatsPktsIn   ipvsStats = 2  // IPVS_STATS_ATTR_INPKTS
	ipvsStatsPktsOut  ipvsStats = 3  // IPVS_STATS_ATTR_OUTPKTS
	ipvsStatsBytesIn  ipvsStats = 4  // IPVS_STATS_ATTR_INBYTES
	ipvsStatsBytesOut ipvsStats = 5  // IPVS_STATS_ATTR_OUTBYTES
	ipvsStatsCPS      ipvsStats = 6  // IPVS_STATS_ATTR_CPS
	ipvsStatsPPSIn    ipvsStats = 7  // IPVS_STATS_ATTR_INPPS
	ipvsStatsPPSOut   ipvsStats = 8  // IPVS_STATS_ATTR_OUTPPS
	ipvsStatsBPSIn    ipvsStats = 9  // IPVS_STATS_ATTR_INBPS
	ipvsStatsBPSOut   ipvsStats = 10 // IPVS_STATS_ATTR_OUTBPS
)

// Deprecated forwarding method names retained for compatibility.
const (
	// ConnectionFlagFwdMask is an alias for ConnFwdMask.
	// Deprecated: Use ConnFwdMask instead.
	ConnectionFlagFwdMask = ConnFwdMask

	// ConnectionFlagMasq is an alias for ConnFwdMasq.
	// Deprecated: Use ConnFwdMasq instead.
	ConnectionFlagMasq = ConnFwdMasq

	// ConnectionFlagLocalNode is an alias for ConnFwdLocalNode.
	// Deprecated: Use ConnFwdLocalNode instead.
	ConnectionFlagLocalNode = ConnFwdLocalNode

	// ConnectionFlagTunnel is an alias for ConnFwdTunnel.
	// Deprecated: Use ConnFwdTunnel instead.
	ConnectionFlagTunnel = ConnFwdTunnel

	// ConnectionFlagDirectRoute is an alias for ConnFwdDirectRoute.
	// Deprecated: Use ConnFwdDirectRoute instead.
	ConnectionFlagDirectRoute = ConnFwdDirectRoute
)

// IPVS scheduler names.
// These correspond to the names registered by the Linux IPVS schedulers.
// See https://github.com/torvalds/linux/tree/v7.2/net/netfilter/ipvs
const (
	// RoundRobin distributes jobs equally amongst the available
	// real servers.
	RoundRobin = "rr"

	// LeastConnection assigns more jobs to real servers with
	// fewer active jobs.
	LeastConnection = "lc"

	// DestinationHashing assigns jobs to servers through looking
	// up a statically assigned hash table by their destination IP
	// addresses.
	DestinationHashing = "dh"

	// SourceHashing assigns jobs to servers through looking up
	// a statically assigned hash table by their source IP
	// addresses.
	SourceHashing = "sh"

	// WeightedRoundRobin assigns jobs to real servers proportionally
	// to there real servers' weight. Servers with higher weights
	// receive new jobs first and get more jobs than servers
	// with lower weights. Servers with equal weights get
	// an equal distribution of new jobs
	WeightedRoundRobin = "wrr"

	// WeightedLeastConnection assigns more jobs to servers
	// with fewer jobs and relative to the real servers' weight
	WeightedLeastConnection = "wlc"
)

// Destination forwarding methods.
// See https://github.com/torvalds/linux/blob/v7.2/include/uapi/linux/ip_vs.h#L72-L132
const (
	// ConnFwdMask is the mask for the forwarding method bits.
	// Corresponds to IP_VS_CONN_F_FWD_MASK.
	ConnFwdMask = 0x0007

	// ConnFwdMasq denotes forwarding via masquerading/NAT.
	// Corresponds to IP_VS_CONN_F_MASQ.
	ConnFwdMasq = 0x0000

	// ConnFwdLocalNode denotes forwarding to a local node.
	// Corresponds to IP_VS_CONN_F_LOCALNODE.
	ConnFwdLocalNode = 0x0001

	// ConnFwdTunnel denotes forwarding via a tunnel.
	// Corresponds to IP_VS_CONN_F_TUNNEL.
	ConnFwdTunnel = 0x0002

	// ConnFwdDirectRoute denotes forwarding via direct routing.
	// Corresponds to IP_VS_CONN_F_DROUTE.
	ConnFwdDirectRoute = 0x0003

	// ConnFwdBypass denotes forwarding while bypassing the cache.
	// Corresponds to IP_VS_CONN_F_BYPASS.
	ConnFwdBypass = 0x0004
)

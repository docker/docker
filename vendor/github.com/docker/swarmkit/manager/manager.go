package manager

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/go-events"
	gmetrics "github.com/docker/go-metrics"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/ca"
	"github.com/docker/swarmkit/connectionbroker"
	"github.com/docker/swarmkit/identity"
	"github.com/docker/swarmkit/log"
	"github.com/docker/swarmkit/manager/allocator"
	"github.com/docker/swarmkit/manager/allocator/cnmallocator"
	"github.com/docker/swarmkit/manager/allocator/networkallocator"
	"github.com/docker/swarmkit/manager/controlapi"
	"github.com/docker/swarmkit/manager/csi"
	"github.com/docker/swarmkit/manager/dispatcher"
	"github.com/docker/swarmkit/manager/drivers"
	"github.com/docker/swarmkit/manager/health"
	"github.com/docker/swarmkit/manager/keymanager"
	"github.com/docker/swarmkit/manager/logbroker"
	"github.com/docker/swarmkit/manager/metrics"
	"github.com/docker/swarmkit/manager/orchestrator/constraintenforcer"
	"github.com/docker/swarmkit/manager/orchestrator/global"
	"github.com/docker/swarmkit/manager/orchestrator/jobs"
	"github.com/docker/swarmkit/manager/orchestrator/replicated"
	"github.com/docker/swarmkit/manager/orchestrator/taskreaper"
	"github.com/docker/swarmkit/manager/orchestrator/volumeenforcer"
	"github.com/docker/swarmkit/manager/resourceapi"
	"github.com/docker/swarmkit/manager/scheduler"
	"github.com/docker/swarmkit/manager/state/raft"
	"github.com/docker/swarmkit/manager/state/raft/transport"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/docker/swarmkit/manager/watchapi"
	"github.com/docker/swarmkit/remotes"
	"github.com/docker/swarmkit/xnet"
	gogotypes "github.com/gogo/protobuf/types"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// defaultTaskHistoryRetentionLimit is the number of tasks to keep.
	defaultTaskHistoryRetentionLimit = 5
)

// RemoteAddrs provides a listening address and an optional advertise address
// for serving the remote API.
type RemoteAddrs struct {
	// Address to bind
	ListenAddr string

	// Address to advertise to remote nodes (optional).
	AdvertiseAddr string
}

// Config is used to tune the Manager.
type Config struct {
	SecurityConfig *ca.SecurityConfig

	// RootCAPaths is the path to which new root certs should be save
	RootCAPaths ca.CertPaths

	// ExternalCAs is a list of initial CAs to which a manager node
	// will make certificate signing requests for node certificates.
	ExternalCAs []*api.ExternalCA

	// ControlAPI is an address for serving the control API.
	ControlAPI string

	// RemoteAPI is a listening address for serving the remote API, and
	// an optional advertise address.
	RemoteAPI *RemoteAddrs

	// JoinRaft is an optional address of a node in an existing raft
	// cluster to join.
	JoinRaft string

	// ForceJoin causes us to invoke raft's Join RPC even if already part
	// of a cluster.
	ForceJoin bool

	// StateDir is the top-level state directory
	StateDir string

	// ForceNewCluster defines if we have to force a new cluster
	// because we are recovering from a backup data directory.
	ForceNewCluster bool

	// ElectionTick defines the amount of ticks needed without
	// leader to trigger a new election
	ElectionTick uint32

	// HeartbeatTick defines the amount of ticks between each
	// heartbeat sent to other members for health-check purposes
	HeartbeatTick uint32

	// AutoLockManagers determines whether or not managers require an unlock key
	// when starting from a stopped state.  This configuration parameter is only
	// applicable when bootstrapping a new cluster for the first time.
	AutoLockManagers bool

	// UnlockKey is the key to unlock a node - used for decrypting manager TLS keys
	// as well as the raft data encryption key (DEK).  It is applicable when
	// bootstrapping a cluster for the first time (it's a cluster-wide setting),
	// and also when loading up any raft data on disk (as a KEK for the raft DEK).
	UnlockKey []byte

	// Availability allows a user to control the current scheduling status of a node
	Availability api.NodeSpec_Availability

	// PluginGetter provides access to docker's plugin inventory.
	PluginGetter plugingetter.PluginGetter

	// FIPS is a boolean stating whether the node is FIPS enabled - if this is the
	// first node in the cluster, this setting is used to set the cluster-wide mandatory
	// FIPS setting.
	FIPS bool

	// NetworkConfig stores network related config for the cluster
	NetworkConfig *cnmallocator.NetworkConfig
}

// Manager is the cluster manager for Swarm.
// This is the high-level object holding and initializing all the manager
// subsystems.
type Manager struct {
	config Config

	collector              *metrics.Collector
	caserver               *ca.Server
	dispatcher             *dispatcher.Dispatcher
	logbroker              *logbroker.LogBroker
	watchServer            *watchapi.Server
	replicatedOrchestrator *replicated.Orchestrator
	globalOrchestrator     *global.Orchestrator
	jobsOrchestrator       *jobs.Orchestrator
	taskReaper             *taskreaper.TaskReaper
	constraintEnforcer     *constraintenforcer.ConstraintEnforcer
	volumeEnforcer         *volumeenforcer.VolumeEnforcer
	scheduler              *scheduler.Scheduler
	allocator              *allocator.Allocator
	volumeManager          *csi.Manager
	keyManager             *keymanager.KeyManager
	server                 *grpc.Server
	localserver            *grpc.Server
	raftNode               *raft.Node
	dekRotator             *RaftDEKManager
	roleManager            *roleManager

	cancelFunc context.CancelFunc

	// mu is a general mutex used to coordinate starting/stopping and
	// leadership events.
	mu sync.Mutex
	// addrMu is a mutex that protects config.ControlAPI and config.RemoteAPI
	addrMu sync.Mutex

	started chan struct{}
	stopped bool

	remoteListener  chan net.Listener
	controlListener chan net.Listener
	errServe        chan error
}

var (
	leaderMetric gmetrics.Gauge
)

func init() {
	ns := gmetrics.NewNamespace("swarm", "manager", nil)
	leaderMetric = ns.NewGauge("leader", "Indicates if this manager node is a leader", "")
	gmetrics.Register(ns)
}

type closeOnceListener struct {
	once sync.Once
	net.Listener
}

func (l *closeOnceListener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.Listener.Close()
	})
	return err
}

// New creates a Manager which has not started to accept requests yet.
func New(config *Config) (*Manager, error) {
	err := os.MkdirAll(config.StateDir, 0700)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create state directory")
	}

	raftStateDir := filepath.Join(config.StateDir, "raft")
	err = os.MkdirAll(raftStateDir, 0700)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create raft state directory")
	}

	raftCfg := raft.DefaultNodeConfig()

	if config.ElectionTick > 0 {
		raftCfg.ElectionTick = int(config.ElectionTick)
	}
	if config.HeartbeatTick > 0 {
		raftCfg.HeartbeatTick = int(config.HeartbeatTick)
	}

	dekRotator, err := NewRaftDEKManager(config.SecurityConfig.KeyWriter(), config.FIPS)
	if err != nil {
		return nil, err
	}

	newNodeOpts := raft.NodeOptions{
		ID:              config.SecurityConfig.ClientTLSCreds.NodeID(),
		JoinAddr:        config.JoinRaft,
		ForceJoin:       config.ForceJoin,
		Config:          raftCfg,
		StateDir:        raftStateDir,
		ForceNewCluster: config.ForceNewCluster,
		TLSCredentials:  config.SecurityConfig.ClientTLSCreds,
		KeyRotator:      dekRotator,
		FIPS:            config.FIPS,
	}
	raftNode := raft.NewNode(newNodeOpts)

	// the interceptorWrappers are functions that wrap the prometheus grpc
	// interceptor, and add some of code to log errors locally. one for stream
	// and one for unary. this is needed because the grpc unary interceptor
	// doesn't natively do chaining, you have to implement it in the caller.
	// note that even though these are logging errors, we're still using
	// debug level. returning errors from GRPC methods is common and expected,
	// and logging an ERROR every time a user mistypes a service name would
	// pollute the logs really fast.
	//
	// NOTE(dperny): Because of the fact that these functions are very simple
	// in their operation and have no side effects other than the log output,
	// they are not automatically tested. If you modify them later, make _sure_
	// that they are correct. If you add substantial side effects, abstract
	// these out and test them!
	unaryInterceptorWrapper := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// pass the call down into the grpc_prometheus interceptor
		resp, err := grpc_prometheus.UnaryServerInterceptor(ctx, req, info, handler)
		if err != nil {
			log.G(ctx).WithField("rpc", info.FullMethod).WithError(err).Debug("error handling rpc")
		}
		return resp, err
	}

	streamInterceptorWrapper := func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// we can't re-write a stream context, so don't bother creating a
		// sub-context like in unary methods
		// pass the call down into the grpc_prometheus interceptor
		err := grpc_prometheus.StreamServerInterceptor(srv, ss, info, handler)
		if err != nil {
			log.G(ss.Context()).WithField("rpc", info.FullMethod).WithError(err).Debug("error handling streaming rpc")
		}
		return err
	}

	opts := []grpc.ServerOption{
		grpc.Creds(config.SecurityConfig.ServerTLSCreds),
		grpc.StreamInterceptor(streamInterceptorWrapper),
		grpc.UnaryInterceptor(unaryInterceptorWrapper),
		grpc.MaxRecvMsgSize(transport.GRPCMaxMsgSize),
	}

	m := &Manager{
		config:          *config,
		caserver:        ca.NewServer(raftNode.MemoryStore(), config.SecurityConfig),
		dispatcher:      dispatcher.New(),
		logbroker:       logbroker.New(raftNode.MemoryStore()),
		watchServer:     watchapi.NewServer(raftNode.MemoryStore()),
		server:          grpc.NewServer(opts...),
		localserver:     grpc.NewServer(opts...),
		raftNode:        raftNode,
		started:         make(chan struct{}),
		dekRotator:      dekRotator,
		remoteListener:  make(chan net.Listener, 1),
		controlListener: make(chan net.Listener, 1),
		errServe:        make(chan error, 2),
	}

	if config.ControlAPI != "" {
		m.config.ControlAPI = ""
		if err := m.BindControl(config.ControlAPI); err != nil {
			return nil, err
		}
	}

	if config.RemoteAPI != nil {
		m.config.RemoteAPI = nil
		// The context isn't used in this case (before (*Manager).Run).
		if err := m.BindRemote(context.Background(), *config.RemoteAPI); err != nil {
			if config.ControlAPI != "" {
				l := <-m.controlListener
				l.Close()
			}
			return nil, err
		}
	}

	return m, nil
}

// BindControl binds a local socket for the control API.
func (m *Manager) BindControl(addr string) error {
	m.addrMu.Lock()
	defer m.addrMu.Unlock()

	if m.config.ControlAPI != "" {
		return errors.New("manager already has a control API address")
	}

	// don't create a socket directory if we're on windows. we used named pipe
	if runtime.GOOS != "windows" {
		err := os.MkdirAll(filepath.Dir(addr), 0700)
		if err != nil {
			return errors.Wrap(err, "failed to create socket directory")
		}
	}

	l, err := xnet.ListenLocal(addr)

	// A unix socket may fail to bind if the file already
	// exists. Try replacing the file.
	if runtime.GOOS != "windows" {
		unwrappedErr := err
		if op, ok := unwrappedErr.(*net.OpError); ok {
			unwrappedErr = op.Err
		}
		if sys, ok := unwrappedErr.(*os.SyscallError); ok {
			unwrappedErr = sys.Err
		}
		if unwrappedErr == syscall.EADDRINUSE {
			os.Remove(addr)
			l, err = xnet.ListenLocal(addr)
		}
	}
	if err != nil {
		return errors.Wrap(err, "failed to listen on control API address")
	}

	m.config.ControlAPI = addr
	m.controlListener <- l
	return nil
}

// BindRemote binds a port for the remote API.
func (m *Manager) BindRemote(ctx context.Context, addrs RemoteAddrs) error {
	m.addrMu.Lock()
	defer m.addrMu.Unlock()

	if m.config.RemoteAPI != nil {
		return errors.New("manager already has remote API address")
	}

	// If an AdvertiseAddr was specified, we use that as our
	// externally-reachable address.
	advertiseAddr := addrs.AdvertiseAddr

	var advertiseAddrPort string
	if advertiseAddr == "" {
		// Otherwise, we know we are joining an existing swarm. Use a
		// wildcard address to trigger remote autodetection of our
		// address.
		var err error
		_, advertiseAddrPort, err = net.SplitHostPort(addrs.ListenAddr)
		if err != nil {
			return fmt.Errorf("missing or invalid listen address %s", addrs.ListenAddr)
		}

		// Even with an IPv6 listening address, it's okay to use
		// 0.0.0.0 here. Any "unspecified" (wildcard) IP will
		// be substituted with the actual source address.
		advertiseAddr = net.JoinHostPort("0.0.0.0", advertiseAddrPort)
	}

	l, err := net.Listen("tcp", addrs.ListenAddr)
	if err != nil {
		return errors.Wrap(err, "failed to listen on remote API address")
	}
	if advertiseAddrPort == "0" {
		advertiseAddr = l.Addr().String()
		addrs.ListenAddr = advertiseAddr
	}

	m.config.RemoteAPI = &addrs

	m.raftNode.SetAddr(ctx, advertiseAddr)
	m.remoteListener <- l

	return nil
}

// RemovedFromRaft returns a channel that's closed if the manager is removed
// from the raft cluster. This should be used to trigger a manager shutdown.
func (m *Manager) RemovedFromRaft() <-chan struct{} {
	return m.raftNode.RemovedFromRaft
}

// Addr returns tcp address on which remote api listens.
func (m *Manager) Addr() string {
	m.addrMu.Lock()
	defer m.addrMu.Unlock()

	if m.config.RemoteAPI == nil {
		return ""
	}
	return m.config.RemoteAPI.ListenAddr
}

// Run starts all manager sub-systems and the gRPC server at the configured
// address.
// The call never returns unless an error occurs or `Stop()` is called.
func (m *Manager) Run(parent context.Context) error {
	ctx, ctxCancel := context.WithCancel(parent)
	defer ctxCancel()

	m.cancelFunc = ctxCancel

	leadershipCh, cancel := m.raftNode.SubscribeLeadership()
	defer cancel()

	go m.handleLeadershipEvents(ctx, leadershipCh)

	authorize := func(ctx context.Context, roles []string) error {
		var (
			blacklistedCerts map[string]*api.BlacklistedCertificate
			clusters         []*api.Cluster
			err              error
		)

		m.raftNode.MemoryStore().View(func(readTx store.ReadTx) {
			clusters, err = store.FindClusters(readTx, store.ByName(store.DefaultClusterName))

		})

		// Not having a cluster object yet means we can't check
		// the blacklist.
		if err == nil && len(clusters) == 1 {
			blacklistedCerts = clusters[0].BlacklistedCertificates
		}

		// Authorize the remote roles, ensure they can only be forwarded by managers
		_, err = ca.AuthorizeForwardedRoleAndOrg(ctx, roles, []string{ca.ManagerRole}, m.config.SecurityConfig.ClientTLSCreds.Organization(), blacklistedCerts)
		return err
	}

	baseControlAPI := controlapi.NewServer(m.raftNode.MemoryStore(), m.raftNode, m.config.SecurityConfig, m.config.PluginGetter, drivers.New(m.config.PluginGetter))
	baseResourceAPI := resourceapi.New(m.raftNode.MemoryStore())
	healthServer := health.NewHealthServer()
	localHealthServer := health.NewHealthServer()

	authenticatedControlAPI := api.NewAuthenticatedWrapperControlServer(baseControlAPI, authorize)
	authenticatedWatchAPI := api.NewAuthenticatedWrapperWatchServer(m.watchServer, authorize)
	authenticatedResourceAPI := api.NewAuthenticatedWrapperResourceAllocatorServer(baseResourceAPI, authorize)
	authenticatedLogsServerAPI := api.NewAuthenticatedWrapperLogsServer(m.logbroker, authorize)
	authenticatedLogBrokerAPI := api.NewAuthenticatedWrapperLogBrokerServer(m.logbroker, authorize)
	authenticatedDispatcherAPI := api.NewAuthenticatedWrapperDispatcherServer(m.dispatcher, authorize)
	authenticatedCAAPI := api.NewAuthenticatedWrapperCAServer(m.caserver, authorize)
	authenticatedNodeCAAPI := api.NewAuthenticatedWrapperNodeCAServer(m.caserver, authorize)
	authenticatedRaftAPI := api.NewAuthenticatedWrapperRaftServer(m.raftNode, authorize)
	authenticatedHealthAPI := api.NewAuthenticatedWrapperHealthServer(healthServer, authorize)
	authenticatedRaftMembershipAPI := api.NewAuthenticatedWrapperRaftMembershipServer(m.raftNode, authorize)

	proxyDispatcherAPI := api.NewRaftProxyDispatcherServer(authenticatedDispatcherAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)
	proxyCAAPI := api.NewRaftProxyCAServer(authenticatedCAAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)
	proxyNodeCAAPI := api.NewRaftProxyNodeCAServer(authenticatedNodeCAAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)
	proxyRaftMembershipAPI := api.NewRaftProxyRaftMembershipServer(authenticatedRaftMembershipAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)
	proxyResourceAPI := api.NewRaftProxyResourceAllocatorServer(authenticatedResourceAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)
	proxyLogBrokerAPI := api.NewRaftProxyLogBrokerServer(authenticatedLogBrokerAPI, m.raftNode, nil, ca.WithMetadataForwardTLSInfo)

	// The following local proxies are only wired up to receive requests
	// from a trusted local socket, and these requests don't use TLS,
	// therefore the requests they handle locally should bypass
	// authorization. When requests are proxied from these servers, they
	// are sent as requests from this manager rather than forwarded
	// requests (it has no TLS information to put in the metadata map).
	forwardAsOwnRequest := func(ctx context.Context) (context.Context, error) { return ctx, nil }
	handleRequestLocally := func(ctx context.Context) (context.Context, error) {
		remoteAddr := "127.0.0.1:0"

		m.addrMu.Lock()
		if m.config.RemoteAPI != nil {
			if m.config.RemoteAPI.AdvertiseAddr != "" {
				remoteAddr = m.config.RemoteAPI.AdvertiseAddr
			} else {
				remoteAddr = m.config.RemoteAPI.ListenAddr
			}
		}
		m.addrMu.Unlock()

		creds := m.config.SecurityConfig.ClientTLSCreds

		nodeInfo := ca.RemoteNodeInfo{
			Roles:        []string{creds.Role()},
			Organization: creds.Organization(),
			NodeID:       creds.NodeID(),
			RemoteAddr:   remoteAddr,
		}

		return context.WithValue(ctx, ca.LocalRequestKey, nodeInfo), nil
	}
	localProxyControlAPI := api.NewRaftProxyControlServer(baseControlAPI, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyLogsAPI := api.NewRaftProxyLogsServer(m.logbroker, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyDispatcherAPI := api.NewRaftProxyDispatcherServer(m.dispatcher, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyCAAPI := api.NewRaftProxyCAServer(m.caserver, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyNodeCAAPI := api.NewRaftProxyNodeCAServer(m.caserver, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyResourceAPI := api.NewRaftProxyResourceAllocatorServer(baseResourceAPI, m.raftNode, handleRequestLocally, forwardAsOwnRequest)
	localProxyLogBrokerAPI := api.NewRaftProxyLogBrokerServer(m.logbroker, m.raftNode, handleRequestLocally, forwardAsOwnRequest)

	// Everything registered on m.server should be an authenticated
	// wrapper, or a proxy wrapping an authenticated wrapper!
	api.RegisterCAServer(m.server, proxyCAAPI)
	api.RegisterNodeCAServer(m.server, proxyNodeCAAPI)
	api.RegisterRaftServer(m.server, authenticatedRaftAPI)
	api.RegisterHealthServer(m.server, authenticatedHealthAPI)
	api.RegisterRaftMembershipServer(m.server, proxyRaftMembershipAPI)
	api.RegisterControlServer(m.server, authenticatedControlAPI)
	api.RegisterWatchServer(m.server, authenticatedWatchAPI)
	api.RegisterLogsServer(m.server, authenticatedLogsServerAPI)
	api.RegisterLogBrokerServer(m.server, proxyLogBrokerAPI)
	api.RegisterResourceAllocatorServer(m.server, proxyResourceAPI)
	api.RegisterDispatcherServer(m.server, proxyDispatcherAPI)
	grpc_prometheus.Register(m.server)

	api.RegisterControlServer(m.localserver, localProxyControlAPI)
	api.RegisterWatchServer(m.localserver, m.watchServer)
	api.RegisterLogsServer(m.localserver, localProxyLogsAPI)
	api.RegisterHealthServer(m.localserver, localHealthServer)
	api.RegisterDispatcherServer(m.localserver, localProxyDispatcherAPI)
	api.RegisterCAServer(m.localserver, localProxyCAAPI)
	api.RegisterNodeCAServer(m.localserver, localProxyNodeCAAPI)
	api.RegisterResourceAllocatorServer(m.localserver, localProxyResourceAPI)
	api.RegisterLogBrokerServer(m.localserver, localProxyLogBrokerAPI)
	grpc_prometheus.Register(m.localserver)

	healthServer.SetServingStatus("Raft", api.HealthCheckResponse_NOT_SERVING)
	localHealthServer.SetServingStatus("ControlAPI", api.HealthCheckResponse_NOT_SERVING)

	if err := m.watchServer.Start(ctx); err != nil {
		log.G(ctx).WithError(err).Error("watch server failed to start")
	}

	go m.serveListener(ctx, m.remoteListener)
	go m.serveListener(ctx, m.controlListener)

	defer func() {
		m.server.Stop()
		m.localserver.Stop()
	}()

	// Set the raft server as serving for the health server
	healthServer.SetServingStatus("Raft", api.HealthCheckResponse_SERVING)

	if err := m.raftNode.JoinAndStart(ctx); err != nil {
		// Don't block future calls to Stop.
		close(m.started)
		return errors.Wrap(err, "can't initialize raft node")
	}

	localHealthServer.SetServingStatus("ControlAPI", api.HealthCheckResponse_SERVING)

	// Start metrics collection.

	m.collector = metrics.NewCollector(m.raftNode.MemoryStore())
	go func(collector *metrics.Collector) {
		if err := collector.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("collector failed with an error")
		}
	}(m.collector)

	close(m.started)

	go func() {
		err := m.raftNode.Run(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Error("raft node stopped")
			m.Stop(ctx, false)
		}
	}()

	if err := raft.WaitForLeader(ctx, m.raftNode); err != nil {
		return err
	}

	c, err := raft.WaitForCluster(ctx, m.raftNode)
	if err != nil {
		return err
	}
	raftConfig := c.Spec.Raft

	if err := m.watchForClusterChanges(ctx); err != nil {
		return err
	}

	if int(raftConfig.ElectionTick) != m.raftNode.Config.ElectionTick {
		log.G(ctx).Warningf("election tick value (%ds) is different from the one defined in the cluster config (%vs), the cluster may be unstable", m.raftNode.Config.ElectionTick, raftConfig.ElectionTick)
	}
	if int(raftConfig.HeartbeatTick) != m.raftNode.Config.HeartbeatTick {
		log.G(ctx).Warningf("heartbeat tick value (%ds) is different from the one defined in the cluster config (%vs), the cluster may be unstable", m.raftNode.Config.HeartbeatTick, raftConfig.HeartbeatTick)
	}

	// wait for an error in serving.
	err = <-m.errServe
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	m.Stop(ctx, false)

	return err
}

const stopTimeout = 8 * time.Second

// Stop stops the manager. It immediately closes all open connections and
// active RPCs as well as stopping the manager's subsystems. If clearData is
// set, the raft logs, snapshots, and keys will be erased.
func (m *Manager) Stop(ctx context.Context, clearData bool) {
	log.G(ctx).Info("Stopping manager")
	// It's not safe to start shutting down while the manager is still
	// starting up.
	<-m.started

	// the mutex stops us from trying to stop while we're already stopping, or
	// from returning before we've finished stopping.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.stopped = true

	srvDone, localSrvDone := make(chan struct{}), make(chan struct{})
	go func() {
		m.server.GracefulStop()
		close(srvDone)
	}()
	go func() {
		m.localserver.GracefulStop()
		close(localSrvDone)
	}()

	m.raftNode.Cancel()

	if m.collector != nil {
		m.collector.Stop()
	}

	// The following components are gRPC services that are
	// registered when creating the manager and will need
	// to be re-registered if they are recreated.
	// For simplicity, they are not nilled out.
	m.dispatcher.Stop()
	m.logbroker.Stop()
	m.watchServer.Stop()
	m.caserver.Stop()

	if m.allocator != nil {
		m.allocator.Stop()
	}
	if m.replicatedOrchestrator != nil {
		m.replicatedOrchestrator.Stop()
	}
	if m.globalOrchestrator != nil {
		m.globalOrchestrator.Stop()
	}
	if m.jobsOrchestrator != nil {
		m.jobsOrchestrator.Stop()
	}
	if m.taskReaper != nil {
		m.taskReaper.Stop()
	}
	if m.constraintEnforcer != nil {
		m.constraintEnforcer.Stop()
	}
	if m.volumeEnforcer != nil {
		m.volumeEnforcer.Stop()
	}
	if m.scheduler != nil {
		m.scheduler.Stop()
	}
	if m.roleManager != nil {
		m.roleManager.Stop()
	}
	if m.keyManager != nil {
		m.keyManager.Stop()
	}

	if clearData {
		m.raftNode.ClearData()
	}
	m.cancelFunc()
	<-m.raftNode.Done()

	timer := time.AfterFunc(stopTimeout, func() {
		m.server.Stop()
		m.localserver.Stop()
	})
	defer timer.Stop()
	// TODO: we're not waiting on ctx because it very well could be passed from Run,
	// which is already cancelled here. We need to refactor that.
	select {
	case <-srvDone:
		<-localSrvDone
	case <-localSrvDone:
		<-srvDone
	}

	log.G(ctx).Info("Manager shut down")
	// mutex is released and Run can return now
}

func (m *Manager) updateKEK(ctx context.Context, cluster *api.Cluster) error {
	securityConfig := m.config.SecurityConfig
	nodeID := m.config.SecurityConfig.ClientTLSCreds.NodeID()
	logger := log.G(ctx).WithFields(logrus.Fields{
		"node.id":   nodeID,
		"node.role": ca.ManagerRole,
	})

	kekData := ca.KEKData{Version: cluster.Meta.Version.Index}
	for _, encryptionKey := range cluster.UnlockKeys {
		if encryptionKey.Subsystem == ca.ManagerRole {
			kekData.KEK = encryptionKey.Key
			break
		}
	}
	updated, unlockedToLocked, err := m.dekRotator.MaybeUpdateKEK(kekData)
	if err != nil {
		logger.WithError(err).Errorf("failed to re-encrypt TLS key with a new KEK")
		return err
	}
	if updated {
		logger.Debug("successfully rotated KEK")
	}
	if unlockedToLocked {
		// a best effort attempt to update the TLS certificate - if it fails, it'll be updated the next time it renews;
		// don't wait because it might take a bit
		go func() {
			insecureCreds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})

			conn, err := grpc.Dial(
				m.config.ControlAPI,
				grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
				grpc.WithStreamInterceptor(grpc_prometheus.StreamClientInterceptor),
				grpc.WithTransportCredentials(insecureCreds),
				grpc.WithDialer(
					func(addr string, timeout time.Duration) (net.Conn, error) {
						return xnet.DialTimeoutLocal(addr, timeout)
					}),
				grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)),
			)
			if err != nil {
				logger.WithError(err).Error("failed to connect to local manager socket after locking the cluster")
				return
			}

			defer conn.Close()

			connBroker := connectionbroker.New(remotes.NewRemotes())
			connBroker.SetLocalConn(conn)
			if err := ca.RenewTLSConfigNow(ctx, securityConfig, connBroker, m.config.RootCAPaths); err != nil {
				logger.WithError(err).Error("failed to download new TLS certificate after locking the cluster")
			}
		}()
	}
	return nil
}

func (m *Manager) watchForClusterChanges(ctx context.Context) error {
	clusterID := m.config.SecurityConfig.ClientTLSCreds.Organization()
	var cluster *api.Cluster
	clusterWatch, clusterWatchCancel, err := store.ViewAndWatch(m.raftNode.MemoryStore(),
		func(tx store.ReadTx) error {
			cluster = store.GetCluster(tx, clusterID)
			if cluster == nil {
				return fmt.Errorf("unable to get current cluster")
			}
			return nil
		},
		api.EventUpdateCluster{
			Cluster: &api.Cluster{ID: clusterID},
			Checks:  []api.ClusterCheckFunc{api.ClusterCheckID},
		},
	)
	if err != nil {
		return err
	}
	if err := m.updateKEK(ctx, cluster); err != nil {
		return err
	}

	go func() {
		for {
			select {
			case event := <-clusterWatch:
				clusterEvent := event.(api.EventUpdateCluster)
				m.updateKEK(ctx, clusterEvent.Cluster)
			case <-ctx.Done():
				clusterWatchCancel()
				return
			}
		}
	}()
	return nil
}

// getLeaderNodeID is a small helper function returning a string with the
// leader's node ID. it is only used for logging, and should not be relied on
// to give a node ID for actual operational purposes (because it returns errors
// as nicely decorated strings)
func (m *Manager) getLeaderNodeID() string {
	// get the current leader ID. this variable tracks the leader *only* for
	// the purposes of logging leadership changes, and should not be relied on
	// for other purposes
	leader, leaderErr := m.raftNode.Leader()
	switch leaderErr {
	case raft.ErrNoRaftMember:
		// this is an unlikely case, but we have to handle it. this means this
		// node is not a member of the raft quorum. this won't look very pretty
		// in logs ("leadership changed from aslkdjfa to ErrNoRaftMember") but
		// it also won't be very common
		return "not yet part of a raft cluster"
	case raft.ErrNoClusterLeader:
		return "no cluster leader"
	default:
		id, err := m.raftNode.GetNodeIDByRaftID(leader)
		// the only possible error here is "ErrMemberUnknown"
		if err != nil {
			return "an unknown node"
		}
		return id
	}
}

// handleLeadershipEvents handles the is leader event or is follower event.
func (m *Manager) handleLeadershipEvents(ctx context.Context, leadershipCh chan events.Event) {
	// get the current leader and save it for logging leadership changes in
	// this loop
	oldLeader := m.getLeaderNodeID()
	for {
		select {
		case leadershipEvent := <-leadershipCh:
			m.mu.Lock()
			if m.stopped {
				m.mu.Unlock()
				return
			}
			newState := leadershipEvent.(raft.LeadershipState)

			if newState == raft.IsLeader {
				m.becomeLeader(ctx)
				leaderMetric.Set(1)
			} else if newState == raft.IsFollower {
				m.becomeFollower()
				leaderMetric.Set(0)
			}
			m.mu.Unlock()

			newLeader := m.getLeaderNodeID()
			// maybe we should use logrus fields for old and new leader, so
			// that users are better able to ingest leadership changes into log
			// aggregators?
			log.G(ctx).Infof("leadership changed from %v to %v", oldLeader, newLeader)
		case <-ctx.Done():
			return
		}
	}
}

// serveListener serves a listener for local and non local connections.
func (m *Manager) serveListener(ctx context.Context, lCh <-chan net.Listener) {
	var l net.Listener
	select {
	case l = <-lCh:
	case <-ctx.Done():
		return
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(
		logrus.Fields{
			"proto": l.Addr().Network(),
			"addr":  l.Addr().String(),
		}))
	if _, ok := l.(*net.TCPListener); !ok {
		log.G(ctx).Info("Listening for local connections")
		// we need to disallow double closes because UnixListener.Close
		// can delete unix-socket file of newer listener. grpc calls
		// Close twice indeed: in Serve and in Stop.
		m.errServe <- m.localserver.Serve(&closeOnceListener{Listener: l})
	} else {
		log.G(ctx).Info("Listening for connections")
		m.errServe <- m.server.Serve(l)
	}
}

// becomeLeader starts the subsystems that are run on the leader.
func (m *Manager) becomeLeader(ctx context.Context) {
	s := m.raftNode.MemoryStore()

	rootCA := m.config.SecurityConfig.RootCA()
	nodeID := m.config.SecurityConfig.ClientTLSCreds.NodeID()

	raftCfg := raft.DefaultRaftConfig()
	raftCfg.ElectionTick = uint32(m.raftNode.Config.ElectionTick)
	raftCfg.HeartbeatTick = uint32(m.raftNode.Config.HeartbeatTick)

	clusterID := m.config.SecurityConfig.ClientTLSCreds.Organization()

	initialCAConfig := ca.DefaultCAConfig()
	initialCAConfig.ExternalCAs = m.config.ExternalCAs

	var (
		unlockKeys []*api.EncryptionKey
		err        error
	)
	if m.config.AutoLockManagers {
		unlockKeys = []*api.EncryptionKey{{
			Subsystem: ca.ManagerRole,
			Key:       m.config.UnlockKey,
		}}
	}
	s.Update(func(tx store.Tx) error {
		// Add a default cluster object to the
		// store. Don't check the error because
		// we expect this to fail unless this
		// is a brand new cluster.
		clusterObj := defaultClusterObject(
			clusterID,
			initialCAConfig,
			raftCfg,
			api.EncryptionConfig{AutoLockManagers: m.config.AutoLockManagers},
			unlockKeys,
			rootCA,
			m.config.FIPS,
			nil,
			0,
			0)

		// If defaultAddrPool is valid we update cluster object with new value
		// If VXLANUDPPort is not 0 then we call update cluster object with new value
		if m.config.NetworkConfig != nil {
			if m.config.NetworkConfig.DefaultAddrPool != nil {
				clusterObj.DefaultAddressPool = m.config.NetworkConfig.DefaultAddrPool
				clusterObj.SubnetSize = m.config.NetworkConfig.SubnetSize
			}

			if m.config.NetworkConfig.VXLANUDPPort != 0 {
				clusterObj.VXLANUDPPort = m.config.NetworkConfig.VXLANUDPPort
			}
		}
		err := store.CreateCluster(tx, clusterObj)

		if err != nil && (err != store.ErrExist || err != store.ErrNameConflict) {
			log.G(ctx).WithError(err).Errorf("error creating cluster object")
		}

		// Add Node entry for ourself, if one
		// doesn't exist already.
		freshCluster := nil == store.CreateNode(tx, managerNode(nodeID, m.config.Availability, clusterObj.VXLANUDPPort))

		if freshCluster {
			// This is a fresh swarm cluster. Add to store now any initial
			// cluster resource, like the default ingress network which
			// provides the routing mesh for this cluster.
			log.G(ctx).Info("Creating default ingress network")
			if err := store.CreateNetwork(tx, newIngressNetwork()); err != nil {
				log.G(ctx).WithError(err).Error("failed to create default ingress network")
			}
		}
		// Create now the static predefined if the store does not contain predefined
		// networks like bridge/host node-local networks which
		// are known to be present in each cluster node. This is needed
		// in order to allow running services on the predefined docker
		// networks like `bridge` and `host`.
		for _, p := range allocator.PredefinedNetworks() {
			if err := store.CreateNetwork(tx, newPredefinedNetwork(p.Name, p.Driver)); err != nil && err != store.ErrNameConflict {
				log.G(ctx).WithError(err).Error("failed to create predefined network " + p.Name)
			}
		}
		return nil
	})

	m.replicatedOrchestrator = replicated.NewReplicatedOrchestrator(s)
	m.constraintEnforcer = constraintenforcer.New(s)
	m.volumeEnforcer = volumeenforcer.New(s)
	m.globalOrchestrator = global.NewGlobalOrchestrator(s)
	m.jobsOrchestrator = jobs.NewOrchestrator(s)
	m.taskReaper = taskreaper.New(s)
	m.scheduler = scheduler.New(s)
	m.keyManager = keymanager.New(s, keymanager.DefaultConfig())
	m.roleManager = newRoleManager(s, m.raftNode)
	m.volumeManager = csi.NewManager(s)

	// TODO(stevvooe): Allocate a context that can be used to
	// shutdown underlying manager processes when leadership isTestUpdaterRollback
	// lost.

	// If DefaultAddrPool is null, Read from store and check if
	// DefaultAddrPool info is stored in cluster object
	// If VXLANUDPPort is 0, read it from the store - cluster object
	if m.config.NetworkConfig == nil || m.config.NetworkConfig.DefaultAddrPool == nil || m.config.NetworkConfig.VXLANUDPPort == 0 {
		var cluster *api.Cluster
		s.View(func(tx store.ReadTx) {
			cluster = store.GetCluster(tx, clusterID)
		})
		if cluster.DefaultAddressPool != nil {
			if m.config.NetworkConfig == nil {
				m.config.NetworkConfig = &cnmallocator.NetworkConfig{}
			}
			m.config.NetworkConfig.DefaultAddrPool = append(m.config.NetworkConfig.DefaultAddrPool, cluster.DefaultAddressPool...)
			m.config.NetworkConfig.SubnetSize = cluster.SubnetSize
		}
		if cluster.VXLANUDPPort != 0 {
			if m.config.NetworkConfig == nil {
				m.config.NetworkConfig = &cnmallocator.NetworkConfig{}
			}
			m.config.NetworkConfig.VXLANUDPPort = cluster.VXLANUDPPort
		}
	}

	m.allocator, err = allocator.New(s, m.config.PluginGetter, m.config.NetworkConfig)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create allocator")
		// TODO(stevvooe): It doesn't seem correct here to fail
		// creating the allocator but then use it anyway.
	}

	if m.keyManager != nil {
		go func(keyManager *keymanager.KeyManager) {
			if err := keyManager.Run(ctx); err != nil {
				log.G(ctx).WithError(err).Error("keymanager failed with an error")
			}
		}(m.keyManager)
	}

	go func(d *dispatcher.Dispatcher) {
		// Initialize the dispatcher.
		var cluster *api.Cluster
		s.View(func(tx store.ReadTx) {
			cluster = store.GetCluster(tx, clusterID)
		})
		var defaultConfig = dispatcher.DefaultConfig()
		heartbeatPeriod, err := gogotypes.DurationFromProto(cluster.Spec.Dispatcher.HeartbeatPeriod)
		if err == nil {
			defaultConfig.HeartbeatPeriod = heartbeatPeriod
		}
		d.Init(m.raftNode, defaultConfig, drivers.New(m.config.PluginGetter), m.config.SecurityConfig)
		if err := d.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("Dispatcher exited with an error")
		}
	}(m.dispatcher)

	if err := m.logbroker.Start(ctx); err != nil {
		log.G(ctx).WithError(err).Error("LogBroker failed to start")
	}

	go func(server *ca.Server) {
		if err := server.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("CA signer exited with an error")
		}
	}(m.caserver)

	// Start all sub-components in separate goroutines.
	// TODO(aluzzardi): This should have some kind of error handling so that
	// any component that goes down would bring the entire manager down.
	if m.allocator != nil {
		go func(allocator *allocator.Allocator) {
			if err := allocator.Run(ctx); err != nil {
				log.G(ctx).WithError(err).Error("allocator exited with an error")
			}
		}(m.allocator)
	}

	go func(scheduler *scheduler.Scheduler) {
		if err := scheduler.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("scheduler exited with an error")
		}
	}(m.scheduler)

	go func(constraintEnforcer *constraintenforcer.ConstraintEnforcer) {
		constraintEnforcer.Run()
	}(m.constraintEnforcer)

	go func(volumeEnforcer *volumeenforcer.VolumeEnforcer) {
		volumeEnforcer.Run()
	}(m.volumeEnforcer)

	go func(taskReaper *taskreaper.TaskReaper) {
		taskReaper.Run(ctx)
	}(m.taskReaper)

	go func(orchestrator *replicated.Orchestrator) {
		if err := orchestrator.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("replicated orchestrator exited with an error")
		}
	}(m.replicatedOrchestrator)

	go func(orchestrator *jobs.Orchestrator) {
		// jobs orchestrator does not return errors.
		orchestrator.Run(ctx)
	}(m.jobsOrchestrator)

	go func(globalOrchestrator *global.Orchestrator) {
		if err := globalOrchestrator.Run(ctx); err != nil {
			log.G(ctx).WithError(err).Error("global orchestrator exited with an error")
		}
	}(m.globalOrchestrator)

	go func(roleManager *roleManager) {
		roleManager.Run(ctx)
	}(m.roleManager)

	go func(volumeManager *csi.Manager) {
		volumeManager.Run(ctx)
	}(m.volumeManager)
}

// becomeFollower shuts down the subsystems that are only run by the leader.
func (m *Manager) becomeFollower() {
	// The following components are gRPC services that are
	// registered when creating the manager and will need
	// to be re-registered if they are recreated.
	// For simplicity, they are not nilled out.
	m.dispatcher.Stop()
	m.logbroker.Stop()
	m.caserver.Stop()

	if m.allocator != nil {
		m.allocator.Stop()
		m.allocator = nil
	}

	m.constraintEnforcer.Stop()
	m.constraintEnforcer = nil

	m.volumeEnforcer.Stop()
	m.volumeEnforcer = nil

	m.replicatedOrchestrator.Stop()
	m.replicatedOrchestrator = nil

	m.globalOrchestrator.Stop()
	m.globalOrchestrator = nil

	m.taskReaper.Stop()
	m.taskReaper = nil

	m.scheduler.Stop()
	m.scheduler = nil

	m.roleManager.Stop()
	m.roleManager = nil

	if m.keyManager != nil {
		m.keyManager.Stop()
		m.keyManager = nil
	}

	m.volumeManager.Stop()
	m.volumeManager = nil
}

// defaultClusterObject creates a default cluster.
func defaultClusterObject(
	clusterID string,
	initialCAConfig api.CAConfig,
	raftCfg api.RaftConfig,
	encryptionConfig api.EncryptionConfig,
	initialUnlockKeys []*api.EncryptionKey,
	rootCA *ca.RootCA,
	fips bool,
	defaultAddressPool []string,
	subnetSize uint32,
	vxlanUDPPort uint32) *api.Cluster {
	var caKey []byte
	if rcaSigner, err := rootCA.Signer(); err == nil {
		caKey = rcaSigner.Key
	}

	return &api.Cluster{
		ID: clusterID,
		Spec: api.ClusterSpec{
			Annotations: api.Annotations{
				Name: store.DefaultClusterName,
			},
			Orchestration: api.OrchestrationConfig{
				TaskHistoryRetentionLimit: defaultTaskHistoryRetentionLimit,
			},
			Dispatcher: api.DispatcherConfig{
				HeartbeatPeriod: gogotypes.DurationProto(dispatcher.DefaultHeartBeatPeriod),
			},
			Raft:             raftCfg,
			CAConfig:         initialCAConfig,
			EncryptionConfig: encryptionConfig,
		},
		RootCA: api.RootCA{
			CAKey:      caKey,
			CACert:     rootCA.Certs,
			CACertHash: rootCA.Digest.String(),
			JoinTokens: api.JoinTokens{
				Worker:  ca.GenerateJoinToken(rootCA, fips),
				Manager: ca.GenerateJoinToken(rootCA, fips),
			},
		},
		UnlockKeys:         initialUnlockKeys,
		FIPS:               fips,
		DefaultAddressPool: defaultAddressPool,
		SubnetSize:         subnetSize,
		VXLANUDPPort:       vxlanUDPPort,
	}
}

// managerNode creates a new node with NodeRoleManager role.
func managerNode(nodeID string, availability api.NodeSpec_Availability, vxlanPort uint32) *api.Node {
	return &api.Node{
		ID: nodeID,
		Certificate: api.Certificate{
			CN:   nodeID,
			Role: api.NodeRoleManager,
			Status: api.IssuanceStatus{
				State: api.IssuanceStateIssued,
			},
		},
		Spec: api.NodeSpec{
			DesiredRole:  api.NodeRoleManager,
			Membership:   api.NodeMembershipAccepted,
			Availability: availability,
		},
		VXLANUDPPort: vxlanPort,
	}
}

// newIngressNetwork returns the network object for the default ingress
// network, the network which provides the routing mesh. Caller will save to
// store this object once, at fresh cluster creation. It is expected to
// call this function inside a store update transaction.
func newIngressNetwork() *api.Network {
	return &api.Network{
		ID: identity.NewID(),
		Spec: api.NetworkSpec{
			Ingress: true,
			Annotations: api.Annotations{
				Name: "ingress",
			},
			DriverConfig: &api.Driver{},
			IPAM: &api.IPAMOptions{
				Driver:  &api.Driver{},
				Configs: []*api.IPAMConfig{},
			},
		},
	}
}

// Creates a network object representing one of the predefined networks
// known to be statically created on the cluster nodes. These objects
// are populated in the store at cluster creation solely in order to
// support running services on the nodes' predefined networks.
// External clients can filter these predefined networks by looking
// at the predefined label.
func newPredefinedNetwork(name, driver string) *api.Network {
	return &api.Network{
		ID: identity.NewID(),
		Spec: api.NetworkSpec{
			Annotations: api.Annotations{
				Name: name,
				Labels: map[string]string{
					networkallocator.PredefinedLabel: "true",
				},
			},
			DriverConfig: &api.Driver{Name: driver},
		},
	}
}

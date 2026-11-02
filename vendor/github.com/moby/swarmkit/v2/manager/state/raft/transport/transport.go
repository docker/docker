// Package transport provides grpc transport layer for raft.
// All methods are non-blocking.
package transport

import (
	"context"
	"math"
	"net"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/moby/swarmkit/v2/internal/grpcmetrics"
	"github.com/moby/swarmkit/v2/log"
)

// ErrIsNotFound indicates that peer was never added to transport.
var ErrIsNotFound = errors.New("peer not found")

// Raft is interface which represents Raft API for transport package.
type Raft interface {
	ReportUnreachable(id uint64)
	ReportSnapshot(id uint64, status raft.SnapshotStatus)
	IsIDRemoved(id uint64) bool
	UpdateNode(id uint64, addr string)

	NodeRemoved()
}

// Config for Transport
type Config struct {
	HeartbeatInterval time.Duration
	SendTimeout       time.Duration
	Credentials       credentials.TransportCredentials
	RaftID            string

	Raft
}

// Transport is structure which manages remote raft peers and sends messages
// to them.
type Transport struct {
	config *Config

	unknownc chan raftpb.Message

	mu      sync.Mutex
	peers   map[uint64]*peer
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	deferredConns map[*grpc.ClientConn]*time.Timer
}

// New returns new Transport with specified Config.
func New(cfg *Config) *Transport {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.RaftID != "" {
		ctx = log.WithField(ctx, "raft_id", cfg.RaftID)
	}
	t := &Transport{
		peers:    make(map[uint64]*peer),
		config:   cfg,
		unknownc: make(chan raftpb.Message),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,

		deferredConns: make(map[*grpc.ClientConn]*time.Timer),
	}
	go t.run(ctx)
	return t
}

func (t *Transport) run(ctx context.Context) {
	defer func() {
		log.G(ctx).Debug("stop transport")
		t.mu.Lock()
		defer t.mu.Unlock()
		t.stopped = true
		for _, p := range t.peers {
			p.stop()
			p.cc.Close()
		}
		for cc, timer := range t.deferredConns {
			timer.Stop()
			cc.Close()
		}
		t.deferredConns = nil
		close(t.done)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case m := <-t.unknownc:
			if err := t.sendUnknownMessage(ctx, m); err != nil {
				log.G(ctx).WithError(err).Warnf("ignored message %s to unknown peer %x", m.Type, m.To)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops transport and waits until it finished
func (t *Transport) Stop() {
	t.cancel()
	<-t.done
}

// Send sends raft message to remote peers.
func (t *Transport) Send(m raftpb.Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return errors.New("transport stopped")
	}
	if t.config.IsIDRemoved(m.To) {
		return errors.Errorf("refusing to send message %s to removed member %x", m.Type, m.To)
	}
	p, ok := t.peers[m.To]
	if !ok {
		log.G(t.ctx).Warningf("sending message %s to an unrecognized member ID %x", m.Type, m.To)
		select {
		// we need to process messages to unknown peers in separate goroutine
		// to not block sender
		case t.unknownc <- m:
		case <-t.ctx.Done():
			return t.ctx.Err()
		default:
			return errors.New("unknown messages queue is full")
		}
		return nil
	}
	if err := p.send(m); err != nil {
		return errors.Wrapf(err, "failed to send message %x to %x", m.Type, m.To)
	}
	return nil
}

// AddPeer adds new peer with id and address addr to Transport.
// If there is already peer with such id in Transport it will return error if
// address is different (UpdatePeer should be used) or nil otherwise.
func (t *Transport) AddPeer(id uint64, addr string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return errors.New("transport stopped")
	}
	if ep, ok := t.peers[id]; ok {
		if ep.address() == addr {
			return nil
		}
		return errors.Errorf("peer %x already added with addr %s", id, ep.addr)
	}
	log.G(t.ctx).Debugf("transport: add peer %x with address %s", id, addr)
	p, err := newPeer(id, addr, t)
	if err != nil {
		return errors.Wrapf(err, "failed to create peer %x with addr %s", id, addr)
	}
	t.peers[id] = p
	return nil
}

// RemovePeer removes peer from Transport and wait for it to stop.
func (t *Transport) RemovePeer(id uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return errors.New("transport stopped")
	}
	p, ok := t.peers[id]
	if !ok {
		return ErrIsNotFound
	}
	delete(t.peers, id)
	cc := p.conn()
	p.stop()
	timer := time.AfterFunc(8*time.Second, func() {
		t.mu.Lock()
		if !t.stopped {
			delete(t.deferredConns, cc)
			cc.Close()
		}
		t.mu.Unlock()
	})
	// store connection and timer for cleaning up on stop
	t.deferredConns[cc] = timer

	return nil
}

// UpdatePeer updates peer with new address. It replaces connection immediately.
func (t *Transport) UpdatePeer(id uint64, addr string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return errors.New("transport stopped")
	}
	p, ok := t.peers[id]
	if !ok {
		return ErrIsNotFound
	}
	if err := p.update(addr); err != nil {
		return err
	}
	log.G(t.ctx).Debugf("peer %x updated to address %s", id, addr)
	return nil
}

// UpdatePeerAddr updates peer with new address, but delays connection creation.
// New address won't be used until first failure on old address.
func (t *Transport) UpdatePeerAddr(id uint64, addr string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return errors.New("transport stopped")
	}
	p, ok := t.peers[id]
	if !ok {
		return ErrIsNotFound
	}
	return p.updateAddr(addr)
}

// PeerConn returns raw grpc connection to peer.
func (t *Transport) PeerConn(id uint64) (*grpc.ClientConn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.peers[id]
	if !ok {
		return nil, ErrIsNotFound
	}
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if !active {
		return nil, errors.New("peer is inactive")
	}
	return p.conn(), nil
}

// PeerAddr returns address of peer with id.
func (t *Transport) PeerAddr(id uint64) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.peers[id]
	if !ok {
		return "", ErrIsNotFound
	}
	return p.address(), nil
}

// HealthCheck checks health of particular peer.
func (t *Transport) HealthCheck(ctx context.Context, id uint64) error {
	t.mu.Lock()
	p, ok := t.peers[id]
	t.mu.Unlock()
	if !ok {
		return ErrIsNotFound
	}
	ctx, cancel := t.withContext(ctx)
	defer cancel()
	return p.healthCheck(ctx)
}

// Active returns true if node was recently active and false otherwise.
func (t *Transport) Active(id uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.peers[id]
	if !ok {
		return false
	}
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	return active
}

// LongestActive returns the ID of the peer that has been active for the longest
// length of time.
func (t *Transport) LongestActive() (uint64, error) {
	p, err := t.longestActive()
	if err != nil {
		return 0, err
	}

	return p.id, nil
}

// longestActive returns the peer that has been active for the longest length of
// time.
func (t *Transport) longestActive() (*peer, error) {
	var longest *peer
	var longestTime time.Time
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.peers {
		becameActive := p.activeTime()
		if becameActive.IsZero() {
			continue
		}
		if longest == nil {
			longest = p
			continue
		}
		if becameActive.Before(longestTime) {
			longest = p
			longestTime = becameActive
		}
	}
	if longest == nil {
		return nil, errors.New("failed to find longest active peer")
	}
	return longest, nil
}

func (t *Transport) dial(addr string) (*grpc.ClientConn, error) {
	grpcOptions := []grpc.DialOption{
		grpc.WithUnaryInterceptor(grpcmetrics.Client.UnaryClientInterceptor()),
		grpc.WithStreamInterceptor(grpcmetrics.Client.StreamClientInterceptor()),
		grpc.WithBackoffMaxDelay(8 * time.Second),
	}
	if t.config.Credentials != nil {
		grpcOptions = append(grpcOptions, grpc.WithTransportCredentials(t.config.Credentials))
	} else {
		grpcOptions = append(grpcOptions, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if t.config.SendTimeout > 0 {
		grpcOptions = append(grpcOptions, grpc.WithTimeout(t.config.SendTimeout))
	}

	// gRPC dialer connects to proxy first. Provide a custom dialer here avoid that.
	// TODO(anshul) Add an option to configure this.
	grpcOptions = append(grpcOptions,
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, timeout)
		}))

	// TODO(dperny): this changes the max received message size for outgoing
	// client connections. this means if the server sends a message larger than
	// this, we will still accept and unmarshal it. i'm unsure what the
	// potential consequences are of setting this to be effectively unbounded,
	// so after docker/swarmkit#2774 is fixed, we should remove this option
	grpcOptions = append(grpcOptions, grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(math.MaxInt32),
	))

	cc, err := grpc.Dial(addr, grpcOptions...)
	if err != nil {
		return nil, err
	}

	return cc, nil
}

func (t *Transport) withContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		select {
		case <-ctx.Done():
		case <-t.ctx.Done():
			cancel()
		}
	}()
	return ctx, cancel
}

func (t *Transport) resolvePeer(ctx context.Context, id uint64) (*peer, error) {
	longestActive, err := t.longestActive()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.config.SendTimeout)
	defer cancel()
	addr, err := longestActive.resolveAddr(ctx, id)
	if err != nil {
		return nil, err
	}
	return newPeer(id, addr, t)
}

func (t *Transport) sendUnknownMessage(ctx context.Context, m raftpb.Message) error {
	p, err := t.resolvePeer(ctx, m.To)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve peer")
	}
	defer p.cancel()
	if err := p.sendProcessMessage(ctx, m); err != nil {
		return errors.Wrapf(err, "failed to send message")
	}
	return nil
}

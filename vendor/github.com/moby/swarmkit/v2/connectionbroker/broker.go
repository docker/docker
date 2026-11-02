// Package connectionbroker is a layer on top of remotes that returns
// a gRPC connection to a manager. The connection may be a local connection
// using a local socket such as a UNIX socket.
package connectionbroker

import (
	"net"
	"sync"
	"time"

	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/internal/grpcmetrics"
	"github.com/moby/swarmkit/v2/remotes"
	"google.golang.org/grpc"
)

// Broker is a simple connection broker. It can either return a fresh
// connection to a remote manager selected with weighted randomization, or a
// local gRPC connection to the local manager.
type Broker struct {
	mu        sync.Mutex
	remotes   remotes.Remotes
	localConn *grpc.ClientConn
}

// New creates a new connection broker.
func New(remotes remotes.Remotes) *Broker {
	return &Broker{
		remotes: remotes,
	}
}

// SetLocalConn changes the local gRPC connection used by the connection broker.
func (b *Broker) SetLocalConn(localConn *grpc.ClientConn) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.localConn = localConn
}

// Select a manager from the set of available managers, and return a connection.
func (b *Broker) Select(dialOpts ...grpc.DialOption) (*Conn, error) {
	b.mu.Lock()
	localConn := b.localConn
	b.mu.Unlock()

	if localConn != nil {
		return &Conn{
			ClientConn: localConn,
			isLocal:    true,
		}, nil
	}

	return b.SelectRemote(dialOpts...)
}

// SelectRemote chooses a manager from the remotes, and returns a TCP
// connection.
func (b *Broker) SelectRemote(dialOpts ...grpc.DialOption) (*Conn, error) {
	peer, err := b.remotes.Select()

	if err != nil {
		return nil, err
	}

	// gRPC dialer connects to proxy first. Provide a custom dialer here avoid that.
	// TODO(anshul) Add an option to configure this.
	dialOpts = append(dialOpts,
		grpc.WithUnaryInterceptor(grpcmetrics.Client.UnaryClientInterceptor()),
		grpc.WithStreamInterceptor(grpcmetrics.Client.StreamClientInterceptor()),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, timeout)
		}))

	cc, err := grpc.Dial(peer.Addr, dialOpts...)
	if err != nil {
		b.remotes.ObserveIfExists(peer, -remotes.DefaultObservationWeight)
		return nil, err
	}

	return &Conn{
		ClientConn: cc,
		remotes:    b.remotes,
		peer:       peer,
	}, nil
}

// Remotes returns the remotes interface used by the broker, so the caller
// can make observations or see weights directly.
func (b *Broker) Remotes() remotes.Remotes {
	return b.remotes
}

// Conn is a wrapper around a gRPC client connection.
type Conn struct {
	*grpc.ClientConn
	isLocal bool
	remotes remotes.Remotes
	peer    api.Peer
}

// Peer returns the peer for this Conn.
func (c *Conn) Peer() api.Peer {
	return c.peer
}

// Close closes the client connection if it is a remote connection. It also
// records a positive experience with the remote peer if success is true,
// otherwise it records a negative experience. If a local connection is in use,
// Close is a noop.
func (c *Conn) Close(success bool) error {
	if c.isLocal {
		return nil
	}

	if success {
		c.remotes.ObserveIfExists(c.peer, remotes.DefaultObservationWeight)
	} else {
		c.remotes.ObserveIfExists(c.peer, -remotes.DefaultObservationWeight)
	}

	return c.ClientConn.Close()
}

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package multiplex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	nrinet "github.com/containerd/nri/pkg/net"
	"github.com/containerd/ttrpc"
)

// Mux multiplexes several logical connections over a single net.Conn.
//
// Connections are identified within a Mux by ConnIDs which are simple
// 32-bit unsigned integers. Opening a connection returns a net.Conn
// corrponding to the ConnID. This can then be used to write and read
// data through the connection with the Mux performing multiplexing
// and demultiplexing of data.
//
// Writing to a connection is fully synchronous. The caller can safely
// reuse the buffer once the call returns. Reading from a connection
// returns the oldest demultiplexed buffer for the connection, blocking
// if the connections incoming queue is empty. If any incoming queue is
// ever overflown the underlying trunk and all multiplexed connections
// are closed and an error is recorded. This error is later returned by
// any subsequent read from any connection. All connections of the Mux
// have the same fixed incoming queue length which can be configured
// using the WithReadQueueLength Option during Mux creation.
//
// The Mux interface also provides functions that emulate net.Dial and
// net.Listen for a connection. Usually these can be used for passing
// multiplexed connections to packages that insist to Dial or Accept
// themselves for connection establishment.
//
// Note that opening a connection is a virtual operation in the sense
// that it has no effects outside the Mux. It is performed without any
// signalling or other communication. It merely acquires the net.Conn
// corresponding to the connection and blindly assumes that the same
// ConnID is or will be opened at the other end of the Mux.
type Mux interface {
	// Open the connection for the given ConnID.
	Open(ConnID) (net.Conn, error)

	// Close the Mux and all connections associated with it.
	Close() error

	// Dialer returns a net.Dial-like function for the connection.
	//
	// Calling the returned function (with arguments) will return a
	// net.Conn for the connection.
	Dialer(ConnID) func(string, string) (net.Conn, error)

	// Listener returns a net.Listener for the connection. The first
	// call to Accept() on the listener will return a net.Conn for the
	// connection. Subsequent calls to Accept() will block until the
	// connection is closed then return io.EOF.
	Listen(ConnID) (net.Listener, error)

	// Trunk returns the trunk connection for the Mux.
	Trunk() net.Conn

	// Unblock unblocks the Mux reader.
	Unblock()
}

// ConnID uniquely identifies a logical connection within a Mux.
type ConnID uint32

const (
	// ConnID 0 is reserved for future use.
	reservedConnID ConnID = iota
	// LowestConnID is the lowest externally usable ConnID.
	LowestConnID
)

// Option to apply to a Mux.
type Option func(*mux)

// WithBlockedRead causes the Mux to be blocked for reading until gets Unblock()'ed.
func WithBlockedRead() Option {
	return func(m *mux) {
		if m.blockC == nil {
			m.blockC = make(chan struct{})
		}
	}
}

// WithReadQueueLength overrides the default read queue size.
func WithReadQueueLength(length int) Option {
	return func(m *mux) {
		m.qlen = length
	}
}

// Multiplex returns a multiplexer for the given connection.
func Multiplex(trunk net.Conn, options ...Option) Mux {
	return newMux(trunk, options...)
}

// mux is our implementation of Mux.
type mux struct {
	trunk     net.Conn
	writeLock sync.Mutex
	conns     map[ConnID]*conn
	connLock  sync.RWMutex
	qlen      int
	errOnce   sync.Once
	err       error
	unblkOnce sync.Once
	blockC    chan struct{}
	closeOnce sync.Once
	doneC     chan struct{}
}

const (
	// default read queue length for a single connection
	readQueueLen = 256
	// length of frame header: 4-byte ConnID, 4-byte payload length
	headerLen = 8
	// max. allowed payload size
	maxPayloadSize = ttrpcMessageHeaderLength + ttrpcMessageLengthMax
)

// conn represents a single multiplexed connection.
type conn struct {
	id        ConnID
	mux       *mux
	readC     chan []byte
	closeOnce sync.Once
	doneC     chan error
}

func newMux(trunk net.Conn, options ...Option) *mux {
	m := &mux{
		trunk: trunk,
		conns: make(map[ConnID]*conn),
		qlen:  readQueueLen,
		doneC: make(chan struct{}),
	}

	for _, o := range options {
		o(m)
	}

	if m.blockC == nil {
		WithBlockedRead()(m)
		m.Unblock()
	}

	go m.reader()

	return m
}

func (m *mux) Trunk() net.Conn {
	return m.trunk
}

func (m *mux) Unblock() {
	m.unblkOnce.Do(func() {
		close(m.blockC)
	})
}

func (m *mux) Open(id ConnID) (net.Conn, error) {
	if id == reservedConnID {
		return nil, fmt.Errorf("ConnID %d is reserved", id)
	}

	m.connLock.Lock()
	defer m.connLock.Unlock()

	c, ok := m.conns[id]
	if !ok {
		c = &conn{
			id:    id,
			mux:   m,
			doneC: make(chan error, 1),
			readC: make(chan []byte, m.qlen),
		}
		m.conns[id] = c
	}

	return c, nil
}

func (m *mux) Close() error {
	m.closeOnce.Do(func() {
		m.connLock.Lock()
		defer m.connLock.Unlock()
		for _, conn := range m.conns {
			conn.close()
		}
		close(m.doneC)
		m.trunk.Close()
	})

	return nil
}

func (m *mux) Dialer(id ConnID) func(string, string) (net.Conn, error) {
	return func(string, string) (net.Conn, error) {
		return m.Open(id)
	}
}

func (m *mux) Listen(id ConnID) (net.Listener, error) {
	conn, err := m.Open(id)
	if err != nil {
		return nil, err
	}
	return nrinet.NewConnListener(conn), nil
}

func (m *mux) write(id ConnID, buf []byte) (int, error) {
	var (
		hdr  [headerLen]byte
		data = buf[:]
		size = len(data)
	)

	m.writeLock.Lock()
	defer m.writeLock.Unlock()

	for {
		if size > maxPayloadSize {
			size = maxPayloadSize
		}

		binary.BigEndian.PutUint32(hdr[0:4], uint32(id))
		binary.BigEndian.PutUint32(hdr[4:8], uint32(size))

		n, err := m.trunk.Write(hdr[:])
		if err != nil {
			err = fmt.Errorf("failed to write header to trunk: %w", err)
			if n != 0 {
				m.setError(err)
				m.Close()
			}
			return 0, err
		}

		n, err = m.trunk.Write(data[:size])
		if err != nil {
			err = fmt.Errorf("failed to write payload to trunk: %w", err)
			if n != 0 {
				m.setError(err)
				m.Close()
			}
			return 0, err
		}

		data = data[size:]
		if size > len(data) {
			size = len(data)
		}

		if size == 0 {
			break
		}
	}

	return len(buf), nil
}

func (m *mux) reader() {
	var (
		hdr [headerLen]byte
		cid uint32
		cnt uint32
		buf []byte
		err error
	)

	<-m.blockC

	for {
		select {
		case <-m.doneC:
			return
		default:
		}

		_, err = io.ReadFull(m.trunk, hdr[:])
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
			case errors.Is(err, ttrpc.ErrClosed):
				err = io.EOF
			case errors.Is(err, ttrpc.ErrServerClosed):
				err = io.EOF
			case errors.Is(err, net.ErrClosed):
				err = io.EOF
			default:
				err = fmt.Errorf("failed to read header from trunk: %w", err)
			}
			m.setError(err)
			m.Close()
			return
		}

		cid = binary.BigEndian.Uint32(hdr[0:4])
		cnt = binary.BigEndian.Uint32(hdr[4:8])
		if cnt > maxPayloadSize {
			// The write path never produces frames this large, so the
			// stream is malformed: we cannot skip the frame (the framing
			// has no resync marker) and we cannot allocate the claimed
			// size (up to 4 GiB, and int(cnt) overflows on 32-bit
			// architectures). Tear the mux down, like we do for queue
			// overflow.
			err = fmt.Errorf("conn %d: frame payload %d bytes, maximum is %d", cid, cnt, maxPayloadSize)
			m.setError(err)
			m.Close()
			return
		}
		buf = make([]byte, int(cnt))

		_, err = io.ReadFull(m.trunk, buf)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
			case errors.Is(err, ttrpc.ErrClosed):
				err = io.EOF
			case errors.Is(err, ttrpc.ErrServerClosed):
				err = io.EOF
			case errors.Is(err, net.ErrClosed):
				err = io.EOF
			default:
				err = fmt.Errorf("failed to read payload from trunk: %w", err)
			}
			m.setError(err)
			m.Close()
			return
		}

		m.connLock.RLock()
		conn, ok := m.conns[ConnID(cid)]
		m.connLock.RUnlock()
		if ok {
			select {
			case conn.readC <- buf:
			default:
				m.setError(errors.New("failed to queue payload for reading"))
				m.Close()
				return
			}
		}
	}
}

func (m *mux) setError(err error) {
	m.errOnce.Do(func() {
		m.err = err
	})
}

func (m *mux) error() error {
	m.errOnce.Do(func() {
		if m.err == nil {
			m.err = io.EOF
		}
	})
	return m.err
}

//
// multiplexed connections
//

// Reads reads the next message from the multiplexed connection.
func (c *conn) Read(buf []byte) (int, error) {
	var (
		msg []byte
		err error
		ok  bool
	)

	select {
	case err, ok = <-c.doneC:
		if !ok || err == nil {
			err = c.mux.error()
		}
		return 0, err
	case msg, ok = <-c.readC:
		if !ok {
			return 0, c.mux.error()
		}
		if cap(buf) < len(msg) {
			return 0, syscall.ENOMEM
		}
	}

	copy(buf, msg)
	return len(msg), nil
}

// Write writes the given data to the multiplexed connection.
func (c *conn) Write(b []byte) (int, error) {
	select {
	case err := <-c.doneC:
		if err == nil {
			err = io.EOF
		}
		return 0, err
	default:
	}
	return c.mux.write(c.id, b)
}

// Close closes the multiplexed connection.
func (c *conn) Close() error {
	c.mux.connLock.Lock()
	defer c.mux.connLock.Unlock()
	if c.mux.conns[c.id] == c {
		delete(c.mux.conns, c.id)
	}
	return c.close()
}

func (c *conn) close() error {
	c.closeOnce.Do(func() {
		close(c.doneC)
	})
	return nil
}

// LocalAddr is the unimplemented stub for the corresponding net.Conn function.
func (c *conn) LocalAddr() net.Addr {
	return nil
}

// RemoteAddr is the unimplemented stub for the corresponding net.Conn function.
func (c *conn) RemoteAddr() net.Addr {
	return nil
}

// SetDeadline is the unimplemented stub for the corresponding net.Conn function.
func (c *conn) SetDeadline(_ time.Time) error {
	return nil
}

// SetReadDeadline is the unimplemented stub for the corresponding net.Conn function.
func (c *conn) SetReadDeadline(_ time.Time) error {
	return nil
}

// SetWriteDeadline is the unimplemented stub for the corresponding net.Conn function.
func (c *conn) SetWriteDeadline(_ time.Time) error {
	return nil
}

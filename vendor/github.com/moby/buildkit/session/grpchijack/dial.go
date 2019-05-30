package grpchijack

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func Dialer(api controlapi.ControlClient) session.Dialer {
	return func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {

		meta = lowerHeaders(meta)

		md := metadata.MD(meta)

		ctx = metadata.NewOutgoingContext(ctx, md)

		stream, err := api.Session(ctx)
		if err != nil {
			return nil, err
		}

		c, _ := streamToConn(stream)
		return c, nil
	}
}

func streamToConn(stream grpc.Stream) (net.Conn, <-chan struct{}) {
	closeCh := make(chan struct{})
	c := &conn{stream: stream, buf: make([]byte, 32*1<<10), closeCh: closeCh}
	return c, closeCh
}

type conn struct {
	stream  grpc.Stream
	buf     []byte
	lastBuf []byte

	closedOnce sync.Once
	readMu     sync.Mutex
	writeMu    sync.Mutex
	err        error
	closeCh    chan struct{}
}

func (c *conn) Read(b []byte) (n int, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if c.lastBuf != nil {
		n := copy(b, c.lastBuf)
		c.lastBuf = c.lastBuf[n:]
		if len(c.lastBuf) == 0 {
			c.lastBuf = nil
		}
		return n, nil
	}
	m := new(controlapi.BytesMessage)
	m.Data = c.buf

	if err := c.stream.RecvMsg(m); err != nil {
		return 0, err
	}
	c.buf = m.Data[:cap(m.Data)]

	n = copy(b, m.Data)
	if n < len(m.Data) {
		c.lastBuf = m.Data[n:]
	}

	return n, nil
}

func (c *conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	m := &controlapi.BytesMessage{Data: b}
	if err := c.stream.SendMsg(m); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *conn) Close() (err error) {
	c.closedOnce.Do(func() {
		defer func() {
			close(c.closeCh)
		}()

		if cs, ok := c.stream.(grpc.ClientStream); ok {
			c.writeMu.Lock()
			err = cs.CloseSend()
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}

		c.readMu.Lock()
		for {
			m := new(controlapi.BytesMessage)
			m.Data = c.buf
			err = c.stream.RecvMsg(m)
			if err != nil {
				if err != io.EOF {
					c.readMu.Unlock()
					return
				}
				err = nil
				break
			}
			c.buf = m.Data[:cap(m.Data)]
			c.lastBuf = append(c.lastBuf, c.buf...)
		}
		c.readMu.Unlock()

	})
	return nil
}

func (c *conn) LocalAddr() net.Addr {
	return dummyAddr{}
}
func (c *conn) RemoteAddr() net.Addr {
	return dummyAddr{}
}
func (c *conn) SetDeadline(t time.Time) error {
	return nil
}
func (c *conn) SetReadDeadline(t time.Time) error {
	return nil
}
func (c *conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type dummyAddr struct {
}

func (d dummyAddr) Network() string {
	return "tcp"
}

func (d dummyAddr) String() string {
	return "localhost"
}

func lowerHeaders(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k := range in {
		out[strings.ToLower(k)] = in[k]
	}
	return out
}

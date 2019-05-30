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

package ttrpc

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/status"
)

// ErrClosed is returned by client methods when the underlying connection is
// closed.
var ErrClosed = errors.New("ttrpc: closed")

type Client struct {
	codec   codec
	conn    net.Conn
	channel *channel
	calls   chan *callRequest

	closed    chan struct{}
	closeOnce sync.Once
	closeFunc func()
	done      chan struct{}
	err       error
}

func NewClient(conn net.Conn) *Client {
	c := &Client{
		codec:     codec{},
		conn:      conn,
		channel:   newChannel(conn),
		calls:     make(chan *callRequest),
		closed:    make(chan struct{}),
		done:      make(chan struct{}),
		closeFunc: func() {},
	}

	go c.run()
	return c
}

type callRequest struct {
	ctx  context.Context
	req  *Request
	resp *Response  // response will be written back here
	errs chan error // error written here on completion
}

func (c *Client) Call(ctx context.Context, service, method string, req, resp interface{}) error {
	payload, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}

	var (
		creq = &Request{
			Service: service,
			Method:  method,
			Payload: payload,
		}

		cresp = &Response{}
	)

	if dl, ok := ctx.Deadline(); ok {
		creq.TimeoutNano = dl.Sub(time.Now()).Nanoseconds()
	}

	if err := c.dispatch(ctx, creq, cresp); err != nil {
		return err
	}

	if err := c.codec.Unmarshal(cresp.Payload, resp); err != nil {
		return err
	}

	if cresp.Status == nil {
		return errors.New("no status provided on response")
	}

	return status.ErrorProto(cresp.Status)
}

func (c *Client) dispatch(ctx context.Context, req *Request, resp *Response) error {
	errs := make(chan error, 1)
	call := &callRequest{
		ctx:  ctx,
		req:  req,
		resp: resp,
		errs: errs,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.calls <- call:
	case <-c.done:
		return c.err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errs:
		return filterCloseErr(err)
	case <-c.done:
		return c.err
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})

	return nil
}

// OnClose allows a close func to be called when the server is closed
func (c *Client) OnClose(closer func()) {
	c.closeFunc = closer
}

type message struct {
	messageHeader
	p   []byte
	err error
}

func (c *Client) run() {
	var (
		streamID    uint32 = 1
		waiters            = make(map[uint32]*callRequest)
		calls              = c.calls
		incoming           = make(chan *message)
		shutdown           = make(chan struct{})
		shutdownErr error
	)

	go func() {
		defer close(shutdown)

		// start one more goroutine to recv messages without blocking.
		for {
			mh, p, err := c.channel.recv(context.TODO())
			if err != nil {
				_, ok := status.FromError(err)
				if !ok {
					// treat all errors that are not an rpc status as terminal.
					// all others poison the connection.
					shutdownErr = err
					return
				}
			}
			select {
			case incoming <- &message{
				messageHeader: mh,
				p:             p[:mh.Length],
				err:           err,
			}:
			case <-c.done:
				return
			}
		}
	}()

	defer c.conn.Close()
	defer close(c.done)
	defer c.closeFunc()

	for {
		select {
		case call := <-calls:
			if err := c.send(call.ctx, streamID, messageTypeRequest, call.req); err != nil {
				call.errs <- err
				continue
			}

			waiters[streamID] = call
			streamID += 2 // enforce odd client initiated request ids
		case msg := <-incoming:
			call, ok := waiters[msg.StreamID]
			if !ok {
				logrus.Errorf("ttrpc: received message for unknown channel %v", msg.StreamID)
				continue
			}

			call.errs <- c.recv(call.resp, msg)
			delete(waiters, msg.StreamID)
		case <-shutdown:
			if shutdownErr != nil {
				shutdownErr = filterCloseErr(shutdownErr)
			} else {
				shutdownErr = ErrClosed
			}

			shutdownErr = errors.Wrapf(shutdownErr, "ttrpc: client shutting down")

			c.err = shutdownErr
			for _, waiter := range waiters {
				waiter.errs <- shutdownErr
			}
			c.Close()
			return
		case <-c.closed:
			if c.err == nil {
				c.err = ErrClosed
			}
			// broadcast the shutdown error to the remaining waiters.
			for _, waiter := range waiters {
				waiter.errs <- c.err
			}
			return
		}
	}
}

func (c *Client) send(ctx context.Context, streamID uint32, mtype messageType, msg interface{}) error {
	p, err := c.codec.Marshal(msg)
	if err != nil {
		return err
	}

	return c.channel.send(ctx, streamID, mtype, p)
}

func (c *Client) recv(resp *Response, msg *message) error {
	if msg.err != nil {
		return msg.err
	}

	if msg.Type != messageTypeResponse {
		return errors.New("unkown message type received")
	}

	defer c.channel.putmbuf(msg.p)
	return proto.Unmarshal(msg.p, resp)
}

// filterCloseErr rewrites EOF and EPIPE errors to ErrClosed. Use when
// returning from call or handling errors from main read loop.
//
// This purposely ignores errors with a wrapped cause.
func filterCloseErr(err error) error {
	if err == nil {
		return nil
	}

	if err == io.EOF {
		return ErrClosed
	}

	if strings.Contains(err.Error(), "use of closed network connection") {
		return ErrClosed
	}

	// if we have an epipe on a write, we cast to errclosed
	if oerr, ok := err.(*net.OpError); ok && oerr.Op == "write" {
		if serr, ok := oerr.Err.(*os.SyscallError); ok && serr.Err == syscall.EPIPE {
			return ErrClosed
		}
	}

	return err
}

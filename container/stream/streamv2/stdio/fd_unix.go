// +build !windows

package stdio

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	fdResponseOK = iota
	fdResponseErr

	fdMax         = 32
	fdHeaderSize  = 8
	cmsgSpaceSize = 4
)

func NewFdServer(addr string) (*FdServer, error) {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		return nil, err
	}

	return &FdServer{l: l}, nil
}

type FdServer struct {
	l *net.UnixListener
}

func (s *FdServer) Close() error {
	s.l.Close()
	unix.Unlink(s.l.Addr().String())
	return nil
}

func (s *FdServer) Serve(ctx context.Context, h func([]int)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := s.l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		go s.handleClient(ctx, conn, h)
	}
}

func (s *FdServer) handleClient(ctx context.Context, conn *net.UnixConn, h func([]int)) {
	defer conn.Close()

	bw := bufio.NewWriter(conn)

	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(fdMax*cmsgSpaceSize))
	retBuf := make([]byte, fdMax+fdHeaderSize)

	writeError := func(err error) bool {
		msg := err.Error()
		binary.BigEndian.PutUint32(retBuf[:4], fdResponseErr)
		binary.BigEndian.PutUint32(retBuf[4:fdHeaderSize], uint32(len(msg)))

		if _, err := bw.Write(retBuf); err != nil {
			logrus.WithError(err).Error("Error writing error header back to client")
			return false
		}
		if _, err := bw.WriteString(msg); err != nil {
			logrus.WithError(err).Error("Error writing error response back to client")
			return false
		}

		if err := bw.Flush(); err != nil {
			logrus.WithError(err).Error("Error flushing error response back to client")
			return false
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, n, _, _, err := conn.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}

		ls, err := unix.ParseSocketControlMessage(oob[:n])
		if err != nil {
			if !writeError(err) {
				return
			}
			continue
		}

		var numFds int
		for _, m := range ls {
			fds, err := unix.ParseUnixRights(&m)
			if err != nil {
				if !writeError(fmt.Errorf("error parsing unix rights message: %w", err)) {
					return
				}
				continue
			}

			if h != nil {
				h(fds)
			}

			numFds += len(fds)
			for i, fd := range fds {
				retBuf[fdHeaderSize+i] = byte(fd)
			}
		}

		binary.BigEndian.PutUint32(retBuf[:4], fdResponseOK)
		binary.BigEndian.PutUint32(retBuf[4:fdHeaderSize], uint32(numFds))

		if _, err := bw.Write(retBuf[:fdHeaderSize+numFds]); err != nil {
			logrus.WithError(err).Error("Error writing response header back to client")
			return
		}
		if err := bw.Flush(); err != nil {
			logrus.WithError(err).Error("Error flushing error response back to client")
			return
		}
	}
}

func NewFdClient(ctx context.Context, addr string) (*FdClient, error) {
	conn, err := dialRetry(ctx, addr, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("error dialing fd server: %w", err)
	}
	return &FdClient{conn: conn.(*net.UnixConn)}, nil
}

type FdClient struct {
	conn *net.UnixConn
}

// Sendfd sends the file descriptors over the specified unix conn.
func (c *FdClient) Sendfd(files ...*os.File) ([]int, error) {
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
	}
	_, _, err := c.conn.WriteMsgUnix([]byte{0}, unix.UnixRights(fds...), nil)
	if err != nil {
		return nil, err
	}

	br := bufio.NewReader(c.conn)
	hbuf := make([]byte, fdHeaderSize)

	if _, err := io.ReadFull(br, hbuf); err != nil {
		return nil, err
	}

	msgSize := binary.BigEndian.Uint32(hbuf[4:])
	msgBuf := make([]byte, msgSize)

	if _, err := io.ReadFull(br, msgBuf); err != nil {
		return nil, err
	}

	switch binary.BigEndian.Uint32(hbuf[:4]) {
	case fdResponseOK:
		fds := make([]int, msgSize)
		for i, fd := range msgBuf[:msgSize] {
			fds[i] = int(fd)
		}
		return fds, nil
	case fdResponseErr:
		return nil, fmt.Errorf(string(msgBuf[:msgSize]))
	default:
		return nil, fmt.Errorf("got unknown response from server")
	}
}

func (c *FdClient) Close() error {
	return c.conn.Close()
}

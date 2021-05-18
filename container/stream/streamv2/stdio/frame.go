package stdio

import (
	"io"
	"sync"

	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func GetFramedStreams(stream io.ReadWriteCloser, framing StreamFraming, includeStdin, includeStdout, includeStderr bool) (stdin io.ReadCloser, stdout, stderr io.WriteCloser) {
	switch framing.Type {
	case StreamFraming_NONE:
		if includeStdin {
			stdin = stream
		}
		if includeStdout {
			stdout = stream
		}
		if includeStderr {
			stderr = stream
		}
	case StreamFraming_STDCOPY:
		if includeStdin {
			stdin = stream
		}
		if includeStdout {
			stdout = ioutils.NewWriteCloserWrapper(stdcopy.NewStdWriter(stream, stdcopy.Stdout), stream.Close)
		}
		if includeStderr {
			stderr = ioutils.NewWriteCloserWrapper(stdcopy.NewStdWriter(stream, stdcopy.Stderr), stream.Close)
		}
	case StreamFraming_WEBSOCKET_TEXT, StreamFraming_WEBSOCKET_BINARY:
		rwc := newWsConn(stream, framing.Type)

		if includeStdin {
			stdin = rwc
		}
		if includeStdout {
			stdout = rwc
		}
		if includeStderr {
			stderr = rwc
		}
	default:
		panic("invalid stream framing type")
	}

	return stdin, stdout, stderr
}

func newWsConn(rwc io.ReadWriteCloser, framing StreamFraming_FramingType) *wsStream {
	var op ws.OpCode
	switch framing {
	case StreamFraming_WEBSOCKET_TEXT:
		op = ws.OpText
	case StreamFraming_WEBSOCKET_BINARY:
		op = ws.OpBinary
	}
	state := ws.StateServerSide
	return &wsStream{
		r:      wsutil.NewServerSideReader(rwc),
		w:      wsutil.NewWriter(rwc, state, op),
		closed: make(chan struct{}),
		state:  state,
	}
}

// wsStream is used to upgrade a raw fd up to a websocket stream
// It handles framing for reads/writes as well as control messages.
type wsStream struct {
	mu    sync.Mutex
	r     *wsutil.Reader
	w     *wsutil.Writer
	state ws.State

	closeOnce sync.Once
	closed    chan struct{}
	closeSent bool

	remain int64
	err    error

	rwc io.ReadWriteCloser
}

func (s *wsStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}

	if s.remain == 0 {
		for {
			select {
			case <-s.closed:
				return 0, io.EOF
			default:
			}
			hdr, err := s.r.NextFrame()
			if err != nil {
				return 0, err
			}
			if hdr.OpCode.IsControl() {
				if err := s.control(hdr); err != nil {
					return 0, err
				}
				continue
			}
			s.remain = hdr.Length
			break
		}
	}

	n, err := s.r.Read(p)
	if n > 0 {
		s.remain -= int64(n)
	}
	if s.remain == 0 && err == io.EOF {
		// In this case... it could be the reader was closed, but it could also be that we reached the end of a frame.
		// Since we know we reached the end of the frame, we'll assume it's not a real EOF.
		// We'll find out on the next read.
		err = nil
	}
	return n, err
}

func (s *wsStream) Write(p []byte) (_ int, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return 0, s.err
	}

	n, err := s.w.WriteThrough(p)
	s.err = err
	return n, err
}

func (s *wsStream) Close() error {
	s.mu.Lock()

	if s.closeSent {
		s.mu.Unlock()
		<-s.closed
		return s.rwc.Close()
	}

	if _, err := s.rwc.Write(ws.CompiledClose); err != nil {
		s.mu.Unlock()
		return err
	}
	s.closeSent = true
	s.err = io.EOF
	s.mu.Unlock()
	<-s.closed

	return s.rwc.Close()
}

func (s *wsStream) control(h ws.Header) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var shouldBreak bool
	if h.OpCode == ws.OpClose {
		s.closeOnce.Do(func() {
			close(s.closed)
		})
		s.closeSent = true
		shouldBreak = true
	}
	if !shouldBreak {
		handler := wsutil.ControlFrameHandler(s, s.state)
		if err := handler(h, s.rwc); err != nil {
			s.err = err
		}
	}
	return s.err
}

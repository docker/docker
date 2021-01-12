package wsutil

import (
	"fmt"
	"io"

	"github.com/gobwas/pool"
	"github.com/gobwas/pool/pbytes"
	"github.com/gobwas/ws"
)

// DefaultWriteBuffer contains size of Writer's default buffer. It used by
// Writer constructor functions.
var DefaultWriteBuffer = 4096

var (
	// ErrNotEmpty is returned by Writer.WriteThrough() to indicate that buffer is
	// not empty and write through could not be done. That is, caller should call
	// Writer.FlushFragment() to make buffer empty.
	ErrNotEmpty = fmt.Errorf("writer not empty")

	// ErrControlOverflow is returned by ControlWriter.Write() to indicate that
	// no more data could be written to the underlying io.Writer because
	// MaxControlFramePayloadSize limit is reached.
	ErrControlOverflow = fmt.Errorf("control frame payload overflow")
)

// Constants which are represent frame length ranges.
const (
	len7  = int64(125) // 126 and 127 are reserved values
	len16 = int64(^uint16(0))
	len64 = int64((^uint64(0)) >> 1)
)

// ControlWriter is a wrapper around Writer that contains some guards for
// buffered writes of control frames.
type ControlWriter struct {
	w     *Writer
	limit int
	n     int
}

// NewControlWriter contains ControlWriter with Writer inside whose buffer size
// is at most ws.MaxControlFramePayloadSize + ws.MaxHeaderSize.
func NewControlWriter(dest io.Writer, state ws.State, op ws.OpCode) *ControlWriter {
	return &ControlWriter{
		w:     NewWriterSize(dest, state, op, ws.MaxControlFramePayloadSize),
		limit: ws.MaxControlFramePayloadSize,
	}
}

// NewControlWriterBuffer returns a new ControlWriter with buf as a buffer.
//
// Note that it reserves x bytes of buf for header data, where x could be
// ws.MinHeaderSize or ws.MinHeaderSize+4 (depending on state). At most
// (ws.MaxControlFramePayloadSize + x) bytes of buf will be used.
//
// It panics if len(buf) <= ws.MinHeaderSize + x.
func NewControlWriterBuffer(dest io.Writer, state ws.State, op ws.OpCode, buf []byte) *ControlWriter {
	max := ws.MaxControlFramePayloadSize + headerSize(state, ws.MaxControlFramePayloadSize)
	if len(buf) > max {
		buf = buf[:max]
	}

	w := NewWriterBuffer(dest, state, op, buf)

	return &ControlWriter{
		w:     w,
		limit: len(w.buf),
	}
}

// Write implements io.Writer. It writes to the underlying Writer until it
// returns error or until ControlWriter write limit will be exceeded.
func (c *ControlWriter) Write(p []byte) (n int, err error) {
	if c.n+len(p) > c.limit {
		return 0, ErrControlOverflow
	}
	return c.w.Write(p)
}

// Flush flushes all buffered data to the underlying io.Writer.
func (c *ControlWriter) Flush() error {
	return c.w.Flush()
}

// Writer contains logic of buffering output data into a WebSocket fragments.
// It is much the same as bufio.Writer, except the thing that it works with
// WebSocket frames, not the raw data.
//
// Writer writes frames with specified OpCode.
// It uses ws.State to decide whether the output frames must be masked.
//
// Note that it does not check control frame size or other RFC rules.
// That is, it must be used with special care to write control frames without
// violation of RFC. You could use ControlWriter that wraps Writer and contains
// some guards for writing control frames.
//
// If an error occurs writing to a Writer, no more data will be accepted and
// all subsequent writes will return the error.
// After all data has been written, the client should call the Flush() method
// to guarantee all data has been forwarded to the underlying io.Writer.
type Writer struct {
	dest io.Writer

	n   int    // Buffered bytes counter.
	raw []byte // Raw representation of buffer, including reserved header bytes.
	buf []byte // Writeable part of buffer, without reserved header bytes.

	op    ws.OpCode
	state ws.State

	dirty      bool
	fragmented bool

	err error
}

var writers = pool.New(128, 65536)

// GetWriter tries to reuse Writer getting it from the pool.
//
// This function is intended for memory consumption optimizations, because
// NewWriter*() functions make allocations for inner buffer.
//
// Note the it ceils n to the power of two.
//
// If you have your own bytes buffer pool you could use NewWriterBuffer to use
// pooled bytes in writer.
func GetWriter(dest io.Writer, state ws.State, op ws.OpCode, n int) *Writer {
	x, m := writers.Get(n)
	if x != nil {
		w := x.(*Writer)
		w.Reset(dest, state, op)
		return w
	}
	// NOTE: we use m instead of n, because m is an attempt to reuse w of such
	// size in the future.
	return NewWriterBufferSize(dest, state, op, m)
}

// PutWriter puts w for future reuse by GetWriter().
func PutWriter(w *Writer) {
	w.Reset(nil, 0, 0)
	writers.Put(w, w.Size())
}

// NewWriter returns a new Writer whose buffer has the DefaultWriteBuffer size.
func NewWriter(dest io.Writer, state ws.State, op ws.OpCode) *Writer {
	return NewWriterBufferSize(dest, state, op, 0)
}

// NewWriterSize returns a new Writer whose buffer size is at most n + ws.MaxHeaderSize.
// That is, output frames payload length could be up to n, except the case when
// Write() is called on empty Writer with len(p) > n.
//
// If n <= 0 then the default buffer size is used as Writer's buffer size.
func NewWriterSize(dest io.Writer, state ws.State, op ws.OpCode, n int) *Writer {
	if n > 0 {
		n += headerSize(state, n)
	}
	return NewWriterBufferSize(dest, state, op, n)
}

// NewWriterBufferSize returns a new Writer whose buffer size is equal to n.
// If n <= ws.MinHeaderSize then the default buffer size is used.
//
// Note that Writer will reserve x bytes for header data, where x is in range
// [ws.MinHeaderSize,ws.MaxHeaderSize]. That is, frames flushed by Writer
// will not have payload length equal to n, except the case when Write() is
// called on empty Writer with len(p) > n.
func NewWriterBufferSize(dest io.Writer, state ws.State, op ws.OpCode, n int) *Writer {
	if n <= ws.MinHeaderSize {
		n = DefaultWriteBuffer
	}
	return NewWriterBuffer(dest, state, op, make([]byte, n))
}

// NewWriterBuffer returns a new Writer with buf as a buffer.
//
// Note that it reserves x bytes of buf for header data, where x is in range
// [ws.MinHeaderSize,ws.MaxHeaderSize] (depending on state and buf size).
//
// You could use ws.HeaderSize() to calculate number of bytes needed to store
// header data.
//
// It panics if len(buf) is too small to fit header and payload data.
func NewWriterBuffer(dest io.Writer, state ws.State, op ws.OpCode, buf []byte) *Writer {
	offset := reserve(state, len(buf))
	if len(buf) <= offset {
		panic("buffer too small")
	}

	return &Writer{
		dest:  dest,
		raw:   buf,
		buf:   buf[offset:],
		state: state,
		op:    op,
	}
}

func reserve(state ws.State, n int) (offset int) {
	var mask int
	if state.ClientSide() {
		mask = 4
	}

	switch {
	case n <= int(len7)+mask+2:
		return mask + 2
	case n <= int(len16)+mask+4:
		return mask + 4
	default:
		return mask + 10
	}
}

// headerSize returns number of bytes needed to encode header of a frame with
// given state and length.
func headerSize(s ws.State, n int) int {
	return ws.HeaderSize(ws.Header{
		Length: int64(n),
		Masked: s.ClientSide(),
	})
}

// Reset discards any buffered data, clears error, and resets w to have given
// state and write frames with given OpCode to dest.
func (w *Writer) Reset(dest io.Writer, state ws.State, op ws.OpCode) {
	w.n = 0
	w.dirty = false
	w.fragmented = false
	w.dest = dest
	w.state = state
	w.op = op
}

// Size returns the size of the underlying buffer in bytes.
func (w *Writer) Size() int {
	return len(w.buf)
}

// Available returns how many bytes are unused in the buffer.
func (w *Writer) Available() int {
	return len(w.buf) - w.n
}

// Buffered returns the number of bytes that have been written into the current
// buffer.
func (w *Writer) Buffered() int {
	return w.n
}

// Write implements io.Writer.
//
// Note that even if the Writer was created to have N-sized buffer, Write()
// with payload of N bytes will not fit into that buffer. Writer reserves some
// space to fit WebSocket header data.
func (w *Writer) Write(p []byte) (n int, err error) {
	// Even empty p may make a sense.
	w.dirty = true

	var nn int
	for len(p) > w.Available() && w.err == nil {
		if w.Buffered() == 0 {
			// Large write, empty buffer. Write directly from p to avoid copy.
			// Trade off here is that we make additional Write() to underlying
			// io.Writer when writing frame header.
			//
			// On large buffers additional write is better than copying.
			nn, _ = w.WriteThrough(p)
		} else {
			nn = copy(w.buf[w.n:], p)
			w.n += nn
			w.FlushFragment()
		}
		n += nn
		p = p[nn:]
	}
	if w.err != nil {
		return n, w.err
	}
	nn = copy(w.buf[w.n:], p)
	w.n += nn
	n += nn

	// Even if w.Available() == 0 we will not flush buffer preventively because
	// this could bring unwanted fragmentation. That is, user could create
	// buffer with size that fits exactly all further Write() call, and then
	// call Flush(), excepting that single and not fragmented frame will be
	// sent. With preemptive flush this case will produce two frames – last one
	// will be empty and just to set fin = true.

	return n, w.err
}

// WriteThrough writes data bypassing the buffer.
// Note that Writer's buffer must be empty before calling WriteThrough().
func (w *Writer) WriteThrough(p []byte) (n int, err error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.Buffered() != 0 {
		return 0, ErrNotEmpty
	}

	w.err = writeFrame(w.dest, w.state, w.opCode(), false, p)
	if w.err == nil {
		n = len(p)
	}

	w.dirty = true
	w.fragmented = true

	return n, w.err
}

// ReadFrom implements io.ReaderFrom.
func (w *Writer) ReadFrom(src io.Reader) (n int64, err error) {
	var nn int
	for err == nil {
		if w.Available() == 0 {
			err = w.FlushFragment()
			continue
		}

		// We copy the behavior of bufio.Writer here.
		// Also, from the docs on io.ReaderFrom:
		//   ReadFrom reads data from r until EOF or error.
		//
		// See https://codereview.appspot.com/76400048/#ps1
		const maxEmptyReads = 100
		var nr int
		for nr < maxEmptyReads {
			nn, err = src.Read(w.buf[w.n:])
			if nn != 0 || err != nil {
				break
			}
			nr++
		}
		if nr == maxEmptyReads {
			return n, io.ErrNoProgress
		}

		w.n += nn
		n += int64(nn)
	}
	if err == io.EOF {
		// NOTE: Do not flush preemptively.
		// See the Write() sources for more info.
		err = nil
		w.dirty = true
	}
	return n, err
}

// Flush writes any buffered data to the underlying io.Writer.
// It sends the frame with "fin" flag set to true.
//
// If no Write() or ReadFrom() was made, then Flush() does nothing.
func (w *Writer) Flush() error {
	if (!w.dirty && w.Buffered() == 0) || w.err != nil {
		return w.err
	}

	w.err = w.flushFragment(true)
	w.n = 0
	w.dirty = false
	w.fragmented = false

	return w.err
}

// FlushFragment writes any buffered data to the underlying io.Writer.
// It sends the frame with "fin" flag set to false.
func (w *Writer) FlushFragment() error {
	if w.Buffered() == 0 || w.err != nil {
		return w.err
	}

	w.err = w.flushFragment(false)
	w.n = 0
	w.fragmented = true

	return w.err
}

func (w *Writer) flushFragment(fin bool) error {
	frame := ws.NewFrame(w.opCode(), fin, w.buf[:w.n])
	if w.state.ClientSide() {
		frame = ws.MaskFrameInPlace(frame)
	}

	// Write header to the header segment of the raw buffer.
	head := len(w.raw) - len(w.buf)
	offset := head - ws.HeaderSize(frame.Header)
	buf := bytesWriter{
		buf: w.raw[offset:head],
	}
	if err := ws.WriteHeader(&buf, frame.Header); err != nil {
		// Must never be reached.
		panic("dump header error: " + err.Error())
	}

	_, err := w.dest.Write(w.raw[offset : head+w.n])

	return err
}

func (w *Writer) opCode() ws.OpCode {
	if w.fragmented {
		return ws.OpContinuation
	}
	return w.op
}

var errNoSpace = fmt.Errorf("not enough buffer space")

type bytesWriter struct {
	buf []byte
	pos int
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	n := copy(w.buf[w.pos:], p)
	w.pos += n
	if n != len(p) {
		return n, errNoSpace
	}
	return n, nil
}

func writeFrame(w io.Writer, s ws.State, op ws.OpCode, fin bool, p []byte) error {
	var frame ws.Frame
	if s.ClientSide() {
		// Should copy bytes to prevent corruption of caller data.
		payload := pbytes.GetLen(len(p))
		defer pbytes.Put(payload)

		copy(payload, p)

		frame = ws.NewFrame(op, fin, payload)
		frame = ws.MaskFrameInPlace(frame)
	} else {
		frame = ws.NewFrame(op, fin, p)
	}

	return ws.WriteFrame(w, frame)
}

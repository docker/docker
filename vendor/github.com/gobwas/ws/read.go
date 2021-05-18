package ws

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Errors used by frame reader.
var (
	ErrHeaderLengthMSB        = fmt.Errorf("header error: the most significant bit must be 0")
	ErrHeaderLengthUnexpected = fmt.Errorf("header error: unexpected payload length bits")
)

// ReadHeader reads a frame header from r.
func ReadHeader(r io.Reader) (h Header, err error) {
	// Make slice of bytes with capacity 12 that could hold any header.
	//
	// The maximum header size is 14, but due to the 2 hop reads,
	// after first hop that reads first 2 constant bytes, we could reuse 2 bytes.
	// So 14 - 2 = 12.
	bts := make([]byte, 2, MaxHeaderSize-2)

	// Prepare to hold first 2 bytes to choose size of next read.
	_, err = io.ReadFull(r, bts)
	if err != nil {
		return
	}

	h.Fin = bts[0]&bit0 != 0
	h.Rsv = (bts[0] & 0x70) >> 4
	h.OpCode = OpCode(bts[0] & 0x0f)

	var extra int

	if bts[1]&bit0 != 0 {
		h.Masked = true
		extra += 4
	}

	length := bts[1] & 0x7f
	switch {
	case length < 126:
		h.Length = int64(length)

	case length == 126:
		extra += 2

	case length == 127:
		extra += 8

	default:
		err = ErrHeaderLengthUnexpected
		return
	}

	if extra == 0 {
		return
	}

	// Increase len of bts to extra bytes need to read.
	// Overwrite first 2 bytes that was read before.
	bts = bts[:extra]
	_, err = io.ReadFull(r, bts)
	if err != nil {
		return
	}

	switch {
	case length == 126:
		h.Length = int64(binary.BigEndian.Uint16(bts[:2]))
		bts = bts[2:]

	case length == 127:
		if bts[0]&0x80 != 0 {
			err = ErrHeaderLengthMSB
			return
		}
		h.Length = int64(binary.BigEndian.Uint64(bts[:8]))
		bts = bts[8:]
	}

	if h.Masked {
		copy(h.Mask[:], bts)
	}

	return
}

// ReadFrame reads a frame from r.
// It is not designed for high optimized use case cause it makes allocation
// for frame.Header.Length size inside to read frame payload into.
//
// Note that ReadFrame does not unmask payload.
func ReadFrame(r io.Reader) (f Frame, err error) {
	f.Header, err = ReadHeader(r)
	if err != nil {
		return
	}

	if f.Header.Length > 0 {
		// int(f.Header.Length) is safe here cause we have
		// checked it for overflow above in ReadHeader.
		f.Payload = make([]byte, int(f.Header.Length))
		_, err = io.ReadFull(r, f.Payload)
	}

	return
}

// MustReadFrame is like ReadFrame but panics if frame can not be read.
func MustReadFrame(r io.Reader) Frame {
	f, err := ReadFrame(r)
	if err != nil {
		panic(err)
	}
	return f
}

// ParseCloseFrameData parses close frame status code and closure reason if any provided.
// If there is no status code in the payload
// the empty status code is returned (code.Empty()) with empty string as a reason.
func ParseCloseFrameData(payload []byte) (code StatusCode, reason string) {
	if len(payload) < 2 {
		// We returning empty StatusCode here, preventing the situation
		// when endpoint really sent code 1005 and we should return ProtocolError on that.
		//
		// In other words, we ignoring this rule [RFC6455:7.1.5]:
		//   If this Close control frame contains no status code, _The WebSocket
		//   Connection Close Code_ is considered to be 1005.
		return
	}
	code = StatusCode(binary.BigEndian.Uint16(payload))
	reason = string(payload[2:])
	return
}

// ParseCloseFrameDataUnsafe is like ParseCloseFrameData except the thing
// that it does not copies payload bytes into reason, but prepares unsafe cast.
func ParseCloseFrameDataUnsafe(payload []byte) (code StatusCode, reason string) {
	if len(payload) < 2 {
		return
	}
	code = StatusCode(binary.BigEndian.Uint16(payload))
	reason = btsToString(payload[2:])
	return
}

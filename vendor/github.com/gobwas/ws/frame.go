package ws

import (
	"bytes"
	"encoding/binary"
	"math/rand"
)

// Constants defined by specification.
const (
	// All control frames MUST have a payload length of 125 bytes or less and MUST NOT be fragmented.
	MaxControlFramePayloadSize = 125
)

// OpCode represents operation code.
type OpCode byte

// Operation codes defined by specification.
// See https://tools.ietf.org/html/rfc6455#section-5.2
const (
	OpContinuation OpCode = 0x0
	OpText         OpCode = 0x1
	OpBinary       OpCode = 0x2
	OpClose        OpCode = 0x8
	OpPing         OpCode = 0x9
	OpPong         OpCode = 0xa
)

// IsControl checks whether the c is control operation code.
// See https://tools.ietf.org/html/rfc6455#section-5.5
func (c OpCode) IsControl() bool {
	// RFC6455: Control frames are identified by opcodes where
	// the most significant bit of the opcode is 1.
	//
	// Note that OpCode is only 4 bit length.
	return c&0x8 != 0
}

// IsData checks whether the c is data operation code.
// See https://tools.ietf.org/html/rfc6455#section-5.6
func (c OpCode) IsData() bool {
	// RFC6455: Data frames (e.g., non-control frames) are identified by opcodes
	// where the most significant bit of the opcode is 0.
	//
	// Note that OpCode is only 4 bit length.
	return c&0x8 == 0
}

// IsReserved checks whether the c is reserved operation code.
// See https://tools.ietf.org/html/rfc6455#section-5.2
func (c OpCode) IsReserved() bool {
	// RFC6455:
	// %x3-7 are reserved for further non-control frames
	// %xB-F are reserved for further control frames
	return (0x3 <= c && c <= 0x7) || (0xb <= c && c <= 0xf)
}

// StatusCode represents the encoded reason for closure of websocket connection.
//
// There are few helper methods on StatusCode that helps to define a range in
// which given code is lay in. accordingly to ranges defined in specification.
//
// See https://tools.ietf.org/html/rfc6455#section-7.4
type StatusCode uint16

// StatusCodeRange describes range of StatusCode values.
type StatusCodeRange struct {
	Min, Max StatusCode
}

// Status code ranges defined by specification.
// See https://tools.ietf.org/html/rfc6455#section-7.4.2
var (
	StatusRangeNotInUse    = StatusCodeRange{0, 999}
	StatusRangeProtocol    = StatusCodeRange{1000, 2999}
	StatusRangeApplication = StatusCodeRange{3000, 3999}
	StatusRangePrivate     = StatusCodeRange{4000, 4999}
)

// Status codes defined by specification.
// See https://tools.ietf.org/html/rfc6455#section-7.4.1
const (
	StatusNormalClosure           StatusCode = 1000
	StatusGoingAway               StatusCode = 1001
	StatusProtocolError           StatusCode = 1002
	StatusUnsupportedData         StatusCode = 1003
	StatusNoMeaningYet            StatusCode = 1004
	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExt            StatusCode = 1010
	StatusInternalServerError     StatusCode = 1011
	StatusTLSHandshake            StatusCode = 1015

	// StatusAbnormalClosure is a special code designated for use in
	// applications.
	StatusAbnormalClosure StatusCode = 1006

	// StatusNoStatusRcvd is a special code designated for use in applications.
	StatusNoStatusRcvd StatusCode = 1005
)

// In reports whether the code is defined in given range.
func (s StatusCode) In(r StatusCodeRange) bool {
	return r.Min <= s && s <= r.Max
}

// Empty reports whether the code is empty.
// Empty code has no any meaning neither app level codes nor other.
// This method is useful just to check that code is golang default value 0.
func (s StatusCode) Empty() bool {
	return s == 0
}

// IsNotUsed reports whether the code is predefined in not used range.
func (s StatusCode) IsNotUsed() bool {
	return s.In(StatusRangeNotInUse)
}

// IsApplicationSpec reports whether the code should be defined by
// application, framework or libraries specification.
func (s StatusCode) IsApplicationSpec() bool {
	return s.In(StatusRangeApplication)
}

// IsPrivateSpec reports whether the code should be defined privately.
func (s StatusCode) IsPrivateSpec() bool {
	return s.In(StatusRangePrivate)
}

// IsProtocolSpec reports whether the code should be defined by protocol specification.
func (s StatusCode) IsProtocolSpec() bool {
	return s.In(StatusRangeProtocol)
}

// IsProtocolDefined reports whether the code is already defined by protocol specification.
func (s StatusCode) IsProtocolDefined() bool {
	switch s {
	case StatusNormalClosure,
		StatusGoingAway,
		StatusProtocolError,
		StatusUnsupportedData,
		StatusInvalidFramePayloadData,
		StatusPolicyViolation,
		StatusMessageTooBig,
		StatusMandatoryExt,
		StatusInternalServerError,
		StatusNoStatusRcvd,
		StatusAbnormalClosure,
		StatusTLSHandshake:
		return true
	}
	return false
}

// IsProtocolReserved reports whether the code is defined by protocol specification
// to be reserved only for application usage purpose.
func (s StatusCode) IsProtocolReserved() bool {
	switch s {
	// [RFC6455]: {1005,1006,1015} is a reserved value and MUST NOT be set as a status code in a
	// Close control frame by an endpoint.
	case StatusNoStatusRcvd, StatusAbnormalClosure, StatusTLSHandshake:
		return true
	default:
		return false
	}
}

// Compiled control frames for common use cases.
// For construct-serialize optimizations.
var (
	CompiledPing  = MustCompileFrame(NewPingFrame(nil))
	CompiledPong  = MustCompileFrame(NewPongFrame(nil))
	CompiledClose = MustCompileFrame(NewCloseFrame(nil))

	CompiledCloseNormalClosure           = MustCompileFrame(closeFrameNormalClosure)
	CompiledCloseGoingAway               = MustCompileFrame(closeFrameGoingAway)
	CompiledCloseProtocolError           = MustCompileFrame(closeFrameProtocolError)
	CompiledCloseUnsupportedData         = MustCompileFrame(closeFrameUnsupportedData)
	CompiledCloseNoMeaningYet            = MustCompileFrame(closeFrameNoMeaningYet)
	CompiledCloseInvalidFramePayloadData = MustCompileFrame(closeFrameInvalidFramePayloadData)
	CompiledClosePolicyViolation         = MustCompileFrame(closeFramePolicyViolation)
	CompiledCloseMessageTooBig           = MustCompileFrame(closeFrameMessageTooBig)
	CompiledCloseMandatoryExt            = MustCompileFrame(closeFrameMandatoryExt)
	CompiledCloseInternalServerError     = MustCompileFrame(closeFrameInternalServerError)
	CompiledCloseTLSHandshake            = MustCompileFrame(closeFrameTLSHandshake)
)

// Header represents websocket frame header.
// See https://tools.ietf.org/html/rfc6455#section-5.2
type Header struct {
	Fin    bool
	Rsv    byte
	OpCode OpCode
	Masked bool
	Mask   [4]byte
	Length int64
}

// Rsv1 reports whether the header has first rsv bit set.
func (h Header) Rsv1() bool { return h.Rsv&bit5 != 0 }

// Rsv2 reports whether the header has second rsv bit set.
func (h Header) Rsv2() bool { return h.Rsv&bit6 != 0 }

// Rsv3 reports whether the header has third rsv bit set.
func (h Header) Rsv3() bool { return h.Rsv&bit7 != 0 }

// Frame represents websocket frame.
// See https://tools.ietf.org/html/rfc6455#section-5.2
type Frame struct {
	Header  Header
	Payload []byte
}

// NewFrame creates frame with given operation code,
// flag of completeness and payload bytes.
func NewFrame(op OpCode, fin bool, p []byte) Frame {
	return Frame{
		Header: Header{
			Fin:    fin,
			OpCode: op,
			Length: int64(len(p)),
		},
		Payload: p,
	}
}

// NewTextFrame creates text frame with p as payload.
// Note that p is not copied.
func NewTextFrame(p []byte) Frame {
	return NewFrame(OpText, true, p)
}

// NewBinaryFrame creates binary frame with p as payload.
// Note that p is not copied.
func NewBinaryFrame(p []byte) Frame {
	return NewFrame(OpBinary, true, p)
}

// NewPingFrame creates ping frame with p as payload.
// Note that p is not copied.
// Note that p must have length of MaxControlFramePayloadSize bytes or less due
// to RFC.
func NewPingFrame(p []byte) Frame {
	return NewFrame(OpPing, true, p)
}

// NewPongFrame creates pong frame with p as payload.
// Note that p is not copied.
// Note that p must have length of MaxControlFramePayloadSize bytes or less due
// to RFC.
func NewPongFrame(p []byte) Frame {
	return NewFrame(OpPong, true, p)
}

// NewCloseFrame creates close frame with given close body.
// Note that p is not copied.
// Note that p must have length of MaxControlFramePayloadSize bytes or less due
// to RFC.
func NewCloseFrame(p []byte) Frame {
	return NewFrame(OpClose, true, p)
}

// NewCloseFrameBody encodes a closure code and a reason into a binary
// representation.
//
// It returns slice which is at most MaxControlFramePayloadSize bytes length.
// If the reason is too big it will be cropped to fit the limit defined by the
// spec.
//
// See https://tools.ietf.org/html/rfc6455#section-5.5
func NewCloseFrameBody(code StatusCode, reason string) []byte {
	n := min(2+len(reason), MaxControlFramePayloadSize)
	p := make([]byte, n)

	crop := min(MaxControlFramePayloadSize-2, len(reason))
	PutCloseFrameBody(p, code, reason[:crop])

	return p
}

// PutCloseFrameBody encodes code and reason into buf.
//
// It will panic if the buffer is too small to accommodate a code or a reason.
//
// PutCloseFrameBody does not check buffer to be RFC compliant, but note that
// by RFC it must be at most MaxControlFramePayloadSize.
func PutCloseFrameBody(p []byte, code StatusCode, reason string) {
	_ = p[1+len(reason)]
	binary.BigEndian.PutUint16(p, uint16(code))
	copy(p[2:], reason)
}

// MaskFrame masks frame and returns frame with masked payload and Mask header's field set.
// Note that it copies f payload to prevent collisions.
// For less allocations you could use MaskFrameInPlace or construct frame manually.
func MaskFrame(f Frame) Frame {
	return MaskFrameWith(f, NewMask())
}

// MaskFrameWith masks frame with given mask and returns frame
// with masked payload and Mask header's field set.
// Note that it copies f payload to prevent collisions.
// For less allocations you could use MaskFrameInPlaceWith or construct frame manually.
func MaskFrameWith(f Frame, mask [4]byte) Frame {
	// TODO(gobwas): check CopyCipher ws copy() Cipher().
	p := make([]byte, len(f.Payload))
	copy(p, f.Payload)
	f.Payload = p
	return MaskFrameInPlaceWith(f, mask)
}

// MaskFrameInPlace masks frame and returns frame with masked payload and Mask
// header's field set.
// Note that it applies xor cipher to f.Payload without copying, that is, it
// modifies f.Payload inplace.
func MaskFrameInPlace(f Frame) Frame {
	return MaskFrameInPlaceWith(f, NewMask())
}

// MaskFrameInPlaceWith masks frame with given mask and returns frame
// with masked payload and Mask header's field set.
// Note that it applies xor cipher to f.Payload without copying, that is, it
// modifies f.Payload inplace.
func MaskFrameInPlaceWith(f Frame, m [4]byte) Frame {
	f.Header.Masked = true
	f.Header.Mask = m
	Cipher(f.Payload, m, 0)
	return f
}

// NewMask creates new random mask.
func NewMask() (ret [4]byte) {
	binary.BigEndian.PutUint32(ret[:], rand.Uint32())
	return
}

// CompileFrame returns byte representation of given frame.
// In terms of memory consumption it is useful to precompile static frames
// which are often used.
func CompileFrame(f Frame) (bts []byte, err error) {
	buf := bytes.NewBuffer(make([]byte, 0, 16))
	err = WriteFrame(buf, f)
	bts = buf.Bytes()
	return
}

// MustCompileFrame is like CompileFrame but panics if frame can not be
// encoded.
func MustCompileFrame(f Frame) []byte {
	bts, err := CompileFrame(f)
	if err != nil {
		panic(err)
	}
	return bts
}

// Rsv creates rsv byte representation.
func Rsv(r1, r2, r3 bool) (rsv byte) {
	if r1 {
		rsv |= bit5
	}
	if r2 {
		rsv |= bit6
	}
	if r3 {
		rsv |= bit7
	}
	return rsv
}

func makeCloseFrame(code StatusCode) Frame {
	return NewCloseFrame(NewCloseFrameBody(code, ""))
}

var (
	closeFrameNormalClosure           = makeCloseFrame(StatusNormalClosure)
	closeFrameGoingAway               = makeCloseFrame(StatusGoingAway)
	closeFrameProtocolError           = makeCloseFrame(StatusProtocolError)
	closeFrameUnsupportedData         = makeCloseFrame(StatusUnsupportedData)
	closeFrameNoMeaningYet            = makeCloseFrame(StatusNoMeaningYet)
	closeFrameInvalidFramePayloadData = makeCloseFrame(StatusInvalidFramePayloadData)
	closeFramePolicyViolation         = makeCloseFrame(StatusPolicyViolation)
	closeFrameMessageTooBig           = makeCloseFrame(StatusMessageTooBig)
	closeFrameMandatoryExt            = makeCloseFrame(StatusMandatoryExt)
	closeFrameInternalServerError     = makeCloseFrame(StatusInternalServerError)
	closeFrameTLSHandshake            = makeCloseFrame(StatusTLSHandshake)
)

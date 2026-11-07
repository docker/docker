package msgp

import (
	"errors"
	"math"
	"strconv"
)

const (
	// Complex64Extension is the extension number used for complex64
	Complex64Extension = 3

	// Complex128Extension is the extension number used for complex128
	Complex128Extension = 4

	// TimeExtension is the extension number used for time.Time
	TimeExtension = 5

	// MsgTimeExtension is the extension number for timestamps as defined in
	// https://github.com/msgpack/msgpack/blob/master/spec.md#timestamp-extension-type
	MsgTimeExtension = -1
)

// msgTimeExtension is a painful workaround to avoid "constant -1 overflows byte".
var msgTimeExtension = int8(MsgTimeExtension)

// our extensions live here
var extensionReg = make(map[int8]func() Extension)

// RegisterExtension registers extensions so that they
// can be initialized and returned by methods that
// decode `interface{}` values. This should only
// be called during initialization. f() should return
// a newly-initialized zero value of the extension. Keep in
// mind that extensions 3, 4, and 5 are reserved for
// complex64, complex128, and time.Time, respectively,
// and that MessagePack reserves extension types from -127 to -1.
//
// For example, if you wanted to register a user-defined struct:
//
//	msgp.RegisterExtension(10, func() msgp.Extension { &MyExtension{} })
//
// RegisterExtension will panic if you call it multiple times
// with the same 'typ' argument, or if you use a reserved
// type (3, 4, or 5).
func RegisterExtension(typ int8, f func() Extension) {
	switch typ {
	case Complex64Extension, Complex128Extension, TimeExtension:
		panic(errors.New("msgp: forbidden extension type: " + strconv.Itoa(int(typ))))
	}
	if _, ok := extensionReg[typ]; ok {
		panic(errors.New("msgp: RegisterExtension() called with typ " + strconv.Itoa(int(typ)) + " more than once"))
	}
	extensionReg[typ] = f
}

// ExtensionTypeError is an error type returned
// when there is a mis-match between an extension type
// and the type encoded on the wire
type ExtensionTypeError struct {
	Got  int8
	Want int8
}

// Error implements the error interface
func (e ExtensionTypeError) Error() string {
	return "msgp: error decoding extension: wanted type " + strconv.Itoa(int(e.Want)) + "; got type " + strconv.Itoa(int(e.Got))
}

// Resumable returns 'true' for ExtensionTypeErrors
func (e ExtensionTypeError) Resumable() bool { return true }

func errExt(got int8, wanted int8) error {
	return ExtensionTypeError{Got: got, Want: wanted}
}

// Extension is the interface fulfilled
// by types that want to define their
// own binary encoding.
type Extension interface {
	// ExtensionType should return
	// a int8 that identifies the concrete
	// type of the extension. (Types <0 are
	// officially reserved by the MessagePack
	// specifications.)
	ExtensionType() int8

	// Len should return the length
	// of the data to be encoded
	Len() int

	// MarshalBinaryTo should copy
	// the data into the supplied slice,
	// assuming that the slice has length Len()
	MarshalBinaryTo([]byte) error

	UnmarshalBinary([]byte) error
}

// RawExtension implements the Extension interface
type RawExtension struct {
	Data []byte
	Type int8
}

// ExtensionType implements Extension.ExtensionType, and returns r.Type
func (r *RawExtension) ExtensionType() int8 { return r.Type }

// Len implements Extension.Len, and returns len(r.Data)
func (r *RawExtension) Len() int { return len(r.Data) }

// MarshalBinaryTo implements Extension.MarshalBinaryTo,
// and returns a copy of r.Data
func (r *RawExtension) MarshalBinaryTo(d []byte) error {
	copy(d, r.Data)
	return nil
}

// UnmarshalBinary implements Extension.UnmarshalBinary,
// and sets r.Data to the contents of the provided slice
func (r *RawExtension) UnmarshalBinary(b []byte) error {
	if cap(r.Data) >= len(b) {
		r.Data = r.Data[0:len(b)]
	} else {
		r.Data = make([]byte, len(b))
	}
	copy(r.Data, b)
	return nil
}

func (mw *Writer) writeExtensionHeader(length int, extType int8) error {
	switch length {
	case 0:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		mw.buf[o] = mext8
		mw.buf[o+1] = 0
		mw.buf[o+2] = byte(extType)
	case 1:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mfixext1
		mw.buf[o+1] = byte(extType)
	case 2:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mfixext2
		mw.buf[o+1] = byte(extType)
	case 4:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mfixext4
		mw.buf[o+1] = byte(extType)
	case 8:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mfixext8
		mw.buf[o+1] = byte(extType)
	case 16:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mfixext16
		mw.buf[o+1] = byte(extType)
	default:
		switch {
		case length < math.MaxUint8:
			o, err := mw.require(3)
			if err != nil {
				return err
			}
			mw.buf[o] = mext8
			mw.buf[o+1] = byte(uint8(length))
			mw.buf[o+2] = byte(extType)
		case length < math.MaxUint16:
			o, err := mw.require(4)
			if err != nil {
				return err
			}
			mw.buf[o] = mext16
			big.PutUint16(mw.buf[o+1:], uint16(length))
			mw.buf[o+3] = byte(extType)
		default:
			o, err := mw.require(6)
			if err != nil {
				return err
			}
			mw.buf[o] = mext32
			big.PutUint32(mw.buf[o+1:], uint32(length))
			mw.buf[o+5] = byte(extType)
		}
	}

	return nil
}

// WriteExtension writes an extension type to the writer
func (mw *Writer) WriteExtension(e Extension) error {
	length := e.Len()

	err := mw.writeExtensionHeader(length, e.ExtensionType())
	if err != nil {
		return err
	}

	// we can only write directly to the
	// buffer if we're sure that it
	// fits the object
	if length <= mw.bufsize() {
		o, err := mw.require(length)
		if err != nil {
			return err
		}
		return e.MarshalBinaryTo(mw.buf[o:])
	}
	// here we create a new buffer
	// just large enough for the body
	// and save it as the write buffer
	err = mw.flush()
	if err != nil {
		return err
	}
	buf := make([]byte, length)
	err = e.MarshalBinaryTo(buf)
	if err != nil {
		return err
	}
	mw.buf = buf
	mw.wloc = length
	return nil
}

// WriteExtensionRaw writes an extension type to the writer
func (mw *Writer) WriteExtensionRaw(extType int8, payload []byte) error {
	if err := mw.writeExtensionHeader(len(payload), extType); err != nil {
		return err
	}

	// instead of using mw.Write(), we'll copy the data through the internal
	// buffer, otherwise the payload would be moved to the heap
	// (meaning we can use stack-allocated buffers with zero allocations)
	for len(payload) > 0 {
		chunkSize := mw.avail()
		if chunkSize == 0 {
			if err := mw.flush(); err != nil {
				return err
			}
			chunkSize = mw.avail()
		}
		if chunkSize > len(payload) {
			chunkSize = len(payload)
		}

		mw.wloc += copy(mw.buf[mw.wloc:], payload[:chunkSize])
		payload = payload[chunkSize:]
	}

	return nil
}

// peek at the extension type, assuming the next
// kind to be read is Extension
func (m *Reader) peekExtensionType() (int8, error) {
	_, _, extType, err := m.peekExtensionHeader()

	return extType, err
}

// peekExtension peeks at the extension encoding type
// (must guarantee at least 1 byte in 'b')
func peekExtension(b []byte) (int8, error) {
	spec := getBytespec(b[0])
	size := spec.size
	if spec.typ != ExtensionType {
		return 0, badPrefix(ExtensionType, b[0])
	}
	if len(b) < int(size) {
		return 0, ErrShortBytes
	}
	// for fixed extensions,
	// the type information is in
	// the second byte
	if spec.extra == constsize {
		return int8(b[1]), nil
	}
	// otherwise, it's in the last
	// part of the prefix
	return int8(b[size-1]), nil
}

func (m *Reader) peekExtensionHeader() (offset int, length int, extType int8, err error) {
	var p []byte
	p, err = m.R.Peek(2)
	if err != nil {
		return
	}

	offset = 2

	lead := p[0]
	switch lead {
	case mfixext1:
		extType = int8(p[1])
		length = 1
		return

	case mfixext2:
		extType = int8(p[1])
		length = 2
		return

	case mfixext4:
		extType = int8(p[1])
		length = 4
		return

	case mfixext8:
		extType = int8(p[1])
		length = 8
		return

	case mfixext16:
		extType = int8(p[1])
		length = 16
		return

	case mext8:
		p, err = m.R.Peek(3)
		if err != nil {
			return
		}
		offset = 3
		extType = int8(p[2])
		length = int(uint8(p[1]))

	case mext16:
		p, err = m.R.Peek(4)
		if err != nil {
			return
		}
		offset = 4
		extType = int8(p[3])
		length = int(big.Uint16(p[1:]))

	case mext32:
		p, err = m.R.Peek(6)
		if err != nil {
			return
		}
		offset = 6
		extType = int8(p[5])
		length = int(big.Uint32(p[1:]))

	default:
		err = badPrefix(ExtensionType, lead)
		return
	}

	return
}

// ReadExtension reads the next object from the reader
// as an extension. ReadExtension will fail if the next
// object in the stream is not an extension, or if
// e.Type() is not the same as the wire type.
func (m *Reader) ReadExtension(e Extension) error {
	offset, length, extType, err := m.peekExtensionHeader()
	if err != nil {
		return err
	}

	if expectedType := e.ExtensionType(); extType != expectedType {
		return errExt(extType, expectedType)
	}
	if uint32(length) > m.GetMaxElements() {
		return ErrLimitExceeded
	}

	p, err := m.R.Peek(offset + length)
	if err != nil {
		return err
	}
	err = e.UnmarshalBinary(p[offset:])
	if err == nil {
		// consume the peeked bytes
		_, err = m.R.Skip(offset + length)
	}
	return err
}

// ReadExtensionRaw reads the next object from the reader
// as an extension. The returned slice is only
// valid until the next *Reader method call.
func (m *Reader) ReadExtensionRaw() (int8, []byte, error) {
	offset, length, extType, err := m.peekExtensionHeader()
	if err != nil {
		return 0, nil, err
	}
	if uint32(length) > m.GetMaxElements() {
		return 0, nil, ErrLimitExceeded
	}

	payload, err := m.R.Next(offset + length)
	if err != nil {
		return 0, nil, err
	}

	return extType, payload[offset:], nil
}

// AppendExtension appends a MessagePack extension to the provided slice
func AppendExtension(b []byte, e Extension) ([]byte, error) {
	l := e.Len()
	var o []byte
	var n int
	switch l {
	case 0:
		o, n = ensure(b, 3)
		o[n] = mext8
		o[n+1] = 0
		o[n+2] = byte(e.ExtensionType())
		return o[:n+3], nil
	case 1:
		o, n = ensure(b, 3)
		o[n] = mfixext1
		o[n+1] = byte(e.ExtensionType())
		n += 2
	case 2:
		o, n = ensure(b, 4)
		o[n] = mfixext2
		o[n+1] = byte(e.ExtensionType())
		n += 2
	case 4:
		o, n = ensure(b, 6)
		o[n] = mfixext4
		o[n+1] = byte(e.ExtensionType())
		n += 2
	case 8:
		o, n = ensure(b, 10)
		o[n] = mfixext8
		o[n+1] = byte(e.ExtensionType())
		n += 2
	case 16:
		o, n = ensure(b, 18)
		o[n] = mfixext16
		o[n+1] = byte(e.ExtensionType())
		n += 2
	default:
		switch {
		case l < math.MaxUint8:
			o, n = ensure(b, l+3)
			o[n] = mext8
			o[n+1] = byte(uint8(l))
			o[n+2] = byte(e.ExtensionType())
			n += 3
		case l < math.MaxUint16:
			o, n = ensure(b, l+4)
			o[n] = mext16
			big.PutUint16(o[n+1:], uint16(l))
			o[n+3] = byte(e.ExtensionType())
			n += 4
		default:
			o, n = ensure(b, l+6)
			o[n] = mext32
			big.PutUint32(o[n+1:], uint32(l))
			o[n+5] = byte(e.ExtensionType())
			n += 6
		}
	}
	return o, e.MarshalBinaryTo(o[n:])
}

// ReadExtensionBytes reads an extension from 'b' into 'e'
// and returns any remaining bytes.
// Possible errors:
// - ErrShortBytes ('b' not long enough)
// - ExtensionTypeError{} (wire type not the same as e.Type())
// - TypeError{} (next object not an extension)
// - InvalidPrefixError
// - An umarshal error returned from e.UnmarshalBinary
func ReadExtensionBytes(b []byte, e Extension) ([]byte, error) {
	typ, remain, data, err := readExt(b)
	if err != nil {
		return b, err
	}
	if typ != e.ExtensionType() {
		return b, errExt(typ, e.ExtensionType())
	}
	return remain, e.UnmarshalBinary(data)
}

// readExt will read the extension type, and return remaining bytes,
// as well as the data of the extension.
func readExt(b []byte) (typ int8, remain []byte, data []byte, err error) {
	l := len(b)
	if l < 3 {
		return 0, b, nil, ErrShortBytes
	}
	lead := b[0]
	var (
		sz  int // size of 'data'
		off int // offset of 'data'
	)
	switch lead {
	case mfixext1:
		typ = int8(b[1])
		sz = 1
		off = 2
	case mfixext2:
		typ = int8(b[1])
		sz = 2
		off = 2
	case mfixext4:
		typ = int8(b[1])
		sz = 4
		off = 2
	case mfixext8:
		typ = int8(b[1])
		sz = 8
		off = 2
	case mfixext16:
		typ = int8(b[1])
		sz = 16
		off = 2
	case mext8:
		sz = int(uint8(b[1]))
		typ = int8(b[2])
		off = 3
		if sz == 0 {
			return typ, b[3:], b[3:3], nil
		}
	case mext16:
		if l < 4 {
			return 0, b, nil, ErrShortBytes
		}
		sz = int(big.Uint16(b[1:]))
		typ = int8(b[3])
		off = 4
	case mext32:
		if l < 6 {
			return 0, b, nil, ErrShortBytes
		}
		sz = int(big.Uint32(b[1:]))
		typ = int8(b[5])
		off = 6
	default:
		return 0, b, nil, badPrefix(ExtensionType, lead)
	}
	// the data of the extension starts
	// at 'off' and is 'sz' bytes long
	tot := off + sz
	if len(b[off:]) < sz {
		return 0, b, nil, ErrShortBytes
	}
	return typ, b[tot:], b[off:tot:tot], nil
}

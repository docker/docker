package etw

import (
	"bytes"
	"encoding/binary"
)

// eventData maintains a buffer which builds up the data for an ETW event. It
// needs to be paired with EventMetadata which describes the event.
type eventData struct {
	buffer bytes.Buffer
}

// bytes returns the raw binary data containing the event data. The returned
// value is not copied from the internal buffer, so it can be mutated by the
// eventData object after it is returned.
func (ed *eventData) bytes() []byte {
	return ed.buffer.Bytes()
}

// writeString appends a string, including the null terminator, to the buffer.
func (ed *eventData) writeString(data string) {
	ed.buffer.WriteString(data)
	ed.buffer.WriteByte(0)
}

// writeInt8 appends a int8 to the buffer.
func (ed *eventData) writeInt8(value int8) {
	ed.buffer.WriteByte(uint8(value))
}

// writeInt16 appends a int16 to the buffer.
func (ed *eventData) writeInt16(value int16) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

// writeInt32 appends a int32 to the buffer.
func (ed *eventData) writeInt32(value int32) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

// writeInt64 appends a int64 to the buffer.
func (ed *eventData) writeInt64(value int64) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

// writeUint8 appends a uint8 to the buffer.
func (ed *eventData) writeUint8(value uint8) {
	ed.buffer.WriteByte(value)
}

// writeUint16 appends a uint16 to the buffer.
func (ed *eventData) writeUint16(value uint16) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

// writeUint32 appends a uint32 to the buffer.
func (ed *eventData) writeUint32(value uint32) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

// writeUint64 appends a uint64 to the buffer.
func (ed *eventData) writeUint64(value uint64) {
	binary.Write(&ed.buffer, binary.LittleEndian, value)
}

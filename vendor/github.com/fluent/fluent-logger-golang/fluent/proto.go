//go:generate msgp

package fluent

import (
	"time"

	"github.com/tinylib/msgp/msgp"
)

//msgp:tuple Entry
type Entry struct {
	Time   int64       `msg:"time"`
	Record interface{} `msg:"record"`
}

//msgp:tuple Forward
type Forward struct {
	Tag     string  `msg:"tag"`
	Entries []Entry `msg:"entries"`
	Option  map[string]string
}

//msgp:tuple Message
type Message struct {
	Tag    string      `msg:"tag"`
	Time   int64       `msg:"time"`
	Record interface{} `msg:"record"`
	Option map[string]string
}

//msgp:tuple MessageExt
type MessageExt struct {
	Tag    string      `msg:"tag"`
	Time   EventTime   `msg:"time,extension"`
	Record interface{} `msg:"record"`
	Option map[string]string
}

type AckResp struct {
	Ack string `json:"ack" msg:"ack"`
}

// EventTime is an extension to the serialized time value. It builds in support
// for sub-second (nanosecond) precision in serialized timestamps.
//
// You can find the full specification for the msgpack message payload here:
// https://github.com/fluent/fluentd/wiki/Forward-Protocol-Specification-v1.
//
// You can find more information on msgpack extension types here:
// https://github.com/tinylib/msgp/wiki/Using-Extensions.
type EventTime time.Time

const (
	extensionType = 0
	length        = 8
)

func init() {
	msgp.RegisterExtension(extensionType, func() msgp.Extension { return new(EventTime) })
}

func (t *EventTime) ExtensionType() int8 { return extensionType }

func (t *EventTime) Len() int { return length }

func (t *EventTime) MarshalBinaryTo(b []byte) error {
	// Unwrap to Golang time
	goTime := time.Time(*t)

	// There's no support for timezones in fluentd's protocol for EventTime.
	// Convert to UTC.
	utc := goTime.UTC()

	// Warning! Converting seconds to an int32 is a lossy operation. This code
	// will hit the "Year 2038" problem.
	sec := int32(utc.Unix())
	nsec := utc.Nanosecond()

	// Fill the buffer with 4 bytes for the second component of the timestamp.
	b[0] = byte(sec >> 24)
	b[1] = byte(sec >> 16)
	b[2] = byte(sec >> 8)
	b[3] = byte(sec)

	// Fill the buffer with 4 bytes for the nanosecond component of the
	// timestamp.
	b[4] = byte(nsec >> 24)
	b[5] = byte(nsec >> 16)
	b[6] = byte(nsec >> 8)
	b[7] = byte(nsec)

	return nil
}

// UnmarshalBinary is not implemented since decoding messages is not supported
// by this library.
func (t *EventTime) UnmarshalBinary(b []byte) error {
	return nil
}

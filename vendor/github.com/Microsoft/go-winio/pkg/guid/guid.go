package guid

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

var _ = (json.Marshaler)(&GUID{})
var _ = (json.Unmarshaler)(&GUID{})

// GUID represents a GUID/UUID. It has the same structure as
// golang.org/x/sys/windows.GUID so that it can be used with functions expecting
// that type. It is defined as its own type so that stringification and
// marshaling can be supported. The representation matches that used by native
// Windows code.
type GUID windows.GUID

// NewV4 returns a new version 4 (pseudorandom) GUID, as defined by RFC 4122.
func NewV4() (*GUID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}

	var g GUID
	g.Data1 = binary.LittleEndian.Uint32(b[0:4])
	g.Data2 = binary.LittleEndian.Uint16(b[4:6])
	g.Data3 = binary.LittleEndian.Uint16(b[6:8])
	copy(g.Data4[:], b[8:16])

	g.Data3 = (g.Data3 & 0x0fff) | 0x4000   // Version 4 (randomly generated)
	g.Data4[0] = (g.Data4[0] & 0x3f) | 0x80 // RFC4122 variant
	return &g, nil
}

func (g *GUID) String() string {
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		g.Data1,
		g.Data2,
		g.Data3,
		g.Data4[:2],
		g.Data4[2:])
}

// FromString parses a string containing a GUID and returns the GUID. The only
// format currently supported is the `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`
// format.
func FromString(s string) (*GUID, error) {
	if len(s) != 36 {
		return nil, errors.New("invalid GUID format (length)")
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return nil, errors.New("invalid GUID format (dashes)")
	}

	var g GUID

	data1, err := strconv.ParseUint(s[0:8], 16, 32)
	if err != nil {
		return nil, errors.Wrap(err, "invalid GUID format (Data1)")
	}
	g.Data1 = uint32(data1)

	data2, err := strconv.ParseUint(s[9:13], 16, 16)
	if err != nil {
		return nil, errors.Wrap(err, "invalid GUID format (Data2)")
	}
	g.Data2 = uint16(data2)

	data3, err := strconv.ParseUint(s[14:18], 16, 16)
	if err != nil {
		return nil, errors.Wrap(err, "invalid GUID format (Data3)")
	}
	g.Data3 = uint16(data3)

	for i, x := range []int{19, 21, 24, 26, 28, 30, 32, 34} {
		v, err := strconv.ParseUint(s[x:x+2], 16, 8)
		if err != nil {
			return nil, errors.Wrap(err, "invalid GUID format (Data4)")
		}
		g.Data4[i] = uint8(v)
	}

	return &g, nil
}

// MarshalJSON marshals the GUID to JSON representation and returns it as a
// slice of bytes.
func (g *GUID) MarshalJSON() ([]byte, error) {
	return json.Marshal(g.String())
}

// UnmarshalJSON unmarshals a GUID from JSON representation and sets itself to
// the unmarshaled GUID.
func (g *GUID) UnmarshalJSON(data []byte) error {
	g2, err := FromString(strings.Trim(string(data), "\""))
	if err != nil {
		return err
	}
	*g = *g2
	return nil
}

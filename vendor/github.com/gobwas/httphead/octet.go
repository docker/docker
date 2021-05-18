package httphead

// OctetType desribes character type.
//
// From the "Basic Rules" chapter of RFC2616
// See https://tools.ietf.org/html/rfc2616#section-2.2
//
// OCTET          = <any 8-bit sequence of data>
// CHAR           = <any US-ASCII character (octets 0 - 127)>
// UPALPHA        = <any US-ASCII uppercase letter "A".."Z">
// LOALPHA        = <any US-ASCII lowercase letter "a".."z">
// ALPHA          = UPALPHA | LOALPHA
// DIGIT          = <any US-ASCII digit "0".."9">
// CTL            = <any US-ASCII control character (octets 0 - 31) and DEL (127)>
// CR             = <US-ASCII CR, carriage return (13)>
// LF             = <US-ASCII LF, linefeed (10)>
// SP             = <US-ASCII SP, space (32)>
// HT             = <US-ASCII HT, horizontal-tab (9)>
// <">            = <US-ASCII double-quote mark (34)>
// CRLF           = CR LF
// LWS            = [CRLF] 1*( SP | HT )
//
// Many HTTP/1.1 header field values consist of words separated by LWS
// or special characters. These special characters MUST be in a quoted
// string to be used within a parameter value (as defined in section
// 3.6).
//
// token          = 1*<any CHAR except CTLs or separators>
// separators     = "(" | ")" | "<" | ">" | "@"
// | "," | ";" | ":" | "\" | <">
// | "/" | "[" | "]" | "?" | "="
// | "{" | "}" | SP | HT
type OctetType byte

// IsChar reports whether octet is CHAR.
func (t OctetType) IsChar() bool { return t&octetChar != 0 }

// IsControl reports whether octet is CTL.
func (t OctetType) IsControl() bool { return t&octetControl != 0 }

// IsSeparator reports whether octet is separator.
func (t OctetType) IsSeparator() bool { return t&octetSeparator != 0 }

// IsSpace reports whether octet is space (SP or HT).
func (t OctetType) IsSpace() bool { return t&octetSpace != 0 }

// IsToken reports whether octet is token.
func (t OctetType) IsToken() bool { return t&octetToken != 0 }

const (
	octetChar OctetType = 1 << iota
	octetControl
	octetSpace
	octetSeparator
	octetToken
)

// OctetTypes is a table of octets.
var OctetTypes [256]OctetType

func init() {
	for c := 32; c < 256; c++ {
		var t OctetType
		if c <= 127 {
			t |= octetChar
		}
		if 0 <= c && c <= 31 || c == 127 {
			t |= octetControl
		}
		switch c {
		case '(', ')', '<', '>', '@', ',', ';', ':', '"', '/', '[', ']', '?', '=', '{', '}', '\\':
			t |= octetSeparator
		case ' ', '\t':
			t |= octetSpace | octetSeparator
		}

		if t.IsChar() && !t.IsControl() && !t.IsSeparator() && !t.IsSpace() {
			t |= octetToken
		}

		OctetTypes[c] = t
	}
}

package httphead

import (
	"bytes"
)

// ScanCookie scans cookie pairs from data using DefaultCookieScanner.Scan()
// method.
func ScanCookie(data []byte, it func(key, value []byte) bool) bool {
	return DefaultCookieScanner.Scan(data, it)
}

// DefaultCookieScanner is a CookieScanner which is used by ScanCookie().
// Note that it is intended to have the same behavior as http.Request.Cookies()
// has.
var DefaultCookieScanner = CookieScanner{}

// CookieScanner contains options for scanning cookie pairs.
// See https://tools.ietf.org/html/rfc6265#section-4.1.1
type CookieScanner struct {
	// DisableNameValidation disables name validation of a cookie. If false,
	// only RFC2616 "tokens" are accepted.
	DisableNameValidation bool

	// DisableValueValidation disables value validation of a cookie. If false,
	// only RFC6265 "cookie-octet" characters are accepted.
	//
	// Note that Strict option also affects validation of a value.
	//
	// If Strict is false, then scanner begins to allow space and comma
	// characters inside the value for better compatibility with non standard
	// cookies implementations.
	DisableValueValidation bool

	// BreakOnPairError sets scanner to immediately return after first pair syntax
	// validation error.
	// If false, scanner will try to skip invalid pair bytes and go ahead.
	BreakOnPairError bool

	// Strict enables strict RFC6265 mode scanning. It affects name and value
	// validation, as also some other rules.
	// If false, it is intended to bring the same behavior as
	// http.Request.Cookies().
	Strict bool
}

// Scan maps data to name and value pairs. Usually data represents value of the
// Cookie header.
func (c CookieScanner) Scan(data []byte, it func(name, value []byte) bool) bool {
	lexer := &Scanner{data: data}

	const (
		statePair = iota
		stateBefore
	)

	state := statePair

	for lexer.Buffered() > 0 {
		switch state {
		case stateBefore:
			// Pairs separated by ";" and space, according to the RFC6265:
			//   cookie-pair *( ";" SP cookie-pair )
			//
			// Cookie pairs MUST be separated by (";" SP). So our only option
			// here is to fail as syntax error.
			a, b := lexer.Peek2()
			if a != ';' {
				return false
			}

			state = statePair

			advance := 1
			if b == ' ' {
				advance++
			} else if c.Strict {
				return false
			}

			lexer.Advance(advance)

		case statePair:
			if !lexer.FetchUntil(';') {
				return false
			}

			var value []byte
			name := lexer.Bytes()
			if i := bytes.IndexByte(name, '='); i != -1 {
				value = name[i+1:]
				name = name[:i]
			} else if c.Strict {
				if !c.BreakOnPairError {
					goto nextPair
				}
				return false
			}

			if !c.Strict {
				trimLeft(name)
			}
			if !c.DisableNameValidation && !ValidCookieName(name) {
				if !c.BreakOnPairError {
					goto nextPair
				}
				return false
			}

			if !c.Strict {
				value = trimRight(value)
			}
			value = stripQuotes(value)
			if !c.DisableValueValidation && !ValidCookieValue(value, c.Strict) {
				if !c.BreakOnPairError {
					goto nextPair
				}
				return false
			}

			if !it(name, value) {
				return true
			}

		nextPair:
			state = stateBefore
		}
	}

	return true
}

// ValidCookieValue reports whether given value is a valid RFC6265
// "cookie-octet" bytes.
//
// cookie-octet = %x21 / %x23-2B / %x2D-3A / %x3C-5B / %x5D-7E
//                ; US-ASCII characters excluding CTLs,
//                ; whitespace DQUOTE, comma, semicolon,
//                ; and backslash
//
// Note that the false strict parameter disables errors on space 0x20 and comma
// 0x2c. This could be useful to bring some compatibility with non-compliant
// clients/servers in the real world.
// It acts the same as standard library cookie parser if strict is false.
func ValidCookieValue(value []byte, strict bool) bool {
	if len(value) == 0 {
		return true
	}
	for _, c := range value {
		switch c {
		case '"', ';', '\\':
			return false
		case ',', ' ':
			if strict {
				return false
			}
		default:
			if c <= 0x20 {
				return false
			}
			if c >= 0x7f {
				return false
			}
		}
	}
	return true
}

// ValidCookieName reports wheter given bytes is a valid RFC2616 "token" bytes.
func ValidCookieName(name []byte) bool {
	for _, c := range name {
		if !OctetTypes[c].IsToken() {
			return false
		}
	}
	return true
}

func stripQuotes(bts []byte) []byte {
	if last := len(bts) - 1; last > 0 && bts[0] == '"' && bts[last] == '"' {
		return bts[1:last]
	}
	return bts
}

func trimLeft(p []byte) []byte {
	var i int
	for i < len(p) && OctetTypes[p[i]].IsSpace() {
		i++
	}
	return p[i:]
}

func trimRight(p []byte) []byte {
	j := len(p)
	for j > 0 && OctetTypes[p[j-1]].IsSpace() {
		j--
	}
	return p[:j]
}

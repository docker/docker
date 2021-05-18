package httphead

import (
	"bufio"
	"bytes"
)

// Version contains protocol major and minor version.
type Version struct {
	Major int
	Minor int
}

// RequestLine contains parameters parsed from the first request line.
type RequestLine struct {
	Method  []byte
	URI     []byte
	Version Version
}

// ResponseLine contains parameters parsed from the first response line.
type ResponseLine struct {
	Version Version
	Status  int
	Reason  []byte
}

// SplitRequestLine splits given slice of bytes into three chunks without
// parsing.
func SplitRequestLine(line []byte) (method, uri, version []byte) {
	return split3(line, ' ')
}

// ParseRequestLine parses http request line like "GET / HTTP/1.0".
func ParseRequestLine(line []byte) (r RequestLine, ok bool) {
	var i int
	for i = 0; i < len(line); i++ {
		c := line[i]
		if !OctetTypes[c].IsToken() {
			if i > 0 && c == ' ' {
				break
			}
			return
		}
	}
	if i == len(line) {
		return
	}

	var proto []byte
	r.Method = line[:i]
	r.URI, proto = split2(line[i+1:], ' ')
	if len(r.URI) == 0 {
		return
	}
	if major, minor, ok := ParseVersion(proto); ok {
		r.Version.Major = major
		r.Version.Minor = minor
		return r, true
	}

	return r, false
}

// SplitResponseLine splits given slice of bytes into three chunks without
// parsing.
func SplitResponseLine(line []byte) (version, status, reason []byte) {
	return split3(line, ' ')
}

// ParseResponseLine parses first response line into ResponseLine struct.
func ParseResponseLine(line []byte) (r ResponseLine, ok bool) {
	var (
		proto  []byte
		status []byte
	)
	proto, status, r.Reason = split3(line, ' ')
	if major, minor, ok := ParseVersion(proto); ok {
		r.Version.Major = major
		r.Version.Minor = minor
	} else {
		return r, false
	}
	if n, ok := IntFromASCII(status); ok {
		r.Status = n
	} else {
		return r, false
	}
	// TODO(gobwas): parse here r.Reason fot TEXT rule:
	//   TEXT = <any OCTET except CTLs,
	//           but including LWS>
	return r, true
}

var (
	httpVersion10     = []byte("HTTP/1.0")
	httpVersion11     = []byte("HTTP/1.1")
	httpVersionPrefix = []byte("HTTP/")
)

// ParseVersion parses major and minor version of HTTP protocol.
// It returns parsed values and true if parse is ok.
func ParseVersion(bts []byte) (major, minor int, ok bool) {
	switch {
	case bytes.Equal(bts, httpVersion11):
		return 1, 1, true
	case bytes.Equal(bts, httpVersion10):
		return 1, 0, true
	case len(bts) < 8:
		return
	case !bytes.Equal(bts[:5], httpVersionPrefix):
		return
	}

	bts = bts[5:]

	dot := bytes.IndexByte(bts, '.')
	if dot == -1 {
		return
	}
	major, ok = IntFromASCII(bts[:dot])
	if !ok {
		return
	}
	minor, ok = IntFromASCII(bts[dot+1:])
	if !ok {
		return
	}

	return major, minor, true
}

// ReadLine reads line from br. It reads until '\n' and returns bytes without
// '\n' or '\r\n' at the end.
// It returns err if and only if line does not end in '\n'. Note that read
// bytes returned in any case of error.
//
// It is much like the textproto/Reader.ReadLine() except the thing that it
// returns raw bytes, instead of string. That is, it avoids copying bytes read
// from br.
//
// textproto/Reader.ReadLineBytes() is also makes copy of resulting bytes to be
// safe with future I/O operations on br.
//
// We could control I/O operations on br and do not need to make additional
// copy for safety.
func ReadLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		bts, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Copy bytes because next read will discard them.
			line = append(line, bts...)
			continue
		}
		// Avoid copy of single read.
		if line == nil {
			line = bts
		} else {
			line = append(line, bts...)
		}
		if err != nil {
			return line, err
		}
		// Size of line is at least 1.
		// In other case bufio.ReadSlice() returns error.
		n := len(line)
		// Cut '\n' or '\r\n'.
		if n > 1 && line[n-2] == '\r' {
			line = line[:n-2]
		} else {
			line = line[:n-1]
		}
		return line, nil
	}
}

// ParseHeaderLine parses HTTP header as key-value pair. It returns parsed
// values and true if parse is ok.
func ParseHeaderLine(line []byte) (k, v []byte, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon == -1 {
		return
	}
	k = trim(line[:colon])
	for _, c := range k {
		if !OctetTypes[c].IsToken() {
			return nil, nil, false
		}
	}
	v = trim(line[colon+1:])
	return k, v, true
}

// IntFromASCII converts ascii encoded decimal numeric value from HTTP entities
// to an integer.
func IntFromASCII(bts []byte) (ret int, ok bool) {
	// ASCII numbers all start with the high-order bits 0011.
	// If you see that, and the next bits are 0-9 (0000 - 1001) you can grab those
	// bits and interpret them directly as an integer.
	var n int
	if n = len(bts); n < 1 {
		return 0, false
	}
	for i := 0; i < n; i++ {
		if bts[i]&0xf0 != 0x30 {
			return 0, false
		}
		ret += int(bts[i]&0xf) * pow(10, n-i-1)
	}
	return ret, true
}

const (
	toLower = 'a' - 'A'      // for use with OR.
	toUpper = ^byte(toLower) // for use with AND.
)

// CanonicalizeHeaderKey is like standard textproto/CanonicalMIMEHeaderKey,
// except that it operates with slice of bytes and modifies it inplace without
// copying.
func CanonicalizeHeaderKey(k []byte) {
	upper := true
	for i, c := range k {
		if upper && 'a' <= c && c <= 'z' {
			k[i] &= toUpper
		} else if !upper && 'A' <= c && c <= 'Z' {
			k[i] |= toLower
		}
		upper = c == '-'
	}
}

// pow for integers implementation.
// See Donald Knuth, The Art of Computer Programming, Volume 2, Section 4.6.3
func pow(a, b int) int {
	p := 1
	for b > 0 {
		if b&1 != 0 {
			p *= a
		}
		b >>= 1
		a *= a
	}
	return p
}

func split3(p []byte, sep byte) (p1, p2, p3 []byte) {
	a := bytes.IndexByte(p, sep)
	b := bytes.IndexByte(p[a+1:], sep)
	if a == -1 || b == -1 {
		return p, nil, nil
	}
	b += a + 1
	return p[:a], p[a+1 : b], p[b+1:]
}

func split2(p []byte, sep byte) (p1, p2 []byte) {
	i := bytes.IndexByte(p, sep)
	if i == -1 {
		return p, nil
	}
	return p[:i], p[i+1:]
}

func trim(p []byte) []byte {
	var i, j int
	for i = 0; i < len(p) && (p[i] == ' ' || p[i] == '\t'); {
		i++
	}
	for j = len(p); j > i && (p[j-1] == ' ' || p[j-1] == '\t'); {
		j--
	}
	return p[i:j]
}

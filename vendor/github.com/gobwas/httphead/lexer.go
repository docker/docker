package httphead

import (
	"bytes"
)

// ItemType encodes type of the lexing token.
type ItemType int

const (
	// ItemUndef reports that token is undefined.
	ItemUndef ItemType = iota
	// ItemToken reports that token is RFC2616 token.
	ItemToken
	// ItemSeparator reports that token is RFC2616 separator.
	ItemSeparator
	// ItemString reports that token is RFC2616 quouted string.
	ItemString
	// ItemComment reports that token is RFC2616 comment.
	ItemComment
	// ItemOctet reports that token is octet slice.
	ItemOctet
)

// Scanner represents header tokens scanner.
// See https://tools.ietf.org/html/rfc2616#section-2
type Scanner struct {
	data []byte
	pos  int

	itemType  ItemType
	itemBytes []byte

	err bool
}

// NewScanner creates new RFC2616 data scanner.
func NewScanner(data []byte) *Scanner {
	return &Scanner{data: data}
}

// Next scans for next token. It returns true on successful scanning, and false
// on error or EOF.
func (l *Scanner) Next() bool {
	c, ok := l.nextChar()
	if !ok {
		return false
	}
	switch c {
	case '"': // quoted-string;
		return l.fetchQuotedString()

	case '(': // comment;
		return l.fetchComment()

	case '\\', ')': // unexpected chars;
		l.err = true
		return false

	default:
		return l.fetchToken()
	}
}

// FetchUntil fetches ItemOctet from current scanner position to first
// occurence of the c or to the end of the underlying data.
func (l *Scanner) FetchUntil(c byte) bool {
	l.resetItem()
	if l.pos == len(l.data) {
		return false
	}
	return l.fetchOctet(c)
}

// Peek reads byte at current position without advancing it. On end of data it
// returns 0.
func (l *Scanner) Peek() byte {
	if l.pos == len(l.data) {
		return 0
	}
	return l.data[l.pos]
}

// Peek2 reads two first bytes at current position without advancing it.
// If there not enough data it returs 0.
func (l *Scanner) Peek2() (a, b byte) {
	if l.pos == len(l.data) {
		return 0, 0
	}
	if l.pos+1 == len(l.data) {
		return l.data[l.pos], 0
	}
	return l.data[l.pos], l.data[l.pos+1]
}

// Buffered reporst how many bytes there are left to scan.
func (l *Scanner) Buffered() int {
	return len(l.data) - l.pos
}

// Advance moves current position index at n bytes. It returns true on
// successful move.
func (l *Scanner) Advance(n int) bool {
	l.pos += n
	if l.pos > len(l.data) {
		l.pos = len(l.data)
		return false
	}
	return true
}

// Skip skips all bytes until first occurence of c.
func (l *Scanner) Skip(c byte) {
	if l.err {
		return
	}
	// Reset scanner state.
	l.resetItem()

	if i := bytes.IndexByte(l.data[l.pos:], c); i == -1 {
		// Reached the end of data.
		l.pos = len(l.data)
	} else {
		l.pos += i + 1
	}
}

// SkipEscaped skips all bytes until first occurence of non-escaped c.
func (l *Scanner) SkipEscaped(c byte) {
	if l.err {
		return
	}
	// Reset scanner state.
	l.resetItem()

	if i := ScanUntil(l.data[l.pos:], c); i == -1 {
		// Reached the end of data.
		l.pos = len(l.data)
	} else {
		l.pos += i + 1
	}
}

// Type reports current token type.
func (l *Scanner) Type() ItemType {
	return l.itemType
}

// Bytes returns current token bytes.
func (l *Scanner) Bytes() []byte {
	return l.itemBytes
}

func (l *Scanner) nextChar() (byte, bool) {
	// Reset scanner state.
	l.resetItem()

	if l.err {
		return 0, false
	}
	l.pos += SkipSpace(l.data[l.pos:])
	if l.pos == len(l.data) {
		return 0, false
	}
	return l.data[l.pos], true
}

func (l *Scanner) resetItem() {
	l.itemType = ItemUndef
	l.itemBytes = nil
}

func (l *Scanner) fetchOctet(c byte) bool {
	i := l.pos
	if j := bytes.IndexByte(l.data[l.pos:], c); j == -1 {
		// Reached the end of data.
		l.pos = len(l.data)
	} else {
		l.pos += j
	}

	l.itemType = ItemOctet
	l.itemBytes = l.data[i:l.pos]

	return true
}

func (l *Scanner) fetchToken() bool {
	n, t := ScanToken(l.data[l.pos:])
	if n == -1 {
		l.err = true
		return false
	}

	l.itemType = t
	l.itemBytes = l.data[l.pos : l.pos+n]
	l.pos += n

	return true
}

func (l *Scanner) fetchQuotedString() (ok bool) {
	l.pos++

	n := ScanUntil(l.data[l.pos:], '"')
	if n == -1 {
		l.err = true
		return false
	}

	l.itemType = ItemString
	l.itemBytes = RemoveByte(l.data[l.pos:l.pos+n], '\\')
	l.pos += n + 1

	return true
}

func (l *Scanner) fetchComment() (ok bool) {
	l.pos++

	n := ScanPairGreedy(l.data[l.pos:], '(', ')')
	if n == -1 {
		l.err = true
		return false
	}

	l.itemType = ItemComment
	l.itemBytes = RemoveByte(l.data[l.pos:l.pos+n], '\\')
	l.pos += n + 1

	return true
}

// ScanUntil scans for first non-escaped character c in given data.
// It returns index of matched c and -1 if c is not found.
func ScanUntil(data []byte, c byte) (n int) {
	for {
		i := bytes.IndexByte(data[n:], c)
		if i == -1 {
			return -1
		}
		n += i
		if n == 0 || data[n-1] != '\\' {
			break
		}
		n++
	}
	return
}

// ScanPairGreedy scans for complete pair of opening and closing chars in greedy manner.
// Note that first opening byte must not be present in data.
func ScanPairGreedy(data []byte, open, close byte) (n int) {
	var m int
	opened := 1
	for {
		i := bytes.IndexByte(data[n:], close)
		if i == -1 {
			return -1
		}
		n += i
		// If found index is not escaped then it is the end.
		if n == 0 || data[n-1] != '\\' {
			opened--
		}

		for m < i {
			j := bytes.IndexByte(data[m:i], open)
			if j == -1 {
				break
			}
			m += j + 1
			opened++
		}

		if opened == 0 {
			break
		}

		n++
		m = n
	}
	return
}

// RemoveByte returns data without c. If c is not present in data it returns
// the same slice. If not, it copies data without c.
func RemoveByte(data []byte, c byte) []byte {
	j := bytes.IndexByte(data, c)
	if j == -1 {
		return data
	}

	n := len(data) - 1

	// If character is present, than allocate slice with n-1 capacity. That is,
	// resulting bytes could be at most n-1 length.
	result := make([]byte, n)
	k := copy(result, data[:j])

	for i := j + 1; i < n; {
		j = bytes.IndexByte(data[i:], c)
		if j != -1 {
			k += copy(result[k:], data[i:i+j])
			i = i + j + 1
		} else {
			k += copy(result[k:], data[i:])
			break
		}
	}

	return result[:k]
}

// SkipSpace skips spaces and lws-sequences from p.
// It returns number ob bytes skipped.
func SkipSpace(p []byte) (n int) {
	for len(p) > 0 {
		switch {
		case len(p) >= 3 &&
			p[0] == '\r' &&
			p[1] == '\n' &&
			OctetTypes[p[2]].IsSpace():
			p = p[3:]
			n += 3
		case OctetTypes[p[0]].IsSpace():
			p = p[1:]
			n++
		default:
			return
		}
	}
	return
}

// ScanToken scan for next token in p. It returns length of the token and its
// type. It do not trim p.
func ScanToken(p []byte) (n int, t ItemType) {
	if len(p) == 0 {
		return 0, ItemUndef
	}

	c := p[0]
	switch {
	case OctetTypes[c].IsSeparator():
		return 1, ItemSeparator

	case OctetTypes[c].IsToken():
		for n = 1; n < len(p); n++ {
			c := p[n]
			if !OctetTypes[c].IsToken() {
				break
			}
		}
		return n, ItemToken

	default:
		return -1, ItemUndef
	}
}

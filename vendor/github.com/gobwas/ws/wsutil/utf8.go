package wsutil

import (
	"fmt"
	"io"
)

// ErrInvalidUTF8 is returned by UTF8 reader on invalid utf8 sequence.
var ErrInvalidUTF8 = fmt.Errorf("invalid utf8")

// UTF8Reader implements io.Reader that calculates utf8 validity state after
// every read byte from Source.
//
// Note that in some cases client must call r.Valid() after all bytes are read
// to ensure that all of them are valid utf8 sequences. That is, some io helper
// functions such io.ReadAtLeast or io.ReadFull could discard the error
// information returned by the reader when they receive all of requested bytes.
// For example, the last read sequence is invalid and UTF8Reader returns number
// of bytes read and an error. But helper function decides to discard received
// error due to all requested bytes are completely read from the source.
//
// Another possible case is when some valid sequence become split by the read
// bound. Then UTF8Reader can not make decision about validity of the last
// sequence cause it is not fully read yet. And if the read stops, Valid() will
// return false, even if Read() by itself dit not.
type UTF8Reader struct {
	Source io.Reader

	accepted int

	state uint32
	codep uint32
}

// NewUTF8Reader creates utf8 reader that reads from r.
func NewUTF8Reader(r io.Reader) *UTF8Reader {
	return &UTF8Reader{
		Source: r,
	}
}

// Reset resets utf8 reader to read from r.
func (u *UTF8Reader) Reset(r io.Reader) {
	u.Source = r
	u.state = 0
	u.codep = 0
}

// Read implements io.Reader.
func (u *UTF8Reader) Read(p []byte) (n int, err error) {
	n, err = u.Source.Read(p)

	accepted := 0
	s, c := u.state, u.codep
	for i := 0; i < n; i++ {
		c, s = decode(s, c, p[i])
		if s == utf8Reject {
			u.state = s
			return accepted, ErrInvalidUTF8
		}
		if s == utf8Accept {
			accepted = i + 1
		}
	}
	u.state, u.codep = s, c
	u.accepted = accepted

	return
}

// Valid checks current reader state. It returns true if all read bytes are
// valid UTF-8 sequences, and false if not.
func (u *UTF8Reader) Valid() bool {
	return u.state == utf8Accept
}

// Accepted returns number of valid bytes in last Read().
func (u *UTF8Reader) Accepted() int {
	return u.accepted
}

// Below is port of UTF-8 decoder from http://bjoern.hoehrmann.de/utf-8/decoder/dfa/
//
// Copyright (c) 2008-2009 Bjoern Hoehrmann <bjoern@hoehrmann.de>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to
// deal in the Software without restriction, including without limitation the
// rights to use, copy, modify, merge, publish, distribute, sublicense, and/or
// sell copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
// FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS
// IN THE SOFTWARE.

const (
	utf8Accept = 0
	utf8Reject = 12
)

var utf8d = [...]byte{
	// The first part of the table maps bytes to character classes that
	// to reduce the size of the transition table and create bitmasks.
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	8, 8, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
	10, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 4, 3, 3, 11, 6, 6, 6, 5, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,

	// The second part is a transition table that maps a combination
	// of a state of the automaton and a character class to a state.
	0, 12, 24, 36, 60, 96, 84, 12, 12, 12, 48, 72, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12,
	12, 0, 12, 12, 12, 12, 12, 0, 12, 0, 12, 12, 12, 24, 12, 12, 12, 12, 12, 24, 12, 24, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 24, 12, 12, 12, 12, 12, 24, 12, 12, 12, 12, 12, 12, 12, 24, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 36, 12, 36, 12, 12, 12, 36, 12, 12, 12, 12, 12, 36, 12, 36, 12, 12,
	12, 36, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12,
}

func decode(state, codep uint32, b byte) (uint32, uint32) {
	t := uint32(utf8d[b])

	if state != utf8Accept {
		codep = (uint32(b) & 0x3f) | (codep << 6)
	} else {
		codep = (0xff >> t) & uint32(b)
	}

	return codep, uint32(utf8d[256+state+t])
}

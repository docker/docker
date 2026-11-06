// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

package huff0

import (
	"github.com/klauspost/compress/internal/le"
	"github.com/klauspost/compress/internal/regmask"
)

// bitState is a bit writer that stays in registers when passed and
// returned by value. It writes into a buffer owned by the caller.
type bitState struct {
	c   uint64
	nb  uint
	pos int
}

// flush writes the whole bytes with one 8-byte store at pos, leaving
// at most 7 bits. buf must have 8 bytes of room at pos.
func (s bitState) flush(buf []byte) bitState {
	le.Store64(buf, s.pos, s.c)
	s.pos += int(s.nb >> 3)
	s.c >>= (s.nb &^ 7) & regmask.Shift64ByUint
	s.nb &= 7
	return s
}

// add adds n bits from v, which must have no bits set above them.
// There must be room for them.
func (s bitState) add(v uint64, n uint) bitState {
	s.c |= v << (s.nb & regmask.Shift64ByUint)
	s.nb += n
	return s
}

// fourSymbols returns the codes for a, b, c and d packed in that order
// from the LSB, and their total length.
func fourSymbols(a, b, c, d cTableEntry) (uint64, uint) {
	bitsA := uint(a.nBits)
	bitsB := bitsA + uint(b.nBits)
	bitsC := bitsB + uint(c.nBits)
	v := uint64(a.val) |
		uint64(b.val)<<(bitsA&regmask.Shift64ByUint) |
		uint64(c.val)<<(bitsB&regmask.Shift64ByUint) |
		uint64(d.val)<<(bitsC&regmask.Shift64ByUint)
	return v, bitsC + uint(d.nBits)
}

// bitWriter will write bits.
// First bit will be LSB of the first byte of output.
type bitWriter struct {
	bitContainer uint64
	nBits        uint8
	out          []byte
}

// addBits16Clean will add up to 16 bits. value may not contain more set bits than indicated.
// It will not check if there is space for them, so the caller must ensure that it has flushed recently.
func (b *bitWriter) addBits16Clean(value uint16, bits uint8) {
	b.bitContainer |= uint64(value) << (b.nBits & 63)
	b.nBits += bits
}

// encSymbol will add up to 16 bits. value may not contain more set bits than indicated.
// It will not check if there is space for them, so the caller must ensure that it has flushed recently.
func (b *bitWriter) encSymbol(ct cTable, symbol byte) {
	enc := ct[symbol]
	b.bitContainer |= uint64(enc.val) << (b.nBits & 63)
	if false {
		if enc.nBits == 0 {
			panic("nbits 0")
		}
	}
	b.nBits += enc.nBits
}

// flushAlign will flush remaining full bytes and align to next byte boundary.
func (b *bitWriter) flushAlign() {
	nbBytes := (b.nBits + 7) >> 3
	for i := range nbBytes {
		b.out = append(b.out, byte(b.bitContainer>>(i*8)))
	}
	b.nBits = 0
	b.bitContainer = 0
}

// close will write the alignment bit and write the final byte(s)
// to the output.
func (b *bitWriter) close() {
	// End mark
	b.addBits16Clean(1, 1)
	// flush until next byte.
	b.flushAlign()
}

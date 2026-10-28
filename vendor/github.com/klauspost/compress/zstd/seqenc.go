// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import "math/bits"

type seqCoders struct {
	llEnc, ofEnc, mlEnc    *fseEncoder
	llPrev, ofPrev, mlPrev *fseEncoder
}

// swap coders with another (block).
func (s *seqCoders) swap(other *seqCoders) {
	*s, *other = *other, *s
}

// setPrev will update the previous encoders to the actually used ones
// and make sure a fresh one is in the main slot.
func (s *seqCoders) setPrev(ll, ml, of *fseEncoder) {
	compareSwap := func(used *fseEncoder, current, prev **fseEncoder) {
		// We used the new one, more current to history and reuse the previous history
		if *current == used {
			*prev, *current = *current, *prev
			c := *current
			p := *prev
			c.reUsed = false
			p.reUsed = true
			return
		}
		if used == *prev {
			return
		}
		// Ensure we cannot reuse by accident
		prevEnc := *prev
		prevEnc.symbolLen = 0
	}
	compareSwap(ll, &s.llEnc, &s.llPrev)
	compareSwap(ml, &s.mlEnc, &s.mlPrev)
	compareSwap(of, &s.ofEnc, &s.ofPrev)
}

func highBit(val uint32) (n uint32) {
	return uint32(bits.Len32(val) - 1)
}

var llCodeTable = [64]byte{0, 1, 2, 3, 4, 5, 6, 7,
	8, 9, 10, 11, 12, 13, 14, 15,
	16, 16, 17, 17, 18, 18, 19, 19,
	20, 20, 20, 20, 21, 21, 21, 21,
	22, 22, 22, 22, 22, 22, 22, 22,
	23, 23, 23, 23, 23, 23, 23, 23,
	24, 24, 24, 24, 24, 24, 24, 24,
	24, 24, 24, 24, 24, 24, 24, 24}

// Up to 6 bits
const maxLLCode = 35

// llBitsTable translates from ll code to number of bits.
var llBitsTable = [maxLLCode + 1]byte{
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 2, 2, 3, 3,
	4, 6, 7, 8, 9, 10, 11, 12,
	13, 14, 15, 16}

// llCode returns the code that represents the literal length requested.
func llCode(litLength uint32) uint8 {
	const llDeltaCode = 19
	if litLength <= 63 {
		return llCodeTable[litLength&63]
	}
	return uint8(highBit(litLength)) + llDeltaCode
}

var mlCodeTable = [128]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	32, 32, 33, 33, 34, 34, 35, 35, 36, 36, 36, 36, 37, 37, 37, 37,
	38, 38, 38, 38, 38, 38, 38, 38, 39, 39, 39, 39, 39, 39, 39, 39,
	40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40,
	41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41, 41,
	42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42,
	42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42}

// Up to 6 bits
const maxMLCode = 52

// mlBitsTable translates from ml code to number of bits.
var mlBitsTable = [maxMLCode + 1]byte{
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 2, 2, 3, 3,
	4, 4, 5, 7, 8, 9, 10, 11,
	12, 13, 14, 15, 16}

// note : mlBase = matchLength - MINMATCH;
// because it's the format it's stored in seqStore->sequences
func mlCode(mlBase uint32) uint8 {
	const mlDeltaCode = 36
	if mlBase <= 127 {
		return mlCodeTable[mlBase&127]
	}
	return uint8(highBit(mlBase)) + mlDeltaCode
}

func ofCode(offset uint32) uint8 {
	// A valid offset will always be > 0.
	return uint8(bits.Len32(offset) - 1)
}

// newDictSeqEncoder builds an encoder for a dictionary sequence table.
// Returns nil if the table is missing or unusable.
func newDictSeqEncoder(src *fseDecoder, t tableIndex) *fseEncoder {
	if src == nil || src.symbolLen == 0 {
		return nil
	}
	var e fseEncoder
	e.norm = src.norm
	e.symbolLen = src.symbolLen
	e.actualTableLog = src.actualTableLog
	if e.buildCTable() != nil {
		return nil
	}
	e.setBits(bitTables[t])
	return &e
}

// seedPrevFromDict loads the dictionary sequence tables as the "previous"
// encoders, so the first block can use repeat mode.
func (s *seqCoders) seedPrevFromDict(d *dict) {
	seed := func(dst, src *fseEncoder) {
		if src == nil {
			return
		}
		ct := dst.ct
		dst.symbolLen = src.symbolLen
		dst.actualTableLog = src.actualTableLog
		dst.maxBits = src.maxBits
		dst.zeroBits = src.zeroBits
		dst.useRLE = false
		dst.preDefined = false
		dst.reUsed = true
		dst.norm = src.norm
		// tableSymbol is only used while building, so it is not copied.
		ct.stateTable = append(ct.stateTable[:0], src.ct.stateTable...)
		if cap(ct.symbolTT) < len(src.ct.symbolTT) {
			ct.symbolTT = make([]symbolTransform, len(src.ct.symbolTT))
		}
		ct.symbolTT = ct.symbolTT[:len(src.ct.symbolTT)]
		copy(ct.symbolTT, src.ct.symbolTT[:src.symbolLen])
		dst.ct = ct
	}
	seed(s.llPrev, d.llEnc)
	seed(s.mlPrev, d.mlEnc)
	seed(s.ofPrev, d.ofEnc)
}

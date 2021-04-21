// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import "fmt"

const (
	betterLongTableBits = 19                       // Bits used in the long match table
	betterLongTableSize = 1 << betterLongTableBits // Size of the table

	// Note: Increasing the short table bits or making the hash shorter
	// can actually lead to compression degradation since it will 'steal' more from the
	// long match table and match offsets are quite big.
	// This greatly depends on the type of input.
	betterShortTableBits = 13                        // Bits used in the short match table
	betterShortTableSize = 1 << betterShortTableBits // Size of the table
)

type prevEntry struct {
	offset int32
	prev   int32
}

// betterFastEncoder uses 2 tables, one for short matches (5 bytes) and one for long matches.
// The long match table contains the previous entry with the same hash,
// effectively making it a "chain" of length 2.
// When we find a long match we choose between the two values and select the longest.
// When we find a short match, after checking the long, we check if we can find a long at n+1
// and that it is longer (lazy matching).
type betterFastEncoder struct {
	fastBase
	table         [betterShortTableSize]tableEntry
	longTable     [betterLongTableSize]prevEntry
	dictTable     []tableEntry
	dictLongTable []prevEntry
}

// Encode improves compression...
func (e *betterFastEncoder) Encode(blk *blockEnc, src []byte) {
	const (
		// Input margin is the number of bytes we read (8)
		// and the maximum we will read ahead (2)
		inputMargin            = 8 + 2
		minNonLiteralBlockSize = 16
	)

	// Protect against e.cur wraparound.
	for e.cur >= bufferReset {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntry{}
			}
			for i := range e.longTable[:] {
				e.longTable[i] = prevEntry{}
			}
			e.cur = e.maxMatchOff
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - e.maxMatchOff
		for i := range e.table[:] {
			v := e.table[i].offset
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + e.maxMatchOff
			}
			e.table[i].offset = v
		}
		for i := range e.longTable[:] {
			v := e.longTable[i].offset
			v2 := e.longTable[i].prev
			if v < minOff {
				v = 0
				v2 = 0
			} else {
				v = v - e.cur + e.maxMatchOff
				if v2 < minOff {
					v2 = 0
				} else {
					v2 = v2 - e.cur + e.maxMatchOff
				}
			}
			e.longTable[i] = prevEntry{
				offset: v,
				prev:   v2,
			}
		}
		e.cur = e.maxMatchOff
		break
	}

	s := e.addBlock(src)
	blk.size = len(src)
	if len(src) < minNonLiteralBlockSize {
		blk.extraLits = len(src)
		blk.literals = blk.literals[:len(src)]
		copy(blk.literals, src)
		return
	}

	// Override src
	src = e.hist
	sLimit := int32(len(src)) - inputMargin
	// stepSize is the number of bytes to skip on every main loop iteration.
	// It should be >= 1.
	const stepSize = 1

	const kSearchStrength = 9

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := s
	cv := load6432(src, s)

	// Relative offsets
	offset1 := int32(blk.recentOffsets[0])
	offset2 := int32(blk.recentOffsets[1])

	addLiterals := func(s *seq, until int32) {
		if until == nextEmit {
			return
		}
		blk.literals = append(blk.literals, src[nextEmit:until]...)
		s.litLen = uint32(until - nextEmit)
	}
	if debug {
		println("recent offsets:", blk.recentOffsets)
	}

encodeLoop:
	for {
		var t int32
		// We allow the encoder to optionally turn off repeat offsets across blocks
		canRepeat := len(blk.sequences) > 2
		var matched int32

		for {
			if debugAsserts && canRepeat && offset1 == 0 {
				panic("offset0 was 0")
			}

			nextHashS := hash5(cv, betterShortTableBits)
			nextHashL := hash8(cv, betterLongTableBits)
			candidateL := e.longTable[nextHashL]
			candidateS := e.table[nextHashS]

			const repOff = 1
			repIndex := s - offset1 + repOff
			off := s + e.cur
			e.longTable[nextHashL] = prevEntry{offset: off, prev: candidateL.offset}
			e.table[nextHashS] = tableEntry{offset: off, val: uint32(cv)}

			if canRepeat {
				if repIndex >= 0 && load3232(src, repIndex) == uint32(cv>>(repOff*8)) {
					// Consider history as well.
					var seq seq
					lenght := 4 + e.matchlen(s+4+repOff, repIndex+4, src)

					seq.matchLen = uint32(lenght - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 0
					seq.offset = 1
					if debugSequences {
						println("repeat sequence", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					// Index match start+1 (long) -> s - 1
					index0 := s + repOff
					s += lenght + repOff

					nextEmit = s
					if s >= sLimit {
						if debug {
							println("repeat ended", s, lenght)

						}
						break encodeLoop
					}
					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hash8(cv0, betterLongTableBits)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.table[hash5(cv1, betterShortTableBits)] = tableEntry{offset: off + 1, val: uint32(cv1)}
						index0 += 2
					}
					cv = load6432(src, s)
					continue
				}
				const repOff2 = 1

				// We deviate from the reference encoder and also check offset 2.
				// Still slower and not much better, so disabled.
				// repIndex = s - offset2 + repOff2
				if false && repIndex >= 0 && load6432(src, repIndex) == load6432(src, s+repOff) {
					// Consider history as well.
					var seq seq
					lenght := 8 + e.matchlen(s+8+repOff2, repIndex+8, src)

					seq.matchLen = uint32(lenght - zstdMinMatch)

					// We might be able to match backwards.
					// Extend as long as we can.
					start := s + repOff2
					// We end the search early, so we don't risk 0 literals
					// and have to do special offset treatment.
					startLimit := nextEmit + 1

					tMin := s - e.maxMatchOff
					if tMin < 0 {
						tMin = 0
					}
					for repIndex > tMin && start > startLimit && src[repIndex-1] == src[start-1] && seq.matchLen < maxMatchLength-zstdMinMatch-1 {
						repIndex--
						start--
						seq.matchLen++
					}
					addLiterals(&seq, start)

					// rep 2
					seq.offset = 2
					if debugSequences {
						println("repeat sequence 2", seq, "next s:", s)
					}
					blk.sequences = append(blk.sequences, seq)

					index0 := s + repOff2
					s += lenght + repOff2
					nextEmit = s
					if s >= sLimit {
						if debug {
							println("repeat ended", s, lenght)

						}
						break encodeLoop
					}

					// Index skipped...
					for index0 < s-1 {
						cv0 := load6432(src, index0)
						cv1 := cv0 >> 8
						h0 := hash8(cv0, betterLongTableBits)
						off := index0 + e.cur
						e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
						e.table[hash5(cv1, betterShortTableBits)] = tableEntry{offset: off + 1, val: uint32(cv1)}
						index0 += 2
					}
					cv = load6432(src, s)
					// Swap offsets
					offset1, offset2 = offset2, offset1
					continue
				}
			}
			// Find the offsets of our two matches.
			coffsetL := candidateL.offset - e.cur
			coffsetLP := candidateL.prev - e.cur

			// Check if we have a long match.
			if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetL+8, src) + 8
				t = coffsetL
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}

				if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
					// Found a long match, at least 8 bytes.
					prevMatch := e.matchlen(s+8, coffsetLP+8, src) + 8
					if prevMatch > matched {
						matched = prevMatch
						t = coffsetLP
					}
					if debugAsserts && s <= t {
						panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
					}
					if debugAsserts && s-t > e.maxMatchOff {
						panic("s - t >e.maxMatchOff")
					}
					if debugMatches {
						println("long match")
					}
				}
				break
			}

			// Check if we have a long match on prev.
			if s-coffsetLP < e.maxMatchOff && cv == load6432(src, coffsetLP) {
				// Found a long match, at least 8 bytes.
				matched = e.matchlen(s+8, coffsetLP+8, src) + 8
				t = coffsetLP
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugMatches {
					println("long match")
				}
				break
			}

			coffsetS := candidateS.offset - e.cur

			// Check if we have a short match.
			if s-coffsetS < e.maxMatchOff && uint32(cv) == candidateS.val {
				// found a regular match
				matched = e.matchlen(s+4, coffsetS+4, src) + 4

				// See if we can find a long match at s+1
				const checkAt = 1
				cv := load6432(src, s+checkAt)
				nextHashL = hash8(cv, betterLongTableBits)
				candidateL = e.longTable[nextHashL]
				coffsetL = candidateL.offset - e.cur

				// We can store it, since we have at least a 4 byte match.
				e.longTable[nextHashL] = prevEntry{offset: s + checkAt + e.cur, prev: candidateL.offset}
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("long match (after short)")
						}
						break
					}
				}

				// Check prev long...
				coffsetL = candidateL.prev - e.cur
				if s-coffsetL < e.maxMatchOff && cv == load6432(src, coffsetL) {
					// Found a long match, at least 8 bytes.
					matchedNext := e.matchlen(s+8+checkAt, coffsetL+8, src) + 8
					if matchedNext > matched {
						t = coffsetL
						s += checkAt
						matched = matchedNext
						if debugMatches {
							println("prev long match (after short)")
						}
						break
					}
				}
				t = coffsetS
				if debugAsserts && s <= t {
					panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
				}
				if debugAsserts && s-t > e.maxMatchOff {
					panic("s - t >e.maxMatchOff")
				}
				if debugAsserts && t < 0 {
					panic("t<0")
				}
				if debugMatches {
					println("short match")
				}
				break
			}

			// No match found, move forward in input.
			s += stepSize + ((s - nextEmit) >> (kSearchStrength - 1))
			if s >= sLimit {
				break encodeLoop
			}
			cv = load6432(src, s)
		}

		// A 4-byte match has been found. Update recent offsets.
		// We'll later see if more than 4 bytes.
		offset2 = offset1
		offset1 = s - t

		if debugAsserts && s <= t {
			panic(fmt.Sprintf("s (%d) <= t (%d)", s, t))
		}

		if debugAsserts && canRepeat && int(offset1) > len(src) {
			panic("invalid offset")
		}

		// Extend the n-byte match as long as possible.
		l := matched

		// Extend backwards
		tMin := s - e.maxMatchOff
		if tMin < 0 {
			tMin = 0
		}
		for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
			s--
			t--
			l++
		}

		// Write our sequence
		var seq seq
		seq.litLen = uint32(s - nextEmit)
		seq.matchLen = uint32(l - zstdMinMatch)
		if seq.litLen > 0 {
			blk.literals = append(blk.literals, src[nextEmit:s]...)
		}
		seq.offset = uint32(s-t) + 3
		s += l
		if debugSequences {
			println("sequence", seq, "next s:", s)
		}
		blk.sequences = append(blk.sequences, seq)
		nextEmit = s
		if s >= sLimit {
			break encodeLoop
		}

		// Index match start+1 (long) -> s - 1
		index0 := s - l + 1
		for index0 < s-1 {
			cv0 := load6432(src, index0)
			cv1 := cv0 >> 8
			h0 := hash8(cv0, betterLongTableBits)
			off := index0 + e.cur
			e.longTable[h0] = prevEntry{offset: off, prev: e.longTable[h0].offset}
			e.table[hash5(cv1, betterShortTableBits)] = tableEntry{offset: off + 1, val: uint32(cv1)}
			index0 += 2
		}

		cv = load6432(src, s)
		if !canRepeat {
			continue
		}

		// Check offset 2
		for {
			o2 := s - offset2
			if load3232(src, o2) != uint32(cv) {
				// Do regular search
				break
			}

			// Store this, since we have it.
			nextHashS := hash5(cv, betterShortTableBits)
			nextHashL := hash8(cv, betterLongTableBits)

			// We have at least 4 byte match.
			// No need to check backwards. We come straight from a match
			l := 4 + e.matchlen(s+4, o2+4, src)

			e.longTable[nextHashL] = prevEntry{offset: s + e.cur, prev: e.longTable[nextHashL].offset}
			e.table[nextHashS] = tableEntry{offset: s + e.cur, val: uint32(cv)}
			seq.matchLen = uint32(l) - zstdMinMatch
			seq.litLen = 0

			// Since litlen is always 0, this is offset 1.
			seq.offset = 1
			s += l
			nextEmit = s
			if debugSequences {
				println("sequence", seq, "next s:", s)
			}
			blk.sequences = append(blk.sequences, seq)

			// Swap offset 1 and 2.
			offset1, offset2 = offset2, offset1
			if s >= sLimit {
				// Finished
				break encodeLoop
			}
			cv = load6432(src, s)
		}
	}

	if int(nextEmit) < len(src) {
		blk.literals = append(blk.literals, src[nextEmit:]...)
		blk.extraLits = len(src) - int(nextEmit)
	}
	blk.recentOffsets[0] = uint32(offset1)
	blk.recentOffsets[1] = uint32(offset2)
	if debug {
		println("returning, recent offsets:", blk.recentOffsets, "extra literals:", blk.extraLits)
	}
}

// EncodeNoHist will encode a block with no history and no following blocks.
// Most notable difference is that src will not be copied for history and
// we do not need to check for max match length.
func (e *betterFastEncoder) EncodeNoHist(blk *blockEnc, src []byte) {
	e.Encode(blk, src)
}

// ResetDict will reset and set a dictionary if not nil
func (e *betterFastEncoder) Reset(d *dict, singleBlock bool) {
	e.resetBase(d, singleBlock)
	if d == nil {
		return
	}
	// Init or copy dict table
	if len(e.dictTable) != len(e.table) || d.id != e.lastDictID {
		if len(e.dictTable) != len(e.table) {
			e.dictTable = make([]tableEntry, len(e.table))
		}
		end := int32(len(d.content)) - 8 + e.maxMatchOff
		for i := e.maxMatchOff; i < end; i += 4 {
			const hashLog = betterShortTableBits

			cv := load6432(d.content, i-e.maxMatchOff)
			nextHash := hash5(cv, hashLog)      // 0 -> 4
			nextHash1 := hash5(cv>>8, hashLog)  // 1 -> 5
			nextHash2 := hash5(cv>>16, hashLog) // 2 -> 6
			nextHash3 := hash5(cv>>24, hashLog) // 3 -> 7
			e.dictTable[nextHash] = tableEntry{
				val:    uint32(cv),
				offset: i,
			}
			e.dictTable[nextHash1] = tableEntry{
				val:    uint32(cv >> 8),
				offset: i + 1,
			}
			e.dictTable[nextHash2] = tableEntry{
				val:    uint32(cv >> 16),
				offset: i + 2,
			}
			e.dictTable[nextHash3] = tableEntry{
				val:    uint32(cv >> 24),
				offset: i + 3,
			}
		}
		e.lastDictID = d.id
	}

	// Init or copy dict table
	if len(e.dictLongTable) != len(e.longTable) || d.id != e.lastDictID {
		if len(e.dictLongTable) != len(e.longTable) {
			e.dictLongTable = make([]prevEntry, len(e.longTable))
		}
		if len(d.content) >= 8 {
			cv := load6432(d.content, 0)
			h := hash8(cv, betterLongTableBits)
			e.dictLongTable[h] = prevEntry{
				offset: e.maxMatchOff,
				prev:   e.dictLongTable[h].offset,
			}

			end := int32(len(d.content)) - 8 + e.maxMatchOff
			off := 8 // First to read
			for i := e.maxMatchOff + 1; i < end; i++ {
				cv = cv>>8 | (uint64(d.content[off]) << 56)
				h := hash8(cv, betterLongTableBits)
				e.dictLongTable[h] = prevEntry{
					offset: i,
					prev:   e.dictLongTable[h].offset,
				}
				off++
			}
		}
		e.lastDictID = d.id
	}
	// Reset table to initial state
	copy(e.longTable[:], e.dictLongTable)

	e.cur = e.maxMatchOff
	// Reset table to initial state
	copy(e.table[:], e.dictTable)
}

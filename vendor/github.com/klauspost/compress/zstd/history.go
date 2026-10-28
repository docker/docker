// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"github.com/klauspost/compress/huff0"
)

// history contains the information transferred between blocks.
type history struct {
	// Literal decompression
	huffTree *huff0.Scratch

	// Sequence decompression
	decoders      sequenceDecs
	recentOffsets [3]int

	// History buffer...
	b []byte

	// ignoreBuffer is meant to ignore a number of bytes
	// when checking for matches in history
	ignoreBuffer int

	windowSize int
	error      bool
	dict       *dict

	// ring backs b when streaming (see ensureBlockRing).
	// ext holds the part of the previous lap of ring still in the window,
	// which precedes b[0] in the stream; nil when b holds all history.
	// The previous lap is ring[extStart:extEnd].
	ring     []byte
	ext      []byte
	extStart int
	extEnd   int
}

// reset will reset the history to initial state of a frame.
// The history must already have been initialized to the desired size.
func (h *history) reset() {
	h.b = h.b[:0]
	h.ext = nil
	h.extStart, h.extEnd = 0, 0
	h.ignoreBuffer = 0
	h.error = false
	h.recentOffsets = [3]int{1, 4, 8}
	h.decoders.freeDecoders()
	h.decoders = sequenceDecs{br: h.decoders.br}
	h.freeHuffDecoder()
	h.huffTree = nil
	h.dict = nil
	//printf("history created: %+v (l: %d, c: %d)", *h, len(h.b), cap(h.b))
}

func (h *history) freeHuffDecoder() {
	if h.huffTree != nil {
		if h.dict == nil || h.dict.litEnc != h.huffTree {
			huffDecoderPool.Put(h.huffTree)
			h.huffTree = nil
		}
	}
}

func (h *history) setDict(dict *dict) {
	if dict == nil {
		return
	}
	h.dict = dict
	h.decoders.litLengths = dict.llDec
	h.decoders.offsets = dict.ofDec
	h.decoders.matchLengths = dict.mlDec
	h.decoders.dict = dict.content
	h.recentOffsets = dict.offsets
	h.huffTree = dict.litEnc
}

// ensureBlockRing makes room for the next block without moving history,
// as the reference decoder does. b is a prefix of ring capped to one block
// of space. When a block no longer fits, the data written so far becomes
// ext and writing restarts at the front of ring. ext is trimmed so the
// block space ahead of b never overlaps it; with ring sized to window plus
// two blocks of space, b and ext together always hold a full window.
func (h *history) ensureBlockRing() {
	// Room for the largest block plus the overrun of the fast copies.
	ringBlockSpace := min(h.windowSize, maxCompressedBlockSize) + compressedBlockOverAlloc
	size := h.windowSize + 2*ringBlockSpace
	pos := len(h.b)
	if cap(h.ring) < size || (pos > 0 && &h.b[0] != &h.ring[0]) {
		// No ring yet, or b no longer lives in it: start one, keeping up
		// to a window of what was decoded so far as the previous lap.
		keep := min(pos, h.windowSize)
		if cap(h.ring) < size {
			h.ring = make([]byte, size)
		}
		h.ring = h.ring[:size]
		copy(h.ring[size-keep:], h.b[pos-keep:])
		h.extStart, h.extEnd = size-keep, size
		pos = 0
	} else if pos+ringBlockSpace > size {
		// Wrap: everything written this lap becomes the previous lap.
		h.extStart, h.extEnd = 0, pos
		pos = 0
	}
	h.ring = h.ring[:size]
	end := pos + ringBlockSpace
	h.b = h.ring[:pos:end]
	h.ext = nil
	if start := max(end, h.extStart); start < h.extEnd {
		h.ext = h.ring[start:h.extEnd]
	}
}

// append bytes to history without ever discarding anything.
func (h *history) appendKeep(b []byte) {
	h.b = append(h.b, b...)
}

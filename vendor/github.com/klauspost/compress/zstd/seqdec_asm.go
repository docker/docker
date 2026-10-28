//go:build (amd64 || arm64) && !noasm && gc

package zstd

import (
	"fmt"
	"io"
)

// This file holds the parts of the assembly sequence decoder that are identical
// across architectures: the context structs exchanged with the asm, the error
// codes, and the decode/decodeSync/executeSimple wrappers. Each architecture
// supplies the small dispatch helpers (decodeAsm, decodeSyncAsm,
// executeSimpleAsm) that select the concrete asm routine — amd64 also chooses a
// BMI2 variant, arm64 has a single implementation.

type decodeSyncAsmContext struct {
	llTable     []decSymbol
	mlTable     []decSymbol
	ofTable     []decSymbol
	llState     uint64
	mlState     uint64
	ofState     uint64
	iteration   int
	litRemain   int
	out         []byte
	outPosition int
	literals    []byte
	litPosition int
	history     []byte
	windowSize  int
	ll          int // set on error (not for all errors, please refer to _generate/gen.go)
	ml          int // set on error (not for all errors, please refer to _generate/gen.go)
	mo          int // set on error (not for all errors, please refer to _generate/gen.go)
}

type decodeAsmContext struct {
	llTable   []decSymbol
	mlTable   []decSymbol
	ofTable   []decSymbol
	llState   uint64
	mlState   uint64
	ofState   uint64
	iteration int
	seqs      []seqVals
	litRemain int
}

type executeAsmContext struct {
	seqs        []seqVals
	seqIndex    int
	out         []byte
	history     []byte
	literals    []byte
	outPosition int
	litPosition int
	windowSize  int
	// prefetch enables the match-source prefetch in the execute loop; see
	// sequenceDecs.useTwoPass.
	prefetch bool
}

const noError = 0

// error reported when mo == 0 && ml > 0
const errorMatchLenOfsMismatch = 1

// error reported when ml > maxMatchLen
const errorMatchLenTooBig = 2

// error reported when mo > available history or mo > s.windowSize
const errorMatchOffTooBig = 3

// error reported when the sum of literal lengths exeeceds the literal buffer size
const errorNotEnoughLiterals = 4

// error reported when capacity of `out` is too small
const errorNotEnoughSpace = 5

// error reported when bits are overread.
const errorOverread = 6

// useSafeDecodeSync reports whether decodeSyncSimple must use the bounds-exact
// ("safe") copy variants for the current buffer geometry.
//
// When the output and literal buffers have compressedBlockOverAlloc (16)
// bytes of slack past their logical use, the assembly may use extended
// memory copies that read and write in 16-byte blocks, overrunning the end
// of a literal run or match by up to 15 bytes; an empty literal run still
// copies one block, reading and writing the full 16 bytes of slack past
// its position. Otherwise it must use the bounds-exact ("safe") copies.
// This mirrors the analogous, always-dynamic selection in executeSimple below.
//
// The unsafe copies were disabled in #644 (2022) as a mitigation for a
// crash, but that crash's root cause — an unguarded bitReader overread that
// produced out-of-range match offsets/lengths — was fixed three days later
// in #645, which also added the fuzz corpus that has guarded this path since.
// See #1168. An asan-instrumented fuzz job (see .github/workflows/go.yml)
// covers the extended-copy path, which -race and plain fuzzing cannot.
func (s *sequenceDecs) useSafeDecodeSync() bool {
	// A block never produces more than maxBlockSize bytes.
	maxBlockSize := min(s.windowSize, maxCompressedBlockSize)
	if s.maxSyncLen == 0 && cap(s.out)-len(s.out) < maxBlockSize+compressedBlockOverAlloc {
		return true
	}
	// The block, not the rest of the frame, must fit with the slack: a
	// buffer sized exactly to the frame needs the safe copies only for the
	// final block(s).
	if s.maxSyncLen > 0 && cap(s.out)-len(s.out)-compressedBlockOverAlloc < min(int(s.maxSyncLen), maxBlockSize) {
		return true
	}
	if cap(s.literals) < len(s.literals)+compressedBlockOverAlloc {
		return true
	}
	return false
}

// decode sequences from the stream with the provided history but without a dictionary.
func (s *sequenceDecs) decodeSyncSimple(hist []byte) (bool, error) {
	if len(s.dict) > 0 {
		return false, nil
	}
	maxBlockSize := min(s.windowSize, maxCompressedBlockSize)
	if s.maxSyncLen == 0 && cap(s.out)-len(s.out) < maxBlockSize {
		return false, nil
	}

	useSafe := s.useSafeDecodeSync()

	br := s.br

	ctx := decodeSyncAsmContext{
		llTable:     s.litLengths.fse.dt[:maxTablesize],
		mlTable:     s.matchLengths.fse.dt[:maxTablesize],
		ofTable:     s.offsets.fse.dt[:maxTablesize],
		llState:     uint64(s.litLengths.state.state),
		mlState:     uint64(s.matchLengths.state.state),
		ofState:     uint64(s.offsets.state.state),
		iteration:   s.nSeqs - 1,
		litRemain:   len(s.literals),
		out:         s.out,
		outPosition: len(s.out),
		literals:    s.literals,
		windowSize:  s.windowSize,
		history:     hist,
	}

	s.seqSize = 0
	startSize := len(s.out)

	errCode := decodeSyncAsm(s, br, &ctx, useSafe)
	switch errCode {
	case noError:
		break

	case errorMatchLenOfsMismatch:
		return true, fmt.Errorf("zero matchoff and matchlen (%d) > 0", ctx.ml)

	case errorMatchLenTooBig:
		return true, fmt.Errorf("match len (%d) bigger than max allowed length", ctx.ml)

	case errorMatchOffTooBig:
		return true, fmt.Errorf("match offset (%d) bigger than current history (%d)",
			ctx.mo, ctx.outPosition-startSize)

	case errorNotEnoughLiterals:
		return true, fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available",
			ctx.ll, ctx.litRemain+ctx.ll)

	case errorOverread:
		return true, io.ErrUnexpectedEOF

	case errorNotEnoughSpace:
		size := ctx.outPosition + ctx.ll + ctx.ml
		if debugDecoder {
			println("msl:", s.maxSyncLen, "cap", cap(s.out), "bef:", startSize, "sz:", size-startSize, "mbs:", maxBlockSize, "outsz:", cap(s.out)-startSize)
		}
		return true, fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)

	default:
		return true, fmt.Errorf("sequenceDecs_decode returned erroneous code %d", errCode)
	}

	s.seqSize += ctx.litRemain
	if s.seqSize > maxBlockSize {
		return true, fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	err := br.close()
	if err != nil {
		printf("Closing sequences: %v, %+v\n", err, *br)
		return true, err
	}

	s.literals = s.literals[ctx.litPosition:]
	t := ctx.outPosition
	s.out = s.out[:t]

	// Add final literals
	s.out = append(s.out, s.literals...)
	if debugDecoder {
		t += len(s.literals)
		if t != len(s.out) {
			panic(fmt.Errorf("length mismatch, want %d, got %d", len(s.out), t))
		}
	}

	return true, nil
}

// decode sequences from the stream without the provided history.
func (s *sequenceDecs) decode(seqs []seqVals) error {
	br := s.br

	maxBlockSize := min(s.windowSize, maxCompressedBlockSize)

	ctx := decodeAsmContext{
		llTable:   s.litLengths.fse.dt[:maxTablesize],
		mlTable:   s.matchLengths.fse.dt[:maxTablesize],
		ofTable:   s.offsets.fse.dt[:maxTablesize],
		llState:   uint64(s.litLengths.state.state),
		mlState:   uint64(s.matchLengths.state.state),
		ofState:   uint64(s.offsets.state.state),
		seqs:      seqs,
		iteration: len(seqs) - 1,
		litRemain: len(s.literals),
	}

	if debugDecoder {
		println("decode: decoding", len(seqs), "sequences", br.remain(), "bits remain on stream")
	}

	s.seqSize = 0
	lte56bits := s.maxBits+s.offsets.fse.actualTableLog+s.matchLengths.fse.actualTableLog+s.litLengths.fse.actualTableLog <= 56
	errCode := decodeAsm(s, br, &ctx, lte56bits)
	if errCode != 0 {
		i := len(seqs) - ctx.iteration - 1
		switch errCode {
		case errorMatchLenOfsMismatch:
			ml := ctx.seqs[i].ml
			return fmt.Errorf("zero matchoff and matchlen (%d) > 0", ml)

		case errorMatchLenTooBig:
			ml := ctx.seqs[i].ml
			return fmt.Errorf("match len (%d) bigger than max allowed length", ml)

		case errorNotEnoughLiterals:
			ll := ctx.seqs[i].ll
			return fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available", ll, ctx.litRemain+ll)
		case errorOverread:
			return io.ErrUnexpectedEOF
		}

		return fmt.Errorf("sequenceDecs_decode_amd64 returned erroneous code %d", errCode)
	}

	if ctx.litRemain < 0 {
		return fmt.Errorf("literal count is too big: total available %d, total requested %d",
			len(s.literals), len(s.literals)-ctx.litRemain)
	}

	s.seqSize += ctx.litRemain
	if s.seqSize > maxBlockSize {
		return fmt.Errorf("output bigger than max block size (%d)", maxBlockSize)
	}
	if debugDecoder {
		println("decode: ", br.remain(), "bits remain on stream. code:", errCode)
	}
	err := br.close()
	if err != nil {
		printf("Closing sequences: %v, %+v\n", err, *br)
	}
	return err
}

// executeSimple handles cases when dictionary is not used.
func (s *sequenceDecs) executeSimple(seqs []seqVals, hist []byte) error {
	// Grow only if the block does not fit. Lacking just the slack is
	// handled by the safe copies below, so a buffer sized exactly to the
	// frame is not reallocated and copied on its last block.
	if len(s.out)+s.seqSize > cap(s.out) {
		addBytes := s.seqSize + len(s.out) + compressedBlockOverAlloc
		s.out = append(s.out, make([]byte, addBytes)...)
		s.out = s.out[:len(s.out)-addBytes]
	}

	if debugDecoder {
		printf("Execute %d seqs with literals: %d into %d bytes\n", len(seqs), len(s.literals), s.seqSize)
	}

	var t = len(s.out)
	out := s.out[:t+s.seqSize]

	ctx := executeAsmContext{
		seqs:        seqs,
		seqIndex:    0,
		out:         out,
		history:     hist,
		outPosition: t,
		litPosition: 0,
		literals:    s.literals,
		windowSize:  s.windowSize,
		prefetch:    s.useTwoPass(),
	}
	// useSafe avoids writing past either buffer when the literals or the
	// output lack the extended copies' over-allocation slack.
	useSafe := cap(s.literals) < len(s.literals)+compressedBlockOverAlloc ||
		cap(out) < len(out)+compressedBlockOverAlloc

	ok := executeSimpleAsm(&ctx, useSafe)
	if !ok {
		return fmt.Errorf("match offset (%d) bigger than current history (%d)",
			seqs[ctx.seqIndex].mo, ctx.outPosition+len(hist))
	}
	s.literals = s.literals[ctx.litPosition:]
	t = ctx.outPosition

	// Add final literals
	copy(out[t:], s.literals)
	if debugDecoder {
		t += len(s.literals)
		if t != len(out) {
			panic(fmt.Errorf("length mismatch, want %d, got %d, ss: %d", len(out), t, s.seqSize))
		}
	}
	s.out = out

	return nil
}

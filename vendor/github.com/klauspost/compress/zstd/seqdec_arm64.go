//go:build arm64 && !noasm && gc

package zstd

// The shared decode/decodeSync/executeSimple wrappers and context structs live
// in seqdec_asm.go; this file only declares the arm64 asm routines (generated
// by the avo arm64 lowering printer) and the dispatch helpers. arm64 has no
// BMI2, so each helper selects only between the 56-bit / safe variants.

// The synchronous decoder takes the two-pass path, and executeSimple
// prefetches, when the window is at least decodeTwoPassMinWindow and the
// block's offset table puts at least twoPassMinFarShare/256 of its
// sequences 128 KiB or more back (sequenceDecs.useTwoPass). Thresholds
// from the per-file N1 measurement in #1212; variables so tests can force
// either path.
var (
	twoPassMinFarShare     = 46
	decodeTwoPassMinWindow = 1 << 20
)

// sequenceDecs_decode_arm64 implements the main loop of sequenceDecs in arm64 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decode_arm64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode_56_arm64 implements the main loop of sequenceDecs in arm64 asm.
//
//go:noescape
func sequenceDecs_decode_56_arm64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// decodeAsm runs the sequenceDecs decode loop, choosing the 56-bit variant.
func decodeAsm(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext, lte56bits bool) int {
	if lte56bits {
		return sequenceDecs_decode_56_arm64(s, br, ctx)
	}
	return sequenceDecs_decode_arm64(s, br, ctx)
}

// sequenceDecs_decodeSync_arm64 implements the main loop of sequenceDecs.decodeSync in arm64 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decodeSync_arm64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_arm64 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_arm64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// decodeSyncAsm runs the decodeSync loop, choosing the safe variant.
func decodeSyncAsm(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext, safe bool) int {
	if safe {
		return sequenceDecs_decodeSync_safe_arm64(s, br, ctx)
	}
	return sequenceDecs_decodeSync_arm64(s, br, ctx)
}

// sequenceDecs_executeSimple_arm64 implements the main loop of sequenceDecs.executeSimple in arm64 asm.
//
// Returns false if a match offset is too big.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_executeSimple_arm64(ctx *executeAsmContext) bool

// Same as above, but with safe memcopies
//
//go:noescape
func sequenceDecs_executeSimple_safe_arm64(ctx *executeAsmContext) bool

// executeSimpleAsm runs the executeSimple loop, choosing the safe variant.
func executeSimpleAsm(ctx *executeAsmContext, safe bool) bool {
	if safe {
		return sequenceDecs_executeSimple_safe_arm64(ctx)
	}
	return sequenceDecs_executeSimple_arm64(ctx)
}

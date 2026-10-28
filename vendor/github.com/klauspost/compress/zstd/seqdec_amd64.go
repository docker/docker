//go:build amd64 && !noasm && gc

package zstd

import (
	"math"

	"github.com/klauspost/compress/internal/cpuinfo"
)

// The shared decode/decodeSync/executeSimple wrappers and context structs live
// in seqdec_asm.go; this file only declares the amd64 asm routines and the
// dispatch helpers that pick the BMI2 / non-BMI2 (and 56-bit / safe) variant.

// See seqdec_arm64.go. Disabled here: on Sapphire Rapids, Granite Rapids
// and Zen 4 the two-pass decode costs 7-12% on real data; their caches
// already cover the offsets involved.
var (
	twoPassMinFarShare     = 257 // never: the share is at most 256
	decodeTwoPassMinWindow = math.MaxInt
)

// sequenceDecs_decode implements the main loop of sequenceDecs in x86 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decode_amd64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode_56_amd64 implements the main loop of sequenceDecs in x86 asm.
//
//go:noescape
func sequenceDecs_decode_56_amd64(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode_bmi2 implements the main loop of sequenceDecs in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decode_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// sequenceDecs_decode_56_bmi2 implements the main loop of sequenceDecs in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decode_56_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext) int

// decodeAsm runs the sequenceDecs decode loop, choosing the BMI2 / 56-bit variant.
func decodeAsm(s *sequenceDecs, br *bitReader, ctx *decodeAsmContext, lte56bits bool) int {
	if cpuinfo.HasBMI2() {
		if lte56bits {
			return sequenceDecs_decode_56_bmi2(s, br, ctx)
		}
		return sequenceDecs_decode_bmi2(s, br, ctx)
	}
	if lte56bits {
		return sequenceDecs_decode_56_amd64(s, br, ctx)
	}
	return sequenceDecs_decode_amd64(s, br, ctx)
}

// sequenceDecs_decodeSync_amd64 implements the main loop of sequenceDecs.decodeSync in x86 asm.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_decodeSync_amd64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_bmi2 implements the main loop of sequenceDecs.decodeSync in x86 asm with BMI2 extensions.
//
//go:noescape
func sequenceDecs_decodeSync_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_amd64 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_amd64(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// sequenceDecs_decodeSync_safe_bmi2 does the same as above, but does not write more than output buffer.
//
//go:noescape
func sequenceDecs_decodeSync_safe_bmi2(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext) int

// decodeSyncAsm runs the decodeSync loop, choosing the BMI2 / safe variant.
func decodeSyncAsm(s *sequenceDecs, br *bitReader, ctx *decodeSyncAsmContext, safe bool) int {
	if cpuinfo.HasBMI2() {
		if safe {
			return sequenceDecs_decodeSync_safe_bmi2(s, br, ctx)
		}
		return sequenceDecs_decodeSync_bmi2(s, br, ctx)
	}
	if safe {
		return sequenceDecs_decodeSync_safe_amd64(s, br, ctx)
	}
	return sequenceDecs_decodeSync_amd64(s, br, ctx)
}

// sequenceDecs_executeSimple_amd64 implements the main loop of sequenceDecs.executeSimple in x86 asm.
//
// Returns false if a match offset is too big.
//
// Please refer to seqdec_generic.go for the reference implementation.
//
//go:noescape
func sequenceDecs_executeSimple_amd64(ctx *executeAsmContext) bool

// Same as above, but with safe memcopies
//
//go:noescape
func sequenceDecs_executeSimple_safe_amd64(ctx *executeAsmContext) bool

// executeSimpleAsm runs the executeSimple loop, choosing the safe variant.
func executeSimpleAsm(ctx *executeAsmContext, safe bool) bool {
	if safe {
		return sequenceDecs_executeSimple_safe_amd64(ctx)
	}
	return sequenceDecs_executeSimple_amd64(ctx)
}

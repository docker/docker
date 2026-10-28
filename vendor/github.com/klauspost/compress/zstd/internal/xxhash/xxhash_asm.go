//go:build (amd64 || arm64) && gc && !purego && !noasm

package xxhash

// Sum64 computes the 64-bit xxHash digest of b.
//
//go:noescape
func Sum64(b []byte) uint64

//go:noescape
func writeBlocks(s *Digest, b []byte) int

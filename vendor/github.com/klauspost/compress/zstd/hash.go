// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import "runtime"

const (
	prime3bytes = 506832829
	prime4bytes = 2654435761
	prime5bytes = 889523592379
	prime6bytes = 227718039650203
	prime7bytes = 58295818150454627
	prime8bytes = 0xcf1bbcdcb7a56463
)

// hashPrimesInRegs reports whether encoders should keep the 64-bit hash primes
// in registers rather than as constants. The compiler rebuilds a constant at
// every use: one instruction on amd64, but up to 4 on arm64, which also has
// the registers to spare. See hashPrimes8and5.
const hashPrimesInRegs = runtime.GOARCH == "arm64"

// hashLen returns a hash of the lowest mls bytes of with length output bits.
// mls must be >=3 and <=8. Any other value will return hash for 4 bytes.
// length should always be < 32.
// Preferably length and mls should be a constant for inlining.
func hashLen(u uint64, length, mls uint8) uint32 {
	switch mls {
	case 3:
		return (uint32(u<<8) * prime3bytes) >> (32 - length)
	case 5:
		return uint32(((u << (64 - 40)) * prime5bytes) >> (64 - length))
	case 6:
		return uint32(((u << (64 - 48)) * prime6bytes) >> (64 - length))
	case 7:
		return uint32(((u << (64 - 56)) * prime7bytes) >> (64 - length))
	case 8:
		return uint32((u * prime8bytes) >> (64 - length))
	default:
		return (uint32(u) * prime4bytes) >> (32 - length)
	}
}

// hashPrimes85 is a var so arm64 keeps the primes in registers instead of
// rebuilding each 64-bit prime with up to 4 instructions per use.
var hashPrimes85 = [2]uint64{prime8bytes, prime5bytes}

// hashPrimes8and5 returns prime8bytes and prime5bytes for encoders that hash
// 8 and 5 bytes. Where registers are too scarce to hold them, it returns the
// constants.
func hashPrimes8and5() (prime8, prime5 uint64) {
	if hashPrimesInRegs {
		return hashPrimes85[0], hashPrimes85[1]
	}
	return prime8bytes, prime5bytes
}

// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

const (
	prime3bytes = 506832829
	prime4bytes = 2654435761
	prime5bytes = 889523592379
	prime6bytes = 227718039650203
	prime7bytes = 58295818150454627
	prime8bytes = 0xcf1bbcdcb7a56463
)

// hashLen returns a hash of the lowest l bytes of u for a size size of h bytes.
// l must be >=4 and <=8. Any other value will return hash for 4 bytes.
// h should always be <32.
// Preferably h and l should be a constant.
// FIXME: This does NOT get resolved, if 'mls' is constant,
//  so this cannot be used.
func hashLen(u uint64, hashLog, mls uint8) uint32 {
	switch mls {
	case 5:
		return hash5(u, hashLog)
	case 6:
		return hash6(u, hashLog)
	case 7:
		return hash7(u, hashLog)
	case 8:
		return hash8(u, hashLog)
	default:
		return hash4x64(u, hashLog)
	}
}

// hash3 returns the hash of the lower 3 bytes of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <32.
func hash3(u uint32, h uint8) uint32 {
	return ((u << (32 - 24)) * prime3bytes) >> ((32 - h) & 31)
}

// hash4 returns the hash of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <32.
func hash4(u uint32, h uint8) uint32 {
	return (u * prime4bytes) >> ((32 - h) & 31)
}

// hash4x64 returns the hash of the lowest 4 bytes of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <32.
func hash4x64(u uint64, h uint8) uint32 {
	return (uint32(u) * prime4bytes) >> ((32 - h) & 31)
}

// hash5 returns the hash of the lowest 5 bytes of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <64.
func hash5(u uint64, h uint8) uint32 {
	return uint32(((u << (64 - 40)) * prime5bytes) >> ((64 - h) & 63))
}

// hash6 returns the hash of the lowest 6 bytes of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <64.
func hash6(u uint64, h uint8) uint32 {
	return uint32(((u << (64 - 48)) * prime6bytes) >> ((64 - h) & 63))
}

// hash7 returns the hash of the lowest 7 bytes of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <64.
func hash7(u uint64, h uint8) uint32 {
	return uint32(((u << (64 - 56)) * prime7bytes) >> ((64 - h) & 63))
}

// hash8 returns the hash of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <64.
func hash8(u uint64, h uint8) uint32 {
	return uint32((u * prime8bytes) >> ((64 - h) & 63))
}

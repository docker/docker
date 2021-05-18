// Package pbytes contains tools for pooling byte pool.
// Note that by default it reuse slices with capacity from 128 to 65536 bytes.
package pbytes

// DefaultPool is used by pacakge level functions.
var DefaultPool = New(128, 65536)

// Get returns probably reused slice of bytes with at least capacity of c and
// exactly len of n.
// Get is a wrapper around DefaultPool.Get().
func Get(n, c int) []byte { return DefaultPool.Get(n, c) }

// GetCap returns probably reused slice of bytes with at least capacity of n.
// GetCap is a wrapper around DefaultPool.GetCap().
func GetCap(c int) []byte { return DefaultPool.GetCap(c) }

// GetLen returns probably reused slice of bytes with at least capacity of n
// and exactly len of n.
// GetLen is a wrapper around DefaultPool.GetLen().
func GetLen(n int) []byte { return DefaultPool.GetLen(n) }

// Put returns given slice to reuse pool.
// Put is a wrapper around DefaultPool.Put().
func Put(p []byte) { DefaultPool.Put(p) }

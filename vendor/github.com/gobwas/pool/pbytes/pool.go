// +build !pool_sanitize

package pbytes

import "github.com/gobwas/pool"

// Pool contains logic of reusing byte slices of various size.
type Pool struct {
	pool *pool.Pool
}

// New creates new Pool that reuses slices which size is in logarithmic range
// [min, max].
//
// Note that it is a shortcut for Custom() constructor with Options provided by
// pool.WithLogSizeMapping() and pool.WithLogSizeRange(min, max) calls.
func New(min, max int) *Pool {
	return &Pool{pool.New(min, max)}
}

// New creates new Pool with given options.
func Custom(opts ...pool.Option) *Pool {
	return &Pool{pool.Custom(opts...)}
}

// Get returns probably reused slice of bytes with at least capacity of c and
// exactly len of n.
func (p *Pool) Get(n, c int) []byte {
	if n > c {
		panic("requested length is greater than capacity")
	}

	v, x := p.pool.Get(c)
	if v != nil {
		bts := v.([]byte)
		bts = bts[:n]
		return bts
	}

	return make([]byte, n, x)
}

// Put returns given slice to reuse pool.
// It does not reuse bytes whose size is not power of two or is out of pool
// min/max range.
func (p *Pool) Put(bts []byte) {
	p.pool.Put(bts, cap(bts))
}

// GetCap returns probably reused slice of bytes with at least capacity of n.
func (p *Pool) GetCap(c int) []byte {
	return p.Get(0, c)
}

// GetLen returns probably reused slice of bytes with at least capacity of n
// and exactly len of n.
func (p *Pool) GetLen(n int) []byte {
	return p.Get(n, n)
}

package pool

import "github.com/gobwas/pool/internal/pmath"

// Option configures pool.
type Option func(Config)

// Config describes generic pool configuration.
type Config interface {
	AddSize(n int)
	SetSizeMapping(func(int) int)
}

// WithSizeLogRange returns an Option that will add logarithmic range of
// pooling sizes containing [min, max] values.
func WithLogSizeRange(min, max int) Option {
	return func(c Config) {
		pmath.LogarithmicRange(min, max, func(n int) {
			c.AddSize(n)
		})
	}
}

// WithSize returns an Option that will add given pooling size to the pool.
func WithSize(n int) Option {
	return func(c Config) {
		c.AddSize(n)
	}
}

func WithSizeMapping(sz func(int) int) Option {
	return func(c Config) {
		c.SetSizeMapping(sz)
	}
}

func WithLogSizeMapping() Option {
	return WithSizeMapping(pmath.CeilToPowerOfTwo)
}

func WithIdentitySizeMapping() Option {
	return WithSizeMapping(pmath.Identity)
}

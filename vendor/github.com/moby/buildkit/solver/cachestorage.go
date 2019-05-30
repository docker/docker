package solver

import (
	"context"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

var ErrNotFound = errors.Errorf("not found")

// CacheKeyStorage is interface for persisting cache metadata
type CacheKeyStorage interface {
	Exists(id string) bool
	Walk(fn func(id string) error) error

	WalkResults(id string, fn func(CacheResult) error) error
	Load(id string, resultID string) (CacheResult, error)
	AddResult(id string, res CacheResult) error
	Release(resultID string) error
	WalkIDsByResult(resultID string, fn func(string) error) error

	AddLink(id string, link CacheInfoLink, target string) error
	WalkLinks(id string, link CacheInfoLink, fn func(id string) error) error
	HasLink(id string, link CacheInfoLink, target string) bool
	WalkBacklinks(id string, fn func(id string, link CacheInfoLink) error) error
}

// CacheResult is a record for a single solve result
type CacheResult struct {
	CreatedAt time.Time
	ID        string
}

// CacheInfoLink is a link between two cache keys
type CacheInfoLink struct {
	Input    Index         `json:"Input,omitempty"`
	Output   Index         `json:"Output,omitempty"`
	Digest   digest.Digest `json:"Digest,omitempty"`
	Selector digest.Digest `json:"Selector,omitempty"`
}

// CacheResultStorage is interface for converting cache metadata result to
// actual solve result
type CacheResultStorage interface {
	Save(Result, time.Time) (CacheResult, error)
	Load(ctx context.Context, res CacheResult) (Result, error)
	LoadRemote(ctx context.Context, res CacheResult) (*Remote, error)
	Exists(id string) bool
}

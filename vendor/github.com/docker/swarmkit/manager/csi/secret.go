package csi

import (
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/manager/state/store"
)

// SecretProvider is an interface for retrieving secrets to use with CSI calls.
type SecretProvider interface {
	// GetSecret returns the secret with the given ID, or nil if not found.
	GetSecret(id string) *api.Secret
}

type secretProvider struct {
	s *store.MemoryStore
}

func NewSecretProvider(s *store.MemoryStore) SecretProvider {
	return &secretProvider{
		s: s,
	}
}

// GetSecret returns the secret with the given ID, or nil if not found.
//
// This method accesses the store, and so should not be called from inside
// another store transaction
func (p *secretProvider) GetSecret(id string) *api.Secret {
	var secret *api.Secret
	p.s.View(func(tx store.ReadTx) {
		secret = store.GetSecret(tx, id)
	})
	return secret
}

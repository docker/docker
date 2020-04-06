package daemon

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/testutil"
	"gotest.tools/assert"
)

// SecretConstructor defines a swarm secret constructor
type SecretConstructor func(*swarm.Secret)

// CreateSecret creates a secret given the specified spec
func (d *Daemon) CreateSecret(t assert.TestingT, secretSpec swarm.SecretSpec) string {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	scr, err := cli.SecretCreate(context.Background(), secretSpec)
	assert.NilError(t, err)

	return scr.ID
}

// ListSecrets returns the list of the current swarm secrets
func (d *Daemon) ListSecrets(t assert.TestingT) []swarm.Secret {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	secrets, err := cli.SecretList(context.Background(), types.SecretListOptions{})
	assert.NilError(t, err)
	return secrets
}

// GetSecret returns a swarm secret identified by the specified id
func (d *Daemon) GetSecret(t assert.TestingT, id string) *swarm.Secret {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	secret, _, err := cli.SecretInspectWithRaw(context.Background(), id)
	assert.NilError(t, err)
	return &secret
}

// DeleteSecret removes the swarm secret identified by the specified id
func (d *Daemon) DeleteSecret(t assert.TestingT, id string) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	err := cli.SecretRemove(context.Background(), id)
	assert.NilError(t, err)
}

// UpdateSecret updates the swarm secret identified by the specified id
// Currently, only label update is supported.
func (d *Daemon) UpdateSecret(t assert.TestingT, id string, f ...SecretConstructor) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	secret := d.GetSecret(t, id)
	for _, fn := range f {
		fn(secret)
	}

	err := cli.SecretUpdate(context.Background(), secret.ID, secret.Version, secret.Spec)

	assert.NilError(t, err)
}

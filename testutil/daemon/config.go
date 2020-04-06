package daemon

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/testutil"
	"gotest.tools/assert"
)

// ConfigConstructor defines a swarm config constructor
type ConfigConstructor func(*swarm.Config)

// CreateConfig creates a config given the specified spec
func (d *Daemon) CreateConfig(t assert.TestingT, configSpec swarm.ConfigSpec) string {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	scr, err := cli.ConfigCreate(context.Background(), configSpec)
	assert.NilError(t, err)
	return scr.ID
}

// ListConfigs returns the list of the current swarm configs
func (d *Daemon) ListConfigs(t assert.TestingT) []swarm.Config {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	configs, err := cli.ConfigList(context.Background(), types.ConfigListOptions{})
	assert.NilError(t, err)
	return configs
}

// GetConfig returns a swarm config identified by the specified id
func (d *Daemon) GetConfig(t assert.TestingT, id string) *swarm.Config {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	config, _, err := cli.ConfigInspectWithRaw(context.Background(), id)
	assert.NilError(t, err)
	return &config
}

// DeleteConfig removes the swarm config identified by the specified id
func (d *Daemon) DeleteConfig(t assert.TestingT, id string) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	err := cli.ConfigRemove(context.Background(), id)
	assert.NilError(t, err)
}

// UpdateConfig updates the swarm config identified by the specified id
// Currently, only label update is supported.
func (d *Daemon) UpdateConfig(t assert.TestingT, id string, f ...ConfigConstructor) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	config := d.GetConfig(t, id)
	for _, fn := range f {
		fn(config)
	}

	err := cli.ConfigUpdate(context.Background(), config.ID, config.Version, config.Spec)
	assert.NilError(t, err)
}

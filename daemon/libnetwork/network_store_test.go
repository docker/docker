package libnetwork

import (
	"slices"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestNetworkStore(t *testing.T) {
	c, err := New(t.Context())
	assert.NilError(t, err)

	// Insert a first network
	nw1 := &Network{id: "testNetwork1", configFrom: "config-network"}
	err = c.storeNetwork(t.Context(), nw1)
	assert.NilError(t, err)

	// Then a second network
	nw2 := &Network{id: "testNetwork2"}
	err = c.storeNetwork(t.Context(), nw2)
	assert.NilError(t, err)

	// Check that we can find both networks, and that the returned values are
	// not copies of the original ones.
	for _, tc := range []struct {
		name        string
		filter      func(nw *Network) bool
		expNetworks []*Network
	}{
		{
			name:        "no filter",
			filter:      func(nw *Network) bool { return true },
			expNetworks: []*Network{nw1, nw2},
		},
		{
			name:        "filter by configFrom",
			filter:      filterNetworkByConfigFrom("config-network"),
			expNetworks: []*Network{nw1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := c.findNetworks(tc.filter)
			assert.Equal(t, len(found), len(tc.expNetworks))
			slices.SortFunc(found, func(a, b *Network) int { return strings.Compare(a.id, b.id) })
			for i, nw := range tc.expNetworks {
				assert.Check(t, is.Equal(found[i], nw), "got: %s; expected: %s", found[i].id, nw.id)
			}
		})
	}

	// Delete the first network
	err = c.deleteStoredNetwork(nw1)
	assert.NilError(t, err)

	// Check that we can only find the second network
	found := c.findNetworks(func(nw *Network) bool { return true })
	assert.Equal(t, len(found), 1)
	assert.Check(t, is.Equal(found[0], nw2), "got: %s; expected: %s", found[0].id, nw2.id)

	// Store the second network again
	err = c.storeNetwork(t.Context(), nw2)
	assert.NilError(t, err)
}

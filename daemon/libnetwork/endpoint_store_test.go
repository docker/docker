package libnetwork

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestEndpointStore(t *testing.T) {
	c, err := New(t.Context())
	assert.NilError(t, err)

	// Insert a first endpoint
	nw := &Network{id: "testNetwork"}
	ep1 := &Endpoint{network: nw, id: "testEndpoint1"}
	err = c.storeEndpoint(t.Context(), ep1)
	assert.NilError(t, err)

	// Then a second endpoint
	ep2 := &Endpoint{network: nw, id: "testEndpoint2"}
	err = c.storeEndpoint(t.Context(), ep2)
	assert.NilError(t, err)

	// Check that we can find both endpoints, and that the returned values are
	// not copies of the original ones.
	found := c.findEndpoints(filterEndpointByNetworkId("testNetwork"))
	slices.SortFunc(found, func(a, b *Endpoint) int { return strings.Compare(a.id, b.id) })
	assert.Equal(t, len(found), 2)
	assert.Check(t, is.Equal(found[0], ep1), "got: %s; expected: %s", found[0].id, ep1.id)
	assert.Check(t, is.Equal(found[1], ep2), "got: %s; expected: %s", found[1].id, ep1.id)

	// Delete the first endpoint
	err = c.deleteStoredEndpoint(ep1)
	assert.NilError(t, err)

	// Check that we can only find the second endpoint
	found = c.findEndpoints(filterEndpointByNetworkId("testNetwork"))
	assert.Equal(t, len(found), 1)
	assert.Check(t, is.Equal(found[0], ep2), "got: %s; expected: %s", found[0].id, ep2.id)

	// Store the second endpoint again
	err = c.storeEndpoint(t.Context(), ep2)
	assert.NilError(t, err)
}

// An endpoint read from the store just before its deletion must not end up in
// the cache after it: Network.delete would see an active endpoint forever.
func TestEndpointStoreDeleteRacingRead(t *testing.T) {
	c, err := New(t.Context())
	assert.NilError(t, err)
	nw := &Network{id: "testNetwork", ctrlr: c}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, err := nw.getEndpointsFromStore()
					assert.Check(t, err)
				}
			}
		})
	}

	for i := range 2000 {
		ep := &Endpoint{network: nw, id: fmt.Sprintf("testEndpoint%d", i)}
		assert.NilError(t, c.storeEndpoint(t.Context(), ep))
		assert.NilError(t, c.deleteStoredEndpoint(ep))
	}
	close(stop)
	readers.Wait()

	assert.Check(t, is.Len(c.findEndpoints(filterEndpointByNetworkId("testNetwork")), 0))
}

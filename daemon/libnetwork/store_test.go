package libnetwork

import (
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/kvstore/boltdb"
)

func testLocalBackend(t *testing.T, path string) {
	kv, err := boltdb.New(path)
	if err != nil {
		t.Fatalf("Error opening datastore: %v", err)
	}
	testController, err := New(t.Context(), config.WithKVStore(kv))
	if err != nil {
		t.Fatalf("Error new controller: %v", err)
	}
	nw, err := testController.NewNetwork(t.Context(), "host", "host", "")
	if err != nil {
		t.Fatalf(`Error creating default "host" network: %v`, err)
	}
	ep, err := nw.CreateEndpoint(t.Context(), "newendpoint", []EndpointOption{}...)
	if err != nil {
		t.Fatalf("Error creating endpoint: %v", err)
	}

	nwKVObject := &Network{id: nw.ID()}
	err = testController.store.GetObject(nwKVObject)
	if err != nil {
		t.Errorf("Error when retrieving network key from store: %v", err)
	}
	if !nwKVObject.Exists() {
		t.Errorf("Network key should have been created.")
	}

	epKVObject := &Endpoint{network: nw, id: ep.ID()}
	err = testController.store.GetObject(epKVObject)
	if err != nil {
		t.Errorf("Error when retrieving Endpoint key from store: %v", err)
	}
	if !epKVObject.Exists() {
		t.Errorf("Endpoint key should have been created.")
	}
	kv.Close()

	// test restore of local store
	kv, err = boltdb.New(path)
	if err != nil {
		t.Fatalf("Error opening datastore: %v", err)
	}
	defer kv.Close()
	testController, err = New(t.Context(), config.WithKVStore(kv))
	if err != nil {
		t.Fatalf("Error creating controller: %v", err)
	}
	if _, err = testController.NetworkByID(nw.ID()); err != nil {
		t.Errorf("Error getting network %v", err)
	}
}

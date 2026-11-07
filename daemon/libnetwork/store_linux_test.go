package libnetwork

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	store "github.com/moby/moby/v2/daemon/libnetwork/kvstore"
	"github.com/moby/moby/v2/daemon/libnetwork/kvstore/boltdb"
)

func TestBoltdbBackend(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "boltdb.db")
	testLocalBackend(t, tmpPath)
}

func TestNoPersist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "local-kv.db")
	kv, err := boltdb.New(dbPath)
	if err != nil {
		t.Fatalf("Error opening datastore: %v", err)
	}
	testController, err := New(t.Context(), config.WithKVStore(kv))
	if err != nil {
		t.Fatalf("Error creating new controller: %v", err)
	}
	nw, err := testController.NewNetwork(t.Context(), "host", "host", "", NetworkOptionPersist(false))
	if err != nil {
		t.Fatalf(`Error creating default "host" network: %v`, err)
	}
	ep, err := nw.CreateEndpoint(t.Context(), "newendpoint", []EndpointOption{}...)
	if err != nil {
		t.Fatalf("Error creating endpoint: %v", err)
	}
	kv.Close()

	// Create a new controller using the same database-file. The network
	// should not have persisted.
	kv, err = boltdb.New(dbPath)
	if err != nil {
		t.Fatalf("Error opening datastore: %v", err)
	}
	defer kv.Close()
	testController, err = New(t.Context(), config.WithKVStore(kv))
	if err != nil {
		t.Fatalf("Error creating new controller: %v", err)
	}

	nwKVObject := &Network{id: nw.ID()}
	err = testController.store.GetObject(nwKVObject)
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("Expected %q error when retrieving network from store, got: %q", store.ErrKeyNotFound, err)
	}
	if nwKVObject.Exists() {
		t.Errorf("Network with persist=false should not be stored in KV Store")
	}

	epKVObject := &Endpoint{network: nw, id: ep.ID()}
	err = testController.store.GetObject(epKVObject)
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("Expected %v error when retrieving endpoint from store, got: %v", store.ErrKeyNotFound, err)
	}
	if epKVObject.Exists() {
		t.Errorf("Endpoint in Network with persist=false should not be stored in KV Store")
	}
}

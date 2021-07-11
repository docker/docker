package layer

import "testing"

func TestLayerStore_prune(t *testing.T) {
	s, _, cleanup := newTestStore(t)
	defer cleanup()

	lStore, ok := s.(*layerStore)
	if !ok {
		t.Fatalf("Unexpected store implementation %s", s)
	}

	// Start new transaction with cacheID that is not committed or canceled.
	_, err := lStore.store.StartTransaction("test-prune")
	if err != nil {
		t.Fatal(err)
	}

	txData, err := lStore.store.ListExistingTransactions()
	if err != nil {
		t.Fatal(err)
	}
	cacheIDs := lStore.prune(txData)
	if len(cacheIDs) != 1 {
		t.Errorf("1 directory was expected to be removed, but got %s", cacheIDs)
	}
}

package layer // import "github.com/docker/docker/layer"

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/stringid"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
)

func writeTarSplitFile(name string, tarContent []byte) error {
	f, err := os.OpenFile(name, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	fz := gzip.NewWriter(f)

	metaPacker := storage.NewJSONPacker(fz)
	defer fz.Close()

	rdr, err := asm.NewInputTarStream(bytes.NewReader(tarContent), metaPacker, nil)
	if err != nil {
		return err
	}

	if _, err := io.Copy(ioutil.Discard, rdr); err != nil {
		return err
	}

	return nil
}

func TestLayerMigration(t *testing.T) {
	// TODO Windows: Figure out why this is failing
	if runtime.GOOS == "windows" {
		t.Skip("Failing on Windows")
	}
	td, err := ioutil.TempDir("", "migration-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(td)

	layer1Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Boring configuration"), 0644),
		newTestFile("/etc/profile", []byte("# Base configuration"), 0644),
	}

	layer2Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Updated configuration"), 0644),
	}

	tar1, err := tarFromFiles(layer1Files...)
	if err != nil {
		t.Fatal(err)
	}

	tar2, err := tarFromFiles(layer2Files...)
	if err != nil {
		t.Fatal(err)
	}

	graph, err := newVFSGraphDriver(filepath.Join(td, "graphdriver-"))
	if err != nil {
		t.Fatal(err)
	}

	graphID1 := stringid.GenerateRandomID()
	if err := graph.Create(graphID1, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ApplyDiff(graphID1, "", bytes.NewReader(tar1)); err != nil {
		t.Fatal(err)
	}

	tf1 := filepath.Join(td, "tar1.json.gz")
	if err := writeTarSplitFile(tf1, tar1); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(td, "layers")
	ls, err := newStoreFromGraphDriver(root, graph, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}

	newTarDataPath := filepath.Join(td, ".migration-tardata")
	diffID, size, err := ls.(*layerStore).ChecksumForGraphID(graphID1, "", tf1, newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer1a, err := ls.(*layerStore).RegisterByGraphID(graphID1, "", diffID, newTarDataPath, size)
	if err != nil {
		t.Fatal(err)
	}

	layer1b, err := ls.Register(bytes.NewReader(tar1), "")
	if err != nil {
		t.Fatal(err)
	}

	assertReferences(t, layer1a, layer1b)
	// Attempt register, should be same
	layer2a, err := ls.Register(bytes.NewReader(tar2), layer1a.ChainID())
	if err != nil {
		t.Fatal(err)
	}

	graphID2 := stringid.GenerateRandomID()
	if err := graph.Create(graphID2, graphID1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ApplyDiff(graphID2, graphID1, bytes.NewReader(tar2)); err != nil {
		t.Fatal(err)
	}

	tf2 := filepath.Join(td, "tar2.json.gz")
	if err := writeTarSplitFile(tf2, tar2); err != nil {
		t.Fatal(err)
	}
	diffID, size, err = ls.(*layerStore).ChecksumForGraphID(graphID2, graphID1, tf2, newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer2b, err := ls.(*layerStore).RegisterByGraphID(graphID2, layer1a.ChainID(), diffID, tf2, size)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, layer2a, layer2b)

	if metadata, err := ls.Release(layer2a); err != nil {
		t.Fatal(err)
	} else if len(metadata) > 0 {
		t.Fatalf("Unexpected layer removal after first release: %#v", metadata)
	}

	metadata, err := ls.Release(layer2b)
	if err != nil {
		t.Fatal(err)
	}

	assertMetadata(t, metadata, createMetadata(layer2a))
}

func tarFromFilesInGraph(graph graphdriver.Driver, graphID, parentID string, files ...FileApplier) ([]byte, error) {
	t, err := tarFromFiles(files...)
	if err != nil {
		return nil, err
	}

	if err := graph.Create(graphID, parentID, nil); err != nil {
		return nil, err
	}
	if _, err := graph.ApplyDiff(graphID, parentID, bytes.NewReader(t)); err != nil {
		return nil, err
	}

	ar, err := graph.Diff(graphID, parentID)
	if err != nil {
		return nil, err
	}
	defer ar.Close()

	return ioutil.ReadAll(ar)
}

func TestLayerMigrationNoTarsplit(t *testing.T) {
	// TODO Windows: Figure out why this is failing
	if runtime.GOOS == "windows" {
		t.Skip("Failing on Windows")
	}
	td, err := ioutil.TempDir("", "migration-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(td)

	layer1Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Boring configuration"), 0644),
		newTestFile("/etc/profile", []byte("# Base configuration"), 0644),
	}

	layer2Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Updated configuration"), 0644),
	}

	graph, err := newVFSGraphDriver(filepath.Join(td, "graphdriver-"))
	if err != nil {
		t.Fatal(err)
	}
	graphID1 := stringid.GenerateRandomID()
	graphID2 := stringid.GenerateRandomID()

	tar1, err := tarFromFilesInGraph(graph, graphID1, "", layer1Files...)
	if err != nil {
		t.Fatal(err)
	}

	tar2, err := tarFromFilesInGraph(graph, graphID2, graphID1, layer2Files...)
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(td, "layers")
	ls, err := newStoreFromGraphDriver(root, graph, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}

	newTarDataPath := filepath.Join(td, ".migration-tardata")
	diffID, size, err := ls.(*layerStore).ChecksumForGraphID(graphID1, "", "", newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer1a, err := ls.(*layerStore).RegisterByGraphID(graphID1, "", diffID, newTarDataPath, size)
	if err != nil {
		t.Fatal(err)
	}

	layer1b, err := ls.Register(bytes.NewReader(tar1), "")
	if err != nil {
		t.Fatal(err)
	}

	assertReferences(t, layer1a, layer1b)

	// Attempt register, should be same
	layer2a, err := ls.Register(bytes.NewReader(tar2), layer1a.ChainID())
	if err != nil {
		t.Fatal(err)
	}

	diffID, size, err = ls.(*layerStore).ChecksumForGraphID(graphID2, graphID1, "", newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer2b, err := ls.(*layerStore).RegisterByGraphID(graphID2, layer1a.ChainID(), diffID, newTarDataPath, size)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, layer2a, layer2b)

	if metadata, err := ls.Release(layer2a); err != nil {
		t.Fatal(err)
	} else if len(metadata) > 0 {
		t.Fatalf("Unexpected layer removal after first release: %#v", metadata)
	}

	metadata, err := ls.Release(layer2b)
	if err != nil {
		t.Fatal(err)
	}

	assertMetadata(t, metadata, createMetadata(layer2a))
}

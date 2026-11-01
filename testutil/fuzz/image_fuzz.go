// +build gofuzz

package fuzz

import (
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"io/ioutil"
	"os"
	"runtime"
)

type mockLayerGetReleaser struct{}

func (ls *mockLayerGetReleaser) Get(layer.ChainID) (layer.Layer, error) {
	return nil, nil
}

func (ls *mockLayerGetReleaser) Release(layer.Layer) ([]layer.Metadata, error) {
	return nil, nil
}

func FuzzImage(data []byte) int {
	tmpdir, err := ioutil.TempDir("", "images-fs-store")
	defer os.RemoveAll(tmpdir)
	if err != nil {
		return -1
	}
	fsBackend, err := image.NewFSStoreBackend(tmpdir)
	if err != nil {
		return -1
	}
	mlgrMap := make(map[string]image.LayerGetReleaser)
	mlgrMap[runtime.GOOS] = &mockLayerGetReleaser{}
	store, err := image.NewImageStore(fsBackend, mlgrMap)
	if err != nil {
		return 0
	}
	_, err = store.Create(data)
	if err != nil {
		return 0
	}
	return 1
}

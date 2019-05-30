package cacheimport

import (
	"encoding/json"

	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/contentutil"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func Parse(configJSON []byte, provider DescriptorProvider, t solver.CacheExporterTarget) error {
	var config CacheConfig
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return err
	}

	return ParseConfig(config, provider, t)
}

func ParseConfig(config CacheConfig, provider DescriptorProvider, t solver.CacheExporterTarget) error {
	cache := map[int]solver.CacheExporterRecord{}

	for i := range config.Records {
		if _, err := parseRecord(config, i, provider, t, cache); err != nil {
			return err
		}
	}
	return nil
}

func parseRecord(cc CacheConfig, idx int, provider DescriptorProvider, t solver.CacheExporterTarget, cache map[int]solver.CacheExporterRecord) (solver.CacheExporterRecord, error) {
	if r, ok := cache[idx]; ok {
		if r == nil {
			return nil, errors.Errorf("invalid looping record")
		}
		return r, nil
	}

	if idx < 0 || idx >= len(cc.Records) {
		return nil, errors.Errorf("invalid record ID: %d", idx)
	}
	rec := cc.Records[idx]

	r := t.Add(rec.Digest)
	cache[idx] = nil
	for i, inputs := range rec.Inputs {
		for _, inp := range inputs {
			src, err := parseRecord(cc, inp.LinkIndex, provider, t, cache)
			if err != nil {
				return nil, err
			}
			r.LinkFrom(src, i, inp.Selector)
		}
	}

	for _, res := range rec.Results {
		visited := map[int]struct{}{}
		remote, err := getRemoteChain(cc.Layers, res.LayerIndex, provider, visited)
		if err != nil {
			return nil, err
		}
		if remote != nil {
			r.AddResult(res.CreatedAt, remote)
		}
	}

	cache[idx] = r
	return r, nil
}

func getRemoteChain(layers []CacheLayer, idx int, provider DescriptorProvider, visited map[int]struct{}) (*solver.Remote, error) {
	if _, ok := visited[idx]; ok {
		return nil, errors.Errorf("invalid looping layer")
	}
	visited[idx] = struct{}{}

	if idx < 0 || idx >= len(layers) {
		return nil, errors.Errorf("invalid layer index %d", idx)
	}

	l := layers[idx]

	descPair, ok := provider[l.Blob]
	if !ok {
		return nil, nil
	}

	var r *solver.Remote
	if l.ParentIndex != -1 {
		var err error
		r, err = getRemoteChain(layers, l.ParentIndex, provider, visited)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, nil
		}
		r.Descriptors = append(r.Descriptors, descPair.Descriptor)
		mp := contentutil.NewMultiProvider(r.Provider)
		mp.Add(descPair.Descriptor.Digest, descPair.Provider)
		r.Provider = mp
		return r, nil
	}
	return &solver.Remote{
		Descriptors: []ocispec.Descriptor{descPair.Descriptor},
		Provider:    descPair.Provider,
	}, nil

}

package cacheimport

import (
	"fmt"
	"sort"

	"github.com/containerd/containerd/content"
	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// sortConfig sorts the config structure to make sure it is deterministic
func sortConfig(cc *CacheConfig) {
	type indexedLayer struct {
		oldIndex int
		newIndex int
		l        CacheLayer
	}

	unsortedLayers := make([]*indexedLayer, len(cc.Layers))
	sortedLayers := make([]*indexedLayer, len(cc.Layers))

	for i, l := range cc.Layers {
		il := &indexedLayer{oldIndex: i, l: l}
		unsortedLayers[i] = il
		sortedLayers[i] = il
	}
	sort.Slice(sortedLayers, func(i, j int) bool {
		li := sortedLayers[i].l
		lj := sortedLayers[j].l
		if li.Blob == lj.Blob {
			return li.ParentIndex < lj.ParentIndex
		}
		return li.Blob < lj.Blob
	})
	for i, l := range sortedLayers {
		l.newIndex = i
	}

	layers := make([]CacheLayer, len(sortedLayers))
	for i, l := range sortedLayers {
		if pID := l.l.ParentIndex; pID != -1 {
			l.l.ParentIndex = unsortedLayers[pID].newIndex
		}
		layers[i] = l.l
	}

	type indexedRecord struct {
		oldIndex int
		newIndex int
		r        CacheRecord
	}

	unsortedRecords := make([]*indexedRecord, len(cc.Records))
	sortedRecords := make([]*indexedRecord, len(cc.Records))

	for i, r := range cc.Records {
		ir := &indexedRecord{oldIndex: i, r: r}
		unsortedRecords[i] = ir
		sortedRecords[i] = ir
	}
	sort.Slice(sortedRecords, func(i, j int) bool {
		ri := sortedRecords[i].r
		rj := sortedRecords[j].r
		if ri.Digest != rj.Digest {
			return ri.Digest < rj.Digest
		}
		if len(ri.Inputs) != len(ri.Inputs) {
			return len(ri.Inputs) < len(ri.Inputs)
		}
		for i, inputs := range ri.Inputs {
			if len(ri.Inputs[i]) != len(rj.Inputs[i]) {
				return len(ri.Inputs[i]) < len(rj.Inputs[i])
			}
			for j := range inputs {
				if ri.Inputs[i][j].Selector != rj.Inputs[i][j].Selector {
					return ri.Inputs[i][j].Selector != rj.Inputs[i][j].Selector
				}
				return cc.Records[ri.Inputs[i][j].LinkIndex].Digest < cc.Records[rj.Inputs[i][j].LinkIndex].Digest
			}
		}
		return ri.Digest < rj.Digest
	})
	for i, l := range sortedRecords {
		l.newIndex = i
	}

	records := make([]CacheRecord, len(sortedRecords))
	for i, r := range sortedRecords {
		for j := range r.r.Results {
			r.r.Results[j].LayerIndex = unsortedLayers[r.r.Results[j].LayerIndex].newIndex
		}
		for j, inputs := range r.r.Inputs {
			for k := range inputs {
				r.r.Inputs[j][k].LinkIndex = unsortedRecords[r.r.Inputs[j][k].LinkIndex].newIndex
			}
			sort.Slice(inputs, func(i, j int) bool {
				return inputs[i].LinkIndex < inputs[j].LinkIndex
			})
		}
		records[i] = r.r
	}

	cc.Layers = layers
	cc.Records = records
}

func outputKey(dgst digest.Digest, idx int) digest.Digest {
	return digest.FromBytes([]byte(fmt.Sprintf("%s@%d", dgst, idx)))
}

type nlink struct {
	dgst     digest.Digest
	input    int
	selector string
}
type normalizeState struct {
	added map[*item]*item
	links map[*item]map[nlink]map[digest.Digest]struct{}
	byKey map[digest.Digest]*item
	next  int
}

func normalizeItem(it *item, state *normalizeState) (*item, error) {
	if it2, ok := state.added[it]; ok {
		return it2, nil
	}

	if len(it.links) == 0 {
		id := it.dgst
		if it2, ok := state.byKey[id]; ok {
			state.added[it] = it2
			return it2, nil
		}
		state.byKey[id] = it
		state.added[it] = it
		return nil, nil
	}

	matches := map[digest.Digest]struct{}{}

	// check if there is already a matching record
	for i, m := range it.links {
		if len(m) == 0 {
			return nil, errors.Errorf("invalid incomplete links")
		}
		for l := range m {
			nl := nlink{dgst: it.dgst, input: i, selector: l.selector}
			it2, err := normalizeItem(l.src, state)
			if err != nil {
				return nil, err
			}
			links := state.links[it2][nl]
			if i == 0 {
				for id := range links {
					matches[id] = struct{}{}
				}
			} else {
				for id := range matches {
					if _, ok := links[id]; !ok {
						delete(matches, id)
					}
				}
			}
		}
	}

	var id digest.Digest

	links := it.links

	if len(matches) > 0 {
		for m := range matches {
			if id == "" || id > m {
				id = m
			}
		}
	} else {
		// keep tmp IDs deterministic
		state.next++
		id = digest.FromBytes([]byte(fmt.Sprintf("%d", state.next)))
		state.byKey[id] = it
		it.links = make([]map[link]struct{}, len(it.links))
		for i := range it.links {
			it.links[i] = map[link]struct{}{}
		}
	}

	it2 := state.byKey[id]
	state.added[it] = it2

	for i, m := range links {
		for l := range m {
			subIt, err := normalizeItem(l.src, state)
			if err != nil {
				return nil, err
			}
			it2.links[i][link{src: subIt, selector: l.selector}] = struct{}{}

			nl := nlink{dgst: it.dgst, input: i, selector: l.selector}
			if _, ok := state.links[subIt]; !ok {
				state.links[subIt] = map[nlink]map[digest.Digest]struct{}{}
			}
			if _, ok := state.links[subIt][nl]; !ok {
				state.links[subIt][nl] = map[digest.Digest]struct{}{}
			}
			state.links[subIt][nl][id] = struct{}{}
		}
	}

	return it2, nil
}

type marshalState struct {
	layers      []CacheLayer
	chainsByID  map[string]int
	descriptors DescriptorProvider

	records       []CacheRecord
	recordsByItem map[*item]int
}

func marshalRemote(r *solver.Remote, state *marshalState) string {
	if len(r.Descriptors) == 0 {
		return ""
	}
	type Remote struct {
		Descriptors []ocispec.Descriptor
		Provider    content.Provider
	}
	var parentID string
	if len(r.Descriptors) > 1 {
		r2 := &solver.Remote{
			Descriptors: r.Descriptors[:len(r.Descriptors)-1],
			Provider:    r.Provider,
		}
		parentID = marshalRemote(r2, state)
	}
	desc := r.Descriptors[len(r.Descriptors)-1]

	state.descriptors[desc.Digest] = DescriptorProviderPair{
		Descriptor: desc,
		Provider:   r.Provider,
	}

	id := desc.Digest.String() + parentID

	if _, ok := state.chainsByID[id]; ok {
		return id
	}

	state.chainsByID[id] = len(state.layers)
	l := CacheLayer{
		Blob:        desc.Digest,
		ParentIndex: -1,
	}
	if parentID != "" {
		l.ParentIndex = state.chainsByID[parentID]
	}
	state.layers = append(state.layers, l)
	return id
}

func marshalItem(it *item, state *marshalState) error {
	if _, ok := state.recordsByItem[it]; ok {
		return nil
	}

	rec := CacheRecord{
		Digest: it.dgst,
		Inputs: make([][]CacheInput, len(it.links)),
	}

	for i, m := range it.links {
		for l := range m {
			if err := marshalItem(l.src, state); err != nil {
				return err
			}
			idx, ok := state.recordsByItem[l.src]
			if !ok {
				return errors.Errorf("invalid source record: %v", l.src)
			}
			rec.Inputs[i] = append(rec.Inputs[i], CacheInput{
				Selector:  l.selector,
				LinkIndex: idx,
			})
		}
	}

	if it.result != nil {
		id := marshalRemote(it.result, state)
		if id != "" {
			idx, ok := state.chainsByID[id]
			if !ok {
				return errors.Errorf("parent chainid not found")
			}
			rec.Results = append(rec.Results, CacheResult{LayerIndex: idx, CreatedAt: it.resultTime})
		}
	}

	state.recordsByItem[it] = len(state.records)
	state.records = append(state.records, rec)
	return nil
}

func isSubRemote(sub, main solver.Remote) bool {
	if len(sub.Descriptors) > len(main.Descriptors) {
		return false
	}
	for i := range sub.Descriptors {
		if sub.Descriptors[i].Digest != main.Descriptors[i].Digest {
			return false
		}
	}
	return true
}

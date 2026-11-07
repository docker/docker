package cacheimport

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"

	cerrdefs "github.com/containerd/errdefs"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/compression"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// sortConfig sorts the config structure to make sure it is deterministic
func sortConfig(cc *cacheimporttypes.CacheConfig) {
	type indexedLayer struct {
		oldIndex int
		newIndex int
		l        cacheimporttypes.CacheLayer
	}

	unsortedLayers := make([]*indexedLayer, len(cc.Layers))
	sortedLayers := make([]*indexedLayer, len(cc.Layers))

	for i, l := range cc.Layers {
		il := &indexedLayer{oldIndex: i, l: l}
		unsortedLayers[i] = il
		sortedLayers[i] = il
	}
	slices.SortFunc(sortedLayers, func(a, b *indexedLayer) int {
		return cmp.Or(cmp.Compare(a.l.Blob, b.l.Blob), cmp.Compare(a.l.ParentIndex, b.l.ParentIndex))
	})
	for i, l := range sortedLayers {
		l.newIndex = i
	}

	layers := make([]cacheimporttypes.CacheLayer, len(sortedLayers))
	for i, l := range sortedLayers {
		if pID := l.l.ParentIndex; pID != -1 {
			l.l.ParentIndex = unsortedLayers[pID].newIndex
		}
		layers[i] = l.l
	}

	type indexedRecord struct {
		oldIndex int
		newIndex int
		r        cacheimporttypes.CacheRecord
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
		if len(ri.Inputs) != len(rj.Inputs) {
			return len(ri.Inputs) < len(rj.Inputs)
		}
		for i, inputs := range ri.Inputs {
			if len(ri.Inputs[i]) != len(rj.Inputs[i]) {
				return len(ri.Inputs[i]) < len(rj.Inputs[i])
			}
			for j := range inputs {
				if ri.Inputs[i][j].Selector != rj.Inputs[i][j].Selector {
					return ri.Inputs[i][j].Selector < rj.Inputs[i][j].Selector
				}
				inputDigesti := cc.Records[ri.Inputs[i][j].LinkIndex].Digest
				inputDigestj := cc.Records[rj.Inputs[i][j].LinkIndex].Digest
				if inputDigesti != inputDigestj {
					return inputDigesti < inputDigestj
				}
			}
		}
		return false
	})
	for i, l := range sortedRecords {
		l.newIndex = i
	}

	records := make([]cacheimporttypes.CacheRecord, len(sortedRecords))
	for i, r := range sortedRecords {
		for j := range r.r.Results {
			r.r.Results[j].LayerIndex = unsortedLayers[r.r.Results[j].LayerIndex].newIndex
		}
		for j, inputs := range r.r.Inputs {
			for k := range inputs {
				r.r.Inputs[j][k].LinkIndex = unsortedRecords[r.r.Inputs[j][k].LinkIndex].newIndex
			}
			slices.SortFunc(inputs, func(a, b cacheimporttypes.CacheInput) int {
				return cmp.Compare(a.LinkIndex, b.LinkIndex)
			})
		}
		records[i] = r.r
	}

	cc.Layers = layers
	cc.Records = records
}

func outputKey(dgst digest.Digest, idx int) digest.Digest {
	return digest.FromBytes(fmt.Appendf(nil, "%s@%d", dgst, idx))
}

type nlink struct {
	dgst     digest.Digest
	input    int
	selector string
}

type marshalState struct {
	layers      []cacheimporttypes.CacheLayer
	chainsByID  map[string]int
	descriptors DescriptorProvider

	records       []cacheimporttypes.CacheRecord
	recordsByItem map[*item]int
}

func marshalRemote(ctx context.Context, r *solver.Remote, state *marshalState) string {
	if len(r.Descriptors) == 0 {
		return ""
	}

	if r.Provider != nil {
		for _, d := range r.Descriptors {
			if _, err := r.Provider.Info(ctx, d.Digest); err != nil && !cerrdefs.IsNotImplemented(err) {
				bklog.G(ctx).WithError(err).Warnf("skipping cache result: blob %s is not available", d.Digest)
				return ""
			}
		}
	}

	var parentID string
	for _, desc := range r.Descriptors {
		if desc.MediaType != "" {
			desc = compression.ConvertAllLayerMediaTypes(ctx, true, desc)[0]
		}

		state.descriptors[desc.Digest] = DescriptorProviderPair{
			Descriptor: desc,
			Provider:   r.Provider,
		}

		id := desc.Digest.String() + parentID

		if _, ok := state.chainsByID[id]; !ok {
			state.chainsByID[id] = len(state.layers)
			l := cacheimporttypes.CacheLayer{
				Blob:        desc.Digest,
				ParentIndex: -1,
			}
			if parentID != "" {
				l.ParentIndex = state.chainsByID[parentID]
			}
			state.layers = append(state.layers, l)
		}
		parentID = id
	}
	return parentID
}

func marshalItem(ctx context.Context, it *item, state *marshalState) error {
	if _, ok := state.recordsByItem[it]; ok {
		return nil
	}
	state.recordsByItem[it] = -1

	rec := cacheimporttypes.CacheRecord{
		Digest: it.dgst,
		Inputs: make([][]cacheimporttypes.CacheInput, len(it.parents)),
	}

	for i, m := range it.parents {
		for l := range m {
			if err := marshalItem(ctx, l.src, state); err != nil {
				return err
			}
			idx, ok := state.recordsByItem[l.src]
			if !ok {
				return errors.Errorf("invalid source record: %v", l.src)
			}
			if idx == -1 {
				continue
			}
			rec.Inputs[i] = append(rec.Inputs[i], cacheimporttypes.CacheInput{
				Selector:  l.selector,
				LinkIndex: idx,
			})
		}
	}

	if res := it.bestResult(); res != nil {
		id := marshalRemote(ctx, res.Result, state)
		if id != "" {
			idx, ok := state.chainsByID[id]
			if !ok {
				return errors.New("parent chainid not found")
			}
			rec.Results = append(rec.Results, cacheimporttypes.CacheResult{LayerIndex: idx, CreatedAt: res.CreatedAt})
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

package containerd

import (
	"context"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/diff/apply"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
	"github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// remappedDiffService is a containerd diff service for a daemon with user
// namespace remapping.
// It stores layer files in snapshots with the remapped host IDs, as the graph
// drivers do, so that snapshots never contain files owned by host root, such
// as setuid binaries.
// Layer archives keep the container IDs.
//
// TODO: Use the containerd diff service once its API takes the ID mappings.
type remappedDiffService struct {
	contentStore   func() content.Store
	uidMap, gidMap []specs.LinuxIDMapping
}

// NewRemappedDiffService returns a containerd diff service which applies layers
// with their IDs mapped to the host IDs of idMapping, and creates layers with
// the IDs mapped back to the container IDs.
// It applies and compares mounts in the daemon process.
func NewRemappedDiffService(contentStore func() content.Store, idMapping user.IdentityMapping) containerd.DiffService {
	return &remappedDiffService{
		contentStore: contentStore,
		uidMap:       toSpecMapping(idMapping.UIDMaps),
		gidMap:       toSpecMapping(idMapping.GIDMaps),
	}
}

func (s *remappedDiffService) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	opts = append(opts[:len(opts):len(opts)], diff.WithApplyIDMapping(s.uidMap, s.gidMap))
	return apply.NewFileSystemApplier(s.contentStore()).Apply(ctx, desc, mounts, opts...)
}

func (s *remappedDiffService) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
	opts = append(opts[:len(opts):len(opts)], diff.WithIDMapping(s.uidMap, s.gidMap))
	return walking.NewWalkingDiff(s.contentStore()).Compare(ctx, lower, upper, opts...)
}

func toSpecMapping(idMaps []user.IDMap) []specs.LinuxIDMapping {
	mappings := make([]specs.LinuxIDMapping, 0, len(idMaps))
	for _, m := range idMaps {
		mappings = append(mappings, specs.LinuxIDMapping{
			ContainerID: uint32(m.ID),
			HostID:      uint32(m.ParentID),
			Size:        uint32(m.Count),
		})
	}
	return mappings
}

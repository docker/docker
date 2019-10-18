/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package snapshots

import (
	"context"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	"github.com/containerd/containerd/snapshots"
)

// snapshotter wraps snapshots.Snapshotter with proper events published.
type snapshotter struct {
	snapshots.Snapshotter
	publisher events.Publisher
}

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.ServicePlugin,
		ID:   services.SnapshotsService,
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			m, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			db := m.(*metadata.DB)
			ss := make(map[string]snapshots.Snapshotter)
			for n, sn := range db.Snapshotters() {
				ss[n] = newSnapshotter(sn, ic.Events)
			}
			return ss, nil
		},
	})
}

func newSnapshotter(sn snapshots.Snapshotter, publisher events.Publisher) snapshots.Snapshotter {
	return &snapshotter{
		Snapshotter: sn,
		publisher:   publisher,
	}
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	mounts, err := s.Snapshotter.Prepare(ctx, key, parent, opts...)
	if err != nil {
		return nil, err
	}
	if err := s.publisher.Publish(ctx, "/snapshot/prepare", &eventstypes.SnapshotPrepare{
		Key:    key,
		Parent: parent,
	}); err != nil {
		return nil, err
	}
	return mounts, nil
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	if err := s.Snapshotter.Commit(ctx, name, key, opts...); err != nil {
		return err
	}
	return s.publisher.Publish(ctx, "/snapshot/commit", &eventstypes.SnapshotCommit{
		Key:  key,
		Name: name,
	})
}

func (s *snapshotter) Remove(ctx context.Context, key string) error {
	if err := s.Snapshotter.Remove(ctx, key); err != nil {
		return err
	}
	return s.publisher.Publish(ctx, "/snapshot/remove", &eventstypes.SnapshotRemove{
		Key: key,
	})
}

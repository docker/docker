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

package plugin

import (
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/diff/apply"
	"github.com/containerd/containerd/diff/walking"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.DiffPlugin,
		ID:   "walking",
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			md, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			cs := md.(*metadata.DB).ContentStore()

			return diffPlugin{
				Comparer: walking.NewWalkingDiff(cs),
				Applier:  apply.NewFileSystemApplier(cs),
			}, nil
		},
	})
}

type diffPlugin struct {
	diff.Comparer
	diff.Applier
}

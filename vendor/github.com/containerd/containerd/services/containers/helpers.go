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

package containers

import (
	api "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/containerd/containerd/containers"
)

func containersToProto(containers []containers.Container) []api.Container {
	var containerspb []api.Container

	for _, image := range containers {
		containerspb = append(containerspb, containerToProto(&image))
	}

	return containerspb
}

func containerToProto(container *containers.Container) api.Container {
	return api.Container{
		ID:     container.ID,
		Labels: container.Labels,
		Image:  container.Image,
		Runtime: &api.Container_Runtime{
			Name:    container.Runtime.Name,
			Options: container.Runtime.Options,
		},
		Spec:        container.Spec,
		Snapshotter: container.Snapshotter,
		SnapshotKey: container.SnapshotKey,
		CreatedAt:   container.CreatedAt,
		UpdatedAt:   container.UpdatedAt,
		Extensions:  container.Extensions,
	}
}

func containerFromProto(containerpb *api.Container) containers.Container {
	var runtime containers.RuntimeInfo
	if containerpb.Runtime != nil {
		runtime = containers.RuntimeInfo{
			Name:    containerpb.Runtime.Name,
			Options: containerpb.Runtime.Options,
		}
	}
	return containers.Container{
		ID:          containerpb.ID,
		Labels:      containerpb.Labels,
		Image:       containerpb.Image,
		Runtime:     runtime,
		Spec:        containerpb.Spec,
		Snapshotter: containerpb.Snapshotter,
		SnapshotKey: containerpb.SnapshotKey,
		Extensions:  containerpb.Extensions,
	}
}

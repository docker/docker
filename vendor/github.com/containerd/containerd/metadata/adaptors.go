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

package metadata

import (
	"strings"

	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
)

func adaptImage(o interface{}) filters.Adaptor {
	obj := o.(images.Image)
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}

		switch fieldpath[0] {
		case "name":
			return obj.Name, len(obj.Name) > 0
		case "target":
			if len(fieldpath) < 2 {
				return "", false
			}

			switch fieldpath[1] {
			case "digest":
				return obj.Target.Digest.String(), len(obj.Target.Digest) > 0
			case "mediatype":
				return obj.Target.MediaType, len(obj.Target.MediaType) > 0
			}
		case "labels":
			return checkMap(fieldpath[1:], obj.Labels)
			// TODO(stevvooe): Greater/Less than filters would be awesome for
			// size. Let's do it!
		case "annotations":
			return checkMap(fieldpath[1:], obj.Target.Annotations)
		}

		return "", false
	})
}
func adaptContainer(o interface{}) filters.Adaptor {
	obj := o.(containers.Container)
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}

		switch fieldpath[0] {
		case "id":
			return obj.ID, len(obj.ID) > 0
		case "runtime":
			if len(fieldpath) <= 1 {
				return "", false
			}

			switch fieldpath[1] {
			case "name":
				return obj.Runtime.Name, len(obj.Runtime.Name) > 0
			default:
				return "", false
			}
		case "image":
			return obj.Image, len(obj.Image) > 0
		case "labels":
			return checkMap(fieldpath[1:], obj.Labels)
		}

		return "", false
	})
}

func adaptContentInfo(info content.Info) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}

		switch fieldpath[0] {
		case "digest":
			return info.Digest.String(), true
		case "size":
			// TODO: support size based filtering
		case "labels":
			return checkMap(fieldpath[1:], info.Labels)
		}

		return "", false
	})
}

func adaptContentStatus(status content.Status) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}
		switch fieldpath[0] {
		case "ref":
			return status.Ref, true
		}

		return "", false
	})
}

func adaptLease(lease leases.Lease) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}

		switch fieldpath[0] {
		case "id":
			return lease.ID, len(lease.ID) > 0
		case "labels":
			return checkMap(fieldpath[1:], lease.Labels)
		}

		return "", false
	})
}

func checkMap(fieldpath []string, m map[string]string) (string, bool) {
	if len(m) == 0 {
		return "", false
	}

	value, ok := m[strings.Join(fieldpath, ".")]
	return value, ok
}

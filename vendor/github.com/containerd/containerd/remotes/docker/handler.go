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

package docker

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	// labelDistributionSource describes the source blob comes from.
	labelDistributionSource = "containerd.io/distribution.source"
)

// AppendDistributionSourceLabel updates the label of blob with distribution source.
func AppendDistributionSourceLabel(manager content.Manager, ref string) (images.HandlerFunc, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse("dummy://" + refspec.Locator)
	if err != nil {
		return nil, err
	}

	source, repo := u.Hostname(), strings.TrimPrefix(u.Path, "/")
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		info, err := manager.Info(ctx, desc.Digest)
		if err != nil {
			return nil, err
		}

		key := distributionSourceLabelKey(source)

		originLabel := ""
		if info.Labels != nil {
			originLabel = info.Labels[key]
		}
		value := appendDistributionSourceLabel(originLabel, repo)

		// The repo name has been limited under 256 and the distribution
		// label might hit the limitation of label size, when blob data
		// is used as the very, very common layer.
		if err := labels.Validate(key, value); err != nil {
			log.G(ctx).Warnf("skip to append distribution label: %s", err)
			return nil, nil
		}

		info = content.Info{
			Digest: desc.Digest,
			Labels: map[string]string{
				key: value,
			},
		}
		_, err = manager.Update(ctx, info, fmt.Sprintf("labels.%s", key))
		return nil, err
	}, nil
}

func appendDistributionSourceLabel(originLabel, repo string) string {
	repos := []string{}
	if originLabel != "" {
		repos = strings.Split(originLabel, ",")
	}
	repos = append(repos, repo)

	// use emtpy string to present duplicate items
	for i := 1; i < len(repos); i++ {
		tmp, j := repos[i], i-1
		for ; j >= 0 && repos[j] >= tmp; j-- {
			if repos[j] == tmp {
				tmp = ""
			}
			repos[j+1] = repos[j]
		}
		repos[j+1] = tmp
	}

	i := 0
	for ; i < len(repos) && repos[i] == ""; i++ {
	}

	return strings.Join(repos[i:], ",")
}

func distributionSourceLabelKey(source string) string {
	return fmt.Sprintf("%s.%s", labelDistributionSource, source)
}

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

package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/moby/sys/userns"
	"golang.org/x/sys/unix"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/errdefs"
)

func apply(ctx context.Context, mounts []mount.Mount, r io.Reader, sync bool, opts ...archive.ApplyOpt) (retErr error) {
	switch {
	case len(mounts) == 1 && mounts[0].Type == "overlay":
		// OverlayConvertWhiteout (mknod c 0 0) doesn't work in userns.
		// https://github.com/containerd/containerd/issues/3762
		if userns.RunningInUserNS() {
			break
		}
		path, parents, err := getOverlayPath(mounts[0].Options)
		if err != nil {
			if errdefs.IsInvalidArgument(err) {
				break
			}
			return err
		}
		opts := append([]archive.ApplyOpt{
			archive.WithConvertWhiteout(archive.OverlayConvertWhiteout),
		}, opts...)
		if len(parents) > 0 {
			opts = append(opts, archive.WithParents(parents))
		}
		_, err = archive.Apply(ctx, path, r, opts...)
		if err == nil && sync {
			err = doSyncFs(path)
		}
		return err
	case sync && len(mounts) == 1 && mounts[0].Type == "bind":
		defer func() {
			if retErr != nil {
				return
			}

			retErr = doSyncFs(mounts[0].Source)
		}()
	}
	return mount.WithTempMount(ctx, mounts, func(root string) error {
		_, err := archive.Apply(ctx, root, r, opts...)
		return err
	})
}

func getOverlayPath(options []string) (upper string, lower []string, err error) {
	const upperdirPrefix = "upperdir="
	const lowerdirPrefix = "lowerdir="

	for _, o := range options {
		if after, ok := strings.CutPrefix(o, upperdirPrefix); ok {
			upper = after
		} else if after, ok := strings.CutPrefix(o, lowerdirPrefix); ok {
			lower = strings.Split(after, ":")
		}
	}
	if upper == "" {
		return "", nil, fmt.Errorf("upperdir not found: %w", errdefs.ErrInvalidArgument)
	}

	return
}

func doSyncFs(file string) error {
	fd, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", file, err)
	}
	defer fd.Close()

	err = unix.Syncfs(int(fd.Fd()))
	if err != nil {
		return fmt.Errorf("failed to syncfs for %s: %w", file, err)
	}
	return nil
}

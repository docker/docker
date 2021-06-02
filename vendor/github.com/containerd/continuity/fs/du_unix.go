// +build !windows

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

package fs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
)

// blocksUnitSize is the unit used by `st_blocks` in `stat` in bytes.
// See https://man7.org/linux/man-pages/man2/stat.2.html
//   st_blocks
//     This field indicates the number of blocks allocated to the
//     file, in 512-byte units.  (This may be smaller than
//     st_size/512 when the file has holes.)
const blocksUnitSize = 512

type inode struct {
	// TODO(stevvooe): Can probably reduce memory usage by not tracking
	// device, but we can leave this right for now.
	dev, ino uint64
}

func newInode(stat *syscall.Stat_t) inode {
	return inode{
		// Dev is uint32 on darwin/bsd, uint64 on linux/solaris/freebsd
		dev: uint64(stat.Dev), // nolint: unconvert
		// Ino is uint32 on bsd, uint64 on darwin/linux/solaris/freebsd
		ino: uint64(stat.Ino), // nolint: unconvert
	}
}

func diskUsage(ctx context.Context, roots ...string) (Usage, error) {

	var (
		size   int64
		inodes = map[inode]struct{}{} // expensive!
	)

	for _, root := range roots {
		if err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			stat := fi.Sys().(*syscall.Stat_t)
			inoKey := newInode(stat)
			if _, ok := inodes[inoKey]; !ok {
				inodes[inoKey] = struct{}{}
				size += stat.Blocks * blocksUnitSize
			}

			return nil
		}); err != nil {
			return Usage{}, err
		}
	}

	return Usage{
		Inodes: int64(len(inodes)),
		Size:   size,
	}, nil
}

func diffUsage(ctx context.Context, a, b string) (Usage, error) {
	var (
		size   int64
		inodes = map[inode]struct{}{} // expensive!
	)

	if err := Changes(ctx, a, b, func(kind ChangeKind, _ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if kind == ChangeKindAdd || kind == ChangeKindModify {
			stat := fi.Sys().(*syscall.Stat_t)
			inoKey := newInode(stat)
			if _, ok := inodes[inoKey]; !ok {
				inodes[inoKey] = struct{}{}
				size += stat.Blocks * blocksUnitSize
			}

			return nil

		}
		return nil
	}); err != nil {
		return Usage{}, err
	}

	return Usage{
		Inodes: int64(len(inodes)),
		Size:   size,
	}, nil
}

package vfs // import "github.com/docker/docker/daemon/graphdriver/vfs"

import "github.com/docker/docker/daemon/graphdriver/copy"

func dirCopy(srcDir, dstDir string, concurrency int) error {
	return copy.DirCopyWithConcurrency(srcDir, dstDir, copy.Content, false, concurrency)
}

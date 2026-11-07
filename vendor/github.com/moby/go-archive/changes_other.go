//go:build !linux

package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func collectFileInfoForChanges(ctx context.Context, oldDir, newDir string) (*FileInfo, *FileInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type treeResult struct {
		old  bool
		root *FileInfo
		err  error
	}
	results := make(chan treeResult, 2)
	go func() {
		root, err := collectFileInfo(ctx, oldDir)
		results <- treeResult{old: true, root: root, err: err}
	}()
	go func() {
		root, err := collectFileInfo(ctx, newDir)
		results <- treeResult{root: root, err: err}
	}()

	var oldRoot, newRoot *FileInfo
	var firstErr error
	for range 2 {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		if result.old {
			oldRoot = result.root
		} else {
			newRoot = result.root
		}
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}

	return oldRoot, newRoot, ctx.Err()
}

func collectFileInfo(ctx context.Context, sourceDir string) (*FileInfo, error) {
	root := newRootFileInfo()

	err := filepath.WalkDir(sourceDir, func(path string, _ os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}

		// Rebase path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		// As this runs on the daemon side, file paths are OS specific.
		relPath = filepath.Join(string(os.PathSeparator), relPath)

		// See https://github.com/golang/go/issues/9168 - bug in filepath.Join.
		// Temporary workaround. If the returned path starts with two backslashes,
		// trim it down to a single backslash. Only relevant on Windows.
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(relPath, `\\`) {
				relPath = relPath[1:]
			}
		}

		if relPath == string(os.PathSeparator) {
			return nil
		}

		parent := root.LookUp(filepath.Dir(relPath))
		if parent == nil {
			return fmt.Errorf("collectFileInfo: Unexpectedly no parent for %s", relPath)
		}

		s, err := os.Lstat(path)
		if err != nil {
			return err
		}

		info := &FileInfo{
			name:     filepath.Base(relPath),
			children: make(map[string]*FileInfo),
			parent:   parent,
			stat:     s,
		}

		info.capability, _ = lgetxattr(path, "security.capability")

		parent.children[info.name] = info

		return nil
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}

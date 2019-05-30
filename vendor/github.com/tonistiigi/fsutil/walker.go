package fsutil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/fileutils"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
)

type WalkOpt struct {
	IncludePatterns []string
	ExcludePatterns []string
	// FollowPaths contains symlinks that are resolved into include patterns
	// before performing the fs walk
	FollowPaths []string
	Map         FilterFunc
}

func Walk(ctx context.Context, p string, opt *WalkOpt, fn filepath.WalkFunc) error {
	root, err := filepath.EvalSymlinks(p)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve %s", root)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return errors.Wrapf(err, "failed to stat: %s", root)
	}
	if !fi.IsDir() {
		return errors.Errorf("%s is not a directory", root)
	}

	var pm *fileutils.PatternMatcher
	if opt != nil && opt.ExcludePatterns != nil {
		pm, err = fileutils.NewPatternMatcher(opt.ExcludePatterns)
		if err != nil {
			return errors.Wrapf(err, "invalid excludepaths %s", opt.ExcludePatterns)
		}
	}

	var includePatterns []string
	if opt != nil && opt.IncludePatterns != nil {
		includePatterns = make([]string, len(opt.IncludePatterns))
		for k := range opt.IncludePatterns {
			includePatterns[k] = filepath.Clean(opt.IncludePatterns[k])
		}
	}
	if opt != nil && opt.FollowPaths != nil {
		targets, err := FollowLinks(p, opt.FollowPaths)
		if err != nil {
			return err
		}
		if targets != nil {
			includePatterns = append(includePatterns, targets...)
			includePatterns = dedupePaths(includePatterns)
		}
	}

	var lastIncludedDir string

	seenFiles := make(map[uint64]string)
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) (retErr error) {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		defer func() {
			if retErr != nil && isNotExist(retErr) {
				retErr = filepath.SkipDir
			}
		}()
		origpath := path
		path, err = filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// Skip root
		if path == "." {
			return nil
		}

		if opt != nil {
			if includePatterns != nil {
				skip := false
				if lastIncludedDir != "" {
					if strings.HasPrefix(path, lastIncludedDir+string(filepath.Separator)) {
						skip = true
					}
				}

				if !skip {
					matched := false
					partial := true
					for _, p := range includePatterns {
						if ok, p := matchPrefix(p, path); ok {
							matched = true
							if !p {
								partial = false
								break
							}
						}
					}
					if !matched {
						if fi.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if !partial && fi.IsDir() {
						lastIncludedDir = path
					}
				}
			}
			if pm != nil {
				m, err := pm.Matches(path)
				if err != nil {
					return errors.Wrap(err, "failed to match excludepatterns")
				}

				if m {
					if fi.IsDir() {
						if !pm.Exclusions() {
							return filepath.SkipDir
						}
						dirSlash := path + string(filepath.Separator)
						for _, pat := range pm.Patterns() {
							if !pat.Exclusion() {
								continue
							}
							patStr := pat.String() + string(filepath.Separator)
							if strings.HasPrefix(patStr, dirSlash) {
								goto passedFilter
							}
						}
						return filepath.SkipDir
					}
					return nil
				}
			}
		}

	passedFilter:
		stat, err := mkstat(origpath, path, fi, seenFiles)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if opt != nil && opt.Map != nil {
				if allowed := opt.Map(stat.Path, stat); !allowed {
					return nil
				}
			}
			if err := fn(stat.Path, &StatInfo{stat}, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

type StatInfo struct {
	*types.Stat
}

func (s *StatInfo) Name() string {
	return filepath.Base(s.Stat.Path)
}
func (s *StatInfo) Size() int64 {
	return s.Stat.Size_
}
func (s *StatInfo) Mode() os.FileMode {
	return os.FileMode(s.Stat.Mode)
}
func (s *StatInfo) ModTime() time.Time {
	return time.Unix(s.Stat.ModTime/1e9, s.Stat.ModTime%1e9)
}
func (s *StatInfo) IsDir() bool {
	return s.Mode().IsDir()
}
func (s *StatInfo) Sys() interface{} {
	return s.Stat
}

func matchPrefix(pattern, name string) (bool, bool) {
	count := strings.Count(name, string(filepath.Separator))
	partial := false
	if strings.Count(pattern, string(filepath.Separator)) > count {
		pattern = trimUntilIndex(pattern, string(filepath.Separator), count)
		partial = true
	}
	m, _ := filepath.Match(pattern, name)
	return m, partial
}

func trimUntilIndex(str, sep string, count int) string {
	s := str
	i := 0
	c := 0
	for {
		idx := strings.Index(s, sep)
		s = s[idx+len(sep):]
		i += idx + len(sep)
		c++
		if c > count {
			return str[:i-len(sep)]
		}
	}
}

func isNotExist(err error) bool {
	err = errors.Cause(err)
	if os.IsNotExist(err) {
		return true
	}
	if pe, ok := err.(*os.PathError); ok {
		err = pe.Err
	}
	return err == syscall.ENOTDIR
}

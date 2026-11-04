//go:build !windows

package snapshotter

import "github.com/moby/sys/mountinfo"

// isMounted parses /proc/mountinfo to check whether the specified path
// is mounted.
func isMounted(path string) bool {
	m, _ := mountinfo.Mounted(path)
	return m
}

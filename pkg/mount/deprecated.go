package mount // import "github.com/docker/docker/pkg/mount"

// Deprecated: this package is not maintained and will be removed.
// Use github.com/moby/sys/mount and github.com/moby/sys/mountinfo instead.

import (
	"github.com/moby/sys/mountinfo"
)

type (
	// FilterFunc is a type.
	// Deprecated: use github.com/moby/sys/mountinfo instead.
	FilterFunc = func(*Info) (skip, stop bool)

	// Info is a type
	// Deprecated: use github.com/moby/sys/mountinfo instead.
	Info struct {
		ID, Parent, Major, Minor                                  int
		Root, Mountpoint, Opts, Optional, Fstype, Source, VfsOpts string
	}
)

// Deprecated: use github.com/moby/sys/mountinfo instead.
var (
	Mounted           = mountinfo.Mounted
	PrefixFilter      = mountinfo.PrefixFilter
	SingleEntryFilter = mountinfo.SingleEntryFilter
	ParentsFilter     = mountinfo.ParentsFilter
	FstypeFilter      = mountinfo.FSTypeFilter
)

// GetMounts is a function.
//
// Deprecated: use github.com/moby/sys/mountinfo.GetMounts() instead.
func GetMounts(f FilterFunc) ([]*Info, error) {
	fi := func(i *mountinfo.Info) (skip, stop bool) {
		return f(toLegacyInfo(i))
	}

	mounts, err := mountinfo.GetMounts(fi)
	if err != nil {
		return nil, err
	}
	mi := make([]*Info, len(mounts))
	for i, m := range mounts {
		mi[i] = toLegacyInfo(m)
	}
	return mi, nil
}

func toLegacyInfo(m *mountinfo.Info) *Info {
	return &Info{
		ID:         m.ID,
		Parent:     m.Parent,
		Major:      m.Major,
		Minor:      m.Minor,
		Root:       m.Root,
		Mountpoint: m.Mountpoint,
		Opts:       m.Options,
		Fstype:     m.FSType,
		Source:     m.Source,
		VfsOpts:    m.VFSOptions,
	}
}

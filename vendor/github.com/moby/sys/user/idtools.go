package user

import (
	"fmt"
	"os"
)

// MkdirOpt is a type for options to pass to Mkdir calls
type MkdirOpt func(*mkdirOptions)

type mkdirOptions struct {
	onlyNew bool
}

// WithOnlyNew configures [MkdirAllAndChown] and [MkdirAndChown] to leave
// the requested directory unchanged if it already exists. Newly created
// directories are still assigned the requested ownership and permissions.
func WithOnlyNew(o *mkdirOptions) {
	o.onlyNew = true
}

// MkdirAllAndChown creates a directory named path, along with any necessary
// parents, and applies the requested ownership and permission bits to all
// directories it creates.
//
// If path already exists as a directory, its ownership and permission bits
// are updated unless [WithOnlyNew] is specified. Existing parent directories
// are not modified.
//
// MkdirAllAndChown is similar to [os.MkdirAll], but also applies ownership and
// reapplies the requested permission bits after creation, so the resulting
// permissions are not affected by the process umask.
func MkdirAllAndChown(path string, mode os.FileMode, uid, gid int, opts ...MkdirOpt) error {
	return mkdirAs(path, mode, uid, gid, true, opts...)
}

// MkdirAndChown creates a directory named path and applies the requested
// ownership and permission bits.
//
// If path already exists as a directory, its ownership and permission bits
// are updated unless [WithOnlyNew] is specified.
//
// MkdirAndChown is similar to [os.Mkdir], but returns nil if path already
// exists as a directory. The resulting permission bits are not affected by
// the process umask.
func MkdirAndChown(path string, mode os.FileMode, uid, gid int, opts ...MkdirOpt) error {
	return mkdirAs(path, mode, uid, gid, false, opts...)
}

// getRootUIDGID retrieves the remapped root uid/gid pair from the set of maps.
// If the maps are empty, then the root uid/gid will default to "real" 0/0
func getRootUIDGID(uidMap, gidMap []IDMap) (int, int, error) {
	uid, err := toHost(0, uidMap)
	if err != nil {
		return -1, -1, err
	}
	gid, err := toHost(0, gidMap)
	if err != nil {
		return -1, -1, err
	}
	return uid, gid, nil
}

// toContainer takes an id mapping, and uses it to translate a
// host ID to the remapped ID. If no map is provided, then the translation
// assumes a 1-to-1 mapping and returns the passed in id
func toContainer(hostID int, idMap []IDMap) (int, error) {
	if idMap == nil {
		return hostID, nil
	}
	for _, m := range idMap {
		if (int64(hostID) >= m.ParentID) && (int64(hostID) <= (m.ParentID + m.Count - 1)) {
			contID := int(m.ID + (int64(hostID) - m.ParentID))
			return contID, nil
		}
	}
	return -1, fmt.Errorf("host ID %d cannot be mapped to a container ID", hostID)
}

// toHost takes an id mapping and a remapped ID, and translates the
// ID to the mapped host ID. If no map is provided, then the translation
// assumes a 1-to-1 mapping and returns the passed in id #
func toHost(contID int, idMap []IDMap) (int, error) {
	if idMap == nil {
		return contID, nil
	}
	for _, m := range idMap {
		if (int64(contID) >= m.ID) && (int64(contID) <= (m.ID + m.Count - 1)) {
			hostID := int(m.ParentID + (int64(contID) - m.ID))
			return hostID, nil
		}
	}
	return -1, fmt.Errorf("container ID %d cannot be mapped to a host ID", contID)
}

// IdentityMapping contains a mappings of UIDs and GIDs.
// The zero value represents an empty mapping.
type IdentityMapping struct {
	UIDMaps []IDMap `json:"UIDMaps"`
	GIDMaps []IDMap `json:"GIDMaps"`
}

// RootPair returns a uid and gid pair for the root user. The error is ignored
// because a root user always exists, and the defaults are correct when the uid
// and gid maps are empty.
func (i IdentityMapping) RootPair() (int, int) {
	uid, gid, _ := getRootUIDGID(i.UIDMaps, i.GIDMaps)
	return uid, gid
}

// ToHost returns the host UID and GID for the container uid and gid.
// An empty mapping is treated as identity.
func (i IdentityMapping) ToHost(uid, gid int) (int, int, error) {
	ruid, err := toHost(uid, i.UIDMaps)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := toHost(gid, i.GIDMaps)
	return ruid, rgid, err
}

// ToContainer returns the container UID and GID for the host uid and gid
func (i IdentityMapping) ToContainer(uid, gid int) (int, int, error) {
	ruid, err := toContainer(uid, i.UIDMaps)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := toContainer(gid, i.GIDMaps)
	return ruid, rgid, err
}

// Empty returns true if there are no id mappings
func (i IdentityMapping) Empty() bool {
	return len(i.UIDMaps) == 0 && len(i.GIDMaps) == 0
}

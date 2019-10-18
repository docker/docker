// +build darwin freebsd solaris linux

package images

import (
	"os/user"
	"strconv"

	_ "github.com/containerd/containerd/snapshots/native"
	_ "github.com/docker/docker/daemon/graphdriver/vfs"
	"github.com/docker/docker/pkg/idtools"
)

var testgraphdriver = "vfs"

func getIDMapping() (*idtools.IdentityMapping, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		return &idtools.IdentityMapping{}, nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, err
	}

	uidM := idtools.IDMap{
		ContainerID: 0,
		HostID:      uid,
		Size:        1,
	}
	gidM := idtools.IDMap{
		ContainerID: 0,
		HostID:      gid,
		Size:        1,
	}

	return idtools.NewIDMappingsFromMaps([]idtools.IDMap{uidM}, []idtools.IDMap{gidM}), nil
}

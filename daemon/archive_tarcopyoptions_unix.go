// +build !windows

package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/docker/container"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/idtools"
)

func (daemon *Daemon) tarCopyOptions(container *container.Container, noOverwriteDirNonDir bool) (*archive.TarOptions, error) {
	if container.Config.User == "" {
		return daemon.defaultTarCopyOptions(noOverwriteDirNonDir), nil
	}

	uid, gid, err := getUIDGID(container.Config.User)
	if err != nil {
		return nil, err
	}

	return &archive.TarOptions{
		NoOverwriteDirNonDir: noOverwriteDirNonDir,
		ChownOpts:            &idtools.Identity{UID: uid, GID: gid},
	}, nil
}

// getUIDGID resolves the UID and GID of a given user and, optionally, group.
//
// usergrp is a username or uid, and optional group, in the format `user[:group]`.
// Both `user` and `group` can be provided as an `uid` / `gid`, so the following
// formats are supported:
//
//   username            - valid username from getent(1)
//   username:groupname  - valid username; valid groupname from getent(1)
//   uid                 - 32-bit unsigned int valid Linux UID value
//   uid:gid             - uid value; 32-bit unsigned int Linux GID value
//   username:gid        - valid username from getent(1), gid value; 32-bit unsigned int Linux GID value
//   uid:groupname       - 32-bit unsigned int valid Linux UID value, valid groupname from getent(1)
//
//  If only a username (or uid) is provided, an attempt is made to lookup the gid
//  for that username using getent(1)
func getUIDGID(usergrp string) (int, int, error) {
	idParts := strings.Split(usergrp, ":")
	if len(idParts) > 2 {
		return 0, 0, fmt.Errorf("invalid user/group specification: %q", usergrp)
	}

	var userID, groupID int

	if uid, err := strconv.ParseUint(idParts[0], 10, 32); err == nil {
		// uid provided
		userID = int(uid)

		if len(idParts) == 1 {
			// no group provided, attempt to look up the gid from the given user
			if usr, err := idtools.LookupUID(int(uid)); err == nil {
				groupID = usr.Gid
			}
		}
	} else {
		usr, err := idtools.LookupUser(idParts[0])
		if err != nil {
			return 0, 0, err
		}
		if len(idParts) == 1 {
			// no group provided, attempt to look up the gid from the given user
			groupID = usr.Gid
		}
	}

	if len(idParts) == 2 {
		if gid, err := strconv.ParseUint(idParts[1], 10, 32); err == nil {
			// gid provided
			groupID = int(gid)
		} else {
			grp, err := idtools.LookupGroup(idParts[1])
			if err != nil {
				return 0, 0, err
			}
			groupID = grp.Gid
		}
	}

	return userID, groupID, nil
}

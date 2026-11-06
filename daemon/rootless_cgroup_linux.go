package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	cdcgroups "github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup2"
	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/pkg/sysinfo"
	ocicgroups "github.com/opencontainers/cgroups"
	"github.com/pkg/errors"
)

const defaultCgroup2Root = "/sys/fs/cgroup"

type rootlessSystemdCgroupInfo struct {
	groupPath   string
	controllers []string
}

type rootlessSystemdCgroupFiles struct {
	userManagerControlGroup func() (string, error)
	cgroup2Root             string
	readCgroupFile          func(dir, file string) (string, error)
}

var (
	rootlessSystemdCgroupMode         = cdcgroups.Mode
	withCgroup2ControllersSysInfo     = sysinfo.WithCgroup2Controllers
	discoverRootlessSystemdCgroupInfo = func() (rootlessSystemdCgroupInfo, error) {
		return rootlessSystemdCgroupFiles{
			userManagerControlGroup: systemdUserManagerControlGroup,
			cgroup2Root:             defaultCgroup2Root,
			readCgroupFile:          ocicgroups.ReadFile,
		}.discover()
	}
)

func systemdUserManagerControlGroup() (string, error) {
	ctx := context.Background()
	conn, err := systemdDbus.NewUserConnectionContext(ctx)
	if err != nil {
		return "", errors.Wrap(err, "connecting to systemd user manager")
	}
	defer conn.Close()

	property, err := conn.GetManagerProperty("ControlGroup")
	if err != nil {
		return "", errors.Wrap(err, "getting systemd user manager control group")
	}
	groupPath, err := strconv.Unquote(property)
	if err != nil {
		return "", errors.Wrapf(err, "decoding systemd user manager ControlGroup property %q", property)
	}
	return groupPath, nil
}

func rootlessSystemdSysInfoOptions(cfg *config.Config) ([]sysinfo.Opt, error) {
	if !cfg.Rootless || cgroupDriver(cfg) != cgroupSystemdDriver {
		return nil, nil
	}

	info, err := rootlessSystemdCgroupControllerInfo()
	if err != nil {
		// Passing no option would make sysinfo.New inspect the cgroup mount root,
		// which can expose controllers that are unavailable to a rootless daemon.
		// Pass an explicit empty set so capability detection remains conservative.
		return []sysinfo.Opt{withCgroup2ControllersSysInfo("/", nil)}, errors.Wrap(err, "detecting rootless systemd cgroup controllers")
	}
	return []sysinfo.Opt{withCgroup2ControllersSysInfo(info.groupPath, info.controllers)}, nil
}

// rootlessSystemdCgroupControllerInfo returns the cgroup and controllers that
// systemd made available to the user manager. runc asks that manager to create
// rootless container scopes, so its ControlGroup is the authoritative root for
// both the hierarchy prefix and the controllers available to those scopes.
//
// Querying systemd also works when dockerd was not launched as a user service,
// and preserves any outer cgroup prefix without assuming a user-slice layout.
func rootlessSystemdCgroupControllerInfo() (rootlessSystemdCgroupInfo, error) {
	if rootlessSystemdCgroupMode() != cdcgroups.Unified {
		return rootlessSystemdCgroupInfo{}, errors.New("rootless systemd driver doesn't support cgroup v1")
	}

	rootlesskitParentEUID := os.Getenv("ROOTLESSKIT_PARENT_EUID")
	if rootlesskitParentEUID == "" {
		return rootlessSystemdCgroupInfo{}, errors.New("$ROOTLESSKIT_PARENT_EUID is not set (requires RootlessKit v0.8.0)")
	}
	if _, err := strconv.ParseUint(rootlesskitParentEUID, 10, 32); err != nil {
		return rootlessSystemdCgroupInfo{}, errors.Wrap(err, "invalid $ROOTLESSKIT_PARENT_EUID: must be a numeric value")
	}

	return discoverRootlessSystemdCgroupInfo()
}

func (f rootlessSystemdCgroupFiles) discover() (rootlessSystemdCgroupInfo, error) {
	if f.userManagerControlGroup == nil {
		return rootlessSystemdCgroupInfo{}, errors.New("systemd user manager control-group lookup is not configured")
	}
	groupPath, err := f.userManagerControlGroup()
	if err != nil {
		return rootlessSystemdCgroupInfo{}, errors.Wrap(err, "resolving systemd user manager cgroup")
	}
	if groupPath == "" {
		return rootlessSystemdCgroupInfo{}, errors.New("systemd user manager returned an empty control-group path")
	}
	if strings.HasSuffix(groupPath, " (deleted)") {
		return rootlessSystemdCgroupInfo{}, fmt.Errorf("systemd user manager cgroup %q has been deleted", groupPath)
	}
	if err := cgroup2.VerifyGroupPath(groupPath); err != nil {
		return rootlessSystemdCgroupInfo{}, errors.Wrapf(err, "invalid systemd user manager cgroup path %q", groupPath)
	}

	if f.readCgroupFile == nil {
		return rootlessSystemdCgroupInfo{}, errors.New("rootless systemd cgroup file reader is not configured")
	}

	groupDir := filepath.Join(f.cgroup2Root, strings.TrimPrefix(groupPath, "/"))
	controllersFile, err := f.readCgroupFile(groupDir, "cgroup.controllers")
	if err != nil {
		return rootlessSystemdCgroupInfo{}, errors.Wrapf(err, "reading controllers for rootless systemd cgroup %q at %s", groupPath, filepath.Join(groupDir, "cgroup.controllers"))
	}

	return rootlessSystemdCgroupInfo{
		groupPath:   groupPath,
		controllers: strings.Fields(controllersFile),
	}, nil
}

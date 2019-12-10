// +build linux freebsd

// Package local provides the default implementation for volumes. It
// is used to mount data volume containers and directories local to
// the host server.
package local // import "github.com/docker/docker/volume/local"

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/mount"
	"github.com/pkg/errors"
)

var (
	oldVfsDir = filepath.Join("vfs", "dir")

	validOpts = map[string]struct{}{
		"type":   {}, // specify the filesystem type for mount, e.g. nfs
		"o":      {}, // generic mount options
		"device": {}, // device to mount from
	}
	mandatoryOpts = map[string]struct{}{
		"device": {},
		"type":   {},
	}
)

type optsConfig struct {
	MountType   string
	MountOpts   string
	MountDevice string
}

func (o *optsConfig) String() string {
	return fmt.Sprintf("type='%s' device='%s' o='%s'", o.MountType, o.MountDevice, o.MountOpts)
}

// scopedPath verifies that the path where the volume is located
// is under Docker's root and the valid local paths.
func (r *Root) scopedPath(realPath string) bool {
	// Volumes path for Docker version >= 1.7
	if strings.HasPrefix(realPath, filepath.Join(r.scope, volumesPathName)) && realPath != filepath.Join(r.scope, volumesPathName) {
		return true
	}

	// Volumes path for Docker version < 1.7
	if strings.HasPrefix(realPath, filepath.Join(r.scope, oldVfsDir)) {
		return true
	}

	return false
}

func setOpts(v *localVolume, opts map[string]string) error {
	if len(opts) == 0 {
		return nil
	}
	if err := validateOpts(opts); err != nil {
		return err
	}

	v.opts = &optsConfig{
		MountType:   opts["type"],
		MountOpts:   opts["o"],
		MountDevice: opts["device"],
	}
	return nil
}

func validateOpts(opts map[string]string) error {
	if len(opts) == 0 {
		return nil
	}
	for opt := range opts {
		if _, ok := validOpts[opt]; !ok {
			return errdefs.InvalidParameter(errors.Errorf("invalid option: %q", opt))
		}
	}
	for opt := range mandatoryOpts {
		if _, ok := opts[opt]; !ok {
			return errdefs.InvalidParameter(errors.Errorf("missing required option: %q", opt))
		}
	}
	return nil
}

func (v *localVolume) mount() error {
	if v.opts.MountDevice == "" {
		return fmt.Errorf("missing device in volume options")
	}
	mountOpts := v.opts.MountOpts
	switch v.opts.MountType {
	case "nfs", "cifs":
		if addrValue := getAddress(v.opts.MountOpts); addrValue != "" && net.ParseIP(addrValue).To4() == nil {
			ipAddr, err := net.ResolveIPAddr("ip", addrValue)
			if err != nil {
				return errors.Wrapf(err, "error resolving passed in network volume address")
			}
			mountOpts = strings.Replace(mountOpts, "addr="+addrValue, "addr="+ipAddr.String(), 1)
		}
	}
	err := mount.Mount(v.opts.MountDevice, v.path, v.opts.MountType, mountOpts)
	return errors.Wrap(err, "failed to mount local volume")
}

func (v *localVolume) CreatedAt() (time.Time, error) {
	fileInfo, err := os.Stat(filepath.Dir(v.path))
	if err != nil {
		return time.Time{}, err
	}
	sec, nsec := fileInfo.Sys().(*syscall.Stat_t).Ctim.Unix()
	return time.Unix(sec, nsec), nil
}

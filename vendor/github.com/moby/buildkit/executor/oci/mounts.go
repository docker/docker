package oci

import (
	"context"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

// MountOpts sets oci spec specific info for mount points
type MountOpts func([]specs.Mount) ([]specs.Mount, error)

//GetMounts returns default required for buildkit
// https://github.com/moby/buildkit/issues/429
func GetMounts(ctx context.Context, mountOpts ...MountOpts) ([]specs.Mount, error) {
	mounts := []specs.Mount{
		{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
		},
		{
			Destination: "/dev",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/dev/pts",
			Type:        "devpts",
			Source:      "devpts",
			Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
		},
		{
			Destination: "/dev/shm",
			Type:        "tmpfs",
			Source:      "shm",
			Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
		},
		{
			Destination: "/dev/mqueue",
			Type:        "mqueue",
			Source:      "mqueue",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "ro"},
		},
	}
	var err error
	for _, o := range mountOpts {
		mounts, err = o(mounts)
		if err != nil {
			return nil, err
		}
	}
	return mounts, nil
}

func withROBind(src, dest string) func(m []specs.Mount) ([]specs.Mount, error) {
	return func(m []specs.Mount) ([]specs.Mount, error) {
		m = append(m, specs.Mount{
			Destination: dest,
			Type:        "bind",
			Source:      src,
			Options:     []string{"rbind", "ro"},
		})
		return m, nil
	}
}

func hasPrefix(p, prefixDir string) bool {
	prefixDir = filepath.Clean(prefixDir)
	if prefixDir == "/" {
		return true
	}
	p = filepath.Clean(p)
	return p == prefixDir || strings.HasPrefix(p, prefixDir+"/")
}

func removeMountsWithPrefix(mounts []specs.Mount, prefixDir string) []specs.Mount {
	var ret []specs.Mount
	for _, m := range mounts {
		if !hasPrefix(m.Destination, prefixDir) {
			ret = append(ret, m)
		}
	}
	return ret
}

func withProcessMode(processMode ProcessMode) func([]specs.Mount) ([]specs.Mount, error) {
	return func(m []specs.Mount) ([]specs.Mount, error) {
		switch processMode {
		case ProcessSandbox:
			// keep the default
		case NoProcessSandbox:
			m = removeMountsWithPrefix(m, "/proc")
			procMount := specs.Mount{
				Destination: "/proc",
				Type:        "bind",
				Source:      "/proc",
				// NOTE: "rbind"+"ro" does not make /proc read-only recursively.
				// So we keep maskedPath and readonlyPaths (although not mandatory for rootless mode)
				Options: []string{"rbind"},
			}
			m = append([]specs.Mount{procMount}, m...)
		default:
			return nil, errors.Errorf("unknown process mode: %v", processMode)
		}
		return m, nil
	}
}

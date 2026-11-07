package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/archutil"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/sys/user"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	copy "github.com/tonistiigi/fsutil/copy"
)

const qemuMountName = "/dev/.buildkit_qemu_emulator"

var qemuArchMap = map[string]string{
	"arm64":   "aarch64",
	"amd64":   "x86_64",
	"riscv64": "riscv64",
	"arm":     "arm",
	"s390x":   "s390x",
	"ppc64le": "ppc64le",
	"386":     "i386",
}

type emulator struct {
	path  string
	idmap *user.IdentityMapping
}

func (e *emulator) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &staticEmulatorMount{path: e.path, idmap: e.idmap}, nil
}

type staticEmulatorMount struct {
	path  string
	idmap *user.IdentityMapping
}

func (m *staticEmulatorMount) Mount() ([]mount.Mount, func() error, error) {
	tmpdir, err := os.MkdirTemp("", "buildkit-qemu-emulator")
	if err != nil {
		return nil, nil, err
	}
	var ret bool
	defer func() {
		if !ret {
			os.RemoveAll(tmpdir)
		}
	}()

	var uid, gid int
	if m.idmap != nil {
		uid, gid = m.idmap.RootPair()
	}
	if err := copy.Copy(context.TODO(), filepath.Dir(m.path), filepath.Base(m.path), tmpdir, qemuMountName, func(ci *copy.CopyInfo) {
		m := 0555
		ci.Mode = &m
	}, copy.WithChown(uid, gid), copy.WithXAttrErrorHandler(ignoreSELinuxXAttrErrorHandler)); err != nil {
		return nil, nil, err
	}

	ret = true
	return []mount.Mount{{
		Type:    "bind",
		Source:  filepath.Join(tmpdir, qemuMountName),
		Options: []string{"ro", "bind"},
	}}, func() error {
		return os.RemoveAll(tmpdir)
	}, nil
}

func (m *staticEmulatorMount) IdentityMapping() *user.IdentityMapping {
	return m.idmap
}

func getEmulator(ctx context.Context, p *pb.Platform) (*emulator, error) {
	all := archutil.SupportedPlatforms(false)
	pp := platforms.Normalize(ocispecs.Platform{
		Architecture: p.Architecture,
		OS:           p.OS,
		OSVersion:    p.OSVersion,
		OSFeatures:   p.OSFeatures,
		Variant:      p.Variant,
	})

	for _, p := range all {
		if platforms.Only(p).Match(pp) {
			return nil, nil
		}
	}

	if pp.Architecture == "amd64" {
		if pp.Variant != "" && pp.Variant != "v2" {
			var supported []string
			for _, p := range all {
				if p.Architecture == "amd64" {
					supported = append(supported, platforms.Format(p))
				}
			}
			return nil, errors.Errorf("no support for running processes with %s platform, supported: %s", platforms.Format(pp), strings.Join(supported, ", "))
		}
	}

	a, ok := qemuArchMap[pp.Architecture]
	if !ok {
		a = pp.Architecture
	}

	fn, err := exec.LookPath("buildkit-qemu-" + a)
	if err != nil {
		bklog.G(ctx).Warn(err.Error()) // TODO: remove this with pull support
		return nil, nil                // no emulator available
	}

	return &emulator{path: fn}, nil
}

// ignoreSELinuxXAttrErrorHandler is an error handler for xattr copy operations
// that specifically ignores ENOTSUP errors for security.selinux extended attributes.
// This addresses SELinux compatibility issues where copying files to filesystems that
// don't support SELinux xattrs (like tmpfs) would fail with ENOTSUP, preventing
// qemu emulator setup on SELinux-enabled systems. Since the security.selinux xattr
// is not critical for the emulator functionality, we safely ignore these errors
// while preserving other xattr error handling.
func ignoreSELinuxXAttrErrorHandler(dst, src, xattrKey string, err error) error {
	// Ignore ENOTSUP errors specifically for security.selinux xattr
	// This allows qemu emulator setup to succeed on SELinux systems
	// when copying to filesystems that don't support SELinux xattrs
	if errors.Is(err, syscall.ENOTSUP) && xattrKey == "security.selinux" {
		return nil
	}
	return err
}

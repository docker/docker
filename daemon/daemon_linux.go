package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/internal/rootless"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/resolvconf"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// On Linux, plugins use a static path for storing execution state,
// instead of deriving path from daemon's exec-root. This is because
// plugin socket files are created here and they cannot exceed max
// path length of 108 bytes.
func getPluginExecRoot(_ *config.Config) string {
	return "/run/docker/plugins"
}

func (daemon *Daemon) cleanupMountsByID(mountID string) error {
	logger := log.G(context.TODO()).WithField("mountid", mountID)
	logger.Debug("cleaning up old mount start")
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()

	if err := daemon.cleanupMountsFromReaderByID(f, mountID, mount.Unmount); err != nil {
		return err
	}
	logger.Debug("cleaning up old mount done")
	return nil
}

func (daemon *Daemon) cleanupMountsFromReaderByID(reader io.Reader, mountID string, unmount func(target string) error) error {
	if daemon.root == "" {
		return nil
	}

	root := filepath.Clean(daemon.root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("cannot clean up mounts with non-absolute daemon root %q", daemon.root)
	}
	if root == "/" {
		return errors.New("cannot clean up mounts with daemon root set to /")
	}

	regexps := getCleanPatterns(mountID)
	mounts, err := mountinfo.GetMountsFromReader(reader, func(m *mountinfo.Info) (skip, stop bool) {
		// Mountinfo paths have no trailing slash. Append one to both paths to avoid
		// matching sibling paths that merely share the daemon-root prefix.
		if !strings.HasPrefix(m.Mountpoint+"/", root+"/") {
			return true, false
		}
		for _, p := range regexps {
			if p.MatchString(m.Mountpoint) {
				return false, false
			}
		}
		return true, false
	})
	if err != nil {
		return err
	}

	logger := log.G(context.TODO())
	if mountID != "" {
		logger = logger.WithField("mountid", mountID)
	}

	var errs []error
	for _, mnt := range mounts {
		if err := unmount(mnt.Mountpoint); err != nil {
			logger.WithError(err).Error("error unmounting mountpoint")
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("error cleaning up mounts:\n%w", err)
	}

	return nil
}

// cleanupMounts umounts used by container resources and the daemon root mount
func (daemon *Daemon) cleanupMounts(cfg *config.Config) error {
	if err := daemon.cleanupMountsByID(""); err != nil {
		return err
	}

	if daemon.root == "" {
		return nil
	}
	root := filepath.Clean(daemon.root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("cannot clean up mounts with non-absolute daemon root %q", daemon.root)
	}
	if root == "/" {
		return errors.New("cannot clean up mounts with daemon root set to /")
	}
	info, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(root))
	if err != nil {
		return fmt.Errorf("error reading mount table for cleanup: %w", err)
	}

	if len(info) == 0 {
		// No mount found; nothing to clean up.
		return nil
	}

	// `info.Root` here is the root mountpoint of the passed in path (`daemon.root`).
	// The only cases that need to be cleaned up is when the daemon has performed a
	//   `mount --bind /daemon/root /daemon/root && mount --make-shared /daemon/root`
	// This is only done when the daemon is started up and `/daemon/root` is not
	// already on a shared mountpoint.
	if !shouldUnmountRoot(root, info[0]) {
		return nil
	}

	unmountFile := getUnmountOnShutdownPath(cfg)
	if _, err := os.Stat(unmountFile); err != nil {
		return nil
	}

	log.G(context.TODO()).WithField("mountpoint", root).Debug("unmounting daemon root")
	if err := mount.Unmount(root); err != nil {
		return err
	}
	return os.Remove(unmountFile)
}

func getCleanPatterns(id string) []*regexp.Regexp {
	var patterns []string
	if id == "" {
		id = "[0-9a-f]{64}"
		patterns = append(patterns, "containers/"+id+"/mounts/shm", "containers/"+id+"/shm")
	}
	patterns = append(patterns, "overlay2/"+id+"/merged$", "zfs/graph/"+id+"$")

	var regexps []*regexp.Regexp
	for _, p := range patterns {
		r, err := regexp.Compile(p)
		if err == nil {
			regexps = append(regexps, r)
		}
	}
	return regexps
}

func shouldUnmountRoot(root string, info *mountinfo.Info) bool {
	if !strings.HasSuffix(root, info.Root) {
		return false
	}
	return hasMountInfoOption(info.Optional, sharedPropagationOption)
}

// setupResolvConf sets the appropriate resolv.conf file if not specified
// When systemd-resolved is running the default /etc/resolv.conf points to
// localhost. In this case fetch the alternative config file that is in a
// different path so that containers can use it
// In all the other cases fallback to the default one
func setupResolvConf(config *config.Config) {
	if config.ResolvConf != "" {
		return
	}
	// FIXME(thaJeztah): we can't use [github.com/moby/moby/v2/daemon/libnetwork/internal/resolvconf.Path] here, because it's internal to libnetwork.
	config.ResolvConf = resolvconf.Path()
}

// ifaceAddrs returns the addresses from family assigned to the network
// interface with name linkName.
//
// No error is returned if the named interface does not exist.
func ifaceAddrs(linkName string, family int) ([]netlink.Addr, error) {
	nl := ns.NlHandle()
	link, err := nl.LinkByName(linkName)
	if err != nil {
		if !errors.As(err, new(netlink.LinkNotFoundError)) {
			return nil, err
		}
		return nil, nil
	}
	return nl.AddrList(link, family)
}

var kernelSupportsRecursivelyReadOnly = sync.OnceValue(func() error {
	tmpMnt, err := os.MkdirTemp("", "moby-detect-rro")
	if err != nil {
		return fmt.Errorf("failed to create a temp directory: %w", err)
	}
	defer func() { _ = os.Remove(tmpMnt) }()

	for {
		err = unix.Mount("", tmpMnt, "tmpfs", 0, "")
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("failed to mount tmpfs on %q: %w", tmpMnt, err)
	}
	defer func() {
		var umErr error
		for {
			umErr = unix.Unmount(tmpMnt, 0)
			if !errors.Is(umErr, unix.EINTR) {
				break
			}
		}
		if umErr != nil {
			log.G(context.TODO()).WithError(umErr).Warnf("Failed to unmount %q", tmpMnt)
		}
	}()
	attr := &unix.MountAttr{
		Attr_set: unix.MOUNT_ATTR_RDONLY,
	}
	for {
		err = unix.MountSetattr(-1, tmpMnt, unix.AT_RECURSIVE, attr)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	// ENOSYS on kernel < 5.12
	if err != nil {
		return fmt.Errorf("failed to call mount_setattr: %w", err)
	}
	return nil
})

func supportsRecursivelyReadOnly(cfg *configStore, runtime string) error {
	if err := kernelSupportsRecursivelyReadOnly(); err != nil {
		return fmt.Errorf("rro is not supported: %w (kernel is older than 5.12?)", err)
	}
	if runtime == "" {
		runtime = cfg.Runtimes.Default
	}
	features := cfg.Runtimes.Features(runtime)
	if features == nil {
		return fmt.Errorf("rro is not supported by runtime %q: OCI features struct is not available", runtime)
	}
	if slices.Contains(features.MountOptions, "rro") {
		return nil
	}
	return fmt.Errorf("rro is not supported by runtime %q", runtime)
}

func (daemon *Daemon) runInNetNS(f func() error) error {
	if rootless.RunningWithRootlessKit() {
		if detachedNetNS, err := rootless.DetachedNetNS(); err != nil {
			return err
		} else if detachedNetNS != "" {
			return rootless.RunInNetNS(detachedNetNS, f)
		}
	}
	return f()
}

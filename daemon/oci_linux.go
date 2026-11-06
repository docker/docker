package daemon

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	coci "github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	dconfig "github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/rootless/mountopts"
	"github.com/moby/moby/v2/daemon/internal/rootless/specconv"
	"github.com/moby/moby/v2/daemon/pkg/oci"
	"github.com/moby/moby/v2/daemon/pkg/oci/caps"
	volumemounts "github.com/moby/moby/v2/daemon/volume/mounts"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/moby/sys/user"
	"github.com/moby/sys/userns"
	"github.com/opencontainers/cgroups"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

const inContainerInitPath = "/sbin/" + dconfig.DefaultInitBinary

// withRlimits sets the container's rlimits along with merging the daemon's rlimits
func withRlimits(daemon *Daemon, daemonCfg *dconfig.Config, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		var rlimits []specs.POSIXRlimit

		// We want to leave the original HostConfig alone so make a copy here
		hostConfig := *c.HostConfig
		// Merge with the daemon defaults
		daemon.mergeUlimits(&hostConfig, daemonCfg)
		for _, ul := range hostConfig.Ulimits {
			rlimits = append(rlimits, specs.POSIXRlimit{
				Type: "RLIMIT_" + strings.ToUpper(ul.Name),
				Soft: uint64(ul.Soft),
				Hard: uint64(ul.Hard),
			})
		}

		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.Rlimits = rlimits
		return nil
	}
}

// withRootless sets the spec to the rootless configuration
func withRootless(_ *Daemon, daemonCfg *dconfig.Config) coci.SpecOpts {
	return func(_ context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		var v2Controllers []string
		if cgroupDriver(daemonCfg) == cgroupSystemdDriver {
			info, err := rootlessSystemdCgroupControllerInfo()
			if err != nil {
				return err
			}
			v2Controllers = info.controllers
		}
		return specconv.ToRootless(s, v2Controllers)
	}
}

// withRootfulInRootless is used for "rootful-in-rootless" dind;
// the daemon is running in UserNS but has no access to RootlessKit API socket, host filesystem, etc.
func withRootfulInRootless(_ *Daemon, _ *dconfig.Config) coci.SpecOpts {
	return func(_ context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		specconv.ToRootfulInRootless(s)
		return nil
	}
}

// WithOOMScore sets the oom score
func WithOOMScore(score *int) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.OOMScoreAdj = score
		return nil
	}
}

// WithSelinux sets the selinux labels
func WithSelinux(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Process.SelinuxLabel = c.GetProcessLabel()
		s.Linux.MountLabel = c.MountLabel
		return nil
	}
}

// WithApparmor sets the apparmor profile
func WithApparmor(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if appArmorSupported() {
			var appArmorProfile string
			if c.AppArmorProfile != "" {
				appArmorProfile = c.AppArmorProfile
			} else if c.HostConfig.Privileged {
				appArmorProfile = unconfinedAppArmorProfile
			} else {
				appArmorProfile = defaultAppArmorProfile
			}

			if appArmorProfile == defaultAppArmorProfile {
				// Unattended upgrades and other fun services can unload AppArmor
				// profiles inadvertently. Since we cannot store our profile in
				// /etc/apparmor.d, nor can we practically add other ways of
				// telling the system to keep our profile loaded, in order to make
				// sure that we keep the default profile enabled we load it again
				// if it is missing.
				if err := daemon.loadDefaultAppArmorProfileIfMissing(); err != nil {
					return err
				}
			}
			if s.Process == nil {
				s.Process = &specs.Process{}
			}
			s.Process.ApparmorProfile = appArmorProfile
		}
		return nil
	}
}

// WithCapabilities adjusts the container's capabilities based on the
// "CapAdd", "CapDrop", and "Privileged" fields in the container's HostConfig.
func WithCapabilities(ctr *container.Container) coci.SpecOpts {
	return func(ctx context.Context, client coci.Client, c *containers.Container, s *specs.Spec) (err error) {
		capabilities, err := caps.TweakCapabilities(
			caps.DefaultCapabilities(),
			ctr.HostConfig.CapAdd,
			ctr.HostConfig.CapDrop,
			ctr.HostConfig.Privileged,
		)
		if err != nil {
			return err
		}
		return coci.WithCapabilities(capabilities)(ctx, client, c, s)
	}
}

func resourcePath(c *container.Container, getPath func() (string, error)) (string, error) {
	p, err := getPath()
	if err != nil {
		return "", err
	}
	return c.GetResourcePath(p)
}

func getUser(c *container.Container, username string) (specs.User, error) {
	var usr specs.User
	passwdPath, err := resourcePath(c, user.GetPasswdPath)
	if err != nil {
		return usr, err
	}
	groupPath, err := resourcePath(c, user.GetGroupPath)
	if err != nil {
		return usr, err
	}
	execUser, err := user.GetExecUserPath(username, nil, passwdPath, groupPath)
	if err != nil {
		return usr, err
	}
	usr.UID = uint32(execUser.Uid)
	usr.GID = uint32(execUser.Gid)
	usr.AdditionalGids = []uint32{usr.GID}

	var addGroups []int
	if len(c.HostConfig.GroupAdd) > 0 {
		addGroups, err = user.GetAdditionalGroupsPath(c.HostConfig.GroupAdd, groupPath)
		if err != nil {
			return usr, err
		}
	}
	for _, g := range append(execUser.Sgids, addGroups...) {
		usr.AdditionalGids = append(usr.AdditionalGids, uint32(g))
	}
	return usr, nil
}

func setNamespace(s *specs.Spec, ns specs.LinuxNamespace) {
	if s.Linux == nil {
		s.Linux = &specs.Linux{}
	}

	for i, n := range s.Linux.Namespaces {
		if n.Type == ns.Type {
			s.Linux.Namespaces[i] = ns
			return
		}
	}
	s.Linux.Namespaces = append(s.Linux.Namespaces, ns)
}

// WithNamespaces sets the container's namespaces
func WithNamespaces(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		userNS := false
		// user
		if c.HostConfig.UsernsMode.IsPrivate() {
			if uidMap := daemon.idMapping.UIDMaps; uidMap != nil {
				userNS = true
				setNamespace(s, specs.LinuxNamespace{
					Type: specs.UserNamespace,
				})
				s.Linux.UIDMappings = specMapping(uidMap)
				s.Linux.GIDMappings = specMapping(daemon.idMapping.GIDMaps)
			}
		}
		// network
		if !c.Config.NetworkDisabled {
			networkMode := c.HostConfig.NetworkMode
			switch {
			case networkMode.IsContainer():
				nc, err := daemon.getNetworkedContainer(c.ID, networkMode.ConnectedContainer())
				if err != nil {
					return err
				}
				setNamespace(s, specs.LinuxNamespace{
					Type: specs.NetworkNamespace,
					Path: fmt.Sprintf("/proc/%d/ns/net", nc.State.GetPID()),
				})
				if userNS {
					// to share a net namespace, the containers must also share a user namespace.
					//
					// FIXME(thaJeztah): this will silently overwrite an earlier user namespace when joining multiple containers: https://github.com/moby/moby/issues/46210
					setNamespace(s, specs.LinuxNamespace{
						Type: specs.UserNamespace,
						Path: fmt.Sprintf("/proc/%d/ns/user", nc.State.GetPID()),
					})
				}
			case networkMode.IsHost():
				oci.RemoveNamespace(s, specs.NetworkNamespace)
			default:
				setNamespace(s, specs.LinuxNamespace{
					Type: specs.NetworkNamespace,
				})
			}
		}

		// Remove time-namespace if not supported. We can remove this once we
		// drop support for kernel < 5.6.
		sysInfo, err := daemon.RawSysInfo()
		if err != nil {
			return errdefs.System(err)
		}
		if !sysInfo.TimeNamespaces {
			oci.RemoveNamespace(s, specs.TimeNamespace)
		}

		// ipc
		ipcMode := c.HostConfig.IpcMode
		if !ipcMode.Valid() {
			return errdefs.InvalidParameter(errors.Errorf("invalid IPC mode: %v", ipcMode))
		}
		switch {
		case ipcMode.IsContainer():
			ic, err := daemon.getIPCContainer(ipcMode.Container())
			if err != nil {
				return errors.Wrap(err, "failed to join IPC namespace")
			}
			setNamespace(s, specs.LinuxNamespace{
				Type: specs.IPCNamespace,
				Path: fmt.Sprintf("/proc/%d/ns/ipc", ic.State.GetPID()),
			})
			if userNS {
				// to share a IPC namespace, the containers must also share a user namespace.
				//
				// FIXME(thaJeztah): this will silently overwrite an earlier user namespace when joining multiple containers: https://github.com/moby/moby/issues/46210
				setNamespace(s, specs.LinuxNamespace{
					Type: specs.UserNamespace,
					Path: fmt.Sprintf("/proc/%d/ns/user", ic.State.GetPID()),
				})
			}
		case ipcMode.IsHost():
			oci.RemoveNamespace(s, specs.IPCNamespace)
		case ipcMode.IsEmpty():
			// A container was created by an older version of the daemon.
			// The default behavior used to be what is now called "shareable".
			fallthrough
		case ipcMode.IsPrivate(), ipcMode.IsShareable(), ipcMode.IsNone():
			setNamespace(s, specs.LinuxNamespace{
				Type: specs.IPCNamespace,
			})
		}

		// pid
		pidMode := c.HostConfig.PidMode
		if !pidMode.Valid() {
			return errdefs.InvalidParameter(errors.Errorf("invalid PID mode: %v", pidMode))
		}
		switch {
		case pidMode.IsContainer():
			pc, err := daemon.getPIDContainer(pidMode.Container())
			if err != nil {
				return errors.Wrap(err, "failed to join PID namespace")
			}
			setNamespace(s, specs.LinuxNamespace{
				Type: specs.PIDNamespace,
				Path: fmt.Sprintf("/proc/%d/ns/pid", pc.State.GetPID()),
			})
			if userNS {
				// to share a PID namespace, the containers must also share a user namespace.
				//
				// FIXME(thaJeztah): this will silently overwrite an earlier user namespace when joining multiple containers: https://github.com/moby/moby/issues/46210
				setNamespace(s, specs.LinuxNamespace{
					Type: specs.UserNamespace,
					Path: fmt.Sprintf("/proc/%d/ns/user", pc.State.GetPID()),
				})
			}
		case pidMode.IsHost():
			oci.RemoveNamespace(s, specs.PIDNamespace)
		default:
			setNamespace(s, specs.LinuxNamespace{
				Type: specs.PIDNamespace,
			})
		}

		// uts
		if !c.HostConfig.UTSMode.Valid() {
			return errdefs.InvalidParameter(errors.Errorf("invalid UTS mode: %v", c.HostConfig.UTSMode))
		}
		if c.HostConfig.UTSMode.IsHost() {
			oci.RemoveNamespace(s, specs.UTSNamespace)
			s.Hostname = ""
		}

		// cgroup
		if !c.HostConfig.CgroupnsMode.Valid() {
			return errdefs.InvalidParameter(errors.Errorf("invalid cgroup namespace mode: %v", c.HostConfig.CgroupnsMode))
		}
		if c.HostConfig.CgroupnsMode.IsPrivate() {
			setNamespace(s, specs.LinuxNamespace{
				Type: specs.CgroupNamespace,
			})
		}

		return nil
	}
}

func specMapping(s []user.IDMap) []specs.LinuxIDMapping {
	var ids []specs.LinuxIDMapping
	for _, item := range s {
		ids = append(ids, specs.LinuxIDMapping{
			HostID:      uint32(item.ParentID),
			ContainerID: uint32(item.ID),
			Size:        uint32(item.Count),
		})
	}
	return ids
}

// Get the source mount point of directory passed in as argument. Also return
// optional fields.
func getSourceMount(source string) (string, string, error) {
	// Ensure any symlinks are resolved.
	sourcePath, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", err
	}

	mi, err := mountinfo.GetMounts(mountinfo.ParentsFilter(sourcePath))
	if err != nil {
		return "", "", err
	}
	if len(mi) < 1 {
		return "", "", fmt.Errorf("Can't find mount point of %s", source)
	}

	// find the longest mount point, preferring later entries when there are
	// multiple mounts at the same path (e.g., with different propagation settings)
	var idx, maxlen int
	for i := range mi {
		if len(mi[i].Mountpoint) >= maxlen {
			maxlen = len(mi[i].Mountpoint)
			idx = i
		}
	}
	return mi[idx].Mountpoint, mi[idx].Optional, nil
}

const (
	sharedPropagationOption = "shared:"
	slavePropagationOption  = "master:"
)

// hasMountInfoOption checks if any of the passed any of the given option values
// are set in the passed in option string.
func hasMountInfoOption(opts string, vals ...string) bool {
	for opt := range strings.SplitSeq(opts, " ") {
		for _, val := range vals {
			if strings.HasPrefix(opt, val) {
				return true
			}
		}
	}
	return false
}

// Ensure mount point on which path is mounted, is shared.
func ensureShared(path string) error {
	sourceMount, optionalOpts, err := getSourceMount(path)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	if !hasMountInfoOption(optionalOpts, sharedPropagationOption) {
		return errors.Errorf("path %s is mounted on %s but it is not a shared mount", path, sourceMount)
	}
	return nil
}

// Ensure mount point on which path is mounted, is either shared or slave.
func ensureSharedOrSlave(path string) error {
	sourceMount, optionalOpts, err := getSourceMount(path)
	if err != nil {
		return err
	}

	if !hasMountInfoOption(optionalOpts, sharedPropagationOption, slavePropagationOption) {
		return errors.Errorf("path %s is mounted on %s but it is not a shared or slave mount", path, sourceMount)
	}
	return nil
}

var (
	mountPropagationMap = map[string]int{
		"private":  mount.PRIVATE,
		"rprivate": mount.RPRIVATE,
		"shared":   mount.SHARED,
		"rshared":  mount.RSHARED,
		"slave":    mount.SLAVE,
		"rslave":   mount.RSLAVE,
	}

	mountPropagationReverseMap = map[int]string{
		mount.PRIVATE:  "private",
		mount.RPRIVATE: "rprivate",
		mount.SHARED:   "shared",
		mount.RSHARED:  "rshared",
		mount.SLAVE:    "slave",
		mount.RSLAVE:   "rslave",
	}
)

// withMounts sets the container's mounts
func withMounts(daemon *Daemon, daemonCfg *configStore, c *container.Container, mounts []container.Mount) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		container.SortMounts(mounts)

		userMounts := make(map[string]struct{})
		for _, m := range mounts {
			userMounts[m.Destination] = struct{}{}
		}

		// Copy all mounts from spec to defaultMounts, except for
		//  - mounts overridden by a user supplied mount;
		//  - all mounts under /dev if a user supplied /dev is present;
		//  - /dev/shm, in case IpcMode is none.
		// While at it, also
		//  - set size for /dev/shm from shmsize.
		defaultMounts := s.Mounts[:0]
		_, mountDev := userMounts["/dev"]
		for _, m := range s.Mounts {
			if _, ok := userMounts[m.Destination]; ok {
				// filter out mount overridden by a user supplied mount
				continue
			}
			if mountDev && strings.HasPrefix(m.Destination, "/dev/") {
				// filter out everything under /dev if /dev is user-mounted
				continue
			}

			if m.Destination == "/dev/shm" {
				if c.HostConfig.IpcMode.IsNone() {
					// filter out /dev/shm for "none" IpcMode
					continue
				}
				// set size for /dev/shm mount from spec
				sizeOpt := "size=" + strconv.FormatInt(c.HostConfig.ShmSize, 10)
				m.Options = append(m.Options, sizeOpt)
			}

			defaultMounts = append(defaultMounts, m)
		}

		s.Mounts = defaultMounts
		for _, m := range mounts {
			if m.Source == "tmpfs" {
				data := m.Data
				parser := volumemounts.NewParser()
				options := []string{"noexec", "nosuid", "nodev", string(parser.DefaultPropagationMode())}
				if data != "" {
					options = append(options, strings.Split(data, ",")...)
				}

				merged, err := mount.MergeTmpfsOptions(options)
				if err != nil {
					return err
				}

				s.Mounts = append(s.Mounts, specs.Mount{Destination: m.Destination, Source: m.Source, Type: "tmpfs", Options: merged})
				continue
			}

			mt := specs.Mount{Destination: m.Destination, Source: m.Source, Type: "bind"}

			// Determine property of RootPropagation based on volume
			// properties. If a volume is shared, then keep root propagation
			// shared. This should work for slave and private volumes too.
			//
			// For slave volumes, it can be either [r]shared/[r]slave.
			//
			// For private volumes any root propagation value should work.
			pFlag := mountPropagationMap[m.Propagation]
			switch pFlag {
			case mount.SHARED, mount.RSHARED:
				if err := ensureShared(m.Source); err != nil {
					return err
				}
				rootpg := mountPropagationMap[s.Linux.RootfsPropagation]
				if rootpg != mount.SHARED && rootpg != mount.RSHARED {
					if s.Linux == nil {
						s.Linux = &specs.Linux{}
					}
					s.Linux.RootfsPropagation = mountPropagationReverseMap[mount.SHARED]
				}
			case mount.SLAVE, mount.RSLAVE:
				var fallback bool
				if err := ensureSharedOrSlave(m.Source); err != nil {
					// For backwards compatibility purposes, treat mounts from the daemon root
					// as special since we automatically add rslave propagation to these mounts
					// when the user did not set anything, so we should fallback to the old
					// behavior which is to use private propagation which is normally the
					// default.
					if !strings.HasPrefix(m.Source, daemon.root) && !strings.HasPrefix(daemon.root, m.Source) {
						return err
					}

					cm, ok := c.MountPoints[m.Destination]
					if !ok {
						return err
					}
					if cm.Spec.BindOptions != nil && cm.Spec.BindOptions.Propagation != "" {
						// This means the user explicitly set a propagation, do not fallback in that case.
						return err
					}
					fallback = true
					log.G(ctx).WithField("container", c.ID).WithField("source", m.Source).Warn("Falling back to default propagation for bind source in daemon root")
				}
				if !fallback {
					rootpg := mountPropagationMap[s.Linux.RootfsPropagation]
					if rootpg != mount.SHARED && rootpg != mount.RSHARED && rootpg != mount.SLAVE && rootpg != mount.RSLAVE {
						if s.Linux == nil {
							s.Linux = &specs.Linux{}
						}
						s.Linux.RootfsPropagation = mountPropagationReverseMap[mount.RSLAVE]
					}
				}
			}

			bindMode := "rbind"
			if m.NonRecursive {
				bindMode = "bind"
			}
			opts := []string{bindMode}
			if !m.Writable {
				rro := true
				if m.ReadOnlyNonRecursive {
					rro = false
					if m.ReadOnlyForceRecursive {
						return errors.New("mount options conflict: ReadOnlyNonRecursive && ReadOnlyForceRecursive")
					}
				}
				if rroErr := supportsRecursivelyReadOnly(daemonCfg, c.HostConfig.Runtime); rroErr != nil {
					rro = false
					if m.ReadOnlyForceRecursive {
						return rroErr
					}
				}
				if rro {
					opts = append(opts, "rro")
				} else {
					opts = append(opts, "ro")
				}
			}
			if pFlag != 0 {
				opts = append(opts, mountPropagationReverseMap[pFlag])
			}

			// If we are using user namespaces, then we must make sure that we
			// don't drop any of the CL_UNPRIVILEGED "locked" flags of the source
			// "mount" when we bind-mount. The reason for this is that at the point
			// when runc sets up the root filesystem, it is already inside a user
			// namespace, and thus cannot change any flags that are locked.
			if daemonCfg.RemappedRoot != "" || userns.RunningInUserNS() {
				unprivOpts, err := mountopts.UnprivilegedMountFlags(m.Source)
				if err != nil {
					return err
				}
				opts = append(opts, unprivOpts...)
			}

			mt.Options = opts
			s.Mounts = append(s.Mounts, mt)
		}

		if s.Root.Readonly {
			for i, m := range s.Mounts {
				switch m.Destination {
				case "/proc", "/dev/pts", "/dev/shm", "/dev/mqueue", "/dev":
					continue
				}
				if _, ok := userMounts[m.Destination]; !ok {
					if !slices.Contains(m.Options, "ro") {
						s.Mounts[i].Options = append(s.Mounts[i].Options, "ro")
					}
				}
			}
		}

		if c.HostConfig.Privileged {
			// clear readonly for /sys
			for i := range s.Mounts {
				if s.Mounts[i].Destination == "/sys" {
					clearReadOnly(&s.Mounts[i])
				}
			}
			if s.Linux != nil {
				s.Linux.ReadonlyPaths = nil
				s.Linux.MaskedPaths = nil
			}
		}

		// if the user didn't specify otherwise, default to the value of privileged
		writableCgroups := c.HostConfig.Privileged
		if c.WritableCgroups != nil {
			if daemonCfg.Rootless || daemon.idMapping.UIDMaps != nil {
				// error if the user requested a configuration we can't explicitly support
				return errors.New("option WritableCgroups conflicts with user namespaces and rootless mode")
			}
			writableCgroups = *c.WritableCgroups
		}
		// TODO: until a kernel/mount solution exists for handling remount in a user namespace,
		// we must clear the readonly flag for the cgroups mount (@mrunalp concurs)
		if daemon.idMapping.UIDMaps != nil {
			writableCgroups = true
		}
		if writableCgroups {
			for i, m := range s.Mounts {
				if m.Type == "cgroup" {
					clearReadOnly(&s.Mounts[i])
				}
			}
		}

		return nil
	}
}

// sysctlExists checks if a sysctl exists; runc will error if we add any that do not actually
// exist, so do not add the default ones if running on an old kernel.
func sysctlExists(s string) bool {
	f := filepath.Join("/proc", "sys", strings.ReplaceAll(s, ".", "/"))
	_, err := os.Stat(f) // #nosec G703 -- only used with fixed / hardcoded paths.
	return err == nil
}

// withCommonOptions sets common docker options
func withCommonOptions(daemon *Daemon, daemonCfg *dconfig.Config, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if c.BaseFS == "" {
			return errors.New("populateCommonSpec: BaseFS of container " + c.ID + " is unexpectedly empty")
		}
		linkedEnv, err := daemon.setupLinkedContainers(c)
		if err != nil {
			return err
		}
		s.Root = &specs.Root{
			Path:     c.BaseFS,
			Readonly: c.HostConfig.ReadonlyRootfs,
		}
		if err := c.SetupWorkingDirectory(daemon.idMapping.RootPair()); err != nil {
			return err
		}
		cwd := c.Config.WorkingDir
		if cwd == "" {
			cwd = "/"
		}
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.Args = append([]string{c.Path}, c.Args...)

		// only add the custom init if it is specified and the container is running in its
		// own private pid namespace.  It does not make sense to add if it is running in the
		// host namespace or another container's pid namespace where we already have an init
		if c.HostConfig.PidMode.IsPrivate() {
			if (c.HostConfig.Init != nil && *c.HostConfig.Init) ||
				(c.HostConfig.Init == nil && daemonCfg.Init) {
				s.Process.Args = append([]string{inContainerInitPath, "--", c.Path}, c.Args...)
				path, err := daemonCfg.LookupInitPath() // this will fall back to DefaultInitBinary and return an absolute path
				if err != nil {
					return err
				}
				s.Mounts = append(s.Mounts, specs.Mount{
					Destination: inContainerInitPath,
					Type:        "bind",
					Source:      path,
					Options:     []string{"bind", "ro"},
				})
			}
		}
		s.Process.Cwd = cwd
		s.Process.Env = c.CreateDaemonEnvironment(c.Config.Tty, linkedEnv)
		s.Process.Terminal = c.Config.Tty

		s.Hostname = c.Config.Hostname
		s.Domainname = c.Config.Domainname

		// Add default sysctls that are generally safe and useful; currently we
		// grant the capabilities to allow these anyway. You can override if
		// you want to restore the original behaviour.
		// We do not set network sysctls if network namespace is host, or if we are
		// joining an existing namespace, only if we create a new net namespace.
		if c.HostConfig.NetworkMode.IsPrivate() {
			if s.Linux == nil {
				s.Linux = &specs.Linux{}
			}
			if s.Linux.Sysctl == nil {
				s.Linux.Sysctl = make(map[string]string)
			}
			// We cannot set up ping socket support in a user namespace
			userNS := daemonCfg.RemappedRoot != "" && c.HostConfig.UsernsMode.IsPrivate()
			if !userNS && !userns.RunningInUserNS() && sysctlExists("net.ipv4.ping_group_range") {
				// allow unprivileged ICMP echo sockets without CAP_NET_RAW
				s.Linux.Sysctl["net.ipv4.ping_group_range"] = "0 2147483647"
			}
			// allow opening any port less than 1024 without CAP_NET_BIND_SERVICE
			if sysctlExists("net.ipv4.ip_unprivileged_port_start") {
				s.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"] = "0"
			}
		}

		return nil
	}
}

// withCgroups sets the container's cgroups
func withCgroups(daemon *Daemon, daemonCfg *dconfig.Config, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		var cgroupsPath string
		scopePrefix := "docker"
		parent := "/docker"
		useSystemd := UsingSystemd(daemonCfg)
		if useSystemd {
			parent = "system.slice"
			if daemonCfg.Rootless {
				parent = "user.slice"
			}
		}

		if c.HostConfig.CgroupParent != "" {
			parent = c.HostConfig.CgroupParent
		} else if daemonCfg.CgroupParent != "" {
			parent = daemonCfg.CgroupParent
		}

		if useSystemd {
			cgroupsPath = parent + ":" + scopePrefix + ":" + c.ID
			log.G(ctx).Debugf("createSpec: cgroupsPath: %s", cgroupsPath)
		} else {
			cgroupsPath = filepath.Join(parent, c.ID)
		}
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Linux.CgroupsPath = cgroupsPath

		// the rest is only needed for CPU RT controller

		if daemonCfg.CPURealtimePeriod == 0 && daemonCfg.CPURealtimeRuntime == 0 {
			return nil
		}

		p := cgroupsPath
		if useSystemd {
			path, err := cgroups.GetOwnCgroup("cpu")
			if err != nil {
				return errors.Wrap(err, "unable to init CPU RT controller")
			}
			p = filepath.Join(path, s.Linux.CgroupsPath)
		}

		// Clean path to guard against things like ../../../BAD
		parentPath := filepath.Dir(p)
		if !filepath.IsAbs(parentPath) {
			parentPath = filepath.Clean("/" + parentPath)
		}

		mnt, root, err := cgroups.FindCgroupMountpointAndRoot("", "cpu")
		if err != nil {
			return errors.Wrap(err, "unable to init CPU RT controller")
		}
		// When docker is run inside docker, the root is based of the host cgroup.
		// Should this be handled in runc/libcontainer/cgroups ?
		if strings.HasPrefix(root, "/docker/") {
			root = "/"
		}
		mnt = filepath.Join(mnt, root)

		if err := daemon.initCPURtController(daemonCfg, mnt, parentPath); err != nil {
			return errors.Wrap(err, "unable to init CPU RT controller")
		}
		return nil
	}
}

// WithDevices sets the container's devices
func WithDevices(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		// Build lists of devices allowed and created within the container.
		var devs []specs.LinuxDevice
		devPermissions := s.Linux.Resources.Devices

		if c.HostConfig.Privileged {
			hostDevices, err := coci.HostDevices()
			if err != nil {
				return err
			}
			devs = append(devs, hostDevices...)

			// adding device mappings in privileged containers
			for _, deviceMapping := range c.HostConfig.Devices {
				// issue a warning that custom cgroup permissions are ignored in privileged mode
				if deviceMapping.CgroupPermissions != "rwm" {
					log.G(ctx).WithField("container", c.ID).Warnf("custom %s permissions for device %s are ignored in privileged mode", deviceMapping.CgroupPermissions, deviceMapping.PathOnHost)
				}
				// issue a warning that the device path already exists via /dev mounting in privileged mode
				if deviceMapping.PathOnHost == deviceMapping.PathInContainer {
					log.G(ctx).WithField("container", c.ID).Warnf("path in container %s already exists in privileged mode", deviceMapping.PathInContainer)
					continue
				}
				d, _, err := oci.DevicesFromPath(deviceMapping.PathOnHost, deviceMapping.PathInContainer, "rwm")
				if err != nil {
					return err
				}
				devs = append(devs, d...)
			}

			devPermissions = []specs.LinuxDeviceCgroup{
				{
					Allow:  true,
					Access: "rwm",
				},
			}
		} else {
			for _, deviceMapping := range c.HostConfig.Devices {
				d, dPermissions, err := oci.DevicesFromPath(deviceMapping.PathOnHost, deviceMapping.PathInContainer, deviceMapping.CgroupPermissions)
				if err != nil {
					return err
				}
				devs = append(devs, d...)
				devPermissions = append(devPermissions, dPermissions...)
			}

			var err error
			devPermissions, err = oci.AppendDevicePermissionsFromCgroupRules(devPermissions, c.HostConfig.DeviceCgroupRules)
			if err != nil {
				return err
			}
		}

		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &specs.LinuxResources{}
		}
		s.Linux.Devices = append(s.Linux.Devices, devs...)
		s.Linux.Resources.Devices = append(s.Linux.Resources.Devices, devPermissions...)

		for _, req := range c.HostConfig.DeviceRequests {
			if err := daemon.handleDevice(req, s); err != nil {
				return err
			}
		}
		return nil
	}
}

// WithResources applies the container resources
func WithResources(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		r := c.HostConfig.Resources
		weightDevices, err := getBlkioWeightDevices(r)
		if err != nil {
			return err
		}
		readBpsDevice, err := getBlkioThrottleDevices(r.BlkioDeviceReadBps)
		if err != nil {
			return err
		}
		writeBpsDevice, err := getBlkioThrottleDevices(r.BlkioDeviceWriteBps)
		if err != nil {
			return err
		}
		readIOpsDevice, err := getBlkioThrottleDevices(r.BlkioDeviceReadIOps)
		if err != nil {
			return err
		}
		writeIOpsDevice, err := getBlkioThrottleDevices(r.BlkioDeviceWriteIOps)
		if err != nil {
			return err
		}

		memoryRes := getMemoryResources(r)
		cpuRes, err := getCPUResources(r)
		if err != nil {
			return err
		}

		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &specs.LinuxResources{}
		}
		s.Linux.Resources.Memory = memoryRes
		s.Linux.Resources.CPU = cpuRes
		s.Linux.Resources.BlockIO = &specs.LinuxBlockIO{
			WeightDevice:            weightDevices,
			ThrottleReadBpsDevice:   readBpsDevice,
			ThrottleWriteBpsDevice:  writeBpsDevice,
			ThrottleReadIOPSDevice:  readIOpsDevice,
			ThrottleWriteIOPSDevice: writeIOpsDevice,
		}
		if r.BlkioWeight != 0 {
			w := r.BlkioWeight
			s.Linux.Resources.BlockIO.Weight = &w
		}
		s.Linux.Resources.Pids = getPidsLimit(r)

		return nil
	}
}

// WithSysctls sets the container's sysctls
func WithSysctls(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if len(c.HostConfig.Sysctls) == 0 {
			return nil
		}
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Sysctl == nil {
			s.Linux.Sysctl = make(map[string]string)
		}
		// We merge the sysctls injected above with the HostConfig (latter takes
		// precedence for backwards-compatibility reasons).
		maps.Copy(s.Linux.Sysctl, c.HostConfig.Sysctls)
		return nil
	}
}

// WithUser sets the container's user
func WithUser(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		user, err := getUser(c, c.Config.User)
		// Preserve fields already set on the process user, such as Umask.
		s.Process.User.UID = user.UID
		s.Process.User.GID = user.GID
		s.Process.User.AdditionalGids = user.AdditionalGids
		return err
	}
}

// WithUmask sets the container's umask.
func WithUmask(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, client coci.Client, ctr *containers.Container, s *coci.Spec) error {
		if c.HostConfig.Umask == nil {
			return nil
		}
		return coci.WithUmask(*c.HostConfig.Umask)(ctx, client, ctr, s)
	}
}

func (daemon *Daemon) createSpec(ctx context.Context, daemonCfg *configStore, c *container.Container, mounts []container.Mount) (retSpec *specs.Spec, _ error) {
	var (
		opts []coci.SpecOpts
		s    = oci.DefaultSpec()
	)
	opts = append(opts,
		withCommonOptions(daemon, &daemonCfg.Config, c),
		withCgroups(daemon, &daemonCfg.Config, c),
		WithResources(c),
		WithSysctls(c),
		// Set the user before CDI device injection, which may append supplementary groups.
		WithUser(c),
		WithUmask(c),
		WithDevices(daemon, c),
		withRlimits(daemon, &daemonCfg.Config, c),
		WithNamespaces(daemon, c),
		WithCapabilities(c),
		WithSeccomp(daemon, c),
		withMounts(daemon, daemonCfg, c, mounts),
		WithApparmor(daemon, c),
		WithSelinux(c),
		WithOOMScore(&c.HostConfig.OomScoreAdj),
		coci.WithAnnotations(c.HostConfig.Annotations),
	)

	if c.NoNewPrivileges {
		opts = append(opts, coci.WithNoNewPrivileges)
	}
	if c.Config.Tty {
		opts = append(opts, WithConsoleSize(c))
	}
	// Set the masked and readonly paths with regard to the host config options if they are set.
	if c.HostConfig.MaskedPaths != nil {
		opts = append(opts, coci.WithMaskedPaths(c.HostConfig.MaskedPaths))
	}
	if c.HostConfig.ReadonlyPaths != nil {
		opts = append(opts, coci.WithReadonlyPaths(c.HostConfig.ReadonlyPaths))
	}
	if daemonCfg.Rootless {
		opts = append(opts, withRootless(daemon, &daemonCfg.Config))
	} else if userns.RunningInUserNS() {
		opts = append(opts, withRootfulInRootless(daemon, &daemonCfg.Config))
	}

	var snapshotter, snapshotKey string
	if daemon.UsesSnapshotter() {
		snapshotter = daemon.imageService.StorageDriver()
		snapshotKey = c.ID
	}

	return &s, coci.ApplyOpts(ctx, daemon.containerdClient, &containers.Container{
		ID:          c.ID,
		Snapshotter: snapshotter,
		SnapshotKey: snapshotKey,
	}, &s, opts...)
}

func clearReadOnly(m *specs.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = opt
}

// mergeUlimits merge the Ulimits from HostConfig with daemon defaults, and update HostConfig
func (daemon *Daemon) mergeUlimits(c *containertypes.HostConfig, daemonCfg *dconfig.Config) {
	ulimits := c.Ulimits
	// Merge ulimits with daemon defaults
	ulIdx := make(map[string]struct{})
	for _, ul := range ulimits {
		ulIdx[ul.Name] = struct{}{}
	}
	for name, ul := range daemonCfg.Ulimits {
		if _, exists := ulIdx[name]; !exists {
			ulimits = append(ulimits, ul)
		}
	}
	c.Ulimits = ulimits
}

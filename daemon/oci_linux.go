package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/containerd/containerd/containers"
	coci "github.com/containerd/containerd/oci"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	daemonconfig "github.com/docker/docker/daemon/config"
	"github.com/docker/docker/oci"
	"github.com/docker/docker/oci/caps"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/rootless/specconv"
	volumemounts "github.com/docker/docker/volume/mounts"
	"github.com/opencontainers/runc/libcontainer/apparmor"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/devices"
	rsystem "github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/user"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const inContainerInitPath = "/sbin/" + daemonconfig.DefaultInitBinary

// WithRlimits sets the container's rlimits along with merging the daemon's rlimits
func WithRlimits(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		var rlimits []specs.POSIXRlimit

		// We want to leave the original HostConfig alone so make a copy here
		hostConfig := *c.HostConfig
		// Merge with the daemon defaults
		daemon.mergeUlimits(&hostConfig)
		for _, ul := range hostConfig.Ulimits {
			rlimits = append(rlimits, specs.POSIXRlimit{
				Type: "RLIMIT_" + strings.ToUpper(ul.Name),
				Soft: uint64(ul.Soft),
				Hard: uint64(ul.Hard),
			})
		}

		s.Process.Rlimits = rlimits
		return nil
	}
}

// WithLibnetwork sets the libnetwork hook
func WithLibnetwork(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if s.Hooks == nil {
			s.Hooks = &specs.Hooks{}
		}
		for _, ns := range s.Linux.Namespaces {
			if ns.Type == "network" && ns.Path == "" && !c.Config.NetworkDisabled {
				target := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "exe")
				s.Hooks.Prestart = append(s.Hooks.Prestart, specs.Hook{
					Path: target,
					Args: []string{
						"libnetwork-setkey",
						"-exec-root=" + daemon.configStore.GetExecRoot(),
						c.ID,
						daemon.netController.ID(),
					},
				})
			}
		}
		return nil
	}
}

// WithRootless sets the spec to the rootless configuration
func WithRootless(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
	return specconv.ToRootless(s)
}

// WithOOMScore sets the oom score
func WithOOMScore(score *int) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		s.Process.OOMScoreAdj = score
		return nil
	}
}

// WithSelinux sets the selinux labels
func WithSelinux(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		s.Process.SelinuxLabel = c.GetProcessLabel()
		s.Linux.MountLabel = c.MountLabel
		return nil
	}
}

// WithApparmor sets the apparmor profile
func WithApparmor(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if apparmor.IsEnabled() {
			var appArmorProfile string
			if c.AppArmorProfile != "" {
				appArmorProfile = c.AppArmorProfile
			} else if c.HostConfig.Privileged {
				appArmorProfile = "unconfined"
			} else {
				appArmorProfile = "docker-default"
			}

			if appArmorProfile == "docker-default" {
				// Unattended upgrades and other fun services can unload AppArmor
				// profiles inadvertently. Since we cannot store our profile in
				// /etc/apparmor.d, nor can we practically add other ways of
				// telling the system to keep our profile loaded, in order to make
				// sure that we keep the default profile enabled we dynamically
				// reload it if necessary.
				if err := ensureDefaultAppArmorProfile(); err != nil {
					return err
				}
			}
			s.Process.ApparmorProfile = appArmorProfile
		}
		return nil
	}
}

// WithCapabilities sets the container's capabilties
func WithCapabilities(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		capabilities, err := caps.TweakCapabilities(
			oci.DefaultCapabilities(),
			c.HostConfig.CapAdd,
			c.HostConfig.CapDrop,
			c.HostConfig.Capabilities,
			c.HostConfig.Privileged,
		)
		if err != nil {
			return err
		}
		return oci.SetCapabilities(s, capabilities)
	}
}

func readUserFile(c *container.Container, p string) (io.ReadCloser, error) {
	fp, err := c.GetResourcePath(p)
	if err != nil {
		return nil, err
	}
	return os.Open(fp)
}

func getUser(c *container.Container, username string) (uint32, uint32, []uint32, error) {
	passwdPath, err := user.GetPasswdPath()
	if err != nil {
		return 0, 0, nil, err
	}
	groupPath, err := user.GetGroupPath()
	if err != nil {
		return 0, 0, nil, err
	}
	passwdFile, err := readUserFile(c, passwdPath)
	if err == nil {
		defer passwdFile.Close()
	}
	groupFile, err := readUserFile(c, groupPath)
	if err == nil {
		defer groupFile.Close()
	}

	execUser, err := user.GetExecUser(username, nil, passwdFile, groupFile)
	if err != nil {
		return 0, 0, nil, err
	}

	// todo: fix this double read by a change to libcontainer/user pkg
	groupFile, err = readUserFile(c, groupPath)
	if err == nil {
		defer groupFile.Close()
	}
	var addGroups []int
	if len(c.HostConfig.GroupAdd) > 0 {
		addGroups, err = user.GetAdditionalGroups(c.HostConfig.GroupAdd, groupFile)
		if err != nil {
			return 0, 0, nil, err
		}
	}
	uid := uint32(execUser.Uid)
	gid := uint32(execUser.Gid)
	sgids := append(execUser.Sgids, addGroups...)
	var additionalGids []uint32
	for _, g := range sgids {
		additionalGids = append(additionalGids, uint32(g))
	}
	return uid, gid, additionalGids, nil
}

func setNamespace(s *specs.Spec, ns specs.LinuxNamespace) {
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
			uidMap := daemon.idMapping.UIDs()
			if uidMap != nil {
				userNS = true
				ns := specs.LinuxNamespace{Type: "user"}
				setNamespace(s, ns)
				s.Linux.UIDMappings = specMapping(uidMap)
				s.Linux.GIDMappings = specMapping(daemon.idMapping.GIDs())
			}
		}
		// network
		if !c.Config.NetworkDisabled {
			ns := specs.LinuxNamespace{Type: "network"}
			parts := strings.SplitN(string(c.HostConfig.NetworkMode), ":", 2)
			if parts[0] == "container" {
				nc, err := daemon.getNetworkedContainer(c.ID, c.HostConfig.NetworkMode.ConnectedContainer())
				if err != nil {
					return err
				}
				ns.Path = fmt.Sprintf("/proc/%d/ns/net", nc.State.GetPID())
				if userNS {
					// to share a net namespace, they must also share a user namespace
					nsUser := specs.LinuxNamespace{Type: "user"}
					nsUser.Path = fmt.Sprintf("/proc/%d/ns/user", nc.State.GetPID())
					setNamespace(s, nsUser)
				}
			} else if c.HostConfig.NetworkMode.IsHost() {
				ns.Path = c.NetworkSettings.SandboxKey
			}
			setNamespace(s, ns)
		}

		// ipc
		ipcMode := c.HostConfig.IpcMode
		switch {
		case ipcMode.IsContainer():
			ns := specs.LinuxNamespace{Type: "ipc"}
			ic, err := daemon.getIpcContainer(ipcMode.Container())
			if err != nil {
				return err
			}
			ns.Path = fmt.Sprintf("/proc/%d/ns/ipc", ic.State.GetPID())
			setNamespace(s, ns)
			if userNS {
				// to share an IPC namespace, they must also share a user namespace
				nsUser := specs.LinuxNamespace{Type: "user"}
				nsUser.Path = fmt.Sprintf("/proc/%d/ns/user", ic.State.GetPID())
				setNamespace(s, nsUser)
			}
		case ipcMode.IsHost():
			oci.RemoveNamespace(s, specs.LinuxNamespaceType("ipc"))
		case ipcMode.IsEmpty():
			// A container was created by an older version of the daemon.
			// The default behavior used to be what is now called "shareable".
			fallthrough
		case ipcMode.IsPrivate(), ipcMode.IsShareable(), ipcMode.IsNone():
			ns := specs.LinuxNamespace{Type: "ipc"}
			setNamespace(s, ns)
		default:
			return fmt.Errorf("Invalid IPC mode: %v", ipcMode)
		}

		// pid
		if c.HostConfig.PidMode.IsContainer() {
			ns := specs.LinuxNamespace{Type: "pid"}
			pc, err := daemon.getPidContainer(c)
			if err != nil {
				return err
			}
			ns.Path = fmt.Sprintf("/proc/%d/ns/pid", pc.State.GetPID())
			setNamespace(s, ns)
			if userNS {
				// to share a PID namespace, they must also share a user namespace
				nsUser := specs.LinuxNamespace{Type: "user"}
				nsUser.Path = fmt.Sprintf("/proc/%d/ns/user", pc.State.GetPID())
				setNamespace(s, nsUser)
			}
		} else if c.HostConfig.PidMode.IsHost() {
			oci.RemoveNamespace(s, specs.LinuxNamespaceType("pid"))
		} else {
			ns := specs.LinuxNamespace{Type: "pid"}
			setNamespace(s, ns)
		}
		// uts
		if c.HostConfig.UTSMode.IsHost() {
			oci.RemoveNamespace(s, specs.LinuxNamespaceType("uts"))
			s.Hostname = ""
		}

		// cgroup
		if !c.HostConfig.CgroupnsMode.IsEmpty() {
			cgroupNsMode := c.HostConfig.CgroupnsMode
			if !cgroupNsMode.Valid() {
				return fmt.Errorf("invalid cgroup namespace mode: %v", cgroupNsMode)
			}

			if cgroupNsMode.IsPrivate() && !c.HostConfig.Privileged {
				nsCgroup := specs.LinuxNamespace{Type: "cgroup"}
				setNamespace(s, nsCgroup)
			}
		}

		return nil
	}
}

func specMapping(s []idtools.IDMap) []specs.LinuxIDMapping {
	var ids []specs.LinuxIDMapping
	for _, item := range s {
		ids = append(ids, specs.LinuxIDMapping{
			HostID:      uint32(item.HostID),
			ContainerID: uint32(item.ContainerID),
			Size:        uint32(item.Size),
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

	mi, err := mount.GetMounts(mount.ParentsFilter(sourcePath))
	if err != nil {
		return "", "", err
	}
	if len(mi) < 1 {
		return "", "", fmt.Errorf("Can't find mount point of %s", source)
	}

	// find the longest mount point
	var idx, maxlen int
	for i := range mi {
		if len(mi[i].Mountpoint) > maxlen {
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

// hasMountinfoOption checks if any of the passed any of the given option values
// are set in the passed in option string.
func hasMountinfoOption(opts string, vals ...string) bool {
	for _, opt := range strings.Split(opts, " ") {
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
	if !hasMountinfoOption(optionalOpts, sharedPropagationOption) {
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

	if !hasMountinfoOption(optionalOpts, sharedPropagationOption, slavePropagationOption) {
		return errors.Errorf("path %s is mounted on %s but it is not a shared or slave mount", path, sourceMount)
	}
	return nil
}

// Get the set of mount flags that are set on the mount that contains the given
// path and are locked by CL_UNPRIVILEGED. This is necessary to ensure that
// bind-mounting "with options" will not fail with user namespaces, due to
// kernel restrictions that require user namespace mounts to preserve
// CL_UNPRIVILEGED locked flags.
func getUnprivilegedMountFlags(path string) ([]string, error) {
	var statfs unix.Statfs_t
	if err := unix.Statfs(path, &statfs); err != nil {
		return nil, err
	}

	// The set of keys come from https://github.com/torvalds/linux/blob/v4.13/fs/namespace.c#L1034-L1048.
	unprivilegedFlags := map[uint64]string{
		unix.MS_RDONLY:     "ro",
		unix.MS_NODEV:      "nodev",
		unix.MS_NOEXEC:     "noexec",
		unix.MS_NOSUID:     "nosuid",
		unix.MS_NOATIME:    "noatime",
		unix.MS_RELATIME:   "relatime",
		unix.MS_NODIRATIME: "nodiratime",
	}

	var flags []string
	for mask, flag := range unprivilegedFlags {
		if uint64(statfs.Flags)&mask == mask {
			flags = append(flags, flag)
		}
	}

	return flags, nil
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

// inSlice tests whether a string is contained in a slice of strings or not.
// Comparison is case sensitive
func inSlice(slice []string, s string) bool {
	for _, ss := range slice {
		if s == ss {
			return true
		}
	}
	return false
}

// WithMounts sets the container's mounts
func WithMounts(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) (err error) {
		if err := daemon.setupContainerMountsRoot(c); err != nil {
			return err
		}

		if err := daemon.setupIpcDirs(c); err != nil {
			return err
		}

		defer func() {
			if err != nil {
				daemon.cleanupSecretDir(c)
			}
		}()

		if err := daemon.setupSecretDir(c); err != nil {
			return err
		}

		ms, err := daemon.setupMounts(c)
		if err != nil {
			return err
		}

		if !c.HostConfig.IpcMode.IsPrivate() && !c.HostConfig.IpcMode.IsEmpty() {
			ms = append(ms, c.IpcMounts()...)
		}

		tmpfsMounts, err := c.TmpfsMounts()
		if err != nil {
			return err
		}
		ms = append(ms, tmpfsMounts...)

		secretMounts, err := c.SecretMounts()
		if err != nil {
			return err
		}
		ms = append(ms, secretMounts...)

		sort.Sort(mounts(ms))

		mounts := ms

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
				parser := volumemounts.NewParser("linux")
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
					logrus.WithField("container", c.ID).WithField("source", m.Source).Warn("Falling back to default propagation for bind source in daemon root")
				}
				if !fallback {
					rootpg := mountPropagationMap[s.Linux.RootfsPropagation]
					if rootpg != mount.SHARED && rootpg != mount.RSHARED && rootpg != mount.SLAVE && rootpg != mount.RSLAVE {
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
				opts = append(opts, "ro")
			}
			if pFlag != 0 {
				opts = append(opts, mountPropagationReverseMap[pFlag])
			}

			// If we are using user namespaces, then we must make sure that we
			// don't drop any of the CL_UNPRIVILEGED "locked" flags of the source
			// "mount" when we bind-mount. The reason for this is that at the point
			// when runc sets up the root filesystem, it is already inside a user
			// namespace, and thus cannot change any flags that are locked.
			if daemon.configStore.RemappedRoot != "" {
				unprivOpts, err := getUnprivilegedMountFlags(m.Source)
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
					if !inSlice(m.Options, "ro") {
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
			s.Linux.ReadonlyPaths = nil
			s.Linux.MaskedPaths = nil
		}

		// TODO: until a kernel/mount solution exists for handling remount in a user namespace,
		// we must clear the readonly flag for the cgroups mount (@mrunalp concurs)
		if uidMap := daemon.idMapping.UIDs(); uidMap != nil || c.HostConfig.Privileged {
			for i, m := range s.Mounts {
				if m.Type == "cgroup" {
					clearReadOnly(&s.Mounts[i])
				}
			}
		}

		return nil

	}
}

// WithCommonOptions sets common docker options
func WithCommonOptions(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if c.BaseFS == nil {
			return errors.New("populateCommonSpec: BaseFS of container " + c.ID + " is unexpectedly nil")
		}
		linkedEnv, err := daemon.setupLinkedContainers(c)
		if err != nil {
			return err
		}
		s.Root = &specs.Root{
			Path:     c.BaseFS.Path(),
			Readonly: c.HostConfig.ReadonlyRootfs,
		}
		if err := c.SetupWorkingDirectory(daemon.idMapping.RootPair()); err != nil {
			return err
		}
		cwd := c.Config.WorkingDir
		if len(cwd) == 0 {
			cwd = "/"
		}
		s.Process.Args = append([]string{c.Path}, c.Args...)

		// only add the custom init if it is specified and the container is running in its
		// own private pid namespace.  It does not make sense to add if it is running in the
		// host namespace or another container's pid namespace where we already have an init
		if c.HostConfig.PidMode.IsPrivate() {
			if (c.HostConfig.Init != nil && *c.HostConfig.Init) ||
				(c.HostConfig.Init == nil && daemon.configStore.Init) {
				s.Process.Args = append([]string{inContainerInitPath, "--", c.Path}, c.Args...)
				path := daemon.configStore.InitPath
				if path == "" {
					path, err = exec.LookPath(daemonconfig.DefaultInitBinary)
					if err != nil {
						return err
					}
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
		setLinuxDomainname(c, s)

		return nil
	}
}

// WithCgroups sets the container's cgroups
func WithCgroups(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		var cgroupsPath string
		scopePrefix := "docker"
		parent := "/docker"
		useSystemd := UsingSystemd(daemon.configStore)
		if useSystemd {
			parent = "system.slice"
		}

		if c.HostConfig.CgroupParent != "" {
			parent = c.HostConfig.CgroupParent
		} else if daemon.configStore.CgroupParent != "" {
			parent = daemon.configStore.CgroupParent
		}

		if useSystemd {
			cgroupsPath = parent + ":" + scopePrefix + ":" + c.ID
			logrus.Debugf("createSpec: cgroupsPath: %s", cgroupsPath)
		} else {
			cgroupsPath = filepath.Join(parent, c.ID)
		}
		s.Linux.CgroupsPath = cgroupsPath
		p := cgroupsPath
		if useSystemd {
			initPath, err := cgroups.GetInitCgroup("cpu")
			if err != nil {
				return err
			}
			_, err = cgroups.GetOwnCgroup("cpu")
			if err != nil {
				return err
			}
			p = filepath.Join(initPath, s.Linux.CgroupsPath)
		}

		// Clean path to guard against things like ../../../BAD
		parentPath := filepath.Dir(p)
		if !filepath.IsAbs(parentPath) {
			parentPath = filepath.Clean("/" + parentPath)
		}

		if err := daemon.initCgroupsPath(parentPath); err != nil {
			return fmt.Errorf("linux init cgroups path: %v", err)
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
		if c.HostConfig.Privileged && !rsystem.RunningInUserNS() {
			hostDevices, err := devices.HostDevices()
			if err != nil {
				return err
			}
			for _, d := range hostDevices {
				devs = append(devs, oci.Device(d))
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

		s.Linux.Devices = append(s.Linux.Devices, devs...)
		s.Linux.Resources.Devices = devPermissions

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
		blkioWeight := r.BlkioWeight

		specResources := &specs.LinuxResources{
			Memory: memoryRes,
			CPU:    cpuRes,
			BlockIO: &specs.LinuxBlockIO{
				Weight:                  &blkioWeight,
				WeightDevice:            weightDevices,
				ThrottleReadBpsDevice:   readBpsDevice,
				ThrottleWriteBpsDevice:  writeBpsDevice,
				ThrottleReadIOPSDevice:  readIOpsDevice,
				ThrottleWriteIOPSDevice: writeIOpsDevice,
			},
			Pids: getPidsLimit(r),
		}

		if s.Linux.Resources != nil && len(s.Linux.Resources.Devices) > 0 {
			specResources.Devices = s.Linux.Resources.Devices
		}

		s.Linux.Resources = specResources
		return nil
	}
}

// WithSysctls sets the container's sysctls
func WithSysctls(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		// We merge the sysctls injected above with the HostConfig (latter takes
		// precedence for backwards-compatibility reasons).
		for k, v := range c.HostConfig.Sysctls {
			s.Linux.Sysctl[k] = v
		}
		return nil
	}
}

// WithUser sets the container's user
func WithUser(c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		uid, gid, additionalGids, err := getUser(c, c.Config.User)
		if err != nil {
			return err
		}
		s.Process.User.UID = uid
		s.Process.User.GID = gid
		s.Process.User.AdditionalGids = additionalGids
		return nil
	}
}

func (daemon *Daemon) createSpec(c *container.Container) (retSpec *specs.Spec, err error) {
	var (
		opts []coci.SpecOpts
		s    = oci.DefaultSpec()
	)
	opts = append(opts,
		WithCommonOptions(daemon, c),
		WithCgroups(daemon, c),
		WithResources(c),
		WithSysctls(c),
		WithDevices(daemon, c),
		WithUser(c),
		WithRlimits(daemon, c),
		WithNamespaces(daemon, c),
		WithCapabilities(c),
		WithSeccomp(daemon, c),
		WithMounts(daemon, c),
		WithLibnetwork(daemon, c),
		WithApparmor(c),
		WithSelinux(c),
		WithOOMScore(&c.HostConfig.OomScoreAdj),
	)
	if c.NoNewPrivileges {
		opts = append(opts, coci.WithNoNewPrivileges)
	}

	// Set the masked and readonly paths with regard to the host config options if they are set.
	if c.HostConfig.MaskedPaths != nil {
		opts = append(opts, coci.WithMaskedPaths(c.HostConfig.MaskedPaths))
	}
	if c.HostConfig.ReadonlyPaths != nil {
		opts = append(opts, coci.WithReadonlyPaths(c.HostConfig.ReadonlyPaths))
	}
	if daemon.configStore.Rootless {
		opts = append(opts, WithRootless)
	}
	return &s, coci.ApplyOpts(context.Background(), nil, &containers.Container{
		ID: c.ID,
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
func (daemon *Daemon) mergeUlimits(c *containertypes.HostConfig) {
	ulimits := c.Ulimits
	// Merge ulimits with daemon defaults
	ulIdx := make(map[string]struct{})
	for _, ul := range ulimits {
		ulIdx[ul.Name] = struct{}{}
	}
	for name, ul := range daemon.configStore.Ulimits {
		if _, exists := ulIdx[name]; !exists {
			ulimits = append(ulimits, ul)
		}
	}
	c.Ulimits = ulimits
}

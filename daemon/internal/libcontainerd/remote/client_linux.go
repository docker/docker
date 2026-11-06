package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/sys/user"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func summaryFromInterface(i any) (*libcontainerdtypes.Summary, error) {
	return &libcontainerdtypes.Summary{}, nil
}

func (t *task) UpdateResources(ctx context.Context, resources *libcontainerdtypes.Resources) error {
	return t.Update(ctx, containerd.WithResources(resources))
}

func hostIDFromMap(id uint32, mp []specs.LinuxIDMapping) int {
	for _, m := range mp {
		if id >= m.ContainerID && id <= m.ContainerID+m.Size-1 {
			return int(m.HostID + id - m.ContainerID)
		}
	}
	return 0
}

func getSpecUser(ociSpec *specs.Spec) (int, int) {
	var (
		uid int
		gid int
	)

	for _, ns := range ociSpec.Linux.Namespaces {
		if ns.Type == specs.UserNamespace {
			uid = hostIDFromMap(0, ociSpec.Linux.UIDMappings)
			gid = hostIDFromMap(0, ociSpec.Linux.GIDMappings)
			break
		}
	}

	return uid, gid
}

// getIOOwner returns the host UID and GID that should own the stdio pipes
// of the container's process, i.e. the host IDs of the user the process
// runs as (spec.Process.User), mapped through the user namespace ID
// mappings. The containerd shim chowns the stdio pipes to the IoUid/IoGid
// passed when the task is created, so a process that reopens /dev/stderr
// by path is only allowed to write when the pipes are owned by its own
// user; mapping container UID 0 here would deny a non-root process with
// EACCES under userns-remap.
//
// Unlike getSpecUser, which maps container UID 0 and is used for bundle
// ownership, this must follow the process user. When no user namespace is
// configured, or the spec has no process, it returns 0, 0.
func getIOOwner(ociSpec *specs.Spec) (int, int) {
	var (
		uid int
		gid int
	)

	if ociSpec.Linux == nil {
		return uid, gid
	}
	for _, ns := range ociSpec.Linux.Namespaces {
		if ns.Type == specs.UserNamespace {
			if ociSpec.Process != nil {
				uid = hostIDFromMap(ociSpec.Process.User.UID, ociSpec.Linux.UIDMappings)
				gid = hostIDFromMap(ociSpec.Process.User.GID, ociSpec.Linux.GIDMappings)
			} else {
				uid = hostIDFromMap(0, ociSpec.Linux.UIDMappings)
				gid = hostIDFromMap(0, ociSpec.Linux.GIDMappings)
			}
			break
		}
	}

	return uid, gid
}

// WithBundle creates the bundle for the container
func WithBundle(bundleDir string, ociSpec *specs.Spec) containerd.NewContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		uid, gid := getSpecUser(ociSpec)
		if uid == 0 && gid == 0 {
			c.Labels[DockerContainerBundlePath] = bundleDir
			return user.MkdirAllAndChown(bundleDir, 0o755, uid, gid, user.WithOnlyNew)
		}

		p := string(filepath.Separator)
		components := strings.Split(bundleDir, string(filepath.Separator))
		for _, d := range components[1:] {
			p = filepath.Join(p, d)
			fi, err := os.Stat(p)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if os.IsNotExist(err) || fi.Mode()&1 == 0 {
				p = fmt.Sprintf("%s.%d.%d", p, uid, gid)
				if err := user.MkdirAndChown(p, 0o700, uid, gid); err != nil && !os.IsExist(err) {
					return err
				}
			}
		}
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		c.Labels[DockerContainerBundlePath] = p
		return nil
	}
}

func withLogLevel(_ log.Level) containerd.NewTaskOpts {
	panic("Not implemented")
}

func newFIFOSet(bundleDir, processID string, withStdin, withTerminal bool) *cio.FIFOSet {
	config := cio.Config{
		Terminal: withTerminal,
		Stdout:   filepath.Join(bundleDir, processID+"-stdout"),
	}
	paths := []string{config.Stdout}

	if withStdin {
		config.Stdin = filepath.Join(bundleDir, processID+"-stdin")
		paths = append(paths, config.Stdin)
	}
	if !withTerminal {
		config.Stderr = filepath.Join(bundleDir, processID+"-stderr")
		paths = append(paths, config.Stderr)
	}
	closer := func() error {
		for _, path := range paths {
			if err := os.RemoveAll(path); err != nil {
				log.G(context.TODO()).Warnf("libcontainerd: failed to remove fifo %v: %v", path, err)
			}
		}
		return nil
	}

	return cio.NewFIFOSet(config, closer)
}

func (c *client) newDirectIO(ctx context.Context, fifos *cio.FIFOSet) (*cio.DirectIO, error) {
	return cio.NewDirectIO(ctx, fifos)
}

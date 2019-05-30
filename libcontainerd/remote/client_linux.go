package remote // import "github.com/docker/docker/libcontainerd/remote"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
	"github.com/docker/docker/pkg/idtools"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

const runtimeName = "io.containerd.runtime.v1.linux"

func summaryFromInterface(i interface{}) (*libcontainerdtypes.Summary, error) {
	return &libcontainerdtypes.Summary{}, nil
}

func (c *client) UpdateResources(ctx context.Context, containerID string, resources *libcontainerdtypes.Resources) error {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return err
	}

	// go doesn't like the alias in 1.8, this means this need to be
	// platform specific
	return p.(containerd.Task).Update(ctx, containerd.WithResources((*specs.LinuxResources)(resources)))
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

// WithBundle creates the bundle for the container
func WithBundle(bundleDir string, ociSpec *specs.Spec) containerd.NewContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		uid, gid := getSpecUser(ociSpec)
		if uid == 0 && gid == 0 {
			c.Labels[DockerContainerBundlePath] = bundleDir
			return idtools.MkdirAllAndChownNew(bundleDir, 0755, idtools.Identity{UID: 0, GID: 0})
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
				if err := idtools.MkdirAndChown(p, 0700, idtools.Identity{UID: uid, GID: gid}); err != nil && !os.IsExist(err) {
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
				logrus.Warnf("libcontainerd: failed to remove fifo %v: %v", path, err)
			}
		}
		return nil
	}

	return cio.NewFIFOSet(config, closer)
}

func (c *client) newDirectIO(ctx context.Context, fifos *cio.FIFOSet) (*cio.DirectIO, error) {
	return cio.NewDirectIO(ctx, fifos)
}

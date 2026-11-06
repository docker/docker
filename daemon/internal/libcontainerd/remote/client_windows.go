package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

func summaryFromInterface(i any) (*libcontainerdtypes.Summary, error) {
	switch pd := i.(type) {
	case *options.ProcessDetails:
		return &libcontainerdtypes.Summary{
			ImageName:                    pd.ImageName,
			CreatedAt:                    pd.CreatedAt,
			KernelTime_100Ns:             pd.KernelTime_100Ns,
			MemoryCommitBytes:            pd.MemoryCommitBytes,
			MemoryWorkingSetPrivateBytes: pd.MemoryWorkingSetPrivateBytes,
			MemoryWorkingSetSharedBytes:  pd.MemoryWorkingSetSharedBytes,
			ProcessID:                    pd.ProcessID,
			UserTime_100Ns:               pd.UserTime_100Ns,
			ExecID:                       pd.ExecID,
		}, nil
	default:
		return nil, errors.Errorf("Unknown process details type %T", pd)
	}
}

// WithBundle creates the bundle for the container
func WithBundle(bundleDir string, ociSpec *specs.Spec) containerd.NewContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		// TODO: (containerd) Determine if we need to use system.MkdirAllWithACL here
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		c.Labels[DockerContainerBundlePath] = bundleDir
		return os.MkdirAll(bundleDir, 0o755)
	}
}

func withLogLevel(level log.Level) containerd.NewTaskOpts {
	// Make sure we set the runhcs options to debug if we are at debug level.
	return func(_ context.Context, _ *containerd.Client, info *containerd.TaskInfo) error {
		if level == log.DebugLevel {
			info.Options = &options.Options{Debug: true}
		}
		return nil
	}
}

func pipeName(containerID, processID, name string) string {
	return fmt.Sprintf(`\\.\pipe\containerd-%s-%s-%s`, containerID, processID, name)
}

func newFIFOSet(bundleDir, processID string, withStdin, withTerminal bool) *cio.FIFOSet {
	containerID := filepath.Base(bundleDir)
	config := cio.Config{
		Terminal: withTerminal,
		Stdout:   pipeName(containerID, processID, "stdout"),
	}

	if withStdin {
		config.Stdin = pipeName(containerID, processID, "stdin")
	}

	if !config.Terminal {
		config.Stderr = pipeName(containerID, processID, "stderr")
	}

	return cio.NewFIFOSet(config, nil)
}

func (c *client) newDirectIO(ctx context.Context, fifos *cio.FIFOSet) (*cio.DirectIO, error) {
	pipes, err := c.newStdioPipes(fifos)
	if err != nil {
		return nil, err
	}
	return cio.NewDirectIOFromFIFOSet(ctx, pipes.stdin, pipes.stdout, pipes.stderr, fifos), nil
}

func (t *task) UpdateResources(ctx context.Context, resources *libcontainerdtypes.Resources) error {
	// TODO: (containerd): Not implemented, but don't error.
	return nil
}

func getSpecUser(ociSpec *specs.Spec) (int, int) {
	// TODO: (containerd): Not implemented, but don't error.
	return 0, 0
}

func getIOOwner(ociSpec *specs.Spec) (int, int) {
	// TODO: (containerd): Not implemented, but don't error.
	return 0, 0
}

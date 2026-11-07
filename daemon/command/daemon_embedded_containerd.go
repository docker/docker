//go:build !no_embedded_containerd

package command

import (
	"context"
	"path/filepath"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/containerd/server/embedded"
	"github.com/pkg/errors"
)

// initEmbeddedContainerd starts containerd inside the daemon process and points
// the containerd clients at it. It is selected by the "embedded-containerd"
// feature.
func (cli *daemonCLI) initEmbeddedContainerd(ctx context.Context) (func(time.Duration) error, error) {
	rootDir, err := containerdRootDir(ctx, cli.Config)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Warn("Running with experimental embedded-containerd mode")
	dropInDir, err := cli.containerdConfigDropInDir()
	if err != nil {
		return nil, err
	}
	d, err := embedded.Start(
		ctx,
		rootDir,
		filepath.Join(cli.Config.ExecRoot, "containerd"),
		dropInDir,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start embedded containerd")
	}
	cli.Config.ContainerdAddr = d.Address()
	cli.containerdDialer = d.Dial

	return d.WaitTimeout, nil
}

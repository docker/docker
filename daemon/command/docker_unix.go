//go:build unix

package command

import (
	"context"
)

func runDaemon(ctx context.Context, cli *daemonCLI) error {
	return cli.start(ctx)
}

func initLogging() {}

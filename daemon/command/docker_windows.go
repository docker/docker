package command

import (
	"context"

	"github.com/Microsoft/go-winio/pkg/etwlogrus"
	"github.com/containerd/log"
)

func runDaemon(ctx context.Context, cli *daemonCLI) error {
	// On Windows, this may be launching as a service or with an option to
	// register the service.
	stop, runAsService, err := initService(ctx, cli)
	if err != nil {
		return err
	}

	if stop {
		return nil
	}

	if runAsService {
		// If Windows SCM manages the service - no need for PID files
		cli.Config.Pidfile = ""
	}

	err = cli.start(ctx)
	notifyShutdown(ctx, err)
	return err
}

func initLogging() {
	// Provider ID: {6996f090-c5de-5082-a81e-5841acc3a635}
	// Hook isn't closed explicitly, as it will exist until process exit.
	// GUID is generated based on name - see Microsoft/go-winio/tools/etw-provider-gen.
	if hook, err := etwlogrus.NewHook("Moby"); err == nil {
		log.L.Logger.AddHook(hook)
	}
	return
}

// Command apiclient wraps the daemon API client fixture for the integration test.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/extensions/sdk"
	daemonapipb "github.com/moby/moby/v2/extpoints/daemonapi/v0/protogen"
	servicegrpcv0 "github.com/moby/moby/v2/extpoints/servicegrpc/v0"
	"github.com/moby/moby/v2/integration/extension/testdata/apiclient"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := sdk.NewServer()
	if err := srv.Register(apiclient.Extension, servicegrpcv0.ServerPoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv.Depends(daemonapipb.ClientPoint)
	if err := srv.Listen(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

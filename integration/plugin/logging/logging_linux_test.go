package logging

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

func TestContinueAfterPluginCrash(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "test requires daemon on the same host")
	t.Parallel()

	d := daemon.New(t)
	d.StartWithBusybox(t, "--iptables=false", "--init")
	defer d.Stop(t)

	client := d.NewClientT(t)
	createPlugin(t, client, "test", "close_on_start", asLogDriver)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	assert.Assert(t, client.PluginEnable(ctx, "test", types.PluginEnableOptions{Timeout: 30}))
	cancel()
	defer client.PluginRemove(context.Background(), "test", types.PluginRemoveOptions{Force: true})

	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)

	id := container.Run(ctx, t, client,
		container.WithAutoRemove,
		container.WithLogDriver("test"),
		container.WithCmd(
			"/bin/sh", "-c", "while true; do sleep 1; echo hello; done",
		),
	)
	cancel()
	defer client.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{Force: true})

	// Attach to the container to make sure it's written a few times to stdout
	attach, err := client.ContainerAttach(context.Background(), id, types.ContainerAttachOptions{Stream: true, Stdout: true})
	assert.NilError(t, err)

	chErr := make(chan error)
	go func() {
		defer close(chErr)
		rdr := bufio.NewReader(attach.Reader)
		for i := 0; i < 5; i++ {
			_, _, err := rdr.ReadLine()
			if err != nil {
				chErr <- err
				return
			}
		}
	}()

	select {
	case err := <-chErr:
		assert.NilError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for container i/o")
	}

	// check daemon logs for "broken pipe"
	// TODO(@cpuguy83): This is horribly hacky but is the only way to really test this case right now.
	// It would be nice if there was a way to know that a broken pipe has occurred without looking through the logs.
	log, err := os.Open(d.LogFileName())
	assert.NilError(t, err)
	scanner := bufio.NewScanner(log)
	for scanner.Scan() {
		assert.Assert(t, !strings.Contains(scanner.Text(), "broken pipe"))
	}
}

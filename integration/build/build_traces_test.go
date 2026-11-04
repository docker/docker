package build

import (
	"context"
	"errors"
	"testing"
	"time"

	// Register the npipe: protocol connection helper so the Buildkit client
	// can dial Windows daemons.
	_ "github.com/moby/buildkit/client/connhelper/npipe"

	moby_buildkit_v1 "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/moby/moby/v2/internal/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/sync/errgroup"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

type testWriter struct {
	*testing.T
}

func (t *testWriter) Write(p []byte) (int, error) {
	t.Log(string(p))
	return len(p), nil
}

func TestBuildkitHistoryTracePropagation(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows" && !testEnv.UsingSnapshotter(),
		"buildkit is not supported on Windows with graphdrivers")

	ctx := testutil.StartSpan(baseContext, t)

	apiClient := testEnv.APIClient()
	bc, err := client.New(ctx, apiClient.DaemonHost())

	assert.NilError(t, err)
	defer bc.Close()

	def, err := llb.Scratch().Marshal(ctx)
	assert.NilError(t, err)

	eg, ctxGo := errgroup.WithContext(ctx)
	ch := make(chan *client.SolveStatus)

	ctxHistory, cancel := context.WithCancel(ctx)
	defer cancel()

	sub, err := bc.ControlClient().ListenBuildHistory(ctxHistory, &moby_buildkit_v1.BuildHistoryRequest{ActiveOnly: true})
	assert.NilError(t, err)
	sub.CloseSend()

	defer func() {
		cancel()
		<-sub.Context().Done()
	}()

	d, err := progressui.NewDisplay(&testWriter{t}, progressui.AutoMode, progressui.WithPhase("test"))
	assert.NilError(t, err)

	eg.Go(func() error {
		_, err := d.UpdateFrom(ctxGo, ch)
		return err
	})

	eg.Go(func() error {
		_, err := bc.Solve(ctxGo, def, client.SolveOpt{}, ch)
		return err
	})
	assert.NilError(t, eg.Wait())

	he, err := sub.Recv()
	assert.NilError(t, err)
	assert.Assert(t, he != nil)
	cancel()

	// Traces for history records are recorded asynchronously, so we need to wait for it to be available.
	if he.Record.Trace != nil {
		return
	}

	tp := sdktrace.NewTracerProvider()
	// Split this into a new span so it doesn't clutter up the trace reporting GUI.
	ctx, span := tp.Tracer("").Start(ctx, "Wait for trace to propagate to history record")
	defer span.End()

	t.Log("Waiting for trace to be available")
	poll.WaitOn(t, func(logger poll.LogT) poll.Result {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		sub, err := bc.ControlClient().ListenBuildHistory(ctx, &moby_buildkit_v1.BuildHistoryRequest{Ref: he.Record.Ref})
		if err != nil {
			return poll.Error(err)
		}
		sub.CloseSend()

		defer func() {
			cancel()
			<-sub.Context().Done()
		}()

		msg, err := sub.Recv()
		if err != nil {
			return poll.Error(err)
		}

		if msg.Record.Ref != he.Record.Ref {
			return poll.Error(errors.New("got incorrect history record"))
		}
		if msg.Record.Trace != nil {
			return poll.Success()
		}
		return poll.Continue("trace not available yet")
	}, poll.WithDelay(time.Second), poll.WithTimeout(30*time.Second))
}

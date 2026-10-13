package extension

import (
	"runtime"
	"testing"

	extensionclient "github.com/moby/extensions/client"
	greeterv0 "github.com/moby/extensions/example/greeter/v0"
	greeterpb "github.com/moby/extensions/example/greeter/v0/protogen"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/extension/testdata/apiclient"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestExtensionDaemonAPIClient(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "the extension binary must be on the daemon's host")

	ctx := testutil.StartSpan(baseContext, t)

	extDir := buildExtension(ctx, t, apiclient.ID, "./testdata/apiclient/cmd/apiclient")
	startArgs := []string{"--extension-dir", extDir}
	if testEnv.DaemonInfo.OSType == "linux" {
		startArgs = append(startArgs, "--iptables=false", "--ip6tables=false")
	}

	d := daemon.New(t)
	d.Start(t, startArgs...)
	defer func() {
		if runtime.GOOS == "windows" {
			assert.NilError(t, d.Kill())
			return
		}
		d.Stop(t)
	}()

	engine := d.NewClientT(t)
	info, err := engine.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	extensions, err := extensionclient.New(engine, extensionclient.WithGRPCPoint(greeterpb.ClientPoint))
	assert.NilError(t, err)
	defer func() { assert.NilError(t, extensions.Close()) }()

	greeter, err := extensionclient.Resolve(extensions, greeterv0.Point)
	assert.NilError(t, err)
	resp, err := greeter.Greet(ctx, &greeterv0.HelloRequest{})
	assert.NilError(t, err)
	assert.Equal(t, resp.Message, info.Info.ID)
}

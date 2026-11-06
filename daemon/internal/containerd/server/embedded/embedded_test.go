//go:build (linux || windows) && !no_embedded_containerd

package embedded

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPluginGraphResolves guards the blank-import plugin set: it starts the
// embedded containerd server and fails if any required plugin is missing from
// the registry or its dependency graph cannot be satisfied. Runtime init
// failures unrelated to registration (e.g. requiring root) are tolerated, so
// the test is safe to run unprivileged.
//
// On successful startup, it also verifies that context cancellation shuts
// down the server.
func TestPluginGraphResolves(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d, err := Start(ctx, t.TempDir(), t.TempDir(), "")
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no plugins registered") {
			t.Fatalf("embedded containerd plugin graph is incomplete: %v", err)
		}
		t.Skipf("embedded containerd did not start in this environment: %v", err)
	}

	// Shutdown the embedded containerd server, and wait for it to be stopped.
	cancel()
	if err := d.WaitTimeout(10 * time.Second); err != nil {
		t.Fatalf("embedded containerd did not stop: %v", err)
	}
}

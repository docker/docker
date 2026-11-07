package osl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/ipvs"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestApplyOSTweaks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []SandboxType
	}{
		{name: "load balancer", types: []SandboxType{SandboxTypeLoadBalancer}},
		{name: "ingress", types: []SandboxType{SandboxTypeIngress}},
		{name: "both", types: []SandboxType{SandboxTypeLoadBalancer, SandboxTypeIngress}},
		{name: "ordinary sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Use a disposable namespace as the caller's "host", so the test
			// never changes the real host's IPVS configuration.
			defer netnsutils.SetupTestOSContext(t)()

			// Ensure the IPVS sysctls exist before setting the sentinel values.
			h, err := ipvs.New("")
			assert.NilError(t, err)
			defer h.Close()

			n, err := NewSandbox(filepath.Join(t.TempDir(), "sandbox"), true, false)
			assert.NilError(t, err)
			defer destroyTest(t, n)

			initial := map[string]string{
				"conn_reuse_mode":           "2",
				"expire_nodest_conn":        "0",
				"expire_quiescent_template": "0",
			}
			want := map[string]string{
				"conn_reuse_mode":           "0",
				"expire_nodest_conn":        "1",
				"expire_quiescent_template": "1",
			}
			if len(tc.types) == 0 {
				want = initial
			}

			setInitial := func() {
				for name, value := range initial {
					assert.Check(t, is.Nil(os.WriteFile(filepath.Join("/proc/sys/net/ipv4/vs", name), []byte(value), 0o644)), "setting %s", name)
				}
			}
			check := func(values map[string]string, scope string) {
				for name, value := range values {
					got, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv4/vs", name))
					assert.Check(t, is.Nil(err), "reading %s %s", scope, name)
					assert.Check(t, is.Equal(strings.TrimSpace(string(got)), value), "%s %s", scope, name)
				}
			}

			setInitial()
			assert.NilError(t, n.InvokeFunc(setInitial))

			assert.NilError(t, n.ApplyOSTweaks(tc.types))

			check(initial, "host")
			assert.NilError(t, n.InvokeFunc(func() { check(want, "sandbox") }))
		})
	}
}

func TestApplyOSTweaksMissingNamespace(t *testing.T) {
	n := &Namespace{path: filepath.Join(t.TempDir(), "missing")}
	assert.ErrorContains(t, n.ApplyOSTweaks([]SandboxType{SandboxTypeLoadBalancer}), "failed get network namespace")
	// A sandbox without IPVS needs no namespace switch.
	assert.NilError(t, n.ApplyOSTweaks(nil))
}

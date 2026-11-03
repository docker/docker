//go:build !windows

package libnetwork

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/resolvconf"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func getResolvConfOptions(t *testing.T, rcPath string) []string {
	t.Helper()
	resolv, err := os.ReadFile(rcPath)
	assert.NilError(t, err)
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolv), "")
	assert.NilError(t, err)
	return rc.Options()
}

func TestDNSOptions(t *testing.T) {
	c, err := New(t.Context())
	assert.NilError(t, err)

	hostResolvConfPath := filepath.Join(t.TempDir(), "host-resolv.conf")
	err = os.WriteFile(hostResolvConfPath, []byte("nameserver 192.0.2.53\n"), filePerm)
	assert.NilError(t, err)

	sb, err := c.NewSandbox(t.Context(), "cnt1",
		OptionWriteResolvConf(filepath.Join(t.TempDir(), "resolv.conf")),
		OptionOriginResolvConfPath(hostResolvConfPath),
	)
	assert.NilError(t, err)

	cleanup := func(s *Sandbox) {
		if err := s.Delete(t.Context()); err != nil {
			t.Error(err)
		}
	}

	defer cleanup(sb)
	sb.startResolver(false)

	err = sb.setupDNS()
	assert.NilError(t, err)
	err = sb.rebuildDNS()
	assert.NilError(t, err)
	dnsOptionsList := getResolvConfOptions(t, sb.config.resolvConfPath)
	assert.Check(t, is.Len(dnsOptionsList, 1))
	assert.Check(t, is.Equal("ndots:0", dnsOptionsList[0]))

	sb.config.dnsOptionsList = []string{"ndots:5"}
	err = sb.setupDNS()
	assert.NilError(t, err)
	dnsOptionsList = getResolvConfOptions(t, sb.config.resolvConfPath)
	assert.Check(t, is.Len(dnsOptionsList, 1))
	assert.Check(t, is.Equal("ndots:5", dnsOptionsList[0]))

	err = sb.rebuildDNS()
	assert.NilError(t, err)
	dnsOptionsList = getResolvConfOptions(t, sb.config.resolvConfPath)
	assert.Check(t, is.Len(dnsOptionsList, 1))
	assert.Check(t, is.Equal("ndots:5", dnsOptionsList[0]))

	sb2, err := c.NewSandbox(t.Context(), "cnt2",
		OptionWriteResolvConf(filepath.Join(t.TempDir(), "resolv.conf")),
		OptionOriginResolvConfPath(hostResolvConfPath),
	)
	assert.NilError(t, err)
	defer cleanup(sb2)
	sb2.startResolver(false)

	sb2.config.dnsOptionsList = []string{"ndots:0"}
	err = sb2.setupDNS()
	assert.NilError(t, err)
	err = sb2.rebuildDNS()
	assert.NilError(t, err)
	dnsOptionsList = getResolvConfOptions(t, sb2.config.resolvConfPath)
	assert.Check(t, is.Len(dnsOptionsList, 1))
	assert.Check(t, is.Equal("ndots:0", dnsOptionsList[0]))

	sb2.config.dnsOptionsList = []string{"ndots:foobar"}
	err = sb2.setupDNS()
	assert.NilError(t, err)
	err = sb2.rebuildDNS()
	assert.NilError(t, err)
	dnsOptionsList = getResolvConfOptions(t, sb2.config.resolvConfPath)
	assert.Check(t, is.DeepEqual([]string{"ndots:0"}, dnsOptionsList))

	sb2.config.dnsOptionsList = []string{"ndots:-1"}
	err = sb2.setupDNS()
	assert.NilError(t, err)
	err = sb2.rebuildDNS()
	assert.NilError(t, err)
	dnsOptionsList = getResolvConfOptions(t, sb2.config.resolvConfPath)
	assert.Check(t, is.DeepEqual([]string{"ndots:0"}, dnsOptionsList))
}

// TestSandboxWritesFilesOnlyWhenAsked checks that a sandbox writes a hosts
// file and a resolv.conf only to the paths its creator gives it, including
// when it joins an endpoint, and that it doesn't write them anywhere else.
func TestSandboxWritesFilesOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name              string
		hosts, resolvConf bool
	}{
		{name: "neither"},
		{name: "hosts", hosts: true},
		{name: "resolv.conf", resolvConf: true},
		{name: "both", hosts: true, resolvConf: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t)()
			c, nws := getTestEnv(t, []NetworkOption{NetworkOptionEnableIPv4(true)})

			dir := t.TempDir()
			var opts []SandboxOption
			var want []string
			if tc.hosts {
				opts = append(opts, OptionWriteHostsFile(filepath.Join(dir, "hosts")))
				want = append(want, "hosts")
			}
			if tc.resolvConf {
				opts = append(opts, OptionWriteResolvConf(filepath.Join(dir, "resolv.conf")))
				want = append(want, "resolv.conf", "resolv.conf.hash")
			}
			sb, err := c.NewSandbox(t.Context(), "c1", opts...)
			assert.NilError(t, err)
			defer func() {
				assert.Check(t, sb.Delete(t.Context()))
			}()
			ep, err := nws[0].CreateEndpoint(t.Context(), "ep1")
			assert.NilError(t, err)
			assert.NilError(t, ep.Join(t.Context(), sb))
			assert.Check(t, sb.resolver != nil, "embedded DNS resolver did not start")

			assert.Check(t, is.DeepEqual(dirNames(t, dir), want))
			_, err = os.Stat(filepath.Join("/var/lib/docker/network/files", sb.ID()))
			assert.Check(t, is.ErrorIs(err, fs.ErrNotExist))
		})
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	assert.NilError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestDNSRebuildAfterUpgradeWithStaleHash is a regression test for
// https://github.com/moby/moby/issues/51619.
func TestDNSRebuildAfterUpgradeWithStaleHash(t *testing.T) {
	ctx := t.Context()
	tmpDir := t.TempDir()
	c, err := New(ctx)
	assert.NilError(t, err)

	hostResolvConfPath := filepath.Join(tmpDir, "host-resolv.conf")
	err = os.WriteFile(hostResolvConfPath, []byte("nameserver 192.0.2.53\n"), filePerm)
	assert.NilError(t, err)

	sb, err := c.NewSandbox(ctx, "cnt-upgrade",
		OptionWriteResolvConf(filepath.Join(tmpDir, "cnt-upgrade", "resolv.conf")),
		OptionOriginResolvConfPath(hostResolvConfPath),
	)
	assert.NilError(t, err)
	sb.startResolver(false)

	err = sb.setupDNS()
	assert.NilError(t, err)
	err = sb.rebuildDNS()
	assert.NilError(t, err)

	resolvConfPath := sb.config.resolvConfPath

	// Simulate an upgrade from an Engine version whose rebuildDNS() wrote the
	// container's resolv.conf without updating its hash file: the on-disk file no
	// longer matches the recorded hash. This out-of-sync ("stale") hash is the
	// exact state that regressed external DNS resolution on restart (#51619),
	// where setupDNS/rebuildDNS were skipped and upstream nameservers never got
	// extracted. Without this, both the fixed and regressed code keep the hash in
	// sync within a single run, so the test would pass either way.
	staleHash := digest.FromBytes([]byte("resolv.conf written by a previous Engine version"))
	err = os.WriteFile(sb.config.resolvConfHashFile, []byte(staleHash), filePerm)
	assert.NilError(t, err)

	err = sb.Delete(ctx)
	assert.NilError(t, err)

	sbRestart, err := c.NewSandbox(ctx, "cnt-upgrade",
		OptionWriteResolvConf(resolvConfPath),
		OptionOriginResolvConfPath(hostResolvConfPath),
	)
	assert.NilError(t, err)
	defer func() {
		if err := sbRestart.Delete(ctx); err != nil {
			t.Error(err)
		}
	}()

	err = sbRestart.setupDNS()
	assert.NilError(t, err)
	sbRestart.startResolver(false)

	assert.Check(t, is.DeepEqual(sbRestart.extDNS, []extDNSEntry{{
		IPStr:        "192.0.2.53",
		HostLoopback: true,
	}}, cmpopts.EquateComparable(extDNSEntry{})))
}

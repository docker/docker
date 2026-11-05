package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/sys/reexec"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

const (
	testListenerNoAddrCmdPhase1 = "test-listener-no-addr1"
	testListenerNoAddrCmdPhase2 = "test-listener-no-addr2"
)

type listenerTestResponse struct {
	Err string
}

func initListenerTestPhase1() {
	os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	os.Setenv("LISTEN_FDS", "1")

	// NOTE: We cannot use O_CLOEXEC here because we need the fd to stay open for the child process.
	_, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cmd := reexec.Command(testListenerNoAddrCmdPhase2)
	if err := unix.Exec(cmd.Path, cmd.Args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func initListenerTestPhase2() {
	cfg := &config.Config{
		CommonConfig: config.CommonConfig{
			Hosts: []string{"fd://"},
		},
	}
	_, _, err := loadListeners(cfg, nil)
	var resp listenerTestResponse
	if err != nil {
		resp.Err = err.Error()
	}

	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Test to make sure that the listen specs without an address are handled
// It requires a 2-phase setup due to how socket activation works (which we are using to test).
// It requires LISTEN_FDS and LISTEN_PID to be set in the environment.
//
// LISTEN_PID is used by socket activation to determine if the process is the one that should be activated.
// LISTEN_FDS is used by socket activation to determine how many file descriptors are passed to the process.
//
// We can sort of fake this without using extra processes, but it ends up not
// being a true test because that's not how socket activation is expected to
// work and we'll end up with nil listeners since the test framework has other
// file descriptors open.
//
// This is not currently testing `tcp://` or `unix://` listen specs without an address because those can conflict with the machine running the test.
// This could be worked around by using linux namespaces, however that would require root privileges which unit tests don't typically have.
func TestLoadListenerNoAddr(t *testing.T) {
	cmd := reexec.Command(testListenerNoAddrCmdPhase1)
	stdout := bytes.NewBuffer(nil)
	cmd.Stdout = stdout
	stderr := bytes.NewBuffer(nil)
	cmd.Stderr = stderr

	assert.NilError(t, cmd.Run(), stderr.String())

	var resp listenerTestResponse
	assert.NilError(t, json.NewDecoder(stdout).Decode(&resp))
	assert.Equal(t, resp.Err, "")
}

func TestC8dSnapshotterWithUsernsRemap(t *testing.T) {
	testcases := []struct {
		name   string
		cfg    *config.Config
		expCfg *config.Config
		expErr string
	}{
		{
			name:   "no remap, no snapshotter",
			cfg:    &config.Config{},
			expCfg: &config.Config{},
		},
		{
			name: "userns remap, no explicit containerd-snapshotter feature",
			cfg:  &config.Config{RemappedRoot: "default"},
			expCfg: &config.Config{
				RemappedRoot: "dockremap:dockremap",
				CommonConfig: config.CommonConfig{
					ContainerdNamespace:       "-100000.100000",
					ContainerdPluginNamespace: "-100000.100000",
					Features:                  map[string]bool{"containerd-snapshotter": false},
				},
			},
		},
		{
			name: "userns remap, explicit containerd-snapshotter feature",
			cfg: &config.Config{
				RemappedRoot: "default",
				CommonConfig: config.CommonConfig{Features: map[string]bool{"containerd-snapshotter": true}},
			},
			expCfg: &config.Config{
				RemappedRoot: "dockremap:dockremap",
				CommonConfig: config.CommonConfig{
					ContainerdNamespace:       "-100000.100000",
					ContainerdPluginNamespace: "-100000.100000",
					Features:                  map[string]bool{"containerd-snapshotter": true},
				},
			},
			expErr: "containerd-snapshotter is explicitly enabled, but is not compatible with userns remapping. Please disable userns remapping or containerd-snapshotter",
		},
		{
			name: "userns remap, containerd-migration feature",
			cfg: &config.Config{
				RemappedRoot: "default",
				CommonConfig: config.CommonConfig{Features: map[string]bool{"containerd-migration": true}},
			},
			expCfg: &config.Config{
				RemappedRoot: "dockremap:dockremap",
				CommonConfig: config.CommonConfig{
					ContainerdNamespace:       "-100000.100000",
					ContainerdPluginNamespace: "-100000.100000",
					Features:                  map[string]bool{"containerd-migration": true},
				},
			},
			expErr: "containerd-migration is enabled, but is not compatible with userns remapping. Please disable userns remapping or containerd-migration",
		},
		{
			name: "no remap, explicit containerd-snapshotter feature",
			cfg: &config.Config{
				CommonConfig: config.CommonConfig{Features: map[string]bool{"containerd-snapshotter": true}},
			},
			expCfg: &config.Config{
				CommonConfig: config.CommonConfig{Features: map[string]bool{"containerd-snapshotter": true}},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			err := setPlatformOptions(tc.cfg)
			assert.DeepEqual(t, tc.expCfg, tc.cfg, cmp.AllowUnexported(config.DefaultBridgeConfig{}))
			if tc.expErr != "" {
				assert.Equal(t, tc.expErr, err.Error())
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/containerprofile"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestContainerProfiles(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	ctx := testutil.StartSpan(baseContext, t)

	profilesDir := containerprofile.DefaultDir
	profileName := fmt.Sprintf("moby-test-%d", os.Getpid())
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Skipf("cannot create container profile directory: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(profilesDir, profileName+".json"))
	})
	secureProfile := map[string]any{
		"read-only":  true,
		"pids-limit": 256,
		"init":       true,
	}
	profileJSON, err := json.Marshal(secureProfile)
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(
		filepath.Join(profilesDir, profileName+".json"), profileJSON, 0o644,
	))

	configFile := filepath.Join(t.TempDir(), "daemon.json")
	config := map[string]string{"default-container-profile": profileName}
	configJSON, err := json.Marshal(config)
	assert.NilError(t, err)
	assert.NilError(t, os.WriteFile(configFile, configJSON, 0o644))

	d := daemon.New(t)
	t.Cleanup(func() { d.Stop(t) })
	d.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false", "--config-file", configFile)
	c := d.NewClientT(t)

	tests := []struct {
		name             string
		profile          string
		pidsLimit        *int64
		expectedPids     int64
		expectedReadOnly bool
		expectedInit     *bool
	}{
		{
			name:             "daemon default profile",
			expectedPids:     256,
			expectedReadOnly: true,
			expectedInit:     boolPtr(true),
		},
		{
			name:             "explicit profile",
			profile:          profileName,
			expectedPids:     256,
			expectedReadOnly: true,
			expectedInit:     boolPtr(true),
		},
		{
			name:             "container option overrides profile",
			pidsLimit:        int64Ptr(1024),
			expectedPids:     1024,
			expectedReadOnly: true,
			expectedInit:     boolPtr(true),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			options := client.ContainerCreateOptions{
				Config: &containertypes.Config{
					Image: "busybox:latest",
				},
				Profile: tc.profile,
			}

			if tc.pidsLimit != nil {
				options.HostConfig = &containertypes.HostConfig{
					Resources: containertypes.Resources{
						PidsLimit: tc.pidsLimit,
					},
				}
			}

			created, err := c.ContainerCreate(ctx, options)
			assert.NilError(t, err)
			t.Cleanup(func() {
				_, _ = c.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{
					Force: true,
				})
			})

			inspect, err := c.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.Equal(
				t,
				inspect.Container.HostConfig.ReadonlyRootfs,
				tc.expectedReadOnly,
			)
			assert.Equal(
				t,
				*inspect.Container.HostConfig.PidsLimit,
				tc.expectedPids,
			)
			if tc.expectedInit == nil {
				assert.Assert(t, inspect.Container.HostConfig.Init == nil)
			} else {
				assert.Equal(
					t,
					*inspect.Container.HostConfig.Init,
					*tc.expectedInit,
				)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

package containerprofile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/container"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		invalid  bool
	}{
		{
			name: "valid profile",
			contents: `{
				"read-only": true,
				"cap-add": ["NET_ADMIN"],
				"cap-drop": ["ALL"],
				"security-opt": ["no-new-privileges"],
				"pids-limit": 256,
				"init": true,
				"tty": true,
				"user": "1000:1000",
				"working-dir": "/work",
				"stop-timeout": 30,
				"memory": 536870912,
				"cpus": 2000000000,
				"cpu-quota": 100000,
				"cpu-period": 1000000,
				"shm-size": 67108864,
				"tmpfs": {"/tmp": "rw,noexec"},
				"sysctls": {"net.ipv4.ip_forward": "1"},
				"ulimits": [{"Name": "nofile", "Soft": 1024, "Hard": 2048}],
				"dns": ["1.1.1.1"],
				"dns-search": ["example.com"],
				"dns-options": ["ndots:1"]
			}`,
		},
		{
			name:     "unknown field",
			contents: `{"privileged": true}`,
			invalid:  true,
		},
		{
			name:     "malformed JSON",
			contents: `{`,
			invalid:  true,
		},
		{
			name:     "multiple JSON values",
			contents: `{} {}`,
			invalid:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			assert.NilError(t, os.WriteFile(
				filepath.Join(dir, "profile.json"), []byte(tc.contents), 0o644,
			))

			profile, err := Load(dir, "profile")
			if tc.invalid {
				assert.Assert(t, errors.Is(err, ErrInvalid))
				return
			}

			assert.NilError(t, err)
			assert.Assert(t, cmp.Equal(*profile.ReadOnly, true))
			assert.DeepEqual(t, profile.CapAdd, []string{"NET_ADMIN"})
			assert.DeepEqual(t, profile.CapDrop, []string{"ALL"})
			assert.DeepEqual(t, profile.SecurityOpt, []string{"no-new-privileges"})
			assert.Equal(t, *profile.PidsLimit, int64(256))
			assert.Equal(t, *profile.Init, true)
			assert.Equal(t, *profile.Tty, true)
			assert.Equal(t, *profile.User, "1000:1000")
			assert.Equal(t, *profile.WorkingDir, "/work")
			assert.Equal(t, *profile.StopTimeout, 30)
			assert.Equal(t, *profile.Memory, int64(536870912))
			assert.Equal(t, *profile.NanoCPUs, int64(2000000000))
			assert.Equal(t, *profile.CPUQuota, int64(100000))
			assert.Equal(t, *profile.CPUPeriod, int64(1000000))
			assert.Equal(t, *profile.ShmSize, int64(67108864))
			assert.DeepEqual(t, profile.Tmpfs, map[string]string{"/tmp": "rw,noexec"})
			assert.DeepEqual(t, profile.Sysctls, map[string]string{"net.ipv4.ip_forward": "1"})
			assert.Equal(t, profile.Ulimits[0].Name, "nofile")
			assert.Equal(t, profile.DNS[0].String(), "1.1.1.1")
			assert.DeepEqual(t, profile.DNSSearch, []string{"example.com"})
			assert.DeepEqual(t, profile.DNSOptions, []string{"ndots:1"})
		})
	}
}

func TestApply(t *testing.T) {
	readOnly := true
	init := true
	tty := true
	user := "1000:1000"
	workingDir := "/work"
	stopTimeout := 30
	pidsLimit := int64(256)
	memory := int64(512 * 1024 * 1024)

	tests := []struct {
		name    string
		profile Profile
		config  *container.Config
		host    *container.HostConfig
		check   func(*testing.T, *container.Config, *container.HostConfig)
	}{
		{
			name: "applies all configured fields",
			profile: Profile{
				ReadOnly: &readOnly, Init: &init, Tty: &tty, User: &user,
				WorkingDir: &workingDir, StopTimeout: &stopTimeout, PidsLimit: &pidsLimit,
				Memory: &memory, CapDrop: []string{"ALL"},
			},
			config: &container.Config{}, host: &container.HostConfig{},
			check: func(t *testing.T, config *container.Config, host *container.HostConfig) {
				assert.Assert(t, config.Tty)
				assert.Equal(t, config.User, user)
				assert.Equal(t, config.WorkingDir, workingDir)
				assert.Equal(t, *config.StopTimeout, stopTimeout)
				assert.Assert(t, host.ReadonlyRootfs)
				assert.Equal(t, *host.Init, init)
				assert.Equal(t, *host.PidsLimit, pidsLimit)
				assert.Equal(t, host.Memory, memory)
				assert.DeepEqual(t, host.CapDrop, []string{"ALL"})
			},
		},
		{
			name:    "leaves omitted fields unchanged",
			profile: Profile{},
			config:  &container.Config{User: "docker", Tty: true},
			host: &container.HostConfig{
				ReadonlyRootfs: true,
				Resources:      container.Resources{PidsLimit: &pidsLimit},
			},
			check: func(t *testing.T, config *container.Config, host *container.HostConfig) {
				assert.Equal(t, config.User, "docker")
				assert.Assert(t, config.Tty)
				assert.Assert(t, host.ReadonlyRootfs)
				assert.Equal(t, *host.PidsLimit, pidsLimit)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Apply(tc.profile, tc.config, tc.host)
			tc.check(t, tc.config, tc.host)
		})
	}
}

func TestLoadRejectsInvalidName(t *testing.T) {
	_, err := Load(t.TempDir(), "../profile")
	assert.Assert(t, errors.Is(err, ErrInvalidName))
}

func TestLoadMissingProfile(t *testing.T) {
	_, err := Load(t.TempDir(), "missing")
	assert.Assert(t, errors.Is(err, ErrLoad))
}

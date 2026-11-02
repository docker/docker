//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
)

func TestExecSetPlatformOptPreservesUmaskWhenResolvingUser(t *testing.T) {
	root := t.TempDir()
	assert.NilError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("test:x:1234:5678::/:/bin/sh\n"), 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(root, "etc/group"), []byte("test:x:5678:\n"), 0o644))

	c := &container.Container{
		BaseFS: root,
		SecurityOptions: container.SecurityOptions{
			AppArmorProfile: unconfinedAppArmorProfile,
		},
		HostConfig: &containertypes.HostConfig{},
	}
	ec := &container.ExecConfig{Container: c, User: "1234:5678"}
	umask := uint32(0o027)
	p := &specs.Process{User: specs.User{Umask: &umask}}
	cfg := &configStore{}

	err := (&Daemon{}).execSetPlatformOpt(t.Context(), &cfg.Config, ec, p)
	assert.NilError(t, err)
	assert.Equal(t, p.User.UID, uint32(1234))
	assert.Equal(t, p.User.GID, uint32(5678))
	assert.Assert(t, p.User.Umask != nil)
	assert.Equal(t, *p.User.Umask, umask)
}

func TestExecSetPlatformOptAppArmor(t *testing.T) {
	appArmorEnabled := appArmorSupported()

	tests := []struct {
		doc             string
		privileged      bool
		appArmorProfile string
		expectedProfile string
	}{
		{
			doc:             "default options",
			expectedProfile: defaultAppArmorProfile,
		},
		{
			doc:             "custom profile",
			appArmorProfile: "my-custom-profile",
			expectedProfile: "my-custom-profile",
		},
		{
			doc:             "privileged container",
			privileged:      true,
			expectedProfile: unconfinedAppArmorProfile,
		},
		{
			doc:             "privileged container, custom profile",
			privileged:      true,
			appArmorProfile: "my-custom-profile",
			expectedProfile: "my-custom-profile",
			// FIXME: execSetPlatformOpts prefers custom profiles over "privileged",
			//        which looks like a bug (--privileged on the container should
			//        disable apparmor, seccomp, and selinux); see the code at:
			//        https://github.com/moby/moby/blob/46cdcd206c56172b95ba5c77b827a722dab426c5/daemon/exec_linux.go#L32-L40
			// expectedProfile: unconfinedAppArmorProfile,
		},
	}

	cfg := &configStore{}
	d := &Daemon{}
	d.configStore.Store(cfg)

	// Currently, `docker exec --privileged` inherits the Privileged configuration
	// of the container, and does not disable AppArmor.
	// See https://github.com/moby/moby/pull/31773#discussion_r105586900
	//
	// This behavior may change in future, but to verify the current behavior,
	// we run the test both with "exec" and "exec --privileged", which should
	// both give the same result.
	for _, execPrivileged := range []bool{false, true} {
		for _, tc := range tests {
			doc := tc.doc
			if !appArmorEnabled {
				// no profile should be set if the host does not support AppArmor
				doc += " (apparmor disabled)"
				tc.appArmorProfile = ""
				tc.expectedProfile = ""
			}
			if execPrivileged {
				doc += " (exec privileged)"
			}
			t.Run(doc, func(t *testing.T) {
				c := &container.Container{
					SecurityOptions: container.SecurityOptions{AppArmorProfile: tc.appArmorProfile},
					HostConfig: &containertypes.HostConfig{
						Privileged: tc.privileged,
					},
				}
				ec := &container.ExecConfig{Container: c, Privileged: execPrivileged}
				p := &specs.Process{}

				err := d.execSetPlatformOpt(t.Context(), &cfg.Config, ec, p)
				assert.NilError(t, err)
				assert.Equal(t, p.ApparmorProfile, tc.expectedProfile)
			})
		}
	}
}

package remote

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
)

func usernsSpec(uid, gid uint32) *specs.Spec {
	return &specs.Spec{
		Process: &specs.Process{
			User: specs.User{UID: uid, GID: gid},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.UserNamespace},
			},
			UIDMappings: []specs.LinuxIDMapping{
				{ContainerID: 0, HostID: 100000, Size: 65536},
			},
			GIDMappings: []specs.LinuxIDMapping{
				{ContainerID: 0, HostID: 100000, Size: 65536},
			},
		},
	}
}

func TestGetIOOwner(t *testing.T) {
	tests := []struct {
		name    string
		spec    *specs.Spec
		wantUID int
		wantGID int
	}{
		{
			name:    "NoUserNamespace",
			spec:    &specs.Spec{Process: &specs.Process{User: specs.User{UID: 65532}}},
			wantUID: 0,
			wantGID: 0,
		},
		{
			name:    "UsernsNonRootUser",
			spec:    usernsSpec(65532, 0),
			wantUID: 165532,
			wantGID: 100000,
		},
		{
			name:    "UsernsRootUser",
			spec:    usernsSpec(0, 0),
			wantUID: 100000,
			wantGID: 100000,
		},
		{
			name: "UsernsNoProcess",
			spec: &specs.Spec{
				Linux: &specs.Linux{
					Namespaces: []specs.LinuxNamespace{
						{Type: specs.UserNamespace},
					},
					UIDMappings: []specs.LinuxIDMapping{
						{ContainerID: 0, HostID: 100000, Size: 65536},
					},
					GIDMappings: []specs.LinuxIDMapping{
						{ContainerID: 0, HostID: 100000, Size: 65536},
					},
				},
			},
			wantUID: 100000,
			wantGID: 100000,
		},
		{
			name: "UsernsSplitMappings",
			spec: &specs.Spec{
				Process: &specs.Process{
					User: specs.User{UID: 1000, GID: 100},
				},
				Linux: &specs.Linux{
					Namespaces: []specs.LinuxNamespace{
						{Type: specs.UserNamespace},
					},
					UIDMappings: []specs.LinuxIDMapping{
						{ContainerID: 0, HostID: 100000, Size: 1},
						{ContainerID: 1000, HostID: 200000, Size: 1},
					},
					GIDMappings: []specs.LinuxIDMapping{
						{ContainerID: 0, HostID: 100000, Size: 1},
						{ContainerID: 100, HostID: 300000, Size: 1},
					},
				},
			},
			wantUID: 200000,
			wantGID: 300000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid := getIOOwner(tc.spec)
			assert.Equal(t, uid, tc.wantUID)
			assert.Equal(t, gid, tc.wantGID)
		})
	}
}

// getSpecUser must keep mapping container UID 0: it owns the bundle
// directory (WithBundle). The stdio pipes, on the other hand, must be
// owned by the user the container process runs as (getIOOwner).
func TestGetSpecUserStillMapsRoot(t *testing.T) {
	spec := usernsSpec(65532, 0)
	uid, gid := getSpecUser(spec)
	assert.Equal(t, uid, 100000)
	assert.Equal(t, gid, 100000)

	// And the IO owner for the same spec is the process user, not root.
	ioUID, ioGID := getIOOwner(spec)
	assert.Equal(t, ioUID, 165532)
	assert.Equal(t, ioGID, 100000)
}

package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/idtools"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestChownFlagParsing(t *testing.T) {
	testFiles := map[string]string{
		"passwd": `root:x:0:0::/bin:/bin/false
bin:x:1:1::/bin:/bin/false
wwwwww:x:21:33::/bin:/bin/false
unicorn:x:1001:1002::/bin:/bin/false
		`,
		"group": `root:x:0:
bin:x:1:
wwwwww:x:33:
unicorn:x:1002:
somegrp:x:5555:
othergrp:x:6666:
		`,
	}
	// test mappings for validating use of maps
	idMaps := []idtools.IDMap{
		{
			ContainerID: 0,
			HostID:      100000,
			Size:        65536,
		},
	}
	remapped := idtools.NewIDMappingsFromMaps(idMaps, idMaps)
	unmapped := &idtools.IdentityMapping{}

	contextDir, cleanup := createTestTempDir(t, "", "builder-chown-parse-test")
	defer cleanup()

	if err := os.Mkdir(filepath.Join(contextDir, "etc"), 0755); err != nil {
		t.Fatalf("error creating test directory: %v", err)
	}

	for filename, content := range testFiles {
		createTestTempFile(t, filepath.Join(contextDir, "etc"), filename, content, 0644)
	}

	platformLinux, err := platforms.Parse("linux")
	if err != nil {
		t.Fatalf("error parsing platform 'linux': %v", err)
	}

	// positive tests
	for _, testcase := range []struct {
		builder   *Builder
		name      string
		chownStr  string
		idMapping *idtools.IdentityMapping
		state     *dispatchState
		expected  idtools.Identity
	}{
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UIDNoMap",
			chownStr:  "1",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 1, GID: 1},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UIDGIDNoMap",
			chownStr:  "0:1",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 0, GID: 1},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UIDWithMap",
			chownStr:  "0",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 100000, GID: 100000},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UIDGIDWithMap",
			chownStr:  "1:33",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 100001, GID: 100033},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UserNoMap",
			chownStr:  "bin:5555",
			idMapping: unmapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 1, GID: 5555},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "GroupWithMap",
			chownStr:  "0:unicorn",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 100000, GID: 101002},
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UserOnlyWithMap",
			chownStr:  "unicorn",
			idMapping: remapped,
			state:     &dispatchState{},
			expected:  idtools.Identity{UID: 101001, GID: 101002},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			idPair, err := parseChownFlag(testcase.builder, testcase.state, testcase.chownStr, contextDir, testcase.idMapping)
			assert.NilError(t, err, "Failed to parse chown flag: %q", testcase.chownStr)
			assert.Check(t, is.DeepEqual(testcase.expected, idPair), "chown flag mapping failure")
		})
	}

	// error tests
	for _, testcase := range []struct {
		builder   *Builder
		name      string
		chownStr  string
		idMapping *idtools.IdentityMapping
		state     *dispatchState
		descr     string
	}{
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "BadChownFlagFormat",
			chownStr:  "bob:1:555",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "invalid chown string format: bob:1:555",
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "UserNoExist",
			chownStr:  "bob",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "can't find uid for user bob: no such user: bob",
		},
		{
			builder:   &Builder{options: &types.ImageBuildOptions{Platform: &platformLinux}},
			name:      "GroupNoExist",
			chownStr:  "root:bob",
			idMapping: unmapped,
			state:     &dispatchState{},
			descr:     "can't find gid for group bob: no such group: bob",
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			_, err := parseChownFlag(testcase.builder, testcase.state, testcase.chownStr, contextDir, testcase.idMapping)
			assert.Check(t, is.Error(err, testcase.descr), "Expected error string doesn't match")
		})
	}
}

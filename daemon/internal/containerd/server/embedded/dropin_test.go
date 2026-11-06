//go:build (linux || windows) && !no_embedded_containerd

package embedded

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/plugin"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestConfigDropInsConfigurePlugins(t *testing.T) {
	t.Parallel()

	// Brackets must not be interpreted as a glob pattern.
	dir := filepath.Join(t.TempDir(), "docker[prod]")
	err := os.Mkdir(dir, 0o755)
	assert.NilError(t, err)
	err = os.WriteFile(filepath.Join(dir, "10-custom.toml"), []byte(`
version = 4

[plugins."io.moby.test.v1.configured"]
  value = "from-drop-in"
`), 0o644)
	assert.NilError(t, err)

	containerdConfig, err := loadConfigDropIns(t.Context(), dir)
	assert.NilError(t, err)

	type testConfig struct {
		Value string `toml:"value"`
	}
	var got any
	cfg := &serverConfig{
		root:       filepath.Join(t.TempDir(), "root"),
		state:      filepath.Join(t.TempDir(), "state"),
		containerd: containerdConfig,
	}
	server, err := newServerWithRegistrations(t.Context(), cfg, []plugin.Registration{{
		Type:   "io.moby.test.v1",
		ID:     "configured",
		Config: &testConfig{Value: "default"},
		InitFn: func(initContext *plugin.InitContext) (any, error) {
			got = initContext.Config
			return struct{}{}, nil
		},
	}})
	assert.NilError(t, err)
	server.Stop()
	assert.Check(t, is.DeepEqual(got, &testConfig{Value: "from-drop-in"}))
}

func TestConfigDropInsRequireMissingPlugin(t *testing.T) {
	t.Parallel()

	cfg := &serverConfig{
		root:  filepath.Join(t.TempDir(), "root"),
		state: filepath.Join(t.TempDir(), "state"),
		containerd: srvconfig.Config{
			RequiredPlugins: []string{"io.moby.test.v1.missing"},
		},
	}
	server, err := newServerWithRegistrations(t.Context(), cfg, nil)
	assert.Check(t, is.Nil(server))
	assert.Check(t, is.ErrorContains(err, "required plugins not loaded: io.moby.test.v1.missing"))
}

func TestConfigDropInsRejectInvalidFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		expErr  string
	}{
		{
			name: "imports",
			content: `
version = 4
imports = ["nested.conf"]
`,
			expErr: "must not contain imports",
		},
		{
			name:    "legacy version",
			content: `version = 3`,
			expErr:  "must set version 4 or later",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			dropInFile := filepath.Join(dir, "10-custom.toml")
			err := os.WriteFile(dropInFile, []byte(tc.content), 0o644)
			assert.NilError(t, err)

			_, err = loadConfigDropIns(t.Context(), dir)
			assert.ErrorContains(t, err, tc.expErr)
			assert.ErrorContains(t, err, strconv.Quote(dropInFile))
		})
	}
}

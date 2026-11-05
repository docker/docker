package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSpecPlugin(t *testing.T) {
	tmpdir := t.TempDir()
	r := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	cases := []struct {
		path string
		name string
		addr string
		fail bool
	}{
		// TODO Windows: Factor out the unix:// variants.
		{filepath.Join(tmpdir, "echo.spec"), "echo", "unix://var/lib/docker/plugins/echo.sock", false},
		{filepath.Join(tmpdir, "echo", "echo.spec"), "echo", "unix://var/lib/docker/plugins/echo.sock", false},
		{filepath.Join(tmpdir, "foo.spec"), "foo", "tcp://localhost:8080", false},
		{filepath.Join(tmpdir, "foo", "foo.spec"), "foo", "tcp://localhost:8080", false},
		{filepath.Join(tmpdir, "bar.spec"), "bar", "localhost:8080", true}, // unknown transport
	}

	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.path, []byte(c.addr), 0o644); err != nil {
			t.Fatal(err)
		}

		p, err := r.Plugin(c.name)
		if c.fail && err == nil {
			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		if p.name != c.name {
			t.Fatalf("Expected plugin `%s`, got %s\n", c.name, p.name)
		}

		if p.Addr != c.addr {
			t.Fatalf("Expected plugin addr `%s`, got %s\n", c.addr, p.Addr)
		}

		if !p.TLSConfig.InsecureSkipVerify {
			t.Fatalf("Expected TLS verification to be skipped")
		}
	}
}

func TestFileJSONSpecPlugin(t *testing.T) {
	tmpdir := t.TempDir()
	r := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	p := filepath.Join(tmpdir, "example.json")
	spec := `{
  "Name": "plugin-example",
  "Addr": "https://example.com/docker/plugin",
  "TLSConfig": {
    "CAFile": "/usr/shared/docker/certs/example-ca.pem",
    "CertFile": "/usr/shared/docker/certs/example-cert.pem",
    "KeyFile": "/usr/shared/docker/certs/example-key.pem"
	}
}`

	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	plugin, err := r.Plugin("example")
	if err != nil {
		t.Fatal(err)
	}

	if expected, actual := "example", plugin.name; expected != actual {
		t.Fatalf("Expected plugin %q, got %s\n", expected, actual)
	}

	if plugin.Addr != "https://example.com/docker/plugin" {
		t.Fatalf("Expected plugin addr `https://example.com/docker/plugin`, got %s\n", plugin.Addr)
	}

	if plugin.TLSConfig.CAFile != "/usr/shared/docker/certs/example-ca.pem" {
		t.Fatalf("Expected plugin CA `/usr/shared/docker/certs/example-ca.pem`, got %s\n", plugin.TLSConfig.CAFile)
	}

	if plugin.TLSConfig.CertFile != "/usr/shared/docker/certs/example-cert.pem" {
		t.Fatalf("Expected plugin Certificate `/usr/shared/docker/certs/example-cert.pem`, got %s\n", plugin.TLSConfig.CertFile)
	}

	if plugin.TLSConfig.KeyFile != "/usr/shared/docker/certs/example-key.pem" {
		t.Fatalf("Expected plugin Key `/usr/shared/docker/certs/example-key.pem`, got %s\n", plugin.TLSConfig.KeyFile)
	}
}

func TestPluginRejectsTraversalName(t *testing.T) {
	// Lay out a plugin directory with a sentinel spec one level above it. A name
	// that escapes the plugin directory via ".." resolves to the sentinel, so a
	// successful lookup of a crafted name would prove a directory traversal.
	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins")
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evil.spec"), []byte("tcp://attacker:9999"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := LocalRegistry{
		socketsPath: pluginDir,
		specsPaths:  []string{pluginDir},
	}

	for _, tc := range []struct {
		name   string
		plugin string
	}{
		{name: "empty", plugin: ""},
		{name: "dot", plugin: "."},
		{name: "dotdot", plugin: ".."},
		{name: "parent", plugin: "../evil"},
		{name: "grandparent", plugin: "../../evil"},
		{name: "embedded traversal", plugin: "foo/../../evil"},
		{name: "native separator", plugin: filepath.Join("..", "evil")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := r.Plugin(tc.plugin)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound for name %q, got plugin=%v, err=%v", tc.plugin, p, err)
			}
			if p != nil {
				t.Fatalf("expected no plugin for name %q, got %v", tc.plugin, p)
			}
		})
	}

	// A legitimate, single-element name must still resolve, to guard against
	// over-rejection.
	if err := os.WriteFile(filepath.Join(pluginDir, "echo.spec"), []byte("tcp://localhost:8080"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Plugin("echo"); err != nil {
		t.Fatalf("expected legitimate plugin name to resolve, got %v", err)
	}
}

func TestFileJSONSpecPluginWithoutTLSConfig(t *testing.T) {
	tmpdir := t.TempDir()
	r := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	p := filepath.Join(tmpdir, "example.json")
	spec := `{
  "Name": "plugin-example",
  "Addr": "https://example.com/docker/plugin"
}`

	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	plugin, err := r.Plugin("example")
	if err != nil {
		t.Fatal(err)
	}

	if expected, actual := "example", plugin.name; expected != actual {
		t.Fatalf("Expected plugin %q, got %s\n", expected, actual)
	}

	if plugin.Addr != "https://example.com/docker/plugin" {
		t.Fatalf("Expected plugin addr `https://example.com/docker/plugin`, got %s\n", plugin.Addr)
	}

	if plugin.TLSConfig != nil {
		t.Fatalf("Expected plugin TLSConfig nil, got %v\n", plugin.TLSConfig)
	}
}

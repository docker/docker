//go:build !windows

package plugins

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestLocalSocket(t *testing.T) {
	// TODO Windows: Enable a similar version for Windows named pipes
	tmpdir := t.TempDir()
	r := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	cases := []string{
		filepath.Join(tmpdir, "echo.sock"),
		filepath.Join(tmpdir, "echo", "echo.sock"),
	}

	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c), 0o755); err != nil {
			t.Fatal(err)
		}

		l, err := net.Listen("unix", c)
		if err != nil {
			t.Fatal(err)
		}

		p, err := r.Plugin("echo")
		if err != nil {
			t.Fatal(err)
		}

		pp, err := r.Plugin("echo")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p, pp) { //nolint:govet // ignore 'deepequalerrors: avoid using reflect.DeepEqual with errors', which is triggered by Plugin having a activateErr field.
			t.Fatalf("Expected %v, was %v\n", p, pp)
		}

		if p.name != "echo" {
			t.Fatalf("Expected plugin `echo`, got %s\n", p.name)
		}

		addr := "unix://" + c
		if p.Addr != addr {
			t.Fatalf("Expected plugin addr `%s`, got %s\n", addr, p.Addr)
		}
		if !p.TLSConfig.InsecureSkipVerify {
			t.Fatalf("Expected TLS verification to be skipped")
		}
		l.Close()
	}
}

func TestScan(t *testing.T) {
	tmpdir := t.TempDir()
	r := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	pluginNames, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if pluginNames != nil {
		t.Fatal("Plugin names should be empty.")
	}

	path := filepath.Join(tmpdir, "echo.spec")
	addr := "unix://var/lib/docker/plugins/echo.sock"
	name := "echo"

	err = os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(addr), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	p, err := r.Plugin(name)
	assert.NilError(t, err)

	pluginNamesNotEmpty, err := r.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(pluginNamesNotEmpty) != 1 {
		t.Fatalf("expected 1 plugin entry: %v", pluginNamesNotEmpty)
	}
	if p.Name() != pluginNamesNotEmpty[0] {
		t.Fatalf("Unable to scan plugin with name %s", p.name)
	}
}

func TestScanNotPlugins(t *testing.T) {
	tmpdir := t.TempDir()
	localRegistry := LocalRegistry{
		socketsPath: tmpdir,
		specsPaths:  []string{tmpdir},
	}

	// not that `Setup()` above sets the sockets path and spec path dirs, which
	// `Scan()` uses to find plugins to the returned `tmpdir`

	notPlugin := filepath.Join(tmpdir, "not-a-plugin")
	if err := os.MkdirAll(notPlugin, 0o700); err != nil {
		t.Fatal(err)
	}

	// this is named differently than the dir it's in, so the scanner should ignore it
	l, err := net.Listen("unix", filepath.Join(notPlugin, "foo.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// same let's test a spec path
	f, err := os.Create(filepath.Join(notPlugin, "foo.spec"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	names, err := localRegistry.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no plugins, got %v", names)
	}

	// Just as a sanity check, let's make an entry that the scanner should read
	f, err = os.Create(filepath.Join(notPlugin, "not-a-plugin.spec"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	names, err = localRegistry.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 entry in result: %v", names)
	}
	if names[0] != "not-a-plugin" {
		t.Fatalf("expected plugin named `not-a-plugin`, got: %s", names[0])
	}
}

func TestSpecsPathsRootless(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("ROOTLESSKIT_STATE_DIR", filepath.Join(tmpdir, "rootlesskit"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpdir, "config"))
	t.Setenv("HOME", filepath.Join(tmpdir, "home"))

	expected := []string{
		filepath.Join(tmpdir, "config", "docker", "plugins"),
		filepath.Join(tmpdir, "home", ".local", "lib", "docker", "plugins"),
	}
	assert.Check(t, is.DeepEqual(SpecsPaths(), expected))
}

func TestScanRootless(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("ROOTLESSKIT_STATE_DIR", filepath.Join(tmpdir, "rootlesskit"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmpdir, "runtime"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpdir, "config"))
	t.Setenv("HOME", filepath.Join(tmpdir, "home"))

	socketsPath := filepath.Join(tmpdir, "runtime", "docker", "plugins")
	assert.NilError(t, os.MkdirAll(socketsPath, 0o755))

	l, err := net.Listen("unix", filepath.Join(socketsPath, "echo.sock"))
	assert.NilError(t, err)
	defer l.Close()

	r := NewLocalRegistry()
	names, err := r.Scan()
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names, []string{"echo"}))
}

// +build windows

package archive // import "github.com/docker/docker/pkg/archive"

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileWithInvalidDest(t *testing.T) {
	// TODO Windows: This is currently failing. Not sure what has
	// recently changed in CopyWithTar as used to pass. Further investigation
	// is required.
	t.Skip("Currently fails")
	folder, err := ioutil.TempDir("", "docker-archive-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(folder)
	dest := "c:dest"
	srcFolder := filepath.Join(folder, "src")
	src := filepath.Join(folder, "src", "src")
	err = os.MkdirAll(srcFolder, 0740)
	if err != nil {
		t.Fatal(err)
	}
	ioutil.WriteFile(src, []byte("content"), 0777)
	err = defaultCopyWithTar(src, dest)
	if err == nil {
		t.Fatalf("archiver.CopyWithTar should throw an error on invalid dest.")
	}
}

func TestCanonicalTarNameForPath(t *testing.T) {
	cases := []struct {
		in, expected string
	}{
		{"foo", "foo"},
		{"foo/bar", "foo/bar"},
		{`foo\bar`, "foo/bar"},
	}
	for _, v := range cases {
		if CanonicalTarNameForPath(v.in) != v.expected {
			t.Fatalf("wrong canonical tar name. expected:%s got:%s", v.expected, CanonicalTarNameForPath(v.in))
		}
	}
}

func TestCanonicalTarName(t *testing.T) {
	cases := []struct {
		in       string
		isDir    bool
		expected string
	}{
		{"foo", false, "foo"},
		{"foo", true, "foo/"},
		{`foo\bar`, false, "foo/bar"},
		{`foo\bar`, true, "foo/bar/"},
	}
	for _, v := range cases {
		if canonicalTarName(v.in, v.isDir) != v.expected {
			t.Fatalf("wrong canonical tar name. expected:%s got:%s", v.expected, canonicalTarName(v.in, v.isDir))
		}
	}
}

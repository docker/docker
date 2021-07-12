package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

type mockFiProvider struct{}

func (mockFiProvider) fileInfo(path string) (exists, isDir bool, err error) {
	dirs := map[string]struct{}{
		`c:\`:                    {},
		`c:\windows\`:            {},
		`c:\windows`:             {},
		`c:\program files`:       {},
		`c:\Windows`:             {},
		`c:\Program Files (x86)`: {},
		`\\?\c:\windows\`:        {},
	}
	files := map[string]struct{}{
		`c:\windows\system32\ntdll.dll`: {},
	}
	if _, ok := dirs[path]; ok {
		return true, true, nil
	}
	if _, ok := files[path]; ok {
		return true, false, nil
	}
	return false, false, nil
}

// always returns the configured error
// this is used to test error handling
type mockFiProviderWithError struct{ err error }

func (m mockFiProviderWithError) fileInfo(path string) (bool, bool, error) {
	return false, false, m.err
}

func TestParseMountSpec(t *testing.T) {
	testDir, err := ioutil.TempDir("", "test-mount-config")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testDir)
	parser := NewParser()
	cases := []struct {
		input    mount.Mount
		expected MountPoint
	}{
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, RW: true, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir + string(os.PathSeparator), Target: testDestinationPath, ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath + string(os.PathSeparator), ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath}, MountPoint{Type: mount.TypeVolume, Destination: testDestinationPath, RW: true, CopyData: parser.DefaultCopyMode()}},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath + string(os.PathSeparator)}, MountPoint{Type: mount.TypeVolume, Destination: testDestinationPath, RW: true, CopyData: parser.DefaultCopyMode()}},
	}

	for i, c := range cases {
		t.Logf("case %d", i)
		mp, err := parser.ParseMountSpec(c.input)
		if err != nil {
			t.Error(err)
		}

		if c.expected.Type != mp.Type {
			t.Errorf("Expected mount types to match. Expected: '%s', Actual: '%s'", c.expected.Type, mp.Type)
		}
		if c.expected.Destination != mp.Destination {
			t.Errorf("Expected mount destination to match. Expected: '%s', Actual: '%s'", c.expected.Destination, mp.Destination)
		}
		if c.expected.Source != mp.Source {
			t.Errorf("Expected mount source to match. Expected: '%s', Actual: '%s'", c.expected.Source, mp.Source)
		}
		if c.expected.RW != mp.RW {
			t.Errorf("Expected mount writable to match. Expected: '%v', Actual: '%v'", c.expected.RW, mp.RW)
		}
		if c.expected.Propagation != mp.Propagation {
			t.Errorf("Expected mount propagation to match. Expected: '%v', Actual: '%s'", c.expected.Propagation, mp.Propagation)
		}
		if c.expected.Driver != mp.Driver {
			t.Errorf("Expected mount driver to match. Expected: '%v', Actual: '%s'", c.expected.Driver, mp.Driver)
		}
		if c.expected.CopyData != mp.CopyData {
			t.Errorf("Expected mount copy data to match. Expected: '%v', Actual: '%v'", c.expected.CopyData, mp.CopyData)
		}
	}

}

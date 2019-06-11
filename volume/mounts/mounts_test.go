package mounts

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/volume"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/fs"
)

func TestSetup(t *testing.T) {
	volDir := fs.NewDir(t, "volume-test-mount-subpath",
		fs.WithFile("foo", ""),
		fs.WithDir("bar"),
	)
	defer volDir.Remove()
	fakeVol := &fakeVolume{path: volDir.Path()}

	cases := []struct {
		name         string
		subPath      string
		volume       volume.Volume
		fail         bool
		expectedErr  string
		expectedPath string
	}{
		{
			name:        "non-existent-sub-path",
			subPath:     "NON-EXISTENT",
			volume:      fakeVol,
			fail:        true,
			expectedErr: "directory NON-EXISTENT does not exist under volume path",
		},
		{
			name:        "non-dir-sub-path",
			subPath:     "foo",
			volume:      fakeVol,
			fail:        true,
			expectedErr: "is not a directory",
		},
		{
			name:        "out-of-scope-sub-path",
			subPath:     "..",
			volume:      fakeVol,
			fail:        true,
			expectedErr: "outside of the volume root",
		},
		{
			name:         "test-mount",
			volume:       fakeVol,
			expectedPath: volDir.Path(),
		},
		{
			name:         "test-mount-subpath",
			subPath:      "bar",
			volume:       fakeVol,
			expectedPath: filepath.Join(volDir.Path(), "bar"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := MountPoint{
				Volume:  tc.volume,
				SubPath: tc.subPath,
			}
			path, err := m.Setup("", idtools.Identity{}, nil)
			if tc.fail {
				assert.Check(t, is.ErrorContains(err, tc.expectedErr))
				return
			}
			assert.NilError(t, err)
			assert.Assert(t, is.Equal(tc.expectedPath, path))
		})
	}
}

func TestPath(t *testing.T) {
	volDir := fs.NewDir(t, "volume-test-mount-subpath",
		fs.WithFile("foo", ""),
		fs.WithDir("bar"),
	)
	defer volDir.Remove()
	fakeVol := &fakeVolume{path: volDir.Path()}

	cases := []struct {
		name         string
		volume       volume.Volume
		subPath      string
		expectedPath string
	}{
		{
			name:         "test-mount-path",
			volume:       fakeVol,
			expectedPath: volDir.Path(),
		},
		{
			name:         "test-mount-subpath",
			volume:       fakeVol,
			subPath:      "bar",
			expectedPath: filepath.Join(volDir.Path(), "bar"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := MountPoint{
				Volume:  tc.volume,
				SubPath: tc.subPath,
			}
			_, err := m.Setup("", idtools.Identity{}, nil)
			assert.NilError(t, err)
			assert.Assert(t, is.Equal(tc.expectedPath, m.Path()))
		})
	}
}

type fakeVolume struct {
	path string
}

func (f *fakeVolume) Name() string {
	return "fake_volume"
}

func (f *fakeVolume) DriverName() string {
	return "fake_driver"
}

func (f *fakeVolume) Path() string {
	return f.path
}

func (f *fakeVolume) Mount(id string) (string, error) {
	return f.path, nil
}

func (f *fakeVolume) Unmount(id string) error {
	return nil
}

func (f *fakeVolume) CreatedAt() (time.Time, error) {
	return time.Now(), nil
}

func (f *fakeVolume) Status() map[string]interface{} {
	return nil
}

package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

type testValidateMountFiProvider struct{}

func (testValidateMountFiProvider) fileInfo(path string) (exist, isDir bool, err error) {
	if path == testSourcePathDoesNotExist {
		return false, false, nil
	}

	return true, true, nil
}

func TestValidateMount(t *testing.T) {
	previousProvider := currentFileInfoProvider
	defer func() { currentFileInfoProvider = previousProvider }()
	currentFileInfoProvider = testValidateMountFiProvider{}

	testDir, err := ioutil.TempDir("", "test-validate-mount")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testDir)

	cases := []struct {
		input    mount.Mount
		expected error
	}{
		{mount.Mount{Type: mount.TypeVolume}, errMissingField("Target")},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath, Source: "hello"}, nil},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath}, nil},
		{mount.Mount{Type: mount.TypeBind}, errMissingField("Target")},
		{mount.Mount{Type: mount.TypeBind, Target: testDestinationPath}, errMissingField("Source")},
		{mount.Mount{Type: mount.TypeBind, Target: testDestinationPath, Source: testSourcePath, VolumeOptions: &mount.VolumeOptions{}}, errExtraField("VolumeOptions")},

		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath}, nil},
		{mount.Mount{Type: "invalid", Target: testDestinationPath}, errors.New("mount type unknown")},
		{mount.Mount{Type: mount.TypeBind, Source: testSourcePathDoesNotExist, Target: testDestinationPath}, errBindSourceDoesNotExist(testSourcePathDoesNotExist)},
	}

	lcowCases := []struct {
		input    mount.Mount
		expected error
	}{
		{mount.Mount{Type: mount.TypeVolume}, errMissingField("Target")},
		{mount.Mount{Type: mount.TypeVolume, Target: "/foo", Source: "hello"}, nil},
		{mount.Mount{Type: mount.TypeVolume, Target: "/foo"}, nil},
		{mount.Mount{Type: mount.TypeBind}, errMissingField("Target")},
		{mount.Mount{Type: mount.TypeBind, Target: "/foo"}, errMissingField("Source")},
		{mount.Mount{Type: mount.TypeBind, Target: "/foo", Source: "c:\\foo", VolumeOptions: &mount.VolumeOptions{}}, errExtraField("VolumeOptions")},
		{mount.Mount{Type: mount.TypeBind, Source: testSourcePathDoesNotExist, Target: "/foo"}, errBindSourceDoesNotExist(testSourcePathDoesNotExist)},
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: "/foo"}, nil},
		{mount.Mount{Type: "invalid", Target: "/foo"}, errors.New("mount type unknown")},
	}
	parser := NewParser(runtime.GOOS)
	for i, x := range cases {
		err := parser.ValidateMountConfig(&x.input)
		if err == nil && x.expected == nil {
			continue
		}
		if (err == nil && x.expected != nil) || (x.expected == nil && err != nil) || !strings.Contains(err.Error(), x.expected.Error()) {
			t.Errorf("expected %q, got %q, case: %d", x.expected, err, i)
		}
	}
	if runtime.GOOS == "windows" {
		parser = &lcowParser{}
		for i, x := range lcowCases {
			err := parser.ValidateMountConfig(&x.input)
			if err == nil && x.expected == nil {
				continue
			}
			if (err == nil && x.expected != nil) || (x.expected == nil && err != nil) || !strings.Contains(err.Error(), x.expected.Error()) {
				t.Errorf("expected %q, got %q, case: %d", x.expected, err, i)
			}
		}
	}
}

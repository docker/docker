package mounts

import (
	"errors"
	"runtime"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestValidateMount(t *testing.T) {
	testDir := t.TempDir()
	parser := NewParser()

	tests := []struct {
		input    mount.Mount
		expected error
	}{
		{
			input:    mount.Mount{Type: mount.TypeVolume},
			expected: errMissingField("Target"),
		},
		{
			input: mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath, Source: "hello"},
		},
		{
			input: mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath},
		},
		{
			input: mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath, Source: "hello", VolumeOptions: &mount.VolumeOptions{Subpath: "world"}},
		},
		{
			input:    mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath, Source: "hello", BindOptions: &mount.BindOptions{}},
			expected: errExtraField("BindOptions"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind},
			expected: errMissingField("Target"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Target: testDestinationPath},
			expected: errMissingField("Source"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Target: testDestinationPath, Source: testSourcePath, VolumeOptions: &mount.VolumeOptions{}},
			expected: errExtraField("VolumeOptions"),
		},
		{
			input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath},
		},
		{
			input:    mount.Mount{Type: "invalid", Target: testDestinationPath},
			expected: errors.New("mount type unknown"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Source: testSourcePath, Target: testDestinationPath},
			expected: errBindSourceDoesNotExist(testSourcePath),
		},
	}

	if runtime.GOOS != "windows" {
		imageTests := []struct {
			input    mount.Mount
			expected error
		}{
			{
				input:    mount.Mount{Type: mount.TypeImage},
				expected: errMissingField("Target"),
			},
			{
				input:    mount.Mount{Type: mount.TypeImage, Target: testDestinationPath},
				expected: errMissingField("Source"),
			},
			{
				input: mount.Mount{Type: mount.TypeImage, Target: testDestinationPath, Source: "hello"},
			},
			{
				input: mount.Mount{Type: mount.TypeImage, Target: testDestinationPath, Source: "hello", ImageOptions: &mount.ImageOptions{Subpath: "world"}},
			},
			{
				input:    mount.Mount{Type: mount.TypeImage, Target: testDestinationPath, Source: "hello", BindOptions: &mount.BindOptions{}},
				expected: errExtraField("BindOptions"),
			},
			{
				input:    mount.Mount{Type: mount.TypeImage, Target: testDestinationPath, Source: "hello", VolumeOptions: &mount.VolumeOptions{}},
				expected: errExtraField("VolumeOptions"),
			},

			{
				input:    mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath, Source: "hello", ImageOptions: &mount.ImageOptions{}},
				expected: errExtraField("ImageOptions"),
			},
			{
				input:    mount.Mount{Type: mount.TypeBind, Target: testDestinationPath, Source: testSourcePath, ImageOptions: &mount.ImageOptions{}},
				expected: errExtraField("ImageOptions"),
			},
		}
		tests = append(tests, imageTests...)

		idMappingTests := []struct {
			input    mount.Mount
			expected error
		}{
			{
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{Source: mount.IDMappingSourceMatchUser},
				}},
			},
			{
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{Source: mount.IDMappingSourceMatchUser, User: "nginx"},
				}},
			},
			{
				// Source "userns" follows the container's user-namespace
				// mapping, resolved when the container starts.
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{Source: mount.IDMappingSourceUserns},
				}},
			},
			{
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{Source: mount.IDMappingSourceUserns, User: "nginx"},
				}},
				expected: errors.New(`invalid ID mapping: User cannot be combined with Source "userns"`),
			},
			{
				// An empty IDMapping does not select a mapping source.
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{},
				}},
				expected: errors.New(`invalid ID mapping: unknown Source "" (must be "match-user" or "userns")`),
			},
			{
				input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, BindOptions: &mount.BindOptions{
					IDMapping: &mount.IDMapping{Source: "magic"},
				}},
				expected: errors.New(`invalid ID mapping: unknown Source "magic" (must be "match-user" or "userns")`),
			},
		}
		tests = append(tests, idMappingTests...)
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			err := parser.ValidateMountConfig(&tc.input)
			if tc.expected != nil {
				assert.Check(t, is.ErrorContains(err, tc.expected.Error()))
			} else {
				assert.Check(t, err)
			}
		})
	}
}

func TestValidateLCOWMount(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only tested on Windows")
	}
	testDir := t.TempDir()
	parser := NewLCOWParser()

	tests := []struct {
		input    mount.Mount
		expected error
	}{
		{
			input:    mount.Mount{Type: mount.TypeVolume},
			expected: errMissingField("Target"),
		},
		{
			input: mount.Mount{Type: mount.TypeVolume, Target: "/foo", Source: "hello"},
		},
		{
			input: mount.Mount{Type: mount.TypeVolume, Target: "/foo"},
		},
		{
			input:    mount.Mount{Type: mount.TypeBind},
			expected: errMissingField("Target"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Target: "/foo"},
			expected: errMissingField("Source"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Target: "/foo", Source: "c:\\foo", VolumeOptions: &mount.VolumeOptions{}},
			expected: errExtraField("VolumeOptions"),
		},
		{
			input:    mount.Mount{Type: mount.TypeBind, Source: "c:\\foo", Target: "/foo"},
			expected: errBindSourceDoesNotExist("c:\\foo"),
		},
		{
			input: mount.Mount{Type: mount.TypeBind, Source: testDir, Target: "/foo"},
		},
		{
			input:    mount.Mount{Type: "invalid", Target: "/foo"},
			expected: errors.New("mount type unknown"),
		},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			err := parser.ValidateMountConfig(&tc.input)
			if tc.expected != nil {
				assert.Check(t, is.ErrorContains(err, tc.expected.Error()))
			} else {
				assert.Check(t, err)
			}
		})
	}
}

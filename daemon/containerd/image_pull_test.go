package containerd

import (
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	v2 "github.com/moby/moby/v2/daemon/pkg/plugin/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type staticFeatures map[string]bool

func (f staticFeatures) Features() map[string]bool { return f }

func TestLazyPull(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		snapshotter string
		features    featuresProvider
		expected    bool
	}{
		{name: "remote snapshotter without features", snapshotter: "stargz", expected: true},
		{name: "regular snapshotter without features", snapshotter: "overlayfs", expected: false},
		{name: "unset feature uses snapshotter default", snapshotter: "stargz", features: staticFeatures{}, expected: true},
		{name: "feature disables remote snapshotter", snapshotter: "stargz", features: staticFeatures{"lazy-pull": false}, expected: false},
		{name: "feature enables regular snapshotter", snapshotter: "overlayfs", features: staticFeatures{"lazy-pull": true}, expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := &ImageService{features: tc.features}
			assert.Equal(t, i.lazyPull(tc.snapshotter), tc.expected)
		})
	}
}

func TestCheckPullDescriptorMediaType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mediaType   string
		expectError string
	}{
		{
			name:        "plugin config",
			mediaType:   v2.MediaTypePluginConfig,
			expectError: `cannot pull image: remote descriptor is a Docker plugin config ("application/vnd.docker.plugin.v1+json")`,
		},
		{
			name:        "unknown plugin config version",
			mediaType:   "application/vnd.docker.plugin.v0+json",
			expectError: `cannot pull image: remote descriptor is a Docker plugin config ("application/vnd.docker.plugin.v0+json")`,
		},
		{
			name:      "image config",
			mediaType: ocispec.MediaTypeImageConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPullDescriptorMediaType(ocispec.Descriptor{
				MediaType: tc.mediaType,
			})

			if tc.expectError == "" {
				assert.NilError(t, err)
				return
			}

			assert.Check(t, is.Error(err, tc.expectError))
			assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
		})
	}
}

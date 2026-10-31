package containerd

import (
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/diff"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

type staticFeatures map[string]bool

func (f staticFeatures) Features() map[string]bool { return f }

func TestPullUnpackOptionsSyncFilesystem(t *testing.T) {
	var config containerd.UnpackConfig
	for _, opt := range []containerd.UnpackOpt{
		containerd.WithUnpackApplyOpts(diff.WithSyncFs(true)),
	} {
		assert.NilError(t, opt(t.Context(), &config))
	}
	assert.Assert(t, config.ApplyOpts != nil)

	var applyConfig diff.ApplyConfig
	for _, opt := range config.ApplyOpts {
		assert.NilError(t, opt(t.Context(), ocispec.Descriptor{}, &applyConfig))
	}
	assert.Assert(t, applyConfig.SyncFs)
}

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

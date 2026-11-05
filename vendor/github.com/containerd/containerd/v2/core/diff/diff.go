/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package diff

import (
	"context"
	"io"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/typeurl/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Config is used to hold parameters needed for a diff operation
type Config struct {
	// MediaType is the type of diff to generate
	// Default depends on the differ,
	// i.e. application/vnd.oci.image.layer.v1.tar+gzip
	MediaType string

	// Reference is the content upload reference
	// Default will use a random reference string
	Reference string

	// Labels are the labels to apply to the generated content
	Labels map[string]string

	// Compressor is a function to compress the diff stream
	// instead of the default gzip compressor. Differ passes
	// the MediaType of the target diff content to the compressor.
	// When using this config, MediaType must be specified as well.
	Compressor func(dest io.Writer, mediaType string) (io.WriteCloser, error)

	// SourceDateEpoch specifies the SOURCE_DATE_EPOCH without touching the env vars.
	SourceDateEpoch *time.Time

	// UIDMap and GIDMap are the user namespace mappings to map the owners
	// of the files from the host IDs in the mounts to the container IDs in
	// the diff.
	UIDMap, GIDMap []specs.LinuxIDMapping
}

// Opt is used to configure a diff operation
type Opt func(*Config) error

// Comparer allows creation of filesystem diffs between mounts
type Comparer interface {
	// Compare computes the difference between two mounts and returns a
	// descriptor for the computed diff. The options can provide
	// a ref which can be used to track the content creation of the diff.
	// The media type which is used to determine the format of the created
	// content can also be provided as an option.
	Compare(ctx context.Context, lower, upper []mount.Mount, opts ...Opt) (ocispec.Descriptor, error)
}

// ApplyConfig is used to hold parameters needed for a apply operation
type ApplyConfig struct {
	// ProcessorPayloads specifies the payload sent to various processors
	ProcessorPayloads map[string]typeurl.Any
	// SyncFs is to synchronize the underlying filesystem containing files
	SyncFs bool
	// Progress is a function which reports status of processed read data
	Progress func(int64)
	// UIDMap and GIDMap are the user namespace mappings to map the owners
	// of the files from the container IDs in the diff to the host IDs in the
	// mounts.
	UIDMap, GIDMap []specs.LinuxIDMapping
}

// ApplyOpt is used to configure an Apply operation
type ApplyOpt func(context.Context, ocispec.Descriptor, *ApplyConfig) error

// Applier allows applying diffs between mounts
type Applier interface {
	// Apply applies the content referred to by the given descriptor to
	// the provided mount. The method of applying is based on the
	// implementation and content descriptor. For example, in the common
	// case the descriptor is a file system difference in tar format,
	// that tar would be applied on top of the mounts.
	Apply(ctx context.Context, desc ocispec.Descriptor, mount []mount.Mount, opts ...ApplyOpt) (ocispec.Descriptor, error)
}

// WithCompressor sets the function to be used to compress the diff stream.
func WithCompressor(f func(dest io.Writer, mediaType string) (io.WriteCloser, error)) Opt {
	return func(c *Config) error {
		c.Compressor = f
		return nil
	}
}

// WithMediaType sets the media type to use for creating the diff, without
// specifying the differ will choose a default.
func WithMediaType(m string) Opt {
	return func(c *Config) error {
		c.MediaType = m
		return nil
	}
}

// WithReference is used to set the content upload reference used by
// the diff operation. This allows the caller to track the upload through
// the content store.
func WithReference(ref string) Opt {
	return func(c *Config) error {
		c.Reference = ref
		return nil
	}
}

// WithLabels is used to set content labels on the created diff content.
func WithLabels(labels map[string]string) Opt {
	return func(c *Config) error {
		c.Labels = labels
		return nil
	}
}

// WithPayloads sets the apply processor payloads to the config
func WithPayloads(payloads map[string]typeurl.Any) ApplyOpt {
	return func(_ context.Context, _ ocispec.Descriptor, c *ApplyConfig) error {
		c.ProcessorPayloads = payloads
		return nil
	}
}

// WithSyncFs sets sync flag to the config.
func WithSyncFs(sync bool) ApplyOpt {
	return func(_ context.Context, _ ocispec.Descriptor, c *ApplyConfig) error {
		c.SyncFs = sync
		return nil
	}
}

// WithProgress is used to indicate process of the apply operation, should
// at least expect a progress of 0 and of the final size. It is up to the applier
// how much progress it reports in between.
func WithProgress(f func(ocispec.Descriptor, int64)) ApplyOpt {
	return func(_ context.Context, desc ocispec.Descriptor, c *ApplyConfig) error {
		c.Progress = func(state int64) {
			f(desc, state)
		}
		return nil
	}
}

// WithSourceDateEpoch specifies the timestamp used to provide control for reproducibility.
// See also https://reproducible-builds.org/docs/source-date-epoch/ .
//
// Since containerd v2.0, the whiteout timestamps are set to zero (1970-01-01),
// not to the source date epoch.
func WithSourceDateEpoch(tm *time.Time) Opt {
	return func(c *Config) error {
		c.SourceDateEpoch = tm
		return nil
	}
}

// WithIDMapping maps the owners of the files from the host IDs in the mounts
// to the container IDs of the user namespace mappings in the diff.
func WithIDMapping(uidMap, gidMap []specs.LinuxIDMapping) Opt {
	return func(c *Config) error {
		c.UIDMap, c.GIDMap = uidMap, gidMap
		return nil
	}
}

// WithApplyIDMapping maps the owners of the files from the container IDs in
// the diff to the host IDs of the user namespace mappings in the mounts.
func WithApplyIDMapping(uidMap, gidMap []specs.LinuxIDMapping) ApplyOpt {
	return func(_ context.Context, _ ocispec.Descriptor, c *ApplyConfig) error {
		c.UIDMap, c.GIDMap = uidMap, gidMap
		return nil
	}
}

// HasIDMapping returns whether uidMap or gidMap map any IDs.
func HasIDMapping(uidMap, gidMap []specs.LinuxIDMapping) bool {
	return len(uidMap) > 0 || len(gidMap) > 0
}

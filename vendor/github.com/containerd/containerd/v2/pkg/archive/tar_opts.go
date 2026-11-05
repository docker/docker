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

package archive

import (
	"archive/tar"
	"context"
	"io"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// ApplyOptions provides additional options for an Apply operation
type ApplyOptions struct {
	Filter          Filter          // Filter tar headers
	ConvertWhiteout ConvertWhiteout // Convert whiteout files
	Parents         []string        // Parent directories to handle inherited attributes without CoW
	NoSameOwner     bool            // NoSameOwner will not attempt to preserve the owner specified in the tar archive.

	idMapping *idMapping

	applyFunc func(context.Context, string, io.Reader, ApplyOptions) (int64, error)
}

// ApplyOpt allows setting mutable archive apply properties on creation
type ApplyOpt func(options *ApplyOptions) error

// Filter specific files from the archive
type Filter func(*tar.Header) (bool, error)

// ConvertWhiteout converts whiteout files from the archive
type ConvertWhiteout func(*tar.Header, string) (bool, error)

// all allows all files
func all(_ *tar.Header) (bool, error) {
	return true, nil
}

// WithFilter uses the filter to select which files are to be extracted.
func WithFilter(f Filter) ApplyOpt {
	return func(options *ApplyOptions) error {
		options.Filter = f
		return nil
	}
}

// WithConvertWhiteout uses the convert function to convert the whiteout files.
func WithConvertWhiteout(c ConvertWhiteout) ApplyOpt {
	return func(options *ApplyOptions) error {
		options.ConvertWhiteout = c
		return nil
	}
}

// WithNoSameOwner is same as '--no-same-owner` in 'tar' command.
// It'll skip attempt to preserve the owner specified in the tar archive.
func WithNoSameOwner() ApplyOpt {
	return func(options *ApplyOptions) error {
		options.NoSameOwner = true
		return nil
	}
}

// WithParents provides parent directories for resolving inherited attributes
// directory from the filesystem.
// Inherited attributes are searched from first to last, making the first
// element in the list the most immediate parent directory.
// NOTE: When applying to a filesystem which supports CoW, file attributes
// should be inherited by the filesystem.
func WithParents(p []string) ApplyOpt {
	return func(options *ApplyOptions) error {
		options.Parents = p
		return nil
	}
}

// WithApplyIDMapping maps the owners of the files in the archive from
// container IDs to the host IDs of the user namespace mappings.
// The root user of the mappings owns the root directory, which the archive
// does not change, and the parent directories created without attributes to
// inherit.
func WithApplyIDMapping(uidMap, gidMap []specs.LinuxIDMapping) ApplyOpt {
	return func(options *ApplyOptions) error {
		options.idMapping = newIDMapping(uidMap, gidMap)
		return nil
	}
}

// WriteDiffOptions provides additional options for a WriteDiff operation
type WriteDiffOptions struct {
	ParentLayers []string // Windows needs the full list of parent layers

	idMapping *idMapping

	writeDiffFunc func(context.Context, io.Writer, string, string, WriteDiffOptions) error

	// SourceDateEpoch specifies the following timestamps to provide control for reproducibility.
	//   - The upper bound timestamp of the diff contents
	//   - The timestamp of the whiteouts
	//
	// See also https://reproducible-builds.org/docs/source-date-epoch/ .
	SourceDateEpoch *time.Time
}

// WriteDiffOpt allows setting mutable archive write properties on creation
type WriteDiffOpt func(options *WriteDiffOptions) error

// WithSourceDateEpoch specifies the SOURCE_DATE_EPOCH without touching the env vars.
func WithSourceDateEpoch(tm *time.Time) WriteDiffOpt {
	return func(options *WriteDiffOptions) error {
		options.SourceDateEpoch = tm
		return nil
	}
}

// WithWriteDiffIDMapping maps the owners of the files in the diff from host
// IDs to the container IDs of the user namespace mappings.
func WithWriteDiffIDMapping(uidMap, gidMap []specs.LinuxIDMapping) WriteDiffOpt {
	return func(options *WriteDiffOptions) error {
		options.idMapping = newIDMapping(uidMap, gidMap)
		return nil
	}
}

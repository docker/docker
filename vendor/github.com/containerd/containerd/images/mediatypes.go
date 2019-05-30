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

package images

// mediatype definitions for image components handled in containerd.
//
// oci components are generally referenced directly, although we may centralize
// here for clarity.
const (
	MediaTypeDockerSchema2Layer            = "application/vnd.docker.image.rootfs.diff.tar"
	MediaTypeDockerSchema2LayerForeign     = "application/vnd.docker.image.rootfs.foreign.diff.tar"
	MediaTypeDockerSchema2LayerGzip        = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeDockerSchema2LayerForeignGzip = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
	MediaTypeDockerSchema2Config           = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerSchema2Manifest         = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerSchema2ManifestList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	// Checkpoint/Restore Media Types
	MediaTypeContainerd1Checkpoint               = "application/vnd.containerd.container.criu.checkpoint.criu.tar"
	MediaTypeContainerd1CheckpointPreDump        = "application/vnd.containerd.container.criu.checkpoint.predump.tar"
	MediaTypeContainerd1Resource                 = "application/vnd.containerd.container.resource.tar"
	MediaTypeContainerd1RW                       = "application/vnd.containerd.container.rw.tar"
	MediaTypeContainerd1CheckpointConfig         = "application/vnd.containerd.container.checkpoint.config.v1+proto"
	MediaTypeContainerd1CheckpointOptions        = "application/vnd.containerd.container.checkpoint.options.v1+proto"
	MediaTypeContainerd1CheckpointRuntimeName    = "application/vnd.containerd.container.checkpoint.runtime.name"
	MediaTypeContainerd1CheckpointRuntimeOptions = "application/vnd.containerd.container.checkpoint.runtime.options+proto"
	// Legacy Docker schema1 manifest
	MediaTypeDockerSchema1Manifest = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

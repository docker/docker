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

package containerd

import (
	"context"
	"io"

	"github.com/containerd/containerd/images/oci"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Export exports an image to a Tar stream.
// OCI format is used by default.
// It is up to caller to put "org.opencontainers.image.ref.name" annotation to desc.
// TODO(AkihiroSuda): support exporting multiple descriptors at once to a single archive stream.
func (c *Client) Export(ctx context.Context, desc ocispec.Descriptor, opts ...oci.V1ExporterOpt) (io.ReadCloser, error) {

	exporter, err := oci.ResolveV1ExportOpt(opts...)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(errors.Wrap(exporter.Export(ctx, c.ContentStore(), desc, pw), "export failed"))
	}()
	return pr, nil
}

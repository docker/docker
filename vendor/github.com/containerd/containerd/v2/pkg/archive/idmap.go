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
	"fmt"
	"os"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/containerd/v2/internal/userns"
)

const (
	paxCapabilityXattr = paxSchilyXattr + "security.capability"

	// Values based on linux/include/uapi/linux/capability.h
	vfsCapRevisionOffset = 3
	vfsCapRevision2      = 2
	vfsCapRevision3      = 3
	vfsCapSize2          = 20
)

// idMapping maps the owners of files between the container IDs in archives
// and the host IDs on disk.
type idMapping struct {
	userns.IDMap
}

func newIDMapping(uidMap, gidMap []specs.LinuxIDMapping) *idMapping {
	return &idMapping{userns.IDMap{UidMap: uidMap, GidMap: gidMap}}
}

// toHost maps the owner of hdr to the host IDs.
func (m *idMapping) toHost(hdr *tar.Header) error {
	host, err := m.ToHost(userns.User{Uid: uint32(hdr.Uid), Gid: uint32(hdr.Gid)})
	if err != nil {
		return fmt.Errorf("failed to map the owner of %q: %w", hdr.Name, err)
	}
	hdr.Uid, hdr.Gid = int(host.Uid), int(host.Gid)
	return nil
}

// toContainer maps the owner of hdr to the container IDs.
// Version 3 file capabilities store the host ID of the root of the user
// namespace which set them, so they are converted to version 2.
func (m *idMapping) toContainer(hdr *tar.Header) error {
	ctr, err := m.ToContainer(userns.User{Uid: uint32(hdr.Uid), Gid: uint32(hdr.Gid)})
	if err != nil {
		return fmt.Errorf("failed to map the owner of %q: %w", hdr.Name, err)
	}
	hdr.Uid, hdr.Gid = int(ctr.Uid), int(ctr.Gid)

	if caps, ok := hdr.PAXRecords[paxCapabilityXattr]; ok && len(caps) > vfsCapRevisionOffset && caps[vfsCapRevisionOffset] == vfsCapRevision3 {
		v2 := []byte(caps)
		v2[vfsCapRevisionOffset] = vfsCapRevision2
		hdr.PAXRecords[paxCapabilityXattr] = string(v2[:min(len(v2), vfsCapSize2)])
	}
	return nil
}

// chownRoot makes the root of the mapping own path.
func (m *idMapping) chownRoot(path string) error {
	root, err := m.RootPair()
	if err != nil {
		return err
	}
	return os.Lchown(path, int(root.Uid), int(root.Gid))
}

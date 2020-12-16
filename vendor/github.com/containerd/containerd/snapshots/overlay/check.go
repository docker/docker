// +build linux

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

package overlay

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/continuity/fs"
	"github.com/pkg/errors"
)

// supportsMultipleLowerDir checks if the system supports multiple lowerdirs,
// which is required for the overlay snapshotter. On 4.x kernels, multiple lowerdirs
// are always available (so this check isn't needed), and backported to RHEL and
// CentOS 3.x kernels (3.10.0-693.el7.x86_64 and up). This function is to detect
// support on those kernels, without doing a kernel version compare.
//
// Ported from moby overlay2.
func supportsMultipleLowerDir(d string) error {
	td, err := ioutil.TempDir(d, "multiple-lowerdir-check")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(td); err != nil {
			log.L.WithError(err).Warnf("Failed to remove check directory %v", td)
		}
	}()

	for _, dir := range []string{"lower1", "lower2", "upper", "work", "merged"} {
		if err := os.Mkdir(filepath.Join(td, dir), 0755); err != nil {
			return err
		}
	}

	opts := fmt.Sprintf("lowerdir=%s:%s,upperdir=%s,workdir=%s", filepath.Join(td, "lower2"), filepath.Join(td, "lower1"), filepath.Join(td, "upper"), filepath.Join(td, "work"))
	m := mount.Mount{
		Type:    "overlay",
		Source:  "overlay",
		Options: []string{opts},
	}
	dest := filepath.Join(td, "merged")
	if err := m.Mount(dest); err != nil {
		return errors.Wrap(err, "failed to mount overlay")
	}
	if err := mount.UnmountAll(dest, 0); err != nil {
		log.L.WithError(err).Warnf("Failed to unmount check directory %v", dest)
	}
	return nil
}

// Supported returns nil when the overlayfs is functional on the system with the root directory.
// Supported is not called during plugin initialization, but exposed for downstream projects which uses
// this snapshotter as a library.
func Supported(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return err
	}
	if !supportsDType {
		return fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	return supportsMultipleLowerDir(root)
}

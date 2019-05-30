// +build !windows

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

package sys

import "os"

// ForceRemoveAll on unix is just a wrapper for os.RemoveAll
func ForceRemoveAll(path string) error {
	return os.RemoveAll(path)
}

// MkdirAllWithACL is a wrapper for os.MkdirAll on Unix systems.
func MkdirAllWithACL(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

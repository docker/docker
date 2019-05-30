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

// SetOOMScore sets the oom score for the process
//
// Not implemented on Windows
func SetOOMScore(pid, score int) error {
	return nil
}

// GetOOMScoreAdj gets the oom score for a process
//
// Not implemented on Windows
func GetOOMScoreAdj(pid int) (int, error) {
	return 0, nil
}

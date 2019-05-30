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

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/opencontainers/runc/libcontainer/system"
)

// OOMScoreMaxKillable is the maximum score keeping the process killable by the oom killer
const OOMScoreMaxKillable = -999

// SetOOMScore sets the oom score for the provided pid
func SetOOMScore(pid, score int) error {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(strconv.Itoa(score)); err != nil {
		if os.IsPermission(err) && (system.RunningInUserNS() || RunningUnprivileged()) {
			return nil
		}
		return err
	}
	return nil
}

// GetOOMScoreAdj gets the oom score for a process
func GetOOMScoreAdj(pid int) (int, error) {
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

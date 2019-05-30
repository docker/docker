// +build aix darwin dragonfly freebsd linux netbsd openbsd solaris

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
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ParseSignal parses a given string into a syscall.Signal
// the rawSignal can be a string with "SIG" prefix,
// or a signal number in string format.
func ParseSignal(rawSignal string) (syscall.Signal, error) {
	s, err := strconv.Atoi(rawSignal)
	if err == nil {
		signal := syscall.Signal(s)
		if unix.SignalName(signal) != "" {
			return signal, nil
		}
		return -1, fmt.Errorf("unknown signal %q", rawSignal)
	}
	signal := unix.SignalNum(strings.ToUpper(rawSignal))
	if signal == 0 {
		return -1, fmt.Errorf("unknown signal %q", rawSignal)
	}
	return signal, nil
}

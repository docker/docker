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

	"golang.org/x/sys/windows"
)

var signalMap = map[string]syscall.Signal{
	"HUP":    syscall.Signal(windows.SIGHUP),
	"INT":    syscall.Signal(windows.SIGINT),
	"QUIT":   syscall.Signal(windows.SIGQUIT),
	"SIGILL": syscall.Signal(windows.SIGILL),
	"TRAP":   syscall.Signal(windows.SIGTRAP),
	"ABRT":   syscall.Signal(windows.SIGABRT),
	"BUS":    syscall.Signal(windows.SIGBUS),
	"FPE":    syscall.Signal(windows.SIGFPE),
	"KILL":   syscall.Signal(windows.SIGKILL),
	"SEGV":   syscall.Signal(windows.SIGSEGV),
	"PIPE":   syscall.Signal(windows.SIGPIPE),
	"ALRM":   syscall.Signal(windows.SIGALRM),
	"TERM":   syscall.Signal(windows.SIGTERM),
}

// ParseSignal parses a given string into a syscall.Signal
// the rawSignal can be a string with "SIG" prefix,
// or a signal number in string format.
func ParseSignal(rawSignal string) (syscall.Signal, error) {
	s, err := strconv.Atoi(rawSignal)
	if err == nil {
		sig := syscall.Signal(s)
		for _, msig := range signalMap {
			if sig == msig {
				return sig, nil
			}
		}
		return -1, fmt.Errorf("unknown signal %q", rawSignal)
	}
	signal, ok := signalMap[strings.TrimPrefix(strings.ToUpper(rawSignal), "SIG")]
	if !ok {
		return -1, fmt.Errorf("unknown signal %q", rawSignal)
	}
	return signal, nil
}

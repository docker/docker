package seccomp // import "github.com/docker/docker/profiles/seccomp"

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// Seccomp represents the config for a seccomp profile for syscall restriction.
type Seccomp struct {
	DefaultAction    specs.LinuxSeccompAction `json:"defaultAction"`
	DefaultErrnoRet  *uint                    `json:"defaultErrnoRet,omitempty"`
	ListenerPath     string                   `json:"listenerPath,omitempty"`
	ListenerMetadata string                   `json:"listenerMetadata,omitempty"`

	// Architectures is kept to maintain backward compatibility with the old
	// seccomp profile.
	Architectures []specs.Arch   `json:"architectures,omitempty"`
	ArchMap       []Architecture `json:"archMap,omitempty"`
	Syscalls      []*Syscall     `json:"syscalls"`
}

// Architecture is used to represent a specific architecture
// and its sub-architectures
type Architecture struct {
	Arch      specs.Arch   `json:"architecture"`
	SubArches []specs.Arch `json:"subArchitectures"`
}

// Filter is used to conditionally apply Seccomp rules
type Filter struct {
	Caps   []string `json:"caps,omitempty"`
	Arches []string `json:"arches,omitempty"`

	// MinKernel describes the minimum kernel version the rule must be applied
	// on, in the format "<kernel version>.<major revision>" (e.g. "3.12").
	//
	// When matching the kernel version of the host, minor revisions, and distro-
	// specific suffixes are ignored, which means that "3.12.25-gentoo", "3.12-1-amd64",
	// "3.12", and "3.12-rc5" are considered equal (kernel 3, major revision 12).
	MinKernel *KernelVersion `json:"minKernel,omitempty"`
}

// Syscall is used to match a group of syscalls in Seccomp. It extends the
// runtime-spec Syscall type, adding a "Name" field for backward compatibility
// with older JSON representations, additional "Comment" metadata, and conditional
// rules ("Includes", "Excludes") used to generate a runtime-spec Seccomp profile
// based on the container (capabilities) and host's (arch, kernel) configuration.
type Syscall struct {
	specs.LinuxSyscall
	// Deprecated: kept for backward compatibility with old JSON profiles, use Names instead
	Name     string  `json:"name,omitempty"`
	Comment  string  `json:"comment,omitempty"`
	Includes *Filter `json:"includes,omitempty"`
	Excludes *Filter `json:"excludes,omitempty"`
}

// KernelVersion holds information about the kernel.
type KernelVersion struct {
	Kernel uint64 // Version of the Kernel (i.e., the "4" in "4.1.2-generic")
	Major  uint64 // Major revision of the Kernel (i.e., the "1" in "4.1.2-generic")
}

// String implements fmt.Stringer for KernelVersion
func (k *KernelVersion) String() string {
	if k.Kernel > 0 || k.Major > 0 {
		return fmt.Sprintf("%d.%d", k.Kernel, k.Major)
	}
	return ""
}

// MarshalJSON implements json.Unmarshaler for KernelVersion
func (k *KernelVersion) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// UnmarshalJSON implements json.Marshaler for KernelVersion
func (k *KernelVersion) UnmarshalJSON(version []byte) error {
	var (
		ver string
		err error
	)

	// make sure we have a string
	if err = json.Unmarshal(version, &ver); err != nil {
		return fmt.Errorf(`invalid kernel version: %s, expected "<kernel>.<major>": %v`, string(version), err)
	}
	if ver == "" {
		return nil
	}
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) != 2 {
		return fmt.Errorf(`invalid kernel version: %s, expected "<kernel>.<major>"`, string(version))
	}
	if k.Kernel, err = strconv.ParseUint(parts[0], 10, 8); err != nil {
		return fmt.Errorf(`invalid kernel version: %s, expected "<kernel>.<major>": %v`, string(version), err)
	}
	if k.Major, err = strconv.ParseUint(parts[1], 10, 8); err != nil {
		return fmt.Errorf(`invalid kernel version: %s, expected "<kernel>.<major>": %v`, string(version), err)
	}
	if k.Kernel == 0 && k.Major == 0 {
		return fmt.Errorf(`invalid kernel version: %s, expected "<kernel>.<major>": version cannot be 0.0`, string(version))
	}
	return nil
}

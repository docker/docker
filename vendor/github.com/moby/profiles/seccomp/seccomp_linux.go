// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate go run -tags 'seccomp' generate.go

package seccomp

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// GetDefaultProfile returns the default seccomp profile.
func GetDefaultProfile(rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	return setupSeccomp(DefaultProfile(), rs)
}

// LoadProfile takes a json string and decodes the seccomp profile.
func LoadProfile(body string, rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	var config Seccomp
	if err := json.Unmarshal([]byte(body), &config); err != nil {
		return nil, fmt.Errorf("Decoding seccomp profile failed: %w", err)
	}
	return setupSeccomp(&config, rs)
}

// libseccomp string => seccomp arch
var nativeToSeccomp = map[string]specs.Arch{
	"x86":         specs.ArchX86,
	"amd64":       specs.ArchX86_64,
	"arm":         specs.ArchARM,
	"arm64":       specs.ArchAARCH64,
	"loongarch64": specs.ArchLOONGARCH64,
	"mips64":      specs.ArchMIPS64,
	"mips64n32":   specs.ArchMIPS64N32,
	"mipsel64":    specs.ArchMIPSEL64,
	"mips3l64n32": specs.ArchMIPSEL64N32,
	"mipsel":      specs.ArchMIPSEL,
	"ppc":         specs.ArchPPC,
	"ppc64":       specs.ArchPPC64,
	"ppc64le":     specs.ArchPPC64LE,
	"riscv64":     specs.ArchRISCV64,
	"s390":        specs.ArchS390,
	"s390x":       specs.ArchS390X,
}

// GOARCH => libseccomp string
var goToNative = map[string]string{
	"386":         "x86",
	"amd64":       "amd64",
	"arm":         "arm",
	"arm64":       "arm64",
	"loong64":     "loongarch64",
	"mips64":      "mips64",
	"mips64p32":   "mips64n32",
	"mips64le":    "mipsel64",
	"mips64p32le": "mips3l64n32",
	"mipsle":      "mipsel",
	"ppc":         "ppc",
	"ppc64":       "ppc64",
	"ppc64le":     "ppc64le",
	"riscv64":     "riscv64",
	"s390":        "s390",
	"s390x":       "s390x",
}

func setupSeccomp(config *Seccomp, rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	if config == nil {
		return nil, nil
	}

	// No default action specified, no syscalls listed, assume seccomp disabled
	if config.DefaultAction == "" && len(config.Syscalls) == 0 {
		return nil, nil
	}

	if len(config.Architectures) != 0 && len(config.ArchMap) != 0 {
		return nil, errors.New("both 'architectures' and 'archMap' are specified in the seccomp profile, use either 'architectures' or 'archMap'")
	}

	if len(config.LinuxSeccomp.Syscalls) != 0 {
		// The Seccomp type overrides the LinuxSeccomp.Syscalls field,
		// so 'this should never happen' when loaded from JSON, but could
		// happen if someone constructs the Config from source.
		return nil, errors.New("the LinuxSeccomp.Syscalls field should be empty")
	}

	var (
		// Copy all common / standard properties to the output profile
		newConfig = &config.LinuxSeccomp
		arch      = goToNative[runtime.GOARCH]
	)
	if seccompArch, ok := nativeToSeccomp[arch]; ok {
		for _, a := range config.ArchMap {
			if a.Arch == seccompArch {
				newConfig.Architectures = append(newConfig.Architectures, a.Arch)
				newConfig.Architectures = append(newConfig.Architectures, a.SubArches...)
				break
			}
		}
	}

Loop:
	// Convert Syscall to OCI runtimes-spec specs.LinuxSyscall after filtering them.
	for _, call := range config.Syscalls {
		if call.Name != "" {
			if len(call.Names) != 0 {
				return nil, errors.New("both 'name' and 'names' are specified in the seccomp profile, use either 'name' or 'names'")
			}
			call.Names = []string{call.Name}
		}
		if call.Excludes != nil {
			if len(call.Excludes.Arches) > 0 {
				if slices.Contains(call.Excludes.Arches, arch) {
					continue Loop
				}
			}
			if len(call.Excludes.Caps) > 0 {
				for _, c := range call.Excludes.Caps {
					if slices.Contains(rs.Process.Capabilities.Bounding, c) {
						continue Loop
					}
				}
			}
			if call.Excludes.MinKernel != nil {
				if ok, err := kernelGreaterEqualThan(*call.Excludes.MinKernel); err != nil {
					return nil, err
				} else if ok {
					continue Loop
				}
			}
		}
		if call.Includes != nil {
			if len(call.Includes.Arches) > 0 {
				if !slices.Contains(call.Includes.Arches, arch) {
					continue Loop
				}
			}
			if len(call.Includes.Caps) > 0 {
				for _, c := range call.Includes.Caps {
					if !slices.Contains(rs.Process.Capabilities.Bounding, c) {
						continue Loop
					}
				}
			}
			if call.Includes.MinKernel != nil {
				if ok, err := kernelGreaterEqualThan(*call.Includes.MinKernel); err != nil {
					return nil, err
				} else if !ok {
					continue Loop
				}
			}
		}
		newConfig.Syscalls = append(newConfig.Syscalls, call.LinuxSyscall)
	}

	return newConfig, nil
}

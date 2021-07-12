//go:generate go run -tags 'seccomp' generate.go

package seccomp // import "github.com/docker/docker/profiles/seccomp"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// GetDefaultProfile returns the default seccomp profile.
func GetDefaultProfile(rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	return setupSeccomp(DefaultProfile(), rs)
}

// LoadProfile takes a json string and decodes the seccomp profile.
func LoadProfile(body string, rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	var config Seccomp
	if err := json.Unmarshal([]byte(body), &config); err != nil {
		return nil, fmt.Errorf("Decoding seccomp profile failed: %v", err)
	}
	return setupSeccomp(&config, rs)
}

// libseccomp string => seccomp arch
var nativeToSeccomp = map[string]specs.Arch{
	"x86":         specs.ArchX86,
	"amd64":       specs.ArchX86_64,
	"arm":         specs.ArchARM,
	"arm64":       specs.ArchAARCH64,
	"mips64":      specs.ArchMIPS64,
	"mips64n32":   specs.ArchMIPS64N32,
	"mipsel64":    specs.ArchMIPSEL64,
	"mips3l64n32": specs.ArchMIPSEL64N32,
	"mipsle":      specs.ArchMIPSEL,
	"ppc":         specs.ArchPPC,
	"ppc64":       specs.ArchPPC64,
	"ppc64le":     specs.ArchPPC64LE,
	"s390":        specs.ArchS390,
	"s390x":       specs.ArchS390X,
}

// GOARCH => libseccomp string
var goToNative = map[string]string{
	"386":         "x86",
	"amd64":       "amd64",
	"arm":         "arm",
	"arm64":       "arm64",
	"mips64":      "mips64",
	"mips64p32":   "mips64n32",
	"mips64le":    "mipsel64",
	"mips64p32le": "mips3l64n32",
	"mipsle":      "mipsel",
	"ppc":         "ppc",
	"ppc64":       "ppc64",
	"ppc64le":     "ppc64le",
	"s390":        "s390",
	"s390x":       "s390x",
}

// inSlice tests whether a string is contained in a slice of strings or not.
// Comparison is case sensitive
func inSlice(slice []string, s string) bool {
	for _, ss := range slice {
		if s == ss {
			return true
		}
	}
	return false
}

func setupSeccomp(config *Seccomp, rs *specs.Spec) (*specs.LinuxSeccomp, error) {
	if config == nil {
		return nil, nil
	}

	// No default action specified, no syscalls listed, assume seccomp disabled
	if config.DefaultAction == "" && len(config.Syscalls) == 0 {
		return nil, nil
	}

	newConfig := &specs.LinuxSeccomp{}

	if len(config.Architectures) != 0 && len(config.ArchMap) != 0 {
		return nil, errors.New("'architectures' and 'archMap' were specified in the seccomp profile, use either 'architectures' or 'archMap'")
	}

	// if config.Architectures == 0 then libseccomp will figure out the architecture to use
	if len(config.Architectures) != 0 {
		newConfig.Architectures = config.Architectures
	}

	arch := goToNative[runtime.GOARCH]
	seccompArch, archExists := nativeToSeccomp[arch]

	if len(config.ArchMap) != 0 && archExists {
		for _, a := range config.ArchMap {
			if a.Arch == seccompArch {
				newConfig.Architectures = append(newConfig.Architectures, a.Arch)
				newConfig.Architectures = append(newConfig.Architectures, a.SubArches...)
				break
			}
		}
	}

	newConfig.DefaultAction = config.DefaultAction
	newConfig.DefaultErrnoRet = config.DefaultErrnoRet
	newConfig.ListenerPath = config.ListenerPath
	newConfig.ListenerMetadata = config.ListenerMetadata

Loop:
	// Loop through all syscall blocks and convert them to libcontainer format after filtering them
	for _, call := range config.Syscalls {
		if call.Excludes != nil {
			if len(call.Excludes.Arches) > 0 {
				if inSlice(call.Excludes.Arches, arch) {
					continue Loop
				}
			}
			if len(call.Excludes.Caps) > 0 {
				for _, c := range call.Excludes.Caps {
					if inSlice(rs.Process.Capabilities.Bounding, c) {
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
				if !inSlice(call.Includes.Arches, arch) {
					continue Loop
				}
			}
			if len(call.Includes.Caps) > 0 {
				for _, c := range call.Includes.Caps {
					if !inSlice(rs.Process.Capabilities.Bounding, c) {
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

		if call.Name != "" {
			if len(call.Names) != 0 {
				return nil, errors.New("'name' and 'names' were specified in the seccomp profile, use either 'name' or 'names'")
			}
			call.Names = append(call.Names, call.Name)
		}

		newConfig.Syscalls = append(newConfig.Syscalls, call.LinuxSyscall)
	}

	return newConfig, nil
}

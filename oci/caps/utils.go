package caps // import "github.com/docker/docker/oci/caps"

import (
	"fmt"
	"strings"

	"github.com/docker/docker/errdefs"
	"github.com/syndtr/gocapability/capability"
)

var (
	allCaps []string

	// capabilityList maps linux capability name to its value of capability.Cap
	// type. This list contains nil entries for capabilities that are known, but
	// not supported by the current kernel.
	// Capabilities is one of the security systems in Linux Security Module (LSM)
	// framework provided by the kernel.
	// For more details on capabilities, see http://man7.org/linux/man-pages/man7/capabilities.7.html
	capabilityList map[string]*capability.Cap
)

func init() {
	last := capability.CAP_LAST_CAP
	// hack for RHEL6 which has no /proc/sys/kernel/cap_last_cap
	if last == capability.Cap(63) {
		last = capability.CAP_BLOCK_SUSPEND
	}
	if last > capability.CAP_AUDIT_READ {
		// Prevents docker from setting CAP_PERFMON, CAP_BPF, and CAP_CHECKPOINT_RESTORE
		// capabilities on privileged (or CAP_ALL) containers on Kernel 5.8 and up.
		// While these kernels support these capabilities, the current release of
		// runc ships with an older version of /gocapability/capability, and does
		// not know about them, causing an error to be produced.
		//
		// FIXME remove once https://github.com/opencontainers/runc/commit/6dfbe9b80707b1ca188255e8def15263348e0f9a
		//       is included in a runc release and once we stop supporting containerd 1.3.x
		//       (which ships with runc v1.0.0-rc92)
		last = capability.CAP_AUDIT_READ
	}
	rawCaps := capability.List()
	allCaps = make([]string, min(int(last+1), len(rawCaps)))
	capabilityList = make(map[string]*capability.Cap, len(rawCaps))
	for i, c := range rawCaps {
		capName := "CAP_" + strings.ToUpper(c.String())
		if c > last {
			capabilityList[capName] = nil
			continue
		}
		allCaps[i] = capName
		capabilityList[capName] = &c
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetAllCapabilities returns all of the capabilities
func GetAllCapabilities() []string {
	return allCaps
}

// inSlice tests whether a string is contained in a slice of strings or not.
func inSlice(slice []string, s string) bool {
	for _, ss := range slice {
		if s == ss {
			return true
		}
	}
	return false
}

const allCapabilities = "ALL"

// NormalizeLegacyCapabilities normalizes, and validates CapAdd/CapDrop capabilities
// by upper-casing them, and adding a CAP_ prefix (if not yet present).
//
// This function also accepts the "ALL" magic-value, that's used by CapAdd/CapDrop.
func NormalizeLegacyCapabilities(caps []string) ([]string, error) {
	var normalized []string

	for _, c := range caps {
		c = strings.ToUpper(c)
		if c == allCapabilities {
			normalized = append(normalized, c)
			continue
		}
		if !strings.HasPrefix(c, "CAP_") {
			c = "CAP_" + c
		}
		if v, ok := capabilityList[c]; !ok {
			return nil, errdefs.InvalidParameter(fmt.Errorf("unknown capability: %q", c))
		} else if v == nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("capability not supported by your kernel: %q", c))
		}
		normalized = append(normalized, c)
	}
	return normalized, nil
}

// TweakCapabilities tweaks capabilities by adding, dropping, or overriding
// capabilities in the basics capabilities list.
func TweakCapabilities(basics, adds, drops []string, privileged bool) ([]string, error) {
	switch {
	case privileged:
		// Privileged containers get all capabilities
		return GetAllCapabilities(), nil
	case len(adds) == 0 && len(drops) == 0:
		// Nothing to tweak; we're done
		return basics, nil
	}

	capDrop, err := NormalizeLegacyCapabilities(drops)
	if err != nil {
		return nil, err
	}
	capAdd, err := NormalizeLegacyCapabilities(adds)
	if err != nil {
		return nil, err
	}

	var caps []string

	switch {
	case inSlice(capAdd, allCapabilities):
		// Add all capabilities except ones on capDrop
		for _, c := range GetAllCapabilities() {
			if !inSlice(capDrop, c) {
				caps = append(caps, c)
			}
		}
	case inSlice(capDrop, allCapabilities):
		// "Drop" all capabilities; use what's in capAdd instead
		caps = capAdd
	default:
		// First drop some capabilities
		for _, c := range basics {
			if !inSlice(capDrop, c) {
				caps = append(caps, c)
			}
		}
		// Then add the list of capabilities from capAdd
		caps = append(caps, capAdd...)
	}
	return caps, nil
}

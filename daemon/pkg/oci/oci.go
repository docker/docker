package oci

import (
	"fmt"
	"strconv"

	"github.com/moby/moby/v2/daemon/internal/lazyregexp"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var deviceCgroupRuleRegex = lazyregexp.New("^([acb]) ([0-9]+|\\*):([0-9]+|\\*) ([rwm]{1,3})$")

// AppendDevicePermissionsFromCgroupRules takes rules for the devices cgroup to append to the default set
func AppendDevicePermissionsFromCgroupRules(devPermissions []specs.LinuxDeviceCgroup, rules []string) ([]specs.LinuxDeviceCgroup, error) {
	for _, deviceCgroupRule := range rules {
		rule := deviceCgroupRule

		// "a" (all) is a wildcard type; it applies to all device types, all
		// major and minor numbers, and all permissions, so those fields may
		// be omitted, and the kernel ignores them if they are given. The
		// kernel documentation describes it as such, and shows an example
		// passing only the type; "echo a > /sys/fs/cgroup/1/devices.allow";
		// https://github.com/torvalds/linux/blob/v5.10/Documentation/admin-guide/cgroup-v1/devices.rst
		// and the implementation returns early for it;
		// https://github.com/torvalds/linux/blob/v5.10/security/device_cgroup.c#L614-L642
		//
		// Expand the shorthand to the equivalent explicit form, so that the
		// rest of the parsing below is unchanged.
		if rule == "a" {
			rule = "a *:* rwm"
		}

		ss := deviceCgroupRuleRegex.FindAllStringSubmatch(rule, -1)
		if len(ss) == 0 || len(ss[0]) != 5 {
			return nil, fmt.Errorf("invalid device cgroup rule format: '%s'", deviceCgroupRule)
		}
		matches := ss[0]

		dPermissions := specs.LinuxDeviceCgroup{
			Allow:  true,
			Type:   matches[1],
			Access: matches[4],
		}
		if matches[2] == "*" {
			major := int64(-1)
			dPermissions.Major = &major
		} else {
			major, err := strconv.ParseInt(matches[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid major value in device cgroup rule format: '%s'", deviceCgroupRule)
			}
			dPermissions.Major = &major
		}
		if matches[3] == "*" {
			minor := int64(-1)
			dPermissions.Minor = &minor
		} else {
			minor, err := strconv.ParseInt(matches[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid minor value in device cgroup rule format: '%s'", deviceCgroupRule)
			}
			dPermissions.Minor = &minor
		}
		devPermissions = append(devPermissions, dPermissions)
	}
	return devPermissions, nil
}

// +build linux

package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"

	"github.com/containerd/containerd/pkg/apparmor"
	aaprofile "github.com/docker/docker/profiles/apparmor"
)

// Define constants for native driver
const (
	unconfinedAppArmorProfile = "unconfined"
	defaultAppArmorProfile    = "docker-default"
)

// DefaultAppArmorProfile returns the name of the default apparmor profile
func DefaultAppArmorProfile() string {
	if apparmor.HostSupports() {
		return defaultAppArmorProfile
	}
	return ""
}

func clobberDefaultAppArmorProfile() error {
	if apparmor.HostSupports() {
		if err := aaprofile.InstallDefault(defaultAppArmorProfile); err != nil {
			return fmt.Errorf("AppArmor enabled on system but the %s profile could not be loaded: %s", defaultAppArmorProfile, err)
		}
	}
	return nil
}

func ensureDefaultAppArmorProfile() error {
	if apparmor.HostSupports() {
		loaded, err := aaprofile.IsLoaded(defaultAppArmorProfile)
		if err != nil {
			return fmt.Errorf("Could not check if %s AppArmor profile was loaded: %s", defaultAppArmorProfile, err)
		}

		// Nothing to do.
		if loaded {
			return nil
		}

		// Load the profile.
		return clobberDefaultAppArmorProfile()
	}
	return nil
}

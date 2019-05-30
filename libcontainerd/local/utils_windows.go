package local // import "github.com/docker/docker/libcontainerd/local"

import (
	"strings"

	opengcs "github.com/Microsoft/opengcs/client"
)

// setupEnvironmentVariables converts a string array of environment variables
// into a map as required by the HCS. Source array is in format [v1=k1] [v2=k2] etc.
func setupEnvironmentVariables(a []string) map[string]string {
	r := make(map[string]string)
	for _, s := range a {
		arr := strings.SplitN(s, "=", 2)
		if len(arr) == 2 {
			r[arr[0]] = arr[1]
		}
	}
	return r
}

// LCOWOption is a CreateOption required for LCOW configuration
type LCOWOption struct {
	Config *opengcs.Config
}

// Apply for the LCOW option is a no-op.
func (s *LCOWOption) Apply(interface{}) error {
	return nil
}

// debugGCS is a dirty hack for debugging for Linux Utility VMs. It simply
// runs a bunch of commands inside the UVM, but seriously aides in advanced debugging.
func (c *container) debugGCS() {
	if c == nil || c.isWindows || c.hcsContainer == nil {
		return
	}
	cfg := opengcs.Config{
		Uvm:               c.hcsContainer,
		UvmTimeoutSeconds: 600,
	}
	cfg.DebugGCS()
}

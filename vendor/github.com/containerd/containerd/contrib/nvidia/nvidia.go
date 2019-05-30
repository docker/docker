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

package nvidia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// NvidiaCLI is the path to the Nvidia helper binary
const NvidiaCLI = "nvidia-container-cli"

// Capability specifies capabilities for the gpu inside the container
// Detailed explanation of options can be found:
// https://github.com/nvidia/nvidia-container-runtime#supported-driver-capabilities
type Capability string

const (
	// Compute capability
	Compute Capability = "compute"
	// Compat32 capability
	Compat32 Capability = "compat32"
	// Graphics capability
	Graphics Capability = "graphics"
	// Utility capability
	Utility Capability = "utility"
	// Video capability
	Video Capability = "video"
	// Display capability
	Display Capability = "display"
)

// AllCaps returns the complete list of supported Nvidia capabilties.
func AllCaps() []Capability {
	return []Capability{
		Compute,
		Compat32,
		Graphics,
		Utility,
		Video,
		Display,
	}
}

// WithGPUs adds NVIDIA gpu support to a container
func WithGPUs(opts ...Opts) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		c := &config{}
		for _, o := range opts {
			if err := o(c); err != nil {
				return err
			}
		}
		if c.OCIHookPath == "" {
			path, err := exec.LookPath("containerd")
			if err != nil {
				return err
			}
			c.OCIHookPath = path
		}
		nvidiaPath, err := exec.LookPath(NvidiaCLI)
		if err != nil {
			return err
		}
		if s.Hooks == nil {
			s.Hooks = &specs.Hooks{}
		}
		s.Hooks.Prestart = append(s.Hooks.Prestart, specs.Hook{
			Path: c.OCIHookPath,
			Args: append([]string{
				"containerd",
				"oci-hook",
				"--",
				nvidiaPath,
				// ensures the required kernel modules are properly loaded
				"--load-kmods",
			}, c.args()...),
			Env: os.Environ(),
		})
		return nil
	}
}

type config struct {
	Devices      []string
	Capabilities []Capability
	LoadKmods    bool
	LDCache      string
	LDConfig     string
	Requirements []string
	OCIHookPath  string
}

func (c *config) args() []string {
	var args []string

	if c.LoadKmods {
		args = append(args, "--load-kmods")
	}
	if c.LDCache != "" {
		args = append(args, fmt.Sprintf("--ldcache=%s", c.LDCache))
	}
	args = append(args,
		"configure",
	)
	if len(c.Devices) > 0 {
		args = append(args, fmt.Sprintf("--device=%s", strings.Join(c.Devices, ",")))
	}
	for _, c := range c.Capabilities {
		args = append(args, fmt.Sprintf("--%s", c))
	}
	if c.LDConfig != "" {
		args = append(args, fmt.Sprintf("--ldconfig=%s", c.LDConfig))
	}
	for _, r := range c.Requirements {
		args = append(args, fmt.Sprintf("--require=%s", r))
	}
	args = append(args, "--pid={{pid}}", "{{rootfs}}")
	return args
}

// Opts are options for configuring gpu support
type Opts func(*config) error

// WithDevices adds the provided device indexes to the container
func WithDevices(ids ...int) Opts {
	return func(c *config) error {
		for _, i := range ids {
			c.Devices = append(c.Devices, strconv.Itoa(i))
		}
		return nil
	}
}

// WithDeviceUUIDs adds the specific device UUID to the container
func WithDeviceUUIDs(uuids ...string) Opts {
	return func(c *config) error {
		c.Devices = append(c.Devices, uuids...)
		return nil
	}
}

// WithAllDevices adds all gpus to the container
func WithAllDevices(c *config) error {
	c.Devices = []string{"all"}
	return nil
}

// WithAllCapabilities adds all capabilities to the container for the gpus
func WithAllCapabilities(c *config) error {
	c.Capabilities = AllCaps()
	return nil
}

// WithCapabilities adds the specified capabilities to the container for the gpus
func WithCapabilities(caps ...Capability) Opts {
	return func(c *config) error {
		c.Capabilities = append(c.Capabilities, caps...)
		return nil
	}
}

// WithRequiredCUDAVersion sets the required cuda version
func WithRequiredCUDAVersion(major, minor int) Opts {
	return func(c *config) error {
		c.Requirements = append(c.Requirements, fmt.Sprintf("cuda>=%d.%d", major, minor))
		return nil
	}
}

// WithOCIHookPath sets the hook path for the binary
func WithOCIHookPath(path string) Opts {
	return func(c *config) error {
		c.OCIHookPath = path
		return nil
	}
}

// WithLookupOCIHookPath sets the hook path for the binary via a binary name
func WithLookupOCIHookPath(name string) Opts {
	return func(c *config) error {
		path, err := exec.LookPath(name)
		if err != nil {
			return err
		}
		c.OCIHookPath = path
		return nil
	}
}

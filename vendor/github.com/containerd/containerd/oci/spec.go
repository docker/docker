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

package oci

import (
	"context"
	"path/filepath"
	"runtime"

	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"

	"github.com/containerd/containerd/containers"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	rwm               = "rwm"
	defaultRootfsPath = "rootfs"
)

var (
	defaultUnixEnv = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
)

// Spec is a type alias to the OCI runtime spec to allow third part SpecOpts
// to be created without the "issues" with go vendoring and package imports
type Spec = specs.Spec

// GenerateSpec will generate a default spec from the provided image
// for use as a containerd container
func GenerateSpec(ctx context.Context, client Client, c *containers.Container, opts ...SpecOpts) (*Spec, error) {
	return GenerateSpecWithPlatform(ctx, client, platforms.DefaultString(), c, opts...)
}

// GenerateSpecWithPlatform will generate a default spec from the provided image
// for use as a containerd container in the platform requested.
func GenerateSpecWithPlatform(ctx context.Context, client Client, platform string, c *containers.Container, opts ...SpecOpts) (*Spec, error) {
	var s Spec
	if err := generateDefaultSpecWithPlatform(ctx, platform, c.ID, &s); err != nil {
		return nil, err
	}

	return &s, ApplyOpts(ctx, client, c, &s, opts...)
}

func generateDefaultSpecWithPlatform(ctx context.Context, platform, id string, s *Spec) error {
	plat, err := platforms.Parse(platform)
	if err != nil {
		return err
	}

	if plat.OS == "windows" {
		err = populateDefaultWindowsSpec(ctx, s, id)
	} else {
		err = populateDefaultUnixSpec(ctx, s, id)
		if err == nil && runtime.GOOS == "windows" {
			// To run LCOW we have a Linux and Windows section. Add an empty one now.
			s.Windows = &specs.Windows{}
		}
	}
	return err
}

// ApplyOpts applys the options to the given spec, injecting data from the
// context, client and container instance.
func ApplyOpts(ctx context.Context, client Client, c *containers.Container, s *Spec, opts ...SpecOpts) error {
	for _, o := range opts {
		if err := o(ctx, client, c, s); err != nil {
			return err
		}
	}

	return nil
}

func defaultUnixCaps() []string {
	return []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}
}

func defaultUnixNamespaces() []specs.LinuxNamespace {
	return []specs.LinuxNamespace{
		{
			Type: specs.PIDNamespace,
		},
		{
			Type: specs.IPCNamespace,
		},
		{
			Type: specs.UTSNamespace,
		},
		{
			Type: specs.MountNamespace,
		},
		{
			Type: specs.NetworkNamespace,
		},
	}
}

func populateDefaultUnixSpec(ctx context.Context, s *Spec, id string) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	*s = Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path: defaultRootfsPath,
		},
		Process: &specs.Process{
			Env:             defaultUnixEnv,
			Cwd:             "/",
			NoNewPrivileges: true,
			User: specs.User{
				UID: 0,
				GID: 0,
			},
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    defaultUnixCaps(),
				Permitted:   defaultUnixCaps(),
				Inheritable: defaultUnixCaps(),
				Effective:   defaultUnixCaps(),
			},
			Rlimits: []specs.POSIXRlimit{
				{
					Type: "RLIMIT_NOFILE",
					Hard: uint64(1024),
					Soft: uint64(1024),
				},
			},
		},
		Mounts: []specs.Mount{
			{
				Destination: "/proc",
				Type:        "proc",
				Source:      "proc",
				Options:     []string{"nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/dev",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
			{
				Destination: "/dev/pts",
				Type:        "devpts",
				Source:      "devpts",
				Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
			},
			{
				Destination: "/dev/shm",
				Type:        "tmpfs",
				Source:      "shm",
				Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
			},
			{
				Destination: "/dev/mqueue",
				Type:        "mqueue",
				Source:      "mqueue",
				Options:     []string{"nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/sys",
				Type:        "sysfs",
				Source:      "sysfs",
				Options:     []string{"nosuid", "noexec", "nodev", "ro"},
			},
			{
				Destination: "/run",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
		},
		Linux: &specs.Linux{
			MaskedPaths: []string{
				"/proc/acpi",
				"/proc/asound",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/sys/firmware",
				"/proc/scsi",
			},
			ReadonlyPaths: []string{
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
			CgroupsPath: filepath.Join("/", ns, id),
			Resources: &specs.LinuxResources{
				Devices: []specs.LinuxDeviceCgroup{
					{
						Allow:  false,
						Access: rwm,
					},
				},
			},
			Namespaces: defaultUnixNamespaces(),
		},
	}
	return nil
}

func populateDefaultWindowsSpec(ctx context.Context, s *Spec, id string) error {
	*s = Spec{
		Version: specs.Version,
		Root:    &specs.Root{},
		Process: &specs.Process{
			Cwd: `C:\`,
		},
		Windows: &specs.Windows{},
	}
	return nil
}

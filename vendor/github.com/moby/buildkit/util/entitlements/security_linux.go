package entitlements

import (
	"context"

	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// WithInsecureSpec sets spec with All capability.
func WithInsecureSpec() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		addCaps := []string{
			"CAP_FSETID",
			"CAP_KILL",
			"CAP_FOWNER",
			"CAP_MKNOD",
			"CAP_CHOWN",
			"CAP_DAC_OVERRIDE",
			"CAP_NET_RAW",
			"CAP_SETGID",
			"CAP_SETUID",
			"CAP_SETPCAP",
			"CAP_SETFCAP",
			"CAP_NET_BIND_SERVICE",
			"CAP_SYS_CHROOT",
			"CAP_AUDIT_WRITE",
			"CAP_MAC_ADMIN",
			"CAP_MAC_OVERRIDE",
			"CAP_DAC_READ_SEARCH",
			"CAP_SYS_PTRACE",
			"CAP_SYS_MODULE",
			"CAP_SYSLOG",
			"CAP_SYS_RAWIO",
			"CAP_SYS_ADMIN",
			"CAP_LINUX_IMMUTABLE",
			"CAP_SYS_BOOT",
			"CAP_SYS_NICE",
			"CAP_SYS_PACCT",
			"CAP_SYS_TTY_CONFIG",
			"CAP_SYS_TIME",
			"CAP_WAKE_ALARM",
			"CAP_AUDIT_READ",
			"CAP_AUDIT_CONTROL",
			"CAP_SYS_RESOURCE",
			"CAP_BLOCK_SUSPEND",
			"CAP_IPC_LOCK",
			"CAP_IPC_OWNER",
			"CAP_LEASE",
			"CAP_NET_ADMIN",
			"CAP_NET_BROADCAST",
		}
		for _, cap := range addCaps {
			s.Process.Capabilities.Bounding = append(s.Process.Capabilities.Bounding, cap)
			s.Process.Capabilities.Ambient = append(s.Process.Capabilities.Ambient, cap)
			s.Process.Capabilities.Effective = append(s.Process.Capabilities.Effective, cap)
			s.Process.Capabilities.Inheritable = append(s.Process.Capabilities.Inheritable, cap)
			s.Process.Capabilities.Permitted = append(s.Process.Capabilities.Permitted, cap)
		}
		s.Linux.ReadonlyPaths = []string{}
		s.Linux.MaskedPaths = []string{}
		s.Process.ApparmorProfile = ""

		return nil
	}
}

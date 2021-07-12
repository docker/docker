package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/containers"
	coci "github.com/containerd/containerd/oci"
	"github.com/docker/docker/container"
	"github.com/docker/docker/profiles/seccomp"
	"github.com/sirupsen/logrus"
)

const supportsSeccomp = true

// WithSeccomp sets the seccomp profile
func WithSeccomp(daemon *Daemon, c *container.Container) coci.SpecOpts {
	return func(ctx context.Context, _ coci.Client, _ *containers.Container, s *coci.Spec) error {
		if c.SeccompProfile == "unconfined" {
			return nil
		}
		if c.HostConfig.Privileged {
			return nil
		}
		if !daemon.seccompEnabled {
			if c.SeccompProfile != "" {
				return fmt.Errorf("seccomp is not enabled in your kernel, cannot run a custom seccomp profile")
			}
			logrus.Warn("seccomp is not enabled in your kernel, running container without default profile")
			c.SeccompProfile = "unconfined"
			return nil
		}
		var err error
		switch {
		case c.SeccompProfile != "":
			s.Linux.Seccomp, err = seccomp.LoadProfile(c.SeccompProfile, s)
		case daemon.seccompProfile != nil:
			s.Linux.Seccomp, err = seccomp.LoadProfile(string(daemon.seccompProfile), s)
		default:
			s.Linux.Seccomp, err = seccomp.GetDefaultProfile(s)
		}
		return err
	}
}

package daemon // import "github.com/docker/docker/daemon"

import (
	"testing"

	coci "github.com/containerd/containerd/oci"
	config "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	doci "github.com/docker/docker/oci"
	"github.com/docker/docker/profiles/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
)

func TestWithSeccomp(t *testing.T) {

	type expected struct {
		daemon  *Daemon
		c       *container.Container
		inSpec  coci.Spec
		outSpec coci.Spec
		err     string
		comment string
	}

	for _, x := range []expected{
		{
			comment: "unconfined seccompProfile runs unconfined",
			daemon: &Daemon{
				seccompEnabled: true,
			},
			c: &container.Container{
				SeccompProfile: "unconfined",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec:  doci.DefaultLinuxSpec(),
			outSpec: doci.DefaultLinuxSpec(),
		},
		{
			comment: "privileged container w/ custom profile runs unconfined",
			daemon: &Daemon{
				seccompEnabled: true,
			},
			c: &container.Container{
				SeccompProfile: "{ \"defaultAction\": \"SCMP_ACT_LOG\" }",
				HostConfig: &config.HostConfig{
					Privileged: true,
				},
			},
			inSpec:  doci.DefaultLinuxSpec(),
			outSpec: doci.DefaultLinuxSpec(),
		},
		{
			comment: "privileged container w/ default runs unconfined",
			daemon: &Daemon{
				seccompEnabled: true,
			},
			c: &container.Container{
				SeccompProfile: "",
				HostConfig: &config.HostConfig{
					Privileged: true,
				},
			},
			inSpec:  doci.DefaultLinuxSpec(),
			outSpec: doci.DefaultLinuxSpec(),
		},
		{
			comment: "privileged container w/ daemon profile runs unconfined",
			daemon: &Daemon{
				seccompEnabled: true,
				seccompProfile: []byte("{ \"defaultAction\": \"SCMP_ACT_ERRNO\" }"),
			},
			c: &container.Container{
				SeccompProfile: "",
				HostConfig: &config.HostConfig{
					Privileged: true,
				},
			},
			inSpec:  doci.DefaultLinuxSpec(),
			outSpec: doci.DefaultLinuxSpec(),
		},
		{
			comment: "custom profile when seccomp is disabled returns error",
			daemon: &Daemon{
				seccompEnabled: false,
			},
			c: &container.Container{
				SeccompProfile: "{ \"defaultAction\": \"SCMP_ACT_ERRNO\" }",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec:  doci.DefaultLinuxSpec(),
			outSpec: doci.DefaultLinuxSpec(),
			err:     "seccomp is not enabled in your kernel, cannot run a custom seccomp profile",
		},
		{
			comment: "empty profile name loads default profile",
			daemon: &Daemon{
				seccompEnabled: true,
			},
			c: &container.Container{
				SeccompProfile: "",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec: doci.DefaultLinuxSpec(),
			outSpec: func() coci.Spec {
				s := doci.DefaultLinuxSpec()
				profile, _ := seccomp.GetDefaultProfile(&s)
				s.Linux.Seccomp = profile
				return s
			}(),
		},
		{
			comment: "load container's profile",
			daemon: &Daemon{
				seccompEnabled: true,
			},
			c: &container.Container{
				SeccompProfile: "{ \"defaultAction\": \"SCMP_ACT_ERRNO\" }",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec: doci.DefaultLinuxSpec(),
			outSpec: func() coci.Spec {
				s := doci.DefaultLinuxSpec()
				profile := &specs.LinuxSeccomp{
					DefaultAction: specs.LinuxSeccompAction("SCMP_ACT_ERRNO"),
				}
				s.Linux.Seccomp = profile
				return s
			}(),
		},
		{
			comment: "load daemon's profile",
			daemon: &Daemon{
				seccompEnabled: true,
				seccompProfile: []byte("{ \"defaultAction\": \"SCMP_ACT_ERRNO\" }"),
			},
			c: &container.Container{
				SeccompProfile: "",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec: doci.DefaultLinuxSpec(),
			outSpec: func() coci.Spec {
				s := doci.DefaultLinuxSpec()
				profile := &specs.LinuxSeccomp{
					DefaultAction: specs.LinuxSeccompAction("SCMP_ACT_ERRNO"),
				}
				s.Linux.Seccomp = profile
				return s
			}(),
		},
		{
			comment: "load prioritise container profile over daemon's",
			daemon: &Daemon{
				seccompEnabled: true,
				seccompProfile: []byte("{ \"defaultAction\": \"SCMP_ACT_ERRNO\" }"),
			},
			c: &container.Container{
				SeccompProfile: "{ \"defaultAction\": \"SCMP_ACT_LOG\" }",
				HostConfig: &config.HostConfig{
					Privileged: false,
				},
			},
			inSpec: doci.DefaultLinuxSpec(),
			outSpec: func() coci.Spec {
				s := doci.DefaultLinuxSpec()
				profile := &specs.LinuxSeccomp{
					DefaultAction: specs.LinuxSeccompAction("SCMP_ACT_LOG"),
				}
				s.Linux.Seccomp = profile
				return s
			}(),
		},
	} {
		t.Run(x.comment, func(t *testing.T) {
			opts := WithSeccomp(x.daemon, x.c)
			err := opts(nil, nil, nil, &x.inSpec)

			assert.DeepEqual(t, x.inSpec, x.outSpec)
			if x.err != "" {
				assert.Error(t, err, x.err)
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

package daemon

import (
	"context"
	"errors"

	containerd "github.com/containerd/containerd/v2/client"
	coci "github.com/containerd/containerd/v2/pkg/oci"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/pkg/oci/caps"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func getUserFromContainerd(ctx context.Context, containerdCli *containerd.Client, ec *container.ExecConfig) (specs.User, error) {
	ctr, err := containerdCli.LoadContainer(ctx, ec.Container.ID)
	if err != nil {
		return specs.User{}, err
	}

	cinfo, err := ctr.Info(ctx)
	if err != nil {
		return specs.User{}, err
	}

	spec, err := ctr.Spec(ctx)
	if err != nil {
		return specs.User{}, err
	}

	opts := []coci.SpecOpts{
		coci.WithUser(ec.User),
		coci.WithAdditionalGIDs(ec.User),
		coci.WithAppendAdditionalGroups(ec.Container.HostConfig.GroupAdd...),
	}
	for _, opt := range opts {
		if err := opt(ctx, containerdCli, &cinfo, spec); err != nil {
			return specs.User{}, err
		}
	}

	return spec.Process.User, nil
}

func (daemon *Daemon) execSetPlatformOpt(ctx context.Context, daemonCfg *config.Config, ec *container.ExecConfig, p *specs.Process) error {
	if ec.User != "" {
		var user specs.User
		var err error
		if daemon.UsesSnapshotter() {
			user, err = getUserFromContainerd(ctx, daemon.containerdClient, ec)
		} else {
			user, err = getUser(ec.Container, ec.User)
		}
		if err != nil {
			return err
		}
		// Preserve the umask inherited from the container's process spec.
		user.Umask = p.User.Umask
		p.User = user
	}

	if ec.Privileged {
		p.Capabilities = &specs.LinuxCapabilities{
			Bounding:  caps.GetAllCapabilities(),
			Permitted: caps.GetAllCapabilities(),
			Effective: caps.GetAllCapabilities(),
		}
	}

	if appArmorSupported() {
		var appArmorProfile string
		if ec.Container.AppArmorProfile != "" {
			appArmorProfile = ec.Container.AppArmorProfile
		} else if ec.Container.HostConfig.Privileged {
			// `docker exec --privileged` does not currently disable AppArmor
			// profiles. Privileged configuration of the container is inherited
			appArmorProfile = unconfinedAppArmorProfile
		} else {
			appArmorProfile = defaultAppArmorProfile
		}

		if appArmorProfile == defaultAppArmorProfile {
			// Unattended upgrades and other fun services can unload AppArmor
			// profiles inadvertently. Since we cannot store our profile in
			// /etc/apparmor.d, nor can we practically add other ways of
			// telling the system to keep our profile loaded, in order to make
			// sure that we keep the default profile enabled we load it again
			// if it is missing.
			if err := daemon.loadDefaultAppArmorProfileIfMissing(); err != nil {
				return err
			}
		}
		p.ApparmorProfile = appArmorProfile
	} else {
		// If AppArmor is not supported but a profile was specified, return an error
		if ec.Container.AppArmorProfile != "" {
			return errors.New("AppArmor is not supported on this host, but the profile '" + ec.Container.AppArmorProfile + "' was specified")
		}
	}

	s := &specs.Spec{Process: p}
	return withRlimits(daemon, daemonCfg, ec.Container)(ctx, nil, nil, s)
}

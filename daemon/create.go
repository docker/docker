package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/runconfig"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type createOpts struct {
	params                  types.ContainerCreateConfig
	managed                 bool
	ignoreImagesArgsEscaped bool
}

// CreateManagedContainer creates a container that is managed by a Service
func (daemon *Daemon) CreateManagedContainer(params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(createOpts{
		params:                  params,
		managed:                 true,
		ignoreImagesArgsEscaped: false})
}

// ContainerCreate creates a regular container
func (daemon *Daemon) ContainerCreate(params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(createOpts{
		params:                  params,
		managed:                 false,
		ignoreImagesArgsEscaped: false})
}

// ContainerCreateIgnoreImagesArgsEscaped creates a regular container. This is called from the builder RUN case
// and ensures that we do not take the images ArgsEscaped
func (daemon *Daemon) ContainerCreateIgnoreImagesArgsEscaped(params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(createOpts{
		params:                  params,
		managed:                 false,
		ignoreImagesArgsEscaped: true})
}

func (daemon *Daemon) containerCreate(opts createOpts) (containertypes.ContainerCreateCreatedBody, error) {
	start := time.Now()
	if opts.params.Config == nil {
		return containertypes.ContainerCreateCreatedBody{}, errdefs.InvalidParameter(errors.New("Config cannot be empty in order to create a container"))
	}

	warnings, err := daemon.verifyContainerSettings(opts.params.HostConfig, opts.params.Config, false)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, errdefs.InvalidParameter(err)
	}

	if opts.params.Platform == nil && opts.params.Config.Image != "" {
		if img, _ := daemon.imageService.GetImage(opts.params.Config.Image, opts.params.Platform); img != nil {
			p := platforms.DefaultSpec()
			imgPlat := v1.Platform{
				OS:           img.OS,
				Architecture: img.Architecture,
				Variant:      img.Variant,
			}

			if !images.OnlyPlatformWithFallback(p).Match(imgPlat) {
				warnings = append(warnings, fmt.Sprintf("The requested image's platform (%s) does not match the detected host platform (%s) and no specific platform was requested", platforms.Format(imgPlat), platforms.Format(p)))
			}
		}
	}

	err = verifyNetworkingConfig(opts.params.NetworkingConfig)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, errdefs.InvalidParameter(err)
	}

	if opts.params.HostConfig == nil {
		opts.params.HostConfig = &containertypes.HostConfig{}
	}
	err = daemon.adaptContainerSettings(opts.params.HostConfig, opts.params.AdjustCPUShares)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, errdefs.InvalidParameter(err)
	}

	ctr, err := daemon.create(opts)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, err
	}
	containerActions.WithValues("create").UpdateSince(start)

	if warnings == nil {
		warnings = make([]string, 0) // Create an empty slice to avoid https://github.com/moby/moby/issues/38222
	}

	return containertypes.ContainerCreateCreatedBody{ID: ctr.ID, Warnings: warnings}, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) create(opts createOpts) (retC *container.Container, retErr error) {
	var (
		ctr   *container.Container
		img   *image.Image
		imgID image.ID
		err   error
	)

	os := runtime.GOOS
	if opts.params.Config.Image != "" {
		img, err = daemon.imageService.GetImage(opts.params.Config.Image, opts.params.Platform)
		if err != nil {
			return nil, err
		}
		if img.OS != "" {
			os = img.OS
		}
		imgID = img.ID()
		if isWindows && img.OS == "linux" {
			return nil, errors.New("operating system on which parent image was created is not Windows")
		}
	} else if isWindows {
		os = "linux" // 'scratch' case.
	}

	// On WCOW, if are not being invoked by the builder to create this container (where
	// ignoreImagesArgEscaped will be true) - if the image already has its arguments escaped,
	// ensure that this is replicated across to the created container to avoid double-escaping
	// of the arguments/command line when the runtime attempts to run the container.
	if os == "windows" && !opts.ignoreImagesArgsEscaped && img != nil && img.RunConfig().ArgsEscaped {
		opts.params.Config.ArgsEscaped = true
	}

	if err := daemon.mergeAndVerifyConfig(opts.params.Config, img); err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	if err := daemon.mergeAndVerifyLogConfig(&opts.params.HostConfig.LogConfig); err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	if ctr, err = daemon.newContainer(opts.params.Name, os, opts.params.Config, opts.params.HostConfig, imgID, opts.managed); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.cleanupContainer(ctr, true, true); err != nil {
				logrus.Errorf("failed to cleanup container on create error: %v", err)
			}
		}
	}()

	if err := daemon.setSecurityOptions(ctr, opts.params.HostConfig); err != nil {
		return nil, err
	}

	ctr.HostConfig.StorageOpt = opts.params.HostConfig.StorageOpt

	// Set RWLayer for container after mount labels have been set
	rwLayer, err := daemon.imageService.CreateLayer(ctr, setupInitLayer(daemon.idMapping))
	if err != nil {
		return nil, errdefs.System(err)
	}
	ctr.RWLayer = rwLayer

	if err := idtools.MkdirAndChown(ctr.Root, 0701, idtools.CurrentIdentity()); err != nil {
		return nil, err
	}
	if err := idtools.MkdirAndChown(ctr.CheckpointDir(), 0700, idtools.CurrentIdentity()); err != nil {
		return nil, err
	}

	if err := daemon.setHostConfig(ctr, opts.params.HostConfig); err != nil {
		return nil, err
	}

	if err := daemon.createContainerOSSpecificSettings(ctr, opts.params.Config, opts.params.HostConfig); err != nil {
		return nil, err
	}

	var endpointsConfigs map[string]*networktypes.EndpointSettings
	if opts.params.NetworkingConfig != nil {
		endpointsConfigs = opts.params.NetworkingConfig.EndpointsConfig
	}
	// Make sure NetworkMode has an acceptable value. We do this to ensure
	// backwards API compatibility.
	runconfig.SetDefaultNetModeIfBlank(ctr.HostConfig)

	daemon.updateContainerNetworkSettings(ctr, endpointsConfigs)
	if err := daemon.Register(ctr); err != nil {
		return nil, err
	}
	stateCtr.set(ctr.ID, "stopped")
	daemon.LogContainerEvent(ctr, "create")
	return ctr, nil
}

func toHostConfigSelinuxLabels(labels []string) []string {
	for i, l := range labels {
		labels[i] = "label=" + l
	}
	return labels
}

func (daemon *Daemon) generateSecurityOpt(hostConfig *containertypes.HostConfig) ([]string, error) {
	for _, opt := range hostConfig.SecurityOpt {
		con := strings.Split(opt, "=")
		if con[0] == "label" {
			// Caller overrode SecurityOpts
			return nil, nil
		}
	}
	ipcMode := hostConfig.IpcMode
	pidMode := hostConfig.PidMode
	privileged := hostConfig.Privileged
	if ipcMode.IsHost() || pidMode.IsHost() || privileged {
		return toHostConfigSelinuxLabels(selinux.DisableSecOpt()), nil
	}

	var ipcLabel []string
	var pidLabel []string
	ipcContainer := ipcMode.Container()
	pidContainer := pidMode.Container()
	if ipcContainer != "" {
		c, err := daemon.GetContainer(ipcContainer)
		if err != nil {
			return nil, err
		}
		ipcLabel, err = selinux.DupSecOpt(c.ProcessLabel)
		if err != nil {
			return nil, err
		}
		if pidContainer == "" {
			return toHostConfigSelinuxLabels(ipcLabel), err
		}
	}
	if pidContainer != "" {
		c, err := daemon.GetContainer(pidContainer)
		if err != nil {
			return nil, err
		}

		pidLabel, err = selinux.DupSecOpt(c.ProcessLabel)
		if err != nil {
			return nil, err
		}
		if ipcContainer == "" {
			return toHostConfigSelinuxLabels(pidLabel), err
		}
	}

	if pidLabel != nil && ipcLabel != nil {
		for i := 0; i < len(pidLabel); i++ {
			if pidLabel[i] != ipcLabel[i] {
				return nil, fmt.Errorf("--ipc and --pid containers SELinux labels aren't the same")
			}
		}
		return toHostConfigSelinuxLabels(pidLabel), nil
	}
	return nil, nil
}

func (daemon *Daemon) mergeAndVerifyConfig(config *containertypes.Config, img *image.Image) error {
	if img != nil && img.Config != nil {
		if err := merge(config, img.Config); err != nil {
			return err
		}
	}
	// Reset the Entrypoint if it is [""]
	if len(config.Entrypoint) == 1 && config.Entrypoint[0] == "" {
		config.Entrypoint = nil
	}
	if len(config.Entrypoint) == 0 && len(config.Cmd) == 0 {
		return fmt.Errorf("No command specified")
	}
	return nil
}

// Checks if the client set configurations for more than one network while creating a container
// Also checks if the IPAMConfig is valid
func verifyNetworkingConfig(nwConfig *networktypes.NetworkingConfig) error {
	if nwConfig == nil || len(nwConfig.EndpointsConfig) == 0 {
		return nil
	}
	if len(nwConfig.EndpointsConfig) > 1 {
		l := make([]string, 0, len(nwConfig.EndpointsConfig))
		for k := range nwConfig.EndpointsConfig {
			l = append(l, k)
		}
		return errors.Errorf("Container cannot be connected to network endpoints: %s", strings.Join(l, ", "))
	}

	for k, v := range nwConfig.EndpointsConfig {
		if v == nil {
			return errdefs.InvalidParameter(errors.Errorf("no EndpointSettings for %s", k))
		}
		if v.IPAMConfig != nil {
			if v.IPAMConfig.IPv4Address != "" && net.ParseIP(v.IPAMConfig.IPv4Address).To4() == nil {
				return errors.Errorf("invalid IPv4 address: %s", v.IPAMConfig.IPv4Address)
			}
			if v.IPAMConfig.IPv6Address != "" {
				n := net.ParseIP(v.IPAMConfig.IPv6Address)
				// if the address is an invalid network address (ParseIP == nil) or if it is
				// an IPv4 address (To4() != nil), then it is an invalid IPv6 address
				if n == nil || n.To4() != nil {
					return errors.Errorf("invalid IPv6 address: %s", v.IPAMConfig.IPv6Address)
				}
			}
		}
	}
	return nil
}

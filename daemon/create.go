package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"

	cimages "github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/runconfig"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type createOpts struct {
	params                  types.ContainerCreateConfig
	rImage                  images.RuntimeImage
	managed                 bool
	ignoreImagesArgsEscaped bool
}

// CreateManagedContainer creates a container that is managed by a Service
func (daemon *Daemon) CreateManagedContainer(ctx context.Context, params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(ctx, createOpts{
		params:                  params,
		managed:                 true,
		ignoreImagesArgsEscaped: false})
}

// ContainerCreate creates a regular container
func (daemon *Daemon) ContainerCreate(ctx context.Context, params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(ctx, createOpts{
		params:                  params,
		managed:                 false,
		ignoreImagesArgsEscaped: false})
}

// ContainerCreateIgnoreImagesArgsEscaped creates a regular container. This is called from the builder RUN case
// and ensures that we do not take the images ArgsEscaped
func (daemon *Daemon) ContainerCreateIgnoreImagesArgsEscaped(ctx context.Context, params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(ctx, createOpts{
		params:                  params,
		managed:                 false,
		ignoreImagesArgsEscaped: true})
}

func (daemon *Daemon) containerCreate(ctx context.Context, opts createOpts) (containertypes.ContainerCreateCreatedBody, error) {
	var (
		err   error
		start = time.Now()
	)

	if opts.params.Config == nil {
		return containertypes.ContainerCreateCreatedBody{}, errdefs.InvalidParameter(errors.New("Config cannot be empty in order to create a container"))
	}

	if opts.params.Descriptor == nil {
		if opts.params.Config.RuntimeImage != nil {
			opts.params.Descriptor = opts.params.Config.RuntimeImage
		} else if opts.params.Config.Image != "" {
			desc, err := daemon.imageService.ResolveImage(ctx, opts.params.Config.Image)
			if err != nil {
				return containertypes.ContainerCreateCreatedBody{}, errors.Wrapf(err, "failed to resolve image %s", opts.params.Config.Image)
			}
			opts.params.Descriptor = &desc
		}
	}

	if opts.params.Descriptor != nil {
		opts.rImage, err = daemon.imageService.ResolveRuntimeImage(ctx, *opts.params.Descriptor)
		if err != nil {
			return containertypes.ContainerCreateCreatedBody{}, err
		}
	} else {
		// TODO(containerd): move this logic to image service
		opts.rImage.Platform = platforms.DefaultSpec()

		// This mean scratch. On Windows, we can safely assume that this is a linux
		// container. On other platforms, it's the host OS (which it already is)
		if opts.rImage.Platform.OS == "windows" && system.LCOWSupported() {
			opts.rImage.Platform.OS = "linux"
			opts.rImage.Platform.OSVersion = ""
			opts.rImage.Platform.OSFeatures = []string{}
		}
	}

	warnings, err := daemon.verifyContainerSettings(opts.rImage.Platform, opts.params.HostConfig, opts.params.Config, false)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, errdefs.InvalidParameter(err)
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

	container, err := daemon.create(ctx, opts)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, err
	}
	containerActions.WithValues("create").UpdateSince(start)

	if warnings == nil {
		warnings = make([]string, 0) // Create an empty slice to avoid https://github.com/moby/moby/issues/38222
	}

	return containertypes.ContainerCreateCreatedBody{ID: container.ID, Warnings: warnings}, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) create(ctx context.Context, opts createOpts) (retC *container.Container, retErr error) {
	if err := daemon.mergeAndVerifyConfig(ctx, opts.params.Config, opts.rImage.ConfigBytes); err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	if err := daemon.mergeAndVerifyLogConfig(&opts.params.HostConfig.LogConfig); err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	container, err := daemon.newContainer(opts.params.Name, opts.params.Config, opts.params.HostConfig, opts.rImage, opts.managed)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.cleanupContainer(container, true, true); err != nil {
				logrus.Errorf("failed to cleanup container on create error: %v", err)
			}
		}
	}()

	if err := daemon.setSecurityOptions(container, opts.params.HostConfig); err != nil {
		return nil, err
	}

	container.HostConfig.StorageOpt = opts.params.HostConfig.StorageOpt

	// Fixes: https://github.com/moby/moby/issues/34074 and
	// https://github.com/docker/for-win/issues/999.
	// Merge the daemon's storage options if they aren't already present. We only
	// do this on Windows as there's no effective sandbox size limit other than
	// physical on Linux.
	if runtime.GOOS == "windows" {
		if container.HostConfig.StorageOpt == nil {
			container.HostConfig.StorageOpt = make(map[string]string)
		}
		for _, v := range daemon.configStore.GraphOptions {
			opt := strings.SplitN(v, "=", 2)
			if _, ok := container.HostConfig.StorageOpt[opt[0]]; !ok {
				container.HostConfig.StorageOpt[opt[0]] = opt[1]
			}
		}
	}

	container.RWLayer, err = daemon.createRWLayer(ctx, opts.rImage, container)
	if err != nil {
		return nil, errdefs.System(err)
	}

	rootIDs := daemon.idMapping.RootPair()

	if err := idtools.MkdirAndChown(container.Root, 0700, rootIDs); err != nil {
		return nil, err
	}
	if err := idtools.MkdirAndChown(container.CheckpointDir(), 0700, rootIDs); err != nil {
		return nil, err
	}

	if err := daemon.setHostConfig(container, opts.params.HostConfig); err != nil {
		return nil, err
	}

	// TODO(containerd): Add containerd GC and Docker layer labels
	if err := daemon.createContainerOSSpecificSettings(container, opts.params.Config, opts.params.HostConfig); err != nil {
		return nil, err
	}

	var endpointsConfigs map[string]*networktypes.EndpointSettings
	if opts.params.NetworkingConfig != nil {
		endpointsConfigs = opts.params.NetworkingConfig.EndpointsConfig
	}
	// Make sure NetworkMode has an acceptable value. We do this to ensure
	// backwards API compatibility.
	runconfig.SetDefaultNetModeIfBlank(container.HostConfig)

	daemon.updateContainerNetworkSettings(container, endpointsConfigs)
	if err := daemon.Register(container); err != nil {
		return nil, err
	}
	stateCtr.set(container.ID, "stopped")
	daemon.LogContainerEvent(container, "create")
	return container, nil
}

func (daemon *Daemon) createRWLayer(ctx context.Context, img images.RuntimeImage, container *container.Container) (layer.RWLayer, error) {
	var chainID digest.Digest
	if img.Config.Digest != "" {
		cs := daemon.containerdCli.ContentStore()
		diffIDs, err := cimages.RootFS(ctx, cs, img.Config)
		if err != nil {
			return nil, errors.Wrap(err, "failed to resolve rootfs")
		}

		chainID = identity.ChainID(diffIDs)
	}

	ls, err := daemon.imageService.GetImageBackend(img)
	if err != nil {
		return nil, err
	}

	rwLayerOpts := &layer.CreateRWLayerOpts{
		MountLabel: container.MountLabel,
		StorageOpt: container.HostConfig.StorageOpt,
		InitFunc:   setupInitLayer(daemon.idMapping),
	}

	return ls.CreateRWLayer(container.ID, layer.ChainID(chainID), rwLayerOpts)
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
		return toHostConfigSelinuxLabels(label.DisableSecOpt()), nil
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
		ipcLabel, err = label.DupSecOpt(c.ProcessLabel)
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

		pidLabel, err = label.DupSecOpt(c.ProcessLabel)
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

func (daemon *Daemon) mergeAndVerifyConfig(ctx context.Context, config *containertypes.Config, configBytes []byte) error {
	if len(configBytes) != 0 {
		// Only parse out the config key
		var imgConfig struct {
			Config *containertypes.Config `json:"config,omitempty"`
		}
		if err := json.Unmarshal(configBytes, &imgConfig); err != nil {
			return errors.Wrap(err, "failed to parse image config")
		}

		if imgConfig.Config != nil {
			if err := merge(config, imgConfig.Config); err != nil {
				return err
			}
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

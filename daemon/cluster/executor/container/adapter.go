package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	gogotypes "github.com/gogo/protobuf/types"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon"
	"github.com/moby/moby/v2/daemon/cluster/convert"
	executorpkg "github.com/moby/moby/v2/daemon/cluster/executor"
	containerpkg "github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/libnetwork"
	networkSettings "github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	volumeopts "github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/moby/swarmkit/v2/agent/exec"
	"github.com/moby/swarmkit/v2/api"
	swarmlog "github.com/moby/swarmkit/v2/log"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

// nodeAttachmentReadyInterval is the interval to poll
const nodeAttachmentReadyInterval = 100 * time.Millisecond

// containerAdapter conducts remote operations for a container. All calls
// are mostly naked calls to the client API, seeded with information from
// containerConfig.
type containerAdapter struct {
	backend       executorpkg.Backend
	imageBackend  executorpkg.ImageBackend
	volumeBackend executorpkg.VolumeBackend
	container     *containerConfig
	dependencies  exec.DependencyGetter
}

func newContainerAdapter(b executorpkg.Backend, i executorpkg.ImageBackend, v executorpkg.VolumeBackend, task *api.Task, node *api.NodeDescription, dependencies exec.DependencyGetter) (*containerAdapter, error) {
	ctnr, err := newContainerConfig(task, node)
	if err != nil {
		return nil, err
	}

	return &containerAdapter{
		container:     ctnr,
		backend:       b,
		imageBackend:  i,
		volumeBackend: v,
		dependencies:  dependencies,
	}, nil
}

// imageExists reports whether the image the container is configured with is
// present in the local image store. An error other than "not found" is not
// conclusive, so the image is reported as present and the container create
// is left to surface the error.
func (c *containerAdapter) imageExists(ctx context.Context) bool {
	_, err := c.imageBackend.GetImage(ctx, c.container.image(), imagebackend.GetImageOpts{})
	return !cerrdefs.IsNotFound(err)
}

func (c *containerAdapter) pullImage(ctx context.Context) error {
	spec := c.container.spec()

	// Skip pulling if the image is referenced by image ID.
	if _, err := digest.Parse(spec.Image); err == nil {
		return nil
	}

	// Skip pulling if the image is referenced by digest and already
	// exists locally.
	named, err := reference.ParseNormalizedNamed(spec.Image)
	if err == nil {
		if _, ok := named.(reference.Canonical); ok {
			_, err := c.imageBackend.GetImage(ctx, spec.Image, imagebackend.GetImageOpts{})
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if err == nil {
				return nil
			}
		}
	}

	// if the image needs to be pulled, the auth config will be retrieved and updated
	var encodedAuthConfig string
	if spec.PullOptions != nil {
		encodedAuthConfig = spec.PullOptions.RegistryAuth
	}

	authConfig := &registry.AuthConfig{}
	if encodedAuthConfig != "" {
		if err := json.NewDecoder(base64.NewDecoder(base64.URLEncoding, strings.NewReader(encodedAuthConfig))).Decode(authConfig); err != nil {
			swarmlog.G(ctx).Warnf("invalid authconfig: %v", err)
		}
	}

	pr, pw := io.Pipe()
	metaHeaders := map[string][]string{}
	go func() {
		// TODO LCOW Support: This will need revisiting as
		// the stack is built up to include LCOW support for swarm.

		// Make sure the image has a tag, otherwise it will pull all tags.
		ref := reference.TagNameOnly(named)
		err := c.imageBackend.PullImage(ctx, ref, imagebackend.PullOptions{
			MetaHeaders: metaHeaders,
			AuthConfig:  authConfig,
			OutStream:   pw,
		})
		pw.CloseWithError(err)
	}()

	dec := json.NewDecoder(pr)
	dec.UseNumber()
	m := map[string]any{}
	spamLimiter := rate.NewLimiter(rate.Every(time.Second), 1)

	lastStatus := ""
	for {
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		l := swarmlog.G(ctx)
		// limit pull progress logs unless the status changes
		if spamLimiter.Allow() || lastStatus != m["status"] {
			// if we have progress details, we have everything we need
			if progress, ok := m["progressDetail"].(map[string]any); ok {
				// first, log the image and status
				l = l.WithFields(log.Fields{
					"image":  c.container.image(),
					"status": m["status"],
				})
				// then, if we have progress, log the progress
				if progress["current"] != nil && progress["total"] != nil {
					l = l.WithFields(log.Fields{
						"current": progress["current"],
						"total":   progress["total"],
					})
				}
			}
			l.Debug("pull in progress")
		}
		// sometimes, we get no useful information at all, and add no fields
		if status, ok := m["status"].(string); ok {
			lastStatus = status
		}
	}

	// if the final stream object contained an error, return it
	if errMsg, ok := m["error"]; ok {
		return fmt.Errorf("%v", errMsg)
	}
	return nil
}

// waitNodeAttachments validates that NetworkAttachments exist on this node
// for every network in use by this task. It blocks until the network
// attachments are ready, or the context times out. If it returns nil, then the
// node's network attachments are all there.
func (c *containerAdapter) waitNodeAttachments(ctx context.Context) error {
	// to do this, we're going to get the attachment store and try getting the
	// IP address for each network. if any network comes back not existing,
	// we'll wait and try again.
	attachmentStore := c.backend.GetAttachmentStore()
	if attachmentStore == nil {
		return errors.New("error getting attachment store")
	}

	// essentially, we're long-polling here. this is really sub-optimal, but a
	// better solution based off signaling channels would require a more
	// substantial rearchitecture and probably not be worth our time in terms
	// of performance gains.
	poll := time.NewTicker(nodeAttachmentReadyInterval)
	defer poll.Stop()
	for {
		// set a flag ready to true. if we try to get a network IP that doesn't
		// exist yet, we will set this flag to "false"
		ready := true
		for _, nw := range c.container.networks {
			// we only need node attachments (IP address) for overlay networks
			// TODO(dperny): unsure if this will work with other network
			// drivers, but i also don't think other network drivers use the
			// node attachment IP address.
			if nw.DriverState.Name == "overlay" {
				if _, exists := attachmentStore.GetIPForNetwork(nw.ID); !exists {
					ready = false
				}
			}
		}

		// if everything is ready here, then we can just return no error
		if ready {
			return nil
		}

		// otherwise, try polling again, or wait for context canceled.
		select {
		case <-ctx.Done():
			return errors.New("node is missing network attachments, ip addresses may be exhausted")
		case <-poll.C:
		}
	}
}

func (c *containerAdapter) createNetworks(ctx context.Context) error {
	for name, nw := range c.container.networks {
		ncr := networkCreateRequest(name, nw)
		if err := c.backend.CreateManagedNetwork(ncr); err != nil { // todo name missing
			if _, ok := err.(libnetwork.NetworkNameError); ok {
				continue
			}
			// We will continue if CreateManagedNetwork returns PredefinedNetworkError error.
			// Other callers still can treat it as Error.
			if _, ok := err.(daemon.PredefinedNetworkError); ok {
				continue
			}
			return err
		}
	}

	return nil
}

func (c *containerAdapter) removeNetworks(ctx context.Context) error {
	var (
		activeEndpointsError *libnetwork.ActiveEndpointsError
		errNoSuchNetwork     libnetwork.ErrNoSuchNetwork
	)

	for name, nw := range c.container.networks {
		if err := c.backend.DeleteManagedNetwork(nw.ID); err != nil {
			switch {
			case errors.As(err, &activeEndpointsError):
				continue
			case errors.As(err, &errNoSuchNetwork):
				continue
			default:
				swarmlog.G(ctx).Errorf("network %s remove failed: %v", name, err)
				return err
			}
		}
	}

	return nil
}

func (c *containerAdapter) networkAttach(ctx context.Context) error {
	config := c.container.createNetworkingConfig(c.backend)

	var (
		networkName string
		networkID   string
	)

	if config != nil {
		for n, epConfig := range config.EndpointsConfig {
			networkName = n
			networkID = epConfig.NetworkID
			break
		}
	}

	return c.backend.UpdateAttachment(networkName, networkID, c.container.networkAttachmentContainerID(), config)
}

func (c *containerAdapter) waitForDetach(ctx context.Context) error {
	config := c.container.createNetworkingConfig(c.backend)

	var (
		networkName string
		networkID   string
	)

	if config != nil {
		for n, epConfig := range config.EndpointsConfig {
			networkName = n
			networkID = epConfig.NetworkID
			break
		}
	}

	return c.backend.WaitForDetachment(ctx, networkName, networkID, c.container.taskID(), c.container.networkAttachmentContainerID())
}

func (c *containerAdapter) create(ctx context.Context) error {
	hostConfig := c.container.hostConfig(c.dependencies.Volumes())
	netConfig := c.container.createNetworkingConfig(c.backend)

	// We need to make sure no empty string or "default" NetworkMode is
	// provided to the daemon as it doesn't support them.
	//
	// This is in line with what the ContainerCreate API endpoint does, but
	// unlike that endpoint we can't do that in the ServiceCreate endpoint as
	// the cluster leader and the current node might not be running on the same
	// OS. Since the normalized value isn't the same on Windows and Linux, we
	// need to make this normalization happen once we're sure we won't make a
	// cross-OS API call.
	if hostConfig.NetworkMode == "" || hostConfig.NetworkMode.IsDefault() {
		hostConfig.NetworkMode = networkSettings.DefaultNetwork
		if v, ok := netConfig.EndpointsConfig[network.NetworkDefault]; ok {
			delete(netConfig.EndpointsConfig, network.NetworkDefault)
			netConfig.EndpointsConfig[networkSettings.DefaultNetwork] = v
		}
	}

	var cr containertypes.CreateResponse
	var err error
	if cr, err = c.backend.CreateManagedContainer(ctx, backend.ContainerCreateConfig{
		Name:       c.container.name(),
		Config:     c.container.config(),
		HostConfig: hostConfig,
		// Use the first network in container create
		NetworkingConfig: netConfig,
	}); err != nil {
		return err
	}

	ctr := c.container.task.Spec.GetContainer()
	if ctr == nil {
		return errors.New("unable to get container from task spec")
	}

	if err := c.backend.SetContainerDependencyStore(cr.ID, c.dependencies); err != nil {
		return err
	}

	// configure secrets
	secretRefs := convert.SecretReferencesFromGRPC(ctr.Secrets)
	if err := c.backend.SetContainerSecretReferences(cr.ID, secretRefs); err != nil {
		return err
	}

	configRefs := convert.ConfigReferencesFromGRPC(ctr.Configs)
	if err := c.backend.SetContainerConfigReferences(cr.ID, configRefs); err != nil {
		return err
	}

	return c.backend.UpdateContainerServiceConfig(cr.ID, c.container.serviceConfig())
}

// checkMounts ensures that the provided mounts won't have any host-specific
// problems at start up. For example, we disallow bind mounts without an
// existing path unless creation was explicitly requested.
func (c *containerAdapter) checkMounts() error {
	spec := c.container.spec()
	for _, mount := range spec.Mounts {
		switch mount.Type {
		case api.MountTypeBind:
			if mount.BindOptions != nil && mount.BindOptions.CreateMountpoint {
				continue
			}
			if _, err := os.Stat(mount.Source); os.IsNotExist(err) {
				return fmt.Errorf("invalid bind mount source, source path not found: %s", mount.Source)
			}
		default:
			// TODO(thaJeztah): make switch exhaustive; add api.MountTypeVolume, api.MountTypeTmpfs, api.MountTypeNamedPipe, api.MountTypeCluster
		}
	}

	return nil
}

func (c *containerAdapter) start(ctx context.Context) error {
	if err := c.checkMounts(); err != nil {
		return err
	}

	return c.backend.ContainerStart(ctx, c.container.name(), "", "")
}

func (c *containerAdapter) inspect(ctx context.Context) (containertypes.InspectResponse, error) {
	cs, _, err := c.backend.ContainerInspect(ctx, c.container.name(), backend.ContainerInspectOptions{})
	if ctx.Err() != nil {
		return containertypes.InspectResponse{}, ctx.Err()
	}
	if err != nil {
		return containertypes.InspectResponse{}, err
	}
	return *cs, nil
}

// events issues a call to the events API and returns a channel with all
// events. The stream of events can be shutdown by cancelling the context.
func (c *containerAdapter) events(ctx context.Context) <-chan events.Message {
	swarmlog.G(ctx).Debugf("waiting on events")

	// Discard buffered events; we don't provide since/until filters and use live events only.
	_, l := c.backend.SubscribeToEvents(time.Time{}, time.Time{}, c.container.eventFilter())
	eventsq := make(chan events.Message)

	go func() {
		defer close(eventsq)
		defer c.backend.UnsubscribeFromEvents(l)

		for {
			select {
			case ev, ok := <-l:
				if !ok {
					return
				}

				jev, ok := ev.(events.Message)
				if !ok {
					swarmlog.G(ctx).Warnf("unexpected event message: %q", ev)
					continue
				}
				select {
				case eventsq <- jev:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return eventsq
}

func (c *containerAdapter) wait(ctx context.Context) (<-chan containerpkg.StateStatus, error) {
	return c.backend.ContainerWait(ctx, c.container.nameOrID(), containertypes.WaitConditionNotRunning)
}

func (c *containerAdapter) shutdown(ctx context.Context) error {
	options := backend.ContainerStopOptions{}
	// Default stop grace period to nil (daemon will use the stopTimeout of the container)
	if spec := c.container.spec(); spec.StopGracePeriod != nil {
		timeout := int(spec.StopGracePeriod.Seconds)
		options.Timeout = &timeout
	}
	return c.backend.ContainerStop(ctx, c.container.name(), options)
}

func (c *containerAdapter) terminate(ctx context.Context) error {
	return c.backend.ContainerKill(ctx, c.container.name(), syscall.SIGKILL.String())
}

func (c *containerAdapter) remove(ctx context.Context) error {
	return c.backend.ContainerRm(c.container.name(), &backend.ContainerRmConfig{
		RemoveVolume: true,
		ForceRemove:  true,
	})
}

func (c *containerAdapter) createVolumes(ctx context.Context) error {
	// Create plugin volumes that are embedded inside a Mount
	for _, mount := range c.container.task.Spec.GetContainer().Mounts {
		if mount.Type != api.MountTypeVolume {
			continue
		}

		if mount.VolumeOptions == nil {
			continue
		}

		if mount.VolumeOptions.DriverConfig == nil {
			continue
		}

		req := c.container.volumeCreateRequest(&mount)

		// Check if this volume exists on the engine
		if _, err := c.volumeBackend.Create(ctx, req.Name, req.Driver,
			volumeopts.WithCreateOptions(req.DriverOpts),
			volumeopts.WithCreateLabels(req.Labels),
		); err != nil {
			// TODO(amitshukla): Today, volume create through the engine api does not return an error
			// when the named volume with the same parameters already exists.
			// It returns an error if the driver name is different - that is a valid error
			return err
		}
	}

	return nil
}

// waitClusterVolumes blocks until the VolumeGetter returns a path for each
// cluster volume in use by this task
func (c *containerAdapter) waitClusterVolumes(ctx context.Context) error {
	for _, attached := range c.container.task.Volumes {
		// for every attachment, try until we succeed or until the context
		// is canceled.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				// continue through the code.
			}
			path, err := c.dependencies.Volumes().Get(attached.ID)
			if err == nil && path != "" {
				// break out of the inner-most loop
				break
			}
		}
	}
	swarmlog.G(ctx).Debug("volumes ready")
	return nil
}

func (c *containerAdapter) activateServiceBinding() error {
	return c.backend.ActivateContainerServiceBinding(c.container.name())
}

func (c *containerAdapter) deactivateServiceBinding() error {
	return c.backend.DeactivateContainerServiceBinding(c.container.name())
}

func (c *containerAdapter) logs(ctx context.Context, options api.LogSubscriptionOptions) (<-chan *backend.LogMessage, error) {
	apiOptions := &backend.ContainerLogsOptions{
		Follow: options.Follow,

		// Always say yes to Timestamps and Details. we make the decision
		// of whether to return these to the user or not way higher up the
		// stack.
		Timestamps: true,
		Details:    true,
	}

	if options.Since != nil {
		since, err := gogotypes.TimestampFromProto(options.Since)
		if err != nil {
			return nil, err
		}
		apiOptions.Since = since
	}

	if options.Tail < 0 {
		// See protobuf documentation for details of how this works.
		apiOptions.Tail = strconv.FormatInt(-options.Tail-1, 10)
	} else if options.Tail > 0 {
		return nil, errors.New("tail relative to start of logs not supported via docker API")
	}

	if len(options.Streams) == 0 {
		// empty == all
		apiOptions.ShowStdout, apiOptions.ShowStderr = true, true
	} else {
		for _, stream := range options.Streams {
			switch stream {
			case api.LogStreamStdout:
				apiOptions.ShowStdout = true
			case api.LogStreamStderr:
				apiOptions.ShowStderr = true
			default:
				// TODO(thaJeztah): make switch exhaustive; add api.LogStreamUnknown
			}
		}
	}
	msgs, _, err := c.backend.ContainerLogs(ctx, c.container.name(), apiOptions)
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

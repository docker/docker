package daemon

import (
	"context"
	"maps"

	"github.com/moby/extensions"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	networkSettings "github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	runtimev0 "github.com/moby/moby/v2/extpoints/runtime/v0"
)

// runtimeExtensionID identifies the builtin extension through which the
// daemon provides the container runtime point.
const runtimeExtensionID = "org.mobyproject.runtime.v0"

// runtimeExtension declares the builtin extension providing the container
// runtime point, backed by the daemon.
func runtimeExtension(d *Daemon) extensions.Extension {
	return extensions.New(extensions.Declaration{
		ID:        runtimeExtensionID,
		Providers: []extensions.Provider{runtimev0.Point.Provide(daemonRuntime{d: d})},
	})
}

// daemonRuntime provides the container runtime point from the daemon,
// translating the point's self-contained types onto the daemon's backend.
// ContainerWait additionally converts the channel element: channels are
// invariant, so the daemon's concrete StateStatus channel cannot satisfy the
// interface-typed one.
type daemonRuntime struct {
	d *Daemon
}

func (r daemonRuntime) ContainerCreate(ctx context.Context, req runtimev0.ContainerCreateRequest) (container.CreateResponse, error) {
	return r.d.ContainerCreate(ctx, backendCreateConfig(req))
}

// backendCreateConfig translates the point's create request onto the backend
// form. It resolves the OS-independent "default" network mode to the OS's
// default network, the same conversion the HTTP API route applies before
// handing create requests to the backend; without it the container fails to
// start, looking up a network literally named "default". The resolution
// works on copies: this conversion never mutates the caller's request (the
// backend may still write defaults into a HostConfig it is handed, as on
// the HTTP path).
func backendCreateConfig(req runtimev0.ContainerCreateRequest) backend.ContainerCreateConfig {
	hostConfig := req.HostConfig
	if hostConfig == nil {
		hostConfig = &container.HostConfig{}
	}
	if hostConfig.NetworkMode == "" || hostConfig.NetworkMode.IsDefault() {
		hc := *hostConfig
		hostConfig = &hc
		hostConfig.NetworkMode = networkSettings.DefaultNetwork
		if req.NetworkingConfig != nil {
			if nw, ok := req.NetworkingConfig.EndpointsConfig[network.NetworkDefault]; ok {
				endpoints := maps.Clone(req.NetworkingConfig.EndpointsConfig)
				delete(endpoints, network.NetworkDefault)
				endpoints[hostConfig.NetworkMode.NetworkName()] = nw
				nc := *req.NetworkingConfig
				nc.EndpointsConfig = endpoints
				req.NetworkingConfig = &nc
			}
		}
	}
	return backend.ContainerCreateConfig{
		Name:             req.Name,
		Config:           req.Config,
		HostConfig:       hostConfig,
		NetworkingConfig: req.NetworkingConfig,
	}
}

func (r daemonRuntime) ContainerStart(ctx context.Context, name string) error {
	return r.d.ContainerStart(ctx, name, "", "")
}

func (r daemonRuntime) ContainerStop(ctx context.Context, name string) error {
	return r.d.ContainerStop(ctx, name, backend.ContainerStopOptions{})
}

func (r daemonRuntime) ContainerRm(name string) error {
	return r.d.ContainerRm(name, &backend.ContainerRmConfig{})
}

func (r daemonRuntime) ContainerWait(ctx context.Context, name string, condition container.WaitCondition) (<-chan runtimev0.StateStatus, error) {
	waitC, err := r.d.ContainerWait(ctx, name, condition)
	if err != nil {
		return nil, err
	}
	out := make(chan runtimev0.StateStatus, 1)
	// The forwarder lives until the container's final exit delivers the
	// status (the daemon-side channel always sends exactly one); under
	// live-restore that can be the rest of the container's life, which is
	// also how long the daemon-side wait it forwards from lives.
	go func() {
		defer close(out)
		if state, ok := <-waitC; ok {
			out <- state
		}
	}()
	return out, nil
}

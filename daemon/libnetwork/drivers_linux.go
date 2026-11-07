package libnetwork

import (
	"context"
	"fmt"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/host"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/ipvlan"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/macvlan"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/null"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay"
	"github.com/moby/moby/v2/daemon/libnetwork/drvregistry"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/rlkclient"
	"github.com/moby/moby/v2/daemon/libnetwork/portmappers/nat"
	"github.com/moby/moby/v2/daemon/libnetwork/portmappers/routed"
)

func registerNetworkDrivers(r driverapi.Registerer, cfg *config.Config, store *datastore.Store, pms *drvregistry.PortMappers) error {
	for _, nr := range []struct {
		ntype    string
		register func() error
	}{
		{
			ntype: bridge.NetworkType,
			register: func() error {
				return bridge.Register(r, store, pms, cfg.BridgeConfig)
			},
		},
		{ntype: host.NetworkType, register: func() error { return host.Register(r) }},
		{ntype: ipvlan.NetworkType, register: func() error { return ipvlan.Register(r, store) }},
		{ntype: macvlan.NetworkType, register: func() error { return macvlan.Register(r, store) }},
		{ntype: null.NetworkType, register: func() error { return null.Register(r) }},
		{ntype: overlay.NetworkType, register: func() error { return overlay.Register(r, cfg.NetnsDir) }},
	} {
		if err := nr.register(); err != nil {
			return fmt.Errorf("failed to register %q driver: %w", nr.ntype, err)
		}
	}

	return nil
}

func registerPortMappers(ctx context.Context, r *drvregistry.PortMappers, cfg *config.Config) error {
	var pdc *rlkclient.PortDriverClient
	if cfg.Rootless {
		var err error
		pdc, err = rlkclient.NewPortDriverClient(ctx)
		if err != nil {
			return fmt.Errorf("failed to create port driver client: %w", err)
		}
	}

	if err := nat.Register(r, nat.Config{RlkClient: pdc}); err != nil {
		return fmt.Errorf("registering nat portmapper: %w", err)
	}

	if err := routed.Register(r); err != nil {
		return fmt.Errorf("registering routed portmapper: %w", err)
	}

	return nil
}

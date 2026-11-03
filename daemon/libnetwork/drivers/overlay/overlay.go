//go:build linux

package overlay

//go:generate protoc -I=. -I=../../../../vendor/ --gogofaster_out=import_path=github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay:. overlay.proto

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/moby/moby/v2/daemon/libnetwork/discoverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
)

const (
	NetworkType  = "overlay"
	vethPrefix   = "veth"
	vethLen      = len(vethPrefix) + 7
	vxlanEncap   = 50
	secureOption = "encrypted"
)

var (
	_ discoverapi.Discover   = (*driver)(nil)
	_ driverapi.TableWatcher = (*driver)(nil)
)

type driver struct {
	// Immutable; mu does not need to be held when accessing these fields.
	initOS sync.Once

	// encrMu guards secMap and keys,
	// and synchronizes the application of encryption parameters
	// to the kernel.
	//
	// This mutex is above mu in the lock hierarchy.
	// Do not lock any locks aside from mu while holding encrMu.
	encrMu sync.Mutex
	secMap encrMap
	keys   []*key

	overlayEncNftInitMu sync.Mutex
	overlayEncNftTable  *nftables.Table

	// mu must be held when accessing the fields which follow it
	// in the struct definition.
	//
	// This mutex is at the bottom of the lock hierarchy:
	// do not lock any other locks while holding it.
	mu               sync.Mutex
	bindAddress      netip.Addr
	advertiseAddress netip.Addr
	networks         networkTable
}

func newDriver() *driver {
	return &driver{
		networks: networkTable{},
		secMap:   encrMap{},
	}
}

// Register registers a new instance of the overlay driver.
func Register(r driverapi.Registerer) error {
	return r.RegisterDriver(NetworkType, newDriver(), driverapi.Capability{
		DataScope:         scope.Global,
		ConnectivityScope: scope.Global,
	})
}

func (d *driver) configure() error {
	// Apply OS specific kernel configs if needed
	d.initOS.Do(func() {
		applyOStweaks()
		if !nftables.Enabled() {
			d.cleanupNft(context.TODO())
			iptables.OnReloaded(func() {
				d.restoreOverlayEncryptionFirewall(context.TODO())
			})
		}
	})

	return nil
}

func (d *driver) IsBuiltIn() bool {
	return true
}

// isIPv6Transport reports whether the outer Layer-3 transport for VXLAN datagrams is IPv6.
func (d *driver) isIPv6Transport() (bool, error) {
	// Infer whether remote peers' virtual tunnel endpoints will be IPv4 or IPv6
	// from the address family of our own advertise address. This is a
	// reasonable inference to make as Linux VXLAN links do not support
	// mixed-address-family remote peers.
	if !d.advertiseAddress.IsValid() {
		return false, errors.New("overlay: cannot determine address family of transport: the local data-plane address is not currently known")
	}
	return d.advertiseAddress.Is6(), nil
}

func (d *driver) nodeJoin(data discoverapi.NodeDiscoveryData) error {
	if data.Self {
		advAddr, _ := netip.ParseAddr(data.Address)
		bindAddr, _ := netip.ParseAddr(data.BindAddress)
		if !advAddr.IsValid() {
			return errors.New("invalid discovery data")
		}
		d.mu.Lock()
		d.advertiseAddress = advAddr
		d.bindAddress = bindAddr
		d.mu.Unlock()
	}
	return nil
}

// DiscoverNew is a notification for a new discovery event, such as a new node joining a cluster
func (d *driver) DiscoverNew(dType discoverapi.DiscoveryType, data any) error {
	switch dType {
	case discoverapi.NodeDiscovery:
		nodeData, ok := data.(discoverapi.NodeDiscoveryData)
		if !ok {
			return fmt.Errorf("invalid discovery data type: %T", data)
		}
		return d.nodeJoin(nodeData)
	case discoverapi.EncryptionKeysConfig:
		encrData, ok := data.(discoverapi.DriverEncryptionConfig)
		if !ok {
			return fmt.Errorf("invalid encryption key notification data type: %T", data)
		}
		return d.setKeys(context.TODO(), encrData)
	case discoverapi.EncryptionKeysUpdate:
		encrData, ok := data.(discoverapi.DriverEncryptionUpdate)
		if !ok {
			return fmt.Errorf("invalid encryption key notification data type: %T", data)
		}
		return d.updateKeys(context.TODO(), encrData)
	default:
		return nil
	}
}

// DiscoverDelete is a notification for a discovery delete event, such as a node leaving a cluster
func (d *driver) DiscoverDelete(dType discoverapi.DiscoveryType, data any) error {
	return nil
}

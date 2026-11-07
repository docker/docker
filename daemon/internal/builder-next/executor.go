package buildkit

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/buildkit/executor/oci"
	resourcestypes "github.com/moby/buildkit/executor/resources/types"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/network"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	"github.com/moby/moby/v2/daemon/libnetwork"
)

type bridgeProvider struct {
	*libnetwork.Controller
	Hooks *ocihook.Server
	Root  string
}

type lnInterface struct {
	ep  *libnetwork.Endpoint
	sbx *libnetwork.Sandbox
	sync.Once
	err      error
	ready    chan struct{}
	provider *bridgeProvider

	// ctx is the context of the container run the namespace was created
	// for. It is the parent context of the hook which sets the sandbox key,
	// so it must not be done before the container has started: the hook
	// fails if it is.
	ctx            context.Context
	unregisterHook func()
}

func (p *bridgeProvider) New(ctx context.Context, _ string, _ network.NamespaceOptions) (network.Namespace, error) {
	n, err := p.NetworkByName(networkName)
	if err != nil {
		return nil, err
	}

	iface := &lnInterface{ready: make(chan struct{}), provider: p, ctx: ctx}
	iface.Once.Do(func() {
		go iface.init(p.Controller, n)
	})

	return iface, nil
}

func (p *bridgeProvider) Close() error {
	return nil
}

func (iface *lnInterface) init(c *libnetwork.Controller, n *libnetwork.Network) {
	defer close(iface.ready)
	id := identity.NewID()

	ep, err := n.CreateEndpoint(context.TODO(), id, libnetwork.CreateOptionDisableResolution())
	if err != nil {
		iface.err = err
		return
	}

	sbx, err := c.NewSandbox(
		context.TODO(),
		id,
		libnetwork.OptionUseExternalKey(),
		libnetwork.OptionWriteHostsFile(filepath.Join(iface.provider.Root, id, "hosts")),
		libnetwork.OptionWriteResolvConf(filepath.Join(iface.provider.Root, id, "resolv.conf")),
	)
	if err != nil {
		iface.err = err
		return
	}

	if err := ep.Join(context.TODO(), sbx); err != nil {
		iface.err = err
		return
	}

	iface.sbx = sbx
	iface.ep = ep
}

// TODO(neersighted): Unstub Sample(), and collect data from the libnetwork Endpoint.
func (iface *lnInterface) Sample() (*resourcestypes.NetworkSample, error) {
	return &resourcestypes.NetworkSample{}, nil
}

func (iface *lnInterface) Close() error {
	<-iface.ready
	if iface.unregisterHook != nil {
		iface.unregisterHook()
	}
	if iface.sbx != nil {
		go func() {
			if err := iface.sbx.Delete(context.TODO()); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("failed to delete builder network sandbox")
			}
			if err := os.RemoveAll(filepath.Join(iface.provider.Root, iface.sbx.ContainerID())); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("failed to delete builder sandbox directory")
			}
		}()
	}
	return iface.err
}

func (iface *lnInterface) DialContext(ctx context.Context, networkName, address string) (net.Conn, error) {
	<-iface.ready
	if iface.err != nil {
		return nil, iface.err
	}

	var conn net.Conn
	var dialErr error
	if err := iface.sbx.ExecFunc(func() {
		conn, dialErr = (&net.Dialer{}).DialContext(ctx, networkName, address)
	}); err != nil {
		return nil, err
	}
	return conn, dialErr
}

func getDNSConfig(cfg config.DNSConfig) *oci.DNSConfig {
	if cfg.DNS != nil || cfg.DNSSearch != nil || cfg.DNSOptions != nil {
		return &oci.DNSConfig{
			Nameservers:   ipAddresses(cfg.DNS),
			SearchDomains: cfg.DNSSearch,
			Options:       cfg.DNSOptions,
		}
	}
	return nil
}

func ipAddresses(ips []netip.Addr) []string {
	var addrs []string
	for _, ip := range ips {
		if ip.IsValid() {
			addrs = append(addrs, ip.String())
		}
	}
	return addrs
}

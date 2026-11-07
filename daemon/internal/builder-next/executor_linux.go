package buildkit

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/executor/resources"
	"github.com/moby/buildkit/executor/runcexecutor"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/network"
	"github.com/moby/buildkit/util/network/proxyprovider"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

const networkName = "bridge"

func newExecutor(opts executorOpts) (executor.Executor, network.ProxyProvider, error) {
	netRoot := filepath.Join(opts.root, "net")
	networkProviders := map[pb.NetMode]network.Provider{
		pb.NetMode_UNSET: &bridgeProvider{Controller: opts.networkController, Hooks: opts.ociHooks, Root: netRoot},
		pb.NetMode_HOST:  network.NewHostProvider(),
		pb.NetMode_NONE:  network.NewNoneProvider(),
	}

	// make sure net state directory is cleared from previous state
	fis, err := os.ReadDir(netRoot)
	if err == nil {
		for _, fi := range fis {
			fp := filepath.Join(netRoot, fi.Name())
			if err := os.RemoveAll(fp); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("failed to delete old network state: %v", fp)
			}
		}
	}

	// Returning a non-nil but empty *IdentityMapping breaks BuildKit:
	// https://github.com/moby/moby/pull/39444
	idmap := &opts.identityMapping
	if opts.identityMapping.Empty() {
		idmap = nil
	}

	rm, err := resources.NewMonitor()
	if err != nil {
		return nil, nil, err
	}

	// TODO: FIXME: testing env var, replace with something better or remove in a major version or two
	runcCmds := []string{"runc"}
	if runcOverride := os.Getenv("DOCKER_BUILDKIT_RUNC_COMMAND"); runcOverride != "" {
		runcCmds = []string{runcOverride}
	}

	proxyProvider := opts.proxyProvider
	ownsProxyProvider := false
	if proxyProvider == nil && proxyprovider.Supported() {
		hostProvider := networkProviders[pb.NetMode_HOST]
		egressProviders := map[pb.NetMode]network.Provider{
			pb.NetMode_UNSET: loopbackFilteredProvider{provider: hostProvider},
			pb.NetMode_HOST:  hostProvider,
		}
		proxyProvider, err = proxyprovider.New(proxyprovider.Opt{
			Root:            filepath.Join(opts.root, "proxy"),
			EgressProviders: egressProviders,
		})
		if err != nil {
			return nil, nil, err
		}
		ownsProxyProvider = true
	}

	exec, err := runcexecutor.New(runcexecutor.Opt{
		Root:                filepath.Join(opts.root, "executor"),
		CommandCandidates:   runcCmds,
		DefaultCgroupParent: opts.cgroupParent,
		Rootless:            opts.rootless,
		NoPivot:             os.Getenv("DOCKER_RAMDISK") != "",
		IdentityMapping:     idmap,
		DNS:                 opts.dnsConfig,
		ApparmorProfile:     opts.apparmorProfile,
		ResourceMonitor:     rm,
		CDIManager:          opts.cdiManager,
		ProxyProvider:       proxyProvider,
	}, networkProviders)
	if err != nil {
		if ownsProxyProvider {
			_ = proxyProvider.Close()
		}
		return nil, nil, err
	}
	return exec, proxyProvider, nil
}

// newExecutorGD calls newExecutor() on Linux. It returns a stubExecutor on
// other platforms.
func newExecutorGD(opts executorOpts) (executor.Executor, network.ProxyProvider, error) {
	return newExecutor(opts)
}

type loopbackFilteredProvider struct {
	provider network.Provider
}

func (p loopbackFilteredProvider) New(ctx context.Context, hostname string, opt network.NamespaceOptions) (network.Namespace, error) {
	ns, err := p.provider.New(ctx, hostname, opt)
	if err != nil {
		return nil, err
	}
	return loopbackFilteredNS{Namespace: ns}, nil
}

func (p loopbackFilteredProvider) Close() error {
	return nil
}

type loopbackFilteredNS struct {
	network.Namespace
}

func (n loopbackFilteredNS) DialContext(ctx context.Context, networkName, address string) (net.Conn, error) {
	if isLoopbackAddress(ctx, address) {
		return nil, errors.Errorf("proxy egress to loopback address %s is not allowed", address)
	}
	dialer, ok := n.Namespace.(network.Dialer)
	if !ok {
		return nil, errors.Errorf("proxy egress network does not support dialing")
	}
	return dialer.DialContext(ctx, networkName, address)
}

func isLoopbackAddress(ctx context.Context, address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if addr.IP.IsLoopback() {
			return true
		}
	}
	return false
}

func (iface *lnInterface) Set(s *specs.Spec) error {
	<-iface.ready
	if iface.err != nil {
		log.G(context.TODO()).WithError(iface.err).Error("failed to set networking spec")
		return iface.err
	}
	if iface.unregisterHook != nil {
		iface.unregisterHook()
	}
	// attach netns to bridge within the container namespace, in a prestart hook
	hook, unregister := iface.provider.Hooks.Register(iface.ctx, func(ctx context.Context, st specs.State) error {
		return iface.sbx.SetKey(ctx, fmt.Sprintf("/proc/%d/ns/net", st.Pid))
	})
	iface.unregisterHook = unregister
	s.Hooks = &specs.Hooks{
		Prestart: []specs.Hook{hook}, //nolint:staticcheck // ignore SA1019 (Prestart is deprecated); FIXME(thaJeztah): see https://github.com/moby/moby/issues/52488
	}
	return nil
}

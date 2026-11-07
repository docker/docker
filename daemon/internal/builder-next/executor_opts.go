package buildkit

import (
	"context"
	"net"

	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/util/network"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/sys/user"
)

// executorOpts holds options for constructing an executor. It contains fields
// used on Linux, Windows, or both.
type executorOpts struct {
	// common fields
	root              string
	networkController *libnetwork.Controller
	ociHooks          *ocihook.Server
	dnsConfig         *oci.DNSConfig
	cdiManager        *cdidevices.Manager
	proxyProvider     network.ProxyProvider

	// linux-only fields
	cgroupParent    string
	apparmorProfile string
	rootless        bool
	identityMapping user.IdentityMapping

	// windows-only fields
	containerdAddr      string
	containerdDialer    func(ctx context.Context, address string) (net.Conn, error)
	containerdNamespace string
	hypervIsolation     bool
}

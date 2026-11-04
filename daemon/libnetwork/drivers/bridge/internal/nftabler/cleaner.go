//go:build linux

package nftabler

import (
	"context"
	"fmt"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
)

// Cleanup deletes all rules created by Nftabler; it's intended to be used
// during startup, to clean up rules created by an old incarnation of the daemon
// after switching to a different Firewaller implementation.
func Cleanup(ctx context.Context, config firewaller.Config) {
	if config.IPv4 {
		tryCleanup(ctx, nftables.IPv4, "IPv4")
	}
	if config.IPv6 {
		tryCleanup(ctx, nftables.IPv6, "IPv6")
	}
}

func tryCleanup(ctx context.Context, family nftables.Family, label string) {
	err := nftables.RunCmd(ctx, fmt.Appendf(nil, "delete table %s %s", family, dockerTable))
	if err != nil {
		// May not exist ("Error: Could not process rule: No such file or directory")
		log.G(ctx).WithError(err).Info("Deleting nftables " + label + " rules")
		return
	}

	log.G(ctx).Info("Deleted nftables " + label + " rules")
}

func (nft *Nftabler) SetFirewallCleaner(fc firewaller.FirewallCleaner) {
	nft.cleaner = fc
}

// FinishCleanup tells the FirewallCleaner, if there is one, that networks and
// endpoints have been restored. Then, it drops the FirewallCleaner.
func (nft *Nftabler) FinishCleanup(ctx context.Context) {
	if nft.cleaner != nil {
		nft.cleaner.Done(ctx)
		nft.cleaner = nil
	}
}

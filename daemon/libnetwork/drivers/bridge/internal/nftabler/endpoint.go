//go:build linux

package nftabler

import (
	"context"
	"net/netip"
)

func (n *network) AddEndpoint(ctx context.Context, epIPv4, epIPv6 netip.Addr) error {
	if n.fw.cleaner != nil {
		n.fw.cleaner.DelEndpoint(ctx, n.config, epIPv4, epIPv6)
	}
	return nil
}

func (n *network) DelEndpoint(_ context.Context, _, _ netip.Addr) error {
	return nil
}

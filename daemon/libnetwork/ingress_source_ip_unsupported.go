//go:build !linux

package libnetwork

import "github.com/docker/go-events"

func (*Endpoint) addIngressGatewayInfoToCluster() error { return nil }

func (*Endpoint) deleteIngressGatewayInfoFromCluster() error { return nil }

func (*Controller) handleIngressGatewayTableEvent(events.Event) {}

func (*Network) restoreIngressSourceIPRouting() {}

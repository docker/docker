package network

import (
	"net"
	"slices"
	"sync"

	networktypes "github.com/moby/moby/api/types/network"
	clustertypes "github.com/moby/moby/v2/daemon/cluster/provider"
	"github.com/pkg/errors"
)

// Settings stores configuration details about the daemon network config
// TODO Windows. Many of these fields can be factored out.,
type Settings struct {
	SandboxID        string
	SandboxKey       string
	Networks         map[string]*EndpointSettings
	Service          *clustertypes.ServiceConfig
	Ports            networktypes.PortMap
	HasSwarmEndpoint bool
}

// EndpointSettings is a package local wrapper for
// networktypes.EndpointSettings which stores Endpoint state that
// needs to be persisted to disk but not exposed in the api.
type EndpointSettings struct {
	*networktypes.EndpointSettings
	IPAMOperational bool
	// DesiredMacAddress is the configured value, it's copied from MacAddress (the
	// API param field) when the container is created.
	DesiredMacAddress networktypes.HardwareAddr
}

// Copy returns a deep copy of es.
func (es *EndpointSettings) Copy() *EndpointSettings {
	if es == nil {
		return nil
	}
	esCopy := *es
	esCopy.EndpointSettings = es.EndpointSettings.Copy()
	esCopy.DesiredMacAddress = slices.Clone(es.DesiredMacAddress)
	return &esCopy
}

// AttachmentStore stores the load balancer IP address for a network id.
type AttachmentStore struct {
	sync.Mutex
	// key: networkd id
	// value: load balancer ip address
	networkToNodeLBIP map[string]net.IP
}

// ResetAttachments clears any existing load balancer IP to network mapping and
// sets the mapping to the given attachments.
func (store *AttachmentStore) ResetAttachments(attachments map[string]string) error {
	store.Lock()
	defer store.Unlock()
	store.clearAttachments()
	for nid, nodeIP := range attachments {
		ip, _, err := net.ParseCIDR(nodeIP)
		if err != nil {
			store.networkToNodeLBIP = make(map[string]net.IP)
			return errors.Wrapf(err, "Failed to parse load balancer address %s", nodeIP)
		}
		store.networkToNodeLBIP[nid] = ip
	}
	return nil
}

// ClearAttachments clears all the mappings of network to load balancer IP Address.
func (store *AttachmentStore) ClearAttachments() {
	store.Lock()
	defer store.Unlock()
	store.clearAttachments()
}

func (store *AttachmentStore) clearAttachments() {
	store.networkToNodeLBIP = make(map[string]net.IP)
}

// GetIPForNetwork return the load balancer IP address for the given network.
func (store *AttachmentStore) GetIPForNetwork(networkID string) (net.IP, bool) {
	store.Lock()
	defer store.Unlock()
	ip, exists := store.networkToNodeLBIP[networkID]
	return ip, exists
}

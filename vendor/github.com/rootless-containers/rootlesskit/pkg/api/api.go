package api

import "net"

const (
	// Version of the REST API, not implementation version.
	// See openapi.yaml for the definition.
	Version = "1.1.1"
)

// ErrorJSON is returned with "application/json" content type and non-2XX status code
type ErrorJSON struct {
	Message string `json:"message"`
}

// Info is the structure returned by `GET /info`
type Info struct {
	APIVersion    string             `json:"apiVersion"` // REST API version
	Version       string             `json:"version"`    // Implementation version
	StateDir      string             `json:"stateDir"`
	ChildPID      int                `json:"childPID"`
	NetworkDriver *NetworkDriverInfo `json:"networkDriver,omitempty"`
	PortDriver    *PortDriverInfo    `json:"portDriver,omitempty"`
}

// NetworkDriverInfo in Info
type NetworkDriverInfo struct {
	Driver         string   `json:"driver"`
	DNS            []net.IP `json:"dns,omitempty"`
	ChildIP        net.IP   `json:"childIP,omitempty"`        // since API v1.1.1 (RootlessKit v0.14.1)
	DynamicChildIP bool     `json:"dynamicChildIP,omitempty"` // since API v1.1.1
}

// PortDriverInfo in Info
type PortDriverInfo struct {
	Driver                  string   `json:"driver"`
	Protos                  []string `json:"protos"`
	DisallowLoopbackChildIP bool     `json:"disallowLoopbackChildIP,omitempty"` // since API v1.1.1
}

package port

import (
	"context"
	"net"

	"github.com/rootless-containers/rootlesskit/pkg/api"
)

type Spec struct {
	// Proto is one of ["tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"].
	// "tcp" may cause listening on both IPv4 and IPv6. (Corresponds to Go's net.Listen .)
	Proto      string `json:"proto,omitempty"`
	ParentIP   string `json:"parentIP,omitempty"` // IPv4 or IPv6 address. can be empty (0.0.0.0).
	ParentPort int    `json:"parentPort,omitempty"`
	ChildPort  int    `json:"childPort,omitempty"`
	// ChildIP is an IPv4 or IPv6 address.
	// Default values:
	// - builtin     driver: 127.0.0.1
	// - slirp4netns driver: slirp4netns's child IP, e.g., 10.0.2.100
	ChildIP string `json:"childIP,omitempty"`
}

type Status struct {
	ID   int  `json:"id"`
	Spec Spec `json:"spec"`
}

// Manager MUST be thread-safe.
type Manager interface {
	AddPort(ctx context.Context, spec Spec) (*Status, error)
	ListPorts(ctx context.Context) ([]Status, error)
	RemovePort(ctx context.Context, id int) error
}

// ChildContext is used for RunParentDriver
type ChildContext struct {
	// PID of the child, can be used for ns-entering to the child namespaces.
	PID int
	// IP of the tap device
	IP net.IP
}

// ParentDriver is a driver for the parent process.
type ParentDriver interface {
	Manager
	Info(ctx context.Context) (*api.PortDriverInfo, error)
	// OpaqueForChild typically consists of socket path
	// for controlling child from parent
	OpaqueForChild() map[string]string
	// RunParentDriver signals initComplete when ParentDriver is ready to
	// serve as Manager.
	// RunParentDriver blocks until quit is signaled.
	//
	// ChildContext is optional.
	RunParentDriver(initComplete chan struct{}, quit <-chan struct{}, cctx *ChildContext) error
}

type ChildDriver interface {
	RunChildDriver(opaque map[string]string, quit <-chan struct{}) error
}

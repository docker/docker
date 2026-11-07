package config

import (
	"github.com/moby/buildkit/cache/remotecache/signing"
	"github.com/moby/buildkit/util/db/compaction"
	resolverconfig "github.com/moby/buildkit/util/resolver/config"
)

// Config provides containerd configuration data for the server
type Config struct {
	// Deprecated: Use Log.Level with "debug" set instead.
	Debug bool `toml:"debug"`

	// Deprecated: Use Log.Level with "trace" set instead.
	Trace bool `toml:"trace"`

	// Root is the path to a directory where buildkit will store persistent data
	Root string `toml:"root"`

	// Entitlements e.g. security.insecure, network.host, device
	Entitlements []string `toml:"insecure-entitlements"`

	// ProxyNetwork enables proxy network enforcement for all builds.
	ProxyNetwork bool `toml:"proxyNetwork"`

	// LogFormat is the format of the logs. It can be "json" or "text".
	Log LogConfig `toml:"log"`

	// GRPC configuration settings
	GRPC GRPCConfig `toml:"grpc"`

	OTEL OTELConfig `toml:"otel"`

	CDI CDIConfig `toml:"cdi"`

	Workers struct {
		OCI        OCIConfig        `toml:"oci"`
		Containerd ContainerdConfig `toml:"containerd"`
	} `toml:"worker"`

	Registries map[string]resolverconfig.RegistryConfig `toml:"registry"`

	DNS *DNSConfig `toml:"dns"`

	History *HistoryConfig `toml:"history"`

	Frontends struct {
		Dockerfile DockerfileFrontendConfig `toml:"dockerfile.v0"`
		Gateway    GatewayFrontendConfig    `toml:"gateway.v0"`
	} `toml:"frontend"`

	System *SystemConfig `toml:"system"`

	// ProvenanceEnvDir is the directory where extra config is loaded
	// that is added to the provenance of builds. Defaults to /etc/buildkit/provenance.d/ ,
	ProvenanceEnvDir string `toml:"provenanceEnvDir"`

	Cache      CacheConfig      `toml:"cache"`
	Compaction CompactionConfig `toml:"compaction"`
}

type CompactionConfig struct {
	Enabled                bool      `toml:"enabled"`
	IdleTimeout            *Duration `toml:"idleTimeout"`
	MaxRetry               *int      `toml:"maxRetry"`
	WritesPerCheck         *uint64   `toml:"writesPerCheck"`
	SizeWatermark          *int64    `toml:"sizeWatermark"`
	SizeGrowthPercent      *int64    `toml:"sizeGrowthPercent"`
	MinReclaimBytes        *int64    `toml:"minReclaimBytes"`
	MinReclaimPercent      *int64    `toml:"minReclaimPercent"`
	MinReclaimPercentFloor *int64    `toml:"minReclaimPercentFloor"`
}

func (c CompactionConfig) Policy() (compaction.Config, error) {
	policy := compaction.DefaultConfig()
	policy.ManualOnly = !c.Enabled
	if c.IdleTimeout != nil {
		policy.IdleTimeout = c.IdleTimeout.Duration
	}
	if c.MaxRetry != nil {
		policy.MaxRetry = *c.MaxRetry
	}
	if c.WritesPerCheck != nil {
		policy.WritesPerCheck = *c.WritesPerCheck
	}
	if c.SizeWatermark != nil {
		policy.SizeWatermark = *c.SizeWatermark
	}
	if c.SizeGrowthPercent != nil {
		policy.SizeGrowthPercent = *c.SizeGrowthPercent
	}
	if c.MinReclaimBytes != nil {
		policy.MinReclaimBytes = *c.MinReclaimBytes
	}
	if c.MinReclaimPercent != nil {
		policy.MinReclaimPercent = *c.MinReclaimPercent
		if c.MinReclaimPercentFloor == nil {
			policy.MinReclaimPercentFloor = min(policy.MinReclaimPercentFloor, policy.MinReclaimPercent)
		}
	}
	if c.MinReclaimPercentFloor != nil {
		policy.MinReclaimPercentFloor = *c.MinReclaimPercentFloor
	}
	return policy, policy.Validate()
}

type CacheConfig struct {
	GHA *signing.Config `toml:"gha"`
}

type SystemConfig struct {
	// PlatformCacheMaxAge controls how often supported platforms
	// are refreshed by rescanning the system.
	PlatformsCacheMaxAge *Duration `toml:"platformsCacheMaxAge"`

	// MaxRegistryConcurrency sets the maximum number of concurrent
	// connections per registry.
	MaxRegistryConcurrency *int `toml:"maxRegistryConcurrency"`

	// SessionAuthTimeout is the timeout for daemon-side session auth
	// round-trips with the buildx client session. Defaults to 60 seconds.
	// A value of zero or less disables the deadline entirely.
	SessionAuthTimeout *Duration `toml:"sessionAuthTimeout"`
}

type LogConfig struct {
	Format string `toml:"format"`
	Level  string `toml:"level"`
}

type GRPCConfig struct {
	Address            []string `toml:"address"`
	DebugAddress       string   `toml:"debugAddress"`
	UID                *int     `toml:"uid"`
	GID                *int     `toml:"gid"`
	SecurityDescriptor string   `toml:"securityDescriptor"`

	TLS TLSConfig `toml:"tls"`
	// MaxRecvMsgSize int    `toml:"max_recv_message_size"`
	// MaxSendMsgSize int    `toml:"max_send_message_size"`
}

type TLSConfig struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
	CA   string `toml:"ca"`
}

type OTELConfig struct {
	SocketPath string `toml:"socketPath"`
}

type CDIConfig struct {
	Disabled    *bool    `toml:"disabled"`
	SpecDirs    []string `toml:"specDirs"`
	AutoAllowed []string `toml:"autoAllowed"`
}

type GCConfig struct {
	GC *bool `toml:"gc"`
	// Deprecated: use GCReservedSpace instead
	GCKeepStorage   DiskSpace  `toml:"gckeepstorage"`
	GCReservedSpace DiskSpace  `toml:"reservedSpace"`
	GCMaxUsedSpace  DiskSpace  `toml:"maxUsedSpace"`
	GCMinFreeSpace  DiskSpace  `toml:"minFreeSpace"`
	GCPolicy        []GCPolicy `toml:"gcpolicy"`
}

type NetworkConfig struct {
	Mode          string `toml:"networkMode"`
	CNIConfigPath string `toml:"cniConfigPath"`
	CNIBinaryPath string `toml:"cniBinaryPath"`
	CNIPoolSize   int    `toml:"cniPoolSize"`
	BridgeName    string `toml:"bridgeName"`
	BridgeSubnet  string `toml:"bridgeSubnet"`
}

type OCIConfig struct {
	Enabled          *bool             `toml:"enabled"`
	Labels           map[string]string `toml:"labels"`
	Platforms        []string          `toml:"platforms,omitempty"`
	Snapshotter      string            `toml:"snapshotter"`
	Rootless         bool              `toml:"rootless"`
	NoProcessSandbox bool              `toml:"noProcessSandbox"`
	GCConfig
	NetworkConfig
	// UserRemapUnsupported is unsupported key for testing. The feature is
	// incomplete and the intention is to make it default without config.
	UserRemapUnsupported string `toml:"userRemapUnsupported"`
	// For use in storing the OCI worker binary name that will replace buildkit-runc
	Binary               string `toml:"binary"`
	ProxySnapshotterPath string `toml:"proxySnapshotterPath"`
	DefaultCgroupParent  string `toml:"defaultCgroupParent"`

	// StargzSnapshotterConfig is configuration for stargz snapshotter.
	// We use a generic map[string]interface{} in order to remove the dependency
	// on stargz snapshotter's config pkg from our config.
	StargzSnapshotterConfig map[string]any `toml:"stargzSnapshotter"`

	// ApparmorProfile is the name of the apparmor profile that should be used to constrain build containers.
	// The profile should already be loaded (by a higher level system) before creating a worker.
	ApparmorProfile string `toml:"apparmor-profile"`

	// SELinux enables applying SELinux labels.
	SELinux bool `toml:"selinux"`

	// MaxParallelism is the maximum number of parallel build steps that can be run at the same time.
	MaxParallelism int `toml:"max-parallelism"`
}

type ContainerdConfig struct {
	Address   string            `toml:"address"`
	Enabled   *bool             `toml:"enabled"`
	Labels    map[string]string `toml:"labels"`
	Platforms []string          `toml:"platforms,omitempty"`
	Namespace string            `toml:"namespace"`
	Runtime   ContainerdRuntime `toml:"runtime"`
	GCConfig
	NetworkConfig
	Snapshotter string `toml:"snapshotter"`

	// ApparmorProfile is the name of the apparmor profile that should be used to constrain build containers.
	// The profile should already be loaded (by a higher level system) before creating a worker.
	ApparmorProfile string `toml:"apparmor-profile"`

	// SELinux enables applying SELinux labels.
	SELinux bool `toml:"selinux"`

	MaxParallelism int `toml:"max-parallelism"`

	DefaultCgroupParent string `toml:"defaultCgroupParent"`

	Rootless bool `toml:"rootless"`

	// HyperVIsolation enables Hyper-V isolation for Windows containers.
	HyperVIsolation bool `toml:"hypervIsolation"`
}

type ContainerdRuntime struct {
	Name    string         `toml:"name"`
	Path    string         `toml:"path"`
	Options map[string]any `toml:"options"`
}

type GCPolicy struct {
	All     bool     `toml:"all"`
	Filters []string `toml:"filters"`

	KeepDuration Duration `toml:"keepDuration"`

	// KeepBytes is the maximum amount of storage this policy is ever allowed
	// to consume. Any storage above this mark can be cleared during a gc
	// sweep.
	//
	// Deprecated: use ReservedSpace instead
	KeepBytes DiskSpace `toml:"keepBytes"`

	// ReservedSpace is the minimum amount of disk space this policy is guaranteed to retain.
	// Any usage below this threshold will not be reclaimed during garbage collection.
	ReservedSpace DiskSpace `toml:"reservedSpace"`

	// MaxUsedSpace is the maximum amount of disk space this policy is allowed to use.
	// Any usage exceeding this limit will be cleaned up during a garbage collection sweep.
	MaxUsedSpace DiskSpace `toml:"maxUsedSpace"`

	// MinFreeSpace is the target amount of free disk space the garbage collector will attempt to leave.
	// However, it will never let the available space fall below ReservedSpace.
	MinFreeSpace DiskSpace `toml:"minFreeSpace"`
}

type DNSConfig struct {
	Nameservers   []string `toml:"nameservers"`
	Options       []string `toml:"options"`
	SearchDomains []string `toml:"searchDomains"`
}

type HistoryConfig struct {
	MaxAge     Duration `toml:"maxAge"`
	MaxEntries *int64   `toml:"maxEntries"`
}

type DockerfileFrontendConfig struct {
	Enabled *bool `toml:"enabled"`
}

type GatewayFrontendConfig struct {
	Enabled             *bool    `toml:"enabled"`
	AllowedRepositories []string `toml:"allowedRepositories"`
}

// Package containerprofile loads named, daemon-managed container defaults.
package containerprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
)

// DefaultDir is the standard directory containing daemon-managed profiles.
const DefaultDir = "/etc/docker/container-profiles"

var (
	// ErrInvalidName indicates that a profile name is not a valid profile name.
	ErrInvalidName = errors.New("invalid container profile name")
	// ErrLoad indicates that a profile could not be read from disk.
	ErrLoad = errors.New("could not load container profile")
	// ErrInvalid indicates that a profile is not valid JSON or contains unsupported fields.
	ErrInvalid = errors.New("invalid container profile")
)

// Profile contains the container settings that may be supplied by a named
// daemon-managed profile. Pointer fields preserve explicitly configured zero
// values while loading the profile.
type Profile struct {
	// ReadOnly makes the root filesystem read-only.
	ReadOnly *bool `json:"read-only,omitempty"`
	// CapAdd adds Linux capabilities.
	CapAdd []string `json:"cap-add,omitempty"`
	// CapDrop removes Linux capabilities.
	CapDrop []string `json:"cap-drop,omitempty"`
	// SecurityOpt sets container security options.
	SecurityOpt []string `json:"security-opt,omitempty"`
	// PidsLimit limits the number of processes.
	PidsLimit *int64 `json:"pids-limit,omitempty"`
	// Init enables Docker's init process.
	Init *bool `json:"init,omitempty"`
	// Tty allocates a pseudo-terminal.
	Tty *bool `json:"tty,omitempty"`
	// User sets the user or user:group for the process.
	User *string `json:"user,omitempty"`
	// WorkingDir sets the process working directory.
	WorkingDir *string `json:"working-dir,omitempty"`
	// StopTimeout sets the graceful stop timeout in seconds.
	StopTimeout *int `json:"stop-timeout,omitempty"`
	// Memory sets the memory limit in bytes.
	Memory *int64 `json:"memory,omitempty"`
	// NanoCPUs sets the CPU limit in 10^-9 CPUs.
	NanoCPUs *int64 `json:"cpus,omitempty"`
	// CPUQuota sets the CFS CPU quota in microseconds.
	CPUQuota *int64 `json:"cpu-quota,omitempty"`
	// CPUPeriod sets the CFS CPU period in microseconds.
	CPUPeriod *int64 `json:"cpu-period,omitempty"`
	// ShmSize sets /dev/shm size in bytes.
	ShmSize *int64 `json:"shm-size,omitempty"`
	// Tmpfs adds tmpfs mounts keyed by container path.
	Tmpfs map[string]string `json:"tmpfs,omitempty"`
	// Sysctls sets namespaced kernel parameters.
	Sysctls map[string]string `json:"sysctls,omitempty"`
	// Ulimits sets process resource limits.
	Ulimits []*container.Ulimit `json:"ulimits,omitempty"`
	// DNS sets the container DNS servers.
	DNS []netip.Addr `json:"dns,omitempty"`
	// DNSSearch sets DNS search domains.
	DNSSearch []string `json:"dns-search,omitempty"`
	// DNSOptions sets resolver options.
	DNSOptions []string `json:"dns-options,omitempty"`
}

// Load reads and validates the named profile from dir.
func Load(dir, name string) (Profile, error) {
	// An empty name means that no profile was selected.
	if name == "" {
		return Profile{}, nil
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return Profile{}, fmt.Errorf("%w %q", ErrInvalidName, name)
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return Profile{}, fmt.Errorf("%w %q: %w", ErrLoad, name, err)
	}
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("%w %q: %w", ErrInvalid, name, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Profile{}, fmt.Errorf("%w %q: multiple JSON values", ErrInvalid, name)
		}
		return Profile{}, fmt.Errorf("%w %q: %w", ErrInvalid, name, err)
	}
	return p, nil
}

// Apply fills unset container settings from p.
func Apply(p Profile, cfg *container.Config, host *container.HostConfig) {
	// Apply only fills fields that were not already supplied by the caller.
	if host == nil {
		return
	}
	if p.Init != nil && host.Init == nil {
		host.Init = p.Init
	}
	if p.Tty != nil && cfg != nil && !cfg.Tty {
		cfg.Tty = *p.Tty
	}
	if p.User != nil && cfg != nil && cfg.User == "" {
		cfg.User = *p.User
	}
	if p.WorkingDir != nil && cfg != nil && cfg.WorkingDir == "" {
		cfg.WorkingDir = *p.WorkingDir
	}
	if p.StopTimeout != nil && cfg != nil && cfg.StopTimeout == nil {
		cfg.StopTimeout = p.StopTimeout
	}
	if p.ReadOnly != nil && !host.ReadonlyRootfs {
		host.ReadonlyRootfs = *p.ReadOnly
	}
	if p.CapAdd != nil && len(host.CapAdd) == 0 {
		host.CapAdd = append([]string(nil), p.CapAdd...)
	}
	if p.CapDrop != nil && len(host.CapDrop) == 0 {
		host.CapDrop = append([]string(nil), p.CapDrop...)
	}
	if p.SecurityOpt != nil && len(host.SecurityOpt) == 0 {
		host.SecurityOpt = append([]string(nil), p.SecurityOpt...)
	}
	if p.PidsLimit != nil && host.PidsLimit == nil {
		host.PidsLimit = p.PidsLimit
	}
	if p.Memory != nil && host.Memory == 0 {
		host.Memory = *p.Memory
	}
	if p.NanoCPUs != nil && host.NanoCPUs == 0 {
		host.NanoCPUs = *p.NanoCPUs
	}
	if p.CPUQuota != nil && host.CPUQuota == 0 {
		host.CPUQuota = *p.CPUQuota
	}
	if p.CPUPeriod != nil && host.CPUPeriod == 0 {
		host.CPUPeriod = *p.CPUPeriod
	}
	if p.ShmSize != nil && host.ShmSize == 0 {
		host.ShmSize = *p.ShmSize
	}
	if p.Tmpfs != nil && len(host.Tmpfs) == 0 {
		host.Tmpfs = maps.Clone(p.Tmpfs)
	}
	if p.Sysctls != nil && len(host.Sysctls) == 0 {
		host.Sysctls = maps.Clone(p.Sysctls)
	}
	if p.Ulimits != nil && len(host.Ulimits) == 0 {
		host.Ulimits = slices.Clone(p.Ulimits)
	}
	if p.DNS != nil && len(host.DNS) == 0 {
		host.DNS = slices.Clone(p.DNS)
	}
	if p.DNSSearch != nil && len(host.DNSSearch) == 0 {
		host.DNSSearch = slices.Clone(p.DNSSearch)
	}
	if p.DNSOptions != nil && len(host.DNSOptions) == 0 {
		host.DNSOptions = slices.Clone(p.DNSOptions)
	}
}

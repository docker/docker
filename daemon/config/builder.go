package config

import (
	"encoding/json"
	"sort"
	"strings"

	bkconfig "github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/moby/moby/v2/daemon/internal/filters"
)

// BuilderGCRule represents a GC rule for buildkit cache
type BuilderGCRule struct {
	All           bool `json:",omitempty"`
	Filter        BuilderGCFilter
	ReservedSpace string `json:",omitempty"`
	MaxUsedSpace  string `json:",omitempty"`
	MinFreeSpace  string `json:",omitempty"`
}

func (x *BuilderGCRule) UnmarshalJSON(data []byte) error {
	var xx struct {
		All           bool `json:",omitempty"`
		Filter        BuilderGCFilter
		ReservedSpace string `json:",omitempty"`
		MaxUsedSpace  string `json:",omitempty"`
		MinFreeSpace  string `json:",omitempty"`

		// Deprecated option is now equivalent to ReservedSpace.
		KeepStorage string `json:",omitempty"`
	}
	if err := json.Unmarshal(data, &xx); err != nil {
		return err
	}

	x.All = xx.All
	x.Filter = xx.Filter
	x.ReservedSpace = xx.ReservedSpace
	x.MaxUsedSpace = xx.MaxUsedSpace
	x.MinFreeSpace = xx.MinFreeSpace
	if x.ReservedSpace == "" {
		x.ReservedSpace = xx.KeepStorage
	}
	return nil
}

// BuilderGCFilter contains garbage-collection filter rules for a BuildKit builder
type BuilderGCFilter filters.Args

// MarshalJSON returns a JSON byte representation of the BuilderGCFilter
func (x *BuilderGCFilter) MarshalJSON() ([]byte, error) {
	f := filters.Args(*x)
	keys := f.Keys()
	sort.Strings(keys)
	arr := make([]string, 0, len(keys))
	for _, k := range keys {
		values := f.Get(k)
		for _, v := range values {
			arr = append(arr, k+"="+v)
		}
	}
	return json.Marshal(arr)
}

// UnmarshalJSON fills the BuilderGCFilter values structure from JSON input
func (x *BuilderGCFilter) UnmarshalJSON(data []byte) error {
	var arr []string
	f := filters.NewArgs()
	if err := json.Unmarshal(data, &arr); err != nil {
		// backwards compat for deprecated buggy form
		err := json.Unmarshal(data, &f)
		*x = BuilderGCFilter(f)
		return err
	}
	for _, s := range arr {
		name, value, _ := strings.Cut(s, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		f.Add(name, value)
	}
	*x = BuilderGCFilter(f)
	return nil
}

// BuilderGCConfig contains GC config for a buildkit builder
type BuilderGCConfig struct {
	Enabled              *bool           `json:",omitempty"`
	Policy               []BuilderGCRule `json:",omitempty"`
	DefaultReservedSpace string          `json:",omitempty"`
	DefaultMaxUsedSpace  string          `json:",omitempty"`
	DefaultMinFreeSpace  string          `json:",omitempty"`
}

func (x *BuilderGCConfig) IsEnabled() bool {
	return x.Enabled == nil || *x.Enabled
}

func (x *BuilderGCConfig) UnmarshalJSON(data []byte) error {
	var xx struct {
		Enabled              bool            `json:",omitempty"`
		Policy               []BuilderGCRule `json:",omitempty"`
		DefaultReservedSpace string          `json:",omitempty"`
		DefaultMaxUsedSpace  string          `json:",omitempty"`
		DefaultMinFreeSpace  string          `json:",omitempty"`

		// Deprecated option is now equivalent to DefaultReservedSpace.
		DefaultKeepStorage string `json:",omitempty"`
	}

	// Set defaults.
	xx.Enabled = true

	if err := json.Unmarshal(data, &xx); err != nil {
		return err
	}

	x.Enabled = &xx.Enabled
	x.Policy = xx.Policy
	x.DefaultReservedSpace = xx.DefaultReservedSpace
	x.DefaultMaxUsedSpace = xx.DefaultMaxUsedSpace
	x.DefaultMinFreeSpace = xx.DefaultMinFreeSpace
	if x.DefaultReservedSpace == "" {
		x.DefaultReservedSpace = xx.DefaultKeepStorage
	}
	return nil
}

// BuilderHistoryConfig contains history config for a buildkit builder
type BuilderHistoryConfig struct {
	MaxAge     bkconfig.Duration
	MaxEntries *int64 `json:",omitempty"`
}

// BuilderEntitlements contains settings to enable/disable entitlements
type BuilderEntitlements struct {
	NetworkHost      *bool `json:"network-host,omitempty"`
	SecurityInsecure *bool `json:"security-insecure,omitempty"`
	Device           *bool `json:"device,omitempty"`
}

// BuilderConfig contains config for the builder
type BuilderConfig struct {
	GC           BuilderGCConfig
	Entitlements BuilderEntitlements
	History      *BuilderHistoryConfig `json:",omitempty"`
	ProxyNetwork bool                  `json:"proxy-network,omitempty"`
}

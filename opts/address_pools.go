package opts

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	types "github.com/docker/libnetwork/ipamutils"
)

// PoolsOpt is a Value type for parsing the default address pools definitions
type PoolsOpt struct {
	Values []*types.NetworkToSplit
}

// UnmarshalJSON fills values structure  info from JSON input
func (p *PoolsOpt) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &(p.Values))
}

// Set predefined pools
func (p *PoolsOpt) Set(value string) error {
	csvReader := csv.NewReader(strings.NewReader(value))
	fields, err := csvReader.Read()
	if err != nil {
		return err
	}

	poolsDef := types.NetworkToSplit{}

	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid field '%s' must be a key=value pair", field)
		}

		key := strings.ToLower(parts[0])
		value := strings.ToLower(parts[1])

		switch key {
		case "base":
			poolsDef.Base = value
		case "size":
			size, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid size value: %q (must be integer): %v", value, err)
			}
			poolsDef.Size = size
		default:
			return fmt.Errorf("unexpected key '%s' in '%s'", key, field)
		}
	}

	p.Values = append(p.Values, &poolsDef)

	return nil
}

// Type returns the type of this option
func (p *PoolsOpt) Type() string {
	return "pool-options"
}

// String returns a string repr of this option
func (p *PoolsOpt) String() string {
	var pools []string
	for _, pool := range p.Values {
		repr := fmt.Sprintf("%s %d", pool.Base, pool.Size)
		pools = append(pools, repr)
	}
	return strings.Join(pools, ", ")
}

// Value returns the mounts
func (p *PoolsOpt) Value() []*types.NetworkToSplit {
	return p.Values
}

// Name returns the flag name of this option
func (p *PoolsOpt) Name() string {
	return "default-address-pools"
}

package network

import (
	"encoding/json"
	"net/netip"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestIPAMConfigUnmarshalJSONAcceptsLegacyCIDRPrefixedGateway(t *testing.T) {
	const (
		legacyGatewayConfig = `{"Subnet":"fd05:d0ca:2::/112","Gateway":"fd05:d0ca:2::1/112"}`
		expectedGateway     = "fd05:d0ca:2::1"
	)

	var config IPAMConfig
	assert.NilError(t, json.Unmarshal([]byte(legacyGatewayConfig), &config))
	assert.Check(t, is.Equal(config.Gateway, netip.MustParseAddr(expectedGateway)))
}

func TestIPAMConfigUnmarshalJSONRejectsInvalidGateway(t *testing.T) {
	const invalidGatewayConfig = `{"Gateway":"not-an-address"}`

	var config IPAMConfig
	assert.Check(t, is.ErrorContains(json.Unmarshal([]byte(invalidGatewayConfig), &config), "unable to parse IP"))
}

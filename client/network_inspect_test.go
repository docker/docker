package client

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestNetworkInspect(t *testing.T) {
	const expectedURL = "/networks/network_id"

	client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == defaultAPIPath+"/networks/" {
			return errorMock(http.StatusInternalServerError, "client should not make a request for empty IDs")(req)
		}
		if strings.HasPrefix(req.URL.Path, defaultAPIPath+"/networks/unknown") {
			return errorMock(http.StatusNotFound, "Error: No such network: unknown")(req)
		}
		if strings.HasPrefix(req.URL.Path, defaultAPIPath+"/networks/test-500-response") {
			return errorMock(http.StatusInternalServerError, "Server error")(req)
		}

		// other test-cases all use "network_id"
		if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
			return nil, err
		}
		if strings.Contains(req.URL.RawQuery, "scope=global") {
			return errorMock(http.StatusNotFound, "Error: No such network: network_id")(req)
		}
		var resp network.Inspect
		if strings.Contains(req.URL.RawQuery, "verbose=true") {
			resp = network.Inspect{
				Network: network.Network{Name: "mynetwork"},
				Services: map[string]network.ServiceInfo{
					"web": {},
				},
			}
		} else {
			resp = network.Inspect{
				Network: network.Network{Name: "mynetwork"},
			}
		}
		return mockJSONResponse(http.StatusOK, nil, resp)(req)
	}))
	assert.NilError(t, err)

	t.Run("empty ID", func(t *testing.T) {
		// verify that the client does not create a request if the network-ID/name is empty.
		_, err := client.NetworkInspect(t.Context(), "", NetworkInspectOptions{})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
		assert.Check(t, is.ErrorContains(err, "value is empty"))

		_, err = client.NetworkInspect(t.Context(), "    ", NetworkInspectOptions{})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
		assert.Check(t, is.ErrorContains(err, "value is empty"))
	})
	t.Run("no options", func(t *testing.T) {
		r, err := client.NetworkInspect(t.Context(), "network_id", NetworkInspectOptions{})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(r.Network.Name, "mynetwork"))
	})
	t.Run("verbose", func(t *testing.T) {
		r, err := client.NetworkInspect(t.Context(), "network_id", NetworkInspectOptions{Verbose: true})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(r.Network.Name, "mynetwork"))
		_, ok := r.Network.Services["web"]
		assert.Check(t, ok, "expected service `web` missing in the verbose output")
	})
	t.Run("global scope", func(t *testing.T) {
		_, err := client.NetworkInspect(t.Context(), "network_id", NetworkInspectOptions{Scope: "global"})
		assert.Check(t, is.ErrorContains(err, "Error: No such network: network_id"))
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})
	t.Run("unknown network", func(t *testing.T) {
		_, err := client.NetworkInspect(t.Context(), "unknown", NetworkInspectOptions{})
		assert.Check(t, is.ErrorContains(err, "Error: No such network: unknown"))
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})
	t.Run("server error", func(t *testing.T) {
		// Just testing that an internal server error is converted correctly by the client
		_, err := client.NetworkInspect(t.Context(), "test-500-response", NetworkInspectOptions{})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
	})
}

func TestNetworkInspectAcceptsLegacyCIDRPrefixedGateway(t *testing.T) {
	const (
		expectedURL                  = "/networks/" + legacyNetworkName
		legacyNetworkInspectResponse = `{"Name":"` + legacyNetworkName + `","Driver":"bridge","IPAM":{"Config":[{"Subnet":"` + legacyNetworkSubnet + `","Gateway":"` + legacyCIDRPrefixedGateway + `"}]}}`
	)

	client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
			return nil, err
		}
		return mockResponse(http.StatusOK, http.Header{"Content-Type": {networkResponseContent}}, legacyNetworkInspectResponse)(req)
	}))
	assert.NilError(t, err)

	res, err := client.NetworkInspect(t.Context(), legacyNetworkName, NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Network.IPAM.Config, 1))
	assert.Check(t, is.Equal(res.Network.IPAM.Config[0].Gateway, netip.MustParseAddr(legacyNetworkGateway)))
}

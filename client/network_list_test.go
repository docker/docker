package client

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	legacyCIDRPrefixedGateway = "fd05:d0ca:2::1/112"
	legacyNetworkGateway      = "fd05:d0ca:2::1"
	legacyNetworkName         = "test-ipv6"
	legacyNetworkSubnet       = "fd05:d0ca:2::/112"
	networkResponseContent    = "application/json"
)

func TestNetworkListError(t *testing.T) {
	client, err := New(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)

	_, err = client.NetworkList(t.Context(), NetworkListOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
}

func TestNetworkList(t *testing.T) {
	const expectedURL = "/networks"

	listCases := []struct {
		options         NetworkListOptions
		expectedFilters string
	}{
		{
			options:         NetworkListOptions{},
			expectedFilters: "",
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).Add("dangling", "false"),
			},
			expectedFilters: `{"dangling":{"false":true}}`,
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).Add("dangling", "true"),
			},
			expectedFilters: `{"dangling":{"true":true}}`,
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).
					Add("label", "label1").
					Add("label", "label2"),
			},
			expectedFilters: `{"label":{"label1":true,"label2":true}}`,
		},
	}

	for _, listCase := range listCases {
		client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
			if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
				return nil, err
			}
			query := req.URL.Query()
			actualFilters := query.Get("filters")
			if actualFilters != listCase.expectedFilters {
				return nil, fmt.Errorf("filters not set in URL query properly. Expected '%s', got %s", listCase.expectedFilters, actualFilters)
			}
			return mockJSONResponse(http.StatusOK, nil, []network.Summary{
				{
					Network: network.Network{
						Name:   "network",
						Driver: "bridge",
					},
				},
			})(req)
		}))
		assert.NilError(t, err)

		res, err := client.NetworkList(t.Context(), listCase.options)
		assert.NilError(t, err)
		assert.Check(t, is.Len(res.Items, 1))
	}
}

func TestNetworkListAcceptsLegacyCIDRPrefixedGateway(t *testing.T) {
	const (
		expectedURL               = "/networks"
		legacyNetworkListResponse = `[{"Name":"` + legacyNetworkName + `","Driver":"bridge","IPAM":{"Config":[{"Subnet":"` + legacyNetworkSubnet + `","Gateway":"` + legacyCIDRPrefixedGateway + `"}]}}]`
	)

	client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
			return nil, err
		}
		return mockResponse(http.StatusOK, http.Header{"Content-Type": {networkResponseContent}}, legacyNetworkListResponse)(req)
	}))
	assert.NilError(t, err)

	res, err := client.NetworkList(t.Context(), NetworkListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Items, 1))
	assert.Assert(t, is.Len(res.Items[0].IPAM.Config, 1))
	assert.Check(t, is.Equal(res.Items[0].IPAM.Config[0].Gateway, netip.MustParseAddr(legacyNetworkGateway)))
}

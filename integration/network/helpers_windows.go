package network

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"gotest.tools/assert/cmp"
)

// DeleteInterface deletes a network interface
func DeleteInterface(_ *testing.T, _ string) cmp.Comparison {
	panic("DeleteInterface is not implemented on Windows")
}

// IsNetworkAvailable provides a comparison to check if a docker network is available
func IsNetworkAvailable(c client.NetworkAPIClient, name string) cmp.Comparison {
	return func() cmp.Result {
		networks, err := c.NetworkList(context.Background(), types.NetworkListOptions{})
		if err != nil {
			return cmp.ResultFromError(err)
		}
		for _, network := range networks {
			if network.Name == name {
				return cmp.ResultSuccess
			}
		}
		return cmp.ResultFailure(fmt.Sprintf("could not find network %s", name))
	}
}

// IsNetworkNotAvailable provides a comparison to check if a docker network is not available
func IsNetworkNotAvailable(c client.NetworkAPIClient, name string) cmp.Comparison {
	return func() cmp.Result {
		networks, err := c.NetworkList(context.Background(), types.NetworkListOptions{})
		if err != nil {
			return cmp.ResultFromError(err)
		}
		for _, network := range networks {
			if network.Name == name {
				return cmp.ResultFailure(fmt.Sprintf("network %s is still present", name))
			}
		}
		return cmp.ResultSuccess
	}
}

// IsUserNamespace returns whether the user namespace remapping is enabled
func IsUserNamespace() bool {
	root := os.Getenv("DOCKER_REMAP_ROOT")
	return root != ""
}

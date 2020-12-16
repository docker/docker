// +build !exclude_graphdriver_containerd,linux

package register // import "github.com/docker/docker/daemon/graphdriver/register"

import (
	// register the containerd-based graphdriver
	_ "github.com/docker/docker/daemon/graphdriver/containerd"
)

package daemon

import (
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/container"
)

const (
	dockerContainerIDLength    = 64
	c8dContainerIDPrefixLength = 32
)

func (daemon *Daemon) getContainerByC8dContainerID(id string) (*container.Container, error) {
	if c := daemon.containers.Get(id); c != nil {
		return c, nil
	}

	if len(id) != dockerContainerIDLength {
		return nil, containerNotFound(id)
	}

	containerID, err := daemon.containersReplica.GetByPrefix(id[:c8dContainerIDPrefixLength])
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			return nil, err
		}
		return nil, containerNotFound(id)
	}
	if c := daemon.containers.Get(containerID); c != nil {
		return c, nil
	}
	return nil, containerNotFound(id)
}

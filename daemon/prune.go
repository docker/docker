package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	timetypes "github.com/docker/docker/api/types/time"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/runconfig"
	"github.com/docker/libnetwork"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	// errPruneRunning is returned when a prune request is received while
	// one is in progress
	errPruneRunning = errdefs.Conflict(errors.New("a prune operation is already running"))

	containersAcceptedFilters = map[string]bool{
		"label":  true,
		"label!": true,
		"until":  true,
	}

	networksAcceptedFilters = map[string]bool{
		"label":  true,
		"label!": true,
		"until":  true,
	}
)

// ContainersPrune removes unused containers
func (daemon *Daemon) ContainersPrune(ctx context.Context, pruneFilters filters.Args) (*types.ContainersPruneReport, error) {
	if !atomic.CompareAndSwapInt32(&daemon.pruneRunning, 0, 1) {
		return nil, errPruneRunning
	}
	defer atomic.StoreInt32(&daemon.pruneRunning, 0)

	rep := &types.ContainersPruneReport{}

	// make sure that only accepted filters have been received
	err := pruneFilters.Validate(containersAcceptedFilters)
	if err != nil {
		return nil, err
	}

	until, err := getUntilFromPruneFilters(pruneFilters)
	if err != nil {
		return nil, err
	}

	allContainers := daemon.List()
	for _, c := range allContainers {
		select {
		case <-ctx.Done():
			logrus.Debugf("ContainersPrune operation cancelled: %#v", *rep)
			return rep, nil
		default:
		}

		if !c.IsRunning() {
			if !until.IsZero() && c.Created.After(until) {
				continue
			}
			if !matchLabels(pruneFilters, c.Config.Labels) {
				continue
			}

			// TODO(containerd): context + id + layer backend must be provided
			return nil, errdefs.NotImplemented(errors.New("containerd layer size not implemented"))

			//cSize, _ := daemon.imageService.GetContainerLayerSize(c.ID)
			//// TODO: sets RmLink to true?
			//err := daemon.ContainerRm(c.ID, &types.ContainerRmConfig{})
			//if err != nil {
			//	logrus.Warnf("failed to prune container %s: %v", c.ID, err)
			//	continue
			//}
			//if cSize > 0 {
			//	rep.SpaceReclaimed += uint64(cSize)
			//}
			//rep.ContainersDeleted = append(rep.ContainersDeleted, c.ID)
		}
	}

	return rep, nil
}

// localNetworksPrune removes unused local networks
func (daemon *Daemon) localNetworksPrune(ctx context.Context, pruneFilters filters.Args) *types.NetworksPruneReport {
	rep := &types.NetworksPruneReport{}

	until, _ := getUntilFromPruneFilters(pruneFilters)

	// When the function returns true, the walk will stop.
	l := func(nw libnetwork.Network) bool {
		select {
		case <-ctx.Done():
			// context cancelled
			return true
		default:
		}
		if nw.Info().ConfigOnly() {
			return false
		}
		if !until.IsZero() && nw.Info().Created().After(until) {
			return false
		}
		if !matchLabels(pruneFilters, nw.Info().Labels()) {
			return false
		}
		nwName := nw.Name()
		if runconfig.IsPreDefinedNetwork(nwName) {
			return false
		}
		if len(nw.Endpoints()) > 0 {
			return false
		}
		if err := daemon.DeleteNetwork(nw.ID()); err != nil {
			logrus.Warnf("could not remove local network %s: %v", nwName, err)
			return false
		}
		rep.NetworksDeleted = append(rep.NetworksDeleted, nwName)
		return false
	}
	daemon.netController.WalkNetworks(l)
	return rep
}

// clusterNetworksPrune removes unused cluster networks
func (daemon *Daemon) clusterNetworksPrune(ctx context.Context, pruneFilters filters.Args) (*types.NetworksPruneReport, error) {
	rep := &types.NetworksPruneReport{}

	until, _ := getUntilFromPruneFilters(pruneFilters)

	cluster := daemon.GetCluster()

	if !cluster.IsManager() {
		return rep, nil
	}

	networks, err := cluster.GetNetworks(pruneFilters)
	if err != nil {
		return rep, err
	}
	networkIsInUse := regexp.MustCompile(`network ([[:alnum:]]+) is in use`)
	for _, nw := range networks {
		select {
		case <-ctx.Done():
			return rep, nil
		default:
			if nw.Ingress {
				// Routing-mesh network removal has to be explicitly invoked by user
				continue
			}
			if !until.IsZero() && nw.Created.After(until) {
				continue
			}
			if !matchLabels(pruneFilters, nw.Labels) {
				continue
			}
			// https://github.com/docker/docker/issues/24186
			// `docker network inspect` unfortunately displays ONLY those containers that are local to that node.
			// So we try to remove it anyway and check the error
			err = cluster.RemoveNetwork(nw.ID)
			if err != nil {
				// we can safely ignore the "network .. is in use" error
				match := networkIsInUse.FindStringSubmatch(err.Error())
				if len(match) != 2 || match[1] != nw.ID {
					logrus.Warnf("could not remove cluster network %s: %v", nw.Name, err)
				}
				continue
			}
			rep.NetworksDeleted = append(rep.NetworksDeleted, nw.Name)
		}
	}
	return rep, nil
}

// NetworksPrune removes unused networks
func (daemon *Daemon) NetworksPrune(ctx context.Context, pruneFilters filters.Args) (*types.NetworksPruneReport, error) {
	if !atomic.CompareAndSwapInt32(&daemon.pruneRunning, 0, 1) {
		return nil, errPruneRunning
	}
	defer atomic.StoreInt32(&daemon.pruneRunning, 0)

	// make sure that only accepted filters have been received
	err := pruneFilters.Validate(networksAcceptedFilters)
	if err != nil {
		return nil, err
	}

	if _, err := getUntilFromPruneFilters(pruneFilters); err != nil {
		return nil, err
	}

	rep := &types.NetworksPruneReport{}
	if clusterRep, err := daemon.clusterNetworksPrune(ctx, pruneFilters); err == nil {
		rep.NetworksDeleted = append(rep.NetworksDeleted, clusterRep.NetworksDeleted...)
	}

	localRep := daemon.localNetworksPrune(ctx, pruneFilters)
	rep.NetworksDeleted = append(rep.NetworksDeleted, localRep.NetworksDeleted...)

	select {
	case <-ctx.Done():
		logrus.Debugf("NetworksPrune operation cancelled: %#v", *rep)
		return rep, nil
	default:
	}

	return rep, nil
}

func getUntilFromPruneFilters(pruneFilters filters.Args) (time.Time, error) {
	until := time.Time{}
	if !pruneFilters.Contains("until") {
		return until, nil
	}
	untilFilters := pruneFilters.Get("until")
	if len(untilFilters) > 1 {
		return until, fmt.Errorf("more than one until filter specified")
	}
	ts, err := timetypes.GetTimestamp(untilFilters[0], time.Now())
	if err != nil {
		return until, err
	}
	seconds, nanoseconds, err := timetypes.ParseTimestamps(ts, 0)
	if err != nil {
		return until, err
	}
	until = time.Unix(seconds, nanoseconds)
	return until, nil
}

func matchLabels(pruneFilters filters.Args, labels map[string]string) bool {
	if !pruneFilters.MatchKVList("label", labels) {
		return false
	}
	// By default MatchKVList will return true if field (like 'label!') does not exist
	// So we have to add additional Contains("label!") check
	if pruneFilters.Contains("label!") {
		if pruneFilters.MatchKVList("label!", labels) {
			return false
		}
	}
	return true
}

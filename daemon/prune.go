package daemon

import (
	"context"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/prune"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/lazyregexp"
	"github.com/moby/moby/v2/daemon/internal/pruneprogress"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/daemon/libnetwork"
	dnetwork "github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
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
)

// ContainerPrune removes unused containers
func (daemon *Daemon) ContainerPrune(ctx context.Context, opts pruneprogress.Options) (*container.PruneReport, error) {
	pruneFilters := opts.Filters
	if !daemon.pruneRunning.CompareAndSwap(false, true) {
		return nil, errPruneRunning
	}
	defer daemon.pruneRunning.Store(false)

	rep := &container.PruneReport{}

	// make sure that only accepted filters have been received
	err := pruneFilters.Validate(containersAcceptedFilters)
	if err != nil {
		return nil, err
	}

	until, err := getUntilFromPruneFilters(pruneFilters)
	if err != nil {
		return nil, err
	}

	cfg := &daemon.config().Config
	allContainers := daemon.List()
	for _, c := range allContainers {
		select {
		case <-ctx.Done():
			log.G(ctx).Debugf("ContainerPrune operation cancelled: %#v", *rep)
			return rep, nil
		default:
		}

		if !c.State.IsRunning() {
			if !until.IsZero() && c.Created.After(until) {
				continue
			}
			if !matchLabels(pruneFilters, c.Config.Labels) {
				continue
			}
			cSize, _, err := daemon.imageService.GetContainerLayerSize(ctx, c.ID)
			if err != nil {
				// Debug log only, the size is only for informational purposes anyway
				log.G(ctx).WithFields(log.Fields{
					"error":     err,
					"container": c.ID,
				}).Debug("failed to get layer size for container")
			}
			// TODO: sets RmLink to true?
			err = daemon.containerRm(cfg, c.ID, &backend.ContainerRmConfig{})
			if err != nil {
				log.G(ctx).Warnf("failed to prune container %s: %v", c.ID, err)
				continue
			}
			if cSize > 0 {
				rep.SpaceReclaimed += uint64(cSize)
			}
			rep.ContainersDeleted = append(rep.ContainersDeleted, c.ID)
			pruneprogress.Notify(opts.Progress, c.ID, "deleted")
		}
	}
	daemon.EventsService.Log(events.ActionPrune, events.ContainerEventType, events.Actor{
		Attributes: map[string]string{"reclaimed": strconv.FormatUint(rep.SpaceReclaimed, 10)},
	})
	return rep, nil
}

// localNetworkPrune removes unused local networks
func (daemon *Daemon) localNetworkPrune(ctx context.Context, pruneFilters dnetwork.Filter, progress func(prune.Object)) *network.PruneReport {
	rep := &network.PruneReport{}

	// When the function returns true, the walk will stop.
	daemon.netController.WalkNetworks(func(nw *libnetwork.Network) bool {
		select {
		case <-ctx.Done():
			// context cancelled
			return true
		default:
		}
		if nw.ConfigOnly() {
			return false
		}
		if !pruneFilters.Matches(nw) {
			return false
		}
		if !nw.IsPruneable() {
			return false
		}
		if len(nw.Endpoints()) > 0 {
			return false
		}
		if err := daemon.DeleteNetwork(nw.ID()); err != nil {
			log.G(ctx).Warnf("could not remove local network %s: %v", nw.Name(), err)
			return false
		}
		rep.NetworksDeleted = append(rep.NetworksDeleted, nw.Name())
		pruneprogress.Notify(progress, nw.Name(), "deleted")
		return false
	})
	return rep
}

var networkIsInUse = lazyregexp.New(`network ([[:alnum:]]+) is in use`)

// clusterNetworkPrune removes unused cluster networks
func (daemon *Daemon) clusterNetworkPrune(ctx context.Context, pruneFilters dnetwork.Filter, progress func(prune.Object)) (*network.PruneReport, error) {
	rep := &network.PruneReport{}

	cluster := daemon.GetCluster()

	if !cluster.IsManager() {
		return rep, nil
	}

	networks, err := cluster.GetNetworks(pruneFilters, false)
	if err != nil {
		return rep, err
	}

	for _, nw := range networks {
		select {
		case <-ctx.Done():
			return rep, nil
		default:
			if nw.Ingress {
				// Routing-mesh network removal has to be explicitly invoked by user
				continue
			}
			// https://github.com/moby/moby/issues/24186
			// `docker network inspect` unfortunately displays ONLY those containers that are local to that node.
			// So we try to remove it anyway and check the error
			err = cluster.RemoveNetwork(nw.ID)
			if err != nil {
				// we can safely ignore the "network .. is in use" error
				match := networkIsInUse.FindStringSubmatch(err.Error())
				if len(match) != 2 || match[1] != nw.ID {
					log.G(ctx).Warnf("could not remove cluster network %s: %v", nw.Name, err)
				}
				continue
			}
			rep.NetworksDeleted = append(rep.NetworksDeleted, nw.Name)
			pruneprogress.Notify(progress, nw.Name, "deleted")
		}
	}
	return rep, nil
}

// NetworkPrune removes unused networks
func (daemon *Daemon) NetworkPrune(ctx context.Context, opts pruneprogress.Options) (*network.PruneReport, error) {
	filterArgs := opts.Filters
	if !daemon.pruneRunning.CompareAndSwap(false, true) {
		return nil, errPruneRunning
	}
	defer daemon.pruneRunning.Store(false)

	pruneFilters, err := dnetwork.NewPruneFilter(filterArgs)
	if err != nil {
		return nil, err
	}

	rep := &network.PruneReport{}
	if clusterRep, err := daemon.clusterNetworkPrune(ctx, pruneFilters, opts.Progress); err == nil {
		rep.NetworksDeleted = append(rep.NetworksDeleted, clusterRep.NetworksDeleted...)
	}

	localRep := daemon.localNetworkPrune(ctx, pruneFilters, opts.Progress)
	rep.NetworksDeleted = append(rep.NetworksDeleted, localRep.NetworksDeleted...)

	select {
	case <-ctx.Done():
		log.G(ctx).Debugf("NetworkPrune operation cancelled: %#v", *rep)
		return rep, nil
	default:
	}
	daemon.EventsService.Log(events.ActionPrune, events.NetworkEventType, events.Actor{
		Attributes: map[string]string{"reclaimed": "0"},
	})
	return rep, nil
}

func getUntilFromPruneFilters(pruneFilters filters.Args) (time.Time, error) {
	if !pruneFilters.Contains("until") {
		return time.Time{}, nil
	}
	untilFilters := pruneFilters.Get("until")
	if len(untilFilters) > 1 {
		return time.Time{}, errdefs.InvalidParameter(errors.New("more than one until filter specified"))
	}
	t, err := timestamp.Parse(untilFilters[0], time.Now())
	if err != nil {
		return time.Time{}, errdefs.InvalidParameter(err)
	}
	return t, nil
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

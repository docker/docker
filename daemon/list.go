package daemon

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var acceptedPsFilterTags = map[string]bool{
	"ancestor":   true,
	"annotation": true,
	"before":     true,
	"exited":     true,
	"id":         true,
	"isolation":  true,
	"label":      true,
	"name":       true,
	"status":     true,
	"health":     true,
	"since":      true,
	"volume":     true,
	"network":    true,
	"is-task":    true,
	"publish":    true,
	"expose":     true,
}

// iterationAction represents possible outcomes happening during the container iteration.
type iterationAction int

const (
	// includeContainer is the action to include a container.
	includeContainer iterationAction = iota
	// excludeContainer is the action to exclude a container.
	excludeContainer
	// stopIteration is the action to stop iterating over the list of containers.
	stopIteration
)

// List returns an array of all containers registered in the daemon.
func (daemon *Daemon) List() []*container.Container {
	return daemon.containers.List()
}

// listContext is the daemon generated filtering to iterate over containers.
// This is created based on the user specification from [backend.ContainerListOptions].
type listContext struct {
	// idx is the container iteration index for this context
	idx int
	// ancestorFilter tells whether it should check ancestors or not
	ancestorFilter bool
	// names is a list of container names to filter with
	names map[string][]string
	// images is a list of images to filter with
	images map[image.ID]bool
	// filters is a collection of arguments to filter with, specified by the user
	filters filters.Args
	// exitAllowed is a list of exit codes allowed to filter with
	exitAllowed []int

	// beforeFilter is a filter to ignore containers that appear before the one given
	beforeFilter *container.Snapshot
	// sinceFilter is a filter to stop the filtering when the iterator arrives to the given container
	sinceFilter *container.Snapshot

	// taskFilter tells if we should filter based on whether a container is part of a task
	taskFilter bool
	// isTask tells us if we should filter container that is a task (true) or not (false)
	isTask bool

	// publish is a list of published ports to filter with
	publish map[string]bool
	// expose is a list of exposed ports to filter with
	expose map[string]bool

	// ContainerListOptions is the filters set by the user
	*backend.ContainerListOptions
}

// byCreatedDescending is a temporary type used to sort a list of containers by creation time.
type byCreatedDescending []container.Snapshot

func (r byCreatedDescending) Len() int      { return len(r) }
func (r byCreatedDescending) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r byCreatedDescending) Less(i, j int) bool {
	return r[j].CreatedAt.UnixNano() < r[i].CreatedAt.UnixNano()
}

// Containers returns the list of containers to show given the user's filtering.
func (daemon *Daemon) Containers(ctx context.Context, config *backend.ContainerListOptions) ([]containertypes.Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.Filters.Validate(acceptedPsFilterTags); err != nil {
		return nil, err
	}

	view := daemon.containersReplica.Snapshot()
	filter, err := daemon.foldFilter(ctx, view, config)
	if err != nil {
		return nil, err
	}

	// shortcut to only look at a subset of containers if specific name
	// or ID matches were provided by the user--otherwise we potentially
	// end up querying many more containers than intended
	//
	// TODO (thaJeztah): given containersReplica.Snapshot() provides a "consistent read-only view of the database" (which indicates "de-referenced copy", is there any reason we wouldn't use a []*container.Snapshot (pointer-slice)?.
	containerList, err := daemon.filterByNameIDMatches(view, filter)
	if err != nil {
		return nil, err
	}
	numContainers := len(containerList)

	// shortcut if there are no containers
	if numContainers == 0 {
		return []containertypes.Summary{}, nil
	}

	// Get the info for each container in the list; this can be slow so we
	// dispatch a set number of worker goroutines to do the jobs. We choose
	// log2(numContainers) workers to avoid creating too many goroutines
	// for large number of containers.
	numWorkers := max(int(math.Log2(float64(numContainers))), 1)

	resultsMut := sync.Mutex{}
	results := make([]containertypes.Summary, numContainers)

	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(numWorkers)

	for i := range containerList {
		if err := ctx.Err(); err != nil {
			break
		}
		currentContainer := &containerList[i]
		stop := false
		switch includeContainerInList(currentContainer, filter) {
		case excludeContainer:
			continue
		case stopIteration:
			stop = true
		case includeContainer:
			idx := filter.idx
			filter.idx++

			g.Go(func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				// refresh the container image info (in case the image changed in
				// the repository)
				newC := daemon.refreshImage(ctx, currentContainer)

				// get the image size (calculation is slow)
				if filter.Size {
					if err := ctx.Err(); err != nil {
						return err
					}
					var err error
					newC.SizeRw, newC.SizeRootFs, err = daemon.imageService.GetContainerLayerSize(ctx, newC.ID)
					if err != nil {
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							return err
						}
						// Don't fail the whole listing because the size of a
						// single container can't be determined.
						log.G(ctx).WithFields(log.Fields{
							"error":       err,
							"containerID": newC.ID,
						}).Debug("failed to get container layer size")
						newC.SizeRw = -1
						newC.SizeRootFs = -1
					}
				}

				// insert the result at the given index (so the output is in the
				// same order as containerList above).
				resultsMut.Lock()
				results[idx] = *newC
				resultsMut.Unlock()

				return nil
			})
		}
		if stop {
			break
		}
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := parentCtx.Err(); err != nil {
		return nil, err
	}

	return results[:filter.idx], nil
}

func (daemon *Daemon) filterByNameIDMatches(view *container.View, filter *listContext) ([]container.Snapshot, error) {
	idSearch := false
	names := filter.filters.Get("name")
	ids := filter.filters.Get("id")
	if len(names)+len(ids) == 0 {
		// if name or ID filters are not in use, return to
		// standard behavior of walking the entire container
		// list from the daemon's in-memory store
		all, err := view.All()
		if err != nil {
			return nil, err
		}

		sort.Sort(byCreatedDescending(all))
		return all, nil
	}

	// idSearch will determine if we limit name matching to the IDs
	// matched from any IDs which were specified as filters
	if len(ids) > 0 {
		idSearch = true
	}

	matches := make(map[string]bool)
	// find ID matches; errors represent "not found" and can be ignored
	for _, id := range ids {
		if fullID, err := daemon.containersReplica.GetByPrefix(id); err == nil {
			matches[fullID] = true
		}
	}

	// look for name matches; if ID filtering was used, then limit the
	// search space to the matches map only; errors represent "not found"
	// and can be ignored
	if len(names) > 0 {
		for id, idNames := range filter.names {
			// if ID filters were used and no matches on that ID were
			// found, continue to next ID in the list
			if idSearch && !matches[id] {
				continue
			}
			for _, eachName := range idNames {
				// match both on container name with, and without slash-prefix
				if filter.filters.Match("name", eachName) || filter.filters.Match("name", strings.TrimPrefix(eachName, "/")) {
					matches[id] = true
				}
			}
		}
	}

	cntrs := make([]container.Snapshot, 0, len(matches))
	for id := range matches {
		c, err := view.Get(id)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				// ignore error
				continue
			}
			return nil, err
		}
		cntrs = append(cntrs, *c)
	}

	// Restore sort-order after filtering
	// Created gives us nanosec resolution for sorting
	sort.Sort(byCreatedDescending(cntrs))

	return cntrs, nil
}

// foldFilter generates the container filter based on the user's filtering options.
func (daemon *Daemon) foldFilter(ctx context.Context, view *container.View, config *backend.ContainerListOptions) (*listContext, error) {
	psFilters := config.Filters

	var filtExited []int

	err := psFilters.WalkValues("exited", func(value string) error {
		code, err := strconv.Atoi(value)
		if err != nil {
			return errdefs.InvalidParameter(errors.Wrapf(err, "invalid filter 'exited=%s'", value))
		}
		filtExited = append(filtExited, code)
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = psFilters.WalkValues("status", func(value string) error {
		if err := containertypes.ValidateContainerState(containertypes.ContainerState(value)); err != nil {
			return errdefs.InvalidParameter(fmt.Errorf("invalid filter 'status=%s': %w", value, err))
		}
		config.All = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	taskFilter := psFilters.Contains("is-task")
	isTask, err := psFilters.GetBoolOrDefault("is-task", false)
	if err != nil {
		return nil, err
	}

	err = psFilters.WalkValues("health", func(value string) error {
		if err := containertypes.ValidateHealthStatus(containertypes.HealthStatus(value)); err != nil {
			return errdefs.InvalidParameter(fmt.Errorf("invalid filter 'health=%s': %w", value, err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var beforeContFilter, sinceContFilter *container.Snapshot

	err = psFilters.WalkValues("before", func(value string) error {
		beforeContFilter, err = idOrNameFilter(view, value)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = psFilters.WalkValues("since", func(value string) error {
		sinceContFilter, err = idOrNameFilter(view, value)
		return err
	})
	if err != nil {
		return nil, err
	}

	imagesFilter := map[image.ID]bool{}
	var ancestorFilter bool
	if psFilters.Contains("ancestor") {
		ancestorFilter = true
		err := psFilters.WalkValues("ancestor", func(ancestor string) error {
			img, err := daemon.imageService.GetImage(ctx, ancestor, imagebackend.GetImageOpts{})
			if err != nil {
				log.G(ctx).Warnf("Error while looking up for image %v", ancestor)
				return nil
			}
			if imagesFilter[img.ID()] {
				// Already seen this ancestor, skip it
				return nil
			}
			// Then walk down the graph and put the imageIds in imagesFilter
			return populateImageFilterByParents(ctx, imagesFilter, img.ID(), daemon.imageService.Children)
		})
		if err != nil {
			return nil, err
		}
	}

	publishFilter := map[string]bool{}
	err = psFilters.WalkValues("publish", portOp("publish", publishFilter))
	if err != nil {
		return nil, err
	}

	exposeFilter := map[string]bool{}
	err = psFilters.WalkValues("expose", portOp("expose", exposeFilter))
	if err != nil {
		return nil, err
	}

	return &listContext{
		filters:        psFilters,
		ancestorFilter: ancestorFilter,
		images:         imagesFilter,
		exitAllowed:    filtExited,
		beforeFilter:   beforeContFilter,
		sinceFilter:    sinceContFilter,
		taskFilter:     taskFilter,
		isTask:         isTask,
		publish:        publishFilter,
		expose:         exposeFilter,
		names:          view.GetAllNames(),

		ContainerListOptions: config,
	}, nil
}

func idOrNameFilter(view *container.View, value string) (*container.Snapshot, error) {
	filter, err := view.Get(value)
	if err != nil && cerrdefs.IsNotFound(err) {
		// Try name search instead
		found := ""
		searchName := strings.TrimPrefix(value, "/")
		for id, idNames := range view.GetAllNames() {
			for _, name := range idNames {
				if searchName == strings.TrimPrefix(name, "/") {
					if found != "" && found != id {
						return nil, err
					}
					found = id
				}
			}
		}
		if found != "" {
			filter, err = view.Get(found)
		}
	}
	return filter, err
}

func portOp(key string, filter map[string]bool) func(value string) error {
	return func(value string) error {
		if strings.Contains(value, ":") {
			return fmt.Errorf("filter for '%s' should not contain ':': %s", key, value)
		}
		// support two formats, original format <portnum>/[<proto>] or <startport-endport>/[<proto>]
		portRange, err := network.ParsePortRange(value)
		if err != nil {
			return fmt.Errorf("error while looking up for %s %s: %s", key, value, err)
		}
		for p := range portRange.All() {
			filter[p.String()] = true
		}
		return nil
	}
}

// includeContainerInList decides whether a container should be included in the output or not based in the filter.
// It also decides if the iteration should be stopped or not.
func includeContainerInList(container *container.Snapshot, filter *listContext) iterationAction {
	// Do not include container if it's in the list before the filter container.
	// Set the filter container to nil to include the rest of containers after this one.
	if filter.beforeFilter != nil {
		if container.ID == filter.beforeFilter.ID {
			filter.beforeFilter = nil
		}
		return excludeContainer
	}

	// Stop iteration when the container arrives to the filter container
	if filter.sinceFilter != nil {
		if container.ID == filter.sinceFilter.ID {
			return stopIteration
		}
	}

	// Do not include container if it's stopped and we're not filters
	if !container.Running && !filter.All && filter.Limit <= 0 {
		return excludeContainer
	}

	// Do not include container if the name doesn't match
	if !filter.filters.Match("name", container.Name) && !filter.filters.Match("name", strings.TrimPrefix(container.Name, "/")) {
		return excludeContainer
	}

	// Do not include container if the id doesn't match
	if !filter.filters.Match("id", container.ID) {
		return excludeContainer
	}

	if filter.taskFilter {
		if filter.isTask != container.Managed {
			return excludeContainer
		}
	}

	// Do not include container if any of the labels don't match
	if !filter.filters.MatchKVList("label", container.Labels) {
		return excludeContainer
	}

	// Do not include container if any of the annotations don't match
	if !filter.filters.MatchKVList("annotation", container.Summary.HostConfig.Annotations) {
		return excludeContainer
	}

	// Do not include container if isolation doesn't match
	if excludeContainer == excludeByIsolation(container, filter) {
		return excludeContainer
	}

	// Stop iteration when the index is over the limit
	if filter.Limit > 0 && filter.idx == filter.Limit {
		return stopIteration
	}

	// Do not include container if its exit code is not in the filter
	if len(filter.exitAllowed) > 0 {
		shouldSkip := true
		for _, code := range filter.exitAllowed {
			if code == container.ExitCode && !container.Running && !container.StartedAt.IsZero() {
				shouldSkip = false
				break
			}
		}
		if shouldSkip {
			return excludeContainer
		}
	}

	// Do not include container if its status doesn't match the filter
	if !filter.filters.Match("status", string(container.State)) {
		return excludeContainer
	}

	// Do not include container if its health doesn't match the filter
	if !filter.filters.ExactMatch("health", string(container.Health)) {
		return excludeContainer
	}

	if filter.filters.Contains("volume") {
		volumesByName := make(map[string]containertypes.MountPoint)
		for _, m := range container.Mounts {
			if m.Name != "" {
				volumesByName[m.Name] = m
			} else {
				volumesByName[m.Source] = m
			}
		}
		volumesByDestination := make(map[string]containertypes.MountPoint)
		for _, m := range container.Mounts {
			if m.Destination != "" {
				volumesByDestination[m.Destination] = m
			}
		}

		volumeExist := errors.New("volume mounted in container")
		err := filter.filters.WalkValues("volume", func(value string) error {
			if _, exist := volumesByDestination[value]; exist {
				return volumeExist
			}
			if _, exist := volumesByName[value]; exist {
				return volumeExist
			}
			return nil
		})
		if !errors.Is(err, volumeExist) {
			return excludeContainer
		}
	}

	if filter.ancestorFilter {
		if len(filter.images) == 0 {
			return excludeContainer
		}
		if !filter.images[image.ID(container.ImageID)] {
			return excludeContainer
		}
	}

	var (
		networkExist = errors.New("container part of network")
		noNetworks   = errors.New("container is not part of any networks")
	)
	if filter.filters.Contains("network") {
		err := filter.filters.WalkValues("network", func(value string) error {
			if container.NetworkSettings == nil {
				return noNetworks
			}
			if _, ok := container.NetworkSettings.Networks[value]; ok {
				return networkExist
			}
			for _, nw := range container.NetworkSettings.Networks {
				if nw == nil {
					continue
				}
				if strings.HasPrefix(nw.NetworkID, value) {
					return networkExist
				}
			}
			return nil
		})
		if !errors.Is(err, networkExist) {
			return excludeContainer
		}
	}

	if len(filter.expose) > 0 || len(filter.publish) > 0 {
		shouldSkip := true
		for _, port := range container.Ports {
			publishedPort := fmt.Sprintf("%d/%s", port.PublicPort, port.Type)
			exposedPort := fmt.Sprintf("%d/%s", port.PrivatePort, port.Type)
			if filter.publish[publishedPort] || filter.expose[exposedPort] {
				shouldSkip = false
				break
			}
		}
		if shouldSkip {
			return excludeContainer
		}
	}

	return includeContainer
}

// refreshImage checks if the Image ref still points to the correct ID, and
// updates the ref to the actual ID when it doesn't.
// This happens when the image with a reference that was used to create
// container was deleted or updated and now resolves to a different ID.
//
// For example:
// $ docker run -d busybox:latest
// $ docker ps -a
// CONTAINER ID   IMAGE     COMMAND   CREATED         STATUS                     PORTS     NAMES
// b0318bca5aef   busybox   "sh"      4 seconds ago   Exited (0) 3 seconds ago             ecstatic_beaver
//
// After some time, busybox image got updated on the Docker Hub:
// $ docker pull busybox:latest
//
// So now busybox:latest points to a different digest, but that doesn't impact
// the ecstatic_beaver container which was still created under an older
// version. In this case, it should still point to the original image ID it was
// created from.
//
// $ docker ps -a
// CONTAINER ID   IMAGE          COMMAND   CREATED       STATUS                  PORTS     NAMES
// b0318bca5aef   3fbc63216742   "sh"      3 years ago   Exited (0) 3 years ago            ecstatic_beaver
func (daemon *Daemon) refreshImage(ctx context.Context, s *container.Snapshot) *containertypes.Summary {
	c := s.Summary

	// s.Image is the image reference passed by the user to create an image
	//         can be a:
	//         - name (like nginx, ubuntu:latest, docker.io/library/busybox:latest),
	//         - truncated ID (abcdef),
	//         - full digest (sha256:abcdef...)
	//
	// s.ImageID is the ID of the image that s.Image resolved to at the time
	// of the container creation. It's always a full digest.

	// If these match, there's nothing to refresh.
	if s.Image == s.ImageID {
		return &c
	}

	// Check if the image reference still resolves to the same digest.
	img, err := daemon.imageService.GetImage(ctx, s.Image, imagebackend.GetImageOpts{})
	// If the image is no longer found or can't be resolved for some other
	// reason. Update the Image to the specific ID of the original image it
	// resolved to when the container was created.
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			log.G(ctx).WithFields(log.Fields{
				"error":       err,
				"containerID": c.ID,
				"image":       s.Image,
				"imageID":     s.ImageID,
			}).Warn("failed to resolve container image")
		}
		c.Image = s.ImageID
		return &c
	}

	// Also update the image to the specific image ID, if the Image now
	// resolves to a different ID.
	if img.ImageID() != s.ImageID {
		c.Image = s.ImageID
	}

	return &c
}

func populateImageFilterByParents(ctx context.Context, ancestorMap map[image.ID]bool, imageID image.ID, getChildren func(context.Context, image.ID) ([]image.ID, error)) error {
	if !ancestorMap[imageID] {
		children, err := getChildren(ctx, imageID)
		if err != nil {
			return err
		}
		for _, id := range children {
			if err := populateImageFilterByParents(ctx, ancestorMap, id, getChildren); err != nil {
				return err
			}
		}
		ancestorMap[imageID] = true
	}
	return nil
}

package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/containerd/containerd/content"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var acceptedPsFilterTags = map[string]bool{
	"ancestor":  true,
	"before":    true,
	"exited":    true,
	"id":        true,
	"isolation": true,
	"label":     true,
	"name":      true,
	"status":    true,
	"health":    true,
	"since":     true,
	"volume":    true,
	"network":   true,
	"is-task":   true,
	"publish":   true,
	"expose":    true,
}

// iterationAction represents possible outcomes happening during the container iteration.
type iterationAction int

// containerReducer represents a reducer for a container.
// Returns the object to serialize by the api.
type containerReducer func(*container.Snapshot, *listContext) (*types.Container, error)

const (
	// includeContainer is the action to include a container in the reducer.
	includeContainer iterationAction = iota
	// excludeContainer is the action to exclude a container in the reducer.
	excludeContainer
	// stopIteration is the action to stop iterating over the list of containers.
	stopIteration
)

// errStopIteration makes the iterator to stop without returning an error.
var errStopIteration = errors.New("container list iteration stopped")

// List returns an array of all containers registered in the daemon.
func (daemon *Daemon) List() []*container.Container {
	return daemon.containers.List()
}

// listContext is the daemon generated filtering to iterate over containers.
// This is created based on the user specification from types.ContainerListOptions.
type listContext struct {
	// idx is the container iteration index for this context
	idx int
	// ancestorFilter tells whether it should check ancestors or not
	ancestorFilter bool
	// names is a list of container names to filter with
	names map[string][]string
	// images is a list of images to filter with
	images map[digest.Digest]bool
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
	publish map[nat.Port]bool
	// expose is a list of exposed ports to filter with
	expose map[nat.Port]bool

	// ContainerListOptions is the filters set by the user
	*types.ContainerListOptions
}

// byCreatedDescending is a temporary type used to sort a list of containers by creation time.
type byCreatedDescending []container.Snapshot

func (r byCreatedDescending) Len() int      { return len(r) }
func (r byCreatedDescending) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r byCreatedDescending) Less(i, j int) bool {
	return r[j].CreatedAt.UnixNano() < r[i].CreatedAt.UnixNano()
}

// Containers returns the list of containers to show given the user's filtering.
func (daemon *Daemon) Containers(ctx context.Context, config *types.ContainerListOptions) ([]*types.Container, error) {
	return daemon.reduceContainers(ctx, config, daemon.refreshImage)
}

func (daemon *Daemon) filterByNameIDMatches(view container.View, ctx *listContext) ([]container.Snapshot, error) {
	idSearch := false
	names := ctx.filters.Get("name")
	ids := ctx.filters.Get("id")
	if len(names)+len(ids) == 0 {
		// if name or ID filters are not in use, return to
		// standard behavior of walking the entire container
		// list from the daemon's in-memory store
		all, err := view.All()
		sort.Sort(byCreatedDescending(all))
		return all, err
	}

	// idSearch will determine if we limit name matching to the IDs
	// matched from any IDs which were specified as filters
	if len(ids) > 0 {
		idSearch = true
	}

	matches := make(map[string]bool)
	// find ID matches; errors represent "not found" and can be ignored
	for _, id := range ids {
		if fullID, err := daemon.idIndex.Get(id); err == nil {
			matches[fullID] = true
		}
	}

	// look for name matches; if ID filtering was used, then limit the
	// search space to the matches map only; errors represent "not found"
	// and can be ignored
	if len(names) > 0 {
		for id, idNames := range ctx.names {
			// if ID filters were used and no matches on that ID were
			// found, continue to next ID in the list
			if idSearch && !matches[id] {
				continue
			}
			for _, eachName := range idNames {
				// match both on container name with, and without slash-prefix
				if ctx.filters.Match("name", eachName) || ctx.filters.Match("name", strings.TrimPrefix(eachName, "/")) {
					matches[id] = true
				}
			}
		}
	}

	cntrs := make([]container.Snapshot, 0, len(matches))
	for id := range matches {
		c, err := view.Get(id)
		switch err.(type) {
		case nil:
			cntrs = append(cntrs, *c)
		case container.NoSuchContainerError:
			// ignore error
		default:
			return nil, err
		}
	}

	// Restore sort-order after filtering
	// Created gives us nanosec resolution for sorting
	sort.Sort(byCreatedDescending(cntrs))

	return cntrs, nil
}

// reduceContainers parses the user's filtering options and generates the list of containers to return based on a reducer.
func (daemon *Daemon) reduceContainers(ctx context.Context, config *types.ContainerListOptions, reducer containerReducer) ([]*types.Container, error) {
	if err := config.Filters.Validate(acceptedPsFilterTags); err != nil {
		return nil, err
	}

	var (
		view       = daemon.containersReplica.Snapshot()
		containers = []*types.Container{}
	)

	lctx, err := daemon.foldFilter(ctx, view, config)
	if err != nil {
		return nil, err
	}

	// fastpath to only look at a subset of containers if specific name
	// or ID matches were provided by the user--otherwise we potentially
	// end up querying many more containers than intended
	containerList, err := daemon.filterByNameIDMatches(view, lctx)
	if err != nil {
		return nil, err
	}

	for i := range containerList {
		t, err := daemon.reducePsContainer(&containerList[i], lctx, reducer)
		if err != nil {
			if err != errStopIteration {
				return nil, err
			}
			break
		}
		if t != nil {
			containers = append(containers, t)
			lctx.idx++
		}
	}

	return containers, nil
}

// reducePsContainer is the basic representation for a container as expected by the ps command.
func (daemon *Daemon) reducePsContainer(container *container.Snapshot, ctx *listContext, reducer containerReducer) (*types.Container, error) {
	// filter containers to return
	switch includeContainerInList(container, ctx) {
	case excludeContainer:
		return nil, nil
	case stopIteration:
		return nil, errStopIteration
	}

	// transform internal container struct into api structs
	newC, err := reducer(container, ctx)
	if err != nil {
		return nil, err
	}

	// release lock because size calculation is slow
	if ctx.Size {
		// TODO(containerd): context + id + layer backend must be provided
		return nil, errdefs.NotImplemented(errors.New("containerd layer size not implemented"))

		//sizeRw, sizeRootFs := daemon.imageService.GetContainerLayerSize(newC.ID)
		//newC.SizeRw = sizeRw
		//newC.SizeRootFs = sizeRootFs
	}
	return newC, nil
}

func (daemon *Daemon) childrenByID(ctx context.Context, id digest.Digest) ([]digest.Digest, error) {
	cs := daemon.containerdCli.ContentStore()

	var children []digest.Digest
	err := cs.Walk(ctx, func(info content.Info) error {
		children = append(children, info.Digest)
		return nil
	}, fmt.Sprintf("labels.%q==%s", images.LabelImageParent, id.String()))
	if err != nil {
		return nil, err
	}

	return children, nil
}

// foldFilter generates the container filter based on the user's filtering options.
func (daemon *Daemon) foldFilter(ctx context.Context, view container.View, config *types.ContainerListOptions) (*listContext, error) {
	psFilters := config.Filters

	var filtExited []int

	err := psFilters.WalkValues("exited", func(value string) error {
		code, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		filtExited = append(filtExited, code)
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = psFilters.WalkValues("status", func(value string) error {
		if !container.IsValidStateString(value) {
			return invalidFilter{"status", value}
		}

		config.All = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	var taskFilter, isTask bool
	if psFilters.Contains("is-task") {
		if psFilters.ExactMatch("is-task", "true") {
			taskFilter = true
			isTask = true
		} else if psFilters.ExactMatch("is-task", "false") {
			taskFilter = true
			isTask = false
		} else {
			return nil, invalidFilter{"is-task", psFilters.Get("is-task")}
		}
	}

	err = psFilters.WalkValues("health", func(value string) error {
		if !container.IsValidHealthString(value) {
			return errdefs.InvalidParameter(errors.Errorf("Unrecognised filter value for health: %s", value))
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

	imagesFilter := map[digest.Digest]bool{}
	var ancestorFilter bool
	if psFilters.Contains("ancestor") {
		ancestorFilter = true
		psFilters.WalkValues("ancestor", func(ancestor string) error {
			img, err := daemon.imageService.ResolveImage(ctx, ancestor)
			if err != nil {
				logrus.Warnf("Error while looking up for image %v", ancestor)
				return nil
			}
			if imagesFilter[img.Digest] {
				// Already seen this ancestor, skip it
				return nil
			}
			// Then walk down the graph and put the imageIds in imagesFilter
			return populateImageFilterByParents(ctx, imagesFilter, img.Digest, daemon.childrenByID)
		})
	}

	publishFilter := map[nat.Port]bool{}
	err = psFilters.WalkValues("publish", portOp("publish", publishFilter))
	if err != nil {
		return nil, err
	}

	exposeFilter := map[nat.Port]bool{}
	err = psFilters.WalkValues("expose", portOp("expose", exposeFilter))
	if err != nil {
		return nil, err
	}

	return &listContext{
		filters:              psFilters,
		ancestorFilter:       ancestorFilter,
		images:               imagesFilter,
		exitAllowed:          filtExited,
		beforeFilter:         beforeContFilter,
		sinceFilter:          sinceContFilter,
		taskFilter:           taskFilter,
		isTask:               isTask,
		publish:              publishFilter,
		expose:               exposeFilter,
		ContainerListOptions: config,
		names:                view.GetAllNames(),
	}, nil
}

func idOrNameFilter(view container.View, value string) (*container.Snapshot, error) {
	filter, err := view.Get(value)
	switch err.(type) {
	case container.NoSuchContainerError:
		// Try name search instead
		found := ""
		for id, idNames := range view.GetAllNames() {
			for _, eachName := range idNames {
				if strings.TrimPrefix(value, "/") == strings.TrimPrefix(eachName, "/") {
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

func portOp(key string, filter map[nat.Port]bool) func(value string) error {
	return func(value string) error {
		if strings.Contains(value, ":") {
			return fmt.Errorf("filter for '%s' should not contain ':': %s", key, value)
		}
		//support two formats, original format <portnum>/[<proto>] or <startport-endport>/[<proto>]
		proto, port := nat.SplitProtoPort(value)
		start, end, err := nat.ParsePortRange(port)
		if err != nil {
			return fmt.Errorf("error while looking up for %s %s: %s", key, value, err)
		}
		for i := start; i <= end; i++ {
			p, err := nat.NewPort(proto, strconv.FormatUint(i, 10))
			if err != nil {
				return fmt.Errorf("error while looking up for %s %s: %s", key, value, err)
			}
			filter[p] = true
		}
		return nil
	}
}

// includeContainerInList decides whether a container should be included in the output or not based in the filter.
// It also decides if the iteration should be stopped or not.
func includeContainerInList(container *container.Snapshot, ctx *listContext) iterationAction {
	// Do not include container if it's in the list before the filter container.
	// Set the filter container to nil to include the rest of containers after this one.
	if ctx.beforeFilter != nil {
		if container.ID == ctx.beforeFilter.ID {
			ctx.beforeFilter = nil
		}
		return excludeContainer
	}

	// Stop iteration when the container arrives to the filter container
	if ctx.sinceFilter != nil {
		if container.ID == ctx.sinceFilter.ID {
			return stopIteration
		}
	}

	// Do not include container if it's stopped and we're not filters
	if !container.Running && !ctx.All && ctx.Limit <= 0 {
		return excludeContainer
	}

	// Do not include container if the name doesn't match
	if !ctx.filters.Match("name", container.Name) && !ctx.filters.Match("name", strings.TrimPrefix(container.Name, "/")) {
		return excludeContainer
	}

	// Do not include container if the id doesn't match
	if !ctx.filters.Match("id", container.ID) {
		return excludeContainer
	}

	if ctx.taskFilter {
		if ctx.isTask != container.Managed {
			return excludeContainer
		}
	}

	// Do not include container if any of the labels don't match
	if !ctx.filters.MatchKVList("label", container.Labels) {
		return excludeContainer
	}

	// Do not include container if isolation doesn't match
	if excludeContainer == excludeByIsolation(container, ctx) {
		return excludeContainer
	}

	// Stop iteration when the index is over the limit
	if ctx.Limit > 0 && ctx.idx == ctx.Limit {
		return stopIteration
	}

	// Do not include container if its exit code is not in the filter
	if len(ctx.exitAllowed) > 0 {
		shouldSkip := true
		for _, code := range ctx.exitAllowed {
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
	if !ctx.filters.Match("status", container.State) {
		return excludeContainer
	}

	// Do not include container if its health doesn't match the filter
	if !ctx.filters.ExactMatch("health", container.Health) {
		return excludeContainer
	}

	if ctx.filters.Contains("volume") {
		volumesByName := make(map[string]types.MountPoint)
		for _, m := range container.Mounts {
			if m.Name != "" {
				volumesByName[m.Name] = m
			} else {
				volumesByName[m.Source] = m
			}
		}
		volumesByDestination := make(map[string]types.MountPoint)
		for _, m := range container.Mounts {
			if m.Destination != "" {
				volumesByDestination[m.Destination] = m
			}
		}

		volumeExist := fmt.Errorf("volume mounted in container")
		err := ctx.filters.WalkValues("volume", func(value string) error {
			if _, exist := volumesByDestination[value]; exist {
				return volumeExist
			}
			if _, exist := volumesByName[value]; exist {
				return volumeExist
			}
			return nil
		})
		if err != volumeExist {
			return excludeContainer
		}
	}

	if ctx.ancestorFilter {
		if len(ctx.images) == 0 {
			return excludeContainer
		}
		if !ctx.images[digest.Digest(container.ImageID)] {
			return excludeContainer
		}
	}

	var (
		networkExist = errors.New("container part of network")
		noNetworks   = errors.New("container is not part of any networks")
	)
	if ctx.filters.Contains("network") {
		err := ctx.filters.WalkValues("network", func(value string) error {
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
		if err != networkExist {
			return excludeContainer
		}
	}

	if len(ctx.publish) > 0 {
		shouldSkip := true
		for port := range ctx.publish {
			if _, ok := container.PortBindings[port]; ok {
				shouldSkip = false
				break
			}
		}
		if shouldSkip {
			return excludeContainer
		}
	}

	if len(ctx.expose) > 0 {
		shouldSkip := true
		for port := range ctx.expose {
			if _, ok := container.ExposedPorts[port]; ok {
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

// refreshImage checks if the Image ref still points to the correct ID, and updates the ref to the actual ID when it doesn't
func (daemon *Daemon) refreshImage(s *container.Snapshot, ctx *listContext) (*types.Container, error) {
	c := s.Container
	updated := s.Image // keep the original ref if still valid (hasn't changed)
	if updated != s.ImageID {
		img, err := daemon.imageService.ResolveImage(context.TODO(), updated)
		if _, isDNE := err.(images.ErrImageDoesNotExist); err != nil && !isDNE {
			return nil, err
		}
		if err != nil || img.Digest.String() != s.ImageID {
			// ref changed, we need to use original ID
			updated = s.ImageID
		}
	}
	c.Image = updated
	return &c, nil
}

func populateImageFilterByParents(ctx context.Context, ancestorMap map[digest.Digest]bool, imageID digest.Digest, getChildren func(context.Context, digest.Digest) ([]digest.Digest, error)) error {
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

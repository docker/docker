package network

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/prune"
	"github.com/moby/moby/v2/daemon/internal/pruneprogress"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	dnetwork "github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/networkbackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

func (n *networkRouter) getNetworksList(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	filterArgs, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	filter, err := dnetwork.NewFilter(filterArgs)
	if err != nil {
		return err
	}

	if versions.LessThan(httputils.VersionFromContext(ctx), "1.28") {
		list, _ := n.cluster.GetNetworks(filter, false)
		var idx map[string]bool
		if len(list) > 0 {
			idx = make(map[string]bool, len(list))
			for _, n := range list {
				idx[n.ID] = true
			}
		}

		localNetworks, err := n.backend.GetNetworks(filter, backend.NetworkListConfig{WithServices: false})
		if err != nil {
			return err
		}
		for _, n := range localNetworks {
			if !idx[n.ID] {
				list = append(list, n)
			}
		}
		if list == nil {
			list = []network.Inspect{}
		}
		return httputils.WriteJSON(w, http.StatusOK, list)
	}

	list, _ := n.cluster.GetNetworkSummaries(filter)
	var idx map[string]bool
	if len(list) > 0 {
		idx = make(map[string]bool, len(list))
		for _, n := range list {
			idx[n.ID] = true
		}
	}

	// Combine the network list returned by Docker daemon if it is not already
	// returned by the cluster manager
	localNetworks, err := n.backend.GetNetworkSummaries(filter)
	if err != nil {
		return err
	}

	for _, n := range localNetworks {
		if !idx[n.ID] {
			list = append(list, n)
		}
	}

	if list == nil {
		list = []network.Summary{}
	}

	return httputils.WriteJSON(w, http.StatusOK, list)
}

type invalidRequestError struct {
	cause error
}

func (e invalidRequestError) Error() string {
	return e.cause.Error()
}

func (e invalidRequestError) InvalidParameter() {}

type ambiguousResultsError string

func (e ambiguousResultsError) Error() string {
	return "network " + string(e) + " is ambiguous"
}

func (ambiguousResultsError) InvalidParameter() {}

func (n *networkRouter) getNetwork(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	term := vars["id"]
	var (
		verbose bool
		err     error
	)
	if v := r.URL.Query().Get("verbose"); v != "" {
		if verbose, err = strconv.ParseBool(v); err != nil {
			return errors.Wrapf(invalidRequestError{err}, "invalid value for verbose: %s", v)
		}
	}
	networkScope := r.URL.Query().Get("scope")

	// In case multiple networks have duplicate names, return error.
	// TODO (yongtang): should we wrap with version here for backward compatibility?

	// First find based on full ID, return immediately once one is found.
	// If a network appears both in swarm and local, assume it is in local first

	// For full name and partial ID, save the result first, and process later
	// in case multiple records was found based on the same term
	listByFullName := map[string]network.Inspect{}
	listByPartialID := map[string]network.Inspect{}

	// TODO(@cpuguy83): All this logic for figuring out which network to return does not belong here
	// Instead there should be a backend function to just get one network.
	filterArgs := filters.NewArgs(filters.Arg("id", term))
	if networkScope != "" {
		filterArgs.Add("scope", networkScope)
	}
	filter, err := dnetwork.NewFilter(filterArgs)
	if err != nil {
		return err
	}
	filter.IDAlsoMatchesName = true

	withStatus := versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.52")
	networks, _ := n.backend.GetNetworks(filter, backend.NetworkListConfig{
		WithServices: verbose,
		WithStatus:   withStatus,
	})
	for _, nw := range networks {
		if nw.ID == term {
			return httputils.WriteJSON(w, http.StatusOK, nw)
		}
		if nw.Name == term {
			// No need to check the ID collision here as we are still in
			// local scope and the network ID is unique in this scope.
			listByFullName[nw.ID] = nw
		}
		if strings.HasPrefix(nw.ID, term) {
			// No need to check the ID collision here as we are still in
			// local scope and the network ID is unique in this scope.
			listByPartialID[nw.ID] = nw
		}
	}

	nwk, err := n.cluster.GetNetwork(term, withStatus)
	if err == nil {
		// If the get network is passed with a specific network ID / partial network ID
		// or if the get network was passed with a network name and scope as swarm
		// return the network. Skipped using isMatchingScope because it is true if the scope
		// is not set which would be case if the client API v1.30
		if strings.HasPrefix(nwk.ID, term) || networkScope == scope.Swarm {
			// If we have a previous match "backend", return it
			// along with the Status from the Swarm leader.
			// ex: overlay/partial_ID or name/swarm_scope
			if nwv, ok := listByPartialID[nwk.ID]; ok {
				nwv.Status = nwk.Status
				nwk = nwv
			} else if nwv, ok = listByFullName[nwk.ID]; ok {
				nwv.Status = nwk.Status
				nwk = nwv
			}
			return httputils.WriteJSON(w, http.StatusOK, nwk)
		}
	}

	networks, _ = n.cluster.GetNetworks(filter, withStatus)
	for _, nw := range networks {
		if nw.ID == term {
			return httputils.WriteJSON(w, http.StatusOK, nw)
		}
		if nw.Name == term {
			// Check the ID collision as we are in swarm scope here, and
			// the map (of the listByFullName) may have already had a
			// network with the same ID (from local scope previously)
			if nwk, ok := listByFullName[nw.ID]; ok {
				nwk.Status = nw.Status
				listByFullName[nw.ID] = nwk
			} else {
				listByFullName[nw.ID] = nw
			}
		}
		if strings.HasPrefix(nw.ID, term) {
			// Check the ID collision as we are in swarm scope here, and
			// the map (of the listByPartialID) may have already had a
			// network with the same ID (from local scope previously)
			if nwk, ok := listByPartialID[nw.ID]; ok {
				nwk.Status = nw.Status
				listByPartialID[nw.ID] = nwk
			} else {
				listByPartialID[nw.ID] = nw
			}
		}
	}

	// Find based on full name, returns true only if no duplicates
	if len(listByFullName) == 1 {
		for _, v := range listByFullName {
			return httputils.WriteJSON(w, http.StatusOK, v)
		}
	}
	if len(listByFullName) > 1 {
		return errors.Wrapf(ambiguousResultsError(term), "%d matches found based on name", len(listByFullName))
	}

	// Find based on partial ID, returns true only if no duplicates
	if len(listByPartialID) == 1 {
		for _, v := range listByPartialID {
			return httputils.WriteJSON(w, http.StatusOK, v)
		}
	}
	if len(listByPartialID) > 1 {
		return errors.Wrapf(ambiguousResultsError(term), "%d matches found based on ID prefix", len(listByPartialID))
	}

	return libnetwork.ErrNoSuchNetwork(term)
}

func (n *networkRouter) postNetworkCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var create network.CreateRequest
	if err := httputils.ReadJSON(r, &create); err != nil {
		return err
	}

	if nws, err := n.cluster.GetNetworksByName(create.Name); err == nil && len(nws) > 0 {
		return libnetwork.NetworkNameError(create.Name)
	}

	version := httputils.VersionFromContext(ctx)

	// EnableIPv4 was introduced in API 1.48.
	if versions.LessThan(version, "1.48") {
		create.EnableIPv4 = nil
	}

	// For a Swarm-scoped network, this call to backend.CreateNetwork is used to
	// validate the configuration. The network will not be created but, if the
	// configuration is valid, ManagerRedirectError will be returned and handled
	// below.
	nw, err := n.backend.CreateNetwork(ctx, create)
	if err != nil {
		if _, ok := err.(libnetwork.ManagerRedirectError); !ok {
			return err
		}
		id, err := n.cluster.CreateNetwork(create)
		if err != nil {
			return err
		}
		nw = &network.CreateResponse{
			ID: id,
		}
	}

	return httputils.WriteJSON(w, http.StatusCreated, nw)
}

func (n *networkRouter) postNetworkConnect(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var connect networkbackend.ConnectRequest
	if err := httputils.ReadJSON(r, &connect); err != nil {
		return err
	}

	// TODO: Do we want to apply it to the older APIs too?
	if versions.LessThan(httputils.VersionFromContext(ctx), "1.54") && connect.EndpointConfig != nil {
		connect.EndpointConfig.MacAddress = nil
	}

	// Unlike other operations, we does not check ambiguity of the name/ID here.
	// The reason is that, In case of attachable network in swarm scope, the actual local network
	// may not be available at the time. At the same time, inside daemon `ConnectContainerToNetwork`
	// does the ambiguity check anyway. Therefore, passing the name to daemon would be enough.
	return n.backend.ConnectContainerToNetwork(ctx, connect.Container, vars["id"], connect.EndpointConfig)
}

func (n *networkRouter) postNetworkDisconnect(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var disconnect networkbackend.DisconnectRequest
	if err := httputils.ReadJSON(r, &disconnect); err != nil {
		return err
	}

	return n.backend.DisconnectContainerFromNetwork(disconnect.Container, vars["id"], disconnect.Force)
}

func (n *networkRouter) deleteNetwork(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	nw, err := n.findUniqueNetwork(vars["id"])
	if err != nil {
		return err
	}
	if nw.Scope == "swarm" {
		if err = n.cluster.RemoveNetwork(nw.ID); err != nil {
			return err
		}
	} else {
		if err := n.backend.DeleteNetwork(nw.ID); err != nil {
			return err
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (n *networkRouter) postNetworkPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	pruneFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	return httputils.WritePruneResponse(ctx, w, r, func(ctx context.Context, progress func(prune.Object)) (any, error) {
		return n.backend.NetworkPrune(ctx, pruneprogress.Options{Filters: pruneFilters, Progress: progress})
	})
}

// findUniqueNetwork will search network across different scopes (both local and swarm).
// NOTE: This findUniqueNetwork is different from FindNetwork in the daemon.
// In case multiple networks have duplicate names, return error.
// First find based on full ID, return immediately once one is found.
// If a network appears both in swarm and local, assume it is in local first
// For full name and partial ID, save the result first, and process later
// in case multiple records was found based on the same term
// TODO (yongtang): should we wrap with version here for backward compatibility?
func (n *networkRouter) findUniqueNetwork(term string) (network.Inspect, error) {
	listByFullName := map[string]network.Inspect{}
	listByPartialID := map[string]network.Inspect{}

	filter, err := dnetwork.NewFilter(filters.NewArgs(filters.Arg("id", term)))
	if err != nil {
		return network.Inspect{}, err
	}
	filter.IDAlsoMatchesName = true

	networks, _ := n.backend.GetNetworks(filter, backend.NetworkListConfig{})
	for _, nw := range networks {
		if nw.ID == term {
			return nw, nil
		}
		if nw.Name == term && !nw.Ingress {
			// No need to check the ID collision here as we are still in
			// local scope and the network ID is unique in this scope.
			listByFullName[nw.ID] = nw
		}
		if strings.HasPrefix(nw.ID, term) {
			// No need to check the ID collision here as we are still in
			// local scope and the network ID is unique in this scope.
			listByPartialID[nw.ID] = nw
		}
	}

	networks, _ = n.cluster.GetNetworks(filter, false)
	for _, nw := range networks {
		if nw.ID == term {
			return nw, nil
		}
		if nw.Name == term {
			// Check the ID collision as we are in swarm scope here, and
			// the map (of the listByFullName) may have already had a
			// network with the same ID (from local scope previously)
			if _, ok := listByFullName[nw.ID]; !ok {
				listByFullName[nw.ID] = nw
			}
		}
		if strings.HasPrefix(nw.ID, term) {
			// Check the ID collision as we are in swarm scope here, and
			// the map (of the listByPartialID) may have already had a
			// network with the same ID (from local scope previously)
			if _, ok := listByPartialID[nw.ID]; !ok {
				listByPartialID[nw.ID] = nw
			}
		}
	}

	// Find based on full name, returns true only if no duplicates
	if len(listByFullName) == 1 {
		for _, v := range listByFullName {
			return v, nil
		}
	}
	if len(listByFullName) > 1 {
		return network.Inspect{}, errdefs.InvalidParameter(errors.Errorf("network %s is ambiguous (%d matches found based on name)", term, len(listByFullName)))
	}

	// Find based on partial ID, returns true only if no duplicates
	if len(listByPartialID) == 1 {
		for _, v := range listByPartialID {
			return v, nil
		}
	}
	if len(listByPartialID) > 1 {
		return network.Inspect{}, errdefs.InvalidParameter(errors.Errorf("network %s is ambiguous (%d matches found based on ID prefix)", term, len(listByPartialID)))
	}

	return network.Inspect{}, errdefs.NotFound(libnetwork.ErrNoSuchNetwork(term))
}

package controlapi

import (
	"context"
	"strings"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/identity"
	"github.com/docker/swarmkit/manager/state/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) CreateVolume(ctx context.Context, request *api.CreateVolumeRequest) (*api.CreateVolumeResponse, error) {
	if request.Spec == nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec must not be nil")
	}

	// validate the volume spec
	if request.Spec.Driver == nil {
		return nil, status.Errorf(codes.InvalidArgument, "driver must be specified")
	}

	if request.Spec.Annotations.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "meta: name must be provided")
	}

	volume := &api.Volume{
		ID:   identity.NewID(),
		Spec: *request.Spec,
	}
	err := s.store.Update(func(tx store.Tx) error {
		// verify that the specified driver exists
		clusters, err := store.FindClusters(tx, store.ByName(store.DefaultClusterName))
		if err == nil && len(clusters) == 1 {
			found := false
			for _, plugin := range clusters[0].Spec.CSIConfig.Plugins {
				if plugin.Name == volume.Spec.Driver.Name {
					found = true
					break
				}
			}

			if !found {
				return status.Errorf(
					codes.FailedPrecondition,
					"CSI plugin %v not found", volume.Spec.Driver.Name,
				)
			}
		}

		// check all secrets, so that we can return an error indicating ALL
		// missing secrets, instead of just the first one.
		var missingSecrets []string
		for _, secret := range volume.Spec.Secrets {
			s := store.GetSecret(tx, secret.Secret)
			if s == nil {
				missingSecrets = append(missingSecrets, secret.Secret)
			}
		}

		if len(missingSecrets) > 0 {
			secretStr := "secrets"
			if len(missingSecrets) == 1 {
				secretStr = "secret"
			}

			return status.Errorf(codes.InvalidArgument, "%s not found: %v", secretStr, strings.Join(missingSecrets, ", "))

		}

		return store.CreateVolume(tx, volume)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVolumeResponse{
		Volume: volume,
	}, nil
}

func (s *Server) UpdateVolume(ctx context.Context, request *api.UpdateVolumeRequest) (*api.UpdateVolumeResponse, error) {
	if request.VolumeID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "VolumeID must not be empty")
	}
	if request.Spec == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Spec must not be empty")
	}
	if request.VolumeVersion == nil {
		return nil, status.Errorf(codes.InvalidArgument, "VolumeVersion must not be empty")
	}

	var volume *api.Volume
	if err := s.store.Update(func(tx store.Tx) error {
		volume = store.GetVolume(tx, request.VolumeID)
		if volume == nil {
			return status.Errorf(codes.NotFound, "volume %v not found", request.VolumeID)
		}

		// compare specs, to see if any invalid fields have changed
		if request.Spec.Annotations.Name != volume.Spec.Annotations.Name {
			return status.Errorf(codes.InvalidArgument, "Name cannot be updated")
		}
		if request.Spec.Group != volume.Spec.Group {
			return status.Errorf(codes.InvalidArgument, "Group cannot be updated")
		}
		if request.Spec.AccessibilityRequirements != volume.Spec.AccessibilityRequirements {
			return status.Errorf(codes.InvalidArgument, "AccessibilityRequirements cannot be updated")
		}
		if request.Spec.Driver == nil || request.Spec.Driver.Name != volume.Spec.Driver.Name {
			return status.Errorf(codes.InvalidArgument, "Driver cannot be updated")
		}
		if request.Spec.AccessMode.Scope != volume.Spec.AccessMode.Scope || request.Spec.AccessMode.Sharing != volume.Spec.AccessMode.Sharing {
			return status.Errorf(codes.InvalidArgument, "AccessMode cannot be updated")
		}

		volume.Spec = *request.Spec
		volume.Meta.Version = *request.VolumeVersion
		if err := store.UpdateVolume(tx, volume); err != nil {
			return err
		}
		// read the volume back out, so it has the correct meta version
		// TODO(dperny): this behavior, while likely more correct, may not be
		// consistent with the rest of swarmkit...
		volume = store.GetVolume(tx, request.VolumeID)
		return nil
	}); err != nil {
		return nil, err
	}
	return &api.UpdateVolumeResponse{
		Volume: volume,
	}, nil
}

func (s *Server) ListVolumes(ctx context.Context, request *api.ListVolumesRequest) (*api.ListVolumesResponse, error) {
	var (
		volumes []*api.Volume
		err     error
	)

	// so the way we do this is with two filtering passes. first, we do a store
	// request, filtering on one of the parameters. then, from the result of
	// the store request, we filter on the remaining filters. This is necessary
	// because the store filters do not expose an AND function.
	s.store.View(func(tx store.ReadTx) {
		var by store.By = store.All
		switch {
		case request.Filters == nil:
			// short circuit to avoid nil pointer deref
		case len(request.Filters.Names) > 0:
			by = buildFilters(store.ByName, request.Filters.Names)
		case len(request.Filters.IDPrefixes) > 0:
			by = buildFilters(store.ByIDPrefix, request.Filters.IDPrefixes)
		case len(request.Filters.Groups) > 0:
			by = buildFilters(store.ByVolumeGroup, request.Filters.Groups)
		case len(request.Filters.Drivers) > 0:
			by = buildFilters(store.ByDriver, request.Filters.Drivers)
		case len(request.Filters.NamePrefixes) > 0:
			by = buildFilters(store.ByNamePrefix, request.Filters.NamePrefixes)
		}
		volumes, err = store.FindVolumes(tx, by)
	})
	if err != nil {
		return nil, err
	}
	if request.Filters == nil {
		return &api.ListVolumesResponse{Volumes: volumes}, nil
	}

	volumes = filterVolumes(volumes,
		// Names
		func(v *api.Volume) bool {
			return filterContains(v.Spec.Annotations.Name, request.Filters.Names)
		},
		// NamePrefixes
		func(v *api.Volume) bool {
			return filterContainsPrefix(v.Spec.Annotations.Name, request.Filters.NamePrefixes)
		},
		// IDPrefixes
		func(v *api.Volume) bool {
			return filterContainsPrefix(v.ID, request.Filters.IDPrefixes)
		},
		// Labels
		func(v *api.Volume) bool {
			return filterMatchLabels(v.Spec.Annotations.Labels, request.Filters.Labels)
		},
		// Groups
		func(v *api.Volume) bool {
			return filterContains(v.Spec.Group, request.Filters.Groups)
		},
		// Drivers
		func(v *api.Volume) bool {
			return v.Spec.Driver != nil && filterContains(v.Spec.Driver.Name, request.Filters.Drivers)
		},
	)

	return &api.ListVolumesResponse{
		Volumes: volumes,
	}, nil
}

func filterVolumes(candidates []*api.Volume, filters ...func(*api.Volume) bool) []*api.Volume {
	result := []*api.Volume{}
	for _, c := range candidates {
		match := true
		for _, f := range filters {
			if !f(c) {
				match = false
				break
			}
		}

		if match {
			result = append(result, c)
		}
	}
	return result
}

func (s *Server) GetVolume(ctx context.Context, request *api.GetVolumeRequest) (*api.GetVolumeResponse, error) {
	var volume *api.Volume
	s.store.View(func(tx store.ReadTx) {
		volume = store.GetVolume(tx, request.VolumeID)
	})
	if volume == nil {
		return nil, status.Errorf(codes.NotFound, "volume %v not found", request.VolumeID)
	}
	return &api.GetVolumeResponse{
		Volume: volume,
	}, nil
}

// RemoveVolume marks a Volume for removal. For a Volume to be removed, it must
// have Availability set to Drain. RemoveVolume does not immediately delete the
// volume, because some clean-up must occur before it can be removed. However,
// calling RemoveVolume is an irrevocable action, and once it occurs, the
// Volume can no longer be used in any way.
func (s *Server) RemoveVolume(ctx context.Context, request *api.RemoveVolumeRequest) (*api.RemoveVolumeResponse, error) {
	err := s.store.Update(func(tx store.Tx) error {
		volume := store.GetVolume(tx, request.VolumeID)
		if volume == nil {
			return status.Errorf(codes.NotFound, "volume %s not found", request.VolumeID)
		}
		if len(volume.PublishStatus) != 0 {
			return status.Error(codes.FailedPrecondition, "volume is still in use")
		}

		volume.PendingDelete = true
		return store.UpdateVolume(tx, volume)
	})

	if err != nil {
		return nil, err
	}
	return &api.RemoveVolumeResponse{}, nil
}

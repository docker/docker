package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/names"
	"github.com/moby/moby/v2/errdefs"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	servicenamegeneratorv0 "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0"
)

var (
	validContainerNameChars   = names.RestrictedNameChars
	validContainerNamePattern = names.RestrictedNamePattern
)

func (daemon *Daemon) registerName(container *container.Container) error {
	if container.ID == "" {
		return errors.New("invalid empty id")
	}
	if daemon.containers.Get(container.ID) != nil {
		// TODO(thaJeztah): should this be a panic (duplicate IDs due to invalid state on disk?)
		// TODO(thaJeztah): should this also check for container.ID being a prefix of another container's ID? (daemon.containersReplica.GetByPrefix); only should happen due to corruption / truncated ID.
		return errors.New("container is already loaded")
	}
	if container.Name == "" {
		name, err := daemon.generateAndReserveName(container.ID, container.Config.Image)
		if err != nil {
			return err
		}
		container.Name = name
		return nil
	}
	return daemon.containersReplica.ReserveName(container.Name, container.ID)
}

func (daemon *Daemon) generateIDAndName(name, image string) (string, string, error) {
	var (
		err error
		id  = stringid.GenerateRandomID()
	)

	if name == "" {
		if name, err = daemon.generateAndReserveName(id, image); err != nil {
			return "", "", err
		}
		return id, name, nil
	}

	if name, err = daemon.reserveName(id, name); err != nil {
		return "", "", err
	}

	return id, name, nil
}

func (daemon *Daemon) reserveName(id, name string) (string, error) {
	effectiveName := strings.TrimPrefix(name, "/")
	if len(effectiveName) < 2 {
		return "", errdefs.InvalidParameter(fmt.Errorf("invalid container name (%s): name must be at least 2 characters", effectiveName))
	}
	if !validContainerNamePattern.MatchString(effectiveName) {
		return "", errdefs.InvalidParameter(fmt.Errorf("invalid container name (%s): only %s are allowed", effectiveName, validContainerNameChars))
	}
	if name[0] != '/' {
		name = "/" + name
	}

	if err := daemon.containersReplica.ReserveName(name, id); err != nil {
		if cerrdefs.IsConflict(err) {
			id, err := daemon.containersReplica.Snapshot().GetID(name)
			if err != nil {
				log.G(context.TODO()).Errorf("got unexpected error while looking up reserved name: %v", err)
				return "", err
			}
			return "", nameConflictError{id: id, name: name}
		}
		return "", fmt.Errorf("error reserving name: %q: %w", name, err)
	}
	return name, nil
}

func (daemon *Daemon) releaseName(name string) {
	daemon.containersReplica.ReleaseName(name)
}

// GenerateServiceName resolves the configured service name-generator extension.
func (daemon *Daemon) GenerateServiceName(ctx context.Context, retry int, image string) (string, error) {
	if daemon.extensionHost == nil {
		return "", errors.New("name generator extension host is not configured")
	}

	name, err := resolveServiceName(ctx, daemon.extensionHost, &servicenamegeneratorv0.GenerateServiceNameRequest{
		Retry: int64(retry),
		Image: image,
	})
	return generatedName(ctx, name, err)
}

func (daemon *Daemon) generateContainerName(ctx context.Context, req *containernamegeneratorv0.GenerateContainerNameRequest) (string, error) {
	if daemon.extensionHost == nil {
		return "", errors.New("name generator extension host is not configured")
	}

	name, err := resolveContainerName(ctx, daemon.extensionHost, req)
	return generatedName(ctx, name, err)
}

func generatedName(ctx context.Context, name string, err error) (string, error) {
	if name != "" {
		if err != nil {
			log.G(ctx).WithError(err).Warn("name generator extension failed; using built-in fallback")
		}
		return name, nil
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("name generator returned no name")
}

func (daemon *Daemon) generateAndReserveName(id, image string) (string, error) {
	var name string
	for i := range 6 {
		var err error
		name, err = daemon.generateContainerName(context.TODO(), &containernamegeneratorv0.GenerateContainerNameRequest{
			Retry:       int64(i),
			ContainerID: id,
			Image:       image,
		})
		if err != nil {
			return "", err
		}
		if name[0] != '/' {
			name = "/" + name
		}

		if err := daemon.containersReplica.ReserveName(name, id); err != nil {
			if cerrdefs.IsConflict(err) {
				continue
			}
			return "", err
		}
		return name, nil
	}

	name = "/" + stringid.TruncateID(id)
	if err := daemon.containersReplica.ReserveName(name, id); err != nil {
		return "", err
	}
	return name, nil
}

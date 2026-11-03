package daemon

import (
	"context"
	"fmt"
	"maps"
	"runtime"
	"strings"
	"time"

	"github.com/distribution/reference"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/builder/dockerfile"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

type mergeOptions struct {
	keepReservedLabels bool
}

// merge merges two Config, the image container configuration (defaults values),
// and the user container configuration, either passed by the API or generated
// by the cli.
// It will mutate the specified user configuration (userConf) with the image
// configuration where the user configuration is incomplete.
func merge(userConf, imageConf *containertypes.Config, options mergeOptions) error {
	if userConf.User == "" {
		userConf.User = imageConf.User
	}
	if len(userConf.ExposedPorts) == 0 {
		userConf.ExposedPorts = imageConf.ExposedPorts
	} else if imageConf.ExposedPorts != nil {
		for port := range imageConf.ExposedPorts {
			if _, exists := userConf.ExposedPorts[port]; !exists {
				userConf.ExposedPorts[port] = struct{}{}
			}
		}
	}

	if len(userConf.Env) == 0 {
		userConf.Env = imageConf.Env
	} else {
		for _, imageEnv := range imageConf.Env {
			found := false
			imageEnvKey, _, _ := strings.Cut(imageEnv, "=")
			for _, userEnv := range userConf.Env {
				userEnvKey, _, _ := strings.Cut(userEnv, "=")
				if runtime.GOOS == "windows" {
					// Case insensitive environment variables on Windows
					found = strings.EqualFold(imageEnvKey, userEnvKey)
				} else {
					found = imageEnvKey == userEnvKey
				}
				if found {
					break
				}
			}
			if !found {
				userConf.Env = append(userConf.Env, imageEnv)
			}
		}
	}

	if userConf.Labels == nil {
		userConf.Labels = map[string]string{}
	}
	for l, v := range imageConf.Labels {
		if !options.keepReservedLabels && opts.IsReservedLabelNamespace(l) {
			continue
		}
		if _, ok := userConf.Labels[l]; !ok {
			userConf.Labels[l] = v
		}
	}

	if len(userConf.Entrypoint) == 0 {
		if len(userConf.Cmd) == 0 {
			userConf.Cmd = imageConf.Cmd
		}

		if userConf.Entrypoint == nil {
			userConf.Entrypoint = imageConf.Entrypoint
		}
	}
	if imageConf.Healthcheck != nil {
		if userConf.Healthcheck == nil {
			userConf.Healthcheck = imageConf.Healthcheck
		} else {
			if len(userConf.Healthcheck.Test) == 0 {
				userConf.Healthcheck.Test = imageConf.Healthcheck.Test
			}
			if userConf.Healthcheck.Interval == 0 {
				userConf.Healthcheck.Interval = imageConf.Healthcheck.Interval
			}
			if userConf.Healthcheck.Timeout == 0 {
				userConf.Healthcheck.Timeout = imageConf.Healthcheck.Timeout
			}
			if userConf.Healthcheck.StartPeriod == 0 {
				userConf.Healthcheck.StartPeriod = imageConf.Healthcheck.StartPeriod
			}
			if userConf.Healthcheck.StartInterval == 0 {
				userConf.Healthcheck.StartInterval = imageConf.Healthcheck.StartInterval
			}
			if userConf.Healthcheck.Retries == 0 {
				userConf.Healthcheck.Retries = imageConf.Healthcheck.Retries
			}
		}
	}

	if userConf.WorkingDir == "" {
		userConf.WorkingDir = imageConf.WorkingDir
	}
	if len(userConf.Volumes) == 0 {
		userConf.Volumes = imageConf.Volumes
	} else {
		maps.Copy(userConf.Volumes, imageConf.Volumes)
	}

	if userConf.StopSignal == "" {
		userConf.StopSignal = imageConf.StopSignal
	}
	return nil
}

// CreateImageFromContainer creates a new image from a container. The container
// config will be updated by applying the change set to the custom config, then
// applying that config over the existing container config.
func (daemon *Daemon) CreateImageFromContainer(ctx context.Context, name string, c *backend.CreateImageConfig) (string, error) {
	start := time.Now()

	container, err := daemon.GetContainer(name)
	if err != nil {
		return "", err
	}

	// It is not possible to commit a running container on Windows
	if runtime.GOOS == "windows" && container.State.IsRunning() {
		return "", errors.Errorf("%+v does not support commit of a running container", runtime.GOOS)
	}

	if container.State.IsDead() {
		return "", errdefs.Conflict(fmt.Errorf("You cannot commit container %s which is Dead", container.ID))
	}

	if container.State.IsRemovalInProgress() {
		return "", errdefs.Conflict(fmt.Errorf("You cannot commit container %s which is being removed", container.ID))
	}

	if !c.NoPause && !container.State.IsPaused() {
		_ = daemon.containerPause(container)
		defer func() {
			_ = daemon.containerUnpause(container)
		}()
	}

	if c.Config == nil {
		c.Config = container.Config
	}
	newConfig, err := dockerfile.BuildFromConfig(ctx, c.Config, c.Changes, container.ImagePlatform.OS)
	if err != nil {
		return "", err
	}
	if err := merge(newConfig, container.Config, mergeOptions{}); err != nil {
		return "", err
	}

	id, err := daemon.imageService.CommitImage(ctx, backend.CommitConfig{
		Author:              c.Author,
		Comment:             c.Comment,
		Config:              newConfig,
		ContainerConfig:     container.Config,
		ContainerID:         container.ID,
		ContainerMountLabel: container.MountLabel,
		ContainerOS:         container.ImagePlatform.OS,
		ParentImageID:       string(container.ImageID),
	})
	if err != nil {
		return "", err
	}

	imageRef := ""
	if c.Tag != nil {
		err = daemon.imageService.TagImage(ctx, id, c.Tag)
		if err != nil {
			return "", err
		}
		imageRef = reference.FamiliarString(c.Tag)
	}
	daemon.LogContainerEventWithAttributes(container, events.ActionCommit, map[string]string{
		"comment":  c.Comment,
		"imageID":  id.String(),
		"imageRef": imageRef,
	})
	metrics.ContainerActions.WithValues("commit").UpdateSince(start)
	return id.String(), nil
}

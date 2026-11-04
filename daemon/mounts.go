package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/log"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/v2/daemon/container"
	volumesservice "github.com/moby/moby/v2/daemon/volume/service"
)

func (daemon *Daemon) prepareMountPoints(ctr *container.Container) error {
	ctr.Lock()
	defer ctr.Unlock()

	alive := ctr.State.Running
	for _, config := range ctr.MountPoints {
		if err := daemon.lazyInitializeVolume(ctr.ID, config); err != nil {
			return err
		}

		// Restore reference to image mount layer
		if config.Type == mounttypes.TypeImage && config.Layer == nil {
			layer, err := daemon.imageService.GetLayerByID(config.ID)
			if err != nil {
				return err
			}

			config.Layer = layer
		}

		if config.Type != mounttypes.TypeVolume && config.Type != mounttypes.TypeCluster {
			continue
		}
		if config.Volume == nil {
			continue
		}
		if alive {
			log.G(context.TODO()).WithFields(log.Fields{
				"container": ctr.ID,
				"volume":    config.Volume.Name(),
			}).Debug("Live-restoring volume for alive container")
			if err := config.LiveRestore(context.TODO()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (daemon *Daemon) removeMountPoints(ctr *container.Container, rm bool) error {
	var rmErrors []string
	ctx := context.TODO()
	for _, m := range ctr.MountPoints {
		if m.Type == mounttypes.TypeVolume {
			if m.Volume == nil {
				continue
			}
			daemon.volumes.Release(ctx, m.Volume.Name(), ctr.ID)
			if !rm {
				continue
			}

			// Do not remove named mountpoints
			// these are mountpoints specified like `docker run -v <name>:/foo`
			if m.Spec.Source != "" {
				continue
			}

			err := daemon.volumes.Remove(ctx, m.Volume.Name())
			// Ignore volume in use errors because having this
			// volume being referenced by other container is
			// not an error, but an implementation detail.
			// This prevents docker from logging "ERROR: Volume in use"
			// where there is another container using the volume.
			if err != nil && !volumesservice.IsInUse(err) {
				rmErrors = append(rmErrors, err.Error())
			}
		}

		if m.Type == mounttypes.TypeImage {
			layer := m.Layer
			if layer != nil {
				err := layer.Unmount()
				if err != nil {
					rmErrors = append(rmErrors, err.Error())
					continue
				}
				err = daemon.imageService.ReleaseLayer(layer)
				if err != nil {
					rmErrors = append(rmErrors, err.Error())
					continue
				}
			} else {
				rmErrors = append(rmErrors, "layer not found for image "+m.Name)
			}
		}
	}

	if len(rmErrors) > 0 {
		return fmt.Errorf("Error removing volumes:\n%v", strings.Join(rmErrors, "\n"))
	}
	return nil
}

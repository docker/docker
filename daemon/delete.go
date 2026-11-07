package daemon

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/containerfs"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/pkg/errors"
)

// ContainerRm removes the container id from the filesystem. An error
// is returned if the container is not found, or if the remove
// fails. If the remove succeeds, the container name is released, and
// network links are removed.
func (daemon *Daemon) ContainerRm(name string, config *backend.ContainerRmConfig) error {
	return daemon.containerRm(&daemon.config().Config, name, config)
}

func (daemon *Daemon) containerRm(cfg *config.Config, name string, opts *backend.ContainerRmConfig) error {
	start := time.Now()
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	// Container state RemovalInProgress should be used to avoid races.
	if inProgress := ctr.State.SetRemovalInProgress(); inProgress {
		// TODO(thaJeztah): Consider waiting for an in-progress removal instead of returning a conflict.
		// Concurrent removal is not itself a conflicting operation, and using Conflict makes it
		// indistinguishable from actual state conflicts, such as attempting to remove a running
		// or paused container without force.
		return errdefs.Conflict(fmt.Errorf("removal of container %s is already in progress", name))
	}
	defer ctr.State.ResetRemovalInProgress()

	// check if container wasn't deregistered by previous rm since Get
	if c := daemon.containers.Get(ctr.ID); c == nil {
		return nil
	}

	if opts.RemoveLink {
		return daemon.rmLink(cfg, ctr, name)
	}

	err = daemon.cleanupContainer(ctr, *opts)
	metrics.ContainerActions.WithValues("delete").UpdateSince(start)
	if err != nil {
		return fmt.Errorf("cannot remove container %q: %w", name, err)
	}
	return nil
}

func (daemon *Daemon) rmLink(cfg *config.Config, ctr *container.Container, name string) error {
	if name[0] != '/' {
		name = "/" + name
	}
	parent, n := path.Split(name)
	if parent == "/" {
		return errors.New("Conflict, cannot remove the default link name of the container")
	}

	parent = strings.TrimSuffix(parent, "/")
	parentID, err := daemon.containersReplica.Snapshot().GetID(parent)
	if err != nil {
		return fmt.Errorf("Cannot get parent %s for link name %s", parent, name)
	}

	daemon.releaseName(name)
	if parentContainer := daemon.containers.Get(parentID); parentContainer != nil {
		daemon.linkIndex.unlink(name, ctr, parentContainer)
		if err := daemon.updateNetwork(cfg, parentContainer); err != nil {
			log.G(context.TODO()).Debugf("Could not update network to remove link %s: %v", n, err)
		}
	}
	return nil
}

// cleanupContainer unregisters a container from the daemon, stops stats
// collection and cleanly removes contents and metadata from the filesystem.
func (daemon *Daemon) cleanupContainer(ctr *container.Container, config backend.ContainerRmConfig) error {
	if ctr.State.IsRunning() {
		if !config.ForceRemove {
			if ctr.State.Paused {
				return errdefs.Conflict(errors.New("container is paused and must be unpaused first"))
			} else {
				return errdefs.Conflict(fmt.Errorf("container is %s: stop the container before removing or force remove", ctr.State.State()))
			}
		}
		if err := daemon.kill(context.TODO(), ctr); err != nil && !isNotRunning(err) {
			return fmt.Errorf("could not kill container: %w", err)
		}
	}

	// stop collection of stats for the container regardless
	// if stats are currently getting collected.
	daemon.statsCollector.StopCollection(ctr)

	// stopTimeout is the number of seconds to wait for the container to stop
	// gracefully before forcibly killing it.
	//
	// Why 3 seconds? The timeout specified here was originally added in commit
	// 1615bb08c7c3fc6c4b22db0a633edda516f97cf0, which added a custom timeout to
	// some commands, but lacking an option for a timeout on "docker rm", was
	// hardcoded to 10 seconds. Commit 28fd289b448164b77affd8103c0d96fd8110daf9
	// later on updated this to 3 seconds (but no background on that change).
	//
	// If you arrived here and know the answer, you earned yourself a picture
	// of a cute animal of your own choosing.
	stopTimeout := 3
	if err := daemon.containerStop(context.TODO(), ctr, backend.ContainerStopOptions{Timeout: &stopTimeout}); err != nil {
		return err
	}

	// Mark container dead. We don't want anybody to be restarting it.
	ctr.Lock()
	ctr.State.SetDead()

	// Copy RWLayer for releasing and clear the reference while holding the container lock.
	rwLayer := ctr.RWLayer
	ctr.RWLayer = nil

	// Save container state to disk. So that if error happens before
	// container meta file got removed from disk, then a restart of
	// docker should not make a dead container alive.
	if err := ctr.CheckpointTo(context.WithoutCancel(context.TODO()), daemon.containersReplica); err != nil && !os.IsNotExist(err) {
		log.G(context.TODO()).Errorf("Error saving dying container to disk: %v", err)
	}
	ctr.Unlock()

	// When container creation fails and `RWLayer` has not been created yet, we
	// do not call `ReleaseRWLayer`
	if rwLayer != nil {
		if err := daemon.imageService.ReleaseLayer(rwLayer); err != nil {
			// Restore the reference on error as it possibly was not released.
			ctr.Lock()
			ctr.RWLayer = rwLayer
			ctr.Unlock()
			ctr.State.SetRemovalError(err)
			return err
		}
	}

	// Hold the container lock while deleting the container root directory
	// so that other goroutines don't attempt to concurrently open files
	// within it. Having any file open on Windows (without the
	// FILE_SHARE_DELETE flag) will block it from being deleted.
	//
	// TODO(thaJeztah): should this be moved to the "container" itself, or possibly be delegated to the graphdriver or snapshotter?
	ctr.Lock()
	err := containerfs.EnsureRemoveAll(ctr.Root)
	ctr.Unlock()
	if err != nil {
		err = errors.Wrap(err, "unable to remove filesystem")
		ctr.State.SetRemovalError(err)
		return err
	}

	linkNames := daemon.linkIndex.delete(ctr)
	if ctr.ProcessLabel != "" {
		selinux.ReleaseLabel(ctr.ProcessLabel)
	}
	daemon.containers.Delete(ctr.ID)
	daemon.containersReplica.Delete(ctr)
	if err := daemon.removeMountPoints(ctr, config.RemoveVolume); err != nil {
		log.G(context.TODO()).Error(err)
	}
	for _, name := range linkNames {
		daemon.releaseName(name)
	}
	ctr.State.SetRemoved()
	metrics.StateCtr.Delete(ctr.ID)

	daemon.LogContainerEvent(ctr, events.ActionDestroy)
	return nil
}

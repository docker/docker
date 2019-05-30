package containerd // import "github.com/docker/docker/plugin/executor/containerd"

import (
	"context"
	"io"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/libcontainerd"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// PluginNamespace is the name used for the plugins namespace
const PluginNamespace = "plugins.moby"

// ExitHandler represents an object that is called when the exit event is received from containerd
type ExitHandler interface {
	HandleExitEvent(id string) error
}

// New creates a new containerd plugin executor
func New(ctx context.Context, rootDir string, cli *containerd.Client, exitHandler ExitHandler) (*Executor, error) {
	e := &Executor{
		rootDir:     rootDir,
		exitHandler: exitHandler,
	}

	client, err := libcontainerd.NewClient(ctx, cli, rootDir, PluginNamespace, e)
	if err != nil {
		return nil, errors.Wrap(err, "error creating containerd exec client")
	}
	e.client = client
	return e, nil
}

// Executor is the containerd client implementation of a plugin executor
type Executor struct {
	rootDir     string
	client      libcontainerdtypes.Client
	exitHandler ExitHandler
}

// deleteTaskAndContainer deletes plugin task and then plugin container from containerd
func deleteTaskAndContainer(ctx context.Context, cli libcontainerdtypes.Client, id string, p libcontainerdtypes.Process) {
	if p != nil {
		if _, _, err := p.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			logrus.WithError(err).WithField("id", id).Error("failed to delete plugin task from containerd")
		}
	} else {
		if _, _, err := cli.DeleteTask(ctx, id); err != nil && !errdefs.IsNotFound(err) {
			logrus.WithError(err).WithField("id", id).Error("failed to delete plugin task from containerd")
		}
	}

	if err := cli.Delete(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		logrus.WithError(err).WithField("id", id).Error("failed to delete plugin container from containerd")
	}
}

// Create creates a new container
func (e *Executor) Create(id string, spec specs.Spec, stdout, stderr io.WriteCloser) error {
	opts := runctypes.RuncOptions{
		RuntimeRoot: filepath.Join(e.rootDir, "runtime-root"),
	}
	ctx := context.Background()
	err := e.client.Create(ctx, id, &spec, &opts)
	if err != nil {
		status, err2 := e.client.Status(ctx, id)
		if err2 != nil {
			if !errdefs.IsNotFound(err2) {
				logrus.WithError(err2).WithField("id", id).Warn("Received an error while attempting to read plugin status")
			}
		} else {
			if status != containerd.Running && status != containerd.Unknown {
				if err2 := e.client.Delete(ctx, id); err2 != nil && !errdefs.IsNotFound(err2) {
					logrus.WithError(err2).WithField("plugin", id).Error("Error cleaning up containerd container")
				}
				err = e.client.Create(ctx, id, &spec, &opts)
			}
		}

		if err != nil {
			return errors.Wrap(err, "error creating containerd container")
		}
	}

	_, err = e.client.Start(ctx, id, "", false, attachStreamsFunc(stdout, stderr))
	if err != nil {
		deleteTaskAndContainer(ctx, e.client, id, nil)
	}
	return err
}

// Restore restores a container
func (e *Executor) Restore(id string, stdout, stderr io.WriteCloser) (bool, error) {
	alive, _, p, err := e.client.Restore(context.Background(), id, attachStreamsFunc(stdout, stderr))
	if err != nil && !errdefs.IsNotFound(err) {
		return false, err
	}
	if !alive {
		deleteTaskAndContainer(context.Background(), e.client, id, p)
	}
	return alive, nil
}

// IsRunning returns if the container with the given id is running
func (e *Executor) IsRunning(id string) (bool, error) {
	status, err := e.client.Status(context.Background(), id)
	return status == containerd.Running, err
}

// Signal sends the specified signal to the container
func (e *Executor) Signal(id string, signal int) error {
	return e.client.SignalProcess(context.Background(), id, libcontainerdtypes.InitProcessName, signal)
}

// ProcessEvent handles events from containerd
// All events are ignored except the exit event, which is sent of to the stored handler
func (e *Executor) ProcessEvent(id string, et libcontainerdtypes.EventType, ei libcontainerdtypes.EventInfo) error {
	switch et {
	case libcontainerdtypes.EventExit:
		deleteTaskAndContainer(context.Background(), e.client, id, nil)
		return e.exitHandler.HandleExitEvent(ei.ContainerID)
	}
	return nil
}

type rio struct {
	cio.IO

	wg sync.WaitGroup
}

func (c *rio) Wait() {
	c.wg.Wait()
	c.IO.Wait()
}

func attachStreamsFunc(stdout, stderr io.WriteCloser) libcontainerdtypes.StdioCallback {
	return func(iop *cio.DirectIO) (cio.IO, error) {
		if iop.Stdin != nil {
			iop.Stdin.Close()
			// closing stdin shouldn't be needed here, it should never be open
			panic("plugin stdin shouldn't have been created!")
		}

		rio := &rio{IO: iop}
		rio.wg.Add(2)
		go func() {
			io.Copy(stdout, iop.Stdout)
			stdout.Close()
			rio.wg.Done()
		}()
		go func() {
			io.Copy(stderr, iop.Stderr)
			stderr.Close()
			rio.wg.Done()
		}()
		return rio, nil
	}
}

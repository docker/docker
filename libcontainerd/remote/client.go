package remote // import "github.com/docker/docker/libcontainerd/remote"

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/containerd/containerd"
	apievents "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/content"
	containerderrors "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	"github.com/containerd/typeurl"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/libcontainerd/queue"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"

	"github.com/docker/docker/pkg/ioutils"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DockerContainerBundlePath is the label key pointing to the container's bundle path
const DockerContainerBundlePath = "com.docker/engine.bundle.path"

type client struct {
	client   *containerd.Client
	stateDir string
	logger   *logrus.Entry
	ns       string

	backend libcontainerdtypes.Backend
	eventQ  queue.Queue
	oomMu   sync.Mutex
	oom     map[string]bool
}

// NewClient creates a new libcontainerd client from a containerd client
func NewClient(ctx context.Context, cli *containerd.Client, stateDir, ns string, b libcontainerdtypes.Backend) (libcontainerdtypes.Client, error) {
	c := &client{
		client:   cli,
		stateDir: stateDir,
		logger:   logrus.WithField("module", "libcontainerd").WithField("namespace", ns),
		ns:       ns,
		backend:  b,
		oom:      make(map[string]bool),
	}

	go c.processEventStream(ctx, ns)

	return c, nil
}

func (c *client) Version(ctx context.Context) (containerd.Version, error) {
	return c.client.Version(ctx)
}

// Restore loads the containerd container.
// It should not be called concurrently with any other operation for the given ID.
func (c *client) Restore(ctx context.Context, id string, attachStdio libcontainerdtypes.StdioCallback) (alive bool, pid int, p libcontainerdtypes.Process, err error) {
	var dio *cio.DirectIO
	defer func() {
		if err != nil && dio != nil {
			dio.Cancel()
			dio.Close()
		}
		err = wrapError(err)
	}()

	ctr, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return false, -1, nil, errors.WithStack(wrapError(err))
	}

	attachIO := func(fifos *cio.FIFOSet) (cio.IO, error) {
		// dio must be assigned to the previously defined dio for the defer above
		// to handle cleanup
		dio, err = c.newDirectIO(ctx, fifos)
		if err != nil {
			return nil, err
		}
		return attachStdio(dio)
	}
	t, err := ctr.Task(ctx, attachIO)
	if err != nil && !containerderrors.IsNotFound(err) {
		return false, -1, nil, errors.Wrap(wrapError(err), "error getting containerd task for container")
	}

	if t != nil {
		s, err := t.Status(ctx)
		if err != nil {
			return false, -1, nil, errors.Wrap(wrapError(err), "error getting task status")
		}
		alive = s.Status != containerd.Stopped
		pid = int(t.Pid())
	}

	c.logger.WithFields(logrus.Fields{
		"container": id,
		"alive":     alive,
		"pid":       pid,
	}).Debug("restored container")

	return alive, pid, &restoredProcess{
		p: t,
	}, nil
}

func (c *client) Create(ctx context.Context, id string, ociSpec *specs.Spec, runtimeOptions interface{}) error {
	bdir := c.bundleDir(id)
	c.logger.WithField("bundle", bdir).WithField("root", ociSpec.Root.Path).Debug("bundle dir created")

	_, err := c.client.NewContainer(ctx, id,
		containerd.WithSpec(ociSpec),
		containerd.WithRuntime(runtimeName, runtimeOptions),
		WithBundle(bdir, ociSpec),
	)
	if err != nil {
		if containerderrors.IsAlreadyExists(err) {
			return errors.WithStack(errdefs.Conflict(errors.New("id already in use")))
		}
		return wrapError(err)
	}
	return nil
}

// Start create and start a task for the specified containerd id
func (c *client) Start(ctx context.Context, id, checkpointDir string, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (int, error) {
	ctr, err := c.getContainer(ctx, id)
	if err != nil {
		return -1, err
	}
	var (
		cp             *types.Descriptor
		t              containerd.Task
		rio            cio.IO
		stdinCloseSync = make(chan struct{})
	)

	if checkpointDir != "" {
		// write checkpoint to the content store
		tar := archive.Diff(ctx, "", checkpointDir)
		cp, err = c.writeContent(ctx, images.MediaTypeContainerd1Checkpoint, checkpointDir, tar)
		// remove the checkpoint when we're done
		defer func() {
			if cp != nil {
				err := c.client.ContentStore().Delete(context.Background(), cp.Digest)
				if err != nil {
					c.logger.WithError(err).WithFields(logrus.Fields{
						"ref":    checkpointDir,
						"digest": cp.Digest,
					}).Warnf("failed to delete temporary checkpoint entry")
				}
			}
		}()
		if err := tar.Close(); err != nil {
			return -1, errors.Wrap(err, "failed to close checkpoint tar stream")
		}
		if err != nil {
			return -1, errors.Wrapf(err, "failed to upload checkpoint to containerd")
		}
	}

	spec, err := ctr.Spec(ctx)
	if err != nil {
		return -1, errors.Wrap(err, "failed to retrieve spec")
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return -1, errors.Wrap(err, "failed to retreive labels")
	}
	bundle := labels[DockerContainerBundlePath]
	uid, gid := getSpecUser(spec)
	t, err = ctr.NewTask(ctx,
		func(id string) (cio.IO, error) {
			fifos := newFIFOSet(bundle, libcontainerdtypes.InitProcessName, withStdin, spec.Process.Terminal)

			rio, err = c.createIO(fifos, id, libcontainerdtypes.InitProcessName, stdinCloseSync, attachStdio)
			return rio, err
		},
		func(_ context.Context, _ *containerd.Client, info *containerd.TaskInfo) error {
			info.Checkpoint = cp
			if runtime.GOOS != "windows" {
				info.Options = &runctypes.CreateOptions{
					IoUid:       uint32(uid),
					IoGid:       uint32(gid),
					NoPivotRoot: os.Getenv("DOCKER_RAMDISK") != "",
				}
			} else {
				// Make sure we set the runhcs options to debug if we are at debug level.
				if c.logger.Level == logrus.DebugLevel {
					info.Options = &options.Options{Debug: true}
				}
			}
			return nil
		})
	if err != nil {
		close(stdinCloseSync)
		if rio != nil {
			rio.Cancel()
			rio.Close()
		}
		return -1, wrapError(err)
	}

	// Signal c.createIO that it can call CloseIO
	close(stdinCloseSync)

	if err := t.Start(ctx); err != nil {
		if _, err := t.Delete(ctx); err != nil {
			c.logger.WithError(err).WithField("container", id).
				Error("failed to delete task after fail start")
		}
		return -1, wrapError(err)
	}

	return int(t.Pid()), nil
}

// Exec creates exec process.
//
// The containerd client calls Exec to register the exec config in the shim side.
// When the client calls Start, the shim will create stdin fifo if needs. But
// for the container main process, the stdin fifo will be created in Create not
// the Start call. stdinCloseSync channel should be closed after Start exec
// process.
func (c *client) Exec(ctx context.Context, containerID, processID string, spec *specs.Process, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (int, error) {
	ctr, err := c.getContainer(ctx, containerID)
	if err != nil {
		return -1, err
	}
	t, err := ctr.Task(ctx, nil)
	if err != nil {
		if containerderrors.IsNotFound(err) {
			return -1, errors.WithStack(errdefs.InvalidParameter(errors.New("container is not running")))
		}
		return -1, wrapError(err)
	}

	var (
		p              containerd.Process
		rio            cio.IO
		stdinCloseSync = make(chan struct{})
	)

	labels, err := ctr.Labels(ctx)
	if err != nil {
		return -1, wrapError(err)
	}

	fifos := newFIFOSet(labels[DockerContainerBundlePath], processID, withStdin, spec.Terminal)

	defer func() {
		if err != nil {
			if rio != nil {
				rio.Cancel()
				rio.Close()
			}
		}
	}()

	p, err = t.Exec(ctx, processID, spec, func(id string) (cio.IO, error) {
		rio, err = c.createIO(fifos, containerID, processID, stdinCloseSync, attachStdio)
		return rio, err
	})
	if err != nil {
		close(stdinCloseSync)
		if containerderrors.IsAlreadyExists(err) {
			return -1, errors.WithStack(errdefs.Conflict(errors.New("id already in use")))
		}
		return -1, wrapError(err)
	}

	// Signal c.createIO that it can call CloseIO
	//
	// the stdin of exec process will be created after p.Start in containerd
	defer close(stdinCloseSync)

	if err = p.Start(ctx); err != nil {
		// use new context for cleanup because old one may be cancelled by user, but leave a timeout to make sure
		// we are not waiting forever if containerd is unresponsive or to work around fifo cancelling issues in
		// older containerd-shim
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		p.Delete(ctx)
		return -1, wrapError(err)
	}
	return int(p.Pid()), nil
}

func (c *client) SignalProcess(ctx context.Context, containerID, processID string, signal int) error {
	p, err := c.getProcess(ctx, containerID, processID)
	if err != nil {
		return err
	}
	return wrapError(p.Kill(ctx, syscall.Signal(signal)))
}

func (c *client) ResizeTerminal(ctx context.Context, containerID, processID string, width, height int) error {
	p, err := c.getProcess(ctx, containerID, processID)
	if err != nil {
		return err
	}

	return p.Resize(ctx, uint32(width), uint32(height))
}

func (c *client) CloseStdin(ctx context.Context, containerID, processID string) error {
	p, err := c.getProcess(ctx, containerID, processID)
	if err != nil {
		return err
	}

	return p.CloseIO(ctx, containerd.WithStdinCloser)
}

func (c *client) Pause(ctx context.Context, containerID string) error {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return err
	}

	return wrapError(p.(containerd.Task).Pause(ctx))
}

func (c *client) Resume(ctx context.Context, containerID string) error {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return err
	}

	return p.(containerd.Task).Resume(ctx)
}

func (c *client) Stats(ctx context.Context, containerID string) (*libcontainerdtypes.Stats, error) {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return nil, err
	}

	m, err := p.(containerd.Task).Metrics(ctx)
	if err != nil {
		return nil, err
	}

	v, err := typeurl.UnmarshalAny(m.Data)
	if err != nil {
		return nil, err
	}
	return libcontainerdtypes.InterfaceToStats(m.Timestamp, v), nil
}

func (c *client) ListPids(ctx context.Context, containerID string) ([]uint32, error) {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return nil, err
	}

	pis, err := p.(containerd.Task).Pids(ctx)
	if err != nil {
		return nil, err
	}

	var pids []uint32
	for _, i := range pis {
		pids = append(pids, i.Pid)
	}

	return pids, nil
}

func (c *client) Summary(ctx context.Context, containerID string) ([]libcontainerdtypes.Summary, error) {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return nil, err
	}

	pis, err := p.(containerd.Task).Pids(ctx)
	if err != nil {
		return nil, err
	}

	var infos []libcontainerdtypes.Summary
	for _, pi := range pis {
		i, err := typeurl.UnmarshalAny(pi.Info)
		if err != nil {
			return nil, errors.Wrap(err, "unable to decode process details")
		}
		s, err := summaryFromInterface(i)
		if err != nil {
			return nil, err
		}
		infos = append(infos, *s)
	}

	return infos, nil
}

type restoredProcess struct {
	p containerd.Process
}

func (p *restoredProcess) Delete(ctx context.Context) (uint32, time.Time, error) {
	if p.p == nil {
		return 255, time.Now(), nil
	}
	status, err := p.p.Delete(ctx)
	if err != nil {
		return 255, time.Now(), nil
	}
	return status.ExitCode(), status.ExitTime(), nil
}

func (c *client) DeleteTask(ctx context.Context, containerID string) (uint32, time.Time, error) {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return 255, time.Now(), nil
	}

	status, err := p.Delete(ctx)
	if err != nil {
		return 255, time.Now(), nil
	}
	return status.ExitCode(), status.ExitTime(), nil
}

func (c *client) Delete(ctx context.Context, containerID string) error {
	ctr, err := c.getContainer(ctx, containerID)
	if err != nil {
		return err
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return err
	}
	bundle := labels[DockerContainerBundlePath]
	if err := ctr.Delete(ctx); err != nil {
		return wrapError(err)
	}
	c.oomMu.Lock()
	delete(c.oom, containerID)
	c.oomMu.Unlock()
	if os.Getenv("LIBCONTAINERD_NOCLEAN") != "1" {
		if err := os.RemoveAll(bundle); err != nil {
			c.logger.WithError(err).WithFields(logrus.Fields{
				"container": containerID,
				"bundle":    bundle,
			}).Error("failed to remove state dir")
		}
	}
	return nil
}

func (c *client) Status(ctx context.Context, containerID string) (containerd.ProcessStatus, error) {
	t, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return containerd.Unknown, err
	}
	s, err := t.Status(ctx)
	if err != nil {
		return containerd.Unknown, wrapError(err)
	}
	return s.Status, nil
}

func (c *client) CreateCheckpoint(ctx context.Context, containerID, checkpointDir string, exit bool) error {
	p, err := c.getProcess(ctx, containerID, libcontainerdtypes.InitProcessName)
	if err != nil {
		return err
	}

	opts := []containerd.CheckpointTaskOpts{}
	if exit {
		opts = append(opts, func(r *containerd.CheckpointTaskInfo) error {
			if r.Options == nil {
				r.Options = &runctypes.CheckpointOptions{
					Exit: true,
				}
			} else {
				opts, _ := r.Options.(*runctypes.CheckpointOptions)
				opts.Exit = true
			}
			return nil
		})
	}
	img, err := p.(containerd.Task).Checkpoint(ctx, opts...)
	if err != nil {
		return wrapError(err)
	}
	// Whatever happens, delete the checkpoint from containerd
	defer func() {
		err := c.client.ImageService().Delete(context.Background(), img.Name())
		if err != nil {
			c.logger.WithError(err).WithField("digest", img.Target().Digest).
				Warnf("failed to delete checkpoint image")
		}
	}()

	b, err := content.ReadBlob(ctx, c.client.ContentStore(), img.Target())
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to retrieve checkpoint data"))
	}
	var index v1.Index
	if err := json.Unmarshal(b, &index); err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to decode checkpoint data"))
	}

	var cpDesc *v1.Descriptor
	for _, m := range index.Manifests {
		if m.MediaType == images.MediaTypeContainerd1Checkpoint {
			cpDesc = &m
			break
		}
	}
	if cpDesc == nil {
		return errdefs.System(errors.Wrapf(err, "invalid checkpoint"))
	}

	rat, err := c.client.ContentStore().ReaderAt(ctx, *cpDesc)
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to get checkpoint reader"))
	}
	defer rat.Close()
	_, err = archive.Apply(ctx, checkpointDir, content.NewReader(rat))
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to read checkpoint reader"))
	}

	return err
}

func (c *client) getContainer(ctx context.Context, id string) (containerd.Container, error) {
	ctr, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if containerderrors.IsNotFound(err) {
			return nil, errors.WithStack(errdefs.NotFound(errors.New("no such container")))
		}
		return nil, wrapError(err)
	}
	return ctr, nil
}

func (c *client) getProcess(ctx context.Context, containerID, processID string) (containerd.Process, error) {
	ctr, err := c.getContainer(ctx, containerID)
	if err != nil {
		return nil, err
	}
	t, err := ctr.Task(ctx, nil)
	if err != nil {
		if containerderrors.IsNotFound(err) {
			return nil, errors.WithStack(errdefs.NotFound(errors.New("container is not running")))
		}
		return nil, wrapError(err)
	}
	if processID == libcontainerdtypes.InitProcessName {
		return t, nil
	}
	p, err := t.LoadProcess(ctx, processID, nil)
	if err != nil {
		if containerderrors.IsNotFound(err) {
			return nil, errors.WithStack(errdefs.NotFound(errors.New("no such exec")))
		}
		return nil, wrapError(err)
	}
	return p, nil
}

// createIO creates the io to be used by a process
// This needs to get a pointer to interface as upon closure the process may not have yet been registered
func (c *client) createIO(fifos *cio.FIFOSet, containerID, processID string, stdinCloseSync chan struct{}, attachStdio libcontainerdtypes.StdioCallback) (cio.IO, error) {
	var (
		io  *cio.DirectIO
		err error
	)
	io, err = c.newDirectIO(context.Background(), fifos)
	if err != nil {
		return nil, err
	}

	if io.Stdin != nil {
		var (
			err       error
			stdinOnce sync.Once
		)
		pipe := io.Stdin
		io.Stdin = ioutils.NewWriteCloserWrapper(pipe, func() error {
			stdinOnce.Do(func() {
				err = pipe.Close()
				// Do the rest in a new routine to avoid a deadlock if the
				// Exec/Start call failed.
				go func() {
					<-stdinCloseSync
					p, err := c.getProcess(context.Background(), containerID, processID)
					if err == nil {
						err = p.CloseIO(context.Background(), containerd.WithStdinCloser)
						if err != nil && strings.Contains(err.Error(), "transport is closing") {
							err = nil
						}
					}
				}()
			})
			return err
		})
	}

	rio, err := attachStdio(io)
	if err != nil {
		io.Cancel()
		io.Close()
	}
	return rio, err
}

func (c *client) processEvent(ctx context.Context, et libcontainerdtypes.EventType, ei libcontainerdtypes.EventInfo) {
	c.eventQ.Append(ei.ContainerID, func() {
		err := c.backend.ProcessEvent(ei.ContainerID, et, ei)
		if err != nil {
			c.logger.WithError(err).WithFields(logrus.Fields{
				"container":  ei.ContainerID,
				"event":      et,
				"event-info": ei,
			}).Error("failed to process event")
		}

		if et == libcontainerdtypes.EventExit && ei.ProcessID != ei.ContainerID {
			p, err := c.getProcess(ctx, ei.ContainerID, ei.ProcessID)
			if err != nil {

				c.logger.WithError(errors.New("no such process")).
					WithFields(logrus.Fields{
						"error":     err,
						"container": ei.ContainerID,
						"process":   ei.ProcessID,
					}).Error("exit event")
				return
			}
			_, err = p.Delete(context.Background())
			if err != nil {
				c.logger.WithError(err).WithFields(logrus.Fields{
					"container": ei.ContainerID,
					"process":   ei.ProcessID,
				}).Warn("failed to delete process")
			}

			ctr, err := c.getContainer(ctx, ei.ContainerID)
			if err != nil {
				c.logger.WithFields(logrus.Fields{
					"container": ei.ContainerID,
					"error":     err,
				}).Error("failed to find container")
			} else {
				labels, err := ctr.Labels(ctx)
				if err != nil {
					c.logger.WithFields(logrus.Fields{
						"container": ei.ContainerID,
						"error":     err,
					}).Error("failed to find container")
					return
				}
				newFIFOSet(labels[DockerContainerBundlePath], ei.ProcessID, true, false).Close()
			}
		}
	})
}

func (c *client) processEventStream(ctx context.Context, ns string) {
	var (
		err error
		ev  *events.Envelope
		et  libcontainerdtypes.EventType
		ei  libcontainerdtypes.EventInfo
	)

	// Filter on both namespace *and* topic. To create an "and" filter,
	// this must be a single, comma-separated string
	eventStream, errC := c.client.EventService().Subscribe(ctx, "namespace=="+ns+",topic~=|^/tasks/|")

	c.logger.Debug("processing event stream")

	for {
		var oomKilled bool
		select {
		case err = <-errC:
			if err != nil {
				errStatus, ok := status.FromError(err)
				if !ok || errStatus.Code() != codes.Canceled {
					c.logger.WithError(err).Error("failed to get event")
					go c.processEventStream(ctx, ns)
				} else {
					c.logger.WithError(ctx.Err()).Info("stopping event stream following graceful shutdown")
				}
			}
			return
		case ev = <-eventStream:
			if ev.Event == nil {
				c.logger.WithField("event", ev).Warn("invalid event")
				continue
			}

			v, err := typeurl.UnmarshalAny(ev.Event)
			if err != nil {
				c.logger.WithError(err).WithField("event", ev).Warn("failed to unmarshal event")
				continue
			}

			c.logger.WithField("topic", ev.Topic).Debug("event")

			switch t := v.(type) {
			case *apievents.TaskCreate:
				et = libcontainerdtypes.EventCreate
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ContainerID,
					Pid:         t.Pid,
				}
			case *apievents.TaskStart:
				et = libcontainerdtypes.EventStart
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ContainerID,
					Pid:         t.Pid,
				}
			case *apievents.TaskExit:
				et = libcontainerdtypes.EventExit
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ID,
					Pid:         t.Pid,
					ExitCode:    t.ExitStatus,
					ExitedAt:    t.ExitedAt,
				}
			case *apievents.TaskOOM:
				et = libcontainerdtypes.EventOOM
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					OOMKilled:   true,
				}
				oomKilled = true
			case *apievents.TaskExecAdded:
				et = libcontainerdtypes.EventExecAdded
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ExecID,
				}
			case *apievents.TaskExecStarted:
				et = libcontainerdtypes.EventExecStarted
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ExecID,
					Pid:         t.Pid,
				}
			case *apievents.TaskPaused:
				et = libcontainerdtypes.EventPaused
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
				}
			case *apievents.TaskResumed:
				et = libcontainerdtypes.EventResumed
				ei = libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
				}
			default:
				c.logger.WithFields(logrus.Fields{
					"topic": ev.Topic,
					"type":  reflect.TypeOf(t)},
				).Info("ignoring event")
				continue
			}

			c.oomMu.Lock()
			if oomKilled {
				c.oom[ei.ContainerID] = true
			}
			ei.OOMKilled = c.oom[ei.ContainerID]
			c.oomMu.Unlock()

			c.processEvent(ctx, et, ei)
		}
	}
}

func (c *client) writeContent(ctx context.Context, mediaType, ref string, r io.Reader) (*types.Descriptor, error) {
	writer, err := c.client.ContentStore().Writer(ctx, content.WithRef(ref))
	if err != nil {
		return nil, err
	}
	defer writer.Close()
	size, err := io.Copy(writer, r)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{
		"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339),
	}
	if err := writer.Commit(ctx, 0, "", content.WithLabels(labels)); err != nil {
		return nil, err
	}
	return &types.Descriptor{
		MediaType: mediaType,
		Digest:    writer.Digest(),
		Size_:     size,
	}, nil
}

func (c *client) bundleDir(id string) string {
	return filepath.Join(c.stateDir, id)
}

func wrapError(err error) error {
	switch {
	case err == nil:
		return nil
	case containerderrors.IsNotFound(err):
		return errdefs.NotFound(err)
	}

	msg := err.Error()
	for _, s := range []string{"container does not exist", "not found", "no such container"} {
		if strings.Contains(msg, s) {
			return errdefs.NotFound(err)
		}
	}
	return err
}

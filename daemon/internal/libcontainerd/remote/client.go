package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	apievents "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	c8devents "github.com/containerd/containerd/v2/core/events"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	"github.com/moby/moby/v2/daemon/internal/libcontainerd/queue"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	pkgerrors "github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
)

// DockerContainerBundlePath is the label key pointing to the container's bundle path
const DockerContainerBundlePath = "com.docker/engine.bundle.path"

type client struct {
	client   *containerd.Client
	stateDir string
	logger   *log.Entry
	ns       string

	backend libcontainerdtypes.Backend
	eventQ  queue.Queue
}

type container struct {
	client *client
	c8dCtr containerd.Container

	v2runcoptions *runcoptions.Options
}

type task struct {
	containerd.Task
	ctr *container
}

type process struct {
	containerd.Process
}

// NewClient creates a new libcontainerd client from a containerd client
func NewClient(ctx context.Context, cli *containerd.Client, stateDir, ns string, b libcontainerdtypes.Backend) (libcontainerdtypes.Client, error) {
	c := &client{
		client:   cli,
		stateDir: stateDir,
		logger:   log.G(ctx).WithField("module", "libcontainerd").WithField("namespace", ns),
		ns:       ns,
		backend:  b,
	}

	go c.processEventStream(ctx, ns)

	return c, nil
}

func (c *client) Version(ctx context.Context) (containerd.Version, error) {
	return c.client.Version(ctx)
}

func (c *container) newTask(t containerd.Task) *task {
	return &task{Task: t, ctr: c}
}

func (c *container) AttachTask(ctx context.Context, attachStdio libcontainerdtypes.StdioCallback) (_ libcontainerdtypes.Task, retErr error) {
	var dio *cio.DirectIO
	defer func() {
		if retErr != nil && dio != nil {
			dio.Cancel()
			_ = dio.Close()
		}
	}()

	attachIO := func(fifos *cio.FIFOSet) (cio.IO, error) {
		// dio must be assigned to the previously defined dio for the defer above
		// to handle cleanup
		var err error
		dio, err = c.client.newDirectIO(ctx, fifos)
		if err != nil {
			return nil, err
		}
		return attachStdio(dio)
	}
	t, err := c.c8dCtr.Task(ctx, attachIO)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "error getting containerd task for container")
	}
	return c.newTask(t), nil
}

func (c *client) NewContainer(ctx context.Context, id string, ociSpec *specs.Spec, shim string, runtimeOptions any, opts ...containerd.NewContainerOpts) (libcontainerdtypes.Container, error) {
	bdir := c.bundleDir(id)
	c.logger.WithField("bundle", bdir).WithField("root", ociSpec.Root.Path).Debug("bundle dir created")

	newOpts := []containerd.NewContainerOpts{
		containerd.WithSpec(ociSpec),
		containerd.WithRuntime(shim, runtimeOptions),
		WithBundle(bdir, ociSpec),
	}
	opts = append(opts, newOpts...)

	ctr, err := c.client.NewContainer(ctx, id, opts...)
	if err != nil {
		if cerrdefs.IsAlreadyExists(err) {
			return nil, pkgerrors.WithStack(errdefs.Conflict(errors.New("id already in use")))
		}
		return nil, err
	}

	created := container{
		client: c,
		c8dCtr: ctr,
	}
	if x, ok := runtimeOptions.(*runcoptions.Options); ok {
		created.v2runcoptions = x
	}
	return &created, nil
}

// NewTask creates a task for the specified containerd id
func (c *container) NewTask(ctx context.Context, checkpointDir string, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (libcontainerdtypes.Task, error) {
	ctx, span := otel.Tracer("").Start(ctx, "libcontainerd.remote.NewTask")
	defer span.End()

	var checkpoint *types.Descriptor
	if checkpointDir != "" {
		// write checkpoint to the content store
		tar := archive.Diff(ctx, "", checkpointDir)
		var err error
		checkpoint, err = c.client.writeContent(ctx, c8dimages.MediaTypeContainerd1Checkpoint, checkpointDir, tar)
		// remove the checkpoint when we're done
		defer func() {
			if checkpoint != nil {
				if err := c.client.client.ContentStore().Delete(ctx, digest.Digest(checkpoint.Digest)); err != nil {
					c.client.logger.WithError(err).WithFields(log.Fields{
						"ref":    checkpointDir,
						"digest": checkpoint.Digest,
					}).Warnf("failed to delete temporary checkpoint entry")
				}
			}
		}()
		if err := tar.Close(); err != nil {
			return nil, pkgerrors.Wrap(err, "failed to close checkpoint tar stream")
		}
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "failed to upload checkpoint to containerd")
		}
	}

	// Optimization: assume the relevant metadata has not changed in the
	// moment since the container was created. Elide redundant RPC requests
	// to refresh the metadata separately for spec and labels.
	md, err := c.c8dCtr.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "failed to retrieve metadata")
	}
	bundle := md.Labels[DockerContainerBundlePath]

	var spec specs.Spec
	if err := json.Unmarshal(md.Spec.GetValue(), &spec); err != nil {
		return nil, pkgerrors.Wrap(err, "failed to retrieve spec")
	}
	uid, gid := getSpecUser(&spec)

	taskOpts := []containerd.NewTaskOpts{
		func(_ context.Context, _ *containerd.Client, info *containerd.TaskInfo) error {
			info.Checkpoint = checkpoint
			return nil
		},
	}

	if runtime.GOOS != "windows" {
		taskOpts = append(taskOpts, func(_ context.Context, _ *containerd.Client, info *containerd.TaskInfo) error {
			if c.v2runcoptions != nil {
				opts := proto.Clone(c.v2runcoptions).(*runcoptions.Options)
				opts.IoUid = uint32(uid)
				opts.IoGid = uint32(gid)
				info.Options = opts
			}
			return nil
		})
	} else {
		taskOpts = append(taskOpts, withLogLevel(c.client.logger.Level))
	}

	var rio cio.IO
	stdinCloseSync := make(chan containerd.Process, 1)
	t, err := c.c8dCtr.NewTask(ctx,
		func(id string) (cio.IO, error) {
			fifos := newFIFOSet(bundle, id, withStdin, spec.Process.Terminal)

			rio, err = c.createIO(fifos, stdinCloseSync, attachStdio)
			return rio, err
		},
		taskOpts...,
	)
	if err != nil {
		close(stdinCloseSync)
		if rio != nil {
			rio.Cancel()
			_ = rio.Close()
		}
		return nil, pkgerrors.Wrap(err, "failed to create task for container")
	}

	// Signal c.createIO that it can call CloseIO
	stdinCloseSync <- t

	return c.newTask(t), nil
}

func (t *task) Start(ctx context.Context) error {
	ctx, span := otel.Tracer("").Start(ctx, "libcontainerd.remote.task.Start")
	defer span.End()
	return t.Task.Start(ctx)
}

// Exec creates exec process.
//
// The containerd client calls Exec to register the exec config in the shim side.
// When the client calls Start, the shim will create stdin fifo if needs. But
// for the container main process, the stdin fifo will be created in Create not
// the Start call. stdinCloseSync channel should be closed after Start exec
// process.
func (t *task) Exec(ctx context.Context, execID string, spec *specs.Process, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (_ libcontainerdtypes.Process, retErr error) {
	var (
		rio            cio.IO
		stdinCloseSync = make(chan containerd.Process, 1)
	)

	// Optimization: assume the DockerContainerBundlePath label has not been
	// updated since the container metadata was last loaded/refreshed.
	md, err := t.ctr.c8dCtr.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		return nil, err
	}

	fifos := newFIFOSet(md.Labels[DockerContainerBundlePath], execID, withStdin, spec.Terminal)

	defer func() {
		if retErr != nil && rio != nil {
			rio.Cancel()
			_ = rio.Close()
		}
	}()

	p, err := t.Task.Exec(ctx, execID, spec, func(id string) (cio.IO, error) {
		var err error
		rio, err = t.ctr.createIO(fifos, stdinCloseSync, attachStdio)
		return rio, err
	})
	if err != nil {
		close(stdinCloseSync)
		if cerrdefs.IsAlreadyExists(err) {
			return nil, pkgerrors.WithStack(errdefs.Conflict(errors.New("id already in use")))
		}
		return nil, err
	}

	// Signal c.createIO that it can call CloseIO
	//
	// the stdin of exec process will be created after p.Start in containerd
	defer func() { stdinCloseSync <- p }()

	if err := p.Start(ctx); err != nil {
		// don't cancel cleanup if the context is cancelled, but add a timeout
		// to make sure we are not waiting forever if containerd is unresponsive
		// or to work around fifo cancelling issues in older containerd-shim.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		if _, err := p.Delete(ctx); err != nil && !cerrdefs.IsNotFound(err) {
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": t.ID(),
				"execID":    execID,
			}).Warn("Failed to delete exec process after failing to start")
		}
		return nil, err
	}
	return process{p}, nil
}

func (t *task) Kill(ctx context.Context, signal syscall.Signal) error {
	return t.Task.Kill(ctx, signal)
}

func (p process) Kill(ctx context.Context, signal syscall.Signal) error {
	return p.Process.Kill(ctx, signal)
}

func (t *task) Pause(ctx context.Context) error {
	return t.Task.Pause(ctx)
}

func (t *task) Resume(ctx context.Context) error {
	return t.Task.Resume(ctx)
}

func (t *task) Stats(ctx context.Context) (*libcontainerdtypes.Stats, error) {
	m, err := t.Metrics(ctx)
	if err != nil {
		return nil, err
	}

	v, err := typeurl.UnmarshalAny(m.Data)
	if err != nil {
		return nil, err
	}
	return libcontainerdtypes.InterfaceToStats(protobuf.FromTimestamp(m.Timestamp), v), nil
}

func (t *task) Summary(ctx context.Context) ([]libcontainerdtypes.Summary, error) {
	pis, err := t.Pids(ctx)
	if err != nil {
		return nil, err
	}

	var infos []libcontainerdtypes.Summary
	for _, pi := range pis {
		i, err := typeurl.UnmarshalAny(pi.Info)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "unable to decode process details")
		}
		s, err := summaryFromInterface(i)
		if err != nil {
			return nil, err
		}
		infos = append(infos, *s)
	}

	return infos, nil
}

func (t *task) Delete(ctx context.Context) (*containerd.ExitStatus, error) {
	s, err := t.Task.Delete(ctx)
	return s, err
}

func (p process) Delete(ctx context.Context) (*containerd.ExitStatus, error) {
	s, err := p.Process.Delete(ctx)
	return s, err
}

func (c *container) Delete(ctx context.Context) error {
	// Optimization: assume the DockerContainerBundlePath label has not been
	// updated since the container metadata was last loaded/refreshed.
	md, err := c.c8dCtr.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		return err
	}
	bundle := md.Labels[DockerContainerBundlePath]
	if err := c.c8dCtr.Delete(ctx); err != nil {
		return err
	}
	if os.Getenv("LIBCONTAINERD_NOCLEAN") != "1" {
		if err := os.RemoveAll(bundle); err != nil {
			c.client.logger.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"container": c.c8dCtr.ID(),
				"bundle":    bundle,
			}).Error("failed to remove state dir")
		}
	}
	return nil
}

func (t *task) ForceDelete(ctx context.Context) error {
	_, err := t.Task.Delete(ctx, containerd.WithProcessKill)
	return err
}

func (t *task) Status(ctx context.Context) (containerd.Status, error) {
	s, err := t.Task.Status(ctx)
	return s, err
}

func (p process) Status(ctx context.Context) (containerd.Status, error) {
	s, err := p.Process.Status(ctx)
	return s, err
}

func (c *container) getCheckpointOptions(exit bool) containerd.CheckpointTaskOpts {
	return func(r *containerd.CheckpointTaskInfo) error {
		if r.Options == nil && c.v2runcoptions != nil {
			r.Options = &runcoptions.CheckpointOptions{}
		}

		switch opts := r.Options.(type) {
		case *runcoptions.CheckpointOptions:
			opts.Exit = exit
		}

		return nil
	}
}

func (t *task) CreateCheckpoint(ctx context.Context, checkpointDir string, exit bool) error {
	img, err := t.Task.Checkpoint(ctx, t.ctr.getCheckpointOptions(exit))
	if err != nil {
		return err
	}
	// Whatever happens, delete the checkpoint from containerd
	defer func() {
		err := t.ctr.client.client.ImageService().Delete(ctx, img.Name())
		if err != nil {
			t.ctr.client.logger.WithError(err).WithField("digest", img.Target().Digest).
				Warnf("failed to delete checkpoint image")
		}
	}()

	b, err := content.ReadBlob(ctx, t.ctr.client.client.ContentStore(), img.Target())
	if err != nil {
		return errdefs.System(pkgerrors.Wrapf(err, "failed to retrieve checkpoint data"))
	}
	var index ocispec.Index
	if err := json.Unmarshal(b, &index); err != nil {
		return errdefs.System(pkgerrors.Wrapf(err, "failed to decode checkpoint data"))
	}

	var cpDesc *ocispec.Descriptor
	for _, m := range index.Manifests {
		if m.MediaType == c8dimages.MediaTypeContainerd1Checkpoint {
			cpDesc = &m
			break
		}
	}
	if cpDesc == nil {
		return errdefs.System(pkgerrors.Wrapf(err, "invalid checkpoint"))
	}

	rat, err := t.ctr.client.client.ContentStore().ReaderAt(ctx, *cpDesc)
	if err != nil {
		return errdefs.System(pkgerrors.Wrapf(err, "failed to get checkpoint reader"))
	}
	defer rat.Close()
	_, err = archive.Apply(ctx, checkpointDir, content.NewReader(rat))
	if err != nil {
		return errdefs.System(pkgerrors.Wrapf(err, "failed to read checkpoint reader"))
	}

	return err
}

// LoadContainer loads the containerd container.
func (c *client) LoadContainer(ctx context.Context, id string) (libcontainerdtypes.Container, error) {
	ctr, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, pkgerrors.WithStack(errdefs.NotFound(errors.New("no such container")))
		}
		return nil, err
	}
	return &container{client: c, c8dCtr: ctr}, nil
}

func (c *container) Task(ctx context.Context) (libcontainerdtypes.Task, error) {
	t, err := c.c8dCtr.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	return c.newTask(t), nil
}

// createIO creates the io to be used by a process
// This needs to get a pointer to interface as upon closure the process may not have yet been registered
func (c *container) createIO(fifos *cio.FIFOSet, stdinCloseSync chan containerd.Process, attachStdio libcontainerdtypes.StdioCallback) (cio.IO, error) {
	dio, err := c.client.newDirectIO(context.Background(), fifos)
	if err != nil {
		return nil, err
	}

	if dio.Stdin != nil {
		var (
			errs      []error
			stdinOnce sync.Once
		)
		pipe := dio.Stdin
		dio.Stdin = ioutils.NewWriteCloserWrapper(pipe, func() error {
			stdinOnce.Do(func() {
				errs = append(errs, pipe.Close())

				select {
				case p, ok := <-stdinCloseSync:
					if !ok {
						return
					}
					errs = append(errs, closeStdin(context.Background(), p))
				default:
					// The process wasn't ready. Close its stdin asynchronously.
					go func() {
						p, ok := <-stdinCloseSync
						if !ok {
							return
						}
						if err := closeStdin(context.Background(), p); err != nil {
							c.client.logger.WithError(err).
								WithField("container", c.c8dCtr.ID()).
								Error("failed to close container stdin")
						}
					}()
				}
			})
			return errors.Join(errs...)
		})
	}

	rio, err := attachStdio(dio)
	if err != nil {
		dio.Cancel()
		_ = dio.Close()
	}
	return rio, err
}

func closeStdin(ctx context.Context, p containerd.Process) error {
	err := p.CloseIO(ctx, containerd.WithStdinCloser)
	if err != nil && strings.Contains(err.Error(), "transport is closing") {
		err = nil
	}
	return err
}

func (c *client) processEvent(ctx context.Context, et libcontainerdtypes.EventType, ei libcontainerdtypes.EventInfo) {
	c.eventQ.Append(ei.ContainerID, func() {
		err := c.backend.ProcessEvent(ei.ContainerID, et, ei)
		if err != nil {
			c.logger.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"container":  ei.ContainerID,
				"event":      et,
				"event-info": ei,
			}).Error("failed to process event")
		}
	})
}

func (c *client) waitServe(ctx context.Context) bool {
	t := 100 * time.Millisecond
	delay := time.NewTimer(t)
	if !delay.Stop() {
		<-delay.C
	}
	defer delay.Stop()

	// `IsServing` will actually block until the service is ready.
	// However it can return early, so we'll loop with a delay to handle it.
	for {
		serving, err := c.client.IsServing(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return false
			}
			log.G(ctx).WithError(err).Warn("Error while testing if containerd API is ready")
		}

		if serving {
			return true
		}

		delay.Reset(t)
		select {
		case <-ctx.Done():
			return false
		case <-delay.C:
		}
	}
}

func (c *client) processEventStream(ctx context.Context, ns string) {
	c.processEventStreamWithRestart(ctx, ns, eventStreamRestartDelay, func(ctx context.Context, filter string) (<-chan *c8devents.Envelope, <-chan error) {
		return c.client.EventService().Subscribe(ctx, filter)
	}, c.waitServe)
}

const eventStreamRestartDelay = 100 * time.Millisecond

type eventStreamSubscribeFunc func(context.Context, string) (<-chan *c8devents.Envelope, <-chan error)
type eventStreamReadyFunc func(context.Context) bool

func (c *client) processEventStreamWithRestart(ctx context.Context, ns string, delay time.Duration, subscribe eventStreamSubscribeFunc, waitReady eventStreamReadyFunc) {
	for c.processEventStreamOnce(ctx, ns, subscribe, waitReady) {
		if !waitEventStreamRestart(ctx, delay) {
			return
		}
	}
}

func waitEventStreamRestart(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *client) processEventStreamOnce(ctx context.Context, ns string, subscribe eventStreamSubscribeFunc, waitReady eventStreamReadyFunc) bool {
	// Create a new context specifically for this subscription.
	// The context must be cancelled to cancel the subscription.
	// In cases where we have to restart event stream processing,
	//   we'll need the original context b/c this one will be cancelled
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Filter on both namespace *and* topic. To create an "and" filter,
	// this must be a single, comma-separated string
	eventStream, errC := subscribe(subCtx, "namespace=="+ns+",topic~=|^/tasks/|")

	c.logger.Debug("processing event stream")

	for {
		select {
		case err := <-errC:
			if errors.Is(err, context.Canceled) {
				c.logger.WithError(err).Info("stopping event stream following graceful shutdown")
				return false
			}
			if ctx.Err() != nil {
				c.logger.WithError(ctx.Err()).Info("stopping event stream following graceful shutdown")
				return false
			}
			if err != nil {
				c.logger.WithError(err).Error("Failed to get event")
				c.logger.Info("Waiting for containerd to be ready to restart event processing")
				if waitReady(ctx) {
					return true
				}
			}
			return false
		case ev := <-eventStream:
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
				c.processEvent(ctx, libcontainerdtypes.EventCreate, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ContainerID,
					Pid:         t.Pid,
				})
			case *apievents.TaskStart:
				c.processEvent(ctx, libcontainerdtypes.EventStart, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ContainerID,
					Pid:         t.Pid,
				})
			case *apievents.TaskExit:
				c.processEvent(ctx, libcontainerdtypes.EventExit, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ID,
					Pid:         t.Pid,
					ExitCode:    t.ExitStatus,
					ExitedAt:    protobuf.FromTimestamp(t.ExitedAt),
				})
			case *apievents.TaskOOM:
				c.processEvent(ctx, libcontainerdtypes.EventOOM, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
				})
			case *apievents.TaskExecAdded:
				c.processEvent(ctx, libcontainerdtypes.EventExecAdded, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ExecID,
				})
			case *apievents.TaskExecStarted:
				c.processEvent(ctx, libcontainerdtypes.EventExecStarted, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
					ProcessID:   t.ExecID,
					Pid:         t.Pid,
				})
			case *apievents.TaskPaused:
				c.processEvent(ctx, libcontainerdtypes.EventPaused, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
				})
			case *apievents.TaskResumed:
				c.processEvent(ctx, libcontainerdtypes.EventResumed, libcontainerdtypes.EventInfo{
					ContainerID: t.ContainerID,
				})
			case *apievents.TaskDelete:
				c.logger.WithFields(log.Fields{
					"topic":     ev.Topic,
					"type":      reflect.TypeFor[*apievents.TaskDelete](),
					"container": t.ContainerID,
				}).Info("received task-delete event from containerd")
			default:
				c.logger.WithFields(log.Fields{
					"topic": ev.Topic,
					"type":  reflect.TypeOf(t),
				}).Info("ignoring event")
			}
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
		Digest:    writer.Digest().String(),
		Size:      size,
	}, nil
}

func (c *client) bundleDir(id string) string {
	return filepath.Join(c.stateDir, id)
}

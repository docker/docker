package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/extensions"
	servicev0 "github.com/moby/extensions/extpoints/service/v0"
	"github.com/moby/moby/client"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
	daemonapiv0 "github.com/moby/moby/v2/extpoints/daemonapi/v0"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	storagekvv0 "github.com/moby/moby/v2/extpoints/storage/kv/v0"
)

// ExtensionID identifies the builtin jobs extension.
const ExtensionID = "org.mobyproject.jobs.v0"

// storageNamespace partitions this extension's records from other cooperating
// extensions in the storage point; v0 namespaces are not an authorization
// boundary. Drop the constant once the framework derives namespaces from
// host-resolved extension identity.
const storageNamespace = ExtensionID

// Extension is the builtin jobs extension. It provides the Jobs point and
// offers it for publication on the daemon socket. Run containers are driven
// through the daemon API point and their exits observed through the container
// state point; the storage point persists jobs and runs, and the container
// name-generator point names unnamed job registrations.
//
// The extension host initializes during daemon construction, before the
// daemon can serve container operations, so activation waits for the daemon
// API in the background. API calls arriving before activation completes are
// answered with Unavailable.
type Extension struct {
	mu               sync.Mutex
	api              *client.Client
	manager          *Manager
	cancelActivation context.CancelFunc
	activation       sync.WaitGroup
	// closed is set by shutdown; an activation finishing later stops its
	// manager instead of publishing it.
	closed bool
	// calls tracks the API calls using manager, so shutdown can drain them
	// before stopping it.
	calls sync.WaitGroup
	// stopCalls is cancelled by shutdown to release calls blocked in the
	// manager, such as Wait.
	stopCalls       context.Context
	cancelStopCalls context.CancelFunc
}

// NewExtension builds the jobs extension.
func NewExtension() *Extension {
	stopCalls, cancel := context.WithCancel(context.Background())
	return &Extension{stopCalls: stopCalls, cancelStopCalls: cancel}
}

// Declaration describes the extension to the host.
func (e *Extension) Declaration() extensions.Declaration {
	return extensions.Declaration{
		ID: ExtensionID,
		Providers: []extensions.Provider{
			jobsv0.Point.Provide(&service{ext: e}),
			servicev0.Offer(jobsv0.Point),
		},
		Dependencies: []extensions.Dependency{
			daemonapiv0.Point.Dependency(),
			containerstatev0.Point.Dependency(),
			storagekvv0.Point.Dependency(),
			containernamegeneratorv0.Point.Dependency(),
		},
		Init:     e.init,
		Shutdown: e.shutdown,
	}
}

// init resolves the dependencies, builds the manager (a storage backend
// that cannot list or read records fails daemon startup loudly rather than
// surfacing later as a mysteriously empty API, while individually damaged
// records are quarantined) and finishes activation in the background:
// container operations only work once the daemon serves its API, which
// happens after the host (and this init) runs.
func (e *Extension) init(ctx context.Context, _ extensions.Config, resolver extensions.Resolver) error {
	apiPoint, err := daemonapiv0.Point.Single(resolver)
	if err != nil {
		return fmt.Errorf("resolving the daemon API point: %w", err)
	}
	state, err := containerstatev0.Point.Single(resolver)
	if err != nil {
		return fmt.Errorf("resolving the container state point: %w", err)
	}
	kv, err := storagekvv0.GetKV(resolver, storageNamespace)
	if err != nil {
		return fmt.Errorf("resolving the storage point: %w", err)
	}
	generator, err := containernamegeneratorv0.Point.Single(resolver)
	if err != nil {
		return fmt.Errorf("resolving the container name-generator point: %w", err)
	}
	api, err := daemonapiv0.NewClient(apiPoint)
	if err != nil {
		return fmt.Errorf("creating the daemon API client: %w", err)
	}
	manager, err := e.buildManager(ctx, kv, &engineBackend{api: api, state: state}, nameGenerator(generator))
	if err != nil {
		_ = api.Close()
		return err
	}
	// The manager outlives the init call, so its lifetime cannot be tied to
	// the startup context's cancellation; shutdown cancels the wait
	// instead, releasing the goroutine if the daemon never serves its API.
	activateCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.mu.Lock()
	e.api = api
	e.cancelActivation = cancel
	e.mu.Unlock()
	e.activation.Go(func() {
		if err := waitForAPI(activateCtx, api); err != nil {
			// Cancelled means shutdown released the wait: an orderly stop,
			// not a failure worth alarming about.
			if activateCtx.Err() == nil {
				log.G(activateCtx).WithError(err).Error("jobs: the daemon API never became ready; the Jobs API stays unavailable")
			}
			return
		}
		if err := e.activate(activateCtx, manager); err != nil && activateCtx.Err() == nil {
			log.G(activateCtx).WithError(err).Error("jobs: activation failed; the Jobs API stays unavailable")
		}
	})
	return nil
}

// buildManager loads the store from the extension's storage namespace and
// builds the manager driving backend. The store is pure record I/O and needs
// no running container backend.
func (e *Extension) buildManager(ctx context.Context, kv KV, backend Backend, generateName NameGenerator) (*Manager, error) {
	store, err := NewStore(ctx, kv)
	if err != nil {
		return nil, err
	}
	return NewManager(store, backend, generateName), nil
}

// nameGenerator names unnamed job registrations through the container
// name-generator point, the same source the daemon draws container names
// from; fitting, as job names become components of run container names.
// Generated names are validated like any other job name on registration.
func nameGenerator(generator containernamegeneratorv0.ContainerNameGenerator) NameGenerator {
	return func(ctx context.Context, retry int64) (string, error) {
		// ContainerID and Image stay empty: a job has no container at
		// registration time.
		reply, err := generator.GenerateContainerName(ctx, &containernamegeneratorv0.GenerateContainerNameRequest{Retry: retry})
		if err != nil {
			return "", err
		}
		return reply.Name, nil
	}
}

// activate reconciles the manager's runs, starts the scheduler, and opens
// the API for business. It runs exactly once, when the daemon is ready; a
// second activation is a programming error and is rejected rather than
// silently leaking the previous manager. Reconciliation can block on
// stopping containers, so it runs without the mutex: API calls landing
// meanwhile are answered with Unavailable, and shutdown is not held past its
// deadline.
func (e *Extension) activate(ctx context.Context, manager *Manager) error {
	e.mu.Lock()
	err := e.checkActivatable()
	e.mu.Unlock()

	if err != nil {
		return err
	}
	// Order matters: reconciliation resolves runs orphaned by an unclean
	// stop from the containers' actual state, so triggers re-arm from a
	// truthful picture.
	manager.Restore(ctx)
	manager.Start(ctx)

	e.mu.Lock()
	if err = e.checkActivatable(); err == nil {
		e.manager = manager
	}
	e.mu.Unlock()

	if err != nil {
		// Nothing else can reach the unpublished manager to stop it.
		return errors.Join(err, manager.Shutdown(context.WithoutCancel(ctx)))
	}

	return nil
}

// checkActivatable reports why a manager cannot be published. Called with
// the mutex held.
func (e *Extension) checkActivatable() error {
	if e.closed {
		return errors.New("the jobs extension is shut down")
	}

	if e.manager != nil {
		return errors.New("the jobs extension is already active")
	}

	return nil
}

// shutdown releases a pending activation, stops admitting API calls and
// drains those in flight, then stops the active manager and closes the
// daemon API client once nothing uses them anymore.
func (e *Extension) shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true

	if e.cancelActivation != nil {
		e.cancelActivation()
	}
	e.mu.Unlock()

	// Reconciliation may be stuck stopping a container that ignores its
	// stop signal.
	if err := waitFor(ctx, &e.activation); err != nil {
		// The activation still uses the API client; leave it to the
		// exiting daemon.
		return err
	}

	e.mu.Lock()
	manager, api := e.manager, e.api
	e.manager, e.api = nil, nil
	e.mu.Unlock()

	e.cancelStopCalls()

	if err := waitFor(ctx, &e.calls); err != nil {
		// Calls still use the manager and the API client; leave both to
		// the exiting daemon.
		return err
	}

	var err error
	if manager != nil {
		err = manager.Shutdown(ctx)
	}
	if api != nil {
		err = errors.Join(err, api.Close())
	}
	return err
}

// waitFor waits for wg, or until ctx is done.
func waitFor(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

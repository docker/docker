package agent

import (
	"bytes"
	"context"
	"math/rand"
	"reflect"
	"sync"
	"time"

	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
	"github.com/pkg/errors"
)

const (
	initialSessionFailureBackoff = 100 * time.Millisecond
	maxSessionFailureBackoff     = 8 * time.Second
	nodeUpdatePeriod             = 20 * time.Second
)

// Agent implements the primary node functionality for a member of a swarm
// cluster. The primary functionality is to run and report on the status of
// tasks assigned to the node.
type Agent struct {
	config *Config

	// The latest node object state from manager
	// for this node known to the agent.
	node *api.Node

	keys []*api.EncryptionKey

	sessionq chan sessionOperation
	worker   Worker

	started   chan struct{}
	startOnce sync.Once // start only once
	ready     chan struct{}
	leaving   chan struct{}
	leaveOnce sync.Once
	left      chan struct{} // closed after "run" processes "leaving" and will no longer accept new assignments
	stopped   chan struct{} // requests shutdown
	stopOnce  sync.Once     // only allow stop to be called once
	closed    chan struct{} // only closed in run
	err       error         // read only after closed is closed

	nodeUpdatePeriod time.Duration
}

// New returns a new agent, ready for task dispatch.
func New(config *Config) (*Agent, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}

	a := &Agent{
		config:           config,
		sessionq:         make(chan sessionOperation),
		started:          make(chan struct{}),
		leaving:          make(chan struct{}),
		left:             make(chan struct{}),
		stopped:          make(chan struct{}),
		closed:           make(chan struct{}),
		ready:            make(chan struct{}),
		nodeUpdatePeriod: nodeUpdatePeriod,
	}

	a.worker = newWorker(config.DB, config.Executor, a)
	return a, nil
}

// Start begins execution of the agent in the provided context, if not already
// started.
//
// Start returns an error if the agent has already started.
func (a *Agent) Start(ctx context.Context) error {
	err := errAgentStarted

	a.startOnce.Do(func() {
		close(a.started)
		go a.run(ctx)
		err = nil // clear error above, only once.
	})

	return err
}

// Leave instructs the agent to leave the cluster. This method will shutdown
// assignment processing and remove all assignments from the node.
// Leave blocks until worker has finished closing all task managers or agent
// is closed.
func (a *Agent) Leave(ctx context.Context) error {
	select {
	case <-a.started:
	default:
		return errAgentNotStarted
	}

	a.leaveOnce.Do(func() {
		close(a.leaving)
	})

	// Do not call Wait until we have confirmed that the agent is no longer
	// accepting assignments. Starting a worker might race with Wait.
	select {
	case <-a.left:
	case <-a.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}

	// agent could be closed while Leave is in progress
	var err error
	ch := make(chan struct{})
	go func() {
		err = a.worker.Wait(ctx)
		close(ch)
	}()

	select {
	case <-ch:
		return err
	case <-a.closed:
		return ErrClosed
	}
}

// Stop shuts down the agent, blocking until full shutdown. If the agent is not
// started, Stop will block until the agent has fully shutdown.
func (a *Agent) Stop(ctx context.Context) error {
	select {
	case <-a.started:
	default:
		return errAgentNotStarted
	}

	a.stop()

	// wait till closed or context cancelled
	select {
	case <-a.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stop signals the agent shutdown process, returning true if this call was the
// first to actually shutdown the agent.
func (a *Agent) stop() bool {
	var stopped bool
	a.stopOnce.Do(func() {
		close(a.stopped)
		stopped = true
	})

	return stopped
}

// Err returns the error that caused the agent to shutdown or nil. Err blocks
// until the agent is fully shutdown.
func (a *Agent) Err(ctx context.Context) error {
	select {
	case <-a.closed:
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ready returns a channel that will be closed when agent first becomes ready.
func (a *Agent) Ready() <-chan struct{} {
	return a.ready
}

func (a *Agent) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer close(a.closed) // full shutdown.

	ctx = log.WithModule(ctx, "agent")

	log.G(ctx).Debug("(*Agent).run")
	defer log.G(ctx).Debug("(*Agent).run exited")

	nodeTLSInfo := a.config.NodeTLSInfo

	// get the node description
	nodeDescription, err := a.nodeDescriptionWithHostname(ctx, nodeTLSInfo)
	if err != nil {
		log.G(ctx).WithError(err).WithField("agent", a.config.Executor).Error("agent: node description unavailable")
	}
	// nodeUpdateTicker is used to periodically check for updates to node description
	nodeUpdateTicker := time.NewTicker(a.nodeUpdatePeriod)
	defer nodeUpdateTicker.Stop()

	var (
		backoff       time.Duration
		session       = newSession(ctx, a, backoff, "", nodeDescription) // start the initial session
		registered    = session.registered
		ready         = a.ready // first session ready
		sessionq      chan sessionOperation
		leaving       = a.leaving
		subscriptions = map[string]context.CancelFunc{}
		// subscriptionDone is a channel that allows us to notify ourselves
		// that a lot subscription should be finished. this channel is
		// unbuffered, because it is only written to in a goroutine, and
		// therefore cannot block the main execution path.
		subscriptionDone = make(chan string)
	)
	defer func() {
		session.close()
	}()

	if err := a.worker.Init(ctx); err != nil {
		log.G(ctx).WithError(err).Error("worker initialization failed")
		a.err = err
		return // fatal?
	}
	defer a.worker.Close()

	// setup a reliable reporter to call back to us.
	reporter := newStatusReporter(ctx, a)
	defer reporter.Close()

	a.worker.Listen(ctx, reporter)

	updateNode := func() {
		// skip updating if the registration isn't finished
		if registered != nil {
			return
		}
		// get the current node description
		newNodeDescription, err := a.nodeDescriptionWithHostname(ctx, nodeTLSInfo)
		if err != nil {
			log.G(ctx).WithError(err).WithField("agent", a.config.Executor).Error("agent: updated node description unavailable")
		}

		// if newNodeDescription is nil, it will cause a panic when
		// trying to create a session. Typically this can happen
		// if the engine goes down
		if newNodeDescription == nil {
			return
		}

		// if the node description has changed, update it to the new one
		// and close the session. The old session will be stopped and a
		// new one will be created with the updated description
		if !reflect.DeepEqual(nodeDescription, newNodeDescription) {
			nodeDescription = newNodeDescription
			// close the session
			log.G(ctx).Info("agent: found node update")

			if err := session.close(); err != nil {
				log.G(ctx).WithError(err).Error("agent: closing session failed")
			}
			sessionq = nil
			registered = nil
		}
	}

	for {
		select {
		case operation := <-sessionq:
			operation.response <- operation.fn(session)
		case <-leaving:
			leaving = nil

			// TODO(stevvooe): Signal to the manager that the node is leaving.

			// when leaving we remove all assignments.
			if err := a.worker.Assign(ctx, nil); err != nil {
				log.G(ctx).WithError(err).Error("failed removing all assignments")
			}

			close(a.left)
		case msg := <-session.assignments:
			// if we have left, accept no more assignments
			if leaving == nil {
				continue
			}

			switch msg.Type {
			case api.AssignmentsMessage_COMPLETE:
				// Need to assign secrets and configs before tasks,
				// because tasks might depend on new secrets or configs
				if err := a.worker.Assign(ctx, msg.Changes); err != nil {
					log.G(ctx).WithError(err).Error("failed to synchronize worker assignments")
				}
			case api.AssignmentsMessage_INCREMENTAL:
				if err := a.worker.Update(ctx, msg.Changes); err != nil {
					log.G(ctx).WithError(err).Error("failed to update worker assignments")
				}
			}
		case msg := <-session.messages:
			if err := a.handleSessionMessage(ctx, msg, nodeTLSInfo); err != nil {
				log.G(ctx).WithError(err).Error("session message handler failed")
			}
		case sub := <-session.subscriptions:
			if sub.Close {
				if cancel, ok := subscriptions[sub.ID]; ok {
					cancel()
				}
				delete(subscriptions, sub.ID)
				continue
			}

			if _, ok := subscriptions[sub.ID]; ok {
				// Duplicate subscription
				continue
			}

			subCtx, subCancel := context.WithCancel(ctx)
			subscriptions[sub.ID] = subCancel
			// NOTE(dperny): for like 3 years, there has been a to do saying
			// "we're tossing the error here, that seems wrong". this is not a
			// to do anymore. 9/10 of these errors are going to be "context
			// deadline exceeded", and the remaining 1/10 obviously doesn't
			// matter or we'd have missed it by now.
			go func() {
				a.worker.Subscribe(subCtx, sub)
				// when the worker finishes the subscription, we should notify
				// ourselves that this has occurred. We cannot rely on getting
				// a Close message from the manager, as any number of things
				// could go wrong (see github.com/moby/moby/issues/39916).
				subscriptionDone <- sub.ID
			}()
		case subID := <-subscriptionDone:
			// subscription may already have been removed. If so, no need to
			// take any action.
			if cancel, ok := subscriptions[subID]; ok {
				cancel()
				delete(subscriptions, subID)
			}
		case <-registered:
			log.G(ctx).Debugln("agent: registered")
			if ready != nil {
				close(ready)
			}
			if a.config.SessionTracker != nil {
				a.config.SessionTracker.SessionEstablished()
			}
			ready = nil
			registered = nil // we only care about this once per session
			backoff = 0      // reset backoff
			sessionq = a.sessionq
			// re-report all task statuses when re-establishing a session
			go a.worker.Report(ctx, reporter)
		case err := <-session.errs:
			// TODO(stevvooe): This may actually block if a session is closed
			// but no error was sent. This must be the only place
			// session.close is called in response to errors, for this to work.
			if err != nil {
				if a.config.SessionTracker != nil {
					a.config.SessionTracker.SessionError(err)
				}

				backoff = initialSessionFailureBackoff + 2*backoff
				if backoff > maxSessionFailureBackoff {
					backoff = maxSessionFailureBackoff
				}
				log.G(ctx).WithError(err).WithField("backoff", backoff).Errorf("agent: session failed")
			}

			if err := session.close(); err != nil {
				log.G(ctx).WithError(err).Error("agent: closing session failed")
			}
			sessionq = nil
			// if we're here before <-registered, do nothing for that event
			registered = nil
		case <-session.closed:
			if a.config.SessionTracker != nil {
				if err := a.config.SessionTracker.SessionClosed(); err != nil {
					log.G(ctx).WithError(err).Error("agent: exiting")
					a.err = err
					return
				}
			}

			log.G(ctx).Debugf("agent: rebuild session")

			// select a session registration delay from backoff range.
			delay := time.Duration(0)
			if backoff > 0 {
				delay = time.Duration(rand.Int63n(int64(backoff)))
			}
			session = newSession(ctx, a, delay, session.sessionID, nodeDescription)
			registered = session.registered
		case ev := <-a.config.NotifyTLSChange:
			// the TLS info has changed, so force a check to see if we need to restart the session
			if tlsInfo, ok := ev.(*api.NodeTLSInfo); ok {
				nodeTLSInfo = tlsInfo
				updateNode()
				nodeUpdateTicker.Stop()
				nodeUpdateTicker = time.NewTicker(a.nodeUpdatePeriod)
			}
		case <-nodeUpdateTicker.C:
			// periodically check to see whether the node information has changed, and if so, restart the session
			updateNode()
		case <-a.stopped:
			// TODO(stevvooe): Wait on shutdown and cleanup. May need to pump
			// this loop a few times.
			return
		case <-ctx.Done():
			if a.err == nil {
				a.err = ctx.Err()
			}
			return
		}
	}
}

func (a *Agent) handleSessionMessage(ctx context.Context, message *api.SessionMessage, nti *api.NodeTLSInfo) error {
	seen := map[api.Peer]struct{}{}
	for _, manager := range message.Managers {
		if manager.Peer.Addr == "" {
			continue
		}

		a.config.ConnBroker.Remotes().Observe(*manager.Peer, int(manager.Weight))
		seen[*manager.Peer] = struct{}{}
	}

	var changes *NodeChanges
	if message.Node != nil && (a.node == nil || !nodesEqual(a.node, message.Node)) {
		if a.config.NotifyNodeChange != nil {
			changes = &NodeChanges{Node: message.Node.Copy()}
		}
		a.node = message.Node.Copy()
		if err := a.config.Executor.Configure(ctx, a.node); err != nil {
			log.G(ctx).WithError(err).Error("node configure failed")
		}
	}
	if len(message.RootCA) > 0 && !bytes.Equal(message.RootCA, nti.TrustRoot) {
		if changes == nil {
			changes = &NodeChanges{RootCert: message.RootCA}
		} else {
			changes.RootCert = message.RootCA
		}
	}

	if changes != nil {
		a.config.NotifyNodeChange <- changes
	}

	// prune managers not in list.
	for peer := range a.config.ConnBroker.Remotes().Weights() {
		if _, ok := seen[peer]; !ok {
			a.config.ConnBroker.Remotes().Remove(peer)
		}
	}

	if message.NetworkBootstrapKeys == nil {
		return nil
	}

	for _, key := range message.NetworkBootstrapKeys {
		same := false
		for _, agentKey := range a.keys {
			if agentKey.LamportTime == key.LamportTime {
				same = true
			}
		}
		if !same {
			a.keys = message.NetworkBootstrapKeys
			if err := a.config.Executor.SetNetworkBootstrapKeys(a.keys); err != nil {
				return errors.Wrap(err, "configuring network key failed")
			}
		}
	}

	if err := a.config.Executor.SetCSINodePlugins(message.CSINodePlugins); err != nil {
		return errors.Wrap(err, "configuring CSI node plugins failed")
	}

	return nil
}

type sessionOperation struct {
	fn       func(session *session) error
	response chan error
}

// withSession runs fn with the current session.
func (a *Agent) withSession(ctx context.Context, fn func(session *session) error) error {
	response := make(chan error, 1)
	select {
	case a.sessionq <- sessionOperation{
		fn:       fn,
		response: response,
	}:
		select {
		case err := <-response:
			return err
		case <-a.closed:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-a.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UpdateTaskStatus attempts to send a task status update over the current session,
// blocking until the operation is completed.
//
// If an error is returned, the operation should be retried.
func (a *Agent) UpdateTaskStatus(ctx context.Context, taskID string, status *api.TaskStatus) error {
	log.G(ctx).WithField("task.id", taskID).Debug("(*Agent).UpdateTaskStatus")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 1)
	if err := a.withSession(ctx, func(session *session) error {
		go func() {
			err := session.sendTaskStatus(ctx, taskID, status)
			if err != nil {
				if err == errTaskUnknown {
					err = nil // dispatcher no longer cares about this task.
				} else {
					log.G(ctx).WithError(err).Error("closing session after fatal error")
					session.sendError(err)
				}
			} else {
				log.G(ctx).Debug("task status reported")
			}

			errs <- err
		}()

		return nil
	}); err != nil {
		return err
	}

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReportVolumeUnpublished sends a Volume status update to the manager
// indicating that the provided volume has been successfully unpublished.
func (a *Agent) ReportVolumeUnpublished(ctx context.Context, volumeID string) error {
	l := log.G(ctx).WithField("volume.ID", volumeID)
	l.Debug("(*Agent).ReportVolumeUnpublished")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 1)
	if err := a.withSession(ctx, func(session *session) error {
		go func() {
			err := session.reportVolumeUnpublished(ctx, []string{volumeID})
			if err != nil {
				l.WithError(err).Error("error reporting volume unpublished")
			} else {
				l.Debug("reported volume unpublished")
			}

			errs <- err
		}()

		return nil
	}); err != nil {
		return err
	}

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Publisher returns a LogPublisher for the given subscription
// as well as a cancel function that should be called when the log stream
// is completed.
func (a *Agent) Publisher(ctx context.Context, subscriptionID string) (exec.LogPublisher, func(), error) {
	// TODO(stevvooe): The level of coordination here is WAY too much for logs.
	// These should only be best effort and really just buffer until a session is
	// ready. Ideally, they would use a separate connection completely.

	var (
		err       error
		publisher api.LogBroker_PublishLogsClient
	)

	err = a.withSession(ctx, func(session *session) error {
		publisher, err = api.NewLogBrokerClient(session.conn.ClientConn).PublishLogs(ctx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	// make little closure for ending the log stream
	sendCloseMsg := func() {
		// send a close message, to tell the manager our logs are done
		publisher.Send(&api.PublishLogsMessage{
			SubscriptionID: subscriptionID,
			Close:          true,
		})
		// close the stream forreal. ignore the return value and the error,
		// because we don't care.
		publisher.CloseAndRecv()
	}

	return exec.LogPublisherFunc(func(ctx context.Context, message api.LogMessage) error {
			select {
			case <-ctx.Done():
				sendCloseMsg()
				return ctx.Err()
			default:
			}

			return publisher.Send(&api.PublishLogsMessage{
				SubscriptionID: subscriptionID,
				Messages:       []api.LogMessage{message},
			})
		}), func() {
			sendCloseMsg()
		}, nil
}

// nodeDescriptionWithHostname retrieves node description, and overrides hostname if available
func (a *Agent) nodeDescriptionWithHostname(ctx context.Context, tlsInfo *api.NodeTLSInfo) (*api.NodeDescription, error) {
	desc, err := a.config.Executor.Describe(ctx)

	if desc != nil {
		// Override hostname and TLS info
		if a.config.Hostname != "" {
			desc.Hostname = a.config.Hostname
		}
		desc.TLSInfo = tlsInfo
		desc.FIPS = a.config.FIPS
	}
	return desc, err
}

// nodesEqual returns true if the node states are functionally equal, ignoring status,
// version and other superfluous fields.
//
// This used to decide whether or not to propagate a node update to executor.
func nodesEqual(a, b *api.Node) bool {
	a, b = a.Copy(), b.Copy()

	a.Status, b.Status = api.NodeStatus{}, api.NodeStatus{}
	a.Meta, b.Meta = api.Meta{}, api.Meta{}

	return reflect.DeepEqual(a, b)
}

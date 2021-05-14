package agent

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/connectionbroker"
	"github.com/docker/swarmkit/log"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	dispatcherRPCTimeout = 5 * time.Second
	errSessionClosed     = errors.New("agent: session closed")
)

// session encapsulates one round of registration with the manager. session
// starts the registration and heartbeat control cycle. Any failure will result
// in a complete shutdown of the session and it must be reestablished.
//
// All communication with the master is done through session.  Changes that
// flow into the agent, such as task assignment, are called back into the
// agent through errs, messages and tasks.
type session struct {
	conn *connectionbroker.Conn

	agent         *Agent
	sessionID     string
	session       api.Dispatcher_SessionClient
	errs          chan error
	messages      chan *api.SessionMessage
	assignments   chan *api.AssignmentsMessage
	subscriptions chan *api.SubscriptionMessage

	cancel     func()        // this is assumed to be never nil, and set whenever a session is created
	registered chan struct{} // closed registration
	closed     chan struct{}
	closeOnce  sync.Once
}

func newSession(ctx context.Context, agent *Agent, delay time.Duration, sessionID string, description *api.NodeDescription) *session {
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	s := &session{
		agent:         agent,
		sessionID:     sessionID,
		errs:          make(chan error, 1),
		messages:      make(chan *api.SessionMessage),
		assignments:   make(chan *api.AssignmentsMessage),
		subscriptions: make(chan *api.SubscriptionMessage),
		registered:    make(chan struct{}),
		closed:        make(chan struct{}),
		cancel:        sessionCancel,
	}

	// TODO(stevvooe): Need to move connection management up a level or create
	// independent connection for log broker client.

	cc, err := agent.config.ConnBroker.Select(
		grpc.WithTransportCredentials(agent.config.Credentials),
		grpc.WithTimeout(dispatcherRPCTimeout),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)),
	)

	if err != nil {
		// since we are returning without launching the session goroutine, we
		// need to provide the delay that is guaranteed by calling this
		// function. We launch a goroutine so that we only delay the retry and
		// avoid blocking the main loop.
		go func() {
			time.Sleep(delay)
			s.errs <- err
		}()
		return s
	}

	log.G(ctx).Infof("manager selected by agent for new session: %v", cc.Peer())

	s.conn = cc

	go s.run(sessionCtx, delay, description)
	return s
}

func (s *session) run(ctx context.Context, delay time.Duration, description *api.NodeDescription) {
	timer := time.NewTimer(delay) // delay before registering.
	log.G(ctx).Infof("waiting %v before registering session", delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}

	if err := s.start(ctx, description); err != nil {
		select {
		case s.errs <- err:
		case <-s.closed:
		case <-ctx.Done():
		}
		return
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("session.id", s.sessionID))

	go runctx(ctx, s.closed, s.errs, s.heartbeat)
	go runctx(ctx, s.closed, s.errs, s.watch)
	go runctx(ctx, s.closed, s.errs, s.listen)
	go runctx(ctx, s.closed, s.errs, s.logSubscriptions)

	close(s.registered)
}

// start begins the session and returns the first SessionMessage.
func (s *session) start(ctx context.Context, description *api.NodeDescription) error {
	log.G(ctx).Debugf("(*session).start")

	errChan := make(chan error, 1)
	var (
		msg    *api.SessionMessage
		stream api.Dispatcher_SessionClient
		err    error
	)
	// Note: we don't defer cancellation of this context, because the
	// streaming RPC is used after this function returned. We only cancel
	// it in the timeout case to make sure the goroutine completes.

	// We also fork this context again from the `run` context, because on
	// `dispatcherRPCTimeout`, we want to cancel establishing a session and
	// return an error.  If we cancel the `run` context instead of forking,
	// then in `run` it's possible that we just terminate the function because
	// `ctx` is done and hence fail to propagate the timeout error to the agent.
	// If the error is not propogated to the agent, the agent will not close
	// the session or rebuild a new session.
	sessionCtx, cancelSession := context.WithCancel(ctx) //nolint:govet

	// Need to run Session in a goroutine since there's no way to set a
	// timeout for an individual Recv call in a stream.
	go func() {
		client := api.NewDispatcherClient(s.conn.ClientConn)

		stream, err = client.Session(sessionCtx, &api.SessionRequest{
			Description: description,
			SessionID:   s.sessionID,
		})
		if err != nil {
			errChan <- err
			return
		}

		msg, err = stream.Recv()
		errChan <- err
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return err //nolint:govet
		}
	case <-time.After(dispatcherRPCTimeout):
		cancelSession()
		return errors.New("session initiation timed out")
	}

	s.sessionID = msg.SessionID
	s.session = stream

	return s.handleSessionMessage(ctx, msg)
}

func (s *session) heartbeat(ctx context.Context) error {
	log.G(ctx).Debugf("(*session).heartbeat")
	client := api.NewDispatcherClient(s.conn.ClientConn)
	heartbeat := time.NewTimer(1) // send out a heartbeat right away
	defer heartbeat.Stop()

	fields := logrus.Fields{
		"sessionID": s.sessionID,
		"method":    "(*session).heartbeat",
	}

	for {
		select {
		case <-heartbeat.C:
			heartbeatCtx, cancel := context.WithTimeout(ctx, dispatcherRPCTimeout)
			// TODO(anshul) log manager info in all logs in this function.
			log.G(ctx).WithFields(fields).Debugf("sending heartbeat to manager %v with timeout %v", s.conn.Peer(), dispatcherRPCTimeout)
			resp, err := client.Heartbeat(heartbeatCtx, &api.HeartbeatRequest{
				SessionID: s.sessionID,
			})
			cancel()
			if err != nil {
				log.G(ctx).WithFields(fields).WithError(err).Errorf("heartbeat to manager %v failed", s.conn.Peer())
				st, _ := status.FromError(err)
				if st.Code() == codes.NotFound {
					err = errNodeNotRegistered
				}

				return err
			}

			log.G(ctx).WithFields(fields).Debugf("heartbeat successful to manager %v, next heartbeat period: %v", s.conn.Peer(), resp.Period)

			heartbeat.Reset(resp.Period)
		case <-s.closed:
			return errSessionClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *session) listen(ctx context.Context) error {
	defer s.session.CloseSend()
	log.G(ctx).Debugf("(*session).listen")
	for {
		msg, err := s.session.Recv()
		if err != nil {
			return err
		}

		if err := s.handleSessionMessage(ctx, msg); err != nil {
			return err
		}
	}
}

func (s *session) handleSessionMessage(ctx context.Context, msg *api.SessionMessage) error {
	select {
	case s.messages <- msg:
		return nil
	case <-s.closed:
		return errSessionClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) logSubscriptions(ctx context.Context) error {
	log := log.G(ctx).WithFields(logrus.Fields{"method": "(*session).logSubscriptions"})
	log.Debugf("")

	client := api.NewLogBrokerClient(s.conn.ClientConn)
	subscriptions, err := client.ListenSubscriptions(ctx, &api.ListenSubscriptionsRequest{})
	if err != nil {
		return err
	}
	defer subscriptions.CloseSend()

	for {
		resp, err := subscriptions.Recv()
		st, _ := status.FromError(err)
		if st.Code() == codes.Unimplemented {
			log.Warning("manager does not support log subscriptions")
			// Don't return, because returning would bounce the session
			select {
			case <-s.closed:
				return errSessionClosed
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}

		select {
		case s.subscriptions <- resp:
		case <-s.closed:
			return errSessionClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *session) watch(ctx context.Context) error {
	log := log.G(ctx).WithFields(logrus.Fields{"method": "(*session).watch"})
	log.Debugf("")
	var (
		resp            *api.AssignmentsMessage
		assignmentWatch api.Dispatcher_AssignmentsClient
		tasksWatch      api.Dispatcher_TasksClient
		streamReference string
		tasksFallback   bool
		err             error
	)

	client := api.NewDispatcherClient(s.conn.ClientConn)
	for {
		// If this is the first time we're running the loop, or there was a reference mismatch
		// attempt to get the assignmentWatch
		if assignmentWatch == nil && !tasksFallback {
			assignmentWatch, err = client.Assignments(ctx, &api.AssignmentsRequest{SessionID: s.sessionID})
			if err != nil {
				return err
			}
		}
		// We have an assignmentWatch, let's try to receive an AssignmentMessage
		if assignmentWatch != nil {
			// If we get a code = 12 desc = unknown method Assignments, try to use tasks
			resp, err = assignmentWatch.Recv()
			if err != nil {
				st, _ := status.FromError(err)
				if st.Code() != codes.Unimplemented {
					return err
				}
				tasksFallback = true
				assignmentWatch = nil
				log.WithError(err).Infof("falling back to Tasks")
			}
		}

		// This code is here for backwards compatibility (so that newer clients can use the
		// older method Tasks)
		if tasksWatch == nil && tasksFallback {
			tasksWatch, err = client.Tasks(ctx, &api.TasksRequest{SessionID: s.sessionID})
			if err != nil {
				return err
			}
		}
		if tasksWatch != nil {
			// When falling back to Tasks because of an old managers, we wrap the tasks in assignments.
			var taskResp *api.TasksMessage
			var assignmentChanges []*api.AssignmentChange
			taskResp, err = tasksWatch.Recv()
			if err != nil {
				return err
			}
			for _, t := range taskResp.Tasks {
				taskChange := &api.AssignmentChange{
					Assignment: &api.Assignment{
						Item: &api.Assignment_Task{
							Task: t,
						},
					},
					Action: api.AssignmentChange_AssignmentActionUpdate,
				}

				assignmentChanges = append(assignmentChanges, taskChange)
			}
			resp = &api.AssignmentsMessage{Type: api.AssignmentsMessage_COMPLETE, Changes: assignmentChanges}
		}

		// If there seems to be a gap in the stream, let's break out of the inner for and
		// re-sync (by calling Assignments again).
		if streamReference != "" && streamReference != resp.AppliesTo {
			assignmentWatch = nil
		} else {
			streamReference = resp.ResultsIn
		}

		select {
		case s.assignments <- resp:
		case <-s.closed:
			return errSessionClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendTaskStatus uses the current session to send the status of a single task.
func (s *session) sendTaskStatus(ctx context.Context, taskID string, taskStatus *api.TaskStatus) error {
	client := api.NewDispatcherClient(s.conn.ClientConn)
	if _, err := client.UpdateTaskStatus(ctx, &api.UpdateTaskStatusRequest{
		SessionID: s.sessionID,
		Updates: []*api.UpdateTaskStatusRequest_TaskStatusUpdate{
			{
				TaskID: taskID,
				Status: taskStatus,
			},
		},
	}); err != nil {
		// TODO(stevvooe): Dispatcher should not return this error. Status
		// reports for unknown tasks should be ignored.
		st, _ := status.FromError(err)
		if st.Code() == codes.NotFound {
			return errTaskUnknown
		}

		return err
	}

	return nil
}

func (s *session) sendTaskStatuses(ctx context.Context, updates ...*api.UpdateTaskStatusRequest_TaskStatusUpdate) ([]*api.UpdateTaskStatusRequest_TaskStatusUpdate, error) {
	if len(updates) < 1 {
		return nil, nil
	}

	const batchSize = 1024
	select {
	case <-s.registered:
		select {
		case <-s.closed:
			return updates, ErrClosed
		default:
		}
	case <-s.closed:
		return updates, ErrClosed
	case <-ctx.Done():
		return updates, ctx.Err()
	}

	client := api.NewDispatcherClient(s.conn.ClientConn)
	n := batchSize

	if len(updates) < n {
		n = len(updates)
	}

	if _, err := client.UpdateTaskStatus(ctx, &api.UpdateTaskStatusRequest{
		SessionID: s.sessionID,
		Updates:   updates[:n],
	}); err != nil {
		log.G(ctx).WithError(err).Errorf("failed sending task status batch size of %d", len(updates[:n]))
		return updates, err
	}

	return updates[n:], nil
}

// reportVolumeUnpublished sends a status update to the manager reporting that
// all volumes in the slice are unpublished.
func (s *session) reportVolumeUnpublished(ctx context.Context, volumes []string) error {
	updates := []*api.UpdateVolumeStatusRequest_VolumeStatusUpdate{}
	for _, volume := range volumes {
		updates = append(updates, &api.UpdateVolumeStatusRequest_VolumeStatusUpdate{
			ID:          volume,
			Unpublished: true,
		})
	}
	client := api.NewDispatcherClient(s.conn.ClientConn)
	_, err := client.UpdateVolumeStatus(ctx, &api.UpdateVolumeStatusRequest{
		SessionID: s.sessionID,
		Updates:   updates,
	})
	return err
}

// sendError is used to send errors to errs channel and trigger session recreation
func (s *session) sendError(err error) {
	select {
	case s.errs <- err:
	case <-s.closed:
	}
}

// close the given session. It should be called only in <-session.errs branch
// of event loop, or when cleaning up the agent.
func (s *session) close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.conn != nil {
			s.conn.Close(false)
		}
		close(s.closed)
	})

	return nil
}

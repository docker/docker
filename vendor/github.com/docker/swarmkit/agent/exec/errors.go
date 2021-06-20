package exec

import "github.com/pkg/errors"

var (
	// ErrRuntimeUnsupported encountered when a task requires a runtime
	// unsupported by the executor.
	ErrRuntimeUnsupported = errors.New("exec: unsupported runtime")

	// ErrTaskPrepared is called if the task is already prepared.
	ErrTaskPrepared = errors.New("exec: task already prepared")

	// ErrTaskStarted can be returned from any operation that cannot be
	// performed because the task has already been started. This does not imply
	// that the task is running but rather that it is no longer valid to call
	// Start.
	ErrTaskStarted = errors.New("exec: task already started")

	// ErrTaskUpdateRejected is returned if a task update is rejected by a controller.
	ErrTaskUpdateRejected = errors.New("exec: task update rejected")

	// ErrControllerClosed returned when a task controller has been closed.
	ErrControllerClosed = errors.New("exec: controller closed")

	// ErrTaskRetry is returned by Do when an operation failed by should be
	// retried. The status should still be reported in this case.
	ErrTaskRetry = errors.New("exec: task retry")

	// ErrTaskNoop returns when the a subsequent call to Do will not result in
	// advancing the task. Callers should avoid calling Do until the task has been updated.
	ErrTaskNoop = errors.New("exec: task noop")

	// ErrDependencyNotReady is returned if a given dependency can be accessed
	// through the Getter, but is not yet ready to be used. This is most
	// relevant for Volumes, which must be staged and published on the node.
	ErrDependencyNotReady error = errors.New("dependency not ready")
)

// ExitCoder is implemented by errors that have an exit code.
type ExitCoder interface {
	// ExitCode returns the exit code.
	ExitCode() int
}

// Temporary indicates whether or not the error condition is temporary.
//
// If this is encountered in the controller, the failing operation will be
// retried when this returns true. Otherwise, the operation is considered
// fatal.
type Temporary interface {
	Temporary() bool
}

// MakeTemporary makes the error temporary.
func MakeTemporary(err error) error {
	if IsTemporary(err) {
		return err
	}

	return temporary{err}
}

type temporary struct {
	error
}

func (t temporary) Cause() error    { return t.error }
func (t temporary) Temporary() bool { return true }

// IsTemporary returns true if the error or a recursive cause returns true for
// temporary.
func IsTemporary(err error) bool {
	for err != nil {
		if tmp, ok := err.(Temporary); ok && tmp.Temporary() {
			return true
		}

		cause := errors.Cause(err)
		if cause == err {
			break
		}

		err = cause
	}

	return false
}

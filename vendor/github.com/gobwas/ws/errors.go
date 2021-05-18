package ws

// RejectOption represents an option used to control the way connection is
// rejected.
type RejectOption func(*rejectConnectionError)

// RejectionReason returns an option that makes connection to be rejected with
// given reason.
func RejectionReason(reason string) RejectOption {
	return func(err *rejectConnectionError) {
		err.reason = reason
	}
}

// RejectionStatus returns an option that makes connection to be rejected with
// given HTTP status code.
func RejectionStatus(code int) RejectOption {
	return func(err *rejectConnectionError) {
		err.code = code
	}
}

// RejectionHeader returns an option that makes connection to be rejected with
// given HTTP headers.
func RejectionHeader(h HandshakeHeader) RejectOption {
	return func(err *rejectConnectionError) {
		err.header = h
	}
}

// RejectConnectionError constructs an error that could be used to control the way
// handshake is rejected by Upgrader.
func RejectConnectionError(options ...RejectOption) error {
	err := new(rejectConnectionError)
	for _, opt := range options {
		opt(err)
	}
	return err
}

// rejectConnectionError represents a rejection of upgrade error.
//
// It can be returned by Upgrader's On* hooks to control the way WebSocket
// handshake is rejected.
type rejectConnectionError struct {
	reason string
	code   int
	header HandshakeHeader
}

// Error implements error interface.
func (r *rejectConnectionError) Error() string {
	return r.reason
}

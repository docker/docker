package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"sync"

	"github.com/moby/moby/client/internal/httperror"

	"github.com/moby/moby/api/types/jsonstream"
)

func NewJSONMessageStream(rc io.ReadCloser) Stream {
	if rc == nil {
		panic("nil io.ReadCloser")
	}
	return Stream{
		rc:    rc,
		close: sync.OnceValue(rc.Close),
	}
}

type Stream struct {
	rc    io.ReadCloser
	close func() error
}

// Read implements io.ReadCloser
func (r Stream) Read(p []byte) (n int, err error) {
	if r.rc == nil {
		return 0, io.EOF
	}
	return r.rc.Read(p)
}

// Close implements io.ReadCloser
func (r Stream) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

var _ io.ReadCloser = Stream{}

// JSONMessages decodes the response stream as a sequence of [jsonstream.Message].
// The underlying [io.Reader] is closed when the stream ends or if the context
// is cancelled.
func (r Stream) JSONMessages(ctx context.Context) iter.Seq2[jsonstream.Message, error] {
	stop := context.AfterFunc(ctx, func() {
		_ = r.Close()
	})
	return func(yield func(jsonstream.Message, error) bool) {
		defer func() {
			stop() // unregister AfterFunc
			_ = r.Close()
		}()

		dec := json.NewDecoder(r)
		for {
			var jm jsonstream.Message
			if err := dec.Decode(&jm); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				if err := ctx.Err(); err != nil {
					// Do not return decoding errors if the context was
					// cancelled, because the decoding errors may be due
					// to the context being cancelled.
					yield(jsonstream.Message{}, err)
					return
				}
				yield(jsonstream.Message{}, err)
				return
			}
			if !yield(jm, nil) {
				return
			}
		}
	}
}

// Wait consumes the stream until completion.
//
// It returns nil if the operation completes successfully. Errors are
// returned if the context is canceled, a decoding/transport failure
// occurs, or a JSON message reports an error ([jsonstream.Message.Error]).
func (r Stream) Wait(ctx context.Context) error {
	for jm, err := range r.JSONMessages(ctx) {
		if err != nil {
			// decode, transport and context cancellation errors.
			return err
		}
		if jm.Error != nil {
			// push/pull failures.
			return httperror.FromStatusCode(jm.Error, jm.Error.Code)
		}
	}
	return nil
}

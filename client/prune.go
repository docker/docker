package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/url"
	"sync"

	"github.com/moby/moby/api/types/prune"
)

// PruneResponse is implemented by each prune result. Streaming results must be
// consumed through Events or Wait, or closed. Events closes the request when
// iteration ends, including an early break. Wait returns the complete report.
// Concurrent consumption is unsupported; Close is safe at any time.
type PruneResponse[T any] interface {
	Events(context.Context) iter.Seq2[prune.Object, error]
	Wait(context.Context) (T, error)
	Close() error
}

type pruneStream[T any] struct {
	decoder *json.Decoder
	ctx     context.Context
	close   func() error
	mu      sync.Mutex
	json    bool
	done    bool
	report  T
	err     error
}

func startPrune[T any](cli *Client, ctx context.Context, path string, query url.Values, stream bool) (*pruneStream[T], error) {
	if stream {
		if err := cli.requiresVersion(ctx, "1.57", "prune progress"); err != nil {
			return nil, err
		}
		query.Set("stream", "true")
	}
	ctx, cancel := context.WithCancel(ctx)
	resp, err := cli.post(ctx, path, query, nil, nil)
	if err != nil {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	s := &pruneStream[T]{decoder: json.NewDecoder(resp.Body), ctx: ctx, json: !stream}
	s.close = sync.OnceValue(func() error { cancel(); return resp.Body.Close() })
	if stream {
		mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil {
			_ = s.close()
			return nil, fmt.Errorf("invalid prune response content type: %w", err)
		}
		switch mediaType {
		case "application/json":
			s.json = true // Decode fallback without repeating the destructive request.
		case "application/jsonl":
		default:
			_ = s.close()
			return nil, errors.New("daemon did not return a prune progress stream")
		}
	}
	return s, nil
}

func (s *pruneStream[T]) events(ctx context.Context) iter.Seq2[prune.Object, error] {
	return func(yield func(prune.Object, error) bool) {
		if !s.mu.TryLock() {
			yield(prune.Object{}, errors.New("prune response is already being consumed"))
			return
		}
		defer s.mu.Unlock()
		if s.done {
			return
		}
		if s.err != nil {
			yield(prune.Object{}, s.err)
			return
		}
		stop := context.AfterFunc(ctx, func() { _ = s.close() })
		defer stop()
		defer s.close()
		for {
			if err := ctx.Err(); err != nil {
				s.err = err
				yield(prune.Object{}, err)
				return
			}
			if err := s.ctx.Err(); err != nil {
				s.err = err
				yield(prune.Object{}, err)
				return
			}
			var object *prune.Object
			if s.json {
				s.err = s.decoder.Decode(&s.report)
				s.done = s.err == nil
			} else {
				var event prune.Event[T]
				s.err = s.decoder.Decode(&event)
				if s.err == nil {
					count := 0
					if event.Object != nil {
						count++
					}
					if event.Report != nil {
						count++
					}
					if event.Error != nil {
						count++
					}
					if count != 1 {
						s.err = errors.New("invalid prune event: expected one object, report, or error")
					} else if event.Error != nil {
						s.err = fmt.Errorf("prune failed: %s", event.Error.Message)
					} else if event.Report != nil {
						s.report = *event.Report
						s.done = true
					} else {
						object = event.Object
						if object.ID == "" || (object.Action != prune.Deleted && object.Action != prune.Untagged) {
							s.err = errors.New("invalid prune object")
						}
					}
				}
			}
			if s.err != nil {
				if ctx.Err() != nil {
					s.err = ctx.Err()
				} else if s.ctx.Err() != nil {
					s.err = s.ctx.Err()
				} else if errors.Is(s.err, io.EOF) {
					s.err = io.ErrUnexpectedEOF
				}
				yield(prune.Object{}, s.err)
				return
			}
			if s.done {
				return
			}
			if !yield(*object, nil) {
				return
			}
		}
	}
}

func (s *pruneStream[T]) wait(ctx context.Context) (T, error) {
	for _, err := range s.events(ctx) {
		if err != nil {
			var zero T
			return zero, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		var zero T
		return zero, io.ErrUnexpectedEOF
	}
	return s.report, nil
}

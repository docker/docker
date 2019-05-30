package flightcontrol

import (
	"context"
	"io"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/moby/buildkit/util/progress"
	"github.com/pkg/errors"
)

// flightcontrol is like singleflight but with support for cancellation and
// nested progress reporting

var (
	errRetry        = errors.Errorf("retry")
	errRetryTimeout = errors.Errorf("exceeded retry timeout")
)

type contextKeyT string

var contextKey = contextKeyT("buildkit/util/flightcontrol.progress")

// Group is a flightcontrol synchronization group
type Group struct {
	mu sync.Mutex       // protects m
	m  map[string]*call // lazily initialized
}

// Do executes a context function syncronized by the key
func (g *Group) Do(ctx context.Context, key string, fn func(ctx context.Context) (interface{}, error)) (v interface{}, err error) {
	var backoff time.Duration
	for {
		v, err = g.do(ctx, key, fn)
		if err == nil || errors.Cause(err) != errRetry {
			return v, err
		}
		// backoff logic
		if backoff >= 3*time.Second {
			err = errors.Wrapf(errRetryTimeout, "flightcontrol")
			return v, err
		}
		runtime.Gosched()
		if backoff > 0 {
			time.Sleep(backoff)
			backoff *= 2
		} else {
			backoff = time.Millisecond
		}
	}
}

func (g *Group) do(ctx context.Context, key string, fn func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}

	if c, ok := g.m[key]; ok { // register 2nd waiter
		g.mu.Unlock()
		return c.wait(ctx)
	}

	c := newCall(fn)
	g.m[key] = c
	go func() {
		// cleanup after a caller has returned
		<-c.ready
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
	}()
	g.mu.Unlock()
	return c.wait(ctx)
}

type call struct {
	mu     sync.Mutex
	result interface{}
	err    error
	ready  chan struct{}

	ctx  *sharedContext
	ctxs []context.Context
	fn   func(ctx context.Context) (interface{}, error)
	once sync.Once

	closeProgressWriter func()
	progressState       *progressState
	progressCtx         context.Context
}

func newCall(fn func(ctx context.Context) (interface{}, error)) *call {
	c := &call{
		fn:            fn,
		ready:         make(chan struct{}),
		progressState: newProgressState(),
	}
	ctx := newContext(c) // newSharedContext
	pr, pctx, closeProgressWriter := progress.NewContext(context.Background())

	c.progressCtx = pctx
	c.ctx = ctx
	c.closeProgressWriter = closeProgressWriter

	go c.progressState.run(pr) // TODO: remove this, wrap writer instead

	return c
}

func (c *call) run() {
	defer c.closeProgressWriter()
	v, err := c.fn(c.ctx)
	c.mu.Lock()
	c.result = v
	c.err = err
	c.mu.Unlock()
	close(c.ready)
}

func (c *call) wait(ctx context.Context) (v interface{}, err error) {
	c.mu.Lock()
	// detect case where caller has just returned, let it clean up before
	select {
	case <-c.ready: // could return if no error
		c.mu.Unlock()
		return nil, errRetry
	default:
	}

	pw, ok, ctx := progress.FromContext(ctx)
	if ok {
		c.progressState.add(pw)
	}
	c.ctxs = append(c.ctxs, ctx)

	c.mu.Unlock()

	go c.once.Do(c.run)

	select {
	case <-ctx.Done():
		select {
		case <-c.ctx.Done():
			// if this cancelled the last context, then wait for function to shut down
			// and don't accept any more callers
			<-c.ready
			return c.result, c.err
		default:
			if ok {
				c.progressState.close(pw)
			}
			return nil, ctx.Err()
		}
	case <-c.ready:
		return c.result, c.err // shared not implemented yet
	}
}

func (c *call) Deadline() (deadline time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ctx := range c.ctxs {
		select {
		case <-ctx.Done():
		default:
			dl, ok := ctx.Deadline()
			if ok {
				return dl, ok
			}
		}
	}
	return time.Time{}, false
}

func (c *call) Done() <-chan struct{} {
	c.mu.Lock()
	c.ctx.signal()
	c.mu.Unlock()
	return c.ctx.done
}

func (c *call) Err() error {
	select {
	case <-c.ctx.Done():
		return c.ctx.err
	default:
		return nil
	}
}

func (c *call) Value(key interface{}) interface{} {
	if key == contextKey {
		return c.progressState
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx := c.progressCtx
	select {
	case <-ctx.Done():
	default:
		if v := ctx.Value(key); v != nil {
			return v
		}
	}

	if len(c.ctxs) > 0 {
		ctx = c.ctxs[0]
		select {
		case <-ctx.Done():
		default:
			if v := ctx.Value(key); v != nil {
				return v
			}
		}
	}

	return nil
}

type sharedContext struct {
	*call
	done chan struct{}
	err  error
}

func newContext(c *call) *sharedContext {
	return &sharedContext{call: c, done: make(chan struct{})}
}

// call with lock
func (c *sharedContext) signal() {
	select {
	case <-c.done:
	default:
		var err error
		for _, ctx := range c.ctxs {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			default:
				return
			}
		}
		c.err = err
		close(c.done)
	}
}

type rawProgressWriter interface {
	WriteRawProgress(*progress.Progress) error
	Close() error
}

type progressState struct {
	mu      sync.Mutex
	items   map[string]*progress.Progress
	writers []rawProgressWriter
	done    bool
}

func newProgressState() *progressState {
	return &progressState{
		items: make(map[string]*progress.Progress),
	}
}

func (ps *progressState) run(pr progress.Reader) {
	for {
		p, err := pr.Read(context.TODO())
		if err != nil {
			if err == io.EOF {
				ps.mu.Lock()
				ps.done = true
				ps.mu.Unlock()
				for _, w := range ps.writers {
					w.Close()
				}
			}
			return
		}
		ps.mu.Lock()
		for _, p := range p {
			for _, w := range ps.writers {
				w.WriteRawProgress(p)
			}
			ps.items[p.ID] = p
		}
		ps.mu.Unlock()
	}
}

func (ps *progressState) add(pw progress.Writer) {
	rw, ok := pw.(rawProgressWriter)
	if !ok {
		return
	}
	ps.mu.Lock()
	plist := make([]*progress.Progress, 0, len(ps.items))
	for _, p := range ps.items {
		plist = append(plist, p)
	}
	sort.Slice(plist, func(i, j int) bool {
		return plist[i].Timestamp.Before(plist[j].Timestamp)
	})
	for _, p := range plist {
		rw.WriteRawProgress(p)
	}
	if ps.done {
		rw.Close()
	} else {
		ps.writers = append(ps.writers, rw)
	}
	ps.mu.Unlock()
}

func (ps *progressState) close(pw progress.Writer) {
	rw, ok := pw.(rawProgressWriter)
	if !ok {
		return
	}
	ps.mu.Lock()
	for i, w := range ps.writers {
		if w == rw {
			w.Close()
			ps.writers = append(ps.writers[:i], ps.writers[i+1:]...)
			break
		}
	}
	ps.mu.Unlock()
}

package fsutil

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

type ReceiveOpt struct {
	NotifyHashed  ChangeFunc
	ContentHasher ContentHasher
	ProgressCb    func(int, bool)
	Merge         bool
	Filter        FilterFunc
}

func Receive(ctx context.Context, conn Stream, dest string, opt ReceiveOpt) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &receiver{
		conn:          &syncStream{Stream: conn},
		dest:          dest,
		files:         make(map[string]uint32),
		pipes:         make(map[uint32]io.WriteCloser),
		notifyHashed:  opt.NotifyHashed,
		contentHasher: opt.ContentHasher,
		progressCb:    opt.ProgressCb,
		merge:         opt.Merge,
		filter:        opt.Filter,
	}
	return r.run(ctx)
}

type receiver struct {
	dest       string
	conn       Stream
	files      map[string]uint32
	pipes      map[uint32]io.WriteCloser
	mu         sync.RWMutex
	muPipes    sync.RWMutex
	progressCb func(int, bool)
	merge      bool
	filter     FilterFunc

	notifyHashed   ChangeFunc
	contentHasher  ContentHasher
	orderValidator Validator
	hlValidator    Hardlinks
}

type dynamicWalker struct {
	walkChan chan *currentPath
	err      error
	closeCh  chan struct{}
}

func newDynamicWalker() *dynamicWalker {
	return &dynamicWalker{
		walkChan: make(chan *currentPath, 128),
		closeCh:  make(chan struct{}),
	}
}

func (w *dynamicWalker) update(p *currentPath) error {
	select {
	case <-w.closeCh:
		return errors.Wrap(w.err, "walker is closed")
	default:
	}
	if p == nil {
		close(w.walkChan)
		return nil
	}
	select {
	case w.walkChan <- p:
		return nil
	case <-w.closeCh:
		return errors.Wrap(w.err, "walker is closed")
	}
}

func (w *dynamicWalker) fill(ctx context.Context, pathC chan<- *currentPath) error {
	for {
		select {
		case p, ok := <-w.walkChan:
			if !ok {
				return nil
			}
			select {
			case pathC <- p:
			case <-ctx.Done():
				w.err = ctx.Err()
				close(w.closeCh)
				return ctx.Err()
			}
		case <-ctx.Done():
			w.err = ctx.Err()
			close(w.closeCh)
			return ctx.Err()
		}
	}
	return nil
}

func (r *receiver) run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	dw, err := NewDiskWriter(ctx, r.dest, DiskWriterOpt{
		AsyncDataCb:   r.asyncDataFunc,
		NotifyCb:      r.notifyHashed,
		ContentHasher: r.contentHasher,
		Filter:        r.filter,
	})
	if err != nil {
		return err
	}

	w := newDynamicWalker()

	g.Go(func() (retErr error) {
		defer func() {
			if retErr != nil {
				r.conn.SendMsg(&types.Packet{Type: types.PACKET_ERR, Data: []byte(retErr.Error())})
			}
		}()
		destWalker := emptyWalker
		if !r.merge {
			destWalker = GetWalkerFn(r.dest)
		}
		err := doubleWalkDiff(ctx, dw.HandleChange, destWalker, w.fill)
		if err != nil {
			return err
		}
		if err := dw.Wait(ctx); err != nil {
			return err
		}
		r.conn.SendMsg(&types.Packet{Type: types.PACKET_FIN})
		return nil
	})

	g.Go(func() error {
		var i uint32 = 0

		size := 0
		if r.progressCb != nil {
			defer func() {
				r.progressCb(size, true)
			}()
		}
		var p types.Packet
		for {
			p = types.Packet{Data: p.Data[:0]}
			if err := r.conn.RecvMsg(&p); err != nil {
				return err
			}
			if r.progressCb != nil {
				size += p.Size()
				r.progressCb(size, false)
			}

			switch p.Type {
			case types.PACKET_ERR:
				return errors.Errorf("error from sender: %s", p.Data)
			case types.PACKET_STAT:
				if p.Stat == nil {
					if err := w.update(nil); err != nil {
						return err
					}
					break
				}
				if fileCanRequestData(os.FileMode(p.Stat.Mode)) {
					r.mu.Lock()
					r.files[p.Stat.Path] = i
					r.mu.Unlock()
				}
				i++
				cp := &currentPath{path: p.Stat.Path, f: &StatInfo{p.Stat}}
				if err := r.orderValidator.HandleChange(ChangeKindAdd, cp.path, cp.f, nil); err != nil {
					return err
				}
				if err := r.hlValidator.HandleChange(ChangeKindAdd, cp.path, cp.f, nil); err != nil {
					return err
				}
				if err := w.update(cp); err != nil {
					return err
				}
			case types.PACKET_DATA:
				r.muPipes.Lock()
				pw, ok := r.pipes[p.ID]
				r.muPipes.Unlock()
				if !ok {
					return errors.Errorf("invalid file request %d", p.ID)
				}
				if len(p.Data) == 0 {
					if err := pw.Close(); err != nil {
						return err
					}
				} else {
					if _, err := pw.Write(p.Data); err != nil {
						return err
					}
				}
			case types.PACKET_FIN:
				for {
					var p types.Packet
					if err := r.conn.RecvMsg(&p); err != nil {
						if err == io.EOF {
							return nil
						}
						return err
					}
				}
			}
		}
	})
	return g.Wait()
}

func (r *receiver) asyncDataFunc(ctx context.Context, p string, wc io.WriteCloser) error {
	r.mu.Lock()
	id, ok := r.files[p]
	if !ok {
		r.mu.Unlock()
		return errors.Errorf("invalid file request %s", p)
	}
	delete(r.files, p)
	r.mu.Unlock()

	wwc := newWrappedWriteCloser(wc)
	r.muPipes.Lock()
	r.pipes[id] = wwc
	r.muPipes.Unlock()
	if err := r.conn.SendMsg(&types.Packet{Type: types.PACKET_REQ, ID: id}); err != nil {
		return err
	}
	err := wwc.Wait(ctx)
	if err != nil {
		return err
	}
	r.muPipes.Lock()
	delete(r.pipes, id)
	r.muPipes.Unlock()
	return nil
}

type wrappedWriteCloser struct {
	io.WriteCloser
	err  error
	once sync.Once
	done chan struct{}
}

func newWrappedWriteCloser(wc io.WriteCloser) *wrappedWriteCloser {
	return &wrappedWriteCloser{WriteCloser: wc, done: make(chan struct{})}
}

func (w *wrappedWriteCloser) Close() error {
	w.err = w.WriteCloser.Close()
	w.once.Do(func() { close(w.done) })
	return w.err
}

func (w *wrappedWriteCloser) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.err
	}
}

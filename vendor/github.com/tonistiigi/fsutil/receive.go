// send.go and receive.go describe the fsutil file-transfer protocol, which
// allows transferring file trees across a network connection.
//
// The protocol operates as follows:
// - The client (the receiver) connects to the server (the sender).
// - The sender walks the target tree lexicographically and sends a series of
//   STAT packets that describe each file (an empty stat indicates EOF).
// - The receiver sends a REQ packet for each file it requires the contents for,
//   using the ID for the file (determined as its index in the STAT sequence).
// - The sender sends a DATA packet with byte arrays for the contents of the
//   file, associated with an ID (an empty array indicates EOF).
// - Once the receiver has received all files it wants, it sends a FIN packet,
//   and the file transfer is complete.
// If an error is encountered on either side, an ERR packet is sent containing
// a human-readable error.
//
// All paths transferred over the protocol are normalized to unix-style paths,
// regardless of which platforms are present on either side. These path
// conversions are performed right before sending a STAT packet (for the
// sender) or right after receiving the corresponding STAT packet (for the
// receiver); this abstraction doesn't leak into the rest of fsutil, which
// operates on native platform-specific paths.
//
// Note that in the case of cross-platform file transfers, the transfer is
// best-effort. Some filenames that are valid on a unix sender would not be
// valid on a windows receiver, so these paths are rejected as they are
// received. Additionally, file metadata, like user/group owners and xattrs do
// not have an exact correspondence on windows, and so would be discarded by
// a windows receiver.

package fsutil

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

type DiffType int

const (
	DiffMetadata DiffType = iota
	DiffNone
	DiffContent
)

const metadataPath = ".fsutil-metadata"

type ReceiveOpt struct {
	NotifyHashed  ChangeFunc
	ContentHasher ContentHasher
	ProgressCb    func(int, bool)
	Merge         bool
	Filter        FilterFunc
	Differ        DiffType
	MetadataOnly  FilterFunc
}

type receiveDiskWriter interface {
	HandleChange(ChangeKind, string, os.FileInfo, error) error
	Wait(context.Context) error
}

func Receive(ctx context.Context, conn Stream, dest string, opt ReceiveOpt) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r := newReceiver(conn, opt)
	r.dest = dest
	return r.run(ctx)
}

func ReceiveRoot(ctx context.Context, conn Stream, dest Root, opt ReceiveOpt) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r := newReceiver(conn, opt)
	r.root = dest
	return r.run(ctx)
}

func newReceiver(conn Stream, opt ReceiveOpt) *receiver {
	return &receiver{
		conn:          &syncStream{Stream: conn},
		files:         make(map[string]uint32),
		pipes:         make(map[uint32]io.WriteCloser),
		notifyHashed:  opt.NotifyHashed,
		contentHasher: opt.ContentHasher,
		progressCb:    opt.ProgressCb,
		merge:         opt.Merge,
		filter:        opt.Filter,
		differ:        opt.Differ,
		metadataOnly:  opt.MetadataOnly,
	}
}

type receiver struct {
	dest         string
	root         Root
	conn         Stream
	files        map[string]uint32
	pipes        map[uint32]io.WriteCloser
	mu           sync.RWMutex
	muPipes      sync.RWMutex
	progressCb   func(int, bool)
	merge        bool
	filter       FilterFunc
	differ       DiffType
	metadataOnly FilterFunc

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
}

func (r *receiver) run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	dw, err := r.newDiskWriter(ctx, DiskWriterOpt{
		AsyncDataCb:   r.asyncDataFunc,
		NotifyCb:      r.notifyHashed,
		ContentHasher: r.contentHasher,
		Filter:        r.filter,
	})
	if err != nil {
		return err
	}

	w := newDynamicWalker()
	metadataTransfer := r.metadataOnly != nil
	// buffer Stat metadata in framed proto
	metadataBuffer := &buffer{}
	// stack of parent paths that can be replayed if metadata filter matches
	metadataParents := newStack[*currentPath]()

	g.Go(func() (retErr error) {
		defer func() {
			if retErr != nil {
				// If we're unwinding because the errgroup context was
				// cancelled by another goroutine's failure, report that root
				// cause instead of the bare "context canceled", which would
				// otherwise overwrite the real error on the sender side.
				if errors.Is(retErr, context.Canceled) {
					if cause := context.Cause(ctx); cause != nil {
						retErr = cause
					}
				}
				r.conn.SendMsg(&types.Packet{Type: types.PACKET_ERR, Data: []byte(retErr.Error())})
			}
		}()
		destWalker := emptyWalker
		if !r.merge {
			destWalker = r.destWalker()
		}
		err := doubleWalkDiff(ctx, dw.HandleChange, destWalker, w.fill, r.filter, r.differ)
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
			p.ResetVT()
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

				// normalize unix wire-specific paths to platform-specific paths
				path := filepath.FromSlash(p.Stat.Path)
				if filepath.ToSlash(path) != p.Stat.Path {
					// e.g. a linux path foo/bar\baz cannot be represented on windows
					return errors.WithStack(&os.PathError{Path: p.Stat.Path, Err: syscall.EINVAL, Op: "unrepresentable path"})
				}
				var metaOnly bool
				if metadataTransfer {
					if path == metadataPath {
						continue
					}
					n := p.Stat.SizeVT()
					dt := metadataBuffer.alloc(n + 4)
					binary.LittleEndian.PutUint32(dt[0:4], uint32(n))
					_, err := p.Stat.MarshalToSizedBufferVT(dt[4:])
					if err != nil {
						return err
					}
					if !r.metadataOnly(path, p.Stat) {
						metaOnly = true
					}
				}
				p.Stat.Path = path
				p.Stat.Linkname = filepath.FromSlash(p.Stat.Linkname)

				if !metaOnly && fileCanRequestData(os.FileMode(p.Stat.Mode)) {
					r.mu.Lock()
					r.files[p.Stat.Path] = i
					r.mu.Unlock()
				}
				i++

				cp := &currentPath{path: path, stat: p.Stat}
				if err := r.orderValidator.HandleChange(ChangeKindAdd, cp.path, &StatInfo{cp.stat}, nil); err != nil {
					return err
				}
				if err := r.hlValidator.HandleChange(ChangeKindAdd, cp.path, &StatInfo{cp.stat}, nil); err != nil {
					return err
				}
				if metadataTransfer {
					parent := filepath.Dir(cp.path)
					isDir := os.FileMode(p.Stat.Mode).IsDir()
					for {
						last, ok := metadataParents.peek()
						if !ok || parent == last.path {
							break
						}
						metadataParents.pop()
					}
					if isDir {
						metadataParents.push(cp)
					}
					if metaOnly {
						continue
					} else {
						for _, cp := range metadataParents.items {
							if err := w.update(cp); err != nil {
								return err
							}
						}
						metadataParents.clear()
					}
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

	if err := g.Wait(); err != nil {
		return err
	}

	if !metadataTransfer {
		return nil
	}

	return r.writeMetadata(metadataBuffer)
}

func (r *receiver) newDiskWriter(ctx context.Context, opt DiskWriterOpt) (receiveDiskWriter, error) {
	if r.root != nil {
		return NewRootDiskWriter(ctx, r.root, opt)
	}
	return NewDiskWriter(ctx, r.dest, opt)
}

func (r *receiver) destWalker() walkerFn {
	if r.root != nil {
		return getRootWalkerFn(r.root)
	}
	return getWalkerFn(r.dest)
}

func (r *receiver) writeMetadata(metadataBuffer *buffer) error {
	if r.root != nil {
		// although we don't allow tranferring metadataPath, make sure there was no preexisting file/symlink
		_ = r.root.Remove(metadataPath)

		f, err := r.root.OpenFile(metadataPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return errors.WithStack(err)
		}
		if _, err := metadataBuffer.WriteTo(f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}

	// although we don't allow tranferring metadataPath, make sure there was no preexisting file/symlink
	os.Remove(filepath.Join(r.dest, metadataPath))

	f, err := os.OpenFile(filepath.Join(r.dest, metadataPath), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := metadataBuffer.WriteTo(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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

type stack[T any] struct {
	items []T
}

func newStack[T any]() *stack[T] {
	return &stack[T]{
		items: make([]T, 0, 8),
	}
}

func (s *stack[T]) push(v T) {
	s.items = append(s.items, v)
}

func (s *stack[T]) pop() (T, bool) {
	if len(s.items) == 0 {
		var zero T
		return zero, false
	}
	v := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return v, true
}

func (s *stack[T]) peek() (T, bool) {
	if len(s.items) == 0 {
		var zero T
		return zero, false
	}
	return s.items[len(s.items)-1], true
}

func (s *stack[T]) clear() {
	s.items = s.items[:0]
}

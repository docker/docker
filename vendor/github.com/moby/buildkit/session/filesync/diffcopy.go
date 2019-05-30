package filesync

import (
	"bufio"
	io "io"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"google.golang.org/grpc"
)

func sendDiffCopy(stream grpc.Stream, fs fsutil.FS, progress progressCb) error {
	return fsutil.Send(stream.Context(), stream, fs, progress)
}

func newStreamWriter(stream grpc.ClientStream) io.WriteCloser {
	wc := &streamWriterCloser{ClientStream: stream}
	return &bufferedWriteCloser{Writer: bufio.NewWriter(wc), Closer: wc}
}

type bufferedWriteCloser struct {
	*bufio.Writer
	io.Closer
}

func (bwc *bufferedWriteCloser) Close() error {
	if err := bwc.Writer.Flush(); err != nil {
		return err
	}
	return bwc.Closer.Close()
}

type streamWriterCloser struct {
	grpc.ClientStream
}

func (wc *streamWriterCloser) Write(dt []byte) (int, error) {
	if err := wc.ClientStream.SendMsg(&BytesMessage{Data: dt}); err != nil {
		return 0, err
	}
	return len(dt), nil
}

func (wc *streamWriterCloser) Close() error {
	if err := wc.ClientStream.CloseSend(); err != nil {
		return err
	}
	// block until receiver is done
	var bm BytesMessage
	if err := wc.ClientStream.RecvMsg(&bm); err != io.EOF {
		return err
	}
	return nil
}

func recvDiffCopy(ds grpc.Stream, dest string, cu CacheUpdater, progress progressCb, filter func(string, *fstypes.Stat) bool) error {
	st := time.Now()
	defer func() {
		logrus.Debugf("diffcopy took: %v", time.Since(st))
	}()
	var cf fsutil.ChangeFunc
	var ch fsutil.ContentHasher
	if cu != nil {
		cu.MarkSupported(true)
		cf = cu.HandleChange
		ch = cu.ContentHasher()
	}
	return fsutil.Receive(ds.Context(), ds, dest, fsutil.ReceiveOpt{
		NotifyHashed:  cf,
		ContentHasher: ch,
		ProgressCb:    progress,
		Filter:        fsutil.FilterFunc(filter),
	})
}

func syncTargetDiffCopy(ds grpc.Stream, dest string) error {
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	return fsutil.Receive(ds.Context(), ds, dest, fsutil.ReceiveOpt{
		Merge: true,
		Filter: func() func(string, *fstypes.Stat) bool {
			uid := os.Getuid()
			gid := os.Getgid()
			return func(p string, st *fstypes.Stat) bool {
				st.Uid = uint32(uid)
				st.Gid = uint32(gid)
				return true
			}
		}(),
	})
}

func writeTargetFile(ds grpc.Stream, wc io.WriteCloser) error {
	for {
		bm := BytesMessage{}
		if err := ds.RecvMsg(&bm); err != nil {
			if errors.Cause(err) == io.EOF {
				return nil
			}
			return err
		}
		if _, err := wc.Write(bm.Data); err != nil {
			return err
		}
	}
}

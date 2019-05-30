package upload

import (
	"context"
	io "io"
	"net/url"

	"github.com/moby/buildkit/session"
	"google.golang.org/grpc/metadata"
)

const (
	keyPath = "urlpath"
	keyHost = "urlhost"
)

func New(ctx context.Context, c session.Caller, url *url.URL) (*Upload, error) {
	opts := map[string][]string{
		keyPath: {url.Path},
		keyHost: {url.Host},
	}

	client := NewUploadClient(c.Conn())

	ctx = metadata.NewOutgoingContext(ctx, opts)

	cc, err := client.Pull(ctx)
	if err != nil {
		return nil, err
	}

	return &Upload{cc: cc}, nil
}

type Upload struct {
	cc Upload_PullClient
}

func (u *Upload) WriteTo(w io.Writer) (int, error) {
	n := 0
	for {
		var bm BytesMessage
		if err := u.cc.RecvMsg(&bm); err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
		nn, err := w.Write(bm.Data)
		n += nn
		if err != nil {
			return n, err
		}
	}
}

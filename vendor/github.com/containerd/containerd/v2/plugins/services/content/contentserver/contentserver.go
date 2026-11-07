/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package contentserver

import (
	"context"
	"fmt"
	"io"
	"sync"

	api "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
)

type service struct {
	store content.Store
	api.UnimplementedContentServer
}

var (
	empty   = &ptypes.Empty{}
	bufPool = sync.Pool{
		New: func() any {
			buffer := make([]byte, 1<<20)
			return &buffer
		},
	}
)

// New returns the content GRPC server
func New(cs content.Store) api.ContentServer {
	return &service{store: cs}
}

func (s *service) Register(server *grpc.Server) error {
	api.RegisterContentServer(server, s)
	return nil
}

func (s *service) Info(ctx context.Context, req *api.InfoRequest) (*api.InfoResponse, error) {
	dg, err := digest.Parse(req.Digest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q failed validation", req.Digest)
	}

	bi, err := s.store.Info(ctx, dg)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return &api.InfoResponse{
		Info: infoToGRPC(bi),
	}, nil
}

func (s *service) Update(ctx context.Context, req *api.UpdateRequest) (*api.UpdateResponse, error) {
	_, err := digest.Parse(req.Info.Digest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q failed validation", req.Info.Digest)
	}

	info, err := s.store.Update(ctx, infoFromGRPC(req.Info), req.UpdateMask.GetPaths()...)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return &api.UpdateResponse{
		Info: infoToGRPC(info),
	}, nil
}

func (s *service) List(req *api.ListContentRequest, session api.Content_ListServer) error {
	var (
		buffer    []*api.Info
		sendBlock = func(block []*api.Info) error {
			// send last block
			return session.Send(&api.ListContentResponse{
				Info: block,
			})
		}
	)

	if err := s.store.Walk(session.Context(), func(info content.Info) error {
		buffer = append(buffer, infoToGRPC(info))

		if len(buffer) >= 100 {
			if err := sendBlock(buffer); err != nil {
				return err
			}

			buffer = buffer[:0]
		}

		return nil
	}, req.Filters...); err != nil {
		return errgrpc.ToGRPC(err)
	}

	if len(buffer) > 0 {
		// send last block
		if err := sendBlock(buffer); err != nil {
			return err
		}
	}

	return nil
}

func (s *service) Delete(ctx context.Context, req *api.DeleteContentRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("digest", req.Digest).Debugf("delete content")
	dg, err := digest.Parse(req.Digest)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := s.store.Delete(ctx, dg); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return empty, nil
}

func (s *service) Read(req *api.ReadContentRequest, session api.Content_ReadServer) error {
	dg, err := digest.Parse(req.Digest)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "%v: %v", req.Digest, err)
	}

	oi, err := s.store.Info(session.Context(), dg)
	if err != nil {
		return errgrpc.ToGRPC(err)
	}

	ra, err := s.store.ReaderAt(session.Context(), ocispec.Descriptor{Digest: dg})
	if err != nil {
		return errgrpc.ToGRPC(err)
	}
	defer ra.Close()

	var (
		offset = req.Offset
		// size is read size, not the expected size of the blob (oi.Size), which the caller might not be aware of.
		// offset+size can be larger than oi.Size.
		size = req.Size

		// TODO(stevvooe): Using the global buffer pool. At 32KB, it is probably
		// little inefficient for work over a fast network. We can tune this later.
		p = bufPool.Get().(*[]byte)
	)
	defer bufPool.Put(p)

	if offset < 0 {
		offset = 0
	}

	if offset > oi.Size {
		return status.Errorf(codes.OutOfRange, "read past object length %v bytes", oi.Size)
	}

	if size <= 0 || offset+size > oi.Size {
		size = oi.Size - offset
	}

	_, err = io.CopyBuffer(
		&readResponseWriter{session: session},
		io.NewSectionReader(ra, offset, size), *p)
	return errgrpc.ToGRPC(err)
}

// readResponseWriter is a writer that places the output into ReadContentRequest messages.
//
// This allows io.CopyBuffer to do the heavy lifting of chunking the responses
// into the buffer size.
type readResponseWriter struct {
	offset  int64
	session api.Content_ReadServer
}

func (rw *readResponseWriter) Write(p []byte) (n int, err error) {
	if err := rw.session.Send(&api.ReadContentResponse{
		Offset: rw.offset,
		Data:   p,
	}); err != nil {
		return 0, err
	}

	rw.offset += int64(len(p))
	return len(p), nil
}

func (s *service) Status(ctx context.Context, req *api.StatusRequest) (*api.StatusResponse, error) {
	status, err := s.store.Status(ctx, req.Ref)
	if err != nil {
		return nil, errgrpc.ToGRPCf(err, "could not get status for ref %q", req.Ref)
	}

	var resp api.StatusResponse
	resp.Status = &api.Status{
		StartedAt: protobuf.ToTimestamp(status.StartedAt),
		UpdatedAt: protobuf.ToTimestamp(status.UpdatedAt),
		Ref:       status.Ref,
		Offset:    status.Offset,
		Total:     status.Total,
		Expected:  status.Expected.String(),
	}

	return &resp, nil
}

func (s *service) ListStatuses(ctx context.Context, req *api.ListStatusesRequest) (*api.ListStatusesResponse, error) {
	statuses, err := s.store.ListStatuses(ctx, req.Filters...)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	var resp api.ListStatusesResponse
	for _, status := range statuses {
		resp.Statuses = append(resp.Statuses, &api.Status{
			StartedAt: protobuf.ToTimestamp(status.StartedAt),
			UpdatedAt: protobuf.ToTimestamp(status.UpdatedAt),
			Ref:       status.Ref,
			Offset:    status.Offset,
			Total:     status.Total,
			Expected:  status.Expected.String(),
		})
	}

	return &resp, nil
}

func (s *service) Write(session api.Content_WriteServer) (err error) {
	var (
		ctx      = session.Context()
		msg      api.WriteContentResponse
		req      *api.WriteContentRequest
		ref      string
		total    int64
		expected digest.Digest
	)

	defer func(msg *api.WriteContentResponse) {
		// pump through the last message if no error was encountered
		if err != nil {
			if s, ok := status.FromError(err); ok {
				switch s.Code() {
				case codes.AlreadyExists:
					// Do nothing, expected if another service already pushed the same content
				case codes.Unavailable:
					// This happens when multiple services pull the same image at once (e.g., in docker compose)
					log.G(ctx).WithError(err).Debug("(*service).Write skipped: content locked by another service")
					// Do not treat this as an error — safe to ignore
				default:
					// TODO(stevvooe): This log was noisy; now reduced to log only unexpected gRPC errors.
					// Consider using an interceptor for consistent gRPC error reporting across all methods.
					log.G(ctx).WithError(err).Error("(*service).Write failed")
				}
			} else {
				// Not a gRPC error — log it
				log.G(ctx).WithError(err).Error("(*service).Write failed (non-gRPC error)")
			}
			return
		}

		err = session.Send(msg)
	}(&msg)

	// handle the very first request!
	req, err = session.Recv()
	if err != nil {
		return err
	}

	ref = req.Ref

	if ref == "" {
		return status.Errorf(codes.InvalidArgument, "first message must have a reference")
	}

	fields := log.Fields{
		"ref": ref,
	}
	total = req.Total
	expected = digest.Digest(req.Expected)
	if total > 0 {
		fields["total"] = total
	}

	if expected != "" {
		fields["expected"] = expected
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(fields))

	log.G(ctx).Debug("(*service).Write started")
	// this action locks the writer for the session.
	wr, err := s.store.Writer(ctx,
		content.WithRef(ref),
		content.WithDescriptor(ocispec.Descriptor{Size: total, Digest: expected}))
	if err != nil {
		return errgrpc.ToGRPC(err)
	}
	defer wr.Close()

	for {
		msg.Action = req.Action
		ws, err := wr.Status()
		if err != nil {
			return errgrpc.ToGRPC(err)
		}

		msg.Offset = ws.Offset // always set the offset.

		// NOTE(stevvooe): In general, there are two cases underwhich a remote
		// writer is used.
		//
		// For pull, we almost always have this before fetching large content,
		// through descriptors. We allow predeclaration of the expected size
		// and digest.
		//
		// For push, it is more complex. If we want to cut through content into
		// storage, we may have no expectation until we are done processing the
		// content. The case here is the following:
		//
		// 	1. Start writing content.
		// 	2. Compress inline.
		// 	3. Validate digest and size (maybe).
		//
		// Supporting these two paths is quite awkward but it lets both API
		// users use the same writer style for each with a minimum of overhead.
		if req.Expected != "" {
			dg := digest.Digest(req.Expected)
			if expected != "" && expected != dg {
				log.G(ctx).Debugf("commit digest differs from writer digest: %v != %v", dg, expected)
			}
			expected = dg

			if _, err := s.store.Info(session.Context(), dg); err == nil {
				if err := wr.Close(); err != nil {
					log.G(ctx).WithError(err).Error("failed to close writer")
				}
				if err := s.store.Abort(session.Context(), ref); err != nil {
					log.G(ctx).WithError(err).Error("failed to abort write")
				}

				return status.Errorf(codes.AlreadyExists, "blob with expected digest %v exists", req.Expected)
			}
		}

		if req.Total > 0 {
			// Update the expected total. Typically, this could be seen at
			// negotiation time or on a commit message.
			if total > 0 && req.Total != total {
				log.G(ctx).Debugf("commit size differs from writer size: %v != %v", req.Total, total)
			}
			total = req.Total
		}

		switch req.Action {
		case api.WriteAction_STAT:
			msg.Digest = wr.Digest().String()
			msg.StartedAt = protobuf.ToTimestamp(ws.StartedAt)
			msg.UpdatedAt = protobuf.ToTimestamp(ws.UpdatedAt)
			msg.Total = total
		case api.WriteAction_WRITE, api.WriteAction_COMMIT:
			if req.Offset > 0 {
				// validate the offset if provided
				if req.Offset != ws.Offset {
					return status.Errorf(codes.OutOfRange, "write @%v must occur at current offset %v", req.Offset, ws.Offset)
				}
			}

			if req.Offset == 0 && ws.Offset > 0 {
				if err := wr.Truncate(req.Offset); err != nil {
					return fmt.Errorf("truncate failed: %w", err)
				}
				msg.Offset = req.Offset
			}

			// issue the write if we actually have data.
			if len(req.Data) > 0 {
				// While this looks like we could use io.WriterAt here, because we
				// maintain the offset as append only, we just issue the write.
				n, err := wr.Write(req.Data)
				if err != nil {
					return errgrpc.ToGRPC(err)
				}

				if n != len(req.Data) {
					// TODO(stevvooe): Perhaps, we can recover this by including it
					// in the offset on the write return.
					return status.Errorf(codes.DataLoss, "wrote %v of %v bytes", n, len(req.Data))
				}

				msg.Offset += int64(n)
			}

			if req.Action == api.WriteAction_COMMIT {
				var opts []content.Opt
				if req.Labels != nil {
					opts = append(opts, content.WithLabels(req.Labels))
				}
				if err := wr.Commit(ctx, total, expected, opts...); err != nil {
					return errgrpc.ToGRPC(err)
				}
			}

			msg.Digest = wr.Digest().String()
		}

		if err := session.Send(&msg); err != nil {
			return err
		}

		if req.Action == api.WriteAction_COMMIT {
			return nil
		}

		req, err = session.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}
	}
}

func (s *service) Abort(ctx context.Context, req *api.AbortRequest) (*ptypes.Empty, error) {
	if err := s.store.Abort(ctx, req.Ref); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return empty, nil
}

func infoToGRPC(info content.Info) *api.Info {
	return &api.Info{
		Digest:    info.Digest.String(),
		Size:      info.Size,
		CreatedAt: protobuf.ToTimestamp(info.CreatedAt),
		UpdatedAt: protobuf.ToTimestamp(info.UpdatedAt),
		Labels:    info.Labels,
	}
}

func infoFromGRPC(info *api.Info) content.Info {
	return content.Info{
		Digest:    digest.Digest(info.Digest),
		Size:      info.Size,
		CreatedAt: protobuf.FromTimestamp(info.CreatedAt),
		UpdatedAt: protobuf.FromTimestamp(info.UpdatedAt),
		Labels:    info.Labels,
	}
}

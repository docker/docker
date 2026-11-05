package client

import (
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const statusResponseSize = 1024 * 1024

func NewSolveStatus(resp *controlapi.StatusResponse) *SolveStatus {
	s := &SolveStatus{}
	for _, v := range resp.Vertexes {
		s.Vertexes = append(s.Vertexes, &Vertex{
			Digest:        digest.Digest(v.Digest),
			Inputs:        digestSliceFromPB(v.Inputs),
			Name:          v.Name,
			Started:       timestampFromPB(v.Started),
			Completed:     timestampFromPB(v.Completed),
			Error:         v.Error,
			Cached:        v.Cached,
			ProgressGroup: v.ProgressGroup,
		})
	}
	for _, v := range resp.Statuses {
		s.Statuses = append(s.Statuses, &VertexStatus{
			ID:        v.ID,
			Vertex:    digest.Digest(v.Vertex),
			Name:      v.Name,
			Total:     v.Total,
			Current:   v.Current,
			Timestamp: v.Timestamp.AsTime(),
			Started:   timestampFromPB(v.Started),
			Completed: timestampFromPB(v.Completed),
		})
	}
	for _, v := range resp.Logs {
		s.Logs = append(s.Logs, &VertexLog{
			Vertex:    digest.Digest(v.Vertex),
			Stream:    int(v.Stream),
			Data:      v.Msg,
			Timestamp: v.Timestamp.AsTime(),
		})
	}
	for _, v := range resp.Warnings {
		s.Warnings = append(s.Warnings, &VertexWarning{
			Vertex:     digest.Digest(v.Vertex),
			Level:      int(v.Level),
			Short:      v.Short,
			Detail:     v.Detail,
			URL:        v.Url,
			SourceInfo: v.Info,
			Range:      v.Ranges,
		})
	}
	return s
}

// Marshal batches status updates by encoded size without modifying SolveStatus.
// Logs may be split across responses. Individual metadata entries larger than
// the batch target are emitted intact in their own response.
func (ss *SolveStatus) Marshal() (out []*controlapi.StatusResponse) {
	sr := &controlapi.StatusResponse{}
	size := 0

	// Each repeated field has a one-byte tag and a length-prefixed message.
	// Keep indivisible metadata entries intact even if they exceed the target.
	reserve := func(n int) {
		n = 1 + protowire.SizeBytes(n)
		if size > 0 && size+n > statusResponseSize {
			out = append(out, sr)
			sr = &controlapi.StatusResponse{}
			size = 0
		}
		size += n
	}

	for _, v := range ss.Vertexes {
		p := &controlapi.Vertex{
			Digest:        string(v.Digest),
			Inputs:        digestSliceToPB(v.Inputs),
			Name:          v.Name,
			Started:       timestampToPB(v.Started),
			Completed:     timestampToPB(v.Completed),
			Error:         v.Error,
			Cached:        v.Cached,
			ProgressGroup: v.ProgressGroup,
		}
		reserve(p.SizeVT())
		sr.Vertexes = append(sr.Vertexes, p)
	}
	for _, v := range ss.Statuses {
		p := &controlapi.VertexStatus{
			ID:        v.ID,
			Vertex:    string(v.Vertex),
			Name:      v.Name,
			Current:   v.Current,
			Total:     v.Total,
			Timestamp: timestamppb.New(v.Timestamp),
			Started:   timestampToPB(v.Started),
			Completed: timestampToPB(v.Completed),
		}
		reserve(p.SizeVT())
		sr.Statuses = append(sr.Statuses, p)
	}
	for _, v := range ss.Logs {
		p := &controlapi.VertexLog{
			Vertex:    string(v.Vertex),
			Stream:    int64(v.Stream),
			Timestamp: timestamppb.New(v.Timestamp),
		}
		const maxData = statusResponseSize / 2
		data := v.Data
		for {
			p.Msg = data[:min(len(data), maxData)]
			reserve(p.SizeVT())
			sr.Logs = append(sr.Logs, p)
			data = data[len(p.Msg):]
			if len(data) == 0 {
				break
			}
			p = &controlapi.VertexLog{Vertex: p.Vertex, Stream: p.Stream, Timestamp: p.Timestamp}
		}
	}
	for _, v := range ss.Warnings {
		p := &controlapi.VertexWarning{
			Vertex: string(v.Vertex),
			Level:  int64(v.Level),
			Short:  v.Short,
			Detail: v.Detail,
			Info:   v.SourceInfo,
			Ranges: v.Range,
			Url:    v.URL,
		}
		reserve(p.SizeVT())
		sr.Warnings = append(sr.Warnings, p)
	}
	out = append(out, sr)
	return
}

func digestSliceFromPB(elems []string) []digest.Digest {
	clone := make([]digest.Digest, len(elems))
	for i, e := range elems {
		clone[i] = digest.Digest(e)
	}
	return clone
}

func digestSliceToPB(elems []digest.Digest) []string {
	clone := make([]string, len(elems))
	for i, e := range elems {
		clone[i] = string(e)
	}
	return clone
}

func timestampFromPB(ts *timestamppb.Timestamp) *time.Time {
	if ts != nil {
		t := ts.AsTime()
		return &t
	}
	return nil
}

func timestampToPB(ts *time.Time) *timestamppb.Timestamp {
	if ts != nil {
		return timestamppb.New(*ts)
	}
	return nil
}

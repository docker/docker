package client

import (
	"context"

	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	fstypes "github.com/tonistiigi/fsutil/types"
)

type Client interface {
	Solve(ctx context.Context, req SolveRequest) (*Result, error)
	ResolveImageConfig(ctx context.Context, ref string, opt ResolveImageConfigOpt) (digest.Digest, []byte, error)
	BuildOpts() BuildOpts
}

type Reference interface {
	ReadFile(ctx context.Context, req ReadRequest) ([]byte, error)
	StatFile(ctx context.Context, req StatRequest) (*fstypes.Stat, error)
	ReadDir(ctx context.Context, req ReadDirRequest) ([]*fstypes.Stat, error)
}

type ReadRequest struct {
	Filename string
	Range    *FileRange
}

type FileRange struct {
	Offset int
	Length int
}

type ReadDirRequest struct {
	Path           string
	IncludePattern string
}

type StatRequest struct {
	Path string
}

// SolveRequest is same as frontend.SolveRequest but avoiding dependency
type SolveRequest struct {
	Definition   *pb.Definition
	Frontend     string
	FrontendOpt  map[string]string
	CacheImports []CacheOptionsEntry
}

type CacheOptionsEntry struct {
	Type  string
	Attrs map[string]string
}

type WorkerInfo struct {
	ID        string
	Labels    map[string]string
	Platforms []specs.Platform
}

type BuildOpts struct {
	Opts      map[string]string
	SessionID string
	Workers   []WorkerInfo
	Product   string
	LLBCaps   apicaps.CapSet
	Caps      apicaps.CapSet
}

type ResolveImageConfigOpt struct {
	Platform    *specs.Platform
	ResolveMode string
	LogName     string
}

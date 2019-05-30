package worker

import (
	"context"
	"io"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/frontend"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

type Worker interface {
	// ID needs to be unique in the cluster
	ID() string
	Labels() map[string]string
	Platforms() []specs.Platform
	GCPolicy() []client.PruneInfo
	LoadRef(id string, hidden bool) (cache.ImmutableRef, error)
	// ResolveOp resolves Vertex.Sys() to Op implementation.
	ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager) (solver.Op, error)
	ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt, sm *session.Manager) (digest.Digest, []byte, error)
	// Exec is similar to executor.Exec but without []mount.Mount
	Exec(ctx context.Context, meta executor.Meta, rootFS cache.ImmutableRef, stdin io.ReadCloser, stdout, stderr io.WriteCloser) error
	DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error)
	Exporter(name string, sm *session.Manager) (exporter.Exporter, error)
	Prune(ctx context.Context, ch chan client.UsageInfo, opt ...client.PruneInfo) error
	GetRemote(ctx context.Context, ref cache.ImmutableRef, createIfNeeded bool) (*solver.Remote, error)
	FromRemote(ctx context.Context, remote *solver.Remote) (cache.ImmutableRef, error)
}

// Pre-defined label keys
const (
	labelPrefix      = "org.mobyproject.buildkit.worker."
	LabelExecutor    = labelPrefix + "executor"    // "oci" or "containerd"
	LabelSnapshotter = labelPrefix + "snapshotter" // containerd snapshotter name ("overlay", "native", ...)
	LabelHostname    = labelPrefix + "hostname"
)

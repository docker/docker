package llbsolver

import (
	"fmt"
	"strings"

	"github.com/containerd/containerd/platforms"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/util/entitlements"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type vertex struct {
	sys     interface{}
	options solver.VertexOptions
	inputs  []solver.Edge
	digest  digest.Digest
	name    string
}

func (v *vertex) Digest() digest.Digest {
	return v.digest
}

func (v *vertex) Sys() interface{} {
	return v.sys
}

func (v *vertex) Options() solver.VertexOptions {
	return v.options
}

func (v *vertex) Inputs() []solver.Edge {
	return v.inputs
}

func (v *vertex) Name() string {
	if name, ok := v.options.Description["llb.customname"]; ok {
		return name
	}
	return v.name
}

type LoadOpt func(*pb.Op, *pb.OpMetadata, *solver.VertexOptions) error

func WithValidateCaps() LoadOpt {
	cs := pb.Caps.CapSet(pb.Caps.All())
	return func(_ *pb.Op, md *pb.OpMetadata, opt *solver.VertexOptions) error {
		if md != nil {
			for c := range md.Caps {
				if err := cs.Supports(c); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func WithCacheSources(cms []solver.CacheManager) LoadOpt {
	return func(_ *pb.Op, _ *pb.OpMetadata, opt *solver.VertexOptions) error {
		opt.CacheSources = cms
		return nil
	}
}

func RuntimePlatforms(p []specs.Platform) LoadOpt {
	var defaultPlatform *pb.Platform
	pp := make([]specs.Platform, len(p))
	for i := range p {
		pp[i] = platforms.Normalize(p[i])
	}
	return func(op *pb.Op, _ *pb.OpMetadata, opt *solver.VertexOptions) error {
		if op.Platform == nil {
			if defaultPlatform == nil {
				p := platforms.DefaultSpec()
				defaultPlatform = &pb.Platform{
					OS:           p.OS,
					Architecture: p.Architecture,
					Variant:      p.Variant,
				}
			}
			op.Platform = defaultPlatform
		}
		platform := specs.Platform{OS: op.Platform.OS, Architecture: op.Platform.Architecture, Variant: op.Platform.Variant}
		normalizedPlatform := platforms.Normalize(platform)

		op.Platform = &pb.Platform{
			OS:           normalizedPlatform.OS,
			Architecture: normalizedPlatform.Architecture,
			Variant:      normalizedPlatform.Variant,
		}

		if _, ok := op.Op.(*pb.Op_Exec); ok {
			var found bool
			for _, pp := range pp {
				if pp.OS == op.Platform.OS && pp.Architecture == op.Platform.Architecture && pp.Variant == op.Platform.Variant {
					found = true
					break
				}
			}
			if !found {
				return errors.Errorf("runtime execution on platform %s not supported", platforms.Format(specs.Platform{OS: op.Platform.OS, Architecture: op.Platform.Architecture, Variant: op.Platform.Variant}))
			}
		}
		return nil
	}
}

func ValidateEntitlements(ent entitlements.Set) LoadOpt {
	return func(op *pb.Op, _ *pb.OpMetadata, opt *solver.VertexOptions) error {
		switch op := op.Op.(type) {
		case *pb.Op_Exec:
			if op.Exec.Network == pb.NetMode_HOST {
				if !ent.Allowed(entitlements.EntitlementNetworkHost) {
					return errors.Errorf("%s is not allowed", entitlements.EntitlementNetworkHost)
				}
			}

			if op.Exec.Security == pb.SecurityMode_INSECURE {
				if !ent.Allowed(entitlements.EntitlementSecurityInsecure) {
					return errors.Errorf("%s is not allowed", entitlements.EntitlementSecurityInsecure)
				}
			}
		}
		return nil
	}
}

func Load(def *pb.Definition, opts ...LoadOpt) (solver.Edge, error) {
	return loadLLB(def, func(dgst digest.Digest, pbOp *pb.Op, load func(digest.Digest) (solver.Vertex, error)) (solver.Vertex, error) {
		opMetadata := def.Metadata[dgst]
		vtx, err := newVertex(dgst, pbOp, &opMetadata, load, opts...)
		if err != nil {
			return nil, err
		}
		return vtx, nil
	})
}

func newVertex(dgst digest.Digest, op *pb.Op, opMeta *pb.OpMetadata, load func(digest.Digest) (solver.Vertex, error), opts ...LoadOpt) (*vertex, error) {
	opt := solver.VertexOptions{}
	if opMeta != nil {
		opt.IgnoreCache = opMeta.IgnoreCache
		opt.Description = opMeta.Description
		if opMeta.ExportCache != nil {
			opt.ExportCache = &opMeta.ExportCache.Value
		}
	}
	for _, fn := range opts {
		if err := fn(op, opMeta, &opt); err != nil {
			return nil, err
		}
	}

	vtx := &vertex{sys: op, options: opt, digest: dgst, name: llbOpName(op)}
	for _, in := range op.Inputs {
		sub, err := load(in.Digest)
		if err != nil {
			return nil, err
		}
		vtx.inputs = append(vtx.inputs, solver.Edge{Index: solver.Index(in.Index), Vertex: sub})
	}
	return vtx, nil
}

// loadLLB loads LLB.
// fn is executed sequentially.
func loadLLB(def *pb.Definition, fn func(digest.Digest, *pb.Op, func(digest.Digest) (solver.Vertex, error)) (solver.Vertex, error)) (solver.Edge, error) {
	if len(def.Def) == 0 {
		return solver.Edge{}, errors.New("invalid empty definition")
	}

	allOps := make(map[digest.Digest]*pb.Op)

	var dgst digest.Digest

	for _, dt := range def.Def {
		var op pb.Op
		if err := (&op).Unmarshal(dt); err != nil {
			return solver.Edge{}, errors.Wrap(err, "failed to parse llb proto op")
		}
		dgst = digest.FromBytes(dt)
		allOps[dgst] = &op
	}

	lastOp := allOps[dgst]
	delete(allOps, dgst)
	dgst = lastOp.Inputs[0].Digest

	cache := make(map[digest.Digest]solver.Vertex)

	var rec func(dgst digest.Digest) (solver.Vertex, error)
	rec = func(dgst digest.Digest) (solver.Vertex, error) {
		if v, ok := cache[dgst]; ok {
			return v, nil
		}
		op, ok := allOps[dgst]
		if !ok {
			return nil, errors.Errorf("invalid missing input digest %s", dgst)
		}
		v, err := fn(dgst, op, rec)
		if err != nil {
			return nil, err
		}
		cache[dgst] = v
		return v, nil
	}

	v, err := rec(dgst)
	if err != nil {
		return solver.Edge{}, err
	}
	return solver.Edge{Vertex: v, Index: solver.Index(lastOp.Inputs[0].Index)}, nil
}

func llbOpName(op *pb.Op) string {
	switch op := op.Op.(type) {
	case *pb.Op_Source:
		if id, err := source.FromLLB(op, nil); err == nil {
			if id, ok := id.(*source.LocalIdentifier); ok {
				if len(id.IncludePatterns) == 1 {
					return op.Source.Identifier + " (" + id.IncludePatterns[0] + ")"
				}
			}
		}
		return op.Source.Identifier
	case *pb.Op_Exec:
		return strings.Join(op.Exec.Meta.Args, " ")
	case *pb.Op_File:
		return fileOpName(op.File.Actions)
	case *pb.Op_Build:
		return "build"
	default:
		return "unknown"
	}
}

func fileOpName(actions []*pb.FileAction) string {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		switch a := action.Action.(type) {
		case *pb.FileAction_Mkdir:
			names = append(names, fmt.Sprintf("mkdir %s", a.Mkdir.Path))
		case *pb.FileAction_Mkfile:
			names = append(names, fmt.Sprintf("mkfile %s", a.Mkfile.Path))
		case *pb.FileAction_Rm:
			names = append(names, fmt.Sprintf("rm %s", a.Rm.Path))
		case *pb.FileAction_Copy:
			names = append(names, fmt.Sprintf("copy %s %s", a.Copy.Src, a.Copy.Dest))
		}
	}

	return strings.Join(names, ", ")
}

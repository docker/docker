package llbsolver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/llbsolver/linuxresources"
	"github.com/moby/buildkit/solver/llbsolver/ops/opsutils"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/entitlements"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type vertex struct {
	sys     any
	options solver.VertexOptions
	inputs  []solver.Edge
	digest  digest.Digest
	name    string
}

func (v *vertex) Digest() digest.Digest {
	return v.digest
}

func (v *vertex) Sys() any {
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
				if err := cs.Supports(apicaps.CapID(c)); err != nil {
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

func WithLinuxResourcesMetadata() LoadOpt {
	return func(op *pb.Op, md *pb.OpMetadata, opt *solver.VertexOptions) error {
		if _, ok := op.Op.(*pb.Op_Exec); !ok {
			return nil
		}
		if md == nil || md.LinuxResources == nil {
			return nil
		}
		opt.Metadata = &linuxresources.Metadata{LinuxResources: md.LinuxResources}
		return nil
	}
}

func NormalizeRuntimePlatforms() LoadOpt {
	var defaultPlatform *pb.Platform
	return func(op *pb.Op, _ *pb.OpMetadata, opt *solver.VertexOptions) error {
		if op.Platform == nil {
			if defaultPlatform == nil {
				p := platforms.DefaultSpec()
				defaultPlatform = &pb.Platform{
					OS:           p.OS,
					Architecture: p.Architecture,
					Variant:      p.Variant,
					OSVersion:    p.OSVersion,
					OSFeatures:   p.OSFeatures,
				}
			}
			op.Platform = defaultPlatform
		}
		platform := ocispecs.Platform{
			OS:           op.Platform.OS,
			Architecture: op.Platform.Architecture,
			Variant:      op.Platform.Variant,
			OSVersion:    op.Platform.OSVersion,
			OSFeatures:   op.Platform.OSFeatures,
		}
		normalizedPlatform := platforms.Normalize(platform)

		op.Platform = &pb.Platform{
			OS:           normalizedPlatform.OS,
			Architecture: normalizedPlatform.Architecture,
			Variant:      normalizedPlatform.Variant,
			OSVersion:    normalizedPlatform.OSVersion,
		}
		if normalizedPlatform.OSFeatures != nil {
			op.Platform.OSFeatures = slices.Clone(normalizedPlatform.OSFeatures)
		}

		return nil
	}
}

func ValidateEntitlements(ent entitlements.Set, cdiManager *cdidevices.Manager) LoadOpt {
	return func(op *pb.Op, _ *pb.OpMetadata, opt *solver.VertexOptions) error {
		switch op := op.Op.(type) {
		case *pb.Op_Exec:
			v := entitlements.Values{
				NetworkHost:      op.Exec.Network == pb.NetMode_HOST,
				SecurityInsecure: op.Exec.Security == pb.SecurityMode_INSECURE,
			}
			if err := ent.Check(v); err != nil {
				return err
			}
			if device := op.Exec.CdiDevices; len(device) > 0 {
				var cfg *entitlements.DevicesConfig
				if ent, ok := ent[entitlements.EntitlementDevice]; ok {
					cfg, ok = ent.(*entitlements.DevicesConfig)
					if !ok {
						return errors.Errorf("invalid device entitlement config %T", ent)
					}
				}
				if cdiManager == nil {
					for _, d := range device {
						if d == nil || d.Optional {
							continue
						}
						step := "exec"
						if op.Exec.Meta != nil && len(op.Exec.Meta.Args) > 0 {
							step = strings.Join(op.Exec.Meta.Args, " ")
						}
						if name := opt.Description["llb.customname"]; name != "" {
							step = name
						}
						return errors.Errorf("CDI device %q is required by step %q, but CDI device support is disabled", d.Name, step)
					}
					op.Exec.CdiDevices = nil
					return nil
				}
				if cfg != nil && cfg.All {
					return nil
				}

				var allowedDevices []*pb.CDIDevice
				var devicesToResolve []*pb.CDIDevice
				onDemand := make(map[string]struct{})
				for _, d := range cdiManager.ListDevices() {
					if d.OnDemand && d.AutoAllow {
						onDemand[d.Name] = struct{}{}
					}
				}
				for _, d := range op.Exec.CdiDevices {
					if d == nil {
						continue
					}
					if cfg != nil {
						if newName, ok := cfg.Devices[d.Name]; ok && newName != "" {
							d.Name = newName
							allowedDevices = append(allowedDevices, d)
							continue
						}
					}
					kind, _, _ := strings.Cut(d.Name, "=")
					// A kind with an on-demand installer has no registered devices
					// yet, so a match here can only come from a class annotation
					// on another device. That must go through the permission check.
					if _, ok := onDemand[kind]; ok && len(cdiManager.MatchDevices(d)) == 0 {
						// Resolve the requested name after the installer has registered its devices.
						allowedDevices = append(allowedDevices, d)
					} else {
						devicesToResolve = append(devicesToResolve, d)
					}
				}

				mountedDevices, err := cdiManager.FindDevices(devicesToResolve...)
				if err != nil {
					return err
				}
				if len(mountedDevices) == 0 {
					op.Exec.CdiDevices = allowedDevices
					return nil
				}

				grantedDevices := make(map[string]struct{})
				if cfg != nil {
					for d := range cfg.Devices {
						kind, _, _ := strings.Cut(d, "=")
						if _, ok := onDemand[kind]; ok && len(cdiManager.MatchDevices(&pb.CDIDevice{Name: d})) == 0 {
							continue
						}
						resolved, err := cdiManager.FindDevices(&pb.CDIDevice{Name: d})
						if err != nil {
							return err
						}
						for _, r := range resolved {
							grantedDevices[r] = struct{}{}
						}
					}
				}

				var forbidden []string
				for _, d := range mountedDevices {
					if _, ok := grantedDevices[d]; !ok {
						if dev := cdiManager.GetDevice(d); !dev.AutoAllow {
							forbidden = append(forbidden, d)
							continue
						}
					}
					allowedDevices = append(allowedDevices, &pb.CDIDevice{Name: d})
				}

				if len(forbidden) > 0 {
					if len(forbidden) == 1 {
						return errors.Errorf("device %s is requested by the build but not allowed", forbidden[0])
					}
					return errors.Errorf("devices %s are requested by the build but not allowed", strings.Join(forbidden, ", "))
				}

				op.Exec.CdiDevices = allowedDevices
			}
		}
		return nil
	}
}

type detectPrunedCacheID struct {
	ids map[string]bool
}

func (dpc *detectPrunedCacheID) Load(op *pb.Op, md *pb.OpMetadata, opt *solver.VertexOptions) error {
	if md == nil || !md.IgnoreCache {
		return nil
	}
	switch op := op.Op.(type) {
	case *pb.Op_Exec:
		for _, m := range op.Exec.GetMounts() {
			if m.MountType == pb.MountType_CACHE {
				if m.CacheOpt != nil {
					id := m.CacheOpt.ID
					if id == "" {
						id = m.Dest
					}
					if dpc.ids == nil {
						dpc.ids = map[string]bool{}
					}
					// value shows in mount is on top of a ref
					dpc.ids[id] = m.Input != -1
				}
			}
		}
	}
	return nil
}

func Load(ctx context.Context, def *pb.Definition, polEngine SourcePolicyEvaluator, opts ...LoadOpt) (solver.Edge, error) {
	return loadLLB(ctx, def, polEngine, nil, func(dgst digest.Digest, op *op, load func(digest.Digest) (solver.Vertex, error)) (solver.Vertex, error) {
		vtx, err := newVertex(dgst, op, load, opts...)
		if err != nil {
			return nil, err
		}
		return vtx, nil
	})
}

func loadWithProxyNetwork(ctx context.Context, def *pb.Definition, polEngine SourcePolicyEvaluator, proxyNetwork bool, opts ...LoadOpt) (solver.Edge, error) {
	return loadLLB(ctx, def, polEngine, &proxyNetwork, func(dgst digest.Digest, op *op, load func(digest.Digest) (solver.Vertex, error)) (solver.Vertex, error) {
		vtx, err := newVertex(dgst, op, load, opts...)
		if err != nil {
			return nil, err
		}
		return vtx, nil
	})
}

func vertexOptions(opMeta *pb.OpMetadata) solver.VertexOptions {
	opt := solver.VertexOptions{}
	if opMeta != nil {
		opt.IgnoreCache = opMeta.IgnoreCache
		opt.Description = opMeta.Description
		if opMeta.ExportCache != nil {
			opt.ExportCache = &opMeta.ExportCache.Value
		}
		opt.ProgressGroup = opMeta.ProgressGroup
	}
	return opt
}

func newVertex(dgst digest.Digest, op *op, load func(digest.Digest) (solver.Vertex, error), opts ...LoadOpt) (*vertex, error) {
	opt := vertexOptions(op.Metadata)
	for _, fn := range opts {
		if err := fn(op.Op, op.Metadata, &opt); err != nil {
			return nil, err
		}
	}

	name, err := llbOpName(op.Op, func(dgst string) (solver.Vertex, error) {
		return load(digest.Digest(dgst))
	})
	if err != nil {
		return nil, err
	}
	vtx := &vertex{sys: op.Op, options: opt, digest: dgst, name: name}
	for _, in := range op.Inputs {
		sub, err := load(digest.Digest(in.Digest))
		if err != nil {
			return nil, err
		}
		vtx.inputs = append(vtx.inputs, solver.Edge{Index: solver.Index(in.Index), Vertex: sub})
	}
	return vtx, nil
}

func recomputeDigests(ctx context.Context, all map[digest.Digest]*op, visited map[digest.Digest]digest.Digest, dgst digest.Digest) (digest.Digest, error) {
	if dgst, ok := visited[dgst]; ok {
		return dgst, nil
	}
	op, ok := all[dgst]
	if !ok {
		return "", errors.Errorf("invalid missing input digest %s", dgst)
	}

	for _, input := range op.Inputs {
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		default:
		}

		iDgst, err := recomputeDigests(ctx, all, visited, digest.Digest(input.Digest))
		if err != nil {
			return "", err
		}
		input.Digest = string(iDgst)
	}

	dt, err := op.Marshal()
	if err != nil {
		return "", err
	}
	if op.ProxyNetwork {
		dt = append(dt, []byte("\x00buildkit.proxy-network.v0")...)
	}

	newDgst := digest.FromBytes(dt)
	if newDgst != dgst {
		all[newDgst] = op
		delete(all, dgst)
	}
	visited[dgst] = newDgst
	return newDgst, nil
}

// op is a private wrapper around pb.Op that includes its metadata.
type op struct {
	*pb.Op
	Metadata     *pb.OpMetadata
	ProxyNetwork bool
}

// loadLLB loads LLB.
// fn is executed sequentially.
func loadLLB(ctx context.Context, def *pb.Definition, polEngine SourcePolicyEvaluator, proxyNetwork *bool, fn func(digest.Digest, *op, func(digest.Digest) (solver.Vertex, error)) (solver.Vertex, error)) (solver.Edge, error) {
	if len(def.Def) == 0 {
		return solver.Edge{}, errors.New("invalid empty definition")
	}
	if err := validateSourceLocations(def.Source); err != nil {
		return solver.Edge{}, err
	}

	allOps := make(map[digest.Digest]*op)
	sources := make(map[digest.Digest]struct{})

	var lastDgst digest.Digest

	for _, dt := range def.Def {
		var pbop pb.Op
		if err := pbop.Unmarshal(dt); err != nil {
			return solver.Edge{}, errors.Wrap(err, "failed to parse llb proto op")
		}
		for i, input := range pbop.Inputs {
			if input.Index < 0 {
				return solver.Edge{}, errors.Errorf("invalid input %d output index %d", i, input.Index)
			}
		}
		dgst := digest.FromBytes(dt)
		if pbop.GetSource() != nil {
			sources[dgst] = struct{}{}
		}
		allOps[dgst] = &op{
			Op:       &pbop,
			Metadata: def.Metadata[string(dgst)],
		}
		lastDgst = dgst
	}

	if proxyNetwork != nil {
		for _, op := range allOps {
			var err error
			op.ProxyNetwork, err = proxyNetworkForOp(op.Op, *proxyNetwork)
			if err != nil {
				return solver.Edge{}, err
			}
		}
	}

	if polEngine != nil && len(sources) > 0 {
		var eg errgroup.Group
		for dgst := range sources {
			eg.Go(func() error {
				op := allOps[dgst]
				if _, err := polEngine.Evaluate(ctx, op.Op); err != nil {
					return errors.Wrap(err, "error evaluating the source policy")
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return solver.Edge{}, err
		}
	}

	mutatedDigests := make(map[digest.Digest]digest.Digest) // key: old, val: new
	for dgst := range allOps {
		if _, err := recomputeDigests(ctx, allOps, mutatedDigests, dgst); err != nil {
			return solver.Edge{}, err
		}
	}

	if len(allOps) < 2 {
		return solver.Edge{}, errors.Errorf("invalid LLB with %d vertexes", len(allOps))
	}

	for {
		newDgst, ok := mutatedDigests[lastDgst]
		if !ok || newDgst == lastDgst {
			break
		}
		lastDgst = newDgst
	}

	lastOp := allOps[lastDgst]
	delete(allOps, lastDgst)
	if len(lastOp.Inputs) == 0 {
		return solver.Edge{}, errors.Errorf("invalid LLB with no inputs on last vertex")
	}
	dgst := lastOp.Inputs[0].Digest

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

		if err := opsutils.Validate(op.Op); err != nil {
			return nil, err
		}

		v, err := fn(dgst, op, rec)
		if err != nil {
			return nil, err
		}
		cache[dgst] = v
		return v, nil
	}

	v, err := rec(digest.Digest(dgst))
	if err != nil {
		return solver.Edge{}, err
	}
	return solver.Edge{Vertex: v, Index: solver.Index(lastOp.Inputs[0].Index)}, nil
}

func validateSourceLocations(src *pb.Source) error {
	if src == nil {
		return nil
	}
	for dgst, locs := range src.Locations {
		if locs == nil {
			return errors.Errorf("invalid nil source locations for vertex %s", dgst)
		}
		for i, loc := range locs.Locations {
			if loc == nil {
				return errors.Errorf("invalid nil source location %d for vertex %s", i, dgst)
			}
			if loc.SourceIndex < 0 || int(loc.SourceIndex) >= len(src.Infos) {
				return errors.Errorf("invalid source index %d for vertex %s", loc.SourceIndex, dgst)
			}
		}
	}
	return nil
}

func llbOpName(pbOp *pb.Op, load func(string) (solver.Vertex, error)) (string, error) {
	switch op := pbOp.Op.(type) {
	case *pb.Op_Source:
		return op.Source.Identifier, nil
	case *pb.Op_Exec:
		return strings.Join(op.Exec.Meta.Args, " "), nil
	case *pb.Op_File:
		return fileOpName(op.File.Actions), nil
	case *pb.Op_Build:
		return "build", nil
	case *pb.Op_Merge:
		subnames := make([]string, len(pbOp.Inputs))
		for i, inp := range pbOp.Inputs {
			subvtx, err := load(inp.Digest)
			if err != nil {
				return "", err
			}
			subnames[i] = subvtx.Name()
		}
		return "merge " + fmt.Sprintf("(%s)", strings.Join(subnames, ", ")), nil
	case *pb.Op_Diff:
		if op.Diff.Lower == nil || op.Diff.Upper == nil {
			return "", errors.Errorf("invalid diff op with nil lower or upper input")
		}
		lowerName, err := diffSideName(pbOp, op.Diff.Lower.Input, load)
		if err != nil {
			return "", err
		}
		upperName, err := diffSideName(pbOp, op.Diff.Upper.Input, load)
		if err != nil {
			return "", err
		}
		return "diff " + lowerName + " -> " + upperName, nil
	case *pb.Op_Passthrough:
		return "passthrough " + op.Passthrough.Id, nil
	default:
		return "unknown", nil
	}
}

func diffSideName(pbOp *pb.Op, input int64, load func(string) (solver.Vertex, error)) (string, error) {
	if input == int64(pb.Empty) {
		return "scratch", nil
	}
	if input < 0 || int(input) >= len(pbOp.Inputs) {
		return "", errors.Errorf("invalid diff input index %d", input)
	}
	vtx, err := load(pbOp.Inputs[input].Digest)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s)", vtx.Name()), nil
}

func fileOpName(actions []*pb.FileAction) string {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		switch a := action.Action.(type) {
		case *pb.FileAction_Mkdir:
			names = append(names, fmt.Sprintf("mkdir %s", a.Mkdir.Path))
		case *pb.FileAction_Mkfile:
			names = append(names, fmt.Sprintf("mkfile %s", a.Mkfile.Path))
		case *pb.FileAction_Symlink:
			names = append(names, fmt.Sprintf("symlink %s -> %s", a.Symlink.Newpath, a.Symlink.Oldpath))
		case *pb.FileAction_Rm:
			names = append(names, fmt.Sprintf("rm %s", a.Rm.Path))
		case *pb.FileAction_Copy:
			names = append(names, fmt.Sprintf("copy %s %s", a.Copy.Src, a.Copy.Dest))
		}
	}

	return strings.Join(names, ", ")
}

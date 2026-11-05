package llbsolver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/platforms"
	slsa02 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v0.2"
	slsa1 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v1"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/config"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/executor/resources"
	"github.com/moby/buildkit/exporter/containerimage"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend"
	dockerfileversion "github.com/moby/buildkit/frontend/dockerfile/version"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/llbsolver/provenance"
	provenancetypes "github.com/moby/buildkit/solver/llbsolver/provenance/types"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type resultWithBridge struct {
	res    *frontend.Result
	bridge *provenanceBridge
}

// provenanceBridge provides scoped access to LLBBridge and captures the request it makes for provenance
type provenanceBridge struct {
	*llbBridge
	mu      sync.Mutex
	req     *frontend.SolveRequest
	rootReq *frontend.SolveRequest

	images                 []provenancetypes.ImageSource
	builds                 []resultWithBridge
	subBridges             []*provenanceBridge
	provenanceRefRecordIDs []string
}

func (b *provenanceBridge) eachRef(f func(r solver.ResultProxy) error) error {
	builds, subBridges, _ := b.snapshot()
	for _, b := range builds {
		if err := b.res.EachRef(f); err != nil {
			return err
		}
	}
	for _, b := range subBridges {
		if err := b.eachRef(f); err != nil {
			return err
		}
	}
	return nil
}

func (b *provenanceBridge) allImages() []provenancetypes.ImageSource {
	_, subBridges, images := b.snapshot()
	for _, sb := range subBridges {
		images = append(images, sb.allImages()...)
	}
	return images
}

func (b *provenanceBridge) snapshot() ([]resultWithBridge, []*provenanceBridge, []provenancetypes.ImageSource) {
	if b == nil {
		return nil, nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.builds), slices.Clone(b.subBridges), slices.Clone(b.images)
}

func (b *provenanceBridge) requests(r *frontend.Result) (*resultRequests, error) {
	reqs := &resultRequests{
		refs: make(map[string]*resultWithBridge),
		atts: make(map[string][]*resultWithBridge),
	}

	if r.Ref != nil {
		ref, ok := b.findByResult(r.Ref)
		if !ok {
			return nil, errors.Errorf("could not find request for ref %s", r.Ref.ID())
		}
		reqs.ref = ref
	}

	for k, ref := range r.Refs {
		if ref == nil {
			continue
		}
		r, ok := b.findByResult(ref)
		if !ok {
			return nil, errors.Errorf("could not find request for ref %s", ref.ID())
		}
		reqs.refs[k] = r
	}

	for k, atts := range r.Attestations {
		for _, att := range atts {
			if att.Ref == nil {
				continue
			}
			r, ok := b.findByResult(att.Ref)
			if !ok {
				return nil, errors.Errorf("could not find request for ref %s", att.Ref.ID())
			}
			reqs.atts[k] = append(reqs.atts[k], r)
		}
	}

	ps, err := exptypes.ParsePlatforms(r.Metadata)
	if err != nil {
		return nil, err
	}
	reqs.platforms = ps.Platforms

	return reqs, nil
}

func (b *provenanceBridge) findByResult(rp solver.ResultProxy) (*resultWithBridge, bool) {
	builds, subBridges, _ := b.snapshot()
	for _, br := range subBridges {
		if req, ok := br.findByResult(rp); ok {
			return req, true
		}
	}
	for _, bld := range builds {
		found := false
		bld.res.EachRef(func(r solver.ResultProxy) error {
			if r.ID() == rp.ID() {
				found = true
			}
			return nil
		})
		if found {
			return &bld, true
		}
	}
	return nil, false
}

func (b *provenanceBridge) ResolveSourceMetadata(ctx context.Context, op *pb.SourceOp, opt sourceresolver.Opt) (*sourceresolver.MetaResponse, error) {
	resp, err := b.llbBridge.ResolveSourceMetadata(ctx, op, opt)
	if err != nil {
		return nil, err
	}
	if img := resp.Image; img != nil {
		local := !strings.HasPrefix(resp.Op.Identifier, "docker-image://")
		ref := strings.TrimPrefix(resp.Op.Identifier, "docker-image://")
		ref = strings.TrimPrefix(ref, "oci-layout://")
		b.mu.Lock()
		var platform *ocispecs.Platform
		if imgOpt := opt.ImageOpt; imgOpt != nil && imgOpt.Platform != nil {
			platform = imgOpt.Platform
		} else if ociOpt := opt.OCILayoutOpt; ociOpt != nil && ociOpt.Platform != nil {
			platform = ociOpt.Platform
		}
		b.images = append(b.images, provenancetypes.ImageSource{
			Ref:      ref,
			Platform: platform,
			Digest:   img.Digest,
			Local:    local,
		})
		b.mu.Unlock()
	}
	return resp, nil
}

func (b *provenanceBridge) Solve(ctx context.Context, req frontend.SolveRequest, sid string) (res *frontend.Result, err error) {
	req = req.Clone()
	if req.Definition != nil && req.Definition.Def != nil && req.Frontend != "" {
		return nil, errors.New("cannot solve with both Definition and Frontend specified")
	}

	if req.Definition != nil && req.Definition.Def != nil {
		rp := newResultProxy(b, req)
		res = &frontend.Result{Ref: rp}
		b.mu.Lock()
		b.builds = append(b.builds, resultWithBridge{res: res, bridge: b})
		b.mu.Unlock()
	} else if req.Frontend != "" {
		f, ok := b.frontends[req.Frontend]
		if !ok {
			return nil, errors.Errorf("invalid frontend: %s", req.Frontend)
		}
		rootReq := b.rootReq
		if !hasRequestProvenance(rootReq) {
			rootReq = b.req
		}
		wb := &provenanceBridge{llbBridge: b.llbBridge, req: &req, rootReq: rootReq}
		res, err = f.Solve(ctx, wb, b.llbBridge, req.FrontendOpt, req.FrontendInputs, sid, b.sm)
		if err != nil {
			fe := errdefs.Frontend{
				Name:   req.Frontend,
				Source: req.FrontendOpt[frontend.KeySource],
			}
			return nil, fe.WrapError(err)
		}
		wb.mu.Lock()
		wb.builds = append(wb.builds, resultWithBridge{res: res, bridge: wb})
		wb.mu.Unlock()
		b.mu.Lock()
		b.subBridges = append(b.subBridges, wb)
		b.mu.Unlock()
	} else {
		return &frontend.Result{}, nil
	}
	if req.Evaluate {
		err = res.EachRef(func(ref solver.ResultProxy) error {
			_, err := ref.Result(ctx)
			return err
		})
	}
	if err == nil {
		err = b.registerProvenanceRefs(res)
	}
	return
}

type resultRequests struct {
	ref       *resultWithBridge
	refs      map[string]*resultWithBridge
	atts      map[string][]*resultWithBridge
	platforms []exptypes.Platform
}

// filterImagePlatforms filter out images that not for the current platform if an image exists for every platform in a result
func (reqs *resultRequests) filterImagePlatforms(k string, imgs []provenancetypes.ImageSource) []provenancetypes.ImageSource {
	if len(reqs.platforms) == 0 {
		return imgs
	}
	m := map[string]string{}
	for _, img := range imgs {
		if _, ok := m[img.Ref]; ok {
			continue
		}
		hasPlatform := true
		for _, p := range reqs.platforms {
			matcher := platforms.NewMatcher(p.Platform)
			found := false
			for _, img2 := range imgs {
				if img.Ref == img2.Ref && img2.Platform != nil {
					if matcher.Match(*img2.Platform) {
						found = true
						break
					}
				}
			}
			if !found {
				hasPlatform = false
				break
			}
		}
		if hasPlatform {
			m[img.Ref] = img.Ref
		}
	}

	var current ocispecs.Platform
	for _, p := range reqs.platforms {
		if p.ID == k {
			current = p.Platform
		}
	}

	out := make([]provenancetypes.ImageSource, 0, len(imgs))
	for _, img := range imgs {
		if _, ok := m[img.Ref]; ok && img.Platform != nil {
			if current.OS == img.Platform.OS && current.Architecture == img.Platform.Architecture {
				out = append(out, img)
			}
		} else {
			out = append(out, img)
		}
	}
	return out
}

func (reqs *resultRequests) allRes() map[string]struct{} {
	res := make(map[string]struct{})
	if reqs.ref != nil {
		res[reqs.ref.res.Ref.ID()] = struct{}{}
	}
	for _, r := range reqs.refs {
		res[r.res.Ref.ID()] = struct{}{}
	}
	for _, rs := range reqs.atts {
		for _, r := range rs {
			res[r.res.Ref.ID()] = struct{}{}
		}
	}
	return res
}

func captureProvenance(ctx context.Context, res solver.CachedResultWithProvenance) (*provenance.Capture, error) {
	if res == nil {
		return nil, nil
	}
	c := &provenance.Capture{}

	err := res.WalkProvenance(ctx, func(pp solver.ProvenanceProvider) error {
		switch op := pp.(type) {
		case *ops.SourceOp:
			id, pin := op.Pin()
			err := id.Capture(c, pin)
			if err != nil {
				return err
			}
		case *ops.ExecOp:
			pr := op.Proto()
			for _, m := range pr.Mounts {
				if m.MountType == pb.MountType_SECRET {
					c.AddSecret(provenancetypes.Secret{
						ID:       m.SecretOpt.GetID(),
						Optional: m.SecretOpt.GetOptional(),
					})
				}
				if m.MountType == pb.MountType_SSH {
					c.AddSSH(provenancetypes.SSH{
						ID:       m.SSHOpt.GetID(),
						Optional: m.SSHOpt.GetOptional(),
					})
				}
			}
			for _, se := range pr.Secretenv {
				c.AddSecret(provenancetypes.Secret{
					ID:       se.GetID(),
					Optional: se.GetOptional(),
				})
			}
			if pr.Network != pb.NetMode_NONE {
				c.NetworkAccess = true
			}
			if op.ProxyNetwork() {
				c.ProxyNetwork = true
				proxyCap := op.ProxyCapture()
				if proxyCap != nil {
					for _, m := range proxyCap.Materials() {
						c.AddHTTP(provenancetypes.HTTPSource{
							URL:    m.URL,
							Digest: m.Digest,
						})
					}
					for _, in := range proxyCap.Incomplete() {
						c.IncompleteMaterials = true
						c.ProxyIncomplete = append(c.ProxyIncomplete, provenancetypes.ProxyCaptureIncomplete{
							Op:     op.Digest().String(),
							Name:   strings.Join(pr.Meta.Args, " "),
							Method: in.Method,
							URI:    in.URL,
							Reason: in.Reason,
						})
					}
				}
			}
			samples, err := op.Samples()
			if err != nil {
				return err
			}
			if samples != nil {
				c.AddSamples(op.Digest(), samples)
			}
		case *ops.BuildOp:
			c.IncompleteMaterials = true // not supported yet
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, err
}

type ProvenanceCreator struct {
	mu          sync.Mutex // serializes predicate updates and encoding of pr
	pr          *provenancetypes.ProvenancePredicateSLSA1
	slsaVersion provenancetypes.ProvenanceSLSA
	j           *solver.Job
	sampler     *resources.SysSampler
	addLayers   func(context.Context) error
}

func NewProvenanceCreator(ctx context.Context, slsaVersion provenancetypes.ProvenanceSLSA, cp *provenance.Capture, res solver.ResultProxy, attrs map[string]string, j *solver.Job, usage *resources.SysSampler, customEnv map[string]any) (*ProvenanceCreator, error) {
	if slsaVersion == "" {
		slsaVersion = provenancetypes.ProvenanceSLSA1
	}

	var reproducible bool
	if v, ok := attrs["reproducible"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse reproducible flag %q", v)
		}
		reproducible = b
	}

	mode := "max"
	if v, ok := attrs["mode"]; ok {
		switch v {
		case "full":
			mode = "max"
		case "max", "min":
			mode = v
		default:
			return nil, errors.Errorf("invalid mode %q", v)
		}
	}

	withUsage := false
	if v, ok := attrs["capture-usage"]; ok {
		b, err := strconv.ParseBool(v)
		withUsage = err == nil && b
	}
	completeMaterials := false
	if v, ok := attrs["complete-materials"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse complete-materials flag %q", v)
		}
		completeMaterials = b
	}

	pr, err := provenance.NewPredicate(cp)
	if err != nil {
		return nil, err
	}
	if pr.RunDetails.Metadata == nil {
		pr.RunDetails.Metadata = &provenancetypes.ProvenanceMetadataSLSA1{}
	}
	if completeMaterials && !pr.RunDetails.Metadata.Completeness.ResolvedDependencies {
		return nil, incompleteMaterialsError(cp)
	}

	st := j.StartedTime()

	pr.RunDetails.Metadata.StartedOn = &st
	pr.RunDetails.Metadata.Reproducible = reproducible
	pr.RunDetails.Metadata.InvocationID = j.UniqueID()

	pr.RunDetails.Builder.ID = attrs["builder-id"]

	var addLayers func(context.Context) error

	switch mode {
	case "min":
		if scrubMinRequest(&pr.BuildDefinition.ExternalParameters.Request) {
			pr.RunDetails.Metadata.Completeness.Request = false
		}
	case "max":
		dgsts, err := provenance.AddBuildConfig(ctx, pr, cp, res, withUsage)
		if err != nil {
			return nil, err
		}

		r, err := res.Result(ctx)
		if err != nil {
			return nil, err
		}

		wref, ok := r.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid worker ref %T", r.Sys())
		}

		addLayers = func(ctx context.Context) error {
			e := newCacheExporter()

			if wref.ImmutableRef != nil {
				ctx = withDescHandlerCacheOpts(ctx, wref.ImmutableRef)
			}

			if _, err := r.CacheKeys()[0].Exporter.ExportTo(ctx, e, solver.CacheExportOpt{
				ResolveRemotes:  resolveRemotes,
				Mode:            solver.CacheExportModeRemoteOnly,
				ExportRoots:     true,
				IgnoreBacklinks: true,
			}); err != nil {
				return err
			}

			m := map[string][][]ocispecs.Descriptor{}

			for l, descs := range e.layers {
				idx, ok := dgsts[l.digest]
				if !ok {
					continue
				}

				m[fmt.Sprintf("step%d:%d", idx, l.index)] = descs
			}

			if len(m) != 0 {
				if pr.RunDetails.Metadata == nil {
					pr.RunDetails.Metadata = &provenancetypes.ProvenanceMetadataSLSA1{}
				}
				pr.RunDetails.Metadata.BuildKitMetadata.Layers = m
			}

			return nil
		}
	default:
		return nil, errors.Errorf("invalid mode %q", mode)
	}

	pr.BuildDefinition.InternalParameters.ProvenanceCustomEnv = customEnv
	if pr.BuildDefinition.ExternalParameters.Request.Frontend == "dockerfile.v0" {
		if dockerfileVersion := dockerfileversion.Version(); dockerfileVersion != "" {
			pr.BuildDefinition.InternalParameters.DockerfileVersion = dockerfileVersion
		}
	}

	pc := &ProvenanceCreator{
		pr:          pr,
		slsaVersion: slsaVersion,
		j:           j,
		addLayers:   addLayers,
	}
	if withUsage {
		pc.sampler = usage
	}
	return pc, nil
}

func scrubMinRequest(req *provenancetypes.Parameters) bool {
	if req == nil {
		return false
	}

	var incomplete bool
	if len(req.Args) > 0 {
		args := make(map[string]string, len(req.Args))
		for k, v := range req.Args {
			if strings.HasPrefix(k, "build-arg:") || strings.HasPrefix(k, "label:") {
				incomplete = true
				continue
			}
			args[k] = v
		}
		req.Args = args
	}
	if len(req.Secrets) > 0 {
		req.Secrets = nil
	}
	if len(req.SSH) > 0 {
		req.SSH = nil
	}

	for _, in := range req.Inputs {
		if in != nil && scrubMinRequest(in.Request) {
			incomplete = true
		}
	}
	if req.Root != nil && scrubMinRequest(req.Root.Request) {
		incomplete = true
	}
	return incomplete
}

func incompleteMaterialsError(c *provenance.Capture) error {
	var b strings.Builder
	b.WriteString("provenance materials are incomplete\n\n")
	b.WriteString("The build requested complete provenance materials, but not all dependencies could be captured.\n\n")
	details := make([]*errdefs.ProvenanceMaterialIncomplete, 0, len(c.ProxyIncomplete)+len(c.Sources.Local))
	if len(c.ProxyIncomplete) == 0 && len(c.Sources.Local) == 0 {
		return errdefs.WithProvenanceMaterialsIncomplete(errors.New(strings.TrimSpace(b.String())), details)
	}

	if len(c.Sources.Local) > 0 {
		b.WriteString("Uncaptured local sources:")
		for _, l := range c.Sources.Local {
			details = append(details, &errdefs.ProvenanceMaterialIncomplete{
				Name:   l.Name,
				Reason: "local_source",
			})
			b.WriteString("\n  - ")
			if l.Name != "" {
				b.WriteString(l.Name)
			} else {
				b.WriteString("<unnamed>")
			}
			b.WriteString("\n    reason: local_source")
		}
	}

	if len(c.ProxyIncomplete) > 0 {
		if len(c.Sources.Local) > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Uncaptured requests:")
	}
	for _, in := range c.ProxyIncomplete {
		details = append(details, &errdefs.ProvenanceMaterialIncomplete{
			Op:       in.Op,
			Name:     in.Name,
			Method:   in.Method,
			Uri:      in.URI,
			FinalUri: in.FinalURI,
			Reason:   in.Reason,
		})
		b.WriteString("\n  - ")
		if in.Name != "" {
			b.WriteString(in.Name)
		} else if in.Op != "" {
			b.WriteString(in.Op)
		} else {
			b.WriteString(in.URI)
		}
		if in.Method != "" {
			b.WriteString("\n    method: ")
			b.WriteString(in.Method)
		}
		if in.URI != "" {
			b.WriteString("\n    url: ")
			b.WriteString(in.URI)
		}
		if in.FinalURI != "" {
			b.WriteString("\n    finalUrl: ")
			b.WriteString(in.FinalURI)
		}
		if in.Reason != "" {
			b.WriteString("\n    reason: ")
			b.WriteString(in.Reason)
		}
	}
	return errdefs.WithProvenanceMaterialsIncomplete(errors.New(b.String()), details)
}

func (p *ProvenanceCreator) PredicateType() string {
	if p.slsaVersion == provenancetypes.ProvenanceSLSA02 {
		return slsa02.PredicateSLSAProvenance
	}
	return slsa1.PredicateSLSAProvenance
}

// PredicateJSON finalizes the predicate and returns it JSON encoded. It may be
// called concurrently, e.g. by parallel exporters sharing one attestation.
func (p *ProvenanceCreator) PredicateJSON(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pr, err := p.predicate(ctx)
	if err != nil {
		return nil, err
	}
	dt, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return dt, nil
}

func (p *ProvenanceCreator) predicate(ctx context.Context) (any, error) {
	end := p.j.RegisterCompleteTime()
	if p.pr.RunDetails.Metadata == nil {
		p.pr.RunDetails.Metadata = &provenancetypes.ProvenanceMetadataSLSA1{}
	}
	p.pr.RunDetails.Metadata.FinishedOn = &end

	if p.addLayers != nil {
		if err := p.addLayers(ctx); err != nil {
			return nil, err
		}
	}

	if p.sampler != nil {
		sysSamples, err := p.sampler.Close(true)
		if err != nil {
			return nil, err
		}
		p.pr.RunDetails.Metadata.BuildKitMetadata.SysUsage = sysSamples
	}

	compatibilityVersion, err := p.j.CompatibilityVersion()
	if err != nil {
		return nil, err
	}
	p.pr.BuildDefinition.ExternalParameters.Request.CompatibilityVersion = compatibilityVersion

	if p.slsaVersion == provenancetypes.ProvenanceSLSA02 {
		return p.pr.ConvertToSLSA02(), nil
	}

	return p.pr, nil
}

type edge struct {
	digest digest.Digest
	index  int
}

func newCacheExporter() *cacheExporter {
	return &cacheExporter{
		m:      map[any]struct{}{},
		layers: map[edge][][]ocispecs.Descriptor{},
	}
}

type cacheExporter struct {
	layers map[edge][][]ocispecs.Descriptor
	m      map[any]struct{}
}

func (ce *cacheExporter) Add(dgst digest.Digest, deps [][]solver.CacheLink, results []solver.CacheExportResult) (solver.CacheExporterRecord, bool, error) {
	for _, res := range results {
		if res.EdgeVertex == "" {
			continue
		}
		e := edge{
			digest: res.EdgeVertex,
			index:  int(res.EdgeIndex),
		}
		descs := make([]ocispecs.Descriptor, len(res.Result.Descriptors))
		for i, desc := range res.Result.Descriptors {
			d := desc
			d.Annotations = containerimage.RemoveInternalLayerAnnotations(d.Annotations, true)
			descs[i] = d
		}
		ce.layers[e] = appendLayerChain(ce.layers[e], descs)
	}
	return &cacheRecord{}, true, nil
}

type cacheRecord struct {
	solver.CacheExporterRecordBase
}

func resolveRemotes(ctx context.Context, res solver.Result) ([]*solver.Remote, error) {
	ref, ok := res.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid result: %T", res.Sys())
	}

	remotes, err := ref.GetRemotes(ctx, false, config.RefConfig{}, true, nil)
	if err != nil {
		if errors.Is(err, cache.ErrNoBlobs) {
			return nil, nil
		}
		return nil, err
	}
	return remotes, nil
}

// appendLayerChain appends a layer chain to the set of layers while checking for duplicate layer chains.
func appendLayerChain(layers [][]ocispecs.Descriptor, descs []ocispecs.Descriptor) [][]ocispecs.Descriptor {
	for _, layerDescs := range layers {
		if len(layerDescs) != len(descs) {
			continue
		}

		matched := true
		for i, d := range layerDescs {
			if d.Digest != descs[i].Digest {
				matched = false
				break
			}
		}

		if matched {
			return layers
		}
	}
	return append(layers, descs)
}

func addProvenanceToResult(res *frontend.Result, br *provenanceBridge) (*Result, error) {
	if res == nil {
		return nil, nil
	}
	reqs, err := br.requests(res)
	if err != nil {
		return nil, err
	}
	out := &Result{
		Result:     res,
		Provenance: &provenance.Result{},
	}

	if res.Ref != nil {
		cp, err := getProvenance(res.Ref, reqs.ref.bridge, "", reqs)
		if err != nil {
			return nil, err
		}
		out.Provenance.Ref = cp
		if res.Metadata == nil {
			res.Metadata = map[string][]byte{}
		}
	}

	if len(res.Refs) != 0 {
		out.Provenance.Refs = make(map[string]*provenance.Capture, len(res.Refs))
	}
	for k, ref := range res.Refs {
		if ref == nil {
			out.Provenance.Refs[k] = nil
			continue
		}
		cp, err := getProvenance(ref, reqs.refs[k].bridge, k, reqs)
		if err != nil {
			return nil, err
		}
		out.Provenance.Refs[k] = cp
		if res.Metadata == nil {
			res.Metadata = map[string][]byte{}
		}
	}

	if len(res.Attestations) != 0 {
		out.Provenance.Attestations = make(map[string][]result.Attestation[*provenance.Capture], len(res.Attestations))
	}
	for k, as := range res.Attestations {
		for i, a := range as {
			a2, err := result.ConvertAttestation(&a, func(r solver.ResultProxy) (*provenance.Capture, error) {
				return getProvenance(r, reqs.atts[k][i].bridge, k, reqs)
			})
			if err != nil {
				return nil, err
			}
			out.Provenance.Attestations[k] = append(out.Provenance.Attestations[k], *a2)
		}
	}

	return out, nil
}

func getRefProvenance(ref solver.ResultProxy, br *provenanceBridge) (*provenance.Capture, error) {
	if ref == nil {
		return nil, nil
	}
	p := ref.Provenance()
	if p == nil {
		return nil, nil
	}

	pr, ok := p.(*provenance.Capture)
	if !ok {
		return nil, errors.Errorf("invalid provenance type %T", p)
	}
	pr = pr.Clone()

	if br.req != nil {
		if pr == nil {
			return nil, errors.Errorf("missing provenance for %s", ref.ID())
		}

		pr.Request.Frontend = br.req.Frontend
		pr.Request.Args = provenance.FilterArgs(br.req.FrontendOpt)
		pr.Request.Inputs = br.inputProvenance(br.req.FrontendInputs)
		if root := br.rootRequestProvenance(pr.Sources); root != nil {
			req := provenance.RequestProvenance(pr.Request.Frontend, pr.Request.Args, pr.Sources)
			if req.Request == nil {
				req.Request = &provenancetypes.Parameters{}
			}
			req.Request.Inputs = pr.Request.Inputs
			if !root.Equal(req) {
				pr.Request.Root = root
			}
		}
		// TODO: should also save some output options like compression
	}

	return pr, nil
}

func getProvenance(ref solver.ResultProxy, br *provenanceBridge, id string, reqs *resultRequests) (*provenance.Capture, error) {
	pr, err := getRefProvenance(ref, br)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}

	visited := reqs.allRes()
	visited[ref.ID()] = struct{}{}
	// provenance for all the refs not directly in the result needs to be captured as well
	if err := br.eachRef(func(r solver.ResultProxy) error {
		if _, ok := visited[r.ID()]; ok {
			return nil
		}
		visited[r.ID()] = struct{}{}
		pr2, err := getRefProvenance(r, br)
		if err != nil {
			return err
		}
		return pr.Merge(pr2)
	}); err != nil {
		return nil, err
	}

	imgs := br.allImages()
	if id != "" {
		imgs = reqs.filterImagePlatforms(id, imgs)
	}
	for _, img := range imgs {
		pr.AddImage(img)
	}

	if err := pr.OptimizeImageSources(); err != nil {
		return nil, err
	}
	pr.Sort()
	// The platform belongs to the final result, not to any merged input capture.
	for _, p := range reqs.platforms {
		if p.ID == id || (id == "" && len(reqs.platforms) == 1) {
			pr.TargetPlatform = platforms.FormatAll(p.Platform)
			break
		}
	}

	return pr, nil
}

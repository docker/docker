package builder

import (
	"context"
	"maps"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/attestations/sbom"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/frontend/gateway/client"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/frontend/subrequests/convertllb"
	"github.com/moby/buildkit/frontend/subrequests/lint"
	"github.com/moby/buildkit/frontend/subrequests/outline"
	"github.com/moby/buildkit/frontend/subrequests/targets"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/util/bkmaps"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	// Don't forget to update frontend documentation if you add
	// a new build-arg: frontend/dockerfile/docs/reference.md
	keySyntaxArg = "build-arg:BUILDKIT_SYNTAX"
)

func Build(ctx context.Context, c client.Client) (_ *client.Result, err error) {
	c = newWithResolveCache(c)
	bc, err := dockerui.NewClient(c)
	if err != nil {
		return nil, err
	}
	opts := bc.BuildOpts().Opts
	allowForward, capsError := validateCaps(opts["frontend.caps"])
	if !allowForward && capsError != nil {
		return nil, capsError
	}

	src, err := bc.ReadEntrypoint(ctx, "Dockerfile")
	if err != nil {
		return nil, err
	}

	if _, ok := opts["cmdline"]; !ok {
		if cmdline, ok := opts[keySyntaxArg]; ok {
			p := strings.SplitN(strings.TrimSpace(cmdline), " ", 2)
			res, err := forwardGateway(ctx, c, p[0], cmdline)
			if err != nil && len(errdefs.Sources(err)) == 0 {
				return nil, errors.Wrapf(err, "failed with %s = %s", keySyntaxArg, cmdline)
			}
			return res, err
		} else if ref, cmdline, loc, ok := parser.DetectSyntax(src.Data); ok {
			res, err := forwardGateway(ctx, c, ref, cmdline)
			if err != nil && len(errdefs.Sources(err)) == 0 {
				return nil, wrapSource(err, src.SourceMap, loc)
			}
			return res, err
		}
	}

	if capsError != nil {
		return nil, capsError
	}

	convertOpt := dockerfile2llb.ConvertOpt{
		Config:       bc.Config,
		Client:       bc,
		SourceMap:    src.SourceMap,
		MetaResolver: c,
		Warn: func(rulename, description, url, msg string, location []parser.Range) {
			startLine := 0
			if len(location) > 0 {
				startLine = location[0].Start.Line
			}
			msg = linter.LintFormatShort(rulename, msg, startLine)
			src.Warn(ctx, msg, warnOpts(location, [][]byte{[]byte(description)}, url))
		},
	}

	if res, ok, err := bc.HandleSubrequest(ctx, dockerui.RequestHandler{
		Outline: func(ctx context.Context) (*outline.Outline, error) {
			return dockerfile2llb.Dockerfile2Outline(ctx, src.Data, convertOpt)
		},
		ListTargets: func(ctx context.Context) (*targets.List, error) {
			return dockerfile2llb.ListTargets(ctx, src.Data)
		},
		Lint: func(ctx context.Context) (*lint.LintResults, error) {
			return dockerfile2llb.DockerfileLint(ctx, src.Data, convertOpt)
		},
		ConvertLLB: func(ctx context.Context) (*convertllb.Result, error) {
			return dockerfile2llb.DockerfileConvertLLB(ctx, src.Data, convertOpt)
		},
	}); err != nil {
		return nil, err
	} else if ok {
		return res, nil
	}

	defer func() {
		var el *parser.LocationError
		if errors.As(err, &el) {
			for _, l := range el.Locations {
				err = wrapSource(err, src.SourceMap, l)
			}
		}
	}()

	var scanner sbom.Scanner
	if bc.SBOM != nil {
		// TODO: scanner should pass policy
		scannerPlatform := bc.BuildPlatforms[0]
		scanner, err = sbom.CreateSBOMScanner(ctx, c, bc.SBOM.Generator, scannerPlatform, sourceresolver.Opt{
			ImageOpt: &sourceresolver.ResolveImageOpt{
				ResolveMode: opts["image-resolve-mode"],
				Platform:    &scannerPlatform,
			},
		}, bc.SBOM.Parameters)
		if err != nil {
			return nil, err
		}
	}

	scanTargets := bkmaps.SyncMap[string, *dockerfile2llb.Result]{}

	rb, err := bc.Build(ctx, func(ctx context.Context, platform *ocispecs.Platform, idx int) (*dockerui.BuildResult, error) {
		opt := convertOpt
		opt.BuildArgs = maps.Clone(opt.BuildArgs)
		opt.TargetPlatform = platform
		if idx != 0 {
			opt.Warn = nil
		}

		dfRes, err := dockerfile2llb.Dockerfile2LLB(ctx, src.Data, opt)
		if err != nil {
			return nil, err
		}

		def, err := dfRes.State.Marshal(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal LLB definition")
		}

		r, err := c.Solve(ctx, client.SolveRequest{
			Definition:   def.ToPB(),
			CacheImports: bc.CacheImports,
		})
		if err != nil {
			return nil, err
		}

		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}

		var p ocispecs.Platform
		if platform != nil {
			p = *platform
		} else {
			p = platforms.DefaultSpec()
		}
		id := platforms.FormatAll(platforms.Normalize(p))
		scanTargets.Store(id, dfRes)
		return &dockerui.BuildResult{
			Reference: ref,
			Image:     dfRes.Image,
			BaseImage: dfRes.BaseImage,
			Epoch:     dfRes.Epoch,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	if scanner != nil {
		if err := rb.EachPlatform(ctx, func(ctx context.Context, id string, p ocispecs.Platform) error {
			target, ok := scanTargets.Load(id)
			if !ok {
				return errors.Errorf("no scan targets for %s", id)
			}

			var opts []llb.ConstraintsOpt
			if target.IsIgnoreCache {
				opts = append(opts, llb.IgnoreCache)
			}
			att, err := scanner(ctx, id, target.SBOM.Core, target.SBOM.Extras, opts...)
			if err != nil {
				return err
			}

			attSolve, err := result.ConvertAttestation(&att, func(st *llb.State) (client.Reference, error) {
				def, err := st.Marshal(ctx)
				if err != nil {
					return nil, err
				}
				r, err := c.Solve(ctx, frontend.SolveRequest{
					Definition: def.ToPB(),
				})
				if err != nil {
					return nil, err
				}
				return r.Ref, nil
			})
			if err != nil {
				return err
			}
			rb.AddAttestation(id, *attSolve)
			return nil
		}); err != nil {
			return nil, err
		}
	}

	return rb.Finalize()
}

func forwardGateway(ctx context.Context, c client.Client, ref string, cmdline string) (*client.Result, error) {
	opts := c.BuildOpts().Opts
	if opts == nil {
		opts = map[string]string{}
	}
	opts["cmdline"] = cmdline
	opts["source"] = ref

	gwcaps := c.BuildOpts().Caps
	var frontendInputs map[string]*pb.Definition
	if (&gwcaps).Supports(gwpb.CapFrontendInputs) == nil {
		inputs, err := c.Inputs(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get frontend inputs")
		}

		frontendInputs = make(map[string]*pb.Definition)
		for name, state := range inputs {
			def, err := state.Marshal(ctx)
			if err != nil {
				return nil, err
			}
			frontendInputs[name] = def.ToPB()
		}
	}

	return c.Solve(ctx, client.SolveRequest{
		Frontend:       "gateway.v0",
		FrontendOpt:    opts,
		FrontendInputs: frontendInputs,
	})
}

func warnOpts(r []parser.Range, detail [][]byte, url string) client.WarnOpts {
	opts := client.WarnOpts{Level: 1, Detail: detail, URL: url}
	if r == nil {
		return opts
	}
	opts.Range = []*pb.Range{}
	for _, r := range r {
		opts.Range = append(opts.Range, &pb.Range{
			Start: &pb.Position{
				Line:      int32(r.Start.Line),
				Character: int32(r.Start.Character),
			},
			End: &pb.Position{
				Line:      int32(r.End.Line),
				Character: int32(r.End.Character),
			},
		})
	}
	return opts
}

func wrapSource(err error, sm *llb.SourceMap, ranges []parser.Range) error {
	if sm == nil {
		return err
	}
	s := &errdefs.Source{
		Info: &pb.SourceInfo{
			Data:       sm.Data,
			Filename:   sm.Filename,
			Language:   sm.Language,
			Definition: sm.Definition.ToPB(),
		},
		Ranges: make([]*pb.Range, 0, len(ranges)),
	}
	for _, r := range ranges {
		s.Ranges = append(s.Ranges, &pb.Range{
			Start: &pb.Position{
				Line:      int32(r.Start.Line),
				Character: int32(r.Start.Character),
			},
			End: &pb.Position{
				Line:      int32(r.End.Line),
				Character: int32(r.End.Character),
			},
		})
	}
	return errdefs.WithSource(err, s)
}

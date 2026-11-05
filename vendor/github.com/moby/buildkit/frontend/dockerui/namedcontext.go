package dockerui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/imageutil"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/pkg/errors"
)

const (
	contextPrefix       = "context:"
	inputMetadataPrefix = "input-metadata:"
	maxContextRecursion = 10
)

type NamedContext struct {
	input            string
	bc               *Client
	name             string
	nameWithPlatform string
	sharedKey        string
	opt              ContextOpt
}

func (bc *Client) namedContext(name string, nameWithPlatform string, opt ContextOpt) (*NamedContext, error) {
	opts := bc.bopts.Opts
	contextKey := contextPrefix + nameWithPlatform
	v, ok := opts[contextKey]
	if !ok {
		return nil, nil
	}
	sharedKey := opts["sharedkey:localdir:"+nameWithPlatform]

	return &NamedContext{
		input:            v,
		bc:               bc,
		name:             name,
		nameWithPlatform: nameWithPlatform,
		sharedKey:        sharedKey,
		opt:              opt,
	}, nil
}

func (nc *NamedContext) Load(ctx context.Context) (*llb.State, *dockerspec.DockerOCIImage, error) {
	return nc.load(ctx, 0)
}

func (nc *NamedContext) load(ctx context.Context, count int) (*llb.State, *dockerspec.DockerOCIImage, error) {
	opt := nc.opt
	if count > maxContextRecursion {
		return nil, nil, errors.New("context recursion limit exceeded; this may indicate a cycle in the provided source policies: " + nc.input)
	}

	vv := strings.SplitN(nc.input, ":", 2)
	if len(vv) != 2 {
		return nil, nil, errors.Errorf("invalid context specifier %s for %s", nc.input, nc.nameWithPlatform)
	}

	// allow git@ without protocol for SSH URLs for backwards compatibility
	if strings.HasPrefix(vv[0], "git@") {
		vv[0] = "git"
	}

	switch vv[0] {
	case "docker-image":
		ref := strings.TrimPrefix(vv[1], "//")
		if ref == EmptyImageName {
			st := llb.Scratch()
			return &st, nil, nil
		}

		imgOpt := []llb.ImageOption{
			llb.WithCustomName("[context " + nc.nameWithPlatform + "] " + ref),
		}
		if opt.Platform != nil {
			imgOpt = append(imgOpt, llb.Platform(*opt.Platform))
		}

		named, err := reference.ParseNormalizedNamed(ref)
		if err != nil {
			return nil, nil, err
		}

		named = reference.TagNameOnly(named)

		ref, dgst, data, err := nc.bc.client.ResolveImageConfig(ctx, named.String(), sourceresolver.Opt{
			LogName: fmt.Sprintf("[context %s] load metadata for %s", nc.nameWithPlatform, ref),
			ImageOpt: &sourceresolver.ResolveImageOpt{
				Platform:    opt.Platform,
				ResolveMode: opt.ResolveMode,
			},
		})
		if err != nil {
			e := &imageutil.ResolveToNonImageError{}
			if errors.As(err, &e) {
				before, after, ok := strings.Cut(e.Updated, "://")
				if !ok {
					return nil, nil, errors.Errorf("could not parse ref: %s", e.Updated)
				}

				nc.bc.bopts.Opts[contextPrefix+nc.nameWithPlatform] = before + ":" + after

				ncnew, err := nc.bc.namedContext(nc.name, nc.nameWithPlatform, nc.opt)
				if err != nil {
					return nil, nil, err
				}
				if ncnew == nil {
					return nil, nil, nil
				}
				return ncnew.load(ctx, count+1)
			}
			return nil, nil, err
		}

		var img dockerspec.DockerOCIImage
		if err := json.Unmarshal(data, &img); err != nil {
			return nil, nil, err
		}
		img.Created = nil

		st := llb.Image(ref, imgOpt...)
		st, err = st.WithImageConfig(data)
		if err != nil {
			return nil, nil, err
		}
		if opt.CaptureDigest != nil {
			*opt.CaptureDigest = dgst
		}
		return &st, &img, nil
	case "git":
		st, ok, err := DetectGitContext(nc.input, nil, gitSourceOpts(nc.bc.GitAdvice, nc.bc.GitDebugCommands)...)
		if !ok {
			return nil, nil, errors.Errorf("invalid git context %s", nc.input)
		}
		if err != nil {
			return nil, nil, err
		}
		return st, nil, nil
	case "http", "https":
		st, ok, err := DetectGitContext(nc.input, nil, gitSourceOpts(nc.bc.GitAdvice, nc.bc.GitDebugCommands)...)
		if ok {
			if err != nil {
				return nil, nil, err
			}
			return st, nil, nil
		}
		httpst := llb.HTTP(nc.input, llb.Filename("context"), llb.WithCustomName("[context "+nc.nameWithPlatform+"] "+nc.input))
		return &httpst, nil, nil
	case "oci-layout":
		refSpec := strings.TrimPrefix(vv[1], "//")
		ref, err := reference.Parse(refSpec)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "could not parse oci-layout reference %q", refSpec)
		}
		named, ok := ref.(reference.Named)
		if !ok {
			return nil, nil, errors.Errorf("oci-layout reference %q has no name", ref.String())
		}
		dgstd, ok := named.(reference.Digested)
		if !ok {
			return nil, nil, errors.Errorf("oci-layout reference %q has no digest", named.String())
		}

		// for the dummy ref primarily used in log messages, we can use the
		// original name, since the store key may not be significant
		dummyRef, err := reference.ParseNormalizedNamed(nc.name)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "could not parse oci-layout reference %q", nc.name)
		}
		dummyRef, err = reference.WithDigest(dummyRef, dgstd.Digest())
		if err != nil {
			return nil, nil, errors.Wrapf(err, "could not wrap %q with digest", nc.name)
		}

		_, dgst, data, err := nc.bc.client.ResolveImageConfig(ctx, dummyRef.String(), sourceresolver.Opt{
			LogName: fmt.Sprintf("[context %s] load metadata for %s", nc.nameWithPlatform, dummyRef.String()),
			OCILayoutOpt: &sourceresolver.ResolveOCILayoutOpt{
				Platform: opt.Platform,
				Store: sourceresolver.ResolveImageConfigOptStore{
					SessionID: nc.bc.bopts.SessionID,
					StoreID:   named.Name(),
				},
			},
		})
		if err != nil {
			return nil, nil, err
		}

		var img dockerspec.DockerOCIImage
		if err := json.Unmarshal(data, &img); err != nil {
			return nil, nil, errors.Wrap(err, "could not parse oci-layout image config")
		}

		ociOpt := []llb.OCILayoutOption{
			llb.WithCustomName("[context " + nc.nameWithPlatform + "] OCI load from client"),
			llb.OCIStore(nc.bc.bopts.SessionID, named.Name()),
		}
		if opt.Platform != nil {
			ociOpt = append(ociOpt, llb.Platform(*opt.Platform))
		}
		st := llb.OCILayout(
			dummyRef.String(),
			ociOpt...,
		)
		st, err = st.WithImageConfig(data)
		if err != nil {
			return nil, nil, err
		}
		if opt.CaptureDigest != nil {
			*opt.CaptureDigest = dgst
		}
		return &st, &img, nil
	case "local":
		sessionID := nc.bc.bopts.SessionID
		if v, ok := nc.bc.localsSessionIDs[vv[1]]; ok {
			sessionID = v
		}
		st := llb.Local(vv[1],
			llb.SessionID(sessionID),
			llb.FollowPaths([]string{DefaultDockerignoreName}),
			llb.SharedKeyHint("context:"+nc.nameWithPlatform+"-"+DefaultDockerignoreName),
			llb.WithCustomName("[context "+nc.nameWithPlatform+"] load "+DefaultDockerignoreName),
			llb.Differ(llb.DiffNone, false),
		)
		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, nil, err
		}
		res, err := nc.bc.client.Solve(ctx, client.SolveRequest{
			Evaluate:   true,
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, nil, err
		}
		var excludes []string
		if !opt.NoDockerignore {
			// a missing ignore file is not an error, but an oversized one
			// must not be silently skipped
			dt, err := ReadFile(ctx, ref, DefaultDockerignoreName)
			if isFileTooLarge(err) {
				return nil, nil, err
			}
			if len(dt) != 0 {
				excludes, err = ignorefile.ReadAll(bytes.NewBuffer(dt))
				if err != nil {
					return nil, nil, errors.Wrapf(err, "failed parsing %s", DefaultDockerignoreName)
				}
			}
		}

		localOutput := &asyncLocalOutput{
			name:             vv[1],
			nameWithPlatform: nc.nameWithPlatform,
			sessionID:        sessionID,
			sharedKey:        nc.sharedKey,
			excludes:         excludes,
			extraOpts:        opt.AsyncLocalOpts,
		}
		st = llb.NewState(localOutput)
		return &st, nil, nil
	case "input":
		inputs, err := nc.bc.client.Inputs(ctx)
		if err != nil {
			return nil, nil, err
		}
		st, ok := inputs[vv[1]]
		if !ok {
			return nil, nil, errors.Errorf("invalid input %s for %s", vv[1], nc.nameWithPlatform)
		}
		md, ok := nc.bc.bopts.Opts[inputMetadataPrefix+vv[1]]
		if ok {
			m := make(map[string][]byte)
			if err := json.Unmarshal([]byte(md), &m); err != nil {
				return nil, nil, errors.Wrapf(err, "failed to parse input metadata %s", md)
			}
			var img *dockerspec.DockerOCIImage
			if dtic, ok := m[exptypes.ExporterImageConfigKey]; ok {
				st, err = st.WithImageConfig(dtic)
				if err != nil {
					return nil, nil, err
				}
				if err := json.Unmarshal(dtic, &img); err != nil {
					return nil, nil, errors.Wrapf(err, "failed to parse image config for %s", nc.nameWithPlatform)
				}
			}
			return &st, img, nil
		}
		return &st, nil, nil
	default:
		return nil, nil, errors.Errorf("unsupported context source %s for %s", vv[0], nc.nameWithPlatform)
	}
}

// asyncLocalOutput is an llb.Output that computes an llb.Local
// on-demand instead of at the time of initialization.
type asyncLocalOutput struct {
	llb.Output
	name             string
	nameWithPlatform string
	sessionID        string
	sharedKey        string
	excludes         []string
	extraOpts        func() []llb.LocalOption
	once             sync.Once
}

func (a *asyncLocalOutput) ToInput(ctx context.Context, constraints *llb.Constraints) (*pb.Input, error) {
	a.once.Do(a.do)
	return a.Output.ToInput(ctx, constraints)
}

func (a *asyncLocalOutput) Vertex(ctx context.Context, constraints *llb.Constraints) llb.Vertex {
	a.once.Do(a.do)
	return a.Output.Vertex(ctx, constraints)
}

func (a *asyncLocalOutput) do() {
	var extraOpts []llb.LocalOption
	if a.extraOpts != nil {
		extraOpts = a.extraOpts()
	}
	sharedKey := "context:" + a.nameWithPlatform + "-" + a.sharedKey
	opts := append([]llb.LocalOption{
		llb.WithCustomName("[context " + a.nameWithPlatform + "] load from client"),
		llb.SessionID(a.sessionID),
		llb.SharedKeyHint(sharedKey),
		llb.ExcludePatterns(a.excludes),
	}, extraOpts...)

	st := llb.Local(a.name, opts...)
	a.Output = st.Output()
}

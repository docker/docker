package dockerui

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerfile/dfgitutil"
	"github.com/moby/buildkit/frontend/gateway/client"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/archiveutil"
	"github.com/moby/buildkit/util/gitutil/gitobject"
	archivecompression "github.com/moby/go-archive/compression"
	"github.com/pkg/errors"
)

const (
	DefaultLocalNameContext    = "context"
	DefaultLocalNameDockerfile = "dockerfile"
	DefaultDockerfileName      = "Dockerfile"
	DefaultDockerignoreName    = ".dockerignore"
	EmptyImageName             = "scratch"
)

const (
	keyFilename       = "filename"
	keyContextSubDir  = "contextsubdir"
	keyNameContext    = "contextkey"
	keyNameDockerfile = "dockerfilekey"
)

var httpPrefix = regexp.MustCompile(`^https?://`)

type buildContext struct {
	context              *llb.State // set if not local
	dockerfile           *llb.State // override remoteContext if set
	contextRef           client.Reference
	contextLocalName     string
	dockerfileLocalName  string
	filename             string
	forceLocalDockerfile bool
	sourceOp             *pb.SourceOp
	httpContextIsArchive bool
	httpContextFilename  string
}

func (bc *Client) marshalOpts() []llb.ConstraintsOpt {
	return []llb.ConstraintsOpt{llb.WithCaps(bc.bopts.Caps)}
}

func (bc *Client) initContext(ctx context.Context) (*buildContext, error) {
	opts := bc.bopts.Opts
	gwcaps := bc.bopts.Caps

	localNameContext := DefaultLocalNameContext
	if v, ok := opts[keyNameContext]; ok {
		localNameContext = v
	}

	bctx := &buildContext{
		contextLocalName:    DefaultLocalNameContext,
		dockerfileLocalName: DefaultLocalNameDockerfile,
		filename:            DefaultDockerfileName,
	}

	if v, ok := opts[keyFilename]; ok {
		bctx.filename = v
	}

	if v, ok := opts[keyNameDockerfile]; ok {
		bctx.forceLocalDockerfile = true
		bctx.dockerfileLocalName = v
	}

	var keepGit *bool
	if v, err := strconv.ParseBool(opts[keyContextKeepGitDirArg]); err == nil {
		keepGit = &v
	}
	var extraGitOpts []llb.GitOption
	if opts[buildArgPrefix+"SOURCE_DATE_EPOCH"] != "" {
		extraGitOpts = append(extraGitOpts, llb.GitMTimeCommit())
	}
	extraGitOpts = append(extraGitOpts, gitSourceOpts(bc.GitAdvice, bc.GitDebugCommands)...)
	if st, ok, err := DetectGitContext(opts[localNameContext], keepGit, extraGitOpts...); ok {
		if err != nil {
			return nil, err
		}
		sourceOp, err := sourceOpFromState(ctx, st, bc.marshalOpts()...)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to derive git source op")
		}
		bctx.context = st
		bctx.dockerfile = st
		bctx.sourceOp = sourceOp
	} else if st, filename, ok := DetectHTTPContext(opts[localNameContext]); ok {
		sourceOp, err := sourceOpFromState(ctx, st, bc.marshalOpts()...)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to derive http source op")
		}
		def, err := st.Marshal(ctx, bc.marshalOpts()...)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal httpcontext")
		}
		res, err := bc.client.Solve(ctx, client.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to resolve httpcontext")
		}

		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}

		dt, err := ref.ReadFile(ctx, client.ReadRequest{
			Filename: filename,
			Range: &client.FileRange{
				Length: 1024,
			},
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read downloaded context")
		}
		if archiveutil.IsArchive(dt) {
			bc := llb.Scratch().File(llb.Copy(*st, filepath.Join("/", filename), "/", &llb.CopyInfo{
				AttemptUnpack: true,
			}))
			bctx.context = &bc
			bctx.httpContextIsArchive = true
		} else {
			bctx.filename = filename
			bctx.context = st
		}
		bctx.contextRef = ref
		bctx.dockerfile = bctx.context
		bctx.sourceOp = sourceOp
		bctx.httpContextFilename = filename
	} else if (&gwcaps).Supports(gwpb.CapFrontendInputs) == nil {
		inputs, err := bc.client.Inputs(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get frontend inputs")
		}

		if !bctx.forceLocalDockerfile {
			inputDockerfile, ok := inputs[bctx.dockerfileLocalName]
			if ok {
				bctx.dockerfile = &inputDockerfile
			}
		}

		inputCtx, ok := inputs[DefaultLocalNameContext]
		if ok {
			bctx.context = &inputCtx
		}
	}

	if bctx.context != nil {
		if sub, ok := opts[keyContextSubDir]; ok {
			bctx.context = scopeToSubDir(bctx.context, sub)
		}
	}

	return bctx, nil
}

func (bc *Client) ResolveMainContextSourceDateEpoch(ctx context.Context) (*time.Time, error) {
	bctx, err := bc.buildContext(ctx)
	if err != nil {
		return nil, err
	}
	if bctx.sourceOp == nil {
		return nil, nil
	}

	opt := sourceresolver.Opt{
		LogName: "[internal] resolve main build context metadata",
	}
	if strings.HasPrefix(bctx.sourceOp.Identifier, "git://") {
		opt.GitOpt = &sourceresolver.ResolveGitOpt{ReturnObject: true}
	}
	md, err := bc.client.ResolveSourceMetadata(ctx, cloneSourceOp(bctx.sourceOp), opt)
	if err != nil {
		return nil, err
	}
	if md.Git != nil && len(md.Git.CommitObject) > 0 {
		obj, err := gitobject.Parse(md.Git.CommitObject)
		if err != nil {
			return nil, err
		}
		commit, err := obj.ToCommit()
		if err != nil {
			return nil, err
		}
		return commit.Committer.When, nil
	}
	if md.HTTP != nil {
		if md.HTTP.LastModified != nil {
			return md.HTTP.LastModified, nil
		}
		if bctx.httpContextIsArchive {
			return archiveMaxTimeFromHTTPArchive(ctx, bctx)
		}
	}
	return nil, nil
}

func archiveMaxTimeFromHTTPArchive(ctx context.Context, bctx *buildContext) (*time.Time, error) {
	if bctx.contextRef == nil || bctx.httpContextFilename == "" {
		return nil, nil
	}
	dt, err := ReadFile(ctx, bctx.contextRef, bctx.httpContextFilename)
	if err != nil {
		return nil, err
	}
	rc, err := archivecompression.DecompressStream(bytes.NewReader(dt))
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	var maxTime *time.Time
	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return maxTime, nil
			}
			return nil, err
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		tm := hdr.ModTime.UTC()
		if maxTime == nil || tm.After(*maxTime) {
			maxTime = &tm
		}
	}
}

func cloneSourceOp(op *pb.SourceOp) *pb.SourceOp {
	if op == nil {
		return nil
	}
	return &pb.SourceOp{
		Identifier: op.Identifier,
		Attrs:      maps.Clone(op.Attrs),
	}
}

func sourceOpFromState(ctx context.Context, st *llb.State, opts ...llb.ConstraintsOpt) (*pb.SourceOp, error) {
	if st == nil {
		return nil, nil
	}
	def, err := st.Marshal(ctx, opts...)
	if err != nil {
		return nil, err
	}
	dt := def.ToPB().Def
	var src *pb.SourceOp
	for _, d := range dt {
		var op pb.Op
		if err := op.Unmarshal(d); err != nil {
			return nil, err
		}
		opSrc := op.GetSource()
		if opSrc == nil {
			continue
		}
		if src != nil {
			return nil, errors.New("state marshaled to multiple source ops")
		}
		src = opSrc
	}
	if src == nil {
		return nil, errors.New("state did not marshal to a source op")
	}
	return cloneSourceOp(src), nil
}

func DetectGitContext(ref string, keepGit *bool, opts ...llb.GitOption) (*llb.State, bool, error) {
	g, isGit, err := dfgitutil.ParseGitRef(ref)
	if err != nil {
		return nil, isGit, err
	}
	gitOpts := slices.Concat(opts, []llb.GitOption{
		llb.GitRef(g.Ref),
		WithInternalName("load git source " + ref),
	})
	if g.KeepGitDir != nil && *g.KeepGitDir {
		gitOpts = append(gitOpts, llb.KeepGitDir())
	}
	if keepGit != nil && *keepGit {
		gitOpts = append(gitOpts, llb.KeepGitDir())
	}
	if g.SubDir != "" {
		gitOpts = append(gitOpts, llb.GitSubDir(g.SubDir))
	}
	if g.Checksum != "" {
		gitOpts = append(gitOpts, llb.GitChecksum(g.Checksum))
	}
	if g.Submodules != nil && !*g.Submodules {
		gitOpts = append(gitOpts, llb.GitSkipSubmodules())
	}
	if g.MTime != "" {
		gitOpts = append(gitOpts, llb.GitMTime(g.MTime))
	}
	if g.FetchByCommit {
		gitOpts = append(gitOpts, llb.GitFetchByCommit())
	}

	st := llb.Git(g.Remote, "", gitOpts...)
	return &st, true, nil
}

func gitSourceOpts(advice, debugCommands bool) []llb.GitOption {
	var opts []llb.GitOption
	if advice {
		opts = append(opts, llb.GitAdvice(true))
	}
	if debugCommands {
		opts = append(opts, llb.GitDebugCommands(true))
	}
	return opts
}

func DetectHTTPContext(ref string) (*llb.State, string, bool) {
	filename := "context"
	if httpPrefix.MatchString(ref) {
		st := llb.HTTP(ref, llb.Filename(filename), WithInternalName("load remote build context"))
		return &st, filename, true
	}
	return nil, "", false
}

func scopeToSubDir(c *llb.State, dir string) *llb.State {
	bc := llb.Scratch().File(llb.Copy(*c, dir, "/", &llb.CopyInfo{
		CopyDirContentsOnly: true,
	}))
	return &bc
}

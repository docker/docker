package dockerfile2llb

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"maps"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/dockerfile/dfgitutil"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/moby/buildkit/frontend/dockerui"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/gitutil/gitobject"
	archivecompression "github.com/moby/go-archive/compression"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type sourceDateEpochStateOpt struct {
	LogName string
}

func resolveSourceDateEpochValue(ctx context.Context, v string, opt ConvertOpt, stages []instructions.Stage, globalArgs *llb.EnvList, shlex *shell.Lex) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if sde, err := strconv.ParseInt(v, 10, 64); err == nil {
		tm := time.Unix(sde, 0).UTC()
		return &tm, nil
	}

	state, stateOpt, err := resolveSourceDateEpochState(ctx, v, opt, stages, globalArgs, shlex)
	if err != nil {
		return nil, err
	}
	if state == nil || opt.Client == nil {
		return nil, nil
	}
	return resolveSourceDateEpochFromState(ctx, *state, opt.Client, stateOpt)
}

func formatSourceDateEpochValue(tm *time.Time) string {
	if tm == nil {
		return ""
	}
	return strconv.FormatInt(tm.Unix(), 10)
}

func resolveSourceDateEpochState(ctx context.Context, value string, opt ConvertOpt, stages []instructions.Stage, globalArgs *llb.EnvList, shlex *shell.Lex) (*llb.State, sourceDateEpochStateOpt, error) {
	if value == "context" {
		if opt.Client == nil {
			return nil, sourceDateEpochStateOpt{}, nil
		}
		mainContextState, err := opt.Client.MainContext(ctx)
		if err != nil {
			return nil, sourceDateEpochStateOpt{}, err
		}
		return mainContextState, sourceDateEpochStateOpt{
			LogName: "[internal] resolve main build context metadata",
		}, nil
	}

	if opt.Client != nil {
		nc, err := opt.Client.NamedContext(value, dockerui.ContextOpt{})
		if err != nil {
			return nil, sourceDateEpochStateOpt{}, err
		}
		if nc != nil {
			st, _, err := nc.Load(ctx)
			if err != nil {
				return nil, sourceDateEpochStateOpt{}, err
			}
			return st, sourceDateEpochStateOpt{
				LogName: "[internal] resolve SOURCE_DATE_EPOCH named context " + value,
			}, nil
		}
	}

	for i := range stages {
		if !strings.EqualFold(stages[i].Name, value) {
			continue
		}

		args := globalArgs
		if globalArgs != nil {
			updated := globalArgs.Delete("SOURCE_DATE_EPOCH")
			args = &updated
		}

		sourceState, err := sourceDateEpochStageSource(stages[i], opt.BuildArgs, args, shlex, opt.GitAdvice, opt.GitDebugCommands)
		if err != nil {
			return nil, sourceDateEpochStateOpt{}, parser.WithLocation(err, stages[i].Location)
		}

		return sourceState, sourceDateEpochStateOpt{
			LogName: "[internal] resolve SOURCE_DATE_EPOCH source stage " + stages[i].Name,
		}, nil
	}
	return nil, sourceDateEpochStateOpt{}, errors.Errorf("invalid SOURCE_DATE_EPOCH: %s", value)
}

func sourceDateEpochStageSource(stage instructions.Stage, buildArgs map[string]string, globalArgs *llb.EnvList, shlex *shell.Lex, gitAdvice, gitDebugCommands bool) (*llb.State, error) {
	stageBaseName, _, err := shlex.ProcessWord(stage.BaseName, globalArgs)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to process source stage base name %q", stage.BaseName)
	}
	if stageBaseName != emptyImageName {
		return nil, errors.New("SOURCE_DATE_EPOCH stage must use FROM scratch")
	}

	env := globalArgs
	var sourceState *llb.State

	for _, cmd := range stage.Commands {
		switch c := cmd.(type) {
		case *instructions.ArgCommand:
			env, err = applySourceDateEpochStageArgs(c.Args, env, buildArgs, shlex)
			if err != nil {
				return nil, err
			}
		case *instructions.AddCommand:
			if sourceState != nil {
				return nil, errors.New("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD")
			}
			sourceState, err = sourceDateEpochAddSource(c, env, shlex, gitAdvice, gitDebugCommands)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.Errorf("SOURCE_DATE_EPOCH stage does not meet source-only requirements: unsupported %s instruction", cmd.Name())
		}
	}

	if sourceState == nil {
		return nil, errors.New("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD")
	}

	return sourceState, nil
}

func applySourceDateEpochStageArgs(args []instructions.KeyValuePairOptional, env *llb.EnvList, buildArgs map[string]string, shlex *shell.Lex) (*llb.EnvList, error) {
	for _, arg := range args {
		if v, ok := buildArgs[arg.Key]; ok {
			env = env.AddOrReplace(arg.Key, v)
			continue
		}
		if arg.Value == nil {
			continue
		}
		v, _, err := shlex.ProcessWord(*arg.Value, env)
		if err != nil {
			return nil, err
		}
		env = env.AddOrReplace(arg.Key, v)
	}
	return env, nil
}

func sourceDateEpochAddSource(cmd *instructions.AddCommand, env *llb.EnvList, shlex *shell.Lex, gitAdvice, gitDebugCommands bool) (*llb.State, error) {
	if len(cmd.SourceContents) != 0 || len(cmd.SourcePaths) != 1 {
		return nil, errors.New("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD source")
	}

	src, _, err := shlex.ProcessWord(cmd.SourcePaths[0], env)
	if err != nil {
		return nil, err
	}

	if isHTTPSource(src) {
		var checksum digest.Digest
		if cmd.Checksum != "" {
			expandedChecksum, _, err := shlex.ProcessWord(cmd.Checksum, env)
			if err != nil {
				return nil, err
			}
			checksum, err = digest.Parse(expandedChecksum)
			if err != nil {
				return nil, err
			}
		}
		st := llb.HTTP(src, llb.Filename(sourceDateEpochHTTPFilename(src)), llb.Checksum(checksum))
		return &st, nil
	}

	gitRef, isGit, gitRefErr := dfgitutil.ParseGitRef(src)
	if gitRefErr != nil && isGit {
		return nil, gitRefErr
	}
	if gitRefErr == nil && !gitRef.IndistinguishableFromLocal {
		gitOptions := []llb.GitOption{
			llb.GitRef(gitRef.Ref),
		}
		if gitAdvice {
			gitOptions = append(gitOptions, llb.GitAdvice(true))
		}
		if gitDebugCommands {
			gitOptions = append(gitOptions, llb.GitDebugCommands(true))
		}
		if cmd.KeepGitDir != nil && *cmd.KeepGitDir {
			gitOptions = append(gitOptions, llb.KeepGitDir())
		}
		if gitRef.KeepGitDir != nil && *gitRef.KeepGitDir {
			gitOptions = append(gitOptions, llb.KeepGitDir())
		}
		if cmd.Checksum != "" {
			expandedChecksum, _, err := shlex.ProcessWord(cmd.Checksum, env)
			if err != nil {
				return nil, err
			}
			gitOptions = append(gitOptions, llb.GitChecksum(expandedChecksum))
		} else if gitRef.Checksum != "" {
			gitOptions = append(gitOptions, llb.GitChecksum(gitRef.Checksum))
		}
		if gitRef.SubDir != "" {
			gitOptions = append(gitOptions, llb.GitSubDir(gitRef.SubDir))
		}
		if gitRef.Submodules != nil && !*gitRef.Submodules {
			gitOptions = append(gitOptions, llb.GitSkipSubmodules())
		}
		st := llb.Git(gitRef.Remote, "", gitOptions...)
		return &st, nil
	}

	return nil, errors.New("SOURCE_DATE_EPOCH stage source must be a single HTTP(S) or Git ADD")
}

func sourceDateEpochHTTPFilename(src string) string {
	u, err := url.Parse(src)
	if err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			return base
		}
	}
	return "__unnamed__"
}

func resolveSourceDateEpochFromState(ctx context.Context, st llb.State, client *dockerui.Client, opt sourceDateEpochStateOpt) (*time.Time, error) {
	sourceOp, err := sourceOpFromState(ctx, &st, llb.WithCaps(client.BuildOpts().Caps))
	if err != nil {
		return nil, err
	}
	if sourceOp == nil {
		return nil, nil
	}

	metaOpt := sourceresolver.Opt{
		LogName: opt.LogName,
	}
	if strings.HasPrefix(sourceOp.Identifier, "git://") {
		metaOpt.GitOpt = &sourceresolver.ResolveGitOpt{ReturnObject: true}
	}
	isHTTP := strings.HasPrefix(sourceOp.Identifier, "http://") || strings.HasPrefix(sourceOp.Identifier, "https://")
	md, err := client.GatewayClient().ResolveSourceMetadata(ctx, sourceOp, metaOpt)
	if err != nil {
		return nil, err
	}
	if tm, ok, err := sourceDateEpochFromMetadata(md); ok || err != nil {
		return tm, err
	}
	filename := sourceOp.Attrs[pb.AttrHTTPFilename]
	if !isHTTP || filename == "" {
		return nil, nil
	}

	sourceState := llb.NewState(llb.NewSource(sourceOp.Identifier, maps.Clone(sourceOp.Attrs), llb.Constraints{}).Output())
	def, err := sourceState.Marshal(ctx, llb.WithCaps(client.BuildOpts().Caps))
	if err != nil {
		return nil, err
	}
	res, err := client.GatewayClient().Solve(ctx, gwclient.SolveRequest{
		Definition: def.ToPB(),
	})
	if err != nil {
		return nil, err
	}
	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}

	return archiveMaxTimeFromRef(ctx, ref, filename, true)
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
			return nil, nil
		}
		src = opSrc
	}
	return cloneSourceOp(src), nil
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

func sourceDateEpochFromMetadata(md *sourceresolver.MetaResponse) (*time.Time, bool, error) {
	if md.Git != nil && len(md.Git.CommitObject) > 0 {
		obj, err := gitobject.Parse(md.Git.CommitObject)
		if err != nil {
			return nil, false, err
		}
		commit, err := obj.ToCommit()
		if err != nil {
			return nil, false, err
		}
		return commit.Committer.When, true, nil
	}
	if md.HTTP != nil && md.HTTP.LastModified != nil {
		return md.HTTP.LastModified, true, nil
	}
	return nil, false, nil
}

func archiveMaxTimeFromRef(ctx context.Context, ref gwclient.Reference, filename string, allowNonArchive bool) (*time.Time, error) {
	dt, err := dockerui.ReadFile(ctx, ref, filename)
	if err != nil {
		return nil, err
	}
	rc, err := archivecompression.DecompressStream(bytes.NewReader(dt))
	if err != nil {
		if allowNonArchive {
			return nil, nil
		}
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
			if allowNonArchive {
				return nil, nil
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

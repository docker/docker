package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"sort"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/remotecontext"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/system"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/syncmap"
)

var validCommitCommands = map[string]bool{
	"cmd":         true,
	"entrypoint":  true,
	"healthcheck": true,
	"env":         true,
	"expose":      true,
	"label":       true,
	"onbuild":     true,
	"user":        true,
	"volume":      true,
	"workdir":     true,
}

const (
	stepFormat = "Step %d/%d : %v"
)

// BuildManager is shared across all Builder objects
type BuildManager struct {
	idMapping     *idtools.IdentityMapping
	backend       builder.Backend
	pathCache     pathCache // TODO: make this persistent
	containerdCli *containerd.Client
}

// NewBuildManager creates a BuildManager
func NewBuildManager(b builder.Backend, identityMapping *idtools.IdentityMapping, containerdCli *containerd.Client) (*BuildManager, error) {
	bm := &BuildManager{
		backend:       b,
		pathCache:     &syncmap.Map{},
		idMapping:     identityMapping,
		containerdCli: containerdCli,
	}
	return bm, nil
}

// Build starts a new build from a BuildConfig
func (bm *BuildManager) Build(ctx context.Context, config backend.BuildConfig) (*builder.Result, error) {
	buildsTriggered.Inc()
	if config.Options.Dockerfile == "" {
		config.Options.Dockerfile = builder.DefaultDockerfileName
	}

	source, dockerfile, err := remotecontext.Detect(config)
	if err != nil {
		return nil, err
	}
	defer func() {
		if source != nil {
			if err := source.Close(); err != nil {
				logrus.Debugf("[BUILDER] failed to remove temporary context: %v", err)
			}
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if config.Options.SessionID != "" {
		return nil, errors.New("experimental session with v1 builder is no longer supported, use builder version v2 (BuildKit) instead")
	}

	builderOptions := builderOptions{
		Options:        config.Options,
		ProgressWriter: config.ProgressWriter,
		Backend:        bm.backend,
		PathCache:      bm.pathCache,
		IDMapping:      bm.idMapping,
		containerdCli:  bm.containerdCli,
	}
	b, err := newBuilder(ctx, builderOptions)
	if err != nil {
		return nil, err
	}
	return b.build(source, dockerfile)
}

// builderOptions are the dependencies required by the builder
type builderOptions struct {
	Options        *types.ImageBuildOptions
	Backend        builder.Backend
	ProgressWriter backend.ProgressWriter
	PathCache      pathCache
	IDMapping      *idtools.IdentityMapping
	containerdCli  *containerd.Client
}

// Builder is a Dockerfile builder
// It implements the builder.Backend interface.
type Builder struct {
	options *types.ImageBuildOptions

	Stdout io.Writer
	Stderr io.Writer
	Aux    *streamformatter.AuxFormatter
	Output io.Writer

	docker        builder.Backend
	containerdCli *containerd.Client
	clientCtx     context.Context

	idMapping        *idtools.IdentityMapping
	disableCommit    bool
	imageSources     *imageSources
	pathCache        pathCache
	containerManager *containerManager
	imageProber      ImageProber
	platform         *specs.Platform
}

// newBuilder creates a new Dockerfile builder from an optional dockerfile and a Options.
func newBuilder(clientCtx context.Context, options builderOptions) (*Builder, error) {
	config := options.Options
	if config == nil {
		config = new(types.ImageBuildOptions)
	}

	b := &Builder{
		clientCtx:        clientCtx,
		options:          config,
		Stdout:           options.ProgressWriter.StdoutFormatter,
		Stderr:           options.ProgressWriter.StderrFormatter,
		Aux:              options.ProgressWriter.AuxFormatter,
		Output:           options.ProgressWriter.Output,
		docker:           options.Backend,
		containerdCli:    options.containerdCli,
		idMapping:        options.IDMapping,
		imageSources:     newImageSources(clientCtx, options),
		pathCache:        options.PathCache,
		imageProber:      newImageProber(options.Backend, config.CacheFrom, config.NoCache),
		containerManager: newContainerManager(options.Backend),
	}

	// same as in Builder.Build in builder/builder-next/builder.go
	// TODO: remove once config.Platform is of type specs.Platform
	if config.Platform != "" {
		sp, err := platforms.Parse(config.Platform)
		if err != nil {
			return nil, err
		}
		if err := system.ValidatePlatform(sp); err != nil {
			return nil, err
		}
		b.platform = &sp
	}

	return b, nil
}

// Build 'LABEL' command(s) from '--label' options and add to the last stage
func buildLabelOptions(labels map[string]string, stages []instructions.Stage) {
	keys := []string{}
	for key := range labels {
		keys = append(keys, key)
	}

	// Sort the label to have a repeatable order
	sort.Strings(keys)
	for _, key := range keys {
		value := labels[key]
		stages[len(stages)-1].AddCommand(instructions.NewLabelCommand(key, value, true))
	}
}

// Build runs the Dockerfile builder by parsing the Dockerfile and executing
// the instructions from the file.
func (b *Builder) build(source builder.Source, dockerfile *parser.Result) (*builder.Result, error) {
	defer b.imageSources.Unmount()

	stages, metaArgs, err := instructions.Parse(dockerfile.AST)
	if err != nil {
		if instructions.IsUnknownInstruction(err) {
			buildsFailed.WithValues(metricsUnknownInstructionError).Inc()
		}
		return nil, errdefs.InvalidParameter(err)
	}
	if b.options.Target != "" {
		targetIx, found := instructions.HasStage(stages, b.options.Target)
		if !found {
			buildsFailed.WithValues(metricsBuildTargetNotReachableError).Inc()
			return nil, errdefs.InvalidParameter(errors.Errorf("failed to reach build target %s in Dockerfile", b.options.Target))
		}
		stages = stages[:targetIx+1]
	}

	// Add 'LABEL' command specified by '--label' option to the last stage
	buildLabelOptions(b.options.Labels, stages)

	dockerfile.PrintWarnings(b.Stderr)
	dispatchState, err := b.dispatchDockerfileWithCancellation(stages, metaArgs, dockerfile.EscapeToken, source)
	if err != nil {
		return nil, err
	}
	if dispatchState.image == nil {
		buildsFailed.WithValues(metricsDockerfileEmptyError).Inc()
		return nil, errors.New("No image was generated. Is your Dockerfile empty?")
	}
	return &builder.Result{Image: *dispatchState.image, FromImage: dispatchState.baseImage}, nil
}

func emitImageID(aux *streamformatter.AuxFormatter, state *dispatchState) error {
	if aux == nil || state.image == nil {
		return nil
	}
	return aux.Emit("", types.BuildResult{ID: state.image.Digest.String()})
}

func processMetaArg(meta instructions.ArgCommand, shlex *shell.Lex, args *BuildArgs) error {
	// shell.Lex currently only support the concatenated string format
	envs := convertMapToEnvList(args.GetAllAllowed())
	if err := meta.Expand(func(word string) (string, error) {
		return shlex.ProcessWord(word, envs)
	}); err != nil {
		return err
	}
	args.AddArg(meta.Key, meta.Value)
	args.AddMetaArg(meta.Key, meta.Value)
	return nil
}

func printCommand(out io.Writer, currentCommandIndex int, totalCommands int, cmd interface{}) int {
	fmt.Fprintf(out, stepFormat, currentCommandIndex, totalCommands, cmd)
	fmt.Fprintln(out)
	return currentCommandIndex + 1
}

func (b *Builder) dispatchDockerfileWithCancellation(parseResult []instructions.Stage, metaArgs []instructions.ArgCommand, escapeToken rune, source builder.Source) (*dispatchState, error) {
	dispatchRequest := dispatchRequest{}
	buildArgs := NewBuildArgs(b.options.BuildArgs)
	totalCommands := len(metaArgs) + len(parseResult)
	currentCommandIndex := 1
	for _, stage := range parseResult {
		totalCommands += len(stage.Commands)
	}
	shlex := shell.NewLex(escapeToken)
	for _, meta := range metaArgs {
		currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, &meta)

		err := processMetaArg(meta, shlex, buildArgs)
		if err != nil {
			return nil, err
		}
	}

	stagesResults := newStagesBuildResults()

	for _, stage := range parseResult {
		if err := stagesResults.checkStageNameAvailable(stage.Name); err != nil {
			return nil, err
		}
		dispatchRequest = newDispatchRequest(b, escapeToken, source, buildArgs, stagesResults)

		currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, stage.SourceCode)
		if err := initializeStage(dispatchRequest, &stage); err != nil {
			return nil, err
		}
		dispatchRequest.state.updateRunConfig()
		fmt.Fprintf(b.Stdout, " ---> %s\n", shortDispatchID(dispatchRequest.state))
		for _, cmd := range stage.Commands {
			select {
			case <-b.clientCtx.Done():
				logrus.Debug("Builder: build cancelled!")
				fmt.Fprint(b.Stdout, "Build cancelled\n")
				buildsFailed.WithValues(metricsBuildCanceled).Inc()
				return nil, errors.New("Build cancelled")
			default:
				// Not cancelled yet, keep going...
			}

			currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, cmd)

			if err := dispatch(dispatchRequest, cmd); err != nil {
				return nil, err
			}
			dispatchRequest.state.updateRunConfig()
			fmt.Fprintf(b.Stdout, " ---> %s\n", shortDispatchID(dispatchRequest.state))

		}
		if err := emitImageID(b.Aux, dispatchRequest.state); err != nil {
			return nil, err
		}
		buildArgs.MergeReferencedArgs(dispatchRequest.state.buildArgs)
		if err := commitStage(dispatchRequest.state, stagesResults); err != nil {
			return nil, err
		}
	}
	buildArgs.WarnOnUnusedBuildArgs(b.Stdout)
	return dispatchRequest.state, nil
}

func shortDispatchID(state *dispatchState) string {
	if state.image == nil {
		return ""
	}
	return state.image.Digest.Encoded()[:12]
}

// BuildFromConfig builds directly from `changes`, treating it as if it were the contents of a Dockerfile
// It will:
// - Call parse.Parse() to get an AST root for the concatenated Dockerfile entries.
// - Do build by calling builder.dispatch() to call all entries' handling routines
//
// BuildFromConfig is used by the /commit endpoint, with the changes
// coming from the query parameter of the same name.
func BuildFromConfig(ctx context.Context, config *container.Config, changes []string) (*container.Config, error) {
	if len(changes) == 0 {
		return config, nil
	}

	dockerfile, err := parser.Parse(bytes.NewBufferString(strings.Join(changes, "\n")))
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	b, err := newBuilder(ctx, builderOptions{
		Options: &types.ImageBuildOptions{NoCache: true},
	})
	if err != nil {
		return nil, err
	}

	// ensure that the commands are valid
	for _, n := range dockerfile.AST.Children {
		if !validCommitCommands[n.Value] {
			return nil, errdefs.InvalidParameter(errors.Errorf("%s is not a valid change command", n.Value))
		}
	}

	b.Stdout = ioutil.Discard
	b.Stderr = ioutil.Discard
	b.disableCommit = true

	var commands []instructions.Command
	for _, n := range dockerfile.AST.Children {
		cmd, err := instructions.ParseCommand(n)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		commands = append(commands, cmd)
	}

	dispatchRequest := newDispatchRequest(b, dockerfile.EscapeToken, nil, NewBuildArgs(b.options.BuildArgs), newStagesBuildResults())
	// We make mutations to the configuration, ensure we have a copy
	dispatchRequest.state.runConfig = copyRunConfig(config)
	for _, cmd := range commands {
		err := dispatch(dispatchRequest, cmd)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		dispatchRequest.state.updateRunConfig()
	}

	return dispatchRequest.state.runConfig, nil
}

func convertMapToEnvList(m map[string]string) []string {
	result := []string{}
	for k, v := range m {
		result = append(result, k+"="+v)
	}
	return result
}

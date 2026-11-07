package dockerfile

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/builder"
	"github.com/moby/moby/v2/daemon/builder/remotecontext"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/sys/user"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
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
	"stopsignal":  true,
	"user":        true,
	"volume":      true,
	"workdir":     true,
}

const (
	stepFormat = "Step %d/%d : %v"
)

// BuildManager is shared across all Builder objects
type BuildManager struct {
	idMapping user.IdentityMapping
	backend   builder.Backend
	pathCache pathCache // TODO: make this persistent
}

// NewBuildManager creates a BuildManager
func NewBuildManager(b builder.Backend, identityMapping user.IdentityMapping) (*BuildManager, error) {
	bm := &BuildManager{
		backend:   b,
		pathCache: &syncmap.Map{},
		idMapping: identityMapping,
	}
	return bm, nil
}

// Build starts a new build from a BuildConfig
func (bm *BuildManager) Build(ctx context.Context, config buildbackend.BuildConfig) (*builder.Result, error) {
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
				log.G(ctx).Debugf("[BUILDER] failed to remove temporary context: %v", err)
			}
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	b, err := newBuilder(ctx, builderOptions{
		Options:        config.Options,
		ProgressWriter: config.ProgressWriter,
		Backend:        bm.backend,
		PathCache:      bm.pathCache,
		IDMapping:      bm.idMapping,
	})
	if err != nil {
		return nil, err
	}
	return b.build(ctx, source, dockerfile)
}

// builderOptions are the dependencies required by the builder
type builderOptions struct {
	Options        *buildbackend.BuildOptions
	Backend        builder.Backend
	ProgressWriter buildbackend.ProgressWriter
	PathCache      pathCache
	IDMapping      user.IdentityMapping
}

// Builder is a Dockerfile builder
// It implements the builder.Backend interface.
type Builder struct {
	options *buildbackend.BuildOptions

	Stdout io.Writer
	Stderr io.Writer
	Aux    buildbackend.AuxEmitter
	Output io.Writer

	docker builder.Backend

	idMapping        user.IdentityMapping
	disableCommit    bool
	imageSources     *imageSources
	pathCache        pathCache
	containerManager *containerManager
	imageProber      ImageProber
	platform         *ocispec.Platform
}

// newBuilder creates a new Dockerfile builder from an optional dockerfile and a Options.
func newBuilder(ctx context.Context, options builderOptions) (*Builder, error) {
	config := options.Options
	if config == nil {
		config = &buildbackend.BuildOptions{}
	}

	imgProber, err := newImageProber(ctx, options.Backend, config.CacheFrom, config.NoCache)
	if err != nil {
		return nil, err
	}

	b := &Builder{
		options:          config,
		Stdout:           options.ProgressWriter.StdoutFormatter,
		Stderr:           options.ProgressWriter.StderrFormatter,
		Aux:              options.ProgressWriter.AuxFormatter,
		Output:           options.ProgressWriter.Output,
		docker:           options.Backend,
		idMapping:        options.IDMapping,
		imageSources:     newImageSources(options),
		pathCache:        options.PathCache,
		imageProber:      imgProber,
		containerManager: newContainerManager(options.Backend),
	}

	// same as in Builder.Build in builder/builder-next/builder.go
	// TODO: remove once config.Platform is of type specs.Platform
	if config.Platform != "" {
		sp, err := platforms.Parse(config.Platform)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		b.platform = &sp
	}

	return b, nil
}

// Build 'LABEL' command(s) from '--label' options and add to the last stage
func buildLabelOptions(labels map[string]string, stages []instructions.Stage) {
	var keys []string
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
func (b *Builder) build(ctx context.Context, source builder.Source, dockerfile *parser.Result) (*builder.Result, error) {
	defer b.imageSources.Unmount()

	stages, metaArgs, err := instructions.Parse(dockerfile.AST, nil)
	if err != nil {
		var uiErr *instructions.UnknownInstructionError
		if errors.As(err, &uiErr) {
			buildsFailed.WithValues(metricsUnknownInstructionError).Inc()
		}
		return nil, errdefs.InvalidParameter(err)
	}
	if b.options.Target != "" {
		targetIx, found := instructions.HasStage(stages, b.options.Target)
		if !found {
			buildsFailed.WithValues(metricsBuildTargetNotReachableError).Inc()
			return nil, errdefs.InvalidParameter(errors.Errorf("target stage %q could not be found", b.options.Target))
		}
		stages = stages[:targetIx+1]
	}

	// Add 'LABEL' command specified by '--label' option to the last stage
	buildLabelOptions(b.options.Labels, stages)

	dockerfile.PrintWarnings(b.Stderr)
	state, err := b.dispatchDockerfileWithCancellation(ctx, stages, metaArgs, dockerfile.EscapeToken, source)
	if err != nil {
		return nil, err
	}
	if state.imageID == "" {
		buildsFailed.WithValues(metricsDockerfileEmptyError).Inc()
		return nil, errors.New("No image was generated. Is your Dockerfile empty?")
	}
	return &builder.Result{ImageID: state.imageID, FromImage: state.baseImage}, nil
}

func emitImageID(aux buildbackend.AuxEmitter, state *dispatchState) error {
	if aux == nil || state.imageID == "" {
		return nil
	}
	return aux.Emit("", build.Result{ID: state.imageID})
}

func injectPredefinedBuildArgs(buildArgs *BuildArgs, targetPlatform *ocispec.Platform) {
	buildSpec := platforms.DefaultSpec()
	if targetPlatform == nil {
		targetPlatform = &buildSpec
	}
	strPtr := func(s string) *string { return &s }
	buildArgs.AddMetaArg("TARGETPLATFORM", strPtr(platforms.Format(*targetPlatform)))
	buildArgs.AddMetaArg("TARGETOS", strPtr(targetPlatform.OS))
	buildArgs.AddMetaArg("TARGETARCH", strPtr(targetPlatform.Architecture))
	buildArgs.AddMetaArg("TARGETVARIANT", strPtr(targetPlatform.Variant))
	buildArgs.AddMetaArg("BUILDPLATFORM", strPtr(platforms.Format(buildSpec)))
	buildArgs.AddMetaArg("BUILDOS", strPtr(buildSpec.OS))
	buildArgs.AddMetaArg("BUILDARCH", strPtr(buildSpec.Architecture))
	buildArgs.AddMetaArg("BUILDVARIANT", strPtr(buildSpec.Variant))
}

func processMetaArg(meta instructions.ArgCommand, shlex *shell.Lex, args *BuildArgs) error {
	// shell.Lex currently only support the concatenated string format
	envs := shell.EnvsFromSlice(convertMapToEnvList(args.GetAllAllowed()))
	if err := meta.Expand(func(word string) (string, error) {
		newword, _, err := shlex.ProcessWord(word, envs)
		return newword, err
	}); err != nil {
		return err
	}
	for _, arg := range meta.Args {
		args.AddArg(arg.Key, arg.Value)
		args.AddMetaArg(arg.Key, arg.Value)
	}
	return nil
}

func printCommand(out io.Writer, currentCommandIndex int, totalCommands int, cmd any) int {
	_, _ = fmt.Fprintf(out, stepFormat, currentCommandIndex, totalCommands, cmd)
	_, _ = fmt.Fprintln(out)
	return currentCommandIndex + 1
}

func (b *Builder) dispatchDockerfileWithCancellation(ctx context.Context, parseResult []instructions.Stage, metaArgs []instructions.ArgCommand, escapeToken rune, source builder.Source) (*dispatchState, error) {
	request := dispatchRequest{}
	buildArgs := NewBuildArgs(b.options.BuildArgs)
	totalCommands := len(metaArgs) + len(parseResult)
	currentCommandIndex := 1
	for _, stage := range parseResult {
		totalCommands += len(stage.Commands)
	}
	injectPredefinedBuildArgs(buildArgs, b.platform)

	shlex := shell.NewLex(escapeToken)
	for i := range metaArgs {
		currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, &metaArgs[i])

		err := processMetaArg(metaArgs[i], shlex, buildArgs)
		if err != nil {
			return nil, err
		}
	}

	stagesResults := newStagesBuildResults()

	for _, s := range parseResult {
		stage := s
		if err := stagesResults.checkStageNameAvailable(stage.Name); err != nil {
			return nil, err
		}
		request = newDispatchRequest(b, escapeToken, source, buildArgs, stagesResults)

		currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, stage.SourceCode)
		if err := initializeStage(ctx, request, &stage); err != nil {
			return nil, err
		}
		request.state.updateRunConfig()
		_, _ = fmt.Fprintf(b.Stdout, " ---> %s\n", stringid.TruncateID(request.state.imageID))
		for _, cmd := range stage.Commands {
			select {
			case <-ctx.Done():
				log.G(ctx).Debug("Builder: build cancelled!")
				_, _ = fmt.Fprint(b.Stdout, "Build cancelled\n")
				buildsFailed.WithValues(metricsBuildCanceled).Inc()
				return nil, errors.New("Build cancelled")
			default:
				// Not cancelled yet, keep going...
			}

			currentCommandIndex = printCommand(b.Stdout, currentCommandIndex, totalCommands, cmd)

			if err := dispatch(ctx, request, cmd); err != nil {
				return nil, err
			}
			request.state.updateRunConfig()
			_, _ = fmt.Fprintf(b.Stdout, " ---> %s\n", stringid.TruncateID(request.state.imageID))
		}
		if err := emitImageID(b.Aux, request.state); err != nil {
			return nil, err
		}
		buildArgs.MergeReferencedArgs(request.state.buildArgs)
		if err := commitStage(request.state, stagesResults); err != nil {
			return nil, err
		}
	}
	buildArgs.WarnOnUnusedBuildArgs(b.Stdout)
	return request.state, nil
}

// BuildFromConfig builds directly from `changes`, treating it as if it were the contents of a Dockerfile
// It will:
// - Call parse.Parse() to get an AST root for the concatenated Dockerfile entries.
// - Do build by calling builder.dispatch() to call all entries' handling routines
//
// BuildFromConfig is used by the /commit endpoint, with the changes
// coming from the query parameter of the same name.
//
// TODO: Remove?
func BuildFromConfig(ctx context.Context, config *container.Config, changes []string, os string) (*container.Config, error) {
	if len(changes) == 0 {
		return config, nil
	}

	dockerfile, err := parser.Parse(bytes.NewBufferString(strings.Join(changes, "\n")))
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	// ensure that the commands are valid
	var commands []instructions.Command
	for _, n := range dockerfile.AST.Children {
		if !validCommitCommands[strings.ToLower(n.Value)] {
			return nil, errdefs.InvalidParameter(errors.Errorf("%s is not a valid change command", n.Value))
		}
		cmd, err := instructions.ParseCommand(n)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		commands = append(commands, cmd)
	}

	b, err := newBuilder(ctx, builderOptions{
		Options: &buildbackend.BuildOptions{NoCache: true},
	})
	if err != nil {
		return nil, err
	}

	b.Stdout = io.Discard
	b.Stderr = io.Discard
	b.disableCommit = true

	req := newDispatchRequest(b, dockerfile.EscapeToken, nil, NewBuildArgs(b.options.BuildArgs), newStagesBuildResults())
	// We make mutations to the configuration, ensure we have a copy
	req.state.runConfig = copyRunConfig(config)
	req.state.imageID = config.Image
	req.state.operatingSystem = os
	for _, cmd := range commands {
		err := dispatch(ctx, req, cmd)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		req.state.updateRunConfig()
	}

	return req.state.runConfig, nil
}

func convertMapToEnvList(m map[string]string) []string {
	result := []string{}
	for k, v := range m {
		result = append(result, k+"="+v)
	}
	return result
}

// convertKVStringsToMap converts ["key=value"] to {"key":"value"}
func convertKVStringsToMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		k, v, _ := strings.Cut(value, "=")
		result[k] = v
	}

	return result
}

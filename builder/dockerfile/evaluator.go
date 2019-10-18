// Package dockerfile is the evaluation step in the Dockerfile parse/evaluate pipeline.
//
// It incorporates a dispatch table based on the parser.Node values (see the
// parser package for more information) that are yielded from the parser itself.
// Calling newBuilder with the BuildOpts struct can be used to customize the
// experience for execution purposes only. Parsing is controlled in the parser
// package, and this division of responsibility should be respected.
//
// Please see the jump table targets for the actual invocations, most of which
// will call out to the functions in internals.go to deal with their tasks.
//
// ONBUILD is a special case, which is covered in the onbuild() func in
// dispatchers.go.
//
// The evaluator uses the concept of "steps", which are usually each processable
// line in the Dockerfile. Each step is numbered and certain actions are taken
// before and after each step, such as creating an image ID and removing temporary
// containers and images. Note that ONBUILD creates a kinda-sorta "sub run" which
// includes its own set of steps (usually only one of them).
package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/runconfig/opts"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func dispatch(d dispatchRequest, cmd instructions.Command) (err error) {
	if c, ok := cmd.(instructions.PlatformSpecific); ok {
		err := c.CheckPlatform(d.state.operatingSystem)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
	}
	runConfigEnv := d.state.runConfig.Env
	envs := append(runConfigEnv, d.state.buildArgs.FilterAllowed(runConfigEnv)...)

	if ex, ok := cmd.(instructions.SupportsSingleWordExpansion); ok {
		err := ex.Expand(func(word string) (string, error) {
			return d.shlex.ProcessWord(word, envs)
		})
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
	}

	defer func() {
		if d.builder.options.ForceRemove {
			d.builder.containerManager.RemoveAll(d.builder.Stdout)
			return
		}
		if d.builder.options.Remove && err == nil {
			d.builder.containerManager.RemoveAll(d.builder.Stdout)
			return
		}
	}()
	switch c := cmd.(type) {
	case *instructions.EnvCommand:
		return dispatchEnv(d, c)
	case *instructions.MaintainerCommand:
		return dispatchMaintainer(d, c)
	case *instructions.LabelCommand:
		return dispatchLabel(d, c)
	case *instructions.AddCommand:
		return dispatchAdd(d, c)
	case *instructions.CopyCommand:
		return dispatchCopy(d, c)
	case *instructions.OnbuildCommand:
		return dispatchOnbuild(d, c)
	case *instructions.WorkdirCommand:
		return dispatchWorkdir(d, c)
	case *instructions.RunCommand:
		return dispatchRun(d, c)
	case *instructions.CmdCommand:
		return dispatchCmd(d, c)
	case *instructions.HealthCheckCommand:
		return dispatchHealthcheck(d, c)
	case *instructions.EntrypointCommand:
		return dispatchEntrypoint(d, c)
	case *instructions.ExposeCommand:
		return dispatchExpose(d, c, envs)
	case *instructions.UserCommand:
		return dispatchUser(d, c)
	case *instructions.VolumeCommand:
		return dispatchVolume(d, c)
	case *instructions.StopSignalCommand:
		return dispatchStopSignal(d, c)
	case *instructions.ArgCommand:
		return dispatchArg(d, c)
	case *instructions.ShellCommand:
		return dispatchShell(d, c)
	}
	return errors.Errorf("unsupported command type: %v", reflect.TypeOf(cmd))
}

// dispatchState is a data object which is modified by dispatchers
type dispatchState struct {
	ctx         context.Context
	runConfig   *container.Config
	maintainer  string
	cmdSet      bool
	image       *ocispec.Descriptor
	stageName   string
	buildArgs   *BuildArgs
	baseImage   *ocispec.Descriptor // How to represent scratch
	noBaseImage bool

	// TODO(containerd): remove these
	operatingSystem string
}

func newDispatchState(ctx context.Context, baseArgs *BuildArgs) *dispatchState {
	args := baseArgs.Clone()
	args.ResetAllowed()
	return &dispatchState{ctx: ctx, runConfig: &container.Config{}, buildArgs: args}
}

type stagesBuildResults struct {
	flat    []*container.Config
	indexed map[string]*container.Config
}

func newStagesBuildResults() *stagesBuildResults {
	return &stagesBuildResults{
		indexed: make(map[string]*container.Config),
	}
}

func (r *stagesBuildResults) getByName(name string) (*container.Config, bool) {
	c, ok := r.indexed[strings.ToLower(name)]
	return c, ok
}

func (r *stagesBuildResults) validateIndex(i int) error {
	if i == len(r.flat) {
		return errors.New("refers to current build stage")
	}
	if i < 0 || i > len(r.flat) {
		return errors.New("index out of bounds")
	}
	return nil
}

func (r *stagesBuildResults) get(nameOrIndex string) (*container.Config, error) {
	if c, ok := r.getByName(nameOrIndex); ok {
		return c, nil
	}
	ix, err := strconv.ParseInt(nameOrIndex, 10, 0)
	if err != nil {
		return nil, nil
	}
	if err := r.validateIndex(int(ix)); err != nil {
		return nil, err
	}
	return r.flat[ix], nil
}

func (r *stagesBuildResults) checkStageNameAvailable(name string) error {
	if name != "" {
		if _, ok := r.getByName(name); ok {
			return errors.Errorf("%s stage name already used", name)
		}
	}
	return nil
}

func (r *stagesBuildResults) commitStage(name string, config *container.Config) error {
	if name != "" {
		if _, ok := r.getByName(name); ok {
			return errors.Errorf("%s stage name already used", name)
		}
		r.indexed[strings.ToLower(name)] = config
	}
	r.flat = append(r.flat, config)
	return nil
}

func commitStage(state *dispatchState, stages *stagesBuildResults) error {
	return stages.commitStage(state.stageName, state.runConfig)
}

type dispatchRequest struct {
	state   *dispatchState
	shlex   *shell.Lex
	builder *Builder
	source  builder.Source
	stages  *stagesBuildResults
}

func newDispatchRequest(builder *Builder, escapeToken rune, source builder.Source, buildArgs *BuildArgs, stages *stagesBuildResults) dispatchRequest {
	return dispatchRequest{
		state:   newDispatchState(builder.clientCtx, buildArgs),
		shlex:   shell.NewLex(escapeToken),
		builder: builder,
		source:  source,
		stages:  stages,
	}
}

func (s *dispatchState) updateRunConfig() {
	// TODO(containerd): Should this even be set in the run config?
	if s.image != nil {
		s.runConfig.Image = s.image.Digest.String()
	}
}

// hasFromImage returns true if the builder has processed a `FROM <image>` line
func (s *dispatchState) hasFromImage() bool {
	return s.image != nil || s.noBaseImage
}

func (s *dispatchState) beginStage(stageName string, image *ocispec.Descriptor, config []byte) error {
	s.stageName = stageName

	s.image = image

	if len(config) > 0 {
		var c struct {
			Config *container.Config `json:"config,omitempty"`
		}
		if err := json.Unmarshal(config, &c); err != nil {
			return err
		}
		s.runConfig = c.Config
	}
	if s.runConfig == nil {
		s.runConfig = &container.Config{}
	}
	s.baseImage = image
	s.setDefaultPath()
	s.runConfig.OpenStdin = false
	s.runConfig.StdinOnce = false
	return nil
}

// Add the default PATH to runConfig.ENV if one exists for the operating system and there
// is no PATH set. Note that Windows containers on Windows won't have one as it's set by HCS
func (s *dispatchState) setDefaultPath() {
	defaultPath := system.DefaultPathEnv(s.operatingSystem)
	if defaultPath == "" {
		return
	}
	envMap := opts.ConvertKVStringsToMap(s.runConfig.Env)
	if _, ok := envMap["PATH"]; !ok {
		s.runConfig.Env = append(s.runConfig.Env, "PATH="+defaultPath)
	}
}

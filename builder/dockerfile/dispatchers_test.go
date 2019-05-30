package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/go-connections/nat"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func newBuilderWithMockBackend() *Builder {
	mockBackend := &MockBackend{}
	opts := &types.ImageBuildOptions{}
	ctx := context.Background()
	b := &Builder{
		options:       opts,
		docker:        mockBackend,
		Stdout:        new(bytes.Buffer),
		clientCtx:     ctx,
		disableCommit: true,
		imageSources: newImageSources(ctx, builderOptions{
			Options: opts,
			Backend: mockBackend,
		}),
		imageProber:      newImageProber(mockBackend, nil, false),
		containerManager: newContainerManager(mockBackend),
	}
	return b
}

func TestEnv2Variables(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	envCommand := &instructions.EnvCommand{
		Env: instructions.KeyValuePairs{
			instructions.KeyValuePair{Key: "var1", Value: "val1"},
			instructions.KeyValuePair{Key: "var2", Value: "val2"},
		},
	}
	err := dispatch(sb, envCommand)
	assert.NilError(t, err)

	expected := []string{
		"var1=val1",
		"var2=val2",
	}
	assert.Check(t, is.DeepEqual(expected, sb.state.runConfig.Env))
}

func TestEnvValueWithExistingRunConfigEnv(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	sb.state.runConfig.Env = []string{"var1=old", "var2=fromenv"}
	envCommand := &instructions.EnvCommand{
		Env: instructions.KeyValuePairs{
			instructions.KeyValuePair{Key: "var1", Value: "val1"},
		},
	}
	err := dispatch(sb, envCommand)
	assert.NilError(t, err)
	expected := []string{
		"var1=val1",
		"var2=fromenv",
	}
	assert.Check(t, is.DeepEqual(expected, sb.state.runConfig.Env))
}

func TestMaintainer(t *testing.T) {
	maintainerEntry := "Some Maintainer <maintainer@example.com>"
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	cmd := &instructions.MaintainerCommand{Maintainer: maintainerEntry}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(maintainerEntry, sb.state.maintainer))
}

func TestLabel(t *testing.T) {
	labelName := "label"
	labelValue := "value"

	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	cmd := &instructions.LabelCommand{
		Labels: instructions.KeyValuePairs{
			instructions.KeyValuePair{Key: labelName, Value: labelValue},
		},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	assert.Assert(t, is.Contains(sb.state.runConfig.Labels, labelName))
	assert.Check(t, is.Equal(sb.state.runConfig.Labels[labelName], labelValue))
}

func TestFromScratch(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	cmd := &instructions.Stage{
		BaseName: "scratch",
	}
	err := initializeStage(sb, cmd)

	if runtime.GOOS == "windows" && !system.LCOWSupported() {
		assert.Check(t, is.Error(err, "Linux containers are not supported on this system"))
		return
	}

	assert.NilError(t, err)
	assert.Check(t, sb.state.hasFromImage())
	assert.Check(t, is.Equal("", sb.state.imageID))
	expected := "PATH=" + system.DefaultPathEnv(runtime.GOOS)
	assert.Check(t, is.DeepEqual([]string{expected}, sb.state.runConfig.Env))
}

func TestFromWithArg(t *testing.T) {
	tag, expected := ":sometag", "expectedthisid"

	getImage := func(name string) (builder.Image, builder.ROLayer, error) {
		assert.Check(t, is.Equal("alpine"+tag, name))
		return &mockImage{id: "expectedthisid"}, nil, nil
	}
	b := newBuilderWithMockBackend()
	b.docker.(*MockBackend).getImageFunc = getImage
	args := NewBuildArgs(make(map[string]*string))

	val := "sometag"
	metaArg := instructions.ArgCommand{KeyValuePairOptional: instructions.KeyValuePairOptional{
		Key:   "THETAG",
		Value: &val,
	}}
	cmd := &instructions.Stage{
		BaseName: "alpine:${THETAG}",
	}
	err := processMetaArg(metaArg, shell.NewLex('\\'), args)

	sb := newDispatchRequest(b, '\\', nil, args, newStagesBuildResults())
	assert.NilError(t, err)
	err = initializeStage(sb, cmd)
	assert.NilError(t, err)

	assert.Check(t, is.Equal(expected, sb.state.imageID))
	assert.Check(t, is.Equal(expected, sb.state.baseImage.ImageID()))
	assert.Check(t, is.Len(sb.state.buildArgs.GetAllAllowed(), 0))
	assert.Check(t, is.Len(sb.state.buildArgs.GetAllMeta(), 1))
}

func TestFromWithArgButBuildArgsNotGiven(t *testing.T) {
	b := newBuilderWithMockBackend()
	args := NewBuildArgs(make(map[string]*string))

	metaArg := instructions.ArgCommand{}
	cmd := &instructions.Stage{
		BaseName: "${THETAG}",
	}
	err := processMetaArg(metaArg, shell.NewLex('\\'), args)

	sb := newDispatchRequest(b, '\\', nil, args, newStagesBuildResults())
	assert.NilError(t, err)
	err = initializeStage(sb, cmd)
	assert.Error(t, err, "base name (${THETAG}) should not be blank")
}

func TestFromWithUndefinedArg(t *testing.T) {
	tag, expected := "sometag", "expectedthisid"

	getImage := func(name string) (builder.Image, builder.ROLayer, error) {
		assert.Check(t, is.Equal("alpine", name))
		return &mockImage{id: "expectedthisid"}, nil, nil
	}
	b := newBuilderWithMockBackend()
	b.docker.(*MockBackend).getImageFunc = getImage
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	b.options.BuildArgs = map[string]*string{"THETAG": &tag}

	cmd := &instructions.Stage{
		BaseName: "alpine${THETAG}",
	}
	err := initializeStage(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(expected, sb.state.imageID))
}

func TestFromMultiStageWithNamedStage(t *testing.T) {
	b := newBuilderWithMockBackend()
	firstFrom := &instructions.Stage{BaseName: "someimg", Name: "base"}
	secondFrom := &instructions.Stage{BaseName: "base"}
	previousResults := newStagesBuildResults()
	firstSB := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), previousResults)
	secondSB := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), previousResults)
	err := initializeStage(firstSB, firstFrom)
	assert.NilError(t, err)
	assert.Check(t, firstSB.state.hasFromImage())
	previousResults.indexed["base"] = firstSB.state.runConfig
	previousResults.flat = append(previousResults.flat, firstSB.state.runConfig)
	err = initializeStage(secondSB, secondFrom)
	assert.NilError(t, err)
	assert.Check(t, secondSB.state.hasFromImage())
}

func TestOnbuild(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '\\', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	cmd := &instructions.OnbuildCommand{
		Expression: "ADD . /app/src",
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal("ADD . /app/src", sb.state.runConfig.OnBuild[0]))
}

func TestWorkdir(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	sb.state.baseImage = &mockImage{}
	workingDir := "/app"
	if runtime.GOOS == "windows" {
		workingDir = "C:\\app"
	}
	cmd := &instructions.WorkdirCommand{
		Path: workingDir,
	}

	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(workingDir, sb.state.runConfig.WorkingDir))
}

func TestCmd(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	sb.state.baseImage = &mockImage{}
	command := "./executable"

	cmd := &instructions.CmdCommand{
		ShellDependantCmdLine: instructions.ShellDependantCmdLine{
			CmdLine:      strslice.StrSlice{command},
			PrependShell: true,
		},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	var expectedCommand strslice.StrSlice
	if runtime.GOOS == "windows" {
		expectedCommand = strslice.StrSlice(append([]string{"cmd"}, "/S", "/C", command))
	} else {
		expectedCommand = strslice.StrSlice(append([]string{"/bin/sh"}, "-c", command))
	}

	assert.Check(t, is.DeepEqual(expectedCommand, sb.state.runConfig.Cmd))
	assert.Check(t, sb.state.cmdSet)
}

func TestHealthcheckNone(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	cmd := &instructions.HealthCheckCommand{
		Health: &container.HealthConfig{
			Test: []string{"NONE"},
		},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	assert.Assert(t, sb.state.runConfig.Healthcheck != nil)
	assert.Check(t, is.DeepEqual([]string{"NONE"}, sb.state.runConfig.Healthcheck.Test))
}

func TestHealthcheckCmd(t *testing.T) {

	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	expectedTest := []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}
	cmd := &instructions.HealthCheckCommand{
		Health: &container.HealthConfig{
			Test: expectedTest,
		},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	assert.Assert(t, sb.state.runConfig.Healthcheck != nil)
	assert.Check(t, is.DeepEqual(expectedTest, sb.state.runConfig.Healthcheck.Test))
}

func TestEntrypoint(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	sb.state.baseImage = &mockImage{}
	entrypointCmd := "/usr/sbin/nginx"

	cmd := &instructions.EntrypointCommand{
		ShellDependantCmdLine: instructions.ShellDependantCmdLine{
			CmdLine:      strslice.StrSlice{entrypointCmd},
			PrependShell: true,
		},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Assert(t, sb.state.runConfig.Entrypoint != nil)

	var expectedEntrypoint strslice.StrSlice
	if runtime.GOOS == "windows" {
		expectedEntrypoint = strslice.StrSlice(append([]string{"cmd"}, "/S", "/C", entrypointCmd))
	} else {
		expectedEntrypoint = strslice.StrSlice(append([]string{"/bin/sh"}, "-c", entrypointCmd))
	}
	assert.Check(t, is.DeepEqual(expectedEntrypoint, sb.state.runConfig.Entrypoint))
}

func TestExpose(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	exposedPort := "80"
	cmd := &instructions.ExposeCommand{
		Ports: []string{exposedPort},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	assert.Assert(t, sb.state.runConfig.ExposedPorts != nil)
	assert.Assert(t, is.Len(sb.state.runConfig.ExposedPorts, 1))

	portsMapping, err := nat.ParsePortSpec(exposedPort)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(sb.state.runConfig.ExposedPorts, portsMapping[0].Port))
}

func TestUser(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	cmd := &instructions.UserCommand{
		User: "test",
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal("test", sb.state.runConfig.User))
}

func TestVolume(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	exposedVolume := "/foo"

	cmd := &instructions.VolumeCommand{
		Volumes: []string{exposedVolume},
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Assert(t, sb.state.runConfig.Volumes != nil)
	assert.Check(t, is.Len(sb.state.runConfig.Volumes, 1))
	assert.Check(t, is.Contains(sb.state.runConfig.Volumes, exposedVolume))
}

func TestStopSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support stopsignal")
		return
	}
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())
	sb.state.baseImage = &mockImage{}
	signal := "SIGKILL"

	cmd := &instructions.StopSignalCommand{
		Signal: signal,
	}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(signal, sb.state.runConfig.StopSignal))
}

func TestArg(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	argName := "foo"
	argVal := "bar"
	cmd := &instructions.ArgCommand{KeyValuePairOptional: instructions.KeyValuePairOptional{Key: argName, Value: &argVal}}
	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	expected := map[string]string{argName: argVal}
	assert.Check(t, is.DeepEqual(expected, sb.state.buildArgs.GetAllAllowed()))
}

func TestShell(t *testing.T) {
	b := newBuilderWithMockBackend()
	sb := newDispatchRequest(b, '`', nil, NewBuildArgs(make(map[string]*string)), newStagesBuildResults())

	shellCmd := "powershell"
	cmd := &instructions.ShellCommand{Shell: strslice.StrSlice{shellCmd}}

	err := dispatch(sb, cmd)
	assert.NilError(t, err)

	expectedShell := strslice.StrSlice([]string{shellCmd})
	assert.Check(t, is.DeepEqual(expectedShell, sb.state.runConfig.Shell))
}

func TestPrependEnvOnCmd(t *testing.T) {
	buildArgs := NewBuildArgs(nil)
	buildArgs.AddArg("NO_PROXY", nil)

	args := []string{"sorted=nope", "args=not", "http_proxy=foo", "NO_PROXY=YA"}
	cmd := []string{"foo", "bar"}
	cmdWithEnv := prependEnvOnCmd(buildArgs, args, cmd)
	expected := strslice.StrSlice([]string{
		"|3", "NO_PROXY=YA", "args=not", "sorted=nope", "foo", "bar"})
	assert.Check(t, is.DeepEqual(expected, cmdWithEnv))
}

func TestRunWithBuildArgs(t *testing.T) {
	b := newBuilderWithMockBackend()
	args := NewBuildArgs(make(map[string]*string))
	args.argsFromOptions["HTTP_PROXY"] = strPtr("FOO")
	b.disableCommit = false
	sb := newDispatchRequest(b, '`', nil, args, newStagesBuildResults())

	runConfig := &container.Config{}
	origCmd := strslice.StrSlice([]string{"cmd", "in", "from", "image"})

	var cmdWithShell strslice.StrSlice
	if runtime.GOOS == "windows" {
		cmdWithShell = strslice.StrSlice([]string{strings.Join(append(getShell(runConfig, runtime.GOOS), []string{"echo foo"}...), " ")})
	} else {
		cmdWithShell = strslice.StrSlice(append(getShell(runConfig, runtime.GOOS), "echo foo"))
	}

	envVars := []string{"|1", "one=two"}
	cachedCmd := strslice.StrSlice(append(envVars, cmdWithShell...))

	imageCache := &mockImageCache{
		getCacheFunc: func(parentID string, cfg *container.Config) (string, error) {
			// Check the runConfig.Cmd sent to probeCache()
			assert.Check(t, is.DeepEqual(cachedCmd, cfg.Cmd))
			assert.Check(t, is.DeepEqual(strslice.StrSlice(nil), cfg.Entrypoint))
			return "", nil
		},
	}

	mockBackend := b.docker.(*MockBackend)
	mockBackend.makeImageCacheFunc = func(_ []string) builder.ImageCache {
		return imageCache
	}
	b.imageProber = newImageProber(mockBackend, nil, false)
	mockBackend.getImageFunc = func(_ string) (builder.Image, builder.ROLayer, error) {
		return &mockImage{
			id:     "abcdef",
			config: &container.Config{Cmd: origCmd},
		}, nil, nil
	}
	mockBackend.containerCreateFunc = func(config types.ContainerCreateConfig) (container.ContainerCreateCreatedBody, error) {
		// Check the runConfig.Cmd sent to create()
		assert.Check(t, is.DeepEqual(cmdWithShell, config.Config.Cmd))
		assert.Check(t, is.Contains(config.Config.Env, "one=two"))
		assert.Check(t, is.DeepEqual(strslice.StrSlice{""}, config.Config.Entrypoint))
		return container.ContainerCreateCreatedBody{ID: "12345"}, nil
	}
	mockBackend.commitFunc = func(cfg backend.CommitConfig) (image.ID, error) {
		// Check the runConfig.Cmd sent to commit()
		assert.Check(t, is.DeepEqual(origCmd, cfg.Config.Cmd))
		assert.Check(t, is.DeepEqual(cachedCmd, cfg.ContainerConfig.Cmd))
		assert.Check(t, is.DeepEqual(strslice.StrSlice(nil), cfg.Config.Entrypoint))
		return "", nil
	}
	from := &instructions.Stage{BaseName: "abcdef"}
	err := initializeStage(sb, from)
	assert.NilError(t, err)
	sb.state.buildArgs.AddArg("one", strPtr("two"))

	// This is hugely annoying. On the Windows side, it relies on the
	// RunCommand being able to emit String() and Name() (as implemented by
	// withNameAndCode). Unfortunately, that is internal, and no way to directly
	// set. However, we can fortunately use ParseInstruction in the instructions
	// package to parse a fake node which can be used as our instructions.RunCommand
	// instead.
	node := &parser.Node{
		Original: `RUN echo foo`,
		Value:    "run",
	}
	runint, err := instructions.ParseInstruction(node)
	assert.NilError(t, err)
	runinst := runint.(*instructions.RunCommand)
	runinst.CmdLine = strslice.StrSlice{"echo foo"}
	runinst.PrependShell = true

	assert.NilError(t, dispatch(sb, runinst))

	// Check that runConfig.Cmd has not been modified by run
	assert.Check(t, is.DeepEqual(origCmd, sb.state.runConfig.Cmd))
}

func TestRunIgnoresHealthcheck(t *testing.T) {
	b := newBuilderWithMockBackend()
	args := NewBuildArgs(make(map[string]*string))
	sb := newDispatchRequest(b, '`', nil, args, newStagesBuildResults())
	b.disableCommit = false

	origCmd := strslice.StrSlice([]string{"cmd", "in", "from", "image"})

	imageCache := &mockImageCache{
		getCacheFunc: func(parentID string, cfg *container.Config) (string, error) {
			return "", nil
		},
	}

	mockBackend := b.docker.(*MockBackend)
	mockBackend.makeImageCacheFunc = func(_ []string) builder.ImageCache {
		return imageCache
	}
	b.imageProber = newImageProber(mockBackend, nil, false)
	mockBackend.getImageFunc = func(_ string) (builder.Image, builder.ROLayer, error) {
		return &mockImage{
			id:     "abcdef",
			config: &container.Config{Cmd: origCmd},
		}, nil, nil
	}
	mockBackend.containerCreateFunc = func(config types.ContainerCreateConfig) (container.ContainerCreateCreatedBody, error) {
		return container.ContainerCreateCreatedBody{ID: "12345"}, nil
	}
	mockBackend.commitFunc = func(cfg backend.CommitConfig) (image.ID, error) {
		return "", nil
	}
	from := &instructions.Stage{BaseName: "abcdef"}
	err := initializeStage(sb, from)
	assert.NilError(t, err)

	expectedTest := []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}
	cmd := &instructions.HealthCheckCommand{
		Health: &container.HealthConfig{
			Test: expectedTest,
		},
	}
	assert.NilError(t, dispatch(sb, cmd))
	assert.Assert(t, sb.state.runConfig.Healthcheck != nil)

	mockBackend.containerCreateFunc = func(config types.ContainerCreateConfig) (container.ContainerCreateCreatedBody, error) {
		// Check the Healthcheck is disabled.
		assert.Check(t, is.DeepEqual([]string{"NONE"}, config.Config.Healthcheck.Test))
		return container.ContainerCreateCreatedBody{ID: "123456"}, nil
	}

	sb.state.buildArgs.AddArg("one", strPtr("two"))
	run := &instructions.RunCommand{
		ShellDependantCmdLine: instructions.ShellDependantCmdLine{
			CmdLine:      strslice.StrSlice{"echo foo"},
			PrependShell: true,
		},
	}
	assert.NilError(t, dispatch(sb, run))
	assert.Check(t, is.DeepEqual(expectedTest, sb.state.runConfig.Healthcheck.Test))
}

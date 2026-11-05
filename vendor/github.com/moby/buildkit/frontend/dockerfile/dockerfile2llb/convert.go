package dockerfile2llb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/imagemetaresolver"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	dfcommand "github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/frontend/subrequests/convertllb"
	"github.com/moby/buildkit/frontend/subrequests/lint"
	"github.com/moby/buildkit/frontend/subrequests/outline"
	"github.com/moby/buildkit/frontend/subrequests/targets"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/suggest"
	"github.com/moby/buildkit/util/system"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/patternmatcher"
	"github.com/moby/sys/signal"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const (
	emptyImageName = "scratch"
	historyComment = "buildkit.dockerfile.v0"

	sbomScanContext = "BUILDKIT_SBOM_SCAN_CONTEXT"
	sbomScanStage   = "BUILDKIT_SBOM_SCAN_STAGE"
	disableOnbuild  = "BUILDKIT_DISABLE_ONBUILD"
)

var nonEnvArgs = map[string]struct{}{
	sbomScanContext: {},
	sbomScanStage:   {},
}

type ConvertOpt struct {
	dockerui.Config
	Client         *dockerui.Client
	MainContext    *llb.State
	SourceMap      *llb.SourceMap
	TargetPlatform *ocispecs.Platform
	MetaResolver   llb.ImageMetaResolver
	LLBCaps        *apicaps.CapSet
	Warn           linter.LintWarnFunc
	AllStages      bool
}

type SBOMTargets struct {
	Core   llb.State
	Extras map[string]llb.State
}

type Result struct {
	State     llb.State
	Image     *dockerspec.DockerOCIImage
	BaseImage *dockerspec.DockerOCIImage
	SBOM      *SBOMTargets
	Epoch     *time.Time

	IsIgnoreCache bool
}

func Dockerfile2LLB(ctx context.Context, dt []byte, opt ConvertOpt) (*Result, error) {
	ds, err := toDispatchState(ctx, dt, opt)
	if err != nil {
		return nil, err
	}

	res := &Result{
		State:     ds.state,
		Image:     &ds.image,
		BaseImage: ds.baseImg,
		SBOM: &SBOMTargets{
			Core:   ds.state,
			Extras: map[string]llb.State{},
		},
		Epoch: ds.epoch,
	}
	if ds.scanContext {
		res.SBOM.Extras["context"] = ds.opt.buildContext
	}
	if ds.ignoreCache {
		res.IsIgnoreCache = true
	}
	for dsi := range allReachableStages(ds) {
		if ds != dsi && dsi.scanStage {
			res.SBOM.Extras[dsi.stageName] = dsi.state
			if dsi.ignoreCache {
				res.IsIgnoreCache = true
			}
		}
	}

	return res, nil
}

func Dockerfile2Outline(ctx context.Context, dt []byte, opt ConvertOpt) (*outline.Outline, error) {
	ds, err := toDispatchState(ctx, dt, opt)
	if err != nil {
		return nil, err
	}
	o := ds.Outline(dt)
	return &o, nil
}

func DockerfileConvertLLB(ctx context.Context, dt []byte, opt ConvertOpt) (*convertllb.Result, error) {
	ds, err := toDispatchState(ctx, dt, opt)
	if err != nil {
		return nil, err
	}
	return ds.ConvertLLB(ctx)
}

func DockerfileLint(ctx context.Context, dt []byte, opt ConvertOpt) (*lint.LintResults, error) {
	results := &lint.LintResults{}
	sourceIndex := results.AddSource(opt.SourceMap)
	opt.Warn = func(rulename, description, url, fmtmsg string, location []parser.Range) {
		results.AddWarning(rulename, description, url, fmtmsg, sourceIndex, location)
	}
	// for lint, no target means all targets
	if opt.Target == "" {
		opt.AllStages = true
	}

	_, err := toDispatchState(ctx, dt, opt)

	var errLoc *parser.LocationError
	if err != nil {
		buildErr := &lint.BuildError{
			Message: err.Error(),
		}
		if errors.As(err, &errLoc) {
			ranges := mergeLocations(errLoc.Locations...)
			buildErr.Location = toPBLocation(sourceIndex, ranges)
		}
		results.Error = buildErr
	}
	return results, nil
}

func ListTargets(ctx context.Context, dt []byte) (*targets.List, error) {
	dockerfile, err := parser.Parse(bytes.NewReader(dt))
	if err != nil {
		return nil, err
	}

	stages, _, err := instructions.Parse(dockerfile.AST, nil)
	if err != nil {
		return nil, err
	}

	l := &targets.List{
		Sources: [][]byte{dt},
	}

	for i, s := range stages {
		t := targets.Target{
			Name:        s.Name,
			Description: s.DocComment,
			Default:     i == len(stages)-1,
			Base:        s.BaseName,
			Platform:    s.Platform,
			Location:    toSourceLocation(s.Location),
		}
		l.Targets = append(l.Targets, t)
	}
	return l, nil
}

func newRuleLinter(dt []byte, opt *ConvertOpt) (*linter.Linter, error) {
	var lintConfig *linter.Config
	if opt.Client != nil && opt.Client.LinterConfig != nil {
		lintConfig = opt.Client.LinterConfig
	} else {
		var err error
		lintOptionStr, _, _, _ := parser.ParseDirective("check", dt)
		lintConfig, err = linter.ParseLintOptions(lintOptionStr)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse check options")
		}
	}
	lintConfig.Warn = opt.Warn
	return linter.New(lintConfig), nil
}

type dispatchContext struct {
	opt               ConvertOpt
	platformOpt       *platformOpt
	globalArgs        *llb.EnvList
	epoch             *time.Time
	shlex             *shell.Lex
	outline           outlineCapture
	lint              *linter.Linter
	metaResolver      llb.ImageMetaResolver
	allDispatchStates *dispatchStates
	proxyEnv          *llb.ProxyEnv
	namedContext      func(string, dockerui.ContextOpt) (*dockerui.NamedContext, error)
	disableOnbuild    bool
}

func namedContextFunc(opt ConvertOpt) func(string, dockerui.ContextOpt) (*dockerui.NamedContext, error) {
	return func(name string, copt dockerui.ContextOpt) (*dockerui.NamedContext, error) {
		if opt.Client == nil {
			return nil, nil
		}
		if !strings.EqualFold(name, "scratch") && !strings.EqualFold(name, "context") {
			if copt.Platform == nil {
				copt.Platform = opt.TargetPlatform
			}
			return opt.Client.NamedContext(name, copt)
		}
		return nil, nil
	}
}

func toDispatchState(ctx context.Context, dt []byte, opt ConvertOpt) (*dispatchState, error) {
	if len(dt) == 0 {
		return nil, errors.Errorf("the Dockerfile cannot be empty")
	}

	if opt.Client != nil && opt.MainContext != nil {
		return nil, errors.Errorf("Client and MainContext cannot both be provided")
	}

	lint, err := newRuleLinter(dt, &opt)
	if err != nil {
		return nil, err
	}

	if opt.Client != nil && opt.LLBCaps == nil {
		caps := opt.Client.BuildOpts().LLBCaps
		opt.LLBCaps = &caps
	}

	dockerfile, err := parser.Parse(bytes.NewReader(dt))
	if err != nil {
		return nil, err
	}

	// Moby still uses the `dockerfile.PrintWarnings` method to print non-empty
	// continuation line warnings. We iterate over those warnings here.
	for _, warning := range dockerfile.Warnings {
		// The `dockerfile.Warnings` *should* only contain warnings about empty continuation
		// lines, but we'll check the warning message to be sure, so that we don't accidentally
		// process warnings that are not related to empty continuation lines twice.
		if warning.URL == linter.RuleNoEmptyContinuation.URL {
			location := []parser.Range{*warning.Location}
			msg := linter.RuleNoEmptyContinuation.Format()
			lint.Run(&linter.RuleNoEmptyContinuation, location, msg)
		}
	}

	stages, argCmds, err := instructions.Parse(dockerfile.AST, lint)
	if err != nil {
		return nil, err
	}
	if len(stages) == 0 {
		return nil, errors.New("dockerfile contains no stages to build")
	}
	validateStageNames(stages, lint)
	validateCommandCasing(stages, lint)

	platformOpt := buildPlatformOpt(&opt)
	targetName := opt.Target
	if targetName == "" {
		targetName = stages[len(stages)-1].Name
	}
	globalArgs := defaultArgs(platformOpt, opt.BuildArgs, targetName)

	shlex := shell.NewLex(dockerfile.EscapeToken)
	outline := newOutlineCapture()

	// Validate that base images continue to be valid even
	// when no build arguments are used.
	validateBaseImagesWithDefaultArgs(stages, shlex, globalArgs, argCmds, lint)

	// Rebuild the arguments using the provided build arguments
	// for the remainder of the build.
	globalArgs, outline.allArgs, err = buildMetaArgs(globalArgs, shlex, argCmds, opt.BuildArgs)
	if err != nil {
		return nil, err
	}

	var resolvedEpoch *time.Time
	if sourceDateEpoch, ok := getBuildArgValue(opt.BuildArgs, globalArgs, "SOURCE_DATE_EPOCH"); ok {
		resolvedEpoch, err = resolveSourceDateEpochValue(ctx, sourceDateEpoch, opt, stages, globalArgs, shlex)
		if err != nil {
			return nil, err
		}
		globalArgs = setBuildArgValue(opt.BuildArgs, globalArgs, "SOURCE_DATE_EPOCH", formatSourceDateEpochValue(resolvedEpoch))
	}

	var skipOnbuild bool
	if v, ok := getBuildArgValue(opt.BuildArgs, globalArgs, disableOnbuild); ok && v != "" {
		skipOnbuild, err = strconv.ParseBool(v)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid %s value %q", disableOnbuild, v)
		}
	}

	metaResolver := opt.MetaResolver
	if metaResolver == nil {
		metaResolver = imagemetaresolver.Default()
	}

	dctx := &dispatchContext{
		opt:               opt,
		platformOpt:       platformOpt,
		globalArgs:        globalArgs,
		epoch:             resolvedEpoch,
		shlex:             shlex,
		outline:           outline,
		lint:              lint,
		metaResolver:      metaResolver,
		allDispatchStates: newDispatchStates(),
		proxyEnv:          proxyEnvFromBuildArgs(opt.BuildArgs),
		namedContext:      namedContextFunc(opt),
		disableOnbuild:    skipOnbuild,
	}

	if err := dctx.buildDispatchStates(stages); err != nil {
		return nil, err
	}

	target, err := dctx.resolveTarget()
	if err != nil {
		return nil, err
	}

	if err := dctx.buildStageDependencyGraph(); err != nil {
		return nil, err
	}

	allReachable, err := dctx.resolveStages(ctx, target)
	if err != nil {
		return nil, err
	}

	ctxPaths, buildContext, err := dctx.dispatchStages(ctx, allReachable, target)
	if err != nil {
		return nil, err
	}

	if err := dctx.finalizeResultImage(ctx, target, ctxPaths, buildContext); err != nil {
		return nil, err
	}

	return target, nil
}

func getBuildArgValue(buildArgs map[string]string, globalArgs *llb.EnvList, key string) (string, bool) {
	if v, ok := buildArgs[key]; ok {
		return v, true
	}
	if globalArgs == nil {
		return "", false
	}
	v, ok := globalArgs.Get(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func setBuildArgValue(buildArgs map[string]string, globalArgs *llb.EnvList, key, value string) *llb.EnvList {
	if _, ok := buildArgs[key]; ok {
		if value == "" {
			delete(buildArgs, key)
		} else {
			buildArgs[key] = value
		}
	}
	if globalArgs != nil {
		if _, ok := globalArgs.Get(key); ok {
			if value == "" {
				updated := globalArgs.Delete(key)
				globalArgs = &updated
			} else {
				globalArgs = globalArgs.AddOrReplace(key, value)
			}
		}
	}
	return globalArgs
}

func (dctx *dispatchContext) buildDispatchStates(stages []instructions.Stage) error {
	for i, st := range stages {
		lint := dctx.lint.WithMergedConfigFromComments(st.Comments)

		nameMatch, err := dctx.shlex.ProcessWordWithMatches(st.BaseName, dctx.globalArgs)
		argKeys := unusedFromArgsCheckKeys(dctx.globalArgs, dctx.outline.allArgs)
		reportUnusedFromArgs(argKeys, nameMatch.Unmatched, st.Location, lint)
		used := nameMatch.Matched
		if used == nil {
			used = map[string]struct{}{}
		}

		if err != nil {
			return parser.WithLocation(err, st.Location)
		}
		if nameMatch.Result == "" {
			return parser.WithLocation(errors.Errorf("base name (%s) should not be blank", st.BaseName), st.Location)
		}
		st.BaseName = nameMatch.Result

		ds := &dispatchState{
			stage:          st,
			deps:           make(map[*dispatchState]instructions.Command),
			ctxPaths:       make(map[string]struct{}),
			paths:          make(map[string]struct{}),
			stageName:      st.Name,
			prefixPlatform: dctx.opt.MultiPlatformRequested,
			outline:        dctx.outline.clone(),
			epoch:          dctx.epoch,
		}

		if v := st.Platform; v != "" {
			platMatch, err := dctx.shlex.ProcessWordWithMatches(v, dctx.globalArgs)
			argKeys := unusedFromArgsCheckKeys(dctx.globalArgs, dctx.outline.allArgs)
			reportUnusedFromArgs(argKeys, platMatch.Unmatched, st.Location, lint)
			reportRedundantTargetPlatform(st.Platform, platMatch, st.Location, dctx.globalArgs, lint)
			reportConstPlatformDisallowed(st.Name, platMatch, st.Location, lint)

			if err != nil {
				return parser.WithLocation(errors.Wrapf(err, "failed to process arguments for platform %s", platMatch.Result), st.Location)
			}

			if platMatch.Result == "" {
				err := errors.Errorf("empty platform value from expression %s", v)
				err = parser.WithLocation(err, st.Location)
				err = wrapSuggestAny(err, platMatch.Unmatched, dctx.globalArgs.Keys())
				return err
			}

			p, err := platforms.Parse(platMatch.Result)
			if err != nil {
				err = parser.WithLocation(err, st.Location)
				err = wrapSuggestAny(err, platMatch.Unmatched, dctx.globalArgs.Keys())
				return parser.WithLocation(errors.Wrapf(err, "failed to parse platform %s", v), st.Location)
			}

			for k := range platMatch.Matched {
				used[k] = struct{}{}
			}

			ds.platform = &p
		}

		if st.Name != "" {
			nc, err := dctx.namedContext(st.Name, dockerui.ContextOpt{
				Platform:       ds.platform,
				ResolveMode:    dctx.opt.ImageResolveMode.String(),
				AsyncLocalOpts: ds.asyncLocalOpts,
			})
			if err != nil {
				return err
			}
			if nc != nil {
				ds.namedContext = nc
				dctx.allDispatchStates.addState(ds)
				ds.base = nil // reset base set by addState
				continue
			}
		}

		if st.Name == "" {
			ds.stageName = fmt.Sprintf("stage-%d", i)
		}

		dctx.allDispatchStates.addState(ds)

		for k := range used {
			ds.outline.usedArgs[k] = struct{}{}
		}

		total := 0
		if ds.stage.BaseName != emptyImageName && ds.base == nil {
			total = 1
		}
		for _, cmd := range ds.stage.Commands {
			switch cmd.(type) {
			case *instructions.AddCommand, *instructions.CopyCommand, *instructions.RunCommand:
				total++
			case *instructions.WorkdirCommand:
				total++
			}
		}
		ds.cmdTotal = total
		if dctx.opt.Client != nil {
			ds.ignoreCache = dctx.opt.Client.IsNoCache(st.Name)
		}
	}
	return nil
}

func (dctx *dispatchContext) resolveTarget() (*dispatchState, error) {
	if dctx.opt.Target == "" {
		return dctx.allDispatchStates.lastTarget(), nil
	}
	target, ok := dctx.allDispatchStates.findStateByName(dctx.opt.Target)
	if !ok {
		return nil, suggest.WrapError(errors.Errorf("target stage %q could not be found", dctx.opt.Target), dctx.opt.Target, dctx.allDispatchStates.names(), true)
	}
	return target, nil
}

func (dctx *dispatchContext) buildStageDependencyGraph() error {
	for _, d := range dctx.allDispatchStates.states {
		d.commands = make([]command, len(d.stage.Commands))
		for i, cmd := range d.stage.Commands {
			newCmd, err := toCommand(cmd, dctx.allDispatchStates, dctx.shlex)
			if err != nil {
				return err
			}
			d.commands[i] = newCmd
			for _, src := range newCmd.sources {
				if src != nil {
					d.deps[src] = cmd
					if src.unregistered {
						dctx.allDispatchStates.addState(src)
					}
				}
			}
		}
	}

	if err := validateCircularDependency(dctx.allDispatchStates.states); err != nil {
		return err
	}

	if len(dctx.allDispatchStates.states) == 1 {
		dctx.allDispatchStates.states[0].stageName = ""
	}
	return nil
}

func (dctx *dispatchContext) resolveStages(ctx context.Context, target *dispatchState) (map[*dispatchState]struct{}, error) {
	var allReachable map[*dispatchState]struct{}
	for {
		var err error
		allReachable, err = dctx.resolveReachableStages(ctx, dctx.allDispatchStates.states, target)
		if err != nil {
			return nil, err
		}

		// initialize onbuild triggers in case they create new dependencies
		newDeps := false
		for d := range allReachable {
			d.init()

			onbuilds := slices.Clone(d.image.Config.OnBuild)
			if d.base != nil && !d.onBuildInit {
				for _, cmd := range d.base.commands {
					if obCmd, ok := cmd.Command.(*instructions.OnbuildCommand); ok {
						onbuilds = append(onbuilds, obCmd.Expression)
					}
				}
				d.onBuildInit = true
			}
			if dctx.disableOnbuild || slices.Contains(d.stage.Reset, dfcommand.Onbuild) {
				onbuilds = nil
				d.image.Config.OnBuild = nil
			}

			if len(onbuilds) > 0 {
				if b, err := initOnBuildTriggers(d, onbuilds, dctx.allDispatchStates, dctx.shlex); err != nil {
					return nil, parser.SetLocation(err, d.stage.Location)
				} else if b {
					newDeps = true
				}
				d.image.Config.OnBuild = nil
			}
		}
		if !newDeps {
			break
		}
	}
	return allReachable, nil
}

func (dctx *dispatchContext) resolveReachableStages(ctx context.Context, all []*dispatchState, target *dispatchState) (map[*dispatchState]struct{}, error) {
	allReachable := allReachableStages(target)
	eg, ctx := errgroup.WithContext(ctx)
	for _, d := range all {
		_, reachable := allReachable[d]
		if dctx.opt.AllStages {
			reachable = true
		}
		if d.base == nil && !d.dispatched && !d.resolved {
			d.resolved = reachable // avoid re-resolving if called again after onbuild
			if d.stage.BaseName == emptyImageName && d.namedContext == nil {
				d.state = llb.Scratch()
				d.image = emptyImage(dctx.platformOpt.targetPlatform)
				d.platform = &dctx.platformOpt.targetPlatform
				if d.unregistered {
					d.dispatched = true
				}
				continue
			}
			eg.Go(func() error {
				return dctx.resolveBaseImage(ctx, d, reachable)
			})
		}
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return allReachable, nil
}

func (dctx *dispatchContext) resolveBaseImage(ctx context.Context, d *dispatchState, reachable bool) (err error) {
	defer func() {
		if err != nil {
			err = parser.WithLocation(err, d.stage.Location)
		}
		if d.unregistered {
			d.dispatched = true
		}
	}()
	origName := d.stage.BaseName
	ref, err := reference.ParseNormalizedNamed(d.stage.BaseName)
	if err != nil {
		return errors.Wrapf(err, "failed to parse stage name %q", d.stage.BaseName)
	}
	platform := d.platform
	if platform == nil {
		platform = &dctx.platformOpt.targetPlatform
	}
	d.stage.BaseName = reference.TagNameOnly(ref).String()

	var isScratch bool
	if reachable {
		if d.namedContext != nil {
			st, img, err := d.namedContext.Load(ctx)
			if err != nil {
				return err
			}
			d.dispatched = true
			d.state = *st
			if img != nil {
				img.Created = nil
				d.image = *img
				if img.Architecture != "" && img.OS != "" {
					d.platform = &ocispecs.Platform{
						OS:           img.OS,
						Architecture: img.Architecture,
						Variant:      img.Variant,
						OSVersion:    img.OSVersion,
					}
					if img.OSFeatures != nil {
						d.platform.OSFeatures = slices.Clone(img.OSFeatures)
					}
				}
			}
			return nil
		}

		nc, err := dctx.namedContext(d.stage.BaseName, dockerui.ContextOpt{
			ResolveMode:    dctx.opt.ImageResolveMode.String(),
			Platform:       platform,
			AsyncLocalOpts: d.asyncLocalOpts,
		})
		if err != nil {
			return err
		}
		if nc != nil {
			st, img, err := nc.Load(ctx)
			if err != nil {
				return err
			}
			if img == nil {
				imgp := emptyImage(*platform)
				img = &imgp
			}
			d.baseImg = cloneX(img) // immutable
			img.Created = nil
			d.image = *img
			d.state = st.Platform(*platform)
			d.platform = platform
			return nil
		}

		prefix := "["
		if dctx.opt.MultiPlatformRequested && platform != nil {
			prefix += platforms.FormatAll(*platform) + " "
		}
		prefix += "internal]"
		mutRef, dgst, dt, err := dctx.metaResolver.ResolveImageConfig(ctx, d.stage.BaseName, sourceresolver.Opt{
			LogName: fmt.Sprintf("%s load metadata for %s", prefix, d.stage.BaseName),
			ImageOpt: &sourceresolver.ResolveImageOpt{
				Platform:    platform,
				ResolveMode: dctx.opt.ImageResolveMode.String(),
			},
		})
		if err != nil {
			return suggest.WrapError(errors.Wrap(err, origName), origName, append(dctx.allDispatchStates.names(), commonImageNames()...), true)
		}

		if ref.String() != mutRef {
			ref, err = reference.ParseNormalizedNamed(mutRef)
			if err != nil {
				return errors.Wrapf(err, "failed to parse ref %q", mutRef)
			}
		}
		var img dockerspec.DockerOCIImage
		if err := json.Unmarshal(dt, &img); err != nil {
			return errors.Wrap(err, "failed to parse image config")
		}
		d.baseImg = cloneX(&img) // immutable
		img.Created = nil
		if d.platform == nil && dctx.platformOpt.implicitTarget {
			p := autoDetectPlatform(img, *platform, dctx.platformOpt.buildPlatforms)
			platform = &p
		}
		if dgst != "" {
			ref, err = reference.WithDigest(ref, dgst)
			if err != nil {
				return err
			}
		}
		d.stage.BaseName = ref.String()
		if len(img.RootFS.DiffIDs) == 0 {
			isScratch = true
			// schema1 images can't return diffIDs so double check :(
			for _, h := range img.History {
				if !h.EmptyLayer {
					isScratch = false
					break
				}
			}
		}
		d.image = img
	}
	if isScratch {
		d.state = llb.Scratch()
	} else {
		d.state = llb.Image(d.stage.BaseName,
			dfCmd(d.stage.SourceCode),
			llb.Platform(*platform),
			dctx.opt.ImageResolveMode,
			llb.WithCustomName(prefixCommand(d, "FROM "+d.stage.BaseName, dctx.opt.MultiPlatformRequested, platform, emptyEnvs{})),
			location(dctx.opt.SourceMap, d.stage.Location),
		)
		if reachable {
			validateBaseImagePlatform(origName, *platform, d.image.Platform, d.stage.Location, dctx.lint)
		}
	}
	d.platform = platform
	return nil
}

func (dctx *dispatchContext) dispatchStages(ctx context.Context, allReachable map[*dispatchState]struct{}, target *dispatchState) (map[string]struct{}, *mutableOutput, error) {
	buildContext := &mutableOutput{}
	ctxPaths := map[string]struct{}{}

	var dockerIgnoreMatcher *patternmatcher.PatternMatcher
	if dctx.opt.Client != nil {
		dockerIgnorePatterns, err := dctx.opt.Client.DockerIgnorePatterns(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(dockerIgnorePatterns) > 0 {
			dockerIgnoreMatcher, err = patternmatcher.New(dockerIgnorePatterns)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	for _, d := range dctx.allDispatchStates.states {
		if !dctx.opt.AllStages {
			if _, ok := allReachable[d]; !ok || d.dispatched {
				continue
			}
		}
		d.init()
		d.dispatched = true

		// Ensure platform is set.
		if d.platform == nil {
			d.platform = &d.opt.targetPlatform
		}

		resetBaseConfig(d)

		// make sure that PATH is always set
		if _, ok := shell.EnvsFromSlice(d.image.Config.Env).Get("PATH"); !ok {
			var osName string
			if d.platform != nil {
				osName = d.platform.OS
			}
			// except for Windows, leave that to the OS. #5445
			if osName != "windows" {
				d.image.Config.Env = append(d.image.Config.Env, "PATH="+system.DefaultPathEnv(osName))
			}
		}

		// initialize base metadata from image conf
		for _, env := range d.image.Config.Env {
			k, v := parseKeyValue(env)
			d.state = d.state.AddEnv(k, v)
		}
		if dctx.opt.Hostname != "" {
			d.state = d.state.Hostname(dctx.opt.Hostname)
		}
		if d.image.Config.WorkingDir != "" {
			if err := dispatchWorkdir(d, &instructions.WorkdirCommand{Path: d.image.Config.WorkingDir}, false, nil); err != nil {
				return nil, nil, parser.WithLocation(err, d.stage.Location)
			}
		}
		if d.image.Config.User != "" {
			if err := dispatchUser(d, &instructions.UserCommand{User: d.image.Config.User}, false); err != nil {
				return nil, nil, parser.WithLocation(err, d.stage.Location)
			}
		}

		d.state = d.state.Network(dctx.opt.NetworkMode)

		dopt := dispatchOpt{
			allDispatchStates:   dctx.allDispatchStates,
			globalArgs:          dctx.globalArgs,
			buildArgValues:      dctx.opt.BuildArgs,
			shlex:               dctx.shlex,
			buildContext:        llb.NewState(buildContext),
			proxyEnv:            dctx.proxyEnv,
			cacheIDNamespace:    dctx.opt.CacheIDNamespace,
			buildPlatforms:      dctx.platformOpt.buildPlatforms,
			targetPlatform:      dctx.platformOpt.targetPlatform,
			extraHosts:          dctx.opt.ExtraHosts,
			gitAdvice:           dctx.opt.GitAdvice,
			gitDebugCommands:    dctx.opt.GitDebugCommands,
			shmSize:             dctx.opt.ShmSize,
			ulimit:              dctx.opt.Ulimits,
			devices:             dctx.opt.Devices,
			cgroupParent:        dctx.opt.CgroupParent,
			linuxResources:      dctx.opt.LinuxResources,
			llbCaps:             dctx.opt.LLBCaps,
			sourceMap:           dctx.opt.SourceMap,
			lint:                dctx.lint,
			dockerIgnoreMatcher: dockerIgnoreMatcher,
		}

		for _, cmd := range d.commands {
			if err := dispatch(d, cmd, dopt); err != nil {
				return nil, nil, parser.WithLocation(err, cmd.Location())
			}
		}
		d.opt = dopt

		for p := range d.ctxPaths {
			ctxPaths[p] = struct{}{}
		}

		for _, name := range []string{sbomScanContext, sbomScanStage} {
			var b bool
			if v, ok := d.opt.globalArgs.Get(name); ok {
				b = isEnabledForStage(d.stageName, v)
			}
			for _, kv := range d.buildArgs {
				if kv.Key == name && kv.Value != nil {
					b = isEnabledForStage(d.stageName, *kv.Value)
				}
			}
			if b {
				if name == sbomScanContext {
					d.scanContext = true
				} else {
					d.scanStage = true
				}
			}
		}
	}

	// Ensure the entirety of the target state is marked as used.
	// This is done after we've already evaluated every stage to ensure
	// the paths attribute is set correctly.
	target.paths["/"] = struct{}{}

	return ctxPaths, buildContext, nil
}

func (dctx *dispatchContext) finalizeResultImage(ctx context.Context, target *dispatchState, ctxPaths map[string]struct{}, buildContext *mutableOutput) error {
	if len(dctx.opt.Labels) != 0 && target.image.Config.Labels == nil {
		target.image.Config.Labels = make(map[string]string, len(dctx.opt.Labels))
	}
	maps.Copy(target.image.Config.Labels, dctx.opt.Labels)

	// If lint.Error() returns an error, it means that
	// there were warnings, and that our linter has been
	// configured to return an error on warnings,
	// so we appropriately return that error here.
	if err := dctx.lint.Error(); err != nil {
		return err
	}

	opts := filterPaths(ctxPaths)
	bctx := dctx.opt.MainContext
	if dctx.opt.Client != nil {
		var err error
		bctx, err = dctx.opt.Client.MainContext(ctx, opts...)
		if err != nil {
			return err
		}
	} else if bctx == nil {
		bctx = dockerui.DefaultMainContext(opts...)
	}

	buildContext.Output = bctx.Output()

	defaults := []llb.ConstraintsOpt{
		llb.Platform(dctx.platformOpt.targetPlatform),
	}
	if dctx.opt.LLBCaps != nil {
		defaults = append(defaults, llb.WithCaps(*dctx.opt.LLBCaps))
	}
	target.state = target.state.SetMarshalDefaults(defaults...)

	if !dctx.platformOpt.implicitTarget {
		sameOsArch := dctx.platformOpt.targetPlatform.OS == target.image.OS && dctx.platformOpt.targetPlatform.Architecture == target.image.Architecture
		target.image.OS = dctx.platformOpt.targetPlatform.OS
		target.image.Architecture = dctx.platformOpt.targetPlatform.Architecture
		if dctx.platformOpt.targetPlatform.Variant != "" || !sameOsArch {
			target.image.Variant = dctx.platformOpt.targetPlatform.Variant
		}
		if dctx.platformOpt.targetPlatform.OSVersion != "" || !sameOsArch {
			target.image.OSVersion = dctx.platformOpt.targetPlatform.OSVersion
		}
		if dctx.platformOpt.targetPlatform.OSFeatures != nil {
			target.image.OSFeatures = slices.Clone(dctx.platformOpt.targetPlatform.OSFeatures)
		}
	}
	target.image.Platform = platforms.Normalize(target.image.Platform)

	return nil
}

func toCommand(ic instructions.Command, allDispatchStates *dispatchStates, shlex *shell.Lex) (command, error) {
	cmd := command{Command: ic}
	if c, ok := ic.(*instructions.CopyCommand); ok {
		if c.From != "" {
			res, err := shlex.ProcessWordWithMatches(c.From, shell.EnvsFromSlice(nil))
			if err != nil {
				return command{}, err
			}
			if res.Result != c.From {
				return command{}, errors.Errorf("variable expansion is not supported for --from, define a new stage with FROM using ARG from global scope as a workaround")
			}
			var stn *dispatchState
			index, err := strconv.Atoi(c.From)
			if err != nil {
				stn, ok = allDispatchStates.findStateByName(c.From)
				if !ok {
					stn = &dispatchState{
						stage:        instructions.Stage{BaseName: c.From, Location: c.Location()},
						deps:         make(map[*dispatchState]instructions.Command),
						paths:        make(map[string]struct{}),
						unregistered: true,
					}
				}
			} else {
				stn, err = allDispatchStates.findStateByIndex(index)
				if err != nil {
					return command{}, err
				}
			}
			cmd.sources = []*dispatchState{stn}
		}
	}

	if ok := detectRunMount(&cmd, allDispatchStates); ok {
		return cmd, nil
	}

	return cmd, nil
}

type dispatchOpt struct {
	allDispatchStates   *dispatchStates
	globalArgs          shell.EnvGetter
	buildArgValues      map[string]string
	shlex               *shell.Lex
	buildContext        llb.State
	proxyEnv            *llb.ProxyEnv
	cacheIDNamespace    string
	targetPlatform      ocispecs.Platform
	buildPlatforms      []ocispecs.Platform
	extraHosts          []llb.HostIP
	gitAdvice           bool
	gitDebugCommands    bool
	shmSize             int64
	ulimit              []*pb.Ulimit
	devices             []*pb.CDIDevice
	cgroupParent        string
	linuxResources      *pb.LinuxResources
	llbCaps             *apicaps.CapSet
	sourceMap           *llb.SourceMap
	lint                *linter.Linter
	dockerIgnoreMatcher *patternmatcher.PatternMatcher
}

func getEnv(state llb.State) shell.EnvGetter {
	return &envsFromState{state: &state}
}

type envsFromState struct {
	state *llb.State
	once  sync.Once
	env   shell.EnvGetter
}

func (e *envsFromState) init() {
	env, err := e.state.Env(context.TODO())
	if err != nil {
		return
	}
	e.env = env
}

func (e *envsFromState) Get(key string) (string, bool) {
	e.once.Do(e.init)
	return e.env.Get(key)
}

func (e *envsFromState) Keys() []string {
	e.once.Do(e.init)
	return e.env.Keys()
}

func dispatch(d *dispatchState, cmd command, opt dispatchOpt) error {
	opt.lint = opt.lint.WithMergedConfigFromComments(cmd.Comments())

	d.cmdIsOnBuild = cmd.isOnBuild
	var err error
	// ARG command value could be ignored, so defer handling the expansion error
	_, isArg := cmd.Command.(*instructions.ArgCommand)
	if ex, ok := cmd.Command.(instructions.SupportsSingleWordExpansion); ok && !isArg {
		err := ex.Expand(func(word string) (string, error) {
			env := getEnv(d.state)
			newword, unmatched, err := opt.shlex.ProcessWord(word, env)
			reportUnmatchedVariables(cmd, d.buildArgs, env, unmatched, &opt)
			return newword, err
		})
		if err != nil {
			return err
		}
	}
	if ex, ok := cmd.Command.(instructions.SupportsSingleWordExpansionRaw); ok {
		err := ex.ExpandRaw(func(word string) (string, error) {
			lex := shell.NewLex('\\')
			lex.SkipProcessQuotes = true
			env := getEnv(d.state)
			newword, unmatched, err := lex.ProcessWord(word, env)
			reportUnmatchedVariables(cmd, d.buildArgs, env, unmatched, &opt)
			return newword, err
		})
		if err != nil {
			return err
		}
	}

	switch c := cmd.Command.(type) {
	case *instructions.MaintainerCommand:
		err = dispatchMaintainer(d, c)
	case *instructions.EnvCommand:
		err = dispatchEnv(d, c, opt.lint)
	case *instructions.RunCommand:
		err = dispatchRun(d, c, opt.proxyEnv, cmd.sources, opt)
	case *instructions.WorkdirCommand:
		err = dispatchWorkdir(d, c, true, &opt)
	case *instructions.AddCommand:
		err = dispatchCopy(d, copyConfig{
			params:          c.SourcesAndDest,
			excludePatterns: c.ExcludePatterns,
			source:          opt.buildContext,
			isAddCommand:    true,
			cmdToPrint:      c,
			chown:           c.Chown,
			chmod:           c.Chmod,
			link:            c.Link,
			keepGitDir:      c.KeepGitDir,
			checksum:        c.Checksum,
			unpack:          c.Unpack,
			location:        c.Location(),
			ignoreMatcher:   opt.dockerIgnoreMatcher,
			opt:             opt,
		})
		if err == nil {
			for _, src := range c.SourcePaths {
				if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
					d.ctxPaths[path.Join("/", filepath.ToSlash(src))] = struct{}{}
				}
			}
		}
	case *instructions.LabelCommand:
		err = dispatchLabel(d, c, opt.lint)
	case *instructions.OnbuildCommand:
		err = dispatchOnbuild(d, c)
	case *instructions.CmdCommand:
		err = dispatchCmd(d, c, opt.lint)
	case *instructions.EntrypointCommand:
		err = dispatchEntrypoint(d, c, opt.lint)
	case *instructions.HealthCheckCommand:
		err = dispatchHealthcheck(d, c, opt.lint)
	case *instructions.ExposeCommand:
		err = dispatchExpose(d, c, &opt)
	case *instructions.UserCommand:
		err = dispatchUser(d, c, true)
	case *instructions.VolumeCommand:
		err = dispatchVolume(d, c)
	case *instructions.StopSignalCommand:
		err = dispatchStopSignal(d, c)
	case *instructions.ShellCommand:
		err = dispatchShell(d, c)
	case *instructions.ArgCommand:
		err = dispatchArg(d, c, &opt)
	case *instructions.CopyCommand:
		l := opt.buildContext
		var ignoreMatcher *patternmatcher.PatternMatcher
		if len(cmd.sources) != 0 {
			src := cmd.sources[0]
			if !src.dispatched {
				return errors.Errorf("cannot copy from stage %q, it needs to be defined before current stage %q", c.From, d.stageName)
			}
			l = src.state
		} else {
			ignoreMatcher = opt.dockerIgnoreMatcher
		}
		err = dispatchCopy(d, copyConfig{
			params:          c.SourcesAndDest,
			excludePatterns: c.ExcludePatterns,
			source:          l,
			isAddCommand:    false,
			cmdToPrint:      c,
			chown:           c.Chown,
			chmod:           c.Chmod,
			link:            c.Link,
			parents:         c.Parents,
			location:        c.Location(),
			ignoreMatcher:   ignoreMatcher,
			opt:             opt,
		})
		if err == nil {
			if len(cmd.sources) == 0 {
				for _, src := range c.SourcePaths {
					d.ctxPaths[path.Join("/", filepath.ToSlash(src))] = struct{}{}
				}
			} else {
				source := cmd.sources[0]
				if source.paths == nil {
					source.paths = make(map[string]struct{})
				}
				for _, src := range c.SourcePaths {
					source.paths[path.Join("/", filepath.ToSlash(src))] = struct{}{}
				}
			}
		}
	default:
	}
	return err
}

type dispatchState struct {
	opt          dispatchOpt
	state        llb.State
	image        dockerspec.DockerOCIImage
	namedContext *dockerui.NamedContext
	platform     *ocispecs.Platform
	stage        instructions.Stage
	base         *dispatchState
	baseImg      *dockerspec.DockerOCIImage // immutable, unlike image
	dispatched   bool
	resolved     bool // resolved is set to true if base image has been resolved
	onBuildInit  bool
	deps         map[*dispatchState]instructions.Command
	buildArgs    []instructions.KeyValuePairOptional
	commands     []command
	// ctxPaths marks the paths this dispatchState uses from the build context.
	ctxPaths map[string]struct{}
	// paths marks the paths that are used by this dispatchState.
	paths          map[string]struct{}
	ignoreCache    bool
	unregistered   bool
	stageName      string
	cmdIndex       int
	cmdIsOnBuild   bool
	cmdTotal       int
	prefixPlatform bool
	outline        outlineCapture
	epoch          *time.Time
	scanStage      bool
	scanContext    bool
	// workdirSet is set to true if a workdir has been set
	// within the current dockerfile.
	workdirSet bool

	entrypoint  instructionTracker
	cmd         instructionTracker
	healthcheck instructionTracker
}

func (ds *dispatchState) asyncLocalOpts() []llb.LocalOption {
	return filterPaths(ds.paths)
}

// init is invoked when the dispatch state inherits its attributes
// from the base image.
func (ds *dispatchState) init() {
	// mark as initialized, used to determine states that have not been dispatched yet
	if ds.base == nil {
		return
	}

	ds.state = ds.base.state
	ds.platform = ds.base.platform
	ds.image = clone(ds.base.image)
	// onbuild triggers to not carry over from base stage
	ds.image.Config.OnBuild = nil
	ds.baseImg = cloneX(ds.base.baseImg)
	// Utilize the same path index as our base image so we propagate
	// the paths we use back to the base image.
	ds.paths = ds.base.paths
	ds.workdirSet = ds.base.workdirSet
	ds.buildArgs = append(ds.buildArgs, ds.base.buildArgs...)
}

type dispatchStates struct {
	states       []*dispatchState
	statesByName map[string]*dispatchState
}

func newDispatchStates() *dispatchStates {
	return &dispatchStates{statesByName: map[string]*dispatchState{}}
}

func (dss *dispatchStates) names() []string {
	names := make([]string, 0, len(dss.states))
	for _, s := range dss.states {
		if s.stageName != "" {
			names = append(names, s.stageName)
		}
	}
	return names
}

func (dss *dispatchStates) addState(ds *dispatchState) {
	dss.states = append(dss.states, ds)

	if d, ok := dss.statesByName[ds.stage.BaseName]; ok {
		ds.base = d
		ds.outline = d.outline.clone()
	}
	if ds.stage.Name != "" {
		dss.statesByName[strings.ToLower(ds.stage.Name)] = ds
	}
}

func (dss *dispatchStates) findStateByName(name string) (*dispatchState, bool) {
	ds, ok := dss.statesByName[strings.ToLower(name)]
	return ds, ok
}

func (dss *dispatchStates) findStateByIndex(index int) (*dispatchState, error) {
	if index < 0 || index >= len(dss.states) {
		return nil, errors.Errorf("invalid stage index %d", index)
	}

	return dss.states[index], nil
}

func (dss *dispatchStates) lastTarget() *dispatchState {
	return dss.states[len(dss.states)-1]
}

type command struct {
	instructions.Command
	sources   []*dispatchState
	isOnBuild bool
}

// initOnBuildTriggers initializes the onbuild triggers and creates the commands and dependecies for them.
// It returns true if there were any new dependencies added that need to be resolved.
func initOnBuildTriggers(d *dispatchState, triggers []string, allDispatchStates *dispatchStates, shlex *shell.Lex) (bool, error) {
	hasNewDeps := false
	commands := make([]command, 0, len(triggers))

	for _, trigger := range triggers {
		ast, err := parser.Parse(strings.NewReader(trigger))
		if err != nil {
			return false, err
		}
		if len(ast.AST.Children) != 1 {
			return false, errors.New("onbuild trigger should be a single expression")
		}
		node := ast.AST.Children[0]
		// reset the location to the onbuild trigger
		node.StartLine, node.EndLine = rangeStartEnd(d.stage.Location)
		ic, err := instructions.ParseCommand(ast.AST.Children[0])
		if err != nil {
			return false, err
		}
		cmd, err := toCommand(ic, allDispatchStates, shlex)
		if err != nil {
			return false, err
		}
		cmd.isOnBuild = true
		if len(cmd.sources) > 0 {
			hasNewDeps = true
		}

		commands = append(commands, cmd)

		for _, src := range cmd.sources {
			if src != nil {
				d.deps[src] = cmd
				if src.unregistered {
					allDispatchStates.addState(src)
				}
			}
		}
	}
	d.commands = append(commands, d.commands...)
	d.cmdTotal += len(commands)

	return hasNewDeps, nil
}

func dispatchEnv(d *dispatchState, c *instructions.EnvCommand, lint *linter.Linter) error {
	if c.Unset {
		keys := make([]string, 0, len(c.Env))
		for _, e := range c.Env {
			if e.Key == "" {
				return errors.New("ENV --unset names can not be blank")
			}
			unsetEnv(d, e.Key)
			keys = append(keys, e.Key)
		}
		return commitToHistory(&d.image, "ENV --unset "+strings.Join(keys, " "), false, nil, d.epoch)
	}
	commitMessage := bytes.NewBufferString("ENV")
	for _, e := range c.Env {
		if e.NoDelim {
			msg := linter.RuleLegacyKeyValueFormat.Format(c.Name())
			lint.Run(&linter.RuleLegacyKeyValueFormat, c.Location(), msg)
		}
		validateNoSecretKey("ENV", e.Key, c.Location(), lint)
		commitMessage.WriteString(" " + e.String())
		d.state = d.state.AddEnv(e.Key, e.Value)
		d.image.Config.Env = addEnv(d.image.Config.Env, e.Key, e.Value)
	}
	return commitToHistory(&d.image, commitMessage.String(), false, nil, d.epoch)
}

// unsetEnv removes an environment variable and a build arg with the same name.
func unsetEnv(d *dispatchState, key string) {
	for _, k := range getEnv(d.state).Keys() {
		if shell.EqualEnvKeys(k, key) {
			d.state = d.state.DelEnv(k)
		}
	}
	d.image.Config.Env = delEnv(d.image.Config.Env, key)
	d.buildArgs = slices.DeleteFunc(d.buildArgs, func(arg instructions.KeyValuePairOptional) bool {
		return shell.EqualEnvKeys(arg.Key, key)
	})
}

// resetBaseConfig clears the base image config fields set with FROM --reset.
// ONBUILD triggers are skipped when the stages are resolved.
func resetBaseConfig(d *dispatchState) {
	for _, k := range d.stage.Reset {
		switch k {
		case dfcommand.Cmd:
			d.image.Config.Cmd = nil
		case dfcommand.Entrypoint:
			d.image.Config.Entrypoint = nil
		case dfcommand.Env:
			for _, env := range slices.Clone(d.image.Config.Env) {
				key, _ := parseKeyValue(env)
				unsetEnv(d, key)
			}
		case dfcommand.Expose:
			d.image.Config.ExposedPorts = nil
		case dfcommand.Healthcheck:
			d.image.Config.Healthcheck = nil
		case dfcommand.Label:
			d.image.Config.Labels = nil
		case dfcommand.Shell:
			d.image.Config.Shell = nil
		case dfcommand.StopSignal:
			d.image.Config.StopSignal = ""
		case dfcommand.User:
			d.image.Config.User = ""
			d.state = d.state.User("")
		case dfcommand.Volume:
			d.image.Config.Volumes = nil
		case dfcommand.Workdir:
			d.image.Config.WorkingDir = ""
			d.state = d.state.Dir("/")
			// workdir no longer depends on the base image
			d.workdirSet = true
		}
	}
	// ArgsEscaped applies to both cmd and entrypoint
	if len(d.stage.Reset) > 0 && len(d.image.Config.Cmd) == 0 && len(d.image.Config.Entrypoint) == 0 {
		d.image.Config.ArgsEscaped = false //nolint:staticcheck // ignore SA1019: field is deprecated in OCI Image spec, but used for backward-compatibility with Docker image spec.
	}
}

func dispatchRun(d *dispatchState, c *instructions.RunCommand, proxy *llb.ProxyEnv, sources []*dispatchState, dopt dispatchOpt) error {
	var opt []llb.RunOption

	customname := c.String()

	// Run command can potentially access any file. Mark the full filesystem as used.
	d.paths["/"] = struct{}{}

	args := c.CmdLine
	if len(c.Files) > 0 {
		if len(args) != 1 || !c.PrependShell {
			return errors.Errorf("parsing produced an invalid run command: %v", args)
		}

		if heredoc := parser.MustParseHeredoc(args[0]); heredoc != nil {
			data := c.Files[0].Data
			if c.Files[0].Chomp {
				data = parser.ChompHeredocContent(data)
			}

			if d.image.OS != "windows" && strings.HasPrefix(data, "#!") {
				// This is a single heredoc with a shebang, so create a file
				// and run it.
				// NOTE: choosing to expand doesn't really make sense here, so
				// we silently ignore that option if it was provided.
				sourcePath := "/"
				destPath := "/dev/pipes/"

				f := c.Files[0].Name
				st := llb.Scratch().Dir(sourcePath).File(
					llb.Mkfile(f, 0755, []byte(data)),
					dockerui.WithInternalName("preparing inline document"),
					llb.Platform(*d.platform),
				)

				mount := llb.AddMount(destPath, st, llb.SourcePath(sourcePath), llb.Readonly)
				opt = append(opt, mount)

				args = []string{path.Join(destPath, f)}
			} else {
				// Just a simple heredoc, so just run the contents in the
				// shell: this creates the effect of a "fake"-heredoc, so that
				// the syntax can still be used for shells that don't support
				// heredocs directly.
				// NOTE: like above, we ignore the expand option.
				args = []string{data}
			}
			customname += fmt.Sprintf(" (%s)", summarizeHeredoc(c.Files[0].Data))
		} else {
			// More complex heredoc, so reconstitute it, and pass it to the
			// shell to handle.
			var b strings.Builder
			b.WriteString(args[0])
			for _, file := range c.Files {
				b.WriteByte('\n')
				b.WriteString(file.Data)
				b.WriteString(file.Name)
			}
			args = []string{b.String()}
		}
	}
	if c.PrependShell {
		// Don't pass the linter function because we do not report a warning for
		// shell usage on run commands.
		args = withShell(d.image, args)
	}

	opt = append(opt, llb.Args(args), dfCmd(c), location(dopt.sourceMap, c.Location()))
	if d.ignoreCache {
		opt = append(opt, llb.IgnoreCache)
	}
	if proxy != nil {
		opt = append(opt, llb.WithProxy(*proxy))
	}

	runMounts, err := dispatchRunMounts(d, c, sources, dopt)
	if err != nil {
		return err
	}
	opt = append(opt, runMounts...)

	securityOpt, err := dispatchRunSecurity(c)
	if err != nil {
		return err
	}
	if securityOpt != nil {
		opt = append(opt, securityOpt)
	}

	networkOpt, err := dispatchRunNetwork(c)
	if err != nil {
		return err
	}
	if networkOpt != nil {
		opt = append(opt, networkOpt)
	}

	if dopt.llbCaps != nil && dopt.llbCaps.Supports(pb.CapExecMetaUlimit) == nil {
		for _, u := range dopt.ulimit {
			opt = append(opt, llb.AddUlimit(llb.UlimitName(u.Name), u.Soft, u.Hard))
		}
	}

	if dopt.llbCaps != nil && dopt.llbCaps.Supports(pb.CapExecMetaCDI) == nil {
		for _, device := range dopt.devices {
			deviceOpts := []llb.CDIDeviceOption{
				llb.CDIDeviceName(device.Name),
			}
			if device.Optional {
				deviceOpts = append(deviceOpts, llb.CDIDeviceOptional)
			}
			opt = append(opt, llb.AddCDIDevice(deviceOpts...))
		}
		runDevices, err := dispatchRunDevices(c)
		if err != nil {
			return err
		}
		opt = append(opt, runDevices...)
	}

	shlex := *dopt.shlex
	shlex.RawQuotes = true
	shlex.RawEscapes = true
	shlex.SkipUnsetEnv = true

	pl, err := d.state.GetPlatform(context.TODO())
	if err != nil {
		return err
	}
	env := getEnv(d.state)
	opt = append(opt, llb.WithCustomName(prefixCommand(d, uppercaseCmd(processCmdEnv(&shlex, customname, withSecretEnvMask(c, env))), d.prefixPlatform, pl, env)))
	for _, h := range dopt.extraHosts {
		opt = append(opt, llb.AddExtraHost(h.Host, h.IP))
	}

	if dopt.llbCaps != nil && dopt.llbCaps.Supports(pb.CapExecMountTmpfsSize) == nil {
		if dopt.shmSize > 0 {
			opt = append(opt, llb.AddMount("/dev/shm", llb.Scratch(), llb.Tmpfs(llb.TmpfsSize(dopt.shmSize))))
		}
	}

	if dopt.llbCaps != nil && dopt.llbCaps.Supports(pb.CapExecMetaCgroupParent) == nil {
		if len(dopt.cgroupParent) > 0 {
			opt = append(opt, llb.WithCgroupParent(dopt.cgroupParent))
		}
	}

	if dopt.llbCaps != nil && dopt.llbCaps.Supports(pb.CapExecMetaLinuxResources) == nil {
		if dopt.linuxResources != nil {
			opt = append(opt, llb.WithLinuxResources(llb.LinuxResources{
				Memory:     dopt.linuxResources.Memory,
				MemorySwap: dopt.linuxResources.MemorySwap,
				CPUShares:  dopt.linuxResources.CpuShares,
				CPUPeriod:  dopt.linuxResources.CpuPeriod,
				CPUQuota:   dopt.linuxResources.CpuQuota,
				CpusetCpus: dopt.linuxResources.CpusetCpus,
				CpusetMems: dopt.linuxResources.CpusetMems,
			}))
		}
	}

	d.state = d.state.Run(opt...).Root()
	return commitToHistory(&d.image, "RUN "+runCommandString(args, d.buildArgs, env), true, &d.state, d.epoch)
}

func dispatchWorkdir(d *dispatchState, c *instructions.WorkdirCommand, commit bool, opt *dispatchOpt) error {
	if commit {
		// This linter rule checks if workdir has been set to an absolute value locally
		// within the current dockerfile. Absolute paths in base images are ignored
		// because they might change and it is not advised to rely on them.
		//
		// We only run this check when commit is true. Commit is true when we are performing
		// this operation on a local call to workdir rather than one coming from
		// the base image. We only check the first instance of workdir being set
		// so successive relative paths are ignored because every instance is fixed
		// by fixing the first one.
		if !d.workdirSet && !system.IsAbs(c.Path, d.platform.OS) {
			msg := linter.RuleWorkdirRelativePath.Format(c.Path)
			opt.lint.Run(&linter.RuleWorkdirRelativePath, c.Location(), msg)
		}
		d.workdirSet = true
	}

	wd, err := system.NormalizeWorkdir(d.image.Config.WorkingDir, c.Path, d.platform.OS)
	if err != nil {
		return errors.Wrap(err, "normalizing workdir")
	}

	// NormalizeWorkdir returns paths with platform specific separators. For Windows
	// this will be of the form: \some\path, which is needed later when we pass it to
	// HCS.
	d.image.Config.WorkingDir = wd

	// From this point forward, we can use UNIX style paths.
	wd = system.ToSlash(wd, d.platform.OS)
	d.state = d.state.Dir(wd)

	if commit {
		withLayer := false
		if wd != "/" {
			mkdirOpt := []llb.MkdirOption{llb.WithParents(true)}
			if user := d.image.Config.User; user != "" {
				mkdirOpt = append(mkdirOpt, llb.WithUser(user))
			}
			if d.epoch != nil {
				mkdirOpt = append(mkdirOpt, llb.WithCreatedTime(*d.epoch))
			}
			platform := opt.targetPlatform
			if d.platform != nil {
				platform = *d.platform
			}
			env := getEnv(d.state)
			d.state = d.state.File(llb.Mkdir(wd, 0755, mkdirOpt...),
				llb.WithCustomName(prefixCommand(d, uppercaseCmd(processCmdEnv(opt.shlex, c.String(), env)), d.prefixPlatform, &platform, env)),
				location(opt.sourceMap, c.Location()),
				llb.Platform(*d.platform),
			)
			withLayer = true
		}
		return commitToHistory(&d.image, "WORKDIR "+wd, withLayer, nil, d.epoch)
	}
	return nil
}

func dispatchMaintainer(d *dispatchState, c *instructions.MaintainerCommand) error {
	d.image.Author = c.Maintainer
	return commitToHistory(&d.image, fmt.Sprintf("MAINTAINER %v", c.Maintainer), false, nil, d.epoch)
}

func dispatchLabel(d *dispatchState, c *instructions.LabelCommand, lint *linter.Linter) error {
	if c.Unset {
		keys := make([]string, 0, len(c.Labels))
		for _, v := range c.Labels {
			if v.Key == "" {
				return errors.New("LABEL --unset names can not be blank")
			}
			delete(d.image.Config.Labels, v.Key)
			keys = append(keys, v.Key)
		}
		return commitToHistory(&d.image, "LABEL --unset "+strings.Join(keys, " "), false, nil, d.epoch)
	}
	commitMessage := bytes.NewBufferString("LABEL")
	if d.image.Config.Labels == nil {
		d.image.Config.Labels = make(map[string]string, len(c.Labels))
	}
	for _, v := range c.Labels {
		if v.NoDelim {
			msg := linter.RuleLegacyKeyValueFormat.Format(c.Name())
			lint.Run(&linter.RuleLegacyKeyValueFormat, c.Location(), msg)
		}
		d.image.Config.Labels[v.Key] = v.Value
		commitMessage.WriteString(" " + v.String())
	}
	return commitToHistory(&d.image, commitMessage.String(), false, nil, d.epoch)
}

func dispatchOnbuild(d *dispatchState, c *instructions.OnbuildCommand) error {
	d.image.Config.OnBuild = append(d.image.Config.OnBuild, c.Expression)
	return nil
}

func dispatchCmd(d *dispatchState, c *instructions.CmdCommand, lint *linter.Linter) error {
	validateUsedOnce(c, &d.cmd, lint)

	args := c.CmdLine
	if c.PrependShell {
		if len(d.image.Config.Shell) == 0 {
			msg := linter.RuleJSONArgsRecommended.Format(c.Name())
			lint.Run(&linter.RuleJSONArgsRecommended, c.Location(), msg)
		}
		args = withShell(d.image, args)
	}
	d.image.Config.Cmd = args
	d.image.Config.ArgsEscaped = true //nolint:staticcheck // ignore SA1019: field is deprecated in OCI Image spec, but used for backward-compatibility with Docker image spec.
	return commitToHistory(&d.image, fmt.Sprintf("CMD %q", args), false, nil, d.epoch)
}

func dispatchEntrypoint(d *dispatchState, c *instructions.EntrypointCommand, lint *linter.Linter) error {
	validateUsedOnce(c, &d.entrypoint, lint)

	args := c.CmdLine
	if c.PrependShell {
		if len(d.image.Config.Shell) == 0 {
			msg := linter.RuleJSONArgsRecommended.Format(c.Name())
			lint.Run(&linter.RuleJSONArgsRecommended, c.Location(), msg)
		}
		args = withShell(d.image, args)
	}
	d.image.Config.Entrypoint = args
	if !d.cmd.IsSet {
		d.image.Config.Cmd = nil
	}
	return commitToHistory(&d.image, fmt.Sprintf("ENTRYPOINT %q", args), false, nil, d.epoch)
}

func dispatchHealthcheck(d *dispatchState, c *instructions.HealthCheckCommand, lint *linter.Linter) error {
	validateUsedOnce(c, &d.healthcheck, lint)
	d.image.Config.Healthcheck = &dockerspec.HealthcheckConfig{
		Test:          c.Health.Test,
		Interval:      c.Health.Interval,
		Timeout:       c.Health.Timeout,
		StartPeriod:   c.Health.StartPeriod,
		StartInterval: c.Health.StartInterval,
		Retries:       c.Health.Retries,
	}
	return commitToHistory(&d.image, fmt.Sprintf("HEALTHCHECK %+v", *d.image.Config.Healthcheck), false, nil, d.epoch)
}

func dispatchUser(d *dispatchState, c *instructions.UserCommand, commit bool) error {
	d.state = d.state.User(c.User)
	d.image.Config.User = c.User
	if commit {
		return commitToHistory(&d.image, fmt.Sprintf("USER %v", c.User), false, nil, d.epoch)
	}
	return nil
}

func dispatchVolume(d *dispatchState, c *instructions.VolumeCommand) error {
	if slices.Contains(c.Volumes, "") {
		return errors.New("VOLUME specified can not be an empty string")
	}
	if c.Unset {
		for _, v := range c.Volumes {
			v = path.Clean(v)
			for k := range d.image.Config.Volumes {
				if path.Clean(k) == v {
					delete(d.image.Config.Volumes, k)
				}
			}
		}
		return commitToHistory(&d.image, fmt.Sprintf("VOLUME --unset %v", c.Volumes), false, nil, d.epoch)
	}
	if d.image.Config.Volumes == nil {
		d.image.Config.Volumes = map[string]struct{}{}
	}
	for _, v := range c.Volumes {
		d.image.Config.Volumes[v] = struct{}{}
	}
	return commitToHistory(&d.image, fmt.Sprintf("VOLUME %v", c.Volumes), false, nil, d.epoch)
}

func dispatchStopSignal(d *dispatchState, c *instructions.StopSignalCommand) error {
	if _, err := signal.ParseSignal(c.Signal); err != nil {
		return err
	}
	d.image.Config.StopSignal = c.Signal
	return commitToHistory(&d.image, fmt.Sprintf("STOPSIGNAL %v", c.Signal), false, nil, d.epoch)
}

func dispatchShell(d *dispatchState, c *instructions.ShellCommand) error {
	d.image.Config.Shell = c.Shell
	return commitToHistory(&d.image, fmt.Sprintf("SHELL %v", c.Shell), false, nil, d.epoch)
}

func dispatchArg(d *dispatchState, c *instructions.ArgCommand, opt *dispatchOpt) error {
	commitStrs := make([]string, 0, len(c.Args))
	for _, arg := range c.Args {
		validateNoSecretKey("ARG", arg.Key, c.Location(), opt.lint)
		_, hasValue := opt.buildArgValues[arg.Key]
		hasDefault := arg.Value != nil

		skipArgInfo := false // skip the arg info if the arg is inherited from global scope
		if !hasDefault && !hasValue {
			if v, ok := opt.globalArgs.Get(arg.Key); ok {
				arg.Value = &v
				skipArgInfo = true
				hasDefault = false
			}
		}

		if hasValue {
			v := opt.buildArgValues[arg.Key]
			arg.Value = &v
		} else if hasDefault {
			env := getEnv(d.state)
			v, unmatched, err := opt.shlex.ProcessWord(*arg.Value, env)
			reportUnmatchedVariables(c, d.buildArgs, env, unmatched, opt)
			if err != nil {
				return err
			}
			arg.Value = &v
		}

		ai := argInfo{definition: arg, location: c.Location()}

		if arg.Value != nil {
			if _, ok := nonEnvArgs[arg.Key]; !ok {
				d.state = d.state.AddEnv(arg.Key, *arg.Value)
			}
			ai.value = *arg.Value
		}

		if !skipArgInfo {
			d.outline.allArgs[arg.Key] = ai
		}
		d.outline.usedArgs[arg.Key] = struct{}{}

		d.buildArgs = append(d.buildArgs, arg)

		commitStr := arg.Key
		if arg.Value != nil {
			commitStr += "=" + *arg.Value
		}
		commitStrs = append(commitStrs, commitStr)
	}
	return commitToHistory(&d.image, "ARG "+strings.Join(commitStrs, " "), false, nil, d.epoch)
}

func pathRelativeToWorkingDir(s llb.State, p string, platform ocispecs.Platform) (string, error) {
	dir, err := s.GetDir(context.TODO(), llb.Platform(platform))
	if err != nil {
		return "", err
	}

	p, err = system.CheckSystemDriveAndRemoveDriveLetter(p, platform.OS, true)
	if err != nil {
		return "", errors.Wrap(err, "removing drive letter")
	}

	if system.IsAbs(p, platform.OS) {
		return system.NormalizePath("/", p, platform.OS, true)
	}

	// add slashes for "" and "." paths
	// "" is treated as current directory and not necessariy root
	if p == "." || p == "" {
		p = "./"
	}
	return system.NormalizePath(dir, p, platform.OS, true)
}

func addEnv(env []string, k, v string) []string {
	gotOne := false
	for i, envVar := range env {
		key, _ := parseKeyValue(envVar)
		if shell.EqualEnvKeys(key, k) {
			env[i] = k + "=" + v
			gotOne = true
			break
		}
	}
	if !gotOne {
		env = append(env, k+"="+v)
	}
	return env
}

func delEnv(env []string, k string) []string {
	return slices.DeleteFunc(env, func(envVar string) bool {
		key, _ := parseKeyValue(envVar)
		return shell.EqualEnvKeys(key, k)
	})
}

func parseKeyValue(env string) (string, string) {
	parts := strings.SplitN(env, "=", 2)
	v := ""
	if len(parts) > 1 {
		v = parts[1]
	}

	return parts[0], v
}

func dfCmd(cmd any) llb.ConstraintsOpt {
	// TODO: add fmt.Stringer to instructions.Command to remove interface{}
	var cmdStr string
	if cmd, ok := cmd.(fmt.Stringer); ok {
		cmdStr = cmd.String()
	}
	if cmd, ok := cmd.(string); ok {
		cmdStr = cmd
	}
	return llb.WithDescription(map[string]string{
		"com.docker.dockerfile.v1.command": cmdStr,
	})
}

func runCommandString(args []string, buildArgs []instructions.KeyValuePairOptional, env shell.EnvGetter) string {
	var tmpBuildEnv []string
	tmpIdx := map[string]int{}
	for _, arg := range buildArgs {
		v, ok := env.Get(arg.Key)
		if !ok {
			v = arg.ValueString()
		}
		if idx, ok := tmpIdx[arg.Key]; ok {
			tmpBuildEnv[idx] = arg.Key + "=" + v
		} else {
			tmpIdx[arg.Key] = len(tmpBuildEnv)
			tmpBuildEnv = append(tmpBuildEnv, arg.Key+"="+v)
		}
	}
	if len(tmpBuildEnv) > 0 {
		tmpBuildEnv = append([]string{fmt.Sprintf("|%d", len(tmpBuildEnv))}, tmpBuildEnv...)
	}

	return strings.Join(append(tmpBuildEnv, args...), " ")
}

func commitToHistory(img *dockerspec.DockerOCIImage, msg string, withLayer bool, st *llb.State, tm *time.Time) error {
	if st != nil {
		msg += " # buildkit"
	}

	img.History = append(img.History, ocispecs.History{
		CreatedBy:  msg,
		Comment:    historyComment,
		EmptyLayer: !withLayer,
		Created:    tm,
	})
	return nil
}

func allReachableStages(s *dispatchState) map[*dispatchState]struct{} {
	stages := make(map[*dispatchState]struct{})
	addReachableStages(s, stages)
	return stages
}

func addReachableStages(s *dispatchState, stages map[*dispatchState]struct{}) {
	if _, ok := stages[s]; ok {
		return
	}
	stages[s] = struct{}{}
	if s.base != nil {
		addReachableStages(s.base, stages)
	}
	for d := range s.deps {
		addReachableStages(d, stages)
	}
}

func normalizeContextPaths(paths map[string]struct{}) []string {
	// Avoid a useless allocation if the set of paths is empty.
	if len(paths) == 0 {
		return nil
	}

	pathSlice := make([]string, 0, len(paths))
	for p := range paths {
		if p == "/" {
			return nil
		}
		pathSlice = append(pathSlice, path.Join(".", p))
	}

	slices.Sort(pathSlice)
	return pathSlice
}

// filterPaths returns the local options required to filter an llb.Local
// to only the required paths.
func filterPaths(paths map[string]struct{}) []llb.LocalOption {
	if includePaths := normalizeContextPaths(paths); len(includePaths) > 0 {
		return []llb.LocalOption{llb.FollowPaths(includePaths)}
	}
	return nil
}

func proxyEnvFromBuildArgs(args map[string]string) *llb.ProxyEnv {
	pe := &llb.ProxyEnv{}
	isNil := true
	keys := slices.Collect(maps.Keys(args))
	slices.Sort(keys)
	for _, k := range keys {
		v := args[k]
		if strings.EqualFold(k, "http_proxy") {
			pe.HTTPProxy = v
			isNil = false
		}
		if strings.EqualFold(k, "https_proxy") {
			pe.HTTPSProxy = v
			isNil = false
		}
		if strings.EqualFold(k, "ftp_proxy") {
			pe.FTPProxy = v
			isNil = false
		}
		if strings.EqualFold(k, "no_proxy") {
			pe.NoProxy = v
			isNil = false
		}
		if strings.EqualFold(k, "all_proxy") {
			pe.AllProxy = v
			isNil = false
		}
	}
	if isNil {
		return nil
	}
	return pe
}

type mutableOutput struct {
	llb.Output
}

func withShell(img dockerspec.DockerOCIImage, args []string) []string {
	var shell []string
	if len(img.Config.Shell) > 0 {
		shell = slices.Clone(img.Config.Shell)
	} else {
		shell = defaultShell(img.OS)
	}
	return append(shell, strings.Join(args, " "))
}

func autoDetectPlatform(img dockerspec.DockerOCIImage, target ocispecs.Platform, supported []ocispecs.Platform) ocispecs.Platform {
	os := img.OS
	arch := img.Architecture
	if target.OS == os && target.Architecture == arch {
		return target
	}
	for _, p := range supported {
		if p.OS == os && p.Architecture == arch {
			return p
		}
	}
	return target
}

func uppercaseCmd(str string) string {
	p := strings.SplitN(str, " ", 2)
	p[0] = strings.ToUpper(p[0])
	return strings.Join(p, " ")
}

func processCmdEnv(shlex *shell.Lex, cmd string, env shell.EnvGetter) string {
	w, _, err := shlex.ProcessWord(cmd, env)
	if err != nil {
		return cmd
	}
	return w
}

func prefixCommand(ds *dispatchState, str string, prefixPlatform bool, platform *ocispecs.Platform, env shell.EnvGetter) string {
	if ds.cmdTotal == 0 {
		return str
	}
	out := "["
	if prefixPlatform && platform != nil {
		out += platforms.FormatAll(*platform) + formatTargetPlatform(*platform, platformFromEnv(env)) + " "
	}
	if ds.stageName != "" {
		out += ds.stageName + " "
	}
	ds.cmdIndex++
	out += fmt.Sprintf("%*d/%d] ", int(1+math.Log10(float64(ds.cmdTotal))), ds.cmdIndex, ds.cmdTotal)
	if ds.cmdIsOnBuild {
		out += "ONBUILD "
	}
	return out + str
}

// formatTargetPlatform formats a secondary platform string for cross compilation cases
func formatTargetPlatform(base ocispecs.Platform, target *ocispecs.Platform) string {
	if target == nil {
		return ""
	}
	if target.OS == "" {
		target.OS = base.OS
	}
	if target.Architecture == "" {
		target.Architecture = base.Architecture
	}
	p := platforms.Normalize(*target)

	if p.OS == base.OS && p.Architecture != base.Architecture {
		archVariant := p.Architecture
		if p.Variant != "" {
			archVariant += "/" + p.Variant
		}
		return "->" + archVariant
	}
	if p.OS != base.OS {
		return "->" + platforms.FormatAll(p)
	}
	return ""
}

// platformFromEnv returns defined platforms based on TARGET* environment variables
func platformFromEnv(env shell.EnvGetter) *ocispecs.Platform {
	var p ocispecs.Platform
	var set bool
	for _, key := range env.Keys() {
		switch key {
		case "TARGETPLATFORM":
			v, _ := env.Get(key)
			p, err := platforms.Parse(v)
			if err != nil {
				continue
			}
			return &p
		case "TARGETOS":
			p.OS, _ = env.Get(key)
			set = true
		case "TARGETARCH":
			p.Architecture, _ = env.Get(key)
			set = true
		case "TARGETVARIANT":
			p.Variant, _ = env.Get(key)
			set = true
		}
	}
	if !set {
		return nil
	}
	return &p
}

func location(sm *llb.SourceMap, locations []parser.Range) llb.ConstraintsOpt {
	loc := make([]*pb.Range, 0, len(locations))
	for _, l := range locations {
		loc = append(loc, &pb.Range{
			Start: &pb.Position{
				Line:      int32(l.Start.Line),
				Character: int32(l.Start.Character),
			},
			End: &pb.Position{
				Line:      int32(l.End.Line),
				Character: int32(l.End.Character),
			},
		})
	}
	return sm.Location(loc)
}

func summarizeHeredoc(doc string) string {
	doc = strings.TrimSpace(doc)
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	summary := lines[0]
	if len(lines) > 1 {
		summary += "..."
	}
	return summary
}

func commonImageNames() []string {
	repos := []string{
		"alpine", "busybox", "centos", "debian", "golang", "ubuntu", "fedora",
	}
	out := make([]string, 0, len(repos)*4)
	for _, name := range repos {
		out = append(out, name, "docker.io/library"+name, name+":latest", "docker.io/library"+name+":latest")
	}
	return out
}

func isEnabledForStage(stage string, value string) bool {
	if enabled, err := strconv.ParseBool(value); err == nil {
		return enabled
	}

	vv := strings.Split(value, ",")
	return slices.Contains(vv, stage)
}

func mergeLocations(locations ...[]parser.Range) []parser.Range {
	allRanges := []parser.Range{}
	for _, ranges := range locations {
		allRanges = append(allRanges, ranges...)
	}
	if len(allRanges) == 0 {
		return []parser.Range{}
	}
	if len(allRanges) == 1 {
		return allRanges
	}

	slices.SortFunc(allRanges, func(a, b parser.Range) int {
		return a.Start.Line - b.Start.Line
	})

	location := []parser.Range{}
	currentRange := allRanges[0]
	for _, r := range allRanges[1:] {
		if r.Start.Line <= currentRange.End.Line {
			currentRange.End.Line = max(currentRange.End.Line, r.End.Line)
		} else {
			location = append(location, currentRange)
			currentRange = r
		}
	}
	location = append(location, currentRange)
	return location
}

func toPBLocation(sourceIndex int, location []parser.Range) pb.Location {
	loc := make([]*pb.Range, 0, len(location))
	for _, l := range location {
		loc = append(loc, &pb.Range{
			Start: &pb.Position{
				Line:      int32(l.Start.Line),
				Character: int32(l.Start.Character),
			},
			End: &pb.Position{
				Line:      int32(l.End.Line),
				Character: int32(l.End.Character),
			},
		})
	}
	return pb.Location{
		SourceIndex: int32(sourceIndex),
		Ranges:      loc,
	}
}

func unusedFromArgsCheckKeys(env shell.EnvGetter, args map[string]argInfo) map[string]struct{} {
	matched := make(map[string]struct{})
	for _, arg := range args {
		matched[arg.definition.Key] = struct{}{}
	}
	for _, k := range env.Keys() {
		matched[k] = struct{}{}
	}
	return matched
}

func wrapSuggestAny(err error, keys map[string]struct{}, options []string) error {
	for k := range keys {
		var ok bool
		ok, err = suggest.WrapErrorMaybe(err, k, options, true)
		if ok {
			break
		}
	}
	return err
}

func buildMetaArgs(args *llb.EnvList, shlex *shell.Lex, argCommands []instructions.ArgCommand, buildArgs map[string]string) (*llb.EnvList, map[string]argInfo, error) {
	allArgs := make(map[string]argInfo)

	for _, cmd := range argCommands {
		for _, kp := range cmd.Args {
			info := argInfo{definition: kp, location: cmd.Location()}
			if v, ok := buildArgs[kp.Key]; !ok {
				if kp.Value != nil {
					result, err := shlex.ProcessWordWithMatches(*kp.Value, args)
					if err != nil {
						return nil, nil, parser.WithLocation(err, cmd.Location())
					}

					kp.Value = &result.Result
					info.deps = result.Matched
					if _, ok := result.Matched[kp.Key]; ok {
						delete(info.deps, kp.Key)
						if old, ok := allArgs[kp.Key]; ok {
							for k := range old.deps {
								if info.deps == nil {
									info.deps = make(map[string]struct{})
								}
								info.deps[k] = struct{}{}
							}
						}
					}
				}
			} else {
				kp.Value = &v
			}
			if kp.Value != nil {
				args = args.AddOrReplace(kp.Key, *kp.Value)
				info.value = *kp.Value
			}
			allArgs[kp.Key] = info
		}
	}
	return args, allArgs, nil
}

func rangeStartEnd(r []parser.Range) (int, int) {
	if len(r) == 0 {
		return 0, 0
	}
	start := math.MaxInt32
	end := 0
	for _, rng := range r {
		if rng.Start.Line < start {
			start = rng.Start.Line
		}
		if rng.End.Line > end {
			end = rng.End.Line
		}
	}
	return start, end
}

type emptyEnvs struct{}

func (emptyEnvs) Get(string) (string, bool) {
	return "", false
}

func (emptyEnvs) Keys() []string {
	return nil
}

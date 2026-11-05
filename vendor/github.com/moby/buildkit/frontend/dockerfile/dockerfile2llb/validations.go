package dockerfile2llb

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/moby/buildkit/util/suggest"
	"github.com/moby/patternmatcher"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	secretsRegexpOnce  sync.Once
	secretsRegexp      *regexp.Regexp
	secretsAllowRegexp *regexp.Regexp
)

var reservedStageNames = map[string]struct{}{
	"context": {},
	"scratch": {},
}

func validateCopySourcePath(src string, cfg *copyConfig) error {
	// Do not validate copy source paths if there is no dockerignore file
	// or if the dockerignore file contains exclusions.
	//
	// Exclusions are too difficult to statically determine if they're proper
	// because it's ok for a directory to be excluded and a file inside the directory
	// to be negated.
	if cfg.ignoreMatcher == nil || cfg.ignoreMatcher.Exclusions() {
		return nil
	}
	cmd := "Copy"
	if cfg.isAddCommand {
		cmd = "Add"
	}

	src = filepath.ToSlash(filepath.Clean(src))
	if src == "." || src == "/" {
		// "." and "/" are context roots, not real paths that can be excluded.
		// Only keep the warning for patterns that exclude all root entries.
		if !copySourceRootIgnored(cfg.ignoreMatcher) {
			return nil
		}
	}
	ok, err := cfg.ignoreMatcher.MatchesOrParentMatches(src)
	if err != nil {
		return err
	}
	if ok {
		msg := linter.RuleCopyIgnoredFile.Format(cmd, src)
		cfg.opt.lint.Run(&linter.RuleCopyIgnoredFile, cfg.location, msg)
	}

	return nil
}

func copySourceRootIgnored(matcher *patternmatcher.PatternMatcher) bool {
	for _, pattern := range matcher.Patterns() {
		if pattern.Exclusion() {
			continue
		}
		switch pattern.String() {
		case "*", "**", "**/*":
			return true
		}
	}
	return false
}

func validateCircularDependency(states []*dispatchState) error {
	var visit func(*dispatchState, []instructions.Command) []instructions.Command
	if states == nil {
		return nil
	}
	visited := make(map[*dispatchState]struct{})
	path := make(map[*dispatchState]struct{})

	visit = func(state *dispatchState, current []instructions.Command) []instructions.Command {
		_, ok := visited[state]
		if ok {
			return nil
		}
		visited[state] = struct{}{}
		path[state] = struct{}{}
		for dep, c := range state.deps {
			next := append(current, c)
			if _, ok := path[dep]; ok {
				return next
			}
			if c := visit(dep, next); c != nil {
				return c
			}
		}
		delete(path, state)
		return nil
	}
	for _, state := range states {
		if cmds := visit(state, nil); cmds != nil {
			err := errors.Errorf("circular dependency detected on stage: %s", state.stageName)
			for _, c := range cmds {
				err = parser.WithLocation(err, c.Location())
			}
			return err
		}
	}
	return nil
}

func isSelfConsistentCasing(s string) bool {
	return s == strings.ToLower(s) || s == strings.ToUpper(s)
}

func validateCaseMatch(name string, isMajorityLower bool, location []parser.Range, lint *linter.Linter) {
	var correctCasing string
	if isMajorityLower && strings.ToLower(name) != name {
		correctCasing = "lowercase"
	} else if !isMajorityLower && strings.ToUpper(name) != name {
		correctCasing = "uppercase"
	}
	if correctCasing != "" {
		msg := linter.RuleConsistentInstructionCasing.Format(name, correctCasing)
		lint.Run(&linter.RuleConsistentInstructionCasing, location, msg)
	}
}

func validateCommandCasing(stages []instructions.Stage, lint *linter.Linter) {
	var lowerCount, upperCount int
	for _, stage := range stages {
		if isSelfConsistentCasing(stage.OrigCmd) {
			if strings.ToLower(stage.OrigCmd) == stage.OrigCmd {
				lowerCount++
			} else {
				upperCount++
			}
		}
		for _, cmd := range stage.Commands {
			cmdName := cmd.Name()
			if isSelfConsistentCasing(cmdName) {
				if strings.ToLower(cmdName) == cmdName {
					lowerCount++
				} else {
					upperCount++
				}
			}
		}
	}

	isMajorityLower := lowerCount > upperCount
	for _, stage := range stages {
		// Here, we check both if the command is consistent per command (ie, "CMD" or "cmd", not "Cmd")
		// as well as ensuring that the casing is consistent throughout the dockerfile by comparing the
		// command to the casing of the majority of commands.
		validateCaseMatch(stage.OrigCmd, isMajorityLower, stage.Location, lint)
		for _, cmd := range stage.Commands {
			validateCaseMatch(cmd.Name(), isMajorityLower, cmd.Location(), lint)
		}
	}
}

func validateStageNames(stages []instructions.Stage, lint *linter.Linter) {
	stageNames := make(map[string]struct{})
	for _, stage := range stages {
		if stage.Name != "" {
			if _, ok := reservedStageNames[stage.Name]; ok {
				msg := linter.RuleReservedStageName.Format(stage.Name)
				lint.Run(&linter.RuleReservedStageName, stage.Location, msg)
			}

			if _, ok := stageNames[stage.Name]; ok {
				msg := linter.RuleDuplicateStageName.Format(stage.Name)
				lint.Run(&linter.RuleDuplicateStageName, stage.Location, msg)
			}
			stageNames[stage.Name] = struct{}{}
		}
	}
}

func reportUnmatchedVariables(cmd instructions.Command, buildArgs []instructions.KeyValuePairOptional, env shell.EnvGetter, unmatched map[string]struct{}, opt *dispatchOpt) {
	if len(unmatched) == 0 {
		return
	}
	for _, buildArg := range buildArgs {
		delete(unmatched, buildArg.Key)
	}
	if len(unmatched) == 0 {
		return
	}
	options := env.Keys()
	for cmdVar := range unmatched {
		if _, nonEnvOk := nonEnvArgs[cmdVar]; nonEnvOk {
			continue
		}
		match, _ := suggest.Search(cmdVar, options, runtime.GOOS != "windows")
		msg := linter.RuleUndefinedVar.Format(cmdVar, match)
		opt.lint.Run(&linter.RuleUndefinedVar, cmd.Location(), msg)
	}
}

func reportUnusedFromArgs(testArgKeys map[string]struct{}, unmatched map[string]struct{}, location []parser.Range, lint *linter.Linter) {
	var argKeys []string
	for arg := range testArgKeys {
		argKeys = append(argKeys, arg)
	}
	for arg := range unmatched {
		if _, ok := testArgKeys[arg]; ok {
			continue
		}
		suggest, _ := suggest.Search(arg, argKeys, true)
		msg := linter.RuleUndefinedArgInFrom.Format(arg, suggest)
		lint.Run(&linter.RuleUndefinedArgInFrom, location, msg)
	}
}

func reportRedundantTargetPlatform(platformVar string, nameMatch shell.ProcessWordResult, location []parser.Range, env shell.EnvGetter, lint *linter.Linter) {
	// Only match this rule if there was only one matched name.
	// It's psosible there were multiple args and that one of them expanded to an empty
	// string and we don't want to report a warning when that happens.
	if len(nameMatch.Matched) == 1 && len(nameMatch.Unmatched) == 0 {
		const targetPlatform = "TARGETPLATFORM"
		// If target platform is the only environment variable that was substituted and the result
		// matches the target platform exactly, we can infer that the input was ${TARGETPLATFORM} or
		// $TARGETPLATFORM.
		if _, ok := nameMatch.Matched[targetPlatform]; !ok {
			return
		}

		if result, _ := env.Get(targetPlatform); nameMatch.Result == result {
			msg := linter.RuleRedundantTargetPlatform.Format(platformVar)
			lint.Run(&linter.RuleRedundantTargetPlatform, location, msg)
		}
	}
}

func reportConstPlatformDisallowed(stageName string, nameMatch shell.ProcessWordResult, location []parser.Range, lint *linter.Linter) {
	if len(nameMatch.Matched) > 0 || len(nameMatch.Unmatched) > 0 {
		// Some substitution happened so the platform was not a constant.
		// Disable checking for this warning.
		return
	}

	// Attempt to parse the platform result. If this fails, then it will fail
	// later so just ignore.
	p, err := platforms.Parse(nameMatch.Result)
	if err != nil {
		return
	}

	// Check if the platform os or architecture is used in the stage name
	// at all. If it is, then disable this warning.
	if strings.Contains(stageName, p.OS) || strings.Contains(stageName, p.Architecture) {
		return
	}

	// Report the linter warning.
	msg := linter.RuleFromPlatformFlagConstDisallowed.Format(nameMatch.Result)
	lint.Run(&linter.RuleFromPlatformFlagConstDisallowed, location, msg)
}

type instructionTracker struct {
	Loc   []parser.Range
	IsSet bool
}

func (v *instructionTracker) MarkUsed(loc []parser.Range) {
	v.Loc = loc
	v.IsSet = true
}

func validateUsedOnce(c instructions.Command, loc *instructionTracker, lint *linter.Linter) {
	if loc.IsSet {
		msg := linter.RuleMultipleInstructionsDisallowed.Format(c.Name())
		// Report the location of the previous invocation because it is the one
		// that will be ignored.
		lint.Run(&linter.RuleMultipleInstructionsDisallowed, loc.Loc, msg)
	}
	loc.MarkUsed(c.Location())
}

func validateBaseImagePlatform(name string, expected, actual ocispecs.Platform, location []parser.Range, lint *linter.Linter) {
	if expected.OS != actual.OS || expected.Architecture != actual.Architecture {
		expectedStr := platforms.FormatAll(platforms.Normalize(expected))
		actualStr := platforms.FormatAll(platforms.Normalize(actual))
		msg := linter.RuleInvalidBaseImagePlatform.Format(name, expectedStr, actualStr)
		lint.Run(&linter.RuleInvalidBaseImagePlatform, location, msg)
	}
}

func getSecretsRegex() (*regexp.Regexp, *regexp.Regexp) {
	// Check for either full value or first/last word.
	// Examples: api_key, DATABASE_PASSWORD, GITHUB_TOKEN, secret_MESSAGE, AUTH
	// Case insensitive.
	secretsRegexpOnce.Do(func() {
		secretTokens := []string{
			"apikey",
			"auth",
			"credential",
			"credentials",
			"key",
			"password",
			"pword",
			"passwd",
			"psw",
			"secret",
			"token",
		}
		pattern := `(?i)(?:_|^)(?:` + strings.Join(secretTokens, "|") + `)(?:_|$)`
		secretsRegexp = regexp.MustCompile(pattern)

		allowTokens := []string{
			"public",
			"file",
			"version",
		}
		allowPattern := `(?i)(?:_|^)(?:` + strings.Join(allowTokens, "|") + `)(?:_|$)`
		secretsAllowRegexp = regexp.MustCompile(allowPattern)
	})
	return secretsRegexp, secretsAllowRegexp
}

func validateNoSecretKey(instruction, key string, location []parser.Range, lint *linter.Linter) {
	deny, allow := getSecretsRegex()
	if deny.MatchString(key) && !allow.MatchString(key) {
		msg := linter.RuleSecretsUsedInArgOrEnv.Format(instruction, key)
		lint.Run(&linter.RuleSecretsUsedInArgOrEnv, location, msg)
	}
}

func validateBaseImagesWithDefaultArgs(stages []instructions.Stage, shlex *shell.Lex, env *llb.EnvList, argCmds []instructions.ArgCommand, lint *linter.Linter) {
	// Build the arguments as if no build options were given
	// and using only defaults.
	args, _, err := buildMetaArgs(env, shlex, argCmds, nil)
	if err != nil {
		// Abandon running the linter. We'll likely fail after this point
		// with the same error but we shouldn't error here inside
		// of the linting check.
		return
	}

	for _, st := range stages {
		nameMatch, err := shlex.ProcessWordWithMatches(st.BaseName, args)
		if err != nil {
			return
		}

		// Verify the image spec is potentially valid.
		if _, err := reference.ParseNormalizedNamed(nameMatch.Result); err != nil {
			msg := linter.RuleInvalidDefaultArgInFrom.Format(st.BaseName)
			lint.Run(&linter.RuleInvalidDefaultArgInFrom, st.Location, msg)
		}
	}
}

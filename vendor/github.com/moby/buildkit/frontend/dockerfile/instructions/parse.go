// The instructions package contains the definitions of the high-level
// Dockerfile commands, as well as low-level primitives for extracting these
// commands from a pre-parsed Abstract Syntax Tree.

package instructions

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/util/suggest"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type parseRequest struct {
	command    string
	args       []string
	heredocs   []parser.Heredoc
	attributes map[string]bool
	flags      *BFlags
	original   string
	location   []parser.Range
	comments   []string
}

var (
	parseRunPreHooks  []func(*RunCommand, parseRequest) error
	parseRunPostHooks []func(*RunCommand, parseRequest) error
)

var (
	unsetEnabled = false
	resetEnabled = false
)

// ResetAll is the FROM --reset value that resets all ResetKeys.
const ResetAll = "all"

// ResetKeys are the base image config fields that can be reset with
// FROM --reset. Each field is named after the command that sets it.
var ResetKeys = []string{
	command.Cmd,
	command.Entrypoint,
	command.Env,
	command.Expose,
	command.Healthcheck,
	command.Label,
	command.Onbuild,
	command.Shell,
	command.StopSignal,
	command.User,
	command.Volume,
	command.Workdir,
}

func nodeArgs(node *parser.Node) []string {
	result := []string{}
	for ; node.Next != nil; node = node.Next {
		arg := node.Next
		if len(arg.Children) == 0 {
			result = append(result, arg.Value)
		} else if len(arg.Children) == 1 {
			// sub command
			result = append(result, arg.Children[0].Value)
			result = append(result, nodeArgs(arg.Children[0])...)
		}
	}
	return result
}

func newParseRequestFromNode(node *parser.Node) parseRequest {
	return parseRequest{
		command:    node.Value,
		args:       nodeArgs(node),
		heredocs:   node.Heredocs,
		attributes: node.Attributes,
		original:   node.Original,
		flags:      NewBFlagsWithArgs(node.Flags),
		location:   node.Location(),
		comments:   node.PrevComment,
	}
}

func ParseInstruction(node *parser.Node) (v any, err error) {
	return ParseInstructionWithLinter(node, nil)
}

// ParseInstruction converts an AST to a typed instruction (either a command or a build stage beginning when encountering a `FROM` statement)
func ParseInstructionWithLinter(node *parser.Node, lint *linter.Linter) (v any, err error) {
	lint = lint.WithMergedConfigFromComments(node.PrevComment)

	defer func() {
		if err != nil {
			err = parser.WithLocation(err, node.Location())
		}
	}()
	req := newParseRequestFromNode(node)
	switch strings.ToLower(node.Value) {
	case command.Env:
		return parseEnv(req)
	case command.Maintainer:
		msg := linter.RuleMaintainerDeprecated.Format()
		lint.Run(&linter.RuleMaintainerDeprecated, node.Location(), msg)
		return parseMaintainer(req)
	case command.Label:
		return parseLabel(req)
	case command.Add:
		return parseAdd(req)
	case command.Copy:
		return parseCopy(req)
	case command.From:
		if !isLowerCaseStageName(req.args) {
			msg := linter.RuleStageNameCasing.Format(req.args[2])
			lint.Run(&linter.RuleStageNameCasing, node.Location(), msg)
		}
		if !doesFromCaseMatchAsCase(req) {
			msg := linter.RuleFromAsCasing.Format(req.command, req.args[1])
			lint.Run(&linter.RuleFromAsCasing, node.Location(), msg)
		}
		fromCmd, err := parseFrom(req)
		if err != nil {
			return nil, err
		}
		if fromCmd.Name != "" {
			validateDefinitionDescription("FROM", []string{fromCmd.Name}, node.PrevComment, node.Location(), lint)
		}
		return fromCmd, nil
	case command.Onbuild:
		return parseOnBuild(req)
	case command.Workdir:
		return parseWorkdir(req)
	case command.Run:
		return parseRun(req)
	case command.Cmd:
		return parseCmd(req)
	case command.Healthcheck:
		return parseHealthcheck(req)
	case command.Entrypoint:
		return parseEntrypoint(req)
	case command.Expose:
		return parseExpose(req)
	case command.User:
		return parseUser(req)
	case command.Volume:
		return parseVolume(req)
	case command.StopSignal:
		return parseStopSignal(req)
	case command.Arg:
		argCmd, err := parseArg(req)
		if err != nil {
			return nil, err
		}
		argKeys := []string{}
		for _, arg := range argCmd.Args {
			argKeys = append(argKeys, arg.Key)
		}
		validateDefinitionDescription("ARG", argKeys, node.PrevComment, node.Location(), lint)
		return argCmd, nil
	case command.Shell:
		return parseShell(req)
	}
	return nil, suggest.WrapError(&UnknownInstructionError{Instruction: node.Value, Line: node.StartLine}, node.Value, allInstructionNames(), false)
}

// ParseCommand converts an AST to a typed Command
func ParseCommand(node *parser.Node) (Command, error) {
	s, err := ParseInstruction(node)
	if err != nil {
		return nil, err
	}
	if c, ok := s.(Command); ok {
		return c, nil
	}
	return nil, parser.WithLocation(errors.Errorf("%T is not a command type", s), node.Location())
}

// UnknownInstructionError represents an error occurring when a command is unresolvable
type UnknownInstructionError struct {
	Line        int
	Instruction string
}

func (e *UnknownInstructionError) Error() string {
	return fmt.Sprintf("unknown instruction: %s", e.Instruction)
}

type parseError struct {
	inner error
	node  *parser.Node
}

func (e *parseError) Error() string {
	return fmt.Sprintf("dockerfile parse error on line %d: %v", e.node.StartLine, e.inner.Error())
}

func (e *parseError) Unwrap() error {
	return e.inner
}

// Parse a Dockerfile into a collection of buildable stages.
// metaArgs is a collection of ARG instructions that occur before the first FROM.
func Parse(ast *parser.Node, lint *linter.Linter) (stages []Stage, metaArgs []ArgCommand, err error) {
	for _, n := range ast.Children {
		cmd, err := ParseInstructionWithLinter(n, lint)
		if err != nil {
			return nil, nil, &parseError{inner: err, node: n}
		}
		if len(stages) == 0 {
			// meta arg case
			if a, isArg := cmd.(*ArgCommand); isArg {
				metaArgs = append(metaArgs, *a)
				continue
			}
		}
		switch c := cmd.(type) {
		case *Stage:
			stages = append(stages, *c)
		case Command:
			stage, err := CurrentStage(stages)
			if err != nil {
				return nil, nil, parser.WithLocation(err, n.Location())
			}
			stage.AddCommand(c)
		default:
			return nil, nil, parser.WithLocation(errors.Errorf("%T is not a command type", cmd), n.Location())
		}
	}
	return stages, metaArgs, nil
}

func parseKvps(args []string, cmdName string) (KeyValuePairs, error) {
	if len(args) == 0 {
		return nil, errAtLeastOneArgument(cmdName)
	}
	if len(args)%3 != 0 {
		// should never get here, but just in case
		return nil, errTooManyArguments(cmdName)
	}
	var res KeyValuePairs
	for j := 0; j < len(args); j += 3 {
		if len(args[j]) == 0 {
			return nil, errBlankCommandNames(cmdName)
		}
		name, value, delim := args[j], args[j+1], args[j+2]
		res = append(res, KeyValuePair{Key: name, Value: value, NoDelim: delim == ""})
	}
	return res, nil
}

// addUnsetFlag adds the --unset flag if it is enabled in this build.
func addUnsetFlag(req parseRequest) *Flag {
	if !unsetEnabled {
		return nil
	}
	return req.flags.AddBool("unset", false)
}

// parseUnsetKeys returns the keys for the --unset form of a command.
func parseUnsetKeys(args []string, cmdName string) (KeyValuePairs, error) {
	if len(args) == 0 {
		return nil, errAtLeastOneArgument(cmdName + " --unset")
	}
	var res KeyValuePairs
	for _, name := range args {
		if name == "" {
			return nil, errBlankCommandNames(cmdName)
		}
		if strings.Contains(name, "=") {
			return nil, errors.Errorf("%s --unset does not accept values: %s", cmdName, name)
		}
		res = append(res, KeyValuePair{Key: name})
	}
	return res, nil
}

func parseEnv(req parseRequest) (*EnvCommand, error) {
	flUnset := addUnsetFlag(req)
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	if flUnset != nil && flUnset.Value == "true" {
		envs, err := parseUnsetKeys(req.args, "ENV")
		if err != nil {
			return nil, err
		}
		return &EnvCommand{
			Env:             envs,
			Unset:           true,
			withNameAndCode: newWithNameAndCode(req),
		}, nil
	}
	envs, err := parseKvps(req.args, "ENV")
	if err != nil {
		return nil, err
	}
	return &EnvCommand{
		Env:             envs,
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseMaintainer(req parseRequest) (*MaintainerCommand, error) {
	if len(req.args) != 1 {
		return nil, errExactlyOneArgument("MAINTAINER")
	}

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	return &MaintainerCommand{
		Maintainer:      req.args[0],
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseLabel(req parseRequest) (*LabelCommand, error) {
	flUnset := addUnsetFlag(req)
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	if flUnset != nil && flUnset.Value == "true" {
		labels, err := parseUnsetKeys(req.args, "LABEL")
		if err != nil {
			return nil, err
		}
		return &LabelCommand{
			Labels:          labels,
			Unset:           true,
			withNameAndCode: newWithNameAndCode(req),
		}, nil
	}

	labels, err := parseKvps(req.args, "LABEL")
	if err != nil {
		return nil, err
	}

	return &LabelCommand{
		Labels:          labels,
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseSourcesAndDest(req parseRequest, command string) (*SourcesAndDest, error) {
	srcs := req.args[:len(req.args)-1]
	dest := req.args[len(req.args)-1]
	if heredoc := parser.MustParseHeredoc(dest); heredoc != nil {
		return nil, errBadHeredoc(command, "a destination")
	}

	heredocLookup := make(map[string]parser.Heredoc)
	for _, heredoc := range req.heredocs {
		heredocLookup[heredoc.Name] = heredoc
	}

	var sourcePaths []string
	var sourceContents []SourceContent
	for _, src := range srcs {
		if heredoc := parser.MustParseHeredoc(src); heredoc != nil {
			content := heredocLookup[heredoc.Name].Content
			if heredoc.Chomp {
				content = parser.ChompHeredocContent(content)
			}
			sourceContents = append(sourceContents,
				SourceContent{
					Data:   content,
					Path:   heredoc.Name,
					Expand: heredoc.Expand,
				},
			)
		} else {
			sourcePaths = append(sourcePaths, src)
		}
	}

	return &SourcesAndDest{
		DestPath:       dest,
		SourcePaths:    sourcePaths,
		SourceContents: sourceContents,
	}, nil
}

func parseAdd(req parseRequest) (*AddCommand, error) {
	if len(req.args) < 2 {
		return nil, errNoDestinationArgument("ADD")
	}

	flChown := req.flags.AddString("chown", "")
	flChmod := req.flags.AddString("chmod", "")
	flLink := req.flags.AddBool("link", false)
	flKeepGitDir := req.flags.AddBool("keep-git-dir", false)
	flChecksum := req.flags.AddString("checksum", "")
	flUnpack := req.flags.AddBool("unpack", false)
	flExcludes := req.flags.AddStrings("exclude")
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	sourcesAndDest, err := parseSourcesAndDest(req, "ADD")
	if err != nil {
		return nil, err
	}

	var unpack *bool
	if _, ok := req.flags.used["unpack"]; ok {
		b := flUnpack.Value == "true"
		unpack = &b
	}

	var keepGit *bool
	if _, ok := req.flags.used["keep-git-dir"]; ok {
		b := flKeepGitDir.Value == "true"
		keepGit = &b
	}

	return &AddCommand{
		withNameAndCode: newWithNameAndCode(req),
		SourcesAndDest:  *sourcesAndDest,
		Chown:           flChown.Value,
		Chmod:           flChmod.Value,
		Link:            flLink.Value == "true",
		KeepGitDir:      keepGit,
		Checksum:        flChecksum.Value,
		ExcludePatterns: flExcludes.StringValues,
		Unpack:          unpack,
	}, nil
}

func parseCopy(req parseRequest) (*CopyCommand, error) {
	if len(req.args) < 2 {
		return nil, errNoDestinationArgument("COPY")
	}

	flChown := req.flags.AddString("chown", "")
	flFrom := req.flags.AddString("from", "")
	flChmod := req.flags.AddString("chmod", "")
	flLink := req.flags.AddBool("link", false)
	flExcludes := req.flags.AddStrings("exclude")
	flParents := req.flags.AddBool("parents", false)

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	sourcesAndDest, err := parseSourcesAndDest(req, "COPY")
	if err != nil {
		return nil, err
	}

	return &CopyCommand{
		withNameAndCode: newWithNameAndCode(req),
		SourcesAndDest:  *sourcesAndDest,
		From:            flFrom.Value,
		Chown:           flChown.Value,
		Chmod:           flChmod.Value,
		Link:            flLink.Value == "true",
		Parents:         flParents.Value == "true",
		ExcludePatterns: flExcludes.StringValues,
	}, nil
}

func parseFrom(req parseRequest) (*Stage, error) {
	stageName, err := parseBuildStageName(req.args)
	if err != nil {
		return nil, err
	}

	flPlatform := req.flags.AddString("platform", "")
	var flReset *Flag
	if resetEnabled {
		flReset = req.flags.AddStrings("reset")
	}
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	var reset []string
	if flReset != nil {
		reset, err = parseResetKeys(flReset.StringValues)
		if err != nil {
			return nil, err
		}
	}

	code := strings.TrimSpace(req.original)
	return &Stage{
		BaseName:   req.args[0],
		OrigCmd:    req.command,
		Name:       stageName,
		SourceCode: code,
		Commands:   []Command{},
		Platform:   flPlatform.Value,
		Reset:      reset,
		Location:   req.location,
		Comments:   req.comments,
		DocComment: getDocComment(req.comments, stageName),
	}, nil
}

// parseResetKeys returns the sorted list of fields for FROM --reset values.
func parseResetKeys(values []string) ([]string, error) {
	var keys []string
	for _, v := range values {
		for k := range strings.SplitSeq(v, ",") {
			switch {
			case k == ResetAll:
				keys = append(keys, ResetKeys...)
			case slices.Contains(ResetKeys, k):
				keys = append(keys, k)
			case strings.ToLower(k) == ResetAll || slices.Contains(ResetKeys, strings.ToLower(k)):
				return nil, errors.Errorf("invalid FROM --reset value: %q (did you mean %s?)", k, strings.ToLower(k))
			default:
				err := errors.Errorf("invalid FROM --reset value: %q", k)
				return nil, suggest.WrapError(err, k, append([]string{ResetAll}, ResetKeys...), true)
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

var validStageName = regexp.MustCompile("^[a-z][a-z0-9-_.]*$")

func parseBuildStageName(args []string) (stageName string, err error) {
	switch {
	case len(args) == 3 && strings.EqualFold(args[1], "as"):
		stageName = strings.ToLower(args[2])
		if !validStageName.MatchString(stageName) {
			return "", errors.Errorf("invalid name for build stage: %q, name can't start with a number or contain symbols", args[2])
		}
	case len(args) != 1:
		return "", errors.New("FROM requires either one or three arguments")
	}

	return stageName, nil
}

func parseOnBuild(req parseRequest) (*OnbuildCommand, error) {
	if len(req.args) == 0 {
		return nil, errAtLeastOneArgument("ONBUILD")
	}
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	triggerInstruction := strings.ToUpper(strings.TrimSpace(req.args[0]))
	switch strings.ToUpper(triggerInstruction) {
	case "ONBUILD":
		return nil, errors.New("Chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
	case "MAINTAINER", "FROM":
		return nil, errors.Errorf("%s isn't allowed as an ONBUILD trigger", triggerInstruction)
	}

	original := regexp.MustCompile(`(?i)^\s*ONBUILD\s*`).ReplaceAllString(req.original, "")
	if len(req.heredocs) > 0 {
		var b strings.Builder
		b.WriteString(original)
		for _, heredoc := range req.heredocs {
			b.WriteByte('\n')
			b.WriteString(heredoc.Content)
			b.WriteString(heredoc.Name)
		}
		original = b.String()
	}

	return &OnbuildCommand{
		Expression:      original,
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseWorkdir(req parseRequest) (*WorkdirCommand, error) {
	if len(req.args) != 1 {
		return nil, errExactlyOneArgument("WORKDIR")
	}

	err := req.flags.Parse()
	if err != nil {
		return nil, err
	}
	return &WorkdirCommand{
		Path:            req.args[0],
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseShellDependentCommand(req parseRequest, emptyAsNil bool) (ShellDependantCmdLine, error) {
	var files []ShellInlineFile
	for _, heredoc := range req.heredocs {
		file := ShellInlineFile{
			Name:  heredoc.Name,
			Data:  heredoc.Content,
			Chomp: heredoc.Chomp,
		}
		files = append(files, file)
	}

	args := handleJSONArgs(req.args, req.attributes)
	if emptyAsNil && len(args) == 0 {
		args = nil
	}
	return ShellDependantCmdLine{
		CmdLine:      args,
		Files:        files,
		PrependShell: !req.attributes["json"],
	}, nil
}

func parseRun(req parseRequest) (*RunCommand, error) {
	cmd := &RunCommand{}

	for _, fn := range parseRunPreHooks {
		if err := fn(cmd, req); err != nil {
			return nil, err
		}
	}

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	cmd.FlagsUsed = req.flags.Used()

	cmdline, err := parseShellDependentCommand(req, false)
	if err != nil {
		return nil, err
	}
	cmd.ShellDependantCmdLine = cmdline

	cmd.withNameAndCode = newWithNameAndCode(req)

	for _, fn := range parseRunPostHooks {
		if err := fn(cmd, req); err != nil {
			return nil, err
		}
	}

	return cmd, nil
}

func parseCmd(req parseRequest) (*CmdCommand, error) {
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	cmdline, err := parseShellDependentCommand(req, false)
	if err != nil {
		return nil, err
	}

	return &CmdCommand{
		ShellDependantCmdLine: cmdline,
		withNameAndCode:       newWithNameAndCode(req),
	}, nil
}

func parseEntrypoint(req parseRequest) (*EntrypointCommand, error) {
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	cmdline, err := parseShellDependentCommand(req, true)
	if err != nil {
		return nil, err
	}

	return &EntrypointCommand{
		ShellDependantCmdLine: cmdline,
		withNameAndCode:       newWithNameAndCode(req),
	}, nil
}

// parseOptInterval(flag) is the duration of flag.Value, or 0 if
// empty. An error is reported if the value is given and less than minimum duration.
func parseOptInterval(f *Flag) (time.Duration, error) {
	s := f.Value
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d == 0 {
		return 0, nil
	}

	const minimumDuration = time.Millisecond
	if d < minimumDuration {
		return 0, errors.Errorf("Interval %#v cannot be less than %s", f.name, minimumDuration)
	}
	return d, nil
}

func parseHealthcheck(req parseRequest) (*HealthCheckCommand, error) {
	if len(req.args) == 0 {
		return nil, errAtLeastOneArgument("HEALTHCHECK")
	}
	cmd := &HealthCheckCommand{
		withNameAndCode: newWithNameAndCode(req),
	}

	typ := strings.ToUpper(req.args[0])
	args := req.args[1:]
	if typ == "NONE" {
		if len(args) != 0 {
			return nil, errors.New("HEALTHCHECK NONE takes no arguments")
		}
		cmd.Health = &dockerspec.HealthcheckConfig{
			Test: []string{typ},
		}
	} else {
		healthcheck := dockerspec.HealthcheckConfig{}

		flInterval := req.flags.AddString("interval", "")
		flTimeout := req.flags.AddString("timeout", "")
		flStartPeriod := req.flags.AddString("start-period", "")
		flStartInterval := req.flags.AddString("start-interval", "")
		flRetries := req.flags.AddString("retries", "")

		if err := req.flags.Parse(); err != nil {
			return nil, err
		}

		switch typ {
		case "CMD":
			cmdSlice := handleJSONArgs(args, req.attributes)
			if len(cmdSlice) == 0 {
				return nil, errors.New("Missing command after HEALTHCHECK CMD")
			}

			if !req.attributes["json"] {
				typ = "CMD-SHELL"
			}

			healthcheck.Test = append([]string{typ}, cmdSlice...)
		default:
			return nil, errors.Errorf("Unknown type %#v in HEALTHCHECK (try CMD)", typ)
		}

		interval, err := parseOptInterval(flInterval)
		if err != nil {
			return nil, err
		}
		healthcheck.Interval = interval

		timeout, err := parseOptInterval(flTimeout)
		if err != nil {
			return nil, err
		}
		healthcheck.Timeout = timeout

		startPeriod, err := parseOptInterval(flStartPeriod)
		if err != nil {
			return nil, err
		}
		healthcheck.StartPeriod = startPeriod

		startInterval, err := parseOptInterval(flStartInterval)
		if err != nil {
			return nil, err
		}
		healthcheck.StartInterval = startInterval

		if flRetries.Value != "" {
			retries, err := strconv.ParseInt(flRetries.Value, 10, 32)
			if err != nil {
				return nil, err
			}
			if retries < 0 {
				return nil, errors.Errorf("--retries cannot be negative (%d)", retries)
			}
			healthcheck.Retries = int(retries)
		} else {
			healthcheck.Retries = 0
		}

		cmd.Health = &healthcheck
	}
	return cmd, nil
}

func parseExpose(req parseRequest) (*ExposeCommand, error) {
	portsTab := req.args

	if len(req.args) == 0 {
		return nil, errAtLeastOneArgument("EXPOSE")
	}

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	slices.Sort(portsTab)
	return &ExposeCommand{
		Ports:           portsTab,
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseUser(req parseRequest) (*UserCommand, error) {
	if len(req.args) != 1 {
		return nil, errExactlyOneArgument("USER")
	}

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	return &UserCommand{
		User:            req.args[0],
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseVolume(req parseRequest) (*VolumeCommand, error) {
	if len(req.args) == 0 {
		return nil, errAtLeastOneArgument("VOLUME")
	}

	flUnset := addUnsetFlag(req)
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	cmd := &VolumeCommand{
		Unset:           flUnset != nil && flUnset.Value == "true",
		withNameAndCode: newWithNameAndCode(req),
	}

	for _, v := range req.args {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, errors.New("VOLUME specified can not be an empty string")
		}
		cmd.Volumes = append(cmd.Volumes, v)
	}
	return cmd, nil
}

func parseStopSignal(req parseRequest) (*StopSignalCommand, error) {
	if len(req.args) != 1 {
		return nil, errExactlyOneArgument("STOPSIGNAL")
	}
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	sig := req.args[0]

	cmd := &StopSignalCommand{
		Signal:          sig,
		withNameAndCode: newWithNameAndCode(req),
	}
	return cmd, nil
}

func parseArg(req parseRequest) (*ArgCommand, error) {
	if len(req.args) < 1 {
		return nil, errAtLeastOneArgument("ARG")
	}

	if err := req.flags.Parse(); err != nil {
		return nil, err
	}

	pairs := make([]KeyValuePairOptional, len(req.args))

	for i, arg := range req.args {
		kvpo := KeyValuePairOptional{}

		// 'arg' can just be a name or name-value pair. Note that this is different
		// from 'env' that handles the split of name and value at the parser level.
		// The reason for doing it differently for 'arg' is that we support just
		// defining an arg and not assign it a value (while 'env' always expects a
		// name-value pair). If possible, it will be good to harmonize the two.
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			if len(parts[0]) == 0 {
				return nil, errBlankCommandNames("ARG")
			}

			kvpo.Key = parts[0]
			kvpo.Value = &parts[1]
		} else {
			kvpo.Key = arg
		}
		kvpo.DocComment = getDocComment(req.comments, kvpo.Key)
		pairs[i] = kvpo
	}

	return &ArgCommand{
		Args:            pairs,
		withNameAndCode: newWithNameAndCode(req),
	}, nil
}

func parseShell(req parseRequest) (*ShellCommand, error) {
	if err := req.flags.Parse(); err != nil {
		return nil, err
	}
	shellSlice := handleJSONArgs(req.args, req.attributes)
	switch {
	case len(shellSlice) == 0:
		// SHELL []
		return nil, errAtLeastOneArgument("SHELL")
	case req.attributes["json"]:
		// SHELL ["powershell", "-command"]

		return &ShellCommand{
			Shell:           shellSlice,
			withNameAndCode: newWithNameAndCode(req),
		}, nil
	default:
		// SHELL powershell -command - not JSON
		return nil, errNotJSON("SHELL", req.original)
	}
}

func errAtLeastOneArgument(command string) error {
	return errors.Errorf("%s requires at least one argument", command)
}

func errExactlyOneArgument(command string) error {
	return errors.Errorf("%s requires exactly one argument", command)
}

func errNoDestinationArgument(command string) error {
	return errors.Errorf("%s requires at least two arguments, but only one was provided. Destination could not be determined", command)
}

func errBadHeredoc(command string, option string) error {
	return errors.Errorf("%s cannot accept a heredoc as %s", command, option)
}

func errBlankCommandNames(command string) error {
	return errors.Errorf("%s names can not be blank", command)
}

func errTooManyArguments(command string) error {
	return errors.Errorf("Bad input to %s, too many arguments", command)
}

func getDocComment(comments []string, name string) string {
	if name == "" {
		return ""
	}
	for _, line := range comments {
		if after, ok := strings.CutPrefix(line, name+" "); ok {
			return after
		}
	}
	return ""
}

func allInstructionNames() []string {
	out := make([]string, len(command.Commands))
	i := 0
	for name := range command.Commands {
		out[i] = strings.ToUpper(name)
		i++
	}
	return out
}

func isLowerCaseStageName(cmdArgs []string) bool {
	if len(cmdArgs) != 3 {
		return true
	}
	stageName := cmdArgs[2]
	return stageName == strings.ToLower(stageName)
}

func doesFromCaseMatchAsCase(req parseRequest) bool {
	if len(req.args) < 3 {
		return true
	}
	// consistent casing for the command is handled elsewhere.
	// If the command is not consistent, there's no need to
	// add an additional lint warning for the `as` argument.
	fromHasLowerCasing := req.command == strings.ToLower(req.command)
	fromHasUpperCasing := req.command == strings.ToUpper(req.command)
	if !fromHasLowerCasing && !fromHasUpperCasing {
		return true
	}

	if fromHasLowerCasing {
		return req.args[1] == strings.ToLower(req.args[1])
	}
	return req.args[1] == strings.ToUpper(req.args[1])
}

func validateDefinitionDescription(instruction string, argKeys []string, descComments []string, location []parser.Range, lint *linter.Linter) {
	if len(descComments) == 0 || len(argKeys) == 0 {
		return
	}
	descCommentParts := strings.Split(descComments[len(descComments)-1], " ")
	if slices.Contains(argKeys, descCommentParts[0]) {
		return
	}
	exampleKey := argKeys[0]
	if len(argKeys) > 1 {
		exampleKey = "<arg_key>"
	}

	msg := linter.RuleInvalidDefinitionDescription.Format(instruction, exampleKey)
	lint.Run(&linter.RuleInvalidDefinitionDescription, location, msg)
}

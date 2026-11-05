package instructions

import (
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// KeyValuePair represents an arbitrary named value.
//
// This is useful for commands containing key-value maps that want to preserve
// the order of insertion, instead of map[string]string which does not.
type KeyValuePair struct {
	Key     string
	Value   string
	NoDelim bool
}

func (kvp *KeyValuePair) String() string {
	return kvp.Key + "=" + kvp.Value
}

// KeyValuePairOptional is identical to KeyValuePair, but allows for optional values.
type KeyValuePairOptional struct {
	Key        string
	Value      *string
	DocComment string
}

func (kvpo *KeyValuePairOptional) String() string {
	return kvpo.Key + "=" + kvpo.ValueString()
}

func (kvpo *KeyValuePairOptional) ValueString() string {
	v := ""
	if kvpo.Value != nil {
		v = *kvpo.Value
	}
	return v
}

// Command interface is implemented by every possible command in a Dockerfile.
//
// The interface only exposes the minimal common elements shared between every
// command, while more detailed information per-command can be extracted using
// runtime type analysis, e.g. type-switches.
type Command interface {
	Name() string
	Location() []parser.Range
	Comments() []string
}

// KeyValuePairs is a slice of KeyValuePair
type KeyValuePairs []KeyValuePair

// withNameAndCode is the base of every command in a Dockerfile (String() returns its source code)
type withNameAndCode struct {
	code     string
	name     string
	location []parser.Range
	comments []string
}

func (c *withNameAndCode) String() string {
	return c.code
}

// Name of the command
func (c *withNameAndCode) Name() string {
	return c.name
}

// Location of the command in source
func (c *withNameAndCode) Location() []parser.Range {
	return c.location
}

func (c *withNameAndCode) Comments() []string {
	return c.comments
}

func newWithNameAndCode(req parseRequest) withNameAndCode {
	return withNameAndCode{
		code:     strings.TrimSpace(req.original),
		name:     req.command,
		location: req.location,
		comments: req.comments,
	}
}

// SingleWordExpander is a provider for variable expansion where a single word
// corresponds to a single output.
type SingleWordExpander func(word string) (string, error)

// SupportsSingleWordExpansion interface allows a command to support variable.
type SupportsSingleWordExpansion interface {
	Expand(expander SingleWordExpander) error
}

// SupportsSingleWordExpansionRaw interface allows a command to support
// variable expansion, while ensuring that minimal transformations are applied
// during expansion, so that quotes and other special characters are preserved.
type SupportsSingleWordExpansionRaw interface {
	ExpandRaw(expander SingleWordExpander) error
}

// PlatformSpecific adds platform checks to a command
type PlatformSpecific interface {
	CheckPlatform(platform string) error
}

func expandKvp(kvp KeyValuePair, expander SingleWordExpander) (KeyValuePair, error) {
	key, err := expander(kvp.Key)
	if err != nil {
		return KeyValuePair{}, err
	}
	value, err := expander(kvp.Value)
	if err != nil {
		return KeyValuePair{}, err
	}
	return KeyValuePair{Key: key, Value: value, NoDelim: kvp.NoDelim}, nil
}

func expandKvpsInPlace(kvps KeyValuePairs, expander SingleWordExpander) error {
	for i, kvp := range kvps {
		newKvp, err := expandKvp(kvp, expander)
		if err != nil {
			return err
		}
		kvps[i] = newKvp
	}
	return nil
}

func expandSliceInPlace(values []string, expander SingleWordExpander) error {
	for i, v := range values {
		newValue, err := expander(v)
		if err != nil {
			return err
		}
		values[i] = newValue
	}
	return nil
}

// EnvCommand allows setting an variable in the container's environment.
//
//	ENV key1 value1 [keyN valueN...]
//	ENV --unset key1 [keyN...]
type EnvCommand struct {
	withNameAndCode
	Env   KeyValuePairs
	Unset bool // Env keys are removed, values are empty
}

func (c *EnvCommand) Expand(expander SingleWordExpander) error {
	return expandKvpsInPlace(c.Env, expander)
}

// MaintainerCommand (deprecated) allows specifying a maintainer details for
// the image.
//
//	MAINTAINER maintainer_name
type MaintainerCommand struct {
	withNameAndCode
	Maintainer string
}

// NewLabelCommand creates a new 'LABEL' command
func NewLabelCommand(k string, v string, noExp bool) *LabelCommand {
	kvp := KeyValuePair{Key: k, Value: v}
	c := "LABEL "
	c += kvp.String()
	nc := withNameAndCode{code: c, name: "label"}
	cmd := &LabelCommand{
		withNameAndCode: nc,
		Labels: KeyValuePairs{
			kvp,
		},
		noExpand: noExp,
	}
	return cmd
}

// LabelCommand sets an image label in the output
//
//	LABEL some json data describing the image
//	LABEL --unset key1 [keyN...]
type LabelCommand struct {
	withNameAndCode
	Labels   KeyValuePairs
	Unset    bool // Labels keys are removed, values are empty
	noExpand bool
}

func (c *LabelCommand) Expand(expander SingleWordExpander) error {
	if c.noExpand {
		return nil
	}
	return expandKvpsInPlace(c.Labels, expander)
}

// SourceContent represents an anonymous file object
type SourceContent struct {
	Path   string // path to the file
	Data   string // string content from the file
	Expand bool   // whether to expand file contents
}

// SourcesAndDest represent a collection of sources and a destination
type SourcesAndDest struct {
	DestPath       string          // destination to write output
	SourcePaths    []string        // file path sources
	SourceContents []SourceContent // anonymous file sources
}

func (s *SourcesAndDest) Expand(expander SingleWordExpander) error {
	err := expandSliceInPlace(s.SourcePaths, expander)
	if err != nil {
		return err
	}

	expandedDestPath, err := expander(s.DestPath)
	if err != nil {
		return err
	}
	s.DestPath = expandedDestPath

	return nil
}

func (s *SourcesAndDest) ExpandRaw(expander SingleWordExpander) error {
	for i, content := range s.SourceContents {
		if !content.Expand {
			continue
		}

		expandedData, err := expander(content.Data)
		if err != nil {
			return err
		}
		s.SourceContents[i].Data = expandedData
	}
	return nil
}

// AddCommand adds files from the provided sources to the target destination.
//
//	ADD foo /path
//
// ADD supports tarball and remote URL handling, which may not always be
// desired - if you do not wish to have this automatic handling, use COPY.
type AddCommand struct {
	withNameAndCode
	SourcesAndDest
	Chown           string
	Chmod           string
	Link            bool
	ExcludePatterns []string
	KeepGitDir      *bool // whether to keep .git dir, only meaningful for git sources
	Checksum        string
	Unpack          *bool
}

func (c *AddCommand) Expand(expander SingleWordExpander) error {
	expandedChown, err := expander(c.Chown)
	if err != nil {
		return err
	}
	c.Chown = expandedChown

	expandedChmod, err := expander(c.Chmod)
	if err != nil {
		return err
	}
	c.Chmod = expandedChmod

	expandedChecksum, err := expander(c.Checksum)
	if err != nil {
		return err
	}
	c.Checksum = expandedChecksum

	return c.SourcesAndDest.Expand(expander)
}

// CopyCommand copies files from the provided sources to the target destination.
//
//	COPY foo /path
//
// Same as 'ADD' but without the magic additional tarball and remote URL handling.
type CopyCommand struct {
	withNameAndCode
	SourcesAndDest
	From            string
	Chown           string
	Chmod           string
	Link            bool
	ExcludePatterns []string
	Parents         bool // parents preserves directory structure
}

func (c *CopyCommand) Expand(expander SingleWordExpander) error {
	expandedChown, err := expander(c.Chown)
	if err != nil {
		return err
	}
	c.Chown = expandedChown

	expandedChmod, err := expander(c.Chmod)
	if err != nil {
		return err
	}
	c.Chmod = expandedChmod

	return c.SourcesAndDest.Expand(expander)
}

// OnbuildCommand allows specifying a command to be run on builds the use the
// resulting build image as a base image.
//
//	ONBUILD <some other command>
type OnbuildCommand struct {
	withNameAndCode
	Expression string
}

// WorkdirCommand sets the current working directory for all future commands in
// the stage
//
//	WORKDIR /tmp
type WorkdirCommand struct {
	withNameAndCode
	Path string
}

func (c *WorkdirCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.Path)
	if err != nil {
		return err
	}
	c.Path = p
	return nil
}

// ShellInlineFile represents an inline file created for a shell command
type ShellInlineFile struct {
	Name  string
	Data  string
	Chomp bool
}

// ShellDependantCmdLine represents a cmdline optionally prepended with the shell
type ShellDependantCmdLine struct {
	CmdLine      []string
	Files        []ShellInlineFile
	PrependShell bool
}

// RunCommand runs a command.
//
//	RUN "echo hi"       # sh -c "echo hi"
//
// or
//
//	RUN ["echo", "hi"]  # echo hi
type RunCommand struct {
	withNameAndCode
	withExternalData
	ShellDependantCmdLine
	FlagsUsed []string
}

func (c *RunCommand) Expand(expander SingleWordExpander) error {
	if err := setMountState(c, expander); err != nil {
		return err
	}
	return nil
}

// CmdCommand sets the default command to run in the container on start.
//
//	CMD "echo hi"       # sh -c "echo hi"
//
// or
//
//	CMD ["echo", "hi"]  # echo hi
type CmdCommand struct {
	withNameAndCode
	ShellDependantCmdLine
}

// HealthCheckCommand sets the default healthcheck command to run in the container.
//
//	HEALTHCHECK <health-config>
type HealthCheckCommand struct {
	withNameAndCode
	Health *dockerspec.HealthcheckConfig
}

// EntrypointCommand sets the default entrypoint of the container to use the
// provided command.
//
//	ENTRYPOINT /usr/sbin/nginx
//
// Entrypoint uses the default shell if not in JSON format.
type EntrypointCommand struct {
	withNameAndCode
	ShellDependantCmdLine
}

// ExposeCommand marks a container port that can be exposed at runtime.
//
//	EXPOSE 6667/tcp 7000/tcp
type ExposeCommand struct {
	withNameAndCode
	Ports []string
}

// UserCommand sets the user for the rest of the stage, and when starting the
// container at run-time.
//
//	USER user
type UserCommand struct {
	withNameAndCode
	User string
}

func (c *UserCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.User)
	if err != nil {
		return err
	}
	c.User = p
	return nil
}

// VolumeCommand exposes the specified volume for use in the build environment.
//
//	VOLUME /foo
//	VOLUME --unset /foo
type VolumeCommand struct {
	withNameAndCode
	Volumes []string
	Unset   bool // Volumes are removed
}

func (c *VolumeCommand) Expand(expander SingleWordExpander) error {
	return expandSliceInPlace(c.Volumes, expander)
}

// StopSignalCommand sets the signal that will be used to kill the container.
//
//	STOPSIGNAL signal
type StopSignalCommand struct {
	withNameAndCode
	Signal string
}

func (c *StopSignalCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.Signal)
	if err != nil {
		return err
	}
	c.Signal = p
	return nil
}

// CheckPlatform checks that the command is supported in the target platform
func (c *StopSignalCommand) CheckPlatform(platform string) error {
	if platform == "windows" {
		return errors.New("The daemon on this platform does not support the command stopsignal")
	}
	return nil
}

// ArgCommand adds the specified variable to the list of variables that can be
// passed to the builder using the --build-arg flag for expansion and
// substitution.
//
//	ARG name[=value]
type ArgCommand struct {
	withNameAndCode
	Args []KeyValuePairOptional
}

func (c *ArgCommand) Expand(expander SingleWordExpander) error {
	for i, v := range c.Args {
		p, err := expander(v.Key)
		if err != nil {
			return err
		}
		v.Key = p
		if v.Value != nil {
			p, err = expander(*v.Value)
			if err != nil {
				return err
			}
			v.Value = &p
		}
		c.Args[i] = v
	}
	return nil
}

// ShellCommand sets a custom shell to use.
//
//	SHELL bash -e -c
type ShellCommand struct {
	withNameAndCode
	Shell []string
}

// Stage represents a bundled collection of commands.
//
// Each stage begins with a FROM command (which is consumed into the Stage),
// indicating the source or stage to derive from, and ends either at the
// end-of-the file, or the start of the next stage.
//
// Stages can be named, and can be additionally configured to use a specific
// platform, in the case of a multi-arch base image.
type Stage struct {
	Name     string    // name of the stage
	Commands []Command // commands contained within the stage
	OrigCmd  string    // original FROM command, used for rule checks
	BaseName string    // name of the base stage or source
	Platform string    // platform of base source to use
	Reset    []string  // base image config fields to reset, see ResetKeys

	DocComment string // doc-comment directly above the stage

	SourceCode string         // contents of the defining FROM command
	Location   []parser.Range // location of the defining FROM command
	Comments   []string
}

// AddCommand appends a command to the stage.
func (s *Stage) AddCommand(cmd Command) {
	// todo: validate cmd type
	s.Commands = append(s.Commands, cmd)
}

// IsCurrentStage returns true if the provided stage name is the name of the
// current stage, and false otherwise.
func IsCurrentStage(s []Stage, name string) bool {
	if len(s) == 0 {
		return false
	}
	return s[len(s)-1].Name == name
}

// CurrentStage returns the last stage from a list of stages.
func CurrentStage(s []Stage) (*Stage, error) {
	if len(s) == 0 {
		return nil, errors.New("no build stage in current context")
	}
	return &s[len(s)-1], nil
}

// HasStage looks for the presence of a given stage name from a list of stages.
func HasStage(s []Stage, name string) (int, bool) {
	for i, stage := range s {
		// Stage name is case-insensitive by design
		if strings.EqualFold(stage.Name, name) {
			return i, true
		}
	}
	return -1, false
}

type withExternalData struct {
	m map[any]any
}

func (c *withExternalData) getExternalValue(k any) any {
	return c.m[k]
}

func (c *withExternalData) setExternalValue(k, v any) {
	if c.m == nil {
		c.m = map[any]any{}
	}
	c.m[k] = v
}

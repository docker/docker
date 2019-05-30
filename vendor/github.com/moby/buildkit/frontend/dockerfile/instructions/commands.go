package instructions

import (
	"errors"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
)

// KeyValuePair represent an arbitrary named value (useful in slice instead of map[string] string to preserve ordering)
type KeyValuePair struct {
	Key   string
	Value string
}

func (kvp *KeyValuePair) String() string {
	return kvp.Key + "=" + kvp.Value
}

// KeyValuePairOptional is the same as KeyValuePair but Value is optional
type KeyValuePairOptional struct {
	Key   string
	Value *string
}

func (kvpo *KeyValuePairOptional) ValueString() string {
	v := ""
	if kvpo.Value != nil {
		v = *kvpo.Value
	}
	return v
}

// Command is implemented by every command present in a dockerfile
type Command interface {
	Name() string
}

// KeyValuePairs is a slice of KeyValuePair
type KeyValuePairs []KeyValuePair

// withNameAndCode is the base of every command in a Dockerfile (String() returns its source code)
type withNameAndCode struct {
	code string
	name string
}

func (c *withNameAndCode) String() string {
	return c.code
}

// Name of the command
func (c *withNameAndCode) Name() string {
	return c.name
}

func newWithNameAndCode(req parseRequest) withNameAndCode {
	return withNameAndCode{code: strings.TrimSpace(req.original), name: req.command}
}

// SingleWordExpander is a provider for variable expansion where 1 word => 1 output
type SingleWordExpander func(word string) (string, error)

// SupportsSingleWordExpansion interface marks a command as supporting variable expansion
type SupportsSingleWordExpansion interface {
	Expand(expander SingleWordExpander) error
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
	return KeyValuePair{Key: key, Value: value}, nil
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

// EnvCommand : ENV key1 value1 [keyN valueN...]
type EnvCommand struct {
	withNameAndCode
	Env KeyValuePairs // kvp slice instead of map to preserve ordering
}

// Expand variables
func (c *EnvCommand) Expand(expander SingleWordExpander) error {
	return expandKvpsInPlace(c.Env, expander)
}

// MaintainerCommand : MAINTAINER maintainer_name
type MaintainerCommand struct {
	withNameAndCode
	Maintainer string
}

// NewLabelCommand creates a new 'LABEL' command
func NewLabelCommand(k string, v string, NoExp bool) *LabelCommand {
	kvp := KeyValuePair{Key: k, Value: v}
	c := "LABEL "
	c += kvp.String()
	nc := withNameAndCode{code: c, name: "label"}
	cmd := &LabelCommand{
		withNameAndCode: nc,
		Labels: KeyValuePairs{
			kvp,
		},
		noExpand: NoExp,
	}
	return cmd
}

// LabelCommand : LABEL some json data describing the image
//
// Sets the Label variable foo to bar,
//
type LabelCommand struct {
	withNameAndCode
	Labels   KeyValuePairs // kvp slice instead of map to preserve ordering
	noExpand bool
}

// Expand variables
func (c *LabelCommand) Expand(expander SingleWordExpander) error {
	if c.noExpand {
		return nil
	}
	return expandKvpsInPlace(c.Labels, expander)
}

// SourcesAndDest represent a list of source files and a destination
type SourcesAndDest []string

// Sources list the source paths
func (s SourcesAndDest) Sources() []string {
	res := make([]string, len(s)-1)
	copy(res, s[:len(s)-1])
	return res
}

// Dest path of the operation
func (s SourcesAndDest) Dest() string {
	return s[len(s)-1]
}

// AddCommand : ADD foo /path
//
// Add the file 'foo' to '/path'. Tarball and Remote URL (http, https) handling
// exist here. If you do not wish to have this automatic handling, use COPY.
//
type AddCommand struct {
	withNameAndCode
	SourcesAndDest
	Chown string
}

// Expand variables
func (c *AddCommand) Expand(expander SingleWordExpander) error {
	return expandSliceInPlace(c.SourcesAndDest, expander)
}

// CopyCommand : COPY foo /path
//
// Same as 'ADD' but without the tar and remote url handling.
//
type CopyCommand struct {
	withNameAndCode
	SourcesAndDest
	From  string
	Chown string
}

// Expand variables
func (c *CopyCommand) Expand(expander SingleWordExpander) error {
	expandedChown, err := expander(c.Chown)
	if err != nil {
		return err
	}
	c.Chown = expandedChown
	return expandSliceInPlace(c.SourcesAndDest, expander)
}

// OnbuildCommand : ONBUILD <some other command>
type OnbuildCommand struct {
	withNameAndCode
	Expression string
}

// WorkdirCommand : WORKDIR /tmp
//
// Set the working directory for future RUN/CMD/etc statements.
//
type WorkdirCommand struct {
	withNameAndCode
	Path string
}

// Expand variables
func (c *WorkdirCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.Path)
	if err != nil {
		return err
	}
	c.Path = p
	return nil
}

// ShellDependantCmdLine represents a cmdline optionally prepended with the shell
type ShellDependantCmdLine struct {
	CmdLine      strslice.StrSlice
	PrependShell bool
}

// RunCommand : RUN some command yo
//
// run a command and commit the image. Args are automatically prepended with
// the current SHELL which defaults to 'sh -c' under linux or 'cmd /S /C' under
// Windows, in the event there is only one argument The difference in processing:
//
// RUN echo hi          # sh -c echo hi       (Linux)
// RUN echo hi          # cmd /S /C echo hi   (Windows)
// RUN [ "echo", "hi" ] # echo hi
//
type RunCommand struct {
	withNameAndCode
	withExternalData
	ShellDependantCmdLine
}

// CmdCommand : CMD foo
//
// Set the default command to run in the container (which may be empty).
// Argument handling is the same as RUN.
//
type CmdCommand struct {
	withNameAndCode
	ShellDependantCmdLine
}

// HealthCheckCommand : HEALTHCHECK foo
//
// Set the default healthcheck command to run in the container (which may be empty).
// Argument handling is the same as RUN.
//
type HealthCheckCommand struct {
	withNameAndCode
	Health *container.HealthConfig
}

// EntrypointCommand : ENTRYPOINT /usr/sbin/nginx
//
// Set the entrypoint to /usr/sbin/nginx. Will accept the CMD as the arguments
// to /usr/sbin/nginx. Uses the default shell if not in JSON format.
//
// Handles command processing similar to CMD and RUN, only req.runConfig.Entrypoint
// is initialized at newBuilder time instead of through argument parsing.
//
type EntrypointCommand struct {
	withNameAndCode
	ShellDependantCmdLine
}

// ExposeCommand : EXPOSE 6667/tcp 7000/tcp
//
// Expose ports for links and port mappings. This all ends up in
// req.runConfig.ExposedPorts for runconfig.
//
type ExposeCommand struct {
	withNameAndCode
	Ports []string
}

// UserCommand : USER foo
//
// Set the user to 'foo' for future commands and when running the
// ENTRYPOINT/CMD at container run time.
//
type UserCommand struct {
	withNameAndCode
	User string
}

// Expand variables
func (c *UserCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.User)
	if err != nil {
		return err
	}
	c.User = p
	return nil
}

// VolumeCommand : VOLUME /foo
//
// Expose the volume /foo for use. Will also accept the JSON array form.
//
type VolumeCommand struct {
	withNameAndCode
	Volumes []string
}

// Expand variables
func (c *VolumeCommand) Expand(expander SingleWordExpander) error {
	return expandSliceInPlace(c.Volumes, expander)
}

// StopSignalCommand : STOPSIGNAL signal
//
// Set the signal that will be used to kill the container.
type StopSignalCommand struct {
	withNameAndCode
	Signal string
}

// Expand variables
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

// ArgCommand : ARG name[=value]
//
// Adds the variable foo to the trusted list of variables that can be passed
// to builder using the --build-arg flag for expansion/substitution or passing to 'run'.
// Dockerfile author may optionally set a default value of this variable.
type ArgCommand struct {
	withNameAndCode
	KeyValuePairOptional
}

// Expand variables
func (c *ArgCommand) Expand(expander SingleWordExpander) error {
	p, err := expander(c.Key)
	if err != nil {
		return err
	}
	c.Key = p
	if c.Value != nil {
		p, err = expander(*c.Value)
		if err != nil {
			return err
		}
		c.Value = &p
	}
	return nil
}

// ShellCommand : SHELL powershell -command
//
// Set the non-default shell to use.
type ShellCommand struct {
	withNameAndCode
	Shell strslice.StrSlice
}

// Stage represents a single stage in a multi-stage build
type Stage struct {
	Name       string
	Commands   []Command
	BaseName   string
	SourceCode string
	Platform   string
}

// AddCommand to the stage
func (s *Stage) AddCommand(cmd Command) {
	// todo: validate cmd type
	s.Commands = append(s.Commands, cmd)
}

// IsCurrentStage check if the stage name is the current stage
func IsCurrentStage(s []Stage, name string) bool {
	if len(s) == 0 {
		return false
	}
	return s[len(s)-1].Name == name
}

// CurrentStage return the last stage in a slice
func CurrentStage(s []Stage) (*Stage, error) {
	if len(s) == 0 {
		return nil, errors.New("No build stage in current context")
	}
	return &s[len(s)-1], nil
}

// HasStage looks for the presence of a given stage name
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
	m map[interface{}]interface{}
}

func (c *withExternalData) getExternalValue(k interface{}) interface{} {
	return c.m[k]
}

func (c *withExternalData) setExternalValue(k, v interface{}) {
	if c.m == nil {
		c.m = map[interface{}]interface{}{}
	}
	c.m[k] = v
}

package daemon // import "github.com/docker/docker/integration-cli/daemon"

import (
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/internal/test/daemon"
	"github.com/go-check/check"
	"github.com/pkg/errors"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

type testingT interface {
	assert.TestingT
	logT
	Fatalf(string, ...interface{})
}

type logT interface {
	Logf(string, ...interface{})
}

// Daemon represents a Docker daemon for the testing framework.
type Daemon struct {
	*daemon.Daemon
	dockerBinary string
}

// New returns a Daemon instance to be used for testing.
// This will create a directory such as d123456789 in the folder specified by $DOCKER_INTEGRATION_DAEMON_DEST or $DEST.
// The daemon will not automatically start.
func New(t testingT, dockerBinary string, dockerdBinary string, ops ...func(*daemon.Daemon)) *Daemon {
	ops = append(ops, daemon.WithDockerdBinary(dockerdBinary))
	d := daemon.New(t, ops...)
	return &Daemon{
		Daemon:       d,
		dockerBinary: dockerBinary,
	}
}

// Cmd executes a docker CLI command against this daemon.
// Example: d.Cmd("version") will run docker -H unix://path/to/unix.sock version
func (d *Daemon) Cmd(args ...string) (string, error) {
	result := icmd.RunCmd(d.Command(args...))
	return result.Combined(), result.Error
}

// Command creates a docker CLI command against this daemon, to be executed later.
// Example: d.Command("version") creates a command to run "docker -H unix://path/to/unix.sock version"
func (d *Daemon) Command(args ...string) icmd.Cmd {
	return icmd.Command(d.dockerBinary, d.PrependHostArg(args)...)
}

// PrependHostArg prepend the specified arguments by the daemon host flags
func (d *Daemon) PrependHostArg(args []string) []string {
	for _, arg := range args {
		if arg == "--host" || arg == "-H" {
			return args
		}
	}
	return append([]string{"--host", d.Sock()}, args...)
}

// GetIDByName returns the ID of an object (container, volume, …) given its name
func (d *Daemon) GetIDByName(name string) (string, error) {
	return d.inspectFieldWithError(name, "Id")
}

// InspectField returns the field filter by 'filter'
func (d *Daemon) InspectField(name, filter string) (string, error) {
	return d.inspectFilter(name, filter)
}

func (d *Daemon) inspectFilter(name, filter string) (string, error) {
	format := fmt.Sprintf("{{%s}}", filter)
	out, err := d.Cmd("inspect", "-f", format, name)
	if err != nil {
		return "", errors.Errorf("failed to inspect %s: %s", name, out)
	}
	return strings.TrimSpace(out), nil
}

func (d *Daemon) inspectFieldWithError(name, field string) (string, error) {
	return d.inspectFilter(name, fmt.Sprintf(".%s", field))
}

// CheckActiveContainerCount returns the number of active containers
// FIXME(vdemeester) should re-use ActivateContainers in some way
func (d *Daemon) CheckActiveContainerCount(c *check.C) (interface{}, check.CommentInterface) {
	out, err := d.Cmd("ps", "-q")
	assert.NilError(c, err)
	if len(strings.TrimSpace(out)) == 0 {
		return 0, nil
	}
	return len(strings.Split(strings.TrimSpace(out), "\n")), check.Commentf("output: %q", string(out))
}

// WaitRun waits for a container to be running for 10s
func (d *Daemon) WaitRun(contID string) error {
	args := []string{"--host", d.Sock()}
	return WaitInspectWithArgs(d.dockerBinary, contID, "{{.State.Running}}", "true", 10*time.Second, args...)
}

// WaitInspectWithArgs waits for the specified expression to be equals to the specified expected string in the given time.
// Deprecated: use cli.WaitCmd instead
func WaitInspectWithArgs(dockerBinary, name, expr, expected string, timeout time.Duration, arg ...string) error {
	after := time.After(timeout)

	args := append(arg, "inspect", "-f", expr, name)
	for {
		result := icmd.RunCommand(dockerBinary, args...)
		if result.Error != nil {
			if !strings.Contains(strings.ToLower(result.Stderr()), "no such") {
				return errors.Errorf("error executing docker inspect: %v\n%s",
					result.Stderr(), result.Stdout())
			}
			select {
			case <-after:
				return result.Error
			default:
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}

		out := strings.TrimSpace(result.Stdout())
		if out == expected {
			break
		}

		select {
		case <-after:
			return errors.Errorf("condition \"%q == %q\" not true in time (%v)", out, expected, timeout)
		default:
		}

		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

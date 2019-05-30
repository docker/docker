package icmd

import (
	"io"
	"time"
)

// CmdOp is an operation which modified a Cmd structure used to execute commands
type CmdOp func(*Cmd)

// WithTimeout sets the timeout duration of the command
func WithTimeout(timeout time.Duration) CmdOp {
	return func(c *Cmd) {
		c.Timeout = timeout
	}
}

// WithEnv sets the environment variable of the command.
// Each arguments are in the form of KEY=VALUE
func WithEnv(env ...string) CmdOp {
	return func(c *Cmd) {
		c.Env = env
	}
}

// Dir sets the working directory of the command
func Dir(path string) CmdOp {
	return func(c *Cmd) {
		c.Dir = path
	}
}

// WithStdin sets the standard input of the command to the specified reader
func WithStdin(r io.Reader) CmdOp {
	return func(c *Cmd) {
		c.Stdin = r
	}
}

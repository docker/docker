package gitutil

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/moby/buildkit/util/urlutil"
	"github.com/pkg/errors"
)

// GitCLI carries config to pass to the git cli to make running multiple
// commands less repetitive.
type GitCLI struct {
	git  string
	exec func(context.Context, *exec.Cmd) error

	args          []string
	dir           string
	streams       StreamFunc
	advice        *bool
	debugCommands bool

	workTree string
	gitDir   string

	sshAuthSock   string
	sshKnownHosts string
	hostGitConfig bool
}

// Option provides a variadic option for configuring the git client.
type Option func(b *GitCLI)

// WithGitBinary sets the git binary path.
func WithGitBinary(path string) Option {
	return func(b *GitCLI) {
		b.git = path
	}
}

// WithExec sets the command exec function.
func WithExec(exec func(context.Context, *exec.Cmd) error) Option {
	return func(b *GitCLI) {
		b.exec = exec
	}
}

// WithArgs sets extra args.
func WithArgs(args ...string) Option {
	return func(b *GitCLI) {
		b.args = append(b.args, args...)
	}
}

// WithGitAdvice controls whether Git advice messages are emitted.
func WithGitAdvice(enabled bool) Option {
	return func(b *GitCLI) {
		b.advice = &enabled
	}
}

// WithGitDebugCommands controls whether executed Git commands are logged.
func WithGitDebugCommands(debugCommands bool) Option {
	return func(b *GitCLI) {
		b.debugCommands = debugCommands
	}
}

// WithDir sets working directory.
//
// This should be a path to any directory within a standard git repository.
func WithDir(dir string) Option {
	return func(b *GitCLI) {
		b.dir = dir
	}
}

// WithWorkTree sets the --work-tree arg.
//
// This should be the path to the top-level directory of the checkout. When
// setting this, you also likely need to set WithGitDir.
func WithWorkTree(workTree string) Option {
	return func(b *GitCLI) {
		b.workTree = workTree
	}
}

// WithGitDir sets the --git-dir arg.
//
// This should be the path to the .git directory. When setting this, you may
// also need to set WithWorkTree, unless you are working with a bare
// repository.
func WithGitDir(gitDir string) Option {
	return func(b *GitCLI) {
		b.gitDir = gitDir
	}
}

// WithSSHAuthSock sets the ssh auth sock.
func WithSSHAuthSock(sshAuthSock string) Option {
	return func(b *GitCLI) {
		b.sshAuthSock = sshAuthSock
	}
}

// WithSSHKnownHosts sets the known hosts file.
func WithSSHKnownHosts(sshKnownHosts string) Option {
	return func(b *GitCLI) {
		b.sshKnownHosts = sshKnownHosts
	}
}

// WithHostGitConfig allows git to read the host system and user git config.
// This is intended for client-side local git inspection. The default remains
// isolated so daemon-side callers do not leak host configuration into git.
func WithHostGitConfig() Option {
	return func(b *GitCLI) {
		b.hostGitConfig = true
	}
}

type StreamFunc func(context.Context) (io.WriteCloser, io.WriteCloser, func())

// WithStreams configures a callback for getting the streams for a command. The
// stream callback will be called once for each command, and both writers will
// be closed after the command has finished.
func WithStreams(streams StreamFunc) Option {
	return func(b *GitCLI) {
		b.streams = streams
	}
}

// NewGitCLI initializes a new git client
func NewGitCLI(opts ...Option) *GitCLI {
	c := &GitCLI{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// New returns a new git client with the same config as the current one, but
// with the given options applied on top.
func (cli *GitCLI) New(opts ...Option) *GitCLI {
	clone := *cli
	clone.args = slices.Clone(cli.args)

	for _, opt := range opts {
		opt(&clone)
	}
	return &clone
}

// Run executes a git command with the given args.
func (cli *GitCLI) Run(ctx context.Context, args ...string) (_ []byte, err error) {
	gitBinary := "git"
	if cli.git != "" {
		gitBinary = cli.git
	}
	proxyEnvVars := [...]string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	}

	for {
		var cmd *exec.Cmd
		if cli.exec == nil {
			cmd = exec.CommandContext(ctx, gitBinary)
		} else {
			cmd = exec.CommandContext(context.TODO(), gitBinary)
		}

		cmd.Dir = cli.dir
		if cmd.Dir == "" {
			cmd.Dir = cli.workTree
		}

		// Block sneaky repositories from using repos from the filesystem as submodules.
		cmd.Args = append(cmd.Args, "-c", "protocol.file.allow=user")
		if cli.advice != nil && !*cli.advice {
			cmd.Args = append(cmd.Args, "-c", "advice.detachedHead=false")
		}
		if cli.workTree != "" {
			cmd.Args = append(cmd.Args, "--work-tree", cli.workTree)
		}
		if cli.gitDir != "" {
			cmd.Args = append(cmd.Args, "--git-dir", cli.gitDir)
		}
		cmd.Args = append(cmd.Args, cli.args...)
		cmd.Args = append(cmd.Args, args...)

		buf := bytes.NewBuffer(nil)
		errbuf := bytes.NewBuffer(nil)
		cmd.Stdin = nil
		cmd.Stdout = buf
		cmd.Stderr = errbuf
		var stdoutStream io.WriteCloser
		var stderrStream io.WriteCloser
		flush := func() {}
		if cli.streams != nil {
			var flushStream func()
			stdoutStream, stderrStream, flushStream = cli.streams(ctx)
			if flushStream != nil {
				flush = flushStream
			}
			if stdoutStream != nil {
				cmd.Stdout = io.MultiWriter(stdoutStream, cmd.Stdout)
				defer stdoutStream.Close()
			}
			if stderrStream != nil {
				cmd.Stderr = io.MultiWriter(stderrStream, cmd.Stderr)
				defer stderrStream.Close()
			}
			defer func() {
				if err != nil {
					flush()
				}
			}()
		}
		if cli.debugCommands && stderrStream != nil {
			_, _ = io.WriteString(stderrStream, "+ "+formatDebugCommand(cmd.Args)+"\n")
		}

		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_SSH_COMMAND=" + getGitSSHCommand(cli.sshKnownHosts),
			//	"GIT_TRACE=1",
			"LC_ALL=C", // Ensure consistent output.
		}
		if cli.advice != nil {
			if *cli.advice {
				cmd.Env = append(cmd.Env, "GIT_ADVICE=1")
			} else {
				cmd.Env = append(cmd.Env, "GIT_ADVICE=0")
			}
		}
		if cli.hostGitConfig {
			for _, ev := range [...]string{
				"HOME",
				"XDG_CONFIG_HOME",
				"USERPROFILE",
				"HOMEDRIVE",
				"HOMEPATH",
				"GIT_CONFIG_GLOBAL",
				"GIT_CONFIG_SYSTEM",
				// When git runs as root under sudo it consults SUDO_UID to trust
				// repositories owned by the user who invoked sudo, so client-side
				// callers such as `sudo docker build` don't trip git's "detected
				// dubious ownership" check and silently lose commit provenance.
				// This mirrors git's own default behavior and does not disable
				// safe.directory checks. See docker/buildx#3855.
				"SUDO_UID",
			} {
				if v, ok := os.LookupEnv(ev); ok {
					cmd.Env = append(cmd.Env, ev+"="+v)
				}
			}
		} else {
			cmd.Env = append(cmd.Env,
				"GIT_CONFIG_NOSYSTEM=1",         // Disable reading from system gitconfig.
				"HOME="+os.DevNull,              // Disable reading from user gitconfig.
				"GIT_CONFIG_GLOBAL="+os.DevNull, // Disable reading from global gitconfig.
			)
		}
		for _, ev := range proxyEnvVars {
			if v, ok := os.LookupEnv(ev); ok {
				cmd.Env = append(cmd.Env, ev+"="+v)
			}
		}
		if cli.sshAuthSock != "" {
			cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+cli.sshAuthSock)
		}

		if cli.exec != nil {
			// remote git commands spawn helper processes that inherit FDs and don't
			// handle parent death signal so exec.CommandContext can't be used
			err = cli.exec(ctx, cmd)
		} else {
			err = cmd.Run()
		}

		if err != nil {
			select {
			case <-ctx.Done():
				cerr := context.Cause(ctx)
				if cerr != nil {
					return buf.Bytes(), errors.Wrapf(cerr, "context completed: git stderr:\n%s", errbuf.String())
				}
			default:
			}

			if strings.Contains(errbuf.String(), "--depth") || strings.Contains(errbuf.String(), "shallow") {
				if newArgs := argsNoDepth(args); len(args) > len(newArgs) {
					args = newArgs
					continue
				}
			}
			if strings.Contains(errbuf.String(), "not our ref") || strings.Contains(errbuf.String(), "unadvertised object") {
				// server-side error: https://github.com/git/git/blob/34b6ce9b30747131b6e781ff718a45328aa887d0/upload-pack.c#L811-L812
				// client-side error: https://github.com/git/git/blob/34b6ce9b30747131b6e781ff718a45328aa887d0/fetch-pack.c#L2250-L2253
				if newArgs := argsNoCommitRefspec(args); len(args) > len(newArgs) {
					args = newArgs
					continue
				}
			}

			return buf.Bytes(), errors.Wrapf(err, "git stderr:\n%s", errbuf.String())
		}
		return buf.Bytes(), nil
	}
}

func formatDebugCommand(args []string) string {
	formatted := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" && i+1 < len(args) {
			formatted = append(formatted, arg, quoteDebugCommandArg(redactGitConfig(args[i+1])))
			i++
			continue
		}
		formatted = append(formatted, quoteDebugCommandArg(redactURLCredentials(arg)))
	}
	return strings.Join(formatted, " ")
}

func redactGitConfig(arg string) string {
	// URL-scoped keys can contain '=' before the header value separator.
	for i := range arg {
		if arg[i] == '=' && strings.HasSuffix(strings.ToLower(arg[:i]), ".extraheader") {
			return redactURLCredentials(arg[:i]) + "=xxxxx"
		}
	}
	return redactURLCredentials(arg)
}

func redactURLCredentials(arg string) string {
	if !strings.Contains(arg, "://") {
		return arg
	}
	return urlutil.RedactCredentials(arg)
}

// quoteDebugCommandArg preserves argument boundaries and escapes nonprintable
// characters for display. The output is not intended for shell re-execution.
func quoteDebugCommandArg(arg string) string {
	if arg == "" || !utf8.ValidString(arg) || strings.ContainsFunc(arg, func(r rune) bool {
		return !strconv.IsPrint(r) || strings.ContainsRune(" \"'\\", r)
	}) {
		return strconv.Quote(arg)
	}
	return arg
}

func getGitSSHCommand(knownHosts string) string {
	gitSSHCommand := "ssh -F " + os.DevNull
	if knownHosts != "" {
		gitSSHCommand += " -o UserKnownHostsFile=" + knownHosts
	} else {
		gitSSHCommand += " -o StrictHostKeyChecking=no"
	}
	return gitSSHCommand
}

func argsNoDepth(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != "--depth=1" {
			out = append(out, a)
		}
	}
	return out
}

func argsNoCommitRefspec(args []string) []string {
	if len(args) <= 2 {
		return args
	}
	if args[0] != "fetch" {
		return args
	}

	// assume the refspec is the last arg
	if IsCommitSHA(args[len(args)-1]) {
		return args[:len(args)-1]
	}

	return args
}

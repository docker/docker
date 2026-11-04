package dockerfile2llb

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/dfgitutil"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/system"
	"github.com/moby/patternmatcher"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	mode "github.com/tonistiigi/dchapes-mode"
)

type copyConfig struct {
	params          instructions.SourcesAndDest
	excludePatterns []string
	source          llb.State
	isAddCommand    bool
	cmdToPrint      fmt.Stringer
	chown           string
	chmod           string
	link            bool
	keepGitDir      *bool
	checksum        string
	parents         bool
	location        []parser.Range
	ignoreMatcher   *patternmatcher.PatternMatcher
	opt             dispatchOpt
	unpack          *bool
}

func dispatchCopy(d *dispatchState, cfg copyConfig) error {
	dest, err := pathRelativeToWorkingDir(d.state, cfg.params.DestPath, *d.platform)
	if err != nil {
		return err
	}

	var copyOpt []llb.CopyOption

	if cfg.chown != "" {
		if cfg.link && chownByName(cfg.chown) {
			return errors.Errorf("--chown=%s: user and group names can't be resolved when used with --link, use numeric uid[:gid] instead", cfg.chown)
		}
		copyOpt = append(copyOpt, llb.WithUser(cfg.chown))
	}

	if len(cfg.excludePatterns) > 0 {
		// in theory we don't need to check whether there are any exclude patterns,
		// as an empty list is a no-op. However, performing the check makes
		// the code easier to understand and costs virtually nothing.
		copyOpt = append(copyOpt, llb.WithExcludePatterns(cfg.excludePatterns))
	}

	var chopt *llb.ChmodOpt
	if cfg.chmod != "" {
		chopt = &llb.ChmodOpt{}
		p, err := strconv.ParseUint(cfg.chmod, 8, 32)
		nonOctalErr := errors.Errorf("invalid chmod parameter: '%v'. it should be octal string and between 0 and 07777", cfg.chmod)
		if err == nil {
			if p > 0o7777 {
				return nonOctalErr
			}
			chopt.Mode = os.FileMode(p)
		} else {
			if _, err := mode.Parse(cfg.chmod); err != nil {
				var ne *strconv.NumError
				if errors.As(err, &ne) {
					return nonOctalErr // return nonOctalErr for compatibility if the value looks numeric
				}
				return err
			}
			chopt.ModeStr = cfg.chmod
		}
	}

	if cfg.checksum != "" {
		if !cfg.isAddCommand {
			return errors.New("checksum can't be specified for COPY")
		}
		if len(cfg.params.SourcePaths) != 1 {
			return errors.New("checksum can't be specified for multiple sources")
		}
		if !isHTTPSource(cfg.params.SourcePaths[0]) && !isGitSource(cfg.params.SourcePaths[0]) {
			return errors.New("checksum requires HTTP(S) or Git sources")
		}
	}

	commitMessage := bytes.NewBufferString("")
	if cfg.isAddCommand {
		commitMessage.WriteString("ADD")
	} else {
		commitMessage.WriteString("COPY")
	}

	if cfg.parents {
		commitMessage.WriteString(" " + "--parents")
	}
	if cfg.chown != "" {
		commitMessage.WriteString(" " + "--chown=" + cfg.chown)
	}
	if cfg.chmod != "" {
		commitMessage.WriteString(" " + "--chmod=" + cfg.chmod)
	}

	platform := cfg.opt.targetPlatform
	if d.platform != nil {
		platform = *d.platform
	}

	env := getEnv(d.state)
	name := uppercaseCmd(processCmdEnv(cfg.opt.shlex, cfg.cmdToPrint.String(), env))
	pgName := prefixCommand(d, name, d.prefixPlatform, &platform, env)

	var a *llb.FileAction

	for _, src := range cfg.params.SourcePaths {
		commitMessage.WriteString(" " + src)
		gitRef, isGit, gitRefErr := dfgitutil.ParseGitRef(src)
		if gitRefErr != nil && isGit {
			return gitRefErr
		}
		if gitRefErr == nil && !gitRef.IndistinguishableFromLocal {
			if !cfg.isAddCommand {
				return errors.New("source can't be a git ref for COPY")
			}
			// TODO: print a warning (not an error) if gitRef.UnencryptedTCP is true
			gitOptions := []llb.GitOption{
				llb.WithCustomName(pgName),
				llb.GitRef(gitRef.Ref),
			}
			if cfg.opt.gitAdvice {
				gitOptions = append(gitOptions, llb.GitAdvice(true))
			}
			if cfg.opt.gitDebugCommands {
				gitOptions = append(gitOptions, llb.GitDebugCommands(true))
			}
			if cfg.keepGitDir != nil && gitRef.KeepGitDir != nil {
				if *cfg.keepGitDir != *gitRef.KeepGitDir {
					return errors.New("inconsistent keep-git-dir configuration")
				}
			}
			if gitRef.KeepGitDir != nil {
				cfg.keepGitDir = gitRef.KeepGitDir
			}
			if cfg.keepGitDir != nil && *cfg.keepGitDir {
				gitOptions = append(gitOptions, llb.KeepGitDir())
			}
			if cfg.checksum != "" && gitRef.Checksum != "" {
				if cfg.checksum != gitRef.Checksum {
					return errors.Errorf("checksum mismatch %q != %q", cfg.checksum, gitRef.Checksum)
				}
			}
			if gitRef.Checksum != "" {
				cfg.checksum = gitRef.Checksum
			}
			if cfg.checksum != "" {
				gitOptions = append(gitOptions, llb.GitChecksum(cfg.checksum))
			}
			if gitRef.SubDir != "" {
				gitOptions = append(gitOptions, llb.GitSubDir(gitRef.SubDir))
			}
			if gitRef.Submodules != nil && !*gitRef.Submodules {
				gitOptions = append(gitOptions, llb.GitSkipSubmodules())
			}
			if gitRef.FetchByCommit {
				gitOptions = append(gitOptions, llb.GitFetchByCommit())
			}

			st := llb.Git(gitRef.Remote, "", gitOptions...)
			opts := append([]llb.CopyOption{&llb.CopyInfo{
				Mode:           chopt,
				CreateDestPath: true,
			}}, copyOpt...)
			if a == nil {
				a = llb.Copy(st, "/", dest, opts...)
			} else {
				a = a.Copy(st, "/", dest, opts...)
			}
		} else if isHTTPSource(src) {
			if !cfg.isAddCommand {
				return errors.New("source can't be a URL for COPY")
			}

			// Resources from remote URLs are not decompressed.
			// https://docs.docker.com/engine/reference/builder/#add
			//
			// Note: mixing up remote archives and local archives in a single ADD instruction
			// would result in undefined behavior: https://github.com/moby/buildkit/pull/387#discussion_r189494717
			u, err := url.Parse(src)
			f := "__unnamed__"
			if err == nil {
				if base := path.Base(u.Path); base != "." && base != "/" {
					f = base
				}
			}

			var checksum digest.Digest
			if cfg.checksum != "" {
				checksum, err = digest.Parse(cfg.checksum)
				if err != nil {
					return err
				}
			}

			st := llb.HTTP(src, llb.Filename(f), llb.WithCustomName(pgName), llb.Checksum(checksum), dfCmd(cfg.params))

			var unpack bool
			if cfg.unpack != nil {
				unpack = *cfg.unpack
			}

			opts := append([]llb.CopyOption{&llb.CopyInfo{
				Mode:           chopt,
				CreateDestPath: true,
				AttemptUnpack:  unpack,
			}}, copyOpt...)

			if a == nil {
				a = llb.Copy(st, f, dest, opts...)
			} else {
				a = a.Copy(st, f, dest, opts...)
			}
		} else {
			validateCopySourcePath(src, &cfg)
			var patterns []string
			var requiredPaths []string
			if cfg.parents {
				// detect optional pivot point
				parent, pattern, ok := strings.Cut(src, "/./")
				if !ok {
					pattern = src
					src = "/"
				} else {
					src = parent
				}

				pattern, err = system.NormalizePath("/", pattern, d.platform.OS, false)
				if err != nil {
					return errors.Wrap(err, "removing drive letter")
				}

				patterns = []string{strings.TrimPrefix(pattern, "/")}

				// determine if we want to require any paths to exist.
				// we only require a path to exist if wildcards aren't present.
				if !containsWildcards(src) && !containsWildcards(pattern) {
					requiredPaths = []string{filepath.Join(src, pattern)}
				}
			}

			src, err = system.NormalizePath("/", src, d.platform.OS, false)
			if err != nil {
				return errors.Wrap(err, "removing drive letter")
			}

			for i, requiredPath := range requiredPaths {
				p, err := system.NormalizePath("/", requiredPath, d.platform.OS, false)
				if err != nil {
					return errors.Wrap(err, "removing drive letter")
				}
				requiredPaths[i] = p
			}

			unpack := cfg.isAddCommand
			if cfg.unpack != nil {
				unpack = *cfg.unpack
			}

			opts := append([]llb.CopyOption{&llb.CopyInfo{
				Mode:                chopt,
				FollowSymlinks:      true,
				CopyDirContentsOnly: true,
				IncludePatterns:     patterns,
				RequiredPaths:       requiredPaths,
				AttemptUnpack:       unpack,
				CreateDestPath:      true,
				AllowWildcard:       true,
				AllowEmptyWildcard:  true,
			}}, copyOpt...)

			if a == nil {
				a = llb.Copy(cfg.source, src, dest, opts...)
			} else {
				a = a.Copy(cfg.source, src, dest, opts...)
			}
		}
	}

	for _, src := range cfg.params.SourceContents {
		commitMessage.WriteString(" <<" + src.Path)

		data := src.Data
		f, err := system.CheckSystemDriveAndRemoveDriveLetter(src.Path, d.platform.OS, false)
		if err != nil {
			return errors.Wrap(err, "removing drive letter")
		}
		st := llb.Scratch().File(
			llb.Mkfile(f, 0644, []byte(data)),
			dockerui.WithInternalName("preparing inline document"),
			llb.Platform(*d.platform),
		)

		opts := append([]llb.CopyOption{&llb.CopyInfo{
			Mode:           chopt,
			CreateDestPath: true,
		}}, copyOpt...)

		if a == nil {
			a = llb.Copy(st, system.ToSlash(f, d.platform.OS), dest, opts...)
		} else {
			a = a.Copy(st, filepath.ToSlash(f), dest, opts...)
		}
	}

	commitMessage.WriteString(" " + cfg.params.DestPath)

	fileOpt := []llb.ConstraintsOpt{
		llb.WithCustomName(pgName),
		location(cfg.opt.sourceMap, cfg.location),
	}
	if d.ignoreCache {
		fileOpt = append(fileOpt, llb.IgnoreCache)
	}

	// cfg.opt.llbCaps can be nil in unit tests
	if cfg.opt.llbCaps != nil && cfg.opt.llbCaps.Supports(pb.CapMergeOp) == nil && cfg.link && cfg.chmod == "" {
		pgID := identity.NewID()
		d.cmdIndex-- // prefixCommand increases it
		pgName := prefixCommand(d, name, d.prefixPlatform, &platform, env)

		copyOpts := []llb.ConstraintsOpt{
			llb.Platform(*d.platform),
		}
		copyOpts = append(copyOpts, fileOpt...)
		copyOpts = append(copyOpts, llb.ProgressGroup(pgID, pgName, true))

		mergeOpts := slices.Clone(fileOpt)
		d.cmdIndex--
		mergeOpts = append(mergeOpts, llb.ProgressGroup(pgID, pgName, false), llb.WithCustomName(prefixCommand(d, "LINK "+name, d.prefixPlatform, &platform, env)))

		d.state = d.state.WithOutput(llb.Merge([]llb.State{d.state, llb.Scratch().File(a, copyOpts...)}, mergeOpts...).Output())
	} else {
		d.state = d.state.File(a, fileOpt...)
	}

	return commitToHistory(&d.image, commitMessage.String(), true, &d.state, d.epoch)
}

func isHTTPSource(src string) bool {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return false
	}
	return !isGitSource(src)
}

func isGitSource(src string) bool {
	// https://github.com/ORG/REPO.git is a git source, not an http source
	if gitRef, isGit, _ := dfgitutil.ParseGitRef(src); gitRef != nil && isGit {
		return true
	}
	return false
}

// chownByName reports whether chown refers to a user or group by name rather
// than by numeric ID. Names are resolved from /etc/passwd and /etc/group of the
// FileOp base, which is empty when copying into a linked layer, so --link
// rejects them regardless of how the current worker implements the copy.
func chownByName(chown string) bool {
	co, ok := llb.WithUser(chown).(llb.ChownOpt)
	if !ok {
		return false
	}
	return (co.User != nil && co.User.Name != "") || (co.Group != nil && co.Group.Name != "")
}

func containsWildcards(name string) bool {
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '*', '?', '[':
			return true
		case '\\':
			i++
		}
	}
	return false
}

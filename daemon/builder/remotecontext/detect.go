package remotecontext

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/continuity/driver"
	"github.com/containerd/log"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/moby/v2/daemon/builder"
	"github.com/moby/moby/v2/daemon/builder/remotecontext/urlutil"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/moby/sys/symlink"
	"github.com/pkg/errors"
)

// ClientSessionRemote is identifier for client-session context transport
const ClientSessionRemote = "client-session"

// Detect returns a context and dockerfile from remote location or local
// archive.
func Detect(config buildbackend.BuildConfig) (remote builder.Source, dockerfile *parser.Result, _ error) {
	remoteURL := config.Options.RemoteContext
	switch {
	case remoteURL == "":
		return newArchiveRemote(config.Source, config.Options.Dockerfile)
	case remoteURL == ClientSessionRemote:
		return nil, nil, errdefs.InvalidParameter(errors.New("experimental session with v1 builder is no longer supported, use builder version v2 (BuildKit) instead"))
	case urlutil.IsGitURL(remoteURL):
		return newGitRemote(remoteURL, config.Options.Dockerfile)
	case urlutil.IsURL(remoteURL):
		return newURLRemote(remoteURL, config.Options.Dockerfile, config.ProgressWriter.ProgressReaderFunc)
	default:
		return nil, nil, fmt.Errorf("remoteURL (%s) could not be recognized as URL", remoteURL)
	}
}

func newArchiveRemote(rc io.ReadCloser, dockerfilePath string) (builder.Source, *parser.Result, error) {
	defer rc.Close()
	c, err := FromArchive(rc)
	if err != nil {
		return nil, nil, err
	}

	return withDockerfileFromContext(c.(modifiableContext), dockerfilePath)
}

func withDockerfileFromContext(c modifiableContext, dockerfilePath string) (builder.Source, *parser.Result, error) {
	df, err := openAt(c, dockerfilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if dockerfilePath == builder.DefaultDockerfileName {
				lowercase := strings.ToLower(dockerfilePath)
				if _, err := StatAt(c, lowercase); err == nil {
					return withDockerfileFromContext(c, lowercase)
				}
			}
			return nil, nil, errors.Errorf("Cannot locate specified Dockerfile: %s", dockerfilePath) // backwards compatible error
		}
		c.Close()
		return nil, nil, err
	}

	res, err := readAndParseDockerfile(dockerfilePath, df)
	if err != nil {
		return nil, nil, err
	}

	df.Close()

	if err := removeDockerfile(c, dockerfilePath); err != nil {
		c.Close()
		return nil, nil, err
	}

	return c, res, nil
}

func newGitRemote(gitURL string, dockerfilePath string) (builder.Source, *parser.Result, error) {
	c, err := MakeGitContext(gitURL) // TODO: change this to NewLazySource
	if err != nil {
		return nil, nil, err
	}
	return withDockerfileFromContext(c.(modifiableContext), dockerfilePath)
}

func newURLRemote(url string, dockerfilePath string, progressReader func(in io.ReadCloser) io.ReadCloser) (builder.Source, *parser.Result, error) {
	contentType, content, err := downloadRemote(url)
	if err != nil {
		return nil, nil, err
	}
	defer content.Close()

	switch contentType {
	case mimeTypeTextPlain:
		res, err := parser.Parse(progressReader(content))
		return nil, res, errdefs.InvalidParameter(err)
	default:
		source, err := FromArchive(progressReader(content))
		if err != nil {
			return nil, nil, err
		}
		return withDockerfileFromContext(source.(modifiableContext), dockerfilePath)
	}
}

func removeDockerfile(c modifiableContext, filesToRemove ...string) error {
	f, err := openAt(c, ".dockerignore")
	// Note that a missing .dockerignore file isn't treated as an error
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return err
	}
	excludes, err := ignorefile.ReadAll(f)
	if err != nil {
		f.Close()
		return errors.Wrap(err, "error reading .dockerignore")
	}
	f.Close()
	filesToRemove = append([]string{".dockerignore"}, filesToRemove...)
	for _, fileToRemove := range filesToRemove {
		if rm, _ := patternmatcher.MatchesOrParentMatches(fileToRemove, excludes); rm {
			if err := c.Remove(fileToRemove); err != nil {
				log.G(context.TODO()).Errorf("failed to remove %s: %v", fileToRemove, err)
			}
		}
	}
	return nil
}

func readAndParseDockerfile(name string, rc io.Reader) (*parser.Result, error) {
	br := bufio.NewReader(rc)
	peeked, err := br.Peek(512)
	if len(peeked) == 0 {
		if errors.Is(err, io.EOF) {
			return nil, errdefs.InvalidParameter(errors.Errorf("the Dockerfile (%s) cannot be empty", name))
		}
		return nil, errors.Wrap(err, "unexpected error reading Dockerfile")
	}
	if syntaxImage, _, _, ok := parser.DetectSyntax(peeked); ok {
		return nil, errdefs.InvalidParameter(errors.Errorf("the Dockerfile uses a custom syntax (%s) which requires BuildKit. Refer to https://docs.docker.com/go/buildkit/ to learn how to build images with BuildKit enabled", syntaxImage))
	}

	dockerfile, err := parser.Parse(br)
	if err != nil {
		return nil, errdefs.InvalidParameter(errors.Wrapf(err, "failed to parse %s", name))
	}

	return dockerfile, nil
}

func openAt(remote builder.Source, path string) (driver.File, error) {
	fullPath, err := FullPath(remote, path)
	if err != nil {
		return nil, err
	}
	return os.Open(fullPath)
}

// StatAt is a helper for calling Stat on a path from a source
func StatAt(remote builder.Source, path string) (os.FileInfo, error) {
	fullPath, err := FullPath(remote, path)
	if err != nil {
		return nil, err
	}
	return os.Stat(fullPath)
}

// FullPath is a helper for getting a full path for a path from a source
func FullPath(remote builder.Source, path string) (string, error) {
	remoteRoot := remote.Root()
	fullPath, err := symlink.FollowSymlinkInScope(filepath.Join(remoteRoot, path), remoteRoot)
	if err != nil {
		if runtime.GOOS == "windows" {
			return "", fmt.Errorf("failed to resolve scoped path %s (%s): %s. Possible cause is a forbidden path outside the build context", path, fullPath, err)
		}
		return "", fmt.Errorf("forbidden path outside the build context: %s (%s)", path, fullPath) // backwards compat with old error
	}
	return fullPath, nil
}

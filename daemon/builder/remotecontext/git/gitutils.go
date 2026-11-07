package git

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/moby/sys/symlink"
	"github.com/pkg/errors"
)

type gitRepo struct {
	remote string
	ref    string
	subdir string

	isolateConfig bool
}

// CloneOption changes the behaviour of Clone().
type CloneOption func(*gitRepo)

// WithIsolatedConfig disables reading the user or system gitconfig files when
// performing Git operations.
func WithIsolatedConfig(v bool) CloneOption {
	return func(gr *gitRepo) {
		gr.isolateConfig = v
	}
}

// Clone clones a repository into a newly created directory which
// will be under "docker-build-git"
func Clone(remoteURL string, opts ...CloneOption) (string, error) {
	repo, err := parseRemoteURL(remoteURL)
	if err != nil {
		return "", err
	}

	for _, opt := range opts {
		opt(&repo)
	}

	return repo.clone()
}

func (repo gitRepo) clone() (checkoutDir string, retErr error) {
	fetch := fetchArgs(repo.remote, repo.ref)

	root, err := os.MkdirTemp("", "docker-build-git")
	if err != nil {
		return "", err
	}

	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(root)
		}
	}()

	if out, err := repo.gitWithinDir(root, "init"); err != nil {
		return "", errors.Wrapf(err, "failed to init repo at %s: %s", root, out)
	}

	// Add origin remote for compatibility with previous implementation that
	// used "git clone" and also to make sure local refs are created for branches
	if out, err := repo.gitWithinDir(root, "remote", "add", "origin", repo.remote); err != nil {
		return "", errors.Wrapf(err, "failed add origin repo at %s: %s", repo.remote, out)
	}

	if output, err := repo.gitWithinDir(root, fetch...); err != nil {
		return "", errors.Wrapf(err, "error fetching: %s", output)
	}

	checkoutDir, err = repo.checkout(root)
	if err != nil {
		return "", err
	}

	cmd := exec.Command("git", "submodule", "update", "--init", "--recursive", "--depth=1")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.Wrapf(err, "error initializing submodules: %s", output)
	}

	return checkoutDir, nil
}

func parseRemoteURL(remoteURL string) (gitRepo, error) {
	repo := gitRepo{}

	if !isGitTransport(remoteURL) {
		remoteURL = "https://" + remoteURL
	}

	if strings.HasPrefix(remoteURL, "git@") {
		// git@.. is not an URL, so cannot be parsed as URL
		var fragment string
		repo.remote, fragment, _ = strings.Cut(remoteURL, "#")
		repo.ref, repo.subdir = getRefAndSubdir(fragment)
	} else {
		u, err := url.Parse(remoteURL)
		if err != nil {
			return repo, err
		}

		repo.ref, repo.subdir = getRefAndSubdir(u.Fragment)
		u.Fragment = ""
		repo.remote = u.String()
	}

	if strings.HasPrefix(repo.ref, "-") {
		return gitRepo{}, errors.Errorf("invalid refspec: %s", repo.ref)
	}

	return repo, nil
}

func getRefAndSubdir(fragment string) (ref string, subdir string) {
	ref, subdir, _ = strings.Cut(fragment, ":")
	if ref == "" {
		ref = "master"
	}
	return ref, subdir
}

func fetchArgs(remoteURL string, ref string) []string {
	args := []string{"fetch"}

	if supportsShallowClone(remoteURL) {
		args = append(args, "--depth", "1")
	}

	return append(args, "origin", "--", ref)
}

// Check if a given git URL supports a shallow git clone,
// i.e. it is a non-HTTP server or a smart HTTP server.
func supportsShallowClone(remoteURL string) bool {
	if scheme := getScheme(remoteURL); scheme != "http" && scheme != "https" {
		// Non-HTTP protocols always support shallow clones
		return true
	}

	// Check if the HTTP server is smart
	// Smart servers must correctly respond to a query for the git-upload-pack service
	serviceURL := remoteURL + "/info/refs?service=git-upload-pack"

	// Try a HEAD request and fallback to a Get request on error
	res, err := tryHeadThenGet(serviceURL)
	if err != nil {
		return false
	}
	defer res.Body.Close()

	return res.StatusCode == http.StatusOK &&
		res.Header.Get("Content-Type") == "application/x-git-upload-pack-advertisement"
}

func tryHeadThenGet(url string) (*http.Response, error) {
	res, err := http.Head(url) // #nosec G107
	if err == nil && res.StatusCode == http.StatusOK {
		return res, nil
	}

	if err == nil {
		res.Body.Close()
	}

	// HEAD request failed
	return http.Get(url) // #nosec G107
}

func (repo gitRepo) checkout(root string) (string, error) {
	// Try checking out by ref name first. This will work on branches and sets
	// .git/HEAD to the current branch name
	if output, err := repo.gitWithinDir(root, "checkout", repo.ref); err != nil {
		// If checking out by branch name fails check out the last fetched ref
		if _, err2 := repo.gitWithinDir(root, "checkout", "FETCH_HEAD"); err2 != nil {
			return "", errors.Wrapf(err, "error checking out %s: %s", repo.ref, output)
		}
	}

	if repo.subdir != "" {
		newCtx, err := symlink.FollowSymlinkInScope(filepath.Join(root, repo.subdir), root)
		if err != nil {
			return "", errors.Wrapf(err, "error setting git context, %q not within git root", repo.subdir)
		}

		fi, err := os.Stat(newCtx)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", errors.Errorf("error setting git context, not a directory: %s", newCtx)
		}
		root = newCtx
	}

	return root, nil
}

func (repo gitRepo) gitWithinDir(dir string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "protocol.file.allow=never"}, args...) // Block sneaky repositories from using repos from the filesystem as submodules.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Disable unsafe remote protocols.
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL_FROM_USER=0")

	if repo.isolateConfig {
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_NOSYSTEM=1", // Disable reading from system gitconfig.
			"HOME=/dev/null",        // Disable reading from user gitconfig.
		)
	}

	return cmd.CombinedOutput()
}

// isGitTransport returns true if the provided str is a git transport by inspecting
// the prefix of the string for known protocols used in git.
func isGitTransport(str string) bool {
	if strings.HasPrefix(str, "git@") {
		return true
	}

	switch getScheme(str) {
	case "git", "http", "https", "ssh":
		return true
	}

	return false
}

// getScheme returns addresses' scheme in lowercase, or an empty
// string in case address is an invalid URL.
func getScheme(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return u.Scheme
}

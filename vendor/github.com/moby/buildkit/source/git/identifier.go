package git

import (
	"net/url"
	"path"
	"strings"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/moby/buildkit/solver/llbsolver/provenance"
	provenancetypes "github.com/moby/buildkit/solver/llbsolver/provenance/types"
	"github.com/moby/buildkit/source"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/moby/buildkit/util/gitutil"
	"github.com/moby/buildkit/util/sshutil"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type GitIdentifier struct {
	Remote           string
	Ref              string
	Checksum         string
	Subdir           string
	KeepGitDir       bool
	AuthTokenSecret  string
	AuthHeaderSecret string
	MountSSHSock     string
	KnownSSHHosts    string
	SkipSubmodules   bool
	MTime            string // "checkout" (default) or "commit"
	Advice           bool
	DebugCommands    bool
	FetchByCommit    bool

	// Bundle, when non-empty, instructs the git source to fetch commits from a
	// pre-built git bundle stored as a blob instead of fetching from the remote
	// repository. The locator must use the "docker-image+blob://" or
	// "oci-layout+blob://" scheme.
	Bundle string
	// BundleOCISessionID, when set, pins the OCI-layout bundle fetch to a
	// specific client session. Only meaningful when Bundle uses the
	// oci-layout+blob:// scheme.
	BundleOCISessionID string
	// BundleOCIStoreID, when set, overrides the OCI-layout store name
	// derived from the bundle locator. Only meaningful when Bundle uses the
	// oci-layout+blob:// scheme.
	BundleOCIStoreID string
	// CheckoutBundle, when true, produces a single-file git bundle at the
	// checkout mount root (filename "bundle") instead of a worktree.
	CheckoutBundle bool

	VerifySignature *GitSignatureVerifyOptions
}

type GitSignatureVerifyOptions struct {
	PubKey            []byte
	RejectExpiredKeys bool
	RequireSignedTag  bool // signed tag must be present
	IgnoreSignedTag   bool // even if signed tag is present, verify signature on commit object
}

func NewGitIdentifier(remoteURL string) (*GitIdentifier, error) {
	if !gitutil.IsGitTransport(remoteURL) {
		remoteURL = "https://" + remoteURL
	}
	u, err := gitutil.ParseURL(remoteURL)
	if err != nil {
		return nil, err
	}

	repo := GitIdentifier{Remote: u.Remote}
	if u.Opts != nil {
		repo.Ref = u.Opts.Ref
		repo.Subdir = u.Opts.Subdir
	}
	return &repo, nil
}

func validateFullRemoteURL(identifier, remoteURL string) (string, error) {
	if !gitutil.IsGitTransport(remoteURL) {
		remoteURL = "https://" + remoteURL
	}
	remote, err := gitutil.ParseURL(remoteURL)
	if err != nil {
		return "", errors.New("failed to parse git.fullurl")
	}

	if !gitutil.IsGitTransport(identifier) {
		identifier = "https://" + identifier
	}
	id, err := gitutil.ParseURL(identifier)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse Git source identifier")
	}

	if !strings.EqualFold(id.Host, remote.Host) || gitRepositoryPath(id.Path) != gitRepositoryPath(remote.Path) {
		return "", errors.New("git.fullurl does not match source identifier")
	}
	return normalizeGitRemote(remote, id.Path), nil
}

func gitRepositoryPath(p string) string {
	// Match the canonicalization used when llb.Git creates the identifier.
	return strings.TrimSuffix(path.Join("/", p), ".git")
}

func normalizeGitRemote(remote *gitutil.GitURL, repositoryPath string) string {
	repositoryPath = path.Join("/", repositoryPath)
	if !strings.HasPrefix(strings.ToLower(remote.Remote), remote.Scheme+"://") {
		if !strings.HasPrefix(remote.Path, "/") {
			repositoryPath = strings.TrimPrefix(repositoryPath, "/")
		}
		return (&sshutil.SCPStyleURL{
			User: remote.User,
			Host: remote.Host,
			Path: repositoryPath,
		}).String()
	}

	return (&url.URL{
		Scheme: remote.Scheme,
		User:   remote.User,
		Host:   remote.Host,
		Path:   repositoryPath,
	}).String()
}

func (id *GitIdentifier) String() string {
	remote := id.Remote
	if u, err := gitutil.ParseURL(remote); err == nil {
		remote = u.Host + path.Join("/", u.Path)
	}

	ref := srctypes.GitScheme + "://" + remote
	if id.Ref != "" || id.Subdir != "" {
		ref += "#" + id.Ref
		if id.Subdir != "" {
			ref += ":" + id.Subdir
		}
	}
	return ref
}

func (GitIdentifier) Scheme() string {
	return srctypes.GitScheme
}

var _ source.Identifier = (*GitIdentifier)(nil)

func (id *GitIdentifier) Capture(c *provenance.Capture, pin string) error {
	remoteURL := id.Remote
	if id.Ref != "" {
		remoteURL += "#" + id.Ref
	}
	gs := provenancetypes.GitSource{
		URL:    remoteURL,
		Commit: pin,
	}
	if id.Bundle != "" {
		gs.Bundle = &provenancetypes.GitBundle{URL: id.Bundle}
	}
	c.AddGit(gs)
	if id.AuthTokenSecret != "" {
		c.AddSecret(provenancetypes.Secret{
			ID:       id.AuthTokenSecret,
			Optional: true,
		})
	}
	if id.AuthHeaderSecret != "" {
		c.AddSecret(provenancetypes.Secret{
			ID:       id.AuthHeaderSecret,
			Optional: true,
		})
	}
	if id.MountSSHSock != "" {
		c.AddSSH(provenancetypes.SSH{
			ID:       id.MountSSHSock,
			Optional: true,
		})
	}
	return nil
}

// validateBundleAttrs enforces the bundle-mode constraints that are statically
// decidable at identifier parse time.
func validateBundleAttrs(id *GitIdentifier) error {
	if id.Bundle != "" {
		if _, _, _, _, err := parseBundleLocator(id.Bundle); err != nil {
			return err
		}
		if id.Checksum == "" {
			return errors.New("git.bundle requires git.checksum to be set")
		}
		// Reject the pathological "Ref=sha1, Checksum=sha2" case: if both
		// are set and Ref is itself a commit SHA, the two must agree.
		// Without this check bundle mode silently overwrites Ref with
		// Checksum in tryRemoteFetch and the mismatch never surfaces.
		if id.Ref != "" && gitutil.IsCommitSHA(id.Ref) && !strings.HasPrefix(id.Ref, id.Checksum) {
			return errors.Errorf("expected checksum to match %s, got %s", id.Checksum, id.Ref)
		}
	}
	if id.CheckoutBundle {
		if id.KeepGitDir {
			return errors.New("git.checkoutbundle is incompatible with git.keepgitdir")
		}
		if id.Subdir != "" {
			return errors.New("git.checkoutbundle is incompatible with git.subdir")
		}
	}
	return nil
}

// splitBundleLocator splits a locator of the form "<scheme>://<body>" into
// (scheme, body). It returns ("","") if raw does not contain a "://" separator.
// This avoids net/url for schemes like "oci-layout+blob" whose body contains
// a colon after '@' (the digest) and confuses url.Parse.
func splitBundleLocator(raw string) (string, string) {
	const sep = "://"
	scheme, body, ok := strings.Cut(raw, sep)
	if !ok {
		return "", ""
	}
	return scheme, body
}

// parseBundleLocator extracts the scheme, normalized reference string, the
// reference locator (i.e. the body before '@digest', used as the OCI-layout
// store id), and the digest from a bundle locator of the form
// "<scheme>://<ref>@sha256:<hex>". Only sha256 digests are accepted because
// downloadBundleToFile hashes with sha256 and would fail to match any other
// algorithm.
func parseBundleLocator(raw string) (string, string, string, digest.Digest, error) {
	scheme, body := splitBundleLocator(raw)
	if scheme == "" {
		return "", "", "", "", errors.Errorf("failed to parse git.bundle locator %q: missing scheme", raw)
	}
	if scheme != srctypes.DockerImageBlobScheme && scheme != srctypes.OCIBlobScheme {
		return "", "", "", "", errors.Errorf("git.bundle locator scheme %q is not supported; expected %q or %q", scheme, srctypes.DockerImageBlobScheme, srctypes.OCIBlobScheme)
	}
	ref, err := reference.Parse(body)
	if err != nil {
		return "", "", "", "", errors.Wrapf(err, "failed to parse git.bundle locator %q", raw)
	}
	dgst := ref.Digest()
	if err := dgst.Validate(); err != nil {
		return "", "", "", "", errors.Wrapf(err, "failed to parse git.bundle locator %q: invalid digest", raw)
	}
	if dgst.Algorithm() != digest.SHA256 {
		return "", "", "", "", errors.Errorf("git.bundle locator %q: digest algorithm %q not supported, expected %q", raw, dgst.Algorithm(), digest.SHA256)
	}
	return scheme, ref.String(), ref.Locator, dgst, nil
}

package git

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/sshforward"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/compat"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/gitutil"
	"github.com/moby/buildkit/util/gitutil/gitobject"
	"github.com/moby/buildkit/util/gitutil/gitsign"
	"github.com/moby/buildkit/util/pgpsign"
	"github.com/moby/buildkit/util/progress/logs"
	"github.com/moby/buildkit/util/urlutil"
	"github.com/moby/locker"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var defaultBranch = regexp.MustCompile(`refs/heads/(\S+)`)

type Opt struct {
	CacheAccessor cache.Accessor
	// RegistryHosts is used to fetch bundle blobs from a docker registry
	// when a git source uses GitBundleURL("docker-image+blob://...").
	// Optional: when unset, bundle mode requires oci-layout+blob.
	RegistryHosts docker.RegistryHosts
}

type Source struct {
	cache         cache.Accessor
	locker        *locker.Locker
	registryHosts docker.RegistryHosts
}

type Metadata struct {
	Ref            string
	Checksum       string
	CommitChecksum string

	CommitObject []byte
	TagObject    []byte
}

type MetadataOpts struct {
	ReturnObject bool
}

// Supported returns nil if the system supports Git source
func Supported() error {
	if err := exec.CommandContext(context.TODO(), "git", "version").Run(); err != nil {
		return errors.Wrap(err, "failed to find git binary")
	}
	return nil
}

func NewSource(opt Opt) (*Source, error) {
	gs := &Source{
		cache:         opt.CacheAccessor,
		locker:        locker.New(),
		registryHosts: opt.RegistryHosts,
	}
	return gs, nil
}

func (gs *Source) Schemes() []string {
	return []string{srctypes.GitScheme}
}

func (gs *Source) Identifier(scheme, ref string, attrs map[string]string, platform *pb.Platform) (source.Identifier, error) {
	id, err := NewGitIdentifier(ref)
	if err != nil {
		return nil, err
	}

	for k, v := range attrs {
		switch k {
		case pb.AttrKeepGitDir:
			if v == "true" {
				id.KeepGitDir = true
			}
		case pb.AttrFullRemoteURL:
			v, err = validateFullRemoteURL(id.Remote, v)
			if err != nil {
				return nil, err
			}
			id.Remote = v
		case pb.AttrAuthHeaderSecret:
			id.AuthHeaderSecret = v
		case pb.AttrAuthTokenSecret:
			id.AuthTokenSecret = v
		case pb.AttrKnownSSHHosts:
			id.KnownSSHHosts = v
		case pb.AttrMountSSHSock:
			id.MountSSHSock = v
		case pb.AttrGitChecksum:
			id.Checksum = v
		case pb.AttrGitSkipSubmodules:
			if v == "true" {
				id.SkipSubmodules = true
			}
		case pb.AttrGitSignatureVerifyPubKey:
			if id.VerifySignature == nil {
				id.VerifySignature = &GitSignatureVerifyOptions{}
			}
			id.VerifySignature.PubKey = []byte(v)
		case pb.AttrGitSignatureVerifyRejectExpired:
			if id.VerifySignature == nil {
				id.VerifySignature = &GitSignatureVerifyOptions{}
			}
			id.VerifySignature.RejectExpiredKeys = v == "true"
		case pb.AttrGitSignatureVerifyRequireSignedTag:
			if id.VerifySignature == nil {
				id.VerifySignature = &GitSignatureVerifyOptions{}
			}
			id.VerifySignature.RequireSignedTag = v == "true"
		case pb.AttrGitSignatureVerifyIgnoreSignedTag:
			if id.VerifySignature == nil {
				id.VerifySignature = &GitSignatureVerifyOptions{}
			}
			id.VerifySignature.IgnoreSignedTag = v == "true"
		case pb.AttrGitMTime:
			id.MTime = v
		case pb.AttrGitAdvice:
			id.Advice = v == "true"
		case pb.AttrGitDebugCommands:
			id.DebugCommands = v == "true"
		case pb.AttrGitFetchByCommit:
			id.FetchByCommit = v == "true"
		case pb.AttrGitBundle:
			id.Bundle = v
		case pb.AttrGitCheckoutBundle:
			id.CheckoutBundle = v == "true"
		case pb.AttrOCILayoutSessionID:
			id.BundleOCISessionID = v
		case pb.AttrOCILayoutStoreID:
			id.BundleOCIStoreID = v
		}
	}
	if err := validateGitRef(id.Ref); err != nil {
		return nil, err
	}
	if err := validateBundleAttrs(id); err != nil {
		return nil, err
	}

	return id, nil
}

// needs to be called with repo lock
func (gs *Source) mountRemote(ctx context.Context, remote string, authArgs []string, gitAdvice bool, debugCommands bool, sha256 bool, reset bool, g session.Group) (target string, release func() error, retErr error) {
	sis, err := searchGitRemote(ctx, gs.cache, remote)
	if err != nil {
		return "", nil, errors.Wrapf(err, "failed to search metadata for %s", urlutil.RedactCredentials(remote))
	}

	var remoteRef cache.MutableRef
	for _, si := range sis {
		if reset {
			if err := si.clearGitRemote(); err != nil {
				bklog.G(ctx).Warnf("failed to clear git remote metadata for %s %s: %v", urlutil.RedactCredentials(remote), si.ID(), err)
			}
		} else {
			remoteRef, err = gs.cache.GetMutable(ctx, si.ID())
			if err != nil {
				if errors.Is(err, cache.ErrLocked) {
					// should never really happen as no other function should access this metadata, but lets be graceful
					bklog.G(ctx).Warnf("mutable ref for %s  %s was locked: %v", urlutil.RedactCredentials(remote), si.ID(), err)
					continue
				}
				return "", nil, errors.Wrapf(err, "failed to get mutable ref for %s", urlutil.RedactCredentials(remote))
			}
			break
		}
	}

	initializeRepo := false
	if remoteRef == nil {
		remoteRef, err = gs.cache.New(ctx, nil, g, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("shared git repo for %s", urlutil.RedactCredentials(remote))))
		if err != nil {
			return "", nil, errors.Wrapf(err, "failed to create new mutable for %s", urlutil.RedactCredentials(remote))
		}
		initializeRepo = true
	}

	releaseRemoteRef := func() error {
		return remoteRef.Release(context.TODO())
	}

	defer func() {
		if retErr != nil && remoteRef != nil {
			releaseRemoteRef()
		}
	}()

	mount, err := remoteRef.Mount(ctx, false, g)
	if err != nil {
		return "", nil, err
	}

	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return "", nil, err
	}

	defer func() {
		if retErr != nil {
			lm.Unmount()
		}
	}()

	git := gitCLI(
		gitutil.WithGitAdvice(gitAdvice),
		gitutil.WithGitDebugCommands(debugCommands),
		gitutil.WithGitDir(dir),
		gitutil.WithArgs(authArgs...),
	)

	if initializeRepo {
		// Explicitly set the Git config 'init.defaultBranch' to the
		// implied default to suppress "hint:" output about not having a
		// default initial branch name set which otherwise spams unit
		// test logs.
		args := []string{"-c", "init.defaultBranch=master", "init", "--bare"}
		if sha256 {
			args = append(args, "--object-format=sha256")
		}
		if _, err := git.Run(ctx, args...); err != nil {
			return "", nil, errors.Wrapf(err, "failed to init repo at %s", dir)
		}

		if _, err := git.Run(ctx, "remote", "add", "origin", remote); err != nil {
			return "", nil, errors.Wrapf(err, "failed add origin repo at %s", dir)
		}

		// save new remote metadata
		md := cacheRefMetadata{remoteRef}
		if err := md.setGitRemote(remote); err != nil {
			return "", nil, err
		}
	}
	return dir, func() error {
		err := lm.Unmount()
		if err1 := releaseRemoteRef(); err == nil {
			err = err1
		}
		return err
	}, nil
}

type gitSourceHandler struct {
	*Source
	src         GitIdentifier
	cacheKey    string
	cacheCommit string
	sha256      bool
	sm          *session.Manager
	authArgs    []string

	// stagedBundleURL / stagedBundleCleanup cache the temp bundle staging
	// across resolveBundleMetadata (CacheKey) and tryRemoteFetch (Snapshot)
	// so the bundle blob is downloaded only once per handler.
	stagedBundleURL     string
	stagedBundleCleanup func() error
}

func (gs *gitSourceHandler) shaToCacheKey(sha, ref string) string {
	key := sha
	if gs.src.KeepGitDir {
		key += ".git"
		if ref != "" {
			key += "#" + ref
		}
	}
	if gs.src.Subdir != "" {
		key += ":" + gs.src.Subdir
	}
	if gs.src.SkipSubmodules {
		key += "(skip-submodules)"
	}
	if gs.src.MTime != "" && gs.src.MTime != "checkout" {
		key += "(mtime=" + gs.src.MTime + ")"
	}
	if gs.src.CheckoutBundle {
		key += "(bundle)"
	}
	return key
}

func (gs *Source) ResolveMetadata(ctx context.Context, id *GitIdentifier, sm *session.Manager, jobCtx solver.JobContext, opt MetadataOpts) (*Metadata, error) {
	gsh := &gitSourceHandler{
		src:    *id,
		Source: gs,
		sm:     sm,
	}
	// The handler is scoped to this call, so any staged bundle it owns
	// must be released before returning. When a jobCtx is present,
	// ensureStagedBundle hands ownership to the job and this is a no-op;
	// without a jobCtx (e.g. direct ResolveMetadata callers) this is the
	// only hook that frees the temp dir.
	defer func() {
		if err := gsh.releaseStagedBundle(); err != nil {
			bklog.G(ctx).Warnf("failed to release staged git bundle: %v", err)
		}
	}()
	md, err := gsh.resolveMetadata(ctx, jobCtx)
	if err != nil {
		return nil, err
	}

	gsh.cacheCommit = md.Checksum
	gsh.sha256 = len(md.Checksum) == 64

	if !opt.ReturnObject && id.VerifySignature == nil {
		return md, nil
	}

	if err := gsh.addGitObjectsToMetadata(ctx, jobCtx, md); err != nil {
		return nil, err
	}

	if id.VerifySignature != nil {
		if err := verifyGitSignature(md, id.VerifySignature); err != nil {
			return nil, err
		}
	}

	return md, nil
}

func verifyGitSignature(md *Metadata, opts *GitSignatureVerifyOptions) error {
	var tagVerifyError error
	if !opts.IgnoreSignedTag {
		if len(md.TagObject) > 0 {
			tagObj, err := gitobject.Parse(md.TagObject)
			if err != nil {
				return errors.Wrap(err, "failed to parse git tag object")
			}
			if err := tagObj.VerifyChecksum(md.Checksum); err != nil {
				return errors.Wrap(err, "tag object checksum verification failed")
			}
			tagVerifyError = gitsign.VerifySignature(tagObj, opts.PubKey, &pgpsign.VerifyPolicy{
				RejectExpiredKeys: opts.RejectExpiredKeys,
			})
			if tagVerifyError == nil {
				return nil
			}
		}
	}
	if opts.RequireSignedTag {
		if tagVerifyError != nil {
			return tagVerifyError
		}
		return errors.New("signed tag required but no signed tag found")
	}
	commitObj, err := gitobject.Parse(md.CommitObject)
	if err != nil {
		return errors.Wrap(err, "failed to parse git commit object")
	}
	expected := md.Checksum
	if md.CommitChecksum != "" {
		expected = md.CommitChecksum
	}
	if err := commitObj.VerifyChecksum(expected); err != nil {
		return errors.Wrap(err, "commit object checksum verification failed")
	}
	return gitsign.VerifySignature(commitObj, opts.PubKey, &pgpsign.VerifyPolicy{
		RejectExpiredKeys: opts.RejectExpiredKeys,
	})
}

func (gs *Source) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager, _ solver.Vertex) (source.SourceInstance, error) {
	gitIdentifier, ok := id.(*GitIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid git identifier %v", id)
	}

	return &gitSourceHandler{
		src:    *gitIdentifier,
		Source: gs,
		sm:     sm,
	}, nil
}

type authSecret struct {
	token bool
	name  string
}

func (gs *gitSourceHandler) authSecretNames() (sec []authSecret, _ error) {
	u, err := url.Parse(gs.src.Remote)
	if err != nil {
		return nil, err
	}
	if gs.src.AuthHeaderSecret != "" {
		sec = append(sec, authSecret{name: gs.src.AuthHeaderSecret + "." + u.Host})
	}
	if gs.src.AuthTokenSecret != "" {
		sec = append(sec, authSecret{name: gs.src.AuthTokenSecret + "." + u.Host, token: true})
	}
	if gs.src.AuthHeaderSecret != "" {
		sec = append(sec, authSecret{name: gs.src.AuthHeaderSecret})
	}
	if gs.src.AuthTokenSecret != "" {
		sec = append(sec, authSecret{name: gs.src.AuthTokenSecret, token: true})
	}
	return sec, nil
}

func (gs *gitSourceHandler) getAuthToken(ctx context.Context, g session.Group) error {
	if gs.authArgs != nil {
		return nil
	}
	sec, err := gs.authSecretNames()
	if err != nil {
		return err
	}
	err = gs.sm.Any(ctx, g, func(ctx context.Context, _ string, caller session.Caller) error {
		var err error
		for _, s := range sec {
			var dt []byte
			dt, err = secrets.GetSecret(ctx, caller, s.name)
			if err != nil {
				if errors.Is(err, secrets.ErrNotFound) {
					continue
				}
				return err
			}
			if s.token {
				dt = []byte("basic " + base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "x-access-token:%s", dt)))
			}
			gs.authArgs = []string{"-c", "http." + tokenScope(gs.src.Remote) + ".extraheader=Authorization: " + string(dt)}
			break
		}
		return err
	})
	if errors.Is(err, secrets.ErrNotFound) {
		err = nil
	}
	return err
}

func (gs *gitSourceHandler) mountSSHAuthSock(ctx context.Context, sshID string, g session.Group) (string, func() error, error) {
	var caller session.Caller
	err := gs.sm.Any(ctx, g, func(ctx context.Context, _ string, c session.Caller) error {
		if err := sshforward.CheckSSHID(ctx, c, sshID); err != nil {
			if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
				return errors.Errorf("no SSH key %q forwarded from the client", sshID)
			}

			return err
		}
		caller = c
		return nil
	})
	if err != nil {
		return "", nil, err
	}

	usr, err := user.Current()
	if err != nil {
		return "", nil, err
	}

	// best effort, default to root
	uid, _ := strconv.Atoi(usr.Uid)
	gid, _ := strconv.Atoi(usr.Gid)

	sock, cleanup, err := sshforward.MountSSHSocket(ctx, caller, sshforward.SocketOpt{
		ID:   sshID,
		UID:  uid,
		GID:  gid,
		Mode: 0700,
	})
	if err != nil {
		return "", nil, err
	}

	return sock, cleanup, nil
}

func (gs *gitSourceHandler) mountKnownHosts() (string, func() error, error) {
	if gs.src.KnownSSHHosts == "" {
		return "", nil, errors.Errorf("no configured known hosts forwarded from the client")
	}
	knownHosts, err := os.CreateTemp("", "")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error {
		return os.Remove(knownHosts.Name())
	}
	_, err = knownHosts.Write([]byte(gs.src.KnownSSHHosts))
	if err != nil {
		cleanup()
		return "", nil, err
	}
	err = knownHosts.Close()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return knownHosts.Name(), cleanup, nil
}

func (gs *gitSourceHandler) remoteKey() string {
	return gs.src.Remote + "#" + gs.src.Ref
}

func (gs *gitSourceHandler) resolveMetadata(ctx context.Context, jobCtx solver.JobContext) (md *Metadata, retErr error) {
	if gs.src.Checksum != "" {
		matched, err := regexp.MatchString("^[a-fA-F0-9]+$", gs.src.Checksum)
		if err != nil || !matched {
			return nil, errors.Errorf("invalid checksum %s for Git URL, expected hex commit hash", gs.src.Checksum)
		}
	}

	if gs.src.Bundle != "" {
		// Bundle mode stages the bundle in a temp bare repo and runs the
		// shared ls-remote code path against its file:// URL so the
		// resulting Metadata has the same ref shape as the non-bundle
		// path. Bundle mode does not need the per-remote lock: the temp
		// bare repo is private to this call.
		return gs.resolveBundleMetadata(ctx, jobCtx)
	}

	remote := gs.src.Remote
	gs.locker.Lock(remote)
	defer gs.locker.Unlock(remote)

	if gitutil.IsCommitSHA(gs.src.Ref) {
		if gs.src.Checksum != "" && !strings.HasPrefix(gs.src.Ref, gs.src.Checksum) {
			return nil, errors.Errorf("expected checksum to match %s, got %s", gs.src.Checksum, gs.src.Ref)
		}
		return &Metadata{
			Ref:      gs.src.Ref,
			Checksum: gs.src.Ref,
		}, nil
	}

	if gs.src.FetchByCommit {
		if gs.src.Checksum == "" {
			return nil, errors.Errorf("fetch-by-commit requires a checksum or a commit SHA ref")
		}
		if !gitutil.IsCommitSHA(gs.src.Checksum) {
			return nil, errors.Errorf("fetch-by-commit requires a full commit SHA checksum, got %q", gs.src.Checksum)
		}
		// Canonicalize unqualified refs so that cache keys match those
		// produced by the normal path's ls-remote-driven normalization.
		// Unqualified names are treated as branches (git's preferred
		// resolution); tags must be passed as "refs/tags/<name>".
		if gs.src.Ref != "" && !strings.HasPrefix(gs.src.Ref, "refs/") {
			gs.src.Ref = "refs/heads/" + gs.src.Ref
		}
		if gs.src.Ref == "" {
			gs.src.Ref = gs.src.Checksum
		}
		return &Metadata{
			Ref:      gs.src.Ref,
			Checksum: gs.src.Checksum,
		}, nil
	}

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()

		if rc := jobCtx.ResolverCache(); rc != nil {
			values, release, err := rc.Lock(gs.remoteKey())
			if err != nil {
				return nil, err
			}
			saveResolved := true
			defer func() {
				// untyped nil, a nil *Metadata would be stored as a value
				var v any
				if retErr == nil && saveResolved {
					v = md
				}
				if err := release(v); err != nil {
					bklog.G(ctx).Warnf("failed to release resolver cache lock for %s: %v", gs.remoteKey(), err)
				}
			}()
			for _, v := range values {
				v2, ok := v.(*Metadata)
				if !ok {
					return nil, errors.Errorf("invalid resolver cache value for %s: %T", gs.remoteKey(), v)
				}
				if gs.src.Checksum != "" && !strings.HasPrefix(v2.Checksum, gs.src.Checksum) {
					continue
				}
				saveResolved = false
				clone := *v2
				return &clone, nil
			}
		}
	}

	gs.getAuthToken(ctx, g)

	return gs.resolveMetadataFromURL(ctx, g, gs.src.Remote)
}

// resolveMetadataFromURL runs ls-remote against the given URL and parses the
// result into a Metadata. The URL may be the user's configured remote (for
// non-bundle mode) or a file:// URL for a staged bundle. Auth and lock
// management are the caller's responsibility.
func (gs *gitSourceHandler) resolveMetadataFromURL(ctx context.Context, g session.Group, remoteURL string) (*Metadata, error) {
	tmpGit, cleanup, err := gs.emptyGitCli(ctx, g)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	ref := gs.src.Ref
	if ref == "" {
		ref, err = getDefaultBranch(ctx, tmpGit, remoteURL)
		if err != nil {
			return nil, err
		}
	}
	// TODO: should we assume that remote tag is immutable? add a timer?

	buf, err := tmpGit.Run(ctx, "ls-remote", "--", remoteURL, ref, ref+"^{}")
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch remote %s", urlutil.RedactCredentials(remoteURL))
	}
	lines := strings.Split(string(buf), "\n")

	var (
		partialRef      = "refs/" + strings.TrimPrefix(ref, "refs/")
		headRef         = "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/")
		tagRef          = "refs/tags/" + strings.TrimPrefix(ref, "refs/tags/")
		annotatedTagRef = tagRef + "^{}" // dereferenced annotated tag
	)
	var sha, headSha, tagSha, annotatedTagSha string
	var usedRef string

	for _, line := range lines {
		lineSha, lineRef, _ := strings.Cut(line, "\t")
		switch lineRef {
		case headRef:
			headSha = lineSha
		case tagRef:
			tagSha = lineSha
		case annotatedTagRef:
			annotatedTagSha = lineSha
		case partialRef:
			sha = lineSha
			usedRef = lineRef
		}
	}

	// git-checkout prefers branches in case of ambiguity
	if sha == "" {
		sha = headSha
		usedRef = headRef
	}
	if sha != "" {
		annotatedTagSha = "" // ignore annotated tag if branch or commit matched
	}
	if sha == "" {
		sha = tagSha
		usedRef = tagRef
	}
	if sha == "" {
		return nil, errors.Errorf("repository does not contain ref %s, output: %q", ref, string(buf))
	}
	if !gitutil.IsCommitSHA(sha) {
		return nil, errors.Errorf("invalid commit sha %q", sha)
	}
	if gs.src.Checksum != "" {
		if !strings.HasPrefix(sha, gs.src.Checksum) && !strings.HasPrefix(annotatedTagSha, gs.src.Checksum) {
			exp := sha
			if annotatedTagSha != "" {
				exp = " or " + annotatedTagSha
			}
			return nil, errors.Errorf("expected checksum to match %s, got %s", gs.src.Checksum, exp)
		}
	}
	md := &Metadata{
		Ref:      usedRef,
		Checksum: sha,
	}
	if annotatedTagSha != "" && !gs.src.KeepGitDir {
		// prefer commit sha pointed by annotated tag if no git dir is kept for more matches
		md.CommitChecksum = annotatedTagSha
	}
	return md, nil
}

func (gs *gitSourceHandler) addGitObjectsToMetadata(ctx context.Context, jobCtx solver.JobContext, md *Metadata) error {
	repo, err := gs.remoteFetch(ctx, jobCtx)
	if err != nil {
		return err
	}
	defer repo.Release()

	// if ref was commit sha then we don't know the type of the object yet
	buf, err := repo.Run(ctx, "cat-file", "-t", md.Checksum)
	if err != nil {
		return err
	}
	objType := strings.TrimSpace(string(buf))

	if objType != "commit" && objType != "tag" {
		return errors.Errorf("expected commit or tag object, got %s", objType)
	}

	if objType == "tag" && md.CommitChecksum == "" {
		buf, err := repo.Run(ctx, "rev-parse", md.Checksum+"^{commit}")
		if err != nil {
			return err
		}
		md.CommitChecksum = strings.TrimSpace(string(buf))
	} else if objType == "commit" {
		md.CommitChecksum = ""
	}

	commitChecksum := md.Checksum
	if md.CommitChecksum != "" {
		buf, err := repo.Run(ctx, "cat-file", "tag", md.Checksum)
		if err != nil {
			return err
		}
		md.TagObject = buf
		commitChecksum = md.CommitChecksum
	}

	buf, err = repo.Run(ctx, "cat-file", "commit", commitChecksum)
	if err != nil {
		return err
	}
	md.CommitObject = buf
	return nil
}

func (gs *gitSourceHandler) CacheKey(ctx context.Context, jobCtx solver.JobContext, index int) (string, string, solver.CacheOpts, bool, error) {
	md, err := gs.resolveMetadata(ctx, jobCtx)
	if err != nil {
		return "", "", nil, false, err
	}

	gs.sha256 = len(md.Checksum) == 64

	if gs.src.VerifySignature != nil {
		gs.cacheCommit = md.Checksum
		if err := gs.addGitObjectsToMetadata(ctx, jobCtx, md); err != nil {
			return "", "", nil, false, err
		}
		if err := verifyGitSignature(md, gs.src.VerifySignature); err != nil {
			return "", "", nil, false, err
		}
	}

	if gitutil.IsCommitSHA(md.Ref) {
		cacheKey := gs.shaToCacheKey(md.Ref, md.Ref)
		gs.cacheKey = cacheKey
		gs.cacheCommit = md.Ref
		// gs.src.Checksum is verified when checking out the commit
		return cacheKey, md.Ref, nil, true, nil
	}

	shaForCacheKey := md.Checksum
	if md.CommitChecksum != "" && !gs.src.KeepGitDir {
		// prefer commit sha pointed by annotated tag if no git dir is kept for more matches
		shaForCacheKey = md.CommitChecksum
	}
	cacheKey := gs.shaToCacheKey(shaForCacheKey, md.Ref)
	gs.cacheKey = cacheKey
	gs.cacheCommit = md.Checksum
	return cacheKey, md.Checksum, nil, true, nil
}

func (gs *gitSourceHandler) remoteFetch(ctx context.Context, jobCtx solver.JobContext) (_ *gitRepo, retErr error) {
	gs.locker.Lock(gs.src.Remote)
	cleanup := func() error { return gs.locker.Unlock(gs.src.Remote) }

	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}

	repo, err := gs.tryRemoteFetch(ctx, jobCtx, g, false)
	if err != nil {
		var wce *wouldClobberExistingTagError
		var ulre *unableToUpdateLocalRefError
		if errors.As(err, &wce) || errors.As(err, &ulre) {
			repo, err = gs.tryRemoteFetch(ctx, jobCtx, g, true)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	repo.releasers = append(repo.releasers, cleanup)

	defer func() {
		if retErr != nil {
			repo.Release()
			repo = nil
		}
	}()

	ref := gs.src.Ref
	// With fetch-by-commit, the user-provided ref is not written into the
	// bare repo, so resolve the checksum directly.
	if gs.src.FetchByCommit && gs.src.Checksum != "" {
		ref = gs.src.Checksum
	}
	git := repo.GitCLI
	if gs.src.Checksum != "" {
		actualHashBuf, err := repo.Run(ctx, "rev-parse", ref)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to rev-parse %s for %s", ref, urlutil.RedactCredentials(gs.src.Remote))
		}
		actualHash := strings.TrimSpace(string(actualHashBuf))
		if !strings.HasPrefix(actualHash, gs.src.Checksum) {
			retErr := errors.Errorf("expected checksum to match %s, got %s", gs.src.Checksum, actualHash)
			actualHashBuf2, err := git.Run(ctx, "rev-parse", ref+"^{}")
			if err != nil {
				return nil, retErr
			}
			actualHash2 := strings.TrimSpace(string(actualHashBuf2))
			if actualHash2 == actualHash {
				return nil, retErr
			}
			if !strings.HasPrefix(actualHash2, gs.src.Checksum) {
				return nil, errors.Errorf("expected checksum to match %s, got %s or %s", gs.src.Checksum, actualHash, actualHash2)
			}
		}
	}

	return repo, nil
}

func (gs *gitSourceHandler) Snapshot(ctx context.Context, jobCtx solver.JobContext) (cache.ImmutableRef, error) {
	cacheKey := gs.cacheKey
	if cacheKey == "" {
		var err error
		cacheKey, _, _, _, err = gs.CacheKey(ctx, jobCtx, 0)
		if err != nil {
			return nil, err
		}
	}

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}
	gs.getAuthToken(ctx, g)
	compatibilityVersion := 0
	if jobCtx != nil {
		var err error
		compatibilityVersion, err = jobCtx.CompatibilityVersion()
		if err != nil {
			return nil, err
		}
	}

	snapshotKey := cacheKey + ":" + gs.src.Subdir
	gs.locker.Lock(snapshotKey)
	defer gs.locker.Unlock(snapshotKey)

	sis, err := searchGitSnapshot(ctx, gs.cache, snapshotKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to search metadata for %s", snapshotKey)
	}
	if len(sis) > 0 {
		return gs.cache.Get(ctx, sis[0].ID(), nil)
	}

	repo, err := gs.remoteFetch(ctx, jobCtx)
	if err != nil {
		return nil, err
	}
	defer repo.Release()

	var ref cache.ImmutableRef
	if gs.src.CheckoutBundle {
		ref, err = gs.checkoutAsBundle(ctx, repo, g)
	} else {
		ref, err = gs.checkout(ctx, repo, g, compatibilityVersion)
	}
	if err != nil {
		return nil, err
	}

	md := cacheRefMetadata{ref}
	if err := md.setGitSnapshot(snapshotKey); err != nil {
		return nil, err
	}
	return ref, nil
}

type gitRepo struct {
	*gitutil.GitCLI
	dir       string
	releasers []func() error
}

func (g *gitRepo) Release() error {
	var err error
	for _, r := range g.releasers {
		if err1 := r(); err == nil {
			err = err1
		}
	}
	return err
}

func (gs *gitSourceHandler) tryRemoteFetch(ctx context.Context, jobCtx solver.JobContext, g session.Group, reset bool) (_ *gitRepo, retErr error) {
	repo := &gitRepo{}

	defer func() {
		if retErr != nil {
			repo.Release()
			repo = nil
		}
	}()

	// Auth args only apply to non-bundle fetches. In bundle mode the
	// payload comes from a blob fetch, so there is no http remote to
	// authenticate against.
	authArgs := gs.authArgs
	if gs.src.Bundle != "" {
		authArgs = nil
	}

	git, cleanup, err := gs.emptyGitCli(ctx, g)
	if err != nil {
		return nil, err
	}
	repo.releasers = append(repo.releasers, cleanup)

	// Bundle mode may need to create the shared bare repo before the main
	// fetch. Stage the bundle first so stageBundle can probe the bundle's
	// object format via ls-remote and set gs.sha256 for mountRemote.
	stagedURL := ""
	if gs.src.Bundle != "" {
		stagedURL, err = gs.ensureStagedBundle(ctx, jobCtx, g)
		if err != nil {
			return nil, err
		}
	}

	gitDir, unmountGitDir, err := gs.mountRemote(ctx, gs.src.Remote, authArgs, gs.src.Advice, gs.src.DebugCommands, gs.sha256, reset, g)
	if err != nil {
		return nil, err
	}
	repo.releasers = append(repo.releasers, unmountGitDir)
	repo.dir = gitDir

	git = git.New(gitutil.WithGitDir(gitDir))
	repo.GitCLI = git

	// fetchSource is the URL used in place of the "origin" remote name for
	// the main fetch. In bundle mode it points at a staged bundle's file://
	// URL; an empty value means fetch from the configured "origin" remote.
	fetchSource := ""
	// Bundle mode: stage the bundle into an isolated temp bare repo and
	// fetch from it as a file:// remote. The rest of this function runs
	// unchanged — git accepts a URL anywhere a remote name is expected,
	// so the fetch flow treats the staged repo like a normal origin.
	if gs.src.Bundle != "" {
		// When the user did not supply a symbolic ref, use the pinned
		// commit as the ref directly. validateBundleAttrs guarantees
		// Checksum is set.
		if gs.src.Ref == "" || gitutil.IsCommitSHA(gs.src.Ref) {
			gs.src.Ref = gs.src.Checksum
		}
		// Bundle mode enters tryRemoteFetch from Snapshot. If CacheKey
		// has not run yet, prime gs.cacheCommit from the pinned checksum
		// to make the downstream cacheCommit validation a no-op rather
		// than a spurious mismatch.
		if gs.cacheCommit == "" {
			gs.cacheCommit = gs.src.Checksum
		}
		// If the pinned commit is already present in the shared bare
		// repo (from a prior origin or bundle fetch), skip staging
		// entirely. The doFetch check below will no-op the fetch.
		if _, err := repo.Run(ctx, "cat-file", "-e", gs.src.Checksum+"^{commit}"); err != nil {
			// Reuse the staged bundle if CacheKey or the eager bundle-format
			// probe above already built one. ensureStagedBundle owns the
			// teardown (wired to jobCtx.Cleanup on first call), so the
			// cleanup is intentionally not appended to repo.releasers.
			fetchSource = stagedURL
		}
	}

	ref := gs.src.Ref
	if ref == "" {
		ref, err = getDefaultBranch(ctx, git, gs.src.Remote)
		if err != nil {
			return nil, err
		}
		gs.src.Ref = ref
	}
	// fetchRef is the identifier used to fetch from the remote. For fetch-by-commit
	// mode this is the checksum (commit SHA); otherwise it is the ref itself.
	fetchRef := ref
	if gs.src.FetchByCommit && gs.src.Checksum != "" {
		fetchRef = gs.src.Checksum
	}
	doFetch := true
	if gitutil.IsCommitSHA(fetchRef) {
		// skip fetch if commit already exists
		if _, err := git.Run(ctx, "cat-file", "-e", "--", fetchRef+"^{commit}"); err == nil {
			doFetch = false
		}
	}

	// local refs are needed so they would be advertised on next fetches. Force is used
	// in case the ref is a branch and it now points to a different commit sha
	// TODO: is there a better way to do this?
	targetRef := ref
	if !strings.HasPrefix(ref, "refs/tags/") {
		targetRef = "tags/" + ref
	}

	if doFetch {
		gitDirRoot, err := os.OpenRoot(gitDir)
		if err != nil {
			return nil, errors.Wrap(err, "failed to open git dir root")
		}
		defer gitDirRoot.Close()

		// make sure no old lock files have leaked
		gitDirRoot.RemoveAll("shallow.lock")

		origin := "origin"
		if fetchSource != "" {
			origin = fetchSource
		}
		args := []string{"fetch"}
		// For fetch-by-commit, assume the server supports
		// uploadpack.allowReachableSHA1InWant / allowAnySHA1InWant and
		// fetch only the requested commit, without the tag/unshallow
		// fallback used by the generic SHA-ref path.
		if gs.src.FetchByCommit || !gitutil.IsCommitSHA(fetchRef) { // TODO: find a branch from ls-remote?
			args = append(args, "--depth=1", "--no-tags")
		} else {
			args = append(args, "--tags")
			if _, err := gitDirRoot.Lstat("shallow"); err == nil {
				args = append(args, "--unshallow")
			}
		}
		args = append(args, origin)
		if gitutil.IsCommitSHA(ref) {
			args = append(args, ref)
		} else {
			args = append(args, "--force", "--", fetchRef+":"+targetRef)
		}
		if _, err := git.Run(ctx, args...); err != nil {
			err := errors.Wrapf(err, "failed to fetch remote %s", urlutil.RedactCredentials(gs.src.Remote))
			if strings.Contains(err.Error(), "rejected") && strings.Contains(err.Error(), "(would clobber existing tag)") {
				// this can happen if a tag was mutated to another commit in remote.
				// only hope is to abandon the existing shared repo and start a fresh one
				return nil, &wouldClobberExistingTagError{err}
			}
			if isUnableToUpdateLocalRef(err) {
				// this can happen if a branch updated in remote so that old branch
				// is now a parent dir of a new branch
				return nil, &unableToUpdateLocalRefError{err}
			}

			return nil, err
		}

		// verify that commit matches the cache key commit
		dt, err := git.Run(ctx, "rev-parse", fetchRef)
		if err != nil {
			return nil, err
		}
		// if fetched ref does not match cache key, the remote side has changed the ref
		// if possible we can try to force the commit that the cache key points to, otherwise we need to error
		if strings.TrimSpace(string(dt)) != gs.cacheCommit {
			uptRef := targetRef
			if !strings.HasPrefix(uptRef, "refs/") {
				uptRef = "refs/" + uptRef
			}
			// check if the commit still exists in the repo
			if _, err := git.Run(ctx, "cat-file", "-e", gs.cacheCommit); err == nil {
				// force the ref to point to the commit that the cache key points to
				if _, err := git.Run(ctx, "update-ref", uptRef, gs.cacheCommit, "--no-deref"); err != nil {
					return nil, err
				}
			} else {
				// try to fetch the commit directly
				args := []string{"fetch", "--tags"}
				if _, err := gitDirRoot.Lstat("shallow"); err == nil {
					args = append(args, "--unshallow")
				}
				args = append(args, origin, gs.cacheCommit)
				if _, err := git.Run(ctx, args...); err != nil {
					return nil, errors.Wrapf(err, "failed to fetch remote %s", urlutil.RedactCredentials(gs.src.Remote))
				}

				_, err = git.Run(ctx, "reflog", "expire", "--all", "--expire=now")
				if err != nil {
					return nil, errors.Wrapf(err, "failed to expire reflog for remote %s", urlutil.RedactCredentials(gs.src.Remote))
				}
				if _, err := git.Run(ctx, "cat-file", "-e", gs.cacheCommit); err == nil {
					// force the ref to point to the commit that the cache key points to
					if _, err := git.Run(ctx, "update-ref", uptRef, gs.cacheCommit, "--no-deref"); err != nil {
						return nil, err
					}
				} else {
					return nil, errors.Errorf("fetched ref %s does not match expected commit %s and commit can not be found in the repository", ref, gs.cacheCommit)
				}
			}
		}
	}

	return repo, nil
}

func (gs *gitSourceHandler) checkout(ctx context.Context, repo *gitRepo, g session.Group, compatibilityVersion int) (_ cache.ImmutableRef, retErr error) {
	ref := gs.src.Ref
	// refOrCommit is the identifier to use against the bare repo. For
	// fetch-by-commit, the user-provided ref is not written into the bare
	// repo, so fall back to the commit checksum which is always present.
	refOrCommit := ref
	if gs.src.FetchByCommit && gs.src.Checksum != "" {
		refOrCommit = gs.src.Checksum
	}
	// In bundle mode, if the user did not supply a symbolic Ref (empty or
	// a SHA), fall back to the pinned checksum so the downstream
	// `git checkout` / `git fetch` has a valid tip. With a symbolic ref
	// present, use it as-is: bundle import lands refs in the natural
	// refs/heads/* / refs/tags/* namespace in the shared bare repo, so
	// `git checkout <ref>` and `git fetch origin <ref>` resolve it
	// normally and the resulting .git carries the natural ref name.
	if gs.src.Bundle != "" && (ref == "" || gitutil.IsCommitSHA(ref)) {
		ref = gs.src.Checksum
		refOrCommit = ref
	}
	checkoutRef, err := gs.cache.New(ctx, nil, g, cache.WithRecordType(client.UsageRecordTypeGitCheckout), cache.WithDescription(fmt.Sprintf("git snapshot for %s#%s", urlutil.RedactCredentials(gs.src.Remote), gs.src.Ref)))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create new mutable for %s", urlutil.RedactCredentials(gs.src.Remote))
	}

	defer func() {
		if retErr != nil && checkoutRef != nil {
			checkoutRef.Release(context.WithoutCancel(ctx))
		}
	}()

	git := repo.GitCLI
	gitDir := repo.dir

	mount, err := checkoutRef.Mount(ctx, false, g)
	if err != nil {
		return nil, err
	}
	lm := snapshot.LocalMounter(mount)
	checkoutDir, err := lm.Mount()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	subdir := path.Join("/", gs.src.Subdir)
	if subdir == "/" {
		subdir = "."
	}

	cd := checkoutDir
	if gs.src.KeepGitDir && subdir == "." {
		checkoutDirGit := filepath.Join(checkoutDir, ".git")
		if err := os.MkdirAll(checkoutDir, 0711); err != nil {
			return nil, err
		}
		checkoutGit := git.New(gitutil.WithWorkTree(checkoutDir), gitutil.WithGitDir(checkoutDirGit))
		args := []string{"-c", "init.defaultBranch=master", "init"}
		if gs.sha256 {
			args = append(args, "--object-format=sha256")
		}
		_, err = checkoutGit.Run(ctx, args...)
		if err != nil {
			return nil, err
		}
		// Defense-in-depth: clone using the file protocol to disable local-clone
		// optimizations which can be abused on some versions of Git to copy unintended
		// host files into the build context.
		_, err = checkoutGit.Run(ctx, "remote", "add", "origin", "file://"+gitDir)
		if err != nil {
			return nil, err
		}

		gitCatFileBuf, err := git.Run(ctx, "cat-file", "-t", refOrCommit)
		if err != nil {
			return nil, err
		}
		isAnnotatedTag := strings.TrimSpace(string(gitCatFileBuf)) == "tag"

		pullref := ref
		if isAnnotatedTag {
			targetRef := pullref
			if !strings.HasPrefix(pullref, "refs/tags/") {
				targetRef = "refs/tags/" + pullref
			}
			pullref += ":" + targetRef
		} else if gs.src.FetchByCommit && ref != refOrCommit {
			// Fetch the commit object from the bare repo and save it under the
			// user-provided ref name in the checkout clone. The ref does not
			// need to exist in the bare repo.
			pullref = refOrCommit + ":" + ref
		} else if gitutil.IsCommitSHA(ref) {
			pullref = "refs/buildkit/" + identity.NewID()
			_, err = git.Run(ctx, "update-ref", pullref, ref)
			if err != nil {
				return nil, err
			}
		} else {
			pullref += ":" + pullref
		}
		_, err = checkoutGit.Run(ctx, "fetch", "-u", "--depth=1", "--", "origin", pullref)
		if err != nil {
			return nil, err
		}
		_, err = checkoutGit.Run(ctx, "checkout", "FETCH_HEAD")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to checkout remote %s", urlutil.RedactCredentials(gs.src.Remote))
		}
		_, err = checkoutGit.Run(ctx, "remote", "set-url", "origin", urlutil.RedactCredentials(gs.src.Remote))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to set remote origin to %s", urlutil.RedactCredentials(gs.src.Remote))
		}
		_, err = checkoutGit.Run(ctx, "reflog", "expire", "--all", "--expire=now")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to expire reflog for remote %s", urlutil.RedactCredentials(gs.src.Remote))
		}
		if err := os.Remove(filepath.Join(checkoutDirGit, "FETCH_HEAD")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrapf(err, "failed to remove FETCH_HEAD for remote %s", urlutil.RedactCredentials(gs.src.Remote))
		}
		gitDir = checkoutDirGit
	} else {
		if subdir != "." {
			cd, err = os.MkdirTemp(cd, "checkout")
			if err != nil {
				return nil, errors.Wrapf(err, "failed to create temporary checkout dir")
			}
		}
		checkoutGit := git.New(gitutil.WithWorkTree(cd), gitutil.WithGitDir(gitDir))
		_, err = checkoutGit.Run(ctx, "checkout", "--no-overlay", refOrCommit, "--", ".")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to checkout remote %s", urlutil.RedactCredentials(gs.src.Remote))
		}
	}

	git = git.New(gitutil.WithWorkTree(cd), gitutil.WithGitDir(gitDir))
	if !gs.src.SkipSubmodules {
		_, err = git.Run(ctx, "submodule", "update", "--init", "--recursive", "--depth=1")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to update submodules for %s", urlutil.RedactCredentials(gs.src.Remote))
		}
	}

	if subdir != "." {
		subdir = filepath.FromSlash(subdir)
		subdir = rootRelativePath(subdir)
		cdRoot, err := os.OpenRoot(cd)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to open checkout dir root")
		}
		defer cdRoot.Close()

		if err := validateDirsOnly(cdRoot, subdir); err != nil {
			return nil, errors.Wrapf(err, "invalid subdir %v", subdir)
		}

		d, err := cdRoot.Open(subdir)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to open subdir %v", subdir)
		}
		defer func() {
			if d != nil {
				d.Close()
			}
		}()
		names, err := d.Readdirnames(0)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if err := os.Rename(filepath.Join(cd, subdir, n), filepath.Join(checkoutDir, n)); err != nil {
				return nil, err
			}
		}
		if err := d.Close(); err != nil {
			return nil, err
		}
		d = nil // reset defer
		if err := os.RemoveAll(cd); err != nil {
			return nil, err
		}
	}

	checkoutRoot, err := os.OpenRoot(checkoutDir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open git checkout root")
	}
	defer checkoutRoot.Close()

	if compatibilityVersion == compat.CompatibilityVersion013 {
		if err := resetCompatibility014FileModes(checkoutRoot); err != nil {
			return nil, errors.Wrapf(err, "failed to normalize compatibility file modes for %s", urlutil.RedactCredentials(gs.src.Remote))
		}
	}

	if gs.src.MTime == "commit" {
		commitTime, err := getCommitTime(ctx, repo.GitCLI, refOrCommit)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get commit time for %s", urlutil.RedactCredentials(gs.src.Remote))
		}
		if err := resetSnapshotMtimes(checkoutRoot, commitTime); err != nil {
			return nil, errors.Wrapf(err, "failed to normalize mtimes for %s", urlutil.RedactCredentials(gs.src.Remote))
		}
	}

	if idmap := mount.IdentityMapping(); idmap != nil {
		uid, gid := idmap.RootPair()
		root, err := os.OpenRoot(gitDir)
		if err != nil {
			return nil, errors.Wrap(err, "failed to open git checkout root")
		}
		err = fs.WalkDir(root.FS(), ".", func(p string, _ os.DirEntry, _ error) error {
			return root.Lchown(p, uid, gid)
		})
		if closeErr := root.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, errors.Wrap(err, "failed to remap git checkout")
		}
	}

	lm.Unmount()
	lm = nil

	snap, err := checkoutRef.Commit(ctx)
	if err != nil {
		return nil, err
	}
	checkoutRef = nil

	defer func() {
		if retErr != nil {
			snap.Release(context.WithoutCancel(ctx))
		}
	}()

	return snap, nil
}

// getCommitTime returns the committer timestamp of the resolved commit.
// For annotated tags, it peels to the underlying commit.
func getCommitTime(ctx context.Context, git *gitutil.GitCLI, ref string) (time.Time, error) {
	// %ct = committer date, UNIX timestamp; ^{commit} peels tags
	buf, err := git.Run(ctx, "log", "-1", "--format=%ct", ref+"^{commit}")
	if err != nil {
		return time.Time{}, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "failed to parse commit timestamp %q", string(buf))
	}
	return time.Unix(ts, 0), nil
}

// resetSnapshotMtimes walks dir and sets the mtime of every file,
// symlink, and directory to t. Directories are set bottom-up so that
// a parent's mtime is not invalidated by a later child write.
func resetSnapshotMtimes(root *os.Root, t time.Time) error {
	var dirs []string
	err := fs.WalkDir(root.FS(), ".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return lchtimes(filepath.Join(root.Name(), p), t)
		}
		return root.Chtimes(p, t, t)
	})
	if err != nil {
		return err
	}
	for _, dir := range slices.Backward(dirs) {
		if err := root.Chtimes(dir, t, t); err != nil {
			return err
		}
	}
	return nil
}

// resetCompatibility014FileModes restores the pre-v0.15 git checkout file
// mode for non-executable regular files, which were stored with group/other
// write bits set before the exec-option propagation fix. Executable files are
// left untouched: their pre-v0.15 behavior is not covered by the current
// compatibility matrix, and blindly adding write bits to 0o755 would be a
// guess.
func resetCompatibility014FileModes(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !mode.IsRegular() || mode&0o111 != 0 {
			return nil
		}
		return root.Chmod(p, mode|0o222)
	})
}

type wouldClobberExistingTagError struct {
	error
}

func (e *wouldClobberExistingTagError) Unwrap() error {
	return e.error
}

type unableToUpdateLocalRefError struct {
	error
}

func (e *unableToUpdateLocalRefError) Unwrap() error {
	return e.error
}

func isUnableToUpdateLocalRef(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "some local refs could not be updated;") {
		return false
	}
	return strings.Contains(msg, "(unable to update local ref)") ||
		strings.Contains(msg, "refname conflict")
}

func validateGitRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return errors.Errorf("invalid git ref %q", ref)
	}
	return nil
}

func (gs *gitSourceHandler) emptyGitCli(ctx context.Context, g session.Group, opts ...gitutil.Option) (*gitutil.GitCLI, func() error, error) {
	var cleanups []func() error
	cleanup := func() error {
		var err error
		for _, c := range cleanups {
			if err1 := c(); err == nil {
				err = err1
			}
		}
		cleanups = nil
		return err
	}
	var err error

	var sock string
	if gs.src.MountSSHSock != "" {
		var unmountSock func() error
		sock, unmountSock, err = gs.mountSSHAuthSock(ctx, gs.src.MountSSHSock, g)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		cleanups = append(cleanups, unmountSock)
	}

	var knownHosts string
	if gs.src.KnownSSHHosts != "" {
		var unmountKnownHosts func() error
		knownHosts, unmountKnownHosts, err = gs.mountKnownHosts()
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		cleanups = append(cleanups, unmountKnownHosts)
	}

	opts = append([]gitutil.Option{
		gitutil.WithGitAdvice(gs.src.Advice),
		gitutil.WithGitDebugCommands(gs.src.DebugCommands),
		gitutil.WithArgs(gs.authArgs...),
		gitutil.WithSSHAuthSock(sock),
		gitutil.WithSSHKnownHosts(knownHosts),
	}, opts...)
	return gitCLI(opts...), cleanup, err
}

func tokenScope(remote string) string {
	// generally we can only use the token for fetching main remote but in case of github.com we do best effort
	// to try reuse same token for all github.com remotes. This is the same behavior actions/checkout uses
	for _, pfx := range []string{"https://github.com/", "https://www.github.com/"} {
		if strings.HasPrefix(remote, pfx) {
			return pfx
		}
	}
	return remote
}

// getDefaultBranch gets the default branch of a repository using ls-remote
func getDefaultBranch(ctx context.Context, git *gitutil.GitCLI, remoteURL string) (string, error) {
	buf, err := git.Run(ctx, "ls-remote", "--symref", remoteURL, "HEAD")
	if err != nil {
		return "", errors.Wrapf(err, "error fetching default branch for repository %s", urlutil.RedactCredentials(remoteURL))
	}

	ss := defaultBranch.FindAllStringSubmatch(string(buf), -1)
	if len(ss) == 0 || len(ss[0]) != 2 {
		return "", errors.Errorf("could not find default branch for repository: %s", urlutil.RedactCredentials(remoteURL))
	}
	ref := ss[0][1]
	if err := validateGitRef(ref); err != nil {
		return "", err
	}
	return ref, nil
}

const (
	keyGitRemote     = "git-remote"
	gitRemoteIndex   = keyGitRemote + "::"
	keyGitSnapshot   = "git-snapshot"
	gitSnapshotIndex = keyGitSnapshot + "::"
)

func search(ctx context.Context, store cache.MetadataStore, key string, idx string) ([]cacheRefMetadata, error) {
	var results []cacheRefMetadata
	mds, err := store.Search(ctx, idx+key, false)
	if err != nil {
		return nil, err
	}
	for _, md := range mds {
		results = append(results, cacheRefMetadata{md})
	}
	return results, nil
}

func searchGitRemote(ctx context.Context, store cache.MetadataStore, remote string) ([]cacheRefMetadata, error) {
	return search(ctx, store, remote, gitRemoteIndex)
}

func searchGitSnapshot(ctx context.Context, store cache.MetadataStore, key string) ([]cacheRefMetadata, error) {
	return search(ctx, store, key, gitSnapshotIndex)
}

type cacheRefMetadata struct {
	cache.RefMetadata
}

func (md cacheRefMetadata) setGitSnapshot(key string) error {
	return md.SetString(keyGitSnapshot, key, gitSnapshotIndex+key)
}

func (md cacheRefMetadata) setGitRemote(key string) error {
	return md.SetString(keyGitRemote, key, gitRemoteIndex+key)
}

func (md cacheRefMetadata) clearGitRemote() error {
	return md.ClearValueAndIndex(keyGitRemote, gitRemoteIndex)
}

func gitCLI(opts ...gitutil.Option) *gitutil.GitCLI {
	opts = append([]gitutil.Option{
		gitutil.WithExec(runWithStandardUmask),
		gitutil.WithStreams(func(ctx context.Context) (stdout, stderr io.WriteCloser, flush func()) {
			return logs.NewLogStreams(ctx, false)
		}),
	}, opts...)
	return gitutil.NewGitCLI(opts...)
}

// validateDirsOnly checks that the given subpath in the repository
// only contains directories without any symlinks or files.
func validateDirsOnly(r *os.Root, subpath string) error {
	rel := rootRelativePath(subpath)
	if rel == "" || rel == "." {
		return nil
	}

	p := ""
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)

		fi, err := r.Lstat(p)
		if err != nil {
			return errors.Wrapf(err, "failed to lstat %q", p)
		}
		if !fi.IsDir() {
			return errors.Errorf("git subpath %q contains non-directory %q", subpath, p)
		}
	}
	return nil
}

func rootRelativePath(path string) string {
	return strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
}

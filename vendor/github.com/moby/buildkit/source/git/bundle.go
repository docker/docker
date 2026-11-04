package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/source/containerblob/blobfetch"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/gitutil"
	"github.com/moby/buildkit/util/urlutil"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	// bundleFileName is the name of the bundle file written at the
	// checkout mount root when git.checkoutbundle is set. Documented on
	// the GitCheckoutBundle godoc.
	bundleFileName = "bundle"

	// bundleImportFileName is the on-disk name of the transient bundle
	// file streamed in from the blob locator, colocated with the bare
	// repo and removed once the import completes.
	bundleImportFileName = "bundle.pack"

	// bundleFallbackRef is the ref name used when the user's ref is empty
	// or a commit SHA. Bundle creation needs a named ref so the consumer's
	// fetch refspec has something to land; consumer-side resolution
	// short-circuits on SHA when Ref is empty/SHA, so the fallback name
	// is not user-visible.
	bundleFallbackRef = "refs/heads/main"
)

// bundleTargetRef normalizes the user's ref into a fully-qualified ref name
// suitable as the tip ref inside the emitted bundle.
//
//   - Empty / commit SHA: falls back to bundleFallbackRef. The consumer
//     short-circuits on SHA so the chosen branch name is not user-visible,
//     but bundle creation needs a named ref for the fetch refspec to land.
//   - "refs/tags/<name>": preserved as a tag ref so a tag-addressed checkout
//     keeps the tag qualifier on the consumer side.
//   - Anything else (e.g. "master", "refs/heads/main", "v1"): stripped of a
//     leading refs/ or heads/ qualifier and re-prefixed with refs/heads/.
//
// Design note: refs/tags/<x> is preserved, but a bare "v1" is mapped to
// refs/heads/v1 rather than refs/tags/v1 because we cannot tell from a
// symbolic ref alone whether it originated as a tag or a branch; refs/heads/
// is the safer default for tip placement.
func bundleTargetRef(ref string) string {
	if ref == "" || gitutil.IsCommitSHA(ref) {
		return bundleFallbackRef
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return ref
	}
	name := strings.TrimPrefix(ref, "refs/")
	name = strings.TrimPrefix(name, "heads/")
	return "refs/heads/" + name
}

// detectBundleSHA256 probes a raw git bundle and returns whether it carries
// sha256 object IDs. git ls-remote reads bundle files directly, so this works
// before a destination repo exists and lets callers initialize the right object
// format for a subsequent fetch/import.
func detectBundleSHA256(ctx context.Context, bundlePath string, gitAdvice, debugCommands bool) (bool, error) {
	buf, err := gitCLI(gitutil.WithGitAdvice(gitAdvice), gitutil.WithGitDebugCommands(debugCommands)).Run(ctx, "ls-remote", "--", bundlePath)
	if err != nil {
		return false, errors.Wrapf(err, "failed to inspect git bundle %s", bundlePath)
	}
	for line := range strings.SplitSeq(string(buf), "\n") {
		if line == "" {
			continue
		}
		sha, _, _ := strings.Cut(line, "\t")
		if !gitutil.IsCommitSHA(sha) {
			return false, errors.Errorf("failed to inspect git bundle %s: invalid object ID %q", bundlePath, sha)
		}
		return len(sha) == 64, nil
	}
	return false, errors.Errorf("failed to inspect git bundle %s: no refs found", bundlePath)
}

// stageBundle downloads the bundle blob into an isolated temp bare repo,
// imports it, and validates that the pinned commit is present. The returned
// URL (file://<tmpRepoDir>) is suitable for use as a git fetch source (for
// the main fetch flow) or as the remote URL for an ls-remote call (for
// metadata resolution), so both paths converge on the same git primitives
// as a normal origin fetch.
//
// The returned cleanup removes the temp dir and must be called once the
// caller is done with the staged URL.
func (gs *gitSourceHandler) stageBundle(ctx context.Context, g session.Group) (_ string, _ func() error, retErr error) {
	scheme, ref, storeID, dgst, err := parseBundleLocator(gs.src.Bundle)
	if err != nil {
		return "", nil, err
	}

	tmpDir, err := os.MkdirTemp("", "buildkit-bundle-")
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to create temp dir for bundle")
	}
	cleanup := func() error { return os.RemoveAll(tmpDir) }
	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()

	if err := gs.downloadBundleToFile(ctx, g, scheme, ref, storeID, dgst, tmpDir, bundleImportFileName); err != nil {
		return "", nil, err
	}
	bundlePath := filepath.Join(tmpDir, bundleImportFileName)
	sha256, err := detectBundleSHA256(ctx, bundlePath, gs.src.Advice, gs.src.DebugCommands)
	if err != nil {
		return "", nil, err
	}
	gs.sha256 = sha256

	// Initialize an isolated bare repo in the temp dir and import the
	// bundle there. This lets us validate the bundle and stage the refs
	// under their natural names without touching the shared bare repo.
	tmpRepoDir := filepath.Join(tmpDir, "repo.git")
	if err := os.Mkdir(tmpRepoDir, 0700); err != nil {
		return "", nil, errors.Wrap(err, "failed to create temp bare repo dir")
	}
	tmpGit := gitCLI(gitutil.WithGitAdvice(gs.src.Advice), gitutil.WithGitDebugCommands(gs.src.DebugCommands), gitutil.WithGitDir(tmpRepoDir))
	initArgs := []string{"-c", "init.defaultBranch=master", "init", "--bare"}
	if sha256 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	if _, err := tmpGit.Run(ctx, initArgs...); err != nil {
		return "", nil, errors.Wrap(err, "failed to init temp bare repo")
	}

	if _, err := tmpGit.Run(ctx, "fetch", bundlePath, "+refs/*:refs/*"); err != nil {
		return "", nil, errors.Wrapf(err, "failed to import git bundle %s into temp", ref)
	}

	// Confirm the pinned commit is actually present in the bundle. The
	// subsequent fetch in the main flow would surface a generic "failed to
	// fetch" error; this check gives a clearer message.
	if _, err := tmpGit.Run(ctx, "cat-file", "-e", gs.src.Checksum+"^{commit}"); err != nil {
		return "", nil, errors.Errorf("commit %s not found in bundle %s", gs.src.Checksum, ref)
	}

	return "file://" + tmpRepoDir, cleanup, nil
}

// ensureStagedBundle returns the file:// URL of a staged temp bare repo
// produced from gs.src.Bundle, lazily staging it on first call and reusing
// the same staging dir on subsequent calls for the lifetime of gs.
//
// The call-site contract is: callers may call ensureStagedBundle any number
// of times and must never run the cleanup themselves. Cleanup is attached to
// the job via JobContext.Cleanup on the first staging so it fires at
// end-of-solve regardless of whether Snapshot runs (covering the
// CacheKey-then-cache-hit path where tryRemoteFetch never executes). If no
// jobCtx is available (e.g. ResolveMetadata path with a nil jobCtx), the
// returned cleanup is retained on the handler and teardown must come from a
// later call that does provide a jobCtx, or from the caller running
// releaseStagedBundle explicitly.
//
// Handler methods run sequentially per solve, so ensureStagedBundle does not
// guard against concurrent calls.
func (gs *gitSourceHandler) ensureStagedBundle(ctx context.Context, jobCtx solver.JobContext, g session.Group) (string, error) {
	if gs.stagedBundleURL != "" {
		return gs.stagedBundleURL, nil
	}

	url, cleanup, err := gs.stageBundle(ctx, g)
	if err != nil {
		return "", err
	}

	// Make the cleanup idempotent so it is safe to call from both the
	// job cleanup path and an explicit releaseStagedBundle (or the
	// CacheKey fallback path below) without double-removing the temp dir.
	once := sync.OnceValue(cleanup)
	gs.stagedBundleURL = url
	gs.stagedBundleCleanup = once

	if jobCtx != nil {
		if err := jobCtx.Cleanup(func() error { return once() }); err != nil {
			// Failed to register the job cleanup — run it synchronously
			// so we do not leak the temp dir.
			_ = once()
			gs.stagedBundleURL = ""
			gs.stagedBundleCleanup = nil
			return "", err
		}
		// The job cleanup now owns the teardown; clearing the
		// per-handler pointer avoids a second invocation from any
		// fallback path. The stored url stays set so repeat callers on
		// this handler get the cached URL for the remainder of the
		// solve, before the job cleanup fires.
		gs.stagedBundleCleanup = nil
	} else {
		bklog.G(ctx).Debug("bundle staged without jobCtx; cleanup deferred to handler teardown")
	}

	return url, nil
}

// releaseStagedBundle tears down any staged bundle owned by the handler. It
// is a no-op when the cleanup has already been attached to a JobContext or
// when nothing was staged. Safe to call multiple times.
func (gs *gitSourceHandler) releaseStagedBundle() error {
	cleanup := gs.stagedBundleCleanup
	gs.stagedBundleCleanup = nil
	gs.stagedBundleURL = ""
	if cleanup == nil {
		return nil
	}
	return cleanup()
}

// downloadBundleToFile streams the bundle blob into a file named bundleName
// inside bundleDir while verifying its digest against the locator. The file
// is created via os.Root so a pre-existing symlink at bundleName inside
// bundleDir cannot redirect the write outside of bundleDir.
func (gs *gitSourceHandler) downloadBundleToFile(ctx context.Context, g session.Group, scheme, ref, storeID string, expectedDigest digest.Digest, bundleDir, bundleName string) (retErr error) {
	rc, err := gs.openBundleBlob(ctx, g, scheme, ref, storeID, expectedDigest)
	if err != nil {
		return errors.Wrapf(err, "failed to fetch git bundle %s", ref)
	}
	defer rc.Close()

	bundleDirRoot, err := os.OpenRoot(bundleDir)
	if err != nil {
		return errors.Wrapf(err, "failed to open bundle dir root %s", bundleDir)
	}
	defer bundleDirRoot.Close()

	f, err := bundleDirRoot.Create(bundleName)
	if err != nil {
		return errors.Wrapf(err, "failed to create bundle file %s", bundleName)
	}
	defer func() {
		if retErr != nil {
			f.Close()
			bundleDirRoot.Remove(bundleName)
		}
	}()

	d := digest.SHA256.Digester()
	if _, err := io.Copy(io.MultiWriter(f, d.Hash()), rc); err != nil {
		return errors.Wrapf(err, "failed to download git bundle %s", ref)
	}
	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "failed to close bundle file %s", bundleName)
	}

	got := d.Digest()
	if expectedDigest != "" && got != expectedDigest {
		return errors.Errorf("expected checksum to match %s, got %s", expectedDigest, got)
	}
	return nil
}

// openBundleBlob resolves the blob using blobfetch. storeID is the pre-derived
// OCI-layout store id (empty for registry blobs).
func (gs *gitSourceHandler) openBundleBlob(ctx context.Context, g session.Group, scheme, ref, storeID string, dgst digest.Digest) (io.ReadCloser, error) {
	opt := blobfetch.FetchOpt{
		Scheme:         scheme,
		Ref:            ref,
		Digest:         dgst,
		RegistryHosts:  gs.registryHosts,
		SessionManager: gs.sm,
	}
	if scheme == srctypes.OCIBlobScheme {
		opt.SessionID = gs.src.BundleOCISessionID
		opt.StoreID = storeID
		if gs.src.BundleOCIStoreID != "" {
			opt.StoreID = gs.src.BundleOCIStoreID
		}
	}
	rc, _, err := blobfetch.FetchBlob(ctx, g, opt)
	return rc, err
}

// resolveBundleMetadata stages the bundle in an isolated temp bare repo and
// delegates to the shared ls-remote code path using that repo's file:// URL.
// Bundle mode lands refs in refs/heads/* and refs/tags/* just like a normal
// origin fetch, so the resolution logic is identical once the URL swap is
// done — and the resulting Metadata.Ref has the same shape as non-bundle
// mode (e.g. "refs/heads/master" for symbolic ref "master").
//
// For a pinned-commit / empty user ref, short-circuit: the user has already
// told us the commit, so there is nothing for ls-remote to add and no point
// downloading the bundle just for metadata resolution. The bundle will still
// be staged during the Snapshot path if the commit is not already present.
func (gs *gitSourceHandler) resolveBundleMetadata(ctx context.Context, jobCtx solver.JobContext) (*Metadata, error) {
	// Bundle mode requires git.checksum; if Ref is empty or a SHA, the
	// commit is already pinned and no lookup is needed. Report Ref and
	// Checksum as the commit SHA — same shape as the commit-SHA short
	// circuit in non-bundle resolveMetadata.
	if gs.src.Ref == "" || gitutil.IsCommitSHA(gs.src.Ref) {
		ref := gs.src.Ref
		if ref == "" {
			ref = gs.src.Checksum
		}
		if gs.src.Checksum != "" && !strings.HasPrefix(ref, gs.src.Checksum) {
			return nil, errors.Errorf("expected checksum to match %s, got %s", gs.src.Checksum, ref)
		}
		return &Metadata{Ref: ref, Checksum: ref}, nil
	}

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}
	stagedURL, err := gs.ensureStagedBundle(ctx, jobCtx, g)
	if err != nil {
		return nil, err
	}
	return gs.resolveMetadataFromURL(ctx, g, stagedURL)
}

// checkoutAsBundle writes a single-file git bundle at the checkout mount root
// instead of a worktree. This is the checkout counterpart of bundle import and
// is used when GitCheckoutBundle() is set on the identifier. Submodules are
// not included in the bundle.
func (gs *gitSourceHandler) checkoutAsBundle(ctx context.Context, repo *gitRepo, g session.Group) (_ cache.ImmutableRef, retErr error) {
	ref := gs.src.Ref
	commit := gs.src.Checksum
	if commit == "" {
		commit = ref
	}

	checkoutRef, err := gs.cache.New(ctx, nil, g, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("git bundle checkout for %s#%s", urlutil.RedactCredentials(gs.src.Remote), ref)))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create new mutable for bundle checkout")
	}
	defer func() {
		if retErr != nil && checkoutRef != nil {
			checkoutRef.Release(context.WithoutCancel(ctx))
		}
	}()

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

	// The bundle needs to carry a named ref that points at the target
	// commit, so `git fetch <bundle> +refs/*:refs/*` on the consumer side
	// has something to land. We preserve the user's ref name (normalized
	// into refs/heads/* or refs/tags/*) so the bundle reads as if it were
	// produced by the upstream remote; no fake internal namespace.
	targetRef := bundleTargetRef(ref)

	// Stage the bundle in an isolated temp bare repo rather than mutating
	// the shared bare repo's ref namespace. We fetch the pinned commit
	// from the shared repo into the temp bare repo and give it the
	// target-ref name there, so `git bundle create` sees a clean,
	// self-contained repo with exactly the ref we want in the bundle.
	tmpDir, err := os.MkdirTemp("", "buildkit-bundle-create-")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp dir for bundle creation")
	}
	defer os.RemoveAll(tmpDir)

	tmpRepoDir := filepath.Join(tmpDir, "repo.git")
	if err := os.Mkdir(tmpRepoDir, 0700); err != nil {
		return nil, errors.Wrap(err, "failed to create temp bare repo dir for bundle creation")
	}
	tmpGit := repo.New(gitutil.WithGitDir(tmpRepoDir))
	initArgs := []string{"-c", "init.defaultBranch=master", "init", "--bare"}
	if gs.sha256 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	if _, err := tmpGit.Run(ctx, initArgs...); err != nil {
		return nil, errors.Wrap(err, "failed to init temp bare repo for bundle creation")
	}

	// Pull the pinned commit from the shared bare repo into temp, then
	// point the target ref at it. The shared bare repo maps user refs to
	// refs/tags/<name> via the main-path fetch refspec, so there is no
	// natural-name ref to pull; update-ref alone places the tip against
	// the pinned commit under the target name.
	sharedURL := "file://" + repo.dir
	if err := fetchCommitForBundle(ctx, tmpGit, sharedURL, commit); err != nil {
		return nil, err
	}

	if _, err := tmpGit.Run(ctx, "update-ref", targetRef, commit); err != nil {
		return nil, errors.Wrapf(err, "failed to create target ref for bundle %s", commit)
	}

	// Write the bundle under a temp path we fully own, then move it into
	// the checkout mount through os.OpenRoot so a pre-existing symlink at
	// <checkoutDir>/bundle cannot redirect the final artifact outside the
	// mount. git bundle create is an external process that only accepts
	// path strings, so the only safe landing pattern is: write to a path
	// outside the mount, then atomically transfer the bytes through an
	// os.Root-scoped file handle.
	stagePath := filepath.Join(tmpDir, "out.bundle")
	if _, err := tmpGit.Run(ctx, "bundle", "create", stagePath, targetRef); err != nil {
		return nil, errors.Wrapf(err, "failed to create git bundle for %s", commit)
	}

	if err := writeBundleToMount(checkoutDir, bundleFileName, stagePath); err != nil {
		return nil, err
	}

	outPath := filepath.Join(checkoutDir, bundleFileName)
	if idmap := mount.IdentityMapping(); idmap != nil {
		uid, gid := idmap.RootPair()
		if err := os.Lchown(outPath, uid, gid); err != nil {
			return nil, errors.Wrap(err, "failed to remap git bundle ownership")
		}
	}

	lm.Unmount()
	lm = nil

	snap, err := checkoutRef.Commit(ctx)
	if err != nil {
		return nil, err
	}
	checkoutRef = nil
	return snap, nil
}

func fetchCommitForBundle(ctx context.Context, git *gitutil.GitCLI, sharedURL, commit string) error {
	if _, err := git.Run(ctx, "fetch", sharedURL, "--", commit); err != nil {
		return errors.Wrapf(err, "failed to fetch commit %s from shared repo for bundle creation", commit)
	}
	return nil
}

// writeBundleToMount copies the bytes at stagePath into
// <checkoutDir>/<bundleName> via os.OpenRoot, so a symlink at the destination
// cannot redirect the write outside checkoutDir. The staging file is expected
// to be a path the caller fully controls (created under its own temp dir),
// not a path inside checkoutDir.
func writeBundleToMount(checkoutDir, bundleName, stagePath string) (retErr error) {
	src, err := os.Open(stagePath)
	if err != nil {
		return errors.Wrapf(err, "failed to open staged bundle %s", stagePath)
	}
	defer src.Close()

	checkoutRoot, err := os.OpenRoot(checkoutDir)
	if err != nil {
		return errors.Wrapf(err, "failed to open checkout dir root %s", checkoutDir)
	}
	defer checkoutRoot.Close()

	// Remove any pre-existing entry at bundleName. If it's a symlink,
	// os.Root.Remove deletes the link itself, not the target, so a symlink
	// planted in the mount cannot redirect the subsequent create.
	if err := checkoutRoot.Remove(bundleName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Wrapf(err, "failed to clear bundle dest %s", bundleName)
	}

	dst, err := checkoutRoot.Create(bundleName)
	if err != nil {
		return errors.Wrapf(err, "failed to create bundle file %s", bundleName)
	}
	defer func() {
		if retErr != nil {
			dst.Close()
			checkoutRoot.Remove(bundleName)
		}
	}()

	if _, err := io.Copy(dst, src); err != nil {
		return errors.Wrapf(err, "failed to write bundle to %s", bundleName)
	}
	if err := dst.Close(); err != nil {
		return errors.Wrapf(err, "failed to close bundle file %s", bundleName)
	}
	return nil
}

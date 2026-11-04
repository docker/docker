package llb

import (
	"context"
	_ "crypto/sha256" // for opencontainers/go-digest
	"encoding/json"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/gitutil"
	"github.com/moby/buildkit/util/sshutil"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type SourceOp struct {
	cache       MarshalCache
	id          string
	attrs       map[string]string
	output      Output
	constraints Constraints
	err         error
}

func NewSource(id string, attrs map[string]string, c Constraints) *SourceOp {
	s := &SourceOp{
		id:          id,
		attrs:       attrs,
		constraints: c,
	}
	s.output = &output{vertex: s, platform: c.Platform}
	return s
}

func (s *SourceOp) Validate(ctx context.Context, c *Constraints) error {
	if s.err != nil {
		return s.err
	}
	if s.id == "" {
		return errors.New("source identifier can't be empty")
	}
	return nil
}

func (s *SourceOp) Marshal(ctx context.Context, constraints *Constraints) (digest.Digest, []byte, *pb.OpMetadata, []*SourceLocation, error) {
	cache := s.cache.Acquire()
	defer cache.Release()

	if dgst, dt, md, srcs, err := cache.Load(constraints); err == nil {
		return dgst, dt, md, srcs, nil
	}

	if err := s.Validate(ctx, constraints); err != nil {
		return "", nil, nil, nil, err
	}

	if strings.HasPrefix(s.id, "local://") {
		if _, hasSession := s.attrs[pb.AttrLocalSessionID]; !hasSession {
			uid := s.constraints.LocalUniqueID
			if uid == "" {
				uid = constraints.LocalUniqueID
			}
			s.attrs[pb.AttrLocalUniqueID] = uid
			addCap(&s.constraints, pb.CapSourceLocalUnique)
		}
	}
	proto, md := MarshalConstraints(constraints, &s.constraints)

	proto.Op = &pb.Op_Source{
		Source: &pb.SourceOp{Identifier: s.id, Attrs: s.attrs},
	}

	if !platformSpecificSource(s.id) {
		proto.Platform = nil
	}

	dt, err := deterministicMarshal(proto)
	if err != nil {
		return "", nil, nil, nil, err
	}

	return cache.Store(dt, md, s.constraints.SourceLocations, constraints)
}

func (s *SourceOp) Output() Output {
	return s.output
}

func (s *SourceOp) Inputs() []Output {
	return nil
}

type ImageBlobInfo struct {
	constraintsWrapper
	fileinfoWrapper
	sessionID string
	storeID   string
}

type ImageBlobOption interface {
	SetImageBlobOption(*ImageBlobInfo)
}

type FileInfoOption interface {
	HTTPOption
	ImageBlobOption
}

func ImageBlob(ref string, opts ...ImageBlobOption) State {
	bi := &ImageBlobInfo{}
	for _, o := range opts {
		o.SetImageBlobOption(bi)
	}
	attrs := map[string]string{}

	if bi.Filename != "" {
		attrs[pb.AttrHTTPFilename] = bi.Filename
	}
	if bi.Perm != 0 {
		attrs[pb.AttrHTTPPerm] = "0" + strconv.FormatInt(int64(bi.Perm), 8)
	}
	if bi.UID != 0 {
		attrs[pb.AttrHTTPUID] = strconv.Itoa(bi.UID)
	}
	if bi.GID != 0 {
		attrs[pb.AttrHTTPGID] = strconv.Itoa(bi.GID)
	}

	addCap(&bi.Constraints, pb.CapSourceImageBlob)

	var digested reference.Digested

	r, err := reference.ParseNormalizedNamed(ref)
	if err == nil {
		if _, tagged := r.(reference.Tagged); tagged {
			err = errors.New("tagged image reference not allowed for blob reference")
		} else if ref, ok := r.(reference.Digested); !ok {
			err = errors.New("checksum required in blob reference")
		} else {
			digested = ref
		}
	}

	repoName := "invalid"
	if digested != nil {
		repoName = digested.String()
	}

	source := NewSource("docker-image+blob://"+repoName, attrs, bi.Constraints)
	if err != nil {
		source.err = err
	}
	return NewState(source.Output())
}

// OCILayoutBlob returns a state that represents a single digest-addressed blob from an OCI layout store.
func OCILayoutBlob(ref string, opts ...ImageBlobOption) State {
	bi := &ImageBlobInfo{}
	for _, o := range opts {
		o.SetImageBlobOption(bi)
	}
	attrs := map[string]string{}

	if bi.Filename != "" {
		attrs[pb.AttrHTTPFilename] = bi.Filename
	}
	if bi.Perm != 0 {
		attrs[pb.AttrHTTPPerm] = "0" + strconv.FormatInt(int64(bi.Perm), 8)
	}
	if bi.UID != 0 {
		attrs[pb.AttrHTTPUID] = strconv.Itoa(bi.UID)
	}
	if bi.GID != 0 {
		attrs[pb.AttrHTTPGID] = strconv.Itoa(bi.GID)
	}
	if bi.sessionID != "" {
		attrs[pb.AttrOCILayoutSessionID] = bi.sessionID
	}
	if bi.storeID != "" {
		attrs[pb.AttrOCILayoutStoreID] = bi.storeID
	}

	addCap(&bi.Constraints, pb.CapSourceImageBlob)

	var digested reference.Digested

	r, err := reference.ParseNormalizedNamed(ref)
	if err == nil {
		if _, tagged := r.(reference.Tagged); tagged {
			err = errors.New("tagged image reference not allowed for blob reference")
		} else if ref, ok := r.(reference.Digested); !ok {
			err = errors.New("checksum required in blob reference")
		} else {
			digested = ref
		}
	}

	repoName := "invalid"
	if digested != nil {
		repoName = digested.String()
	}

	source := NewSource("oci-layout+blob://"+repoName, attrs, bi.Constraints)
	if err != nil {
		source.err = err
	}
	return NewState(source.Output())
}

// Image returns a state that represents a docker image in a registry.
// Example:
//
//	st := llb.Image("busybox:latest")
func Image(ref string, opts ...ImageOption) State {
	r, err := reference.ParseNormalizedNamed(ref)
	if err == nil {
		r = reference.TagNameOnly(r)
		ref = r.String()
	}
	var info ImageInfo
	for _, opt := range opts {
		opt.SetImageOption(&info)
	}

	addCap(&info.Constraints, pb.CapSourceImage)

	attrs := map[string]string{}
	if info.resolveMode != 0 {
		attrs[pb.AttrImageResolveMode] = info.resolveMode.String()
		if info.resolveMode == ResolveModeForcePull {
			addCap(&info.Constraints, pb.CapSourceImageResolveMode) // only require cap for security enforced mode
		}
	}

	if info.RecordType != "" {
		attrs[pb.AttrImageRecordType] = info.RecordType
	}

	if ll := info.layerLimit; ll != nil {
		attrs[pb.AttrImageLayerLimit] = strconv.FormatInt(int64(*ll), 10)
		addCap(&info.Constraints, pb.CapSourceImageLayerLimit)
	}

	if info.checksum != "" {
		attrs[pb.AttrImageChecksum] = info.checksum.String()
		addCap(&info.Constraints, pb.CapSourceImageChecksum)
	}

	src := NewSource("docker-image://"+ref, attrs, info.Constraints) // controversial
	if err != nil {
		src.err = err
	} else if info.metaResolver != nil {
		if _, ok := r.(reference.Digested); ok || !info.resolveDigest {
			return NewState(src.Output()).Async(func(ctx context.Context, st State, c *Constraints) (State, error) {
				p := info.Platform
				if p == nil {
					p = c.Platform
				}
				_, _, dt, err := info.metaResolver.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
					ImageOpt: &sourceresolver.ResolveImageOpt{
						Platform:    p,
						ResolveMode: info.resolveMode.String(),
					},
				})
				if err != nil {
					return State{}, err
				}
				return st.WithImageConfig(dt)
			})
		}
		return Scratch().Async(func(ctx context.Context, _ State, c *Constraints) (State, error) {
			p := info.Platform
			if p == nil {
				p = c.Platform
			}
			ref, dgst, dt, err := info.metaResolver.ResolveImageConfig(context.TODO(), ref, sourceresolver.Opt{
				ImageOpt: &sourceresolver.ResolveImageOpt{
					Platform:    p,
					ResolveMode: info.resolveMode.String(),
				},
			})
			if err != nil {
				return State{}, err
			}
			r, err := reference.ParseNormalizedNamed(ref)
			if err != nil {
				return State{}, err
			}
			if dgst != "" {
				r, err = reference.WithDigest(r, dgst)
				if err != nil {
					return State{}, err
				}
			}
			return NewState(NewSource("docker-image://"+r.String(), attrs, info.Constraints).Output()).WithImageConfig(dt)
		})
	}
	return NewState(src.Output())
}

type ImageOption interface {
	SetImageOption(*ImageInfo)
}

type imageOptionFunc func(*ImageInfo)

func (fn imageOptionFunc) SetImageOption(ii *ImageInfo) {
	fn(ii)
}

type imageBlobOptionFunc func(*ImageBlobInfo)

func (fn imageBlobOptionFunc) SetImageBlobOption(ib *ImageBlobInfo) {
	fn(ib)
}

var MarkImageInternal = imageOptionFunc(func(ii *ImageInfo) {
	ii.RecordType = "internal"
})

type ResolveMode int

const (
	ResolveModeDefault ResolveMode = iota
	ResolveModeForcePull
	ResolveModePreferLocal
)

func (r ResolveMode) SetImageOption(ii *ImageInfo) {
	ii.resolveMode = r
}

func (r ResolveMode) String() string {
	switch r {
	case ResolveModeDefault:
		return pb.AttrImageResolveModeDefault
	case ResolveModeForcePull:
		return pb.AttrImageResolveModeForcePull
	case ResolveModePreferLocal:
		return pb.AttrImageResolveModePreferLocal
	default:
		return ""
	}
}

type ImageInfo struct {
	constraintsWrapper
	metaResolver  ImageMetaResolver
	resolveDigest bool
	resolveMode   ResolveMode
	layerLimit    *int
	checksum      digest.Digest
	RecordType    string
}

const (
	GitAuthHeaderKey = "GIT_AUTH_HEADER"
	GitAuthTokenKey  = "GIT_AUTH_TOKEN"
)

// Git returns a state that represents a git repository.
// Example:
//
//	st := llb.Git("https://github.com/moby/buildkit.git", "v0.11.6")
//
// The example fetches the v0.11.6 tag of the buildkit repository.
// You can also use a commit hash or a branch name.
//
// Other URL formats are supported such as "git@github.com:moby/buildkit.git", "git://...", "ssh://..."
// Formats that utilize SSH may need to supply credentials as a [GitOption].
// You may need to check the source code for a full list of supported formats.
//
// Fragment can be used to pass ref:subdir format that can set in (old-style)
// Docker Git URL format after # . This is provided for backwards compatibility.
// It is recommended to leave it empty and call GitRef(), GitSubdir() options instead.
//
// By default the git repository is cloned with `--depth=1` to reduce the amount of data downloaded.
// Additionally the ".git" directory is removed after the clone, you can keep ith with the [KeepGitDir] [GitOption].
func Git(url, fragment string, opts ...GitOption) State {
	remote, err := gitutil.ParseURL(url)
	if errors.Is(err, gitutil.ErrUnknownProtocol) {
		url = "https://" + url
		remote, err = gitutil.ParseURL(url)
	}
	if remote != nil {
		url = remote.Remote
	}

	gi := &GitInfo{
		AuthHeaderSecret: GitAuthHeaderKey,
		AuthTokenSecret:  GitAuthTokenKey,
	}
	ref, subdir, ok := strings.Cut(fragment, ":")
	subdir = path.Join("/", subdir)
	subdir = strings.TrimPrefix(subdir, "/")
	if ref != "" {
		GitRef(ref).SetGitOption(gi)
	}
	if ok && subdir != "" {
		GitSubDir(subdir).SetGitOption(gi)
	}
	for _, o := range opts {
		o.SetGitOption(gi)
	}
	var id string
	if err != nil {
		// If we can't parse the URL, just use the full URL as the ID. The git
		// operation will fail later on.
		id = url
	} else {
		// We construct the ID manually here, so that we can create the same ID
		// for different protocols (e.g. https and ssh) that have the same
		// host/path/fragment combination.
		id = remote.Host + path.Join("/", remote.Path)
		if gi.Ref != "" || gi.SubDir != "" {
			id += "#" + gi.Ref
			if gi.SubDir != "" {
				id += ":" + gi.SubDir
			}
		}
	}
	attrs := map[string]string{}
	if gi.KeepGitDir {
		attrs[pb.AttrKeepGitDir] = "true"
		addCap(&gi.Constraints, pb.CapSourceGitKeepDir)
	}
	if url != "" {
		attrs[pb.AttrFullRemoteURL] = url
		addCap(&gi.Constraints, pb.CapSourceGitFullURL)
	}
	if gi.AuthTokenSecret != "" {
		attrs[pb.AttrAuthTokenSecret] = gi.AuthTokenSecret
		if gi.addAuthCap {
			addCap(&gi.Constraints, pb.CapSourceGitHTTPAuth)
		}
	}
	if gi.AuthHeaderSecret != "" {
		attrs[pb.AttrAuthHeaderSecret] = gi.AuthHeaderSecret
		if gi.addAuthCap {
			addCap(&gi.Constraints, pb.CapSourceGitHTTPAuth)
		}
	}
	if remote != nil && remote.Scheme == gitutil.SSHProtocol {
		if gi.KnownSSHHosts != "" {
			attrs[pb.AttrKnownSSHHosts] = gi.KnownSSHHosts
		} else {
			keyscan, err := sshutil.SSHKeyScan(remote.Host)
			if err == nil {
				// best effort
				attrs[pb.AttrKnownSSHHosts] = keyscan
			}
		}
		addCap(&gi.Constraints, pb.CapSourceGitKnownSSHHosts)

		if gi.MountSSHSock == "" {
			attrs[pb.AttrMountSSHSock] = "default"
		} else {
			attrs[pb.AttrMountSSHSock] = gi.MountSSHSock
		}
		addCap(&gi.Constraints, pb.CapSourceGitMountSSHSock)
	}

	checksum := gi.Checksum
	if checksum != "" {
		attrs[pb.AttrGitChecksum] = checksum
		addCap(&gi.Constraints, pb.CapSourceGitChecksum)
	}

	if gi.SkipSubmodules {
		attrs[pb.AttrGitSkipSubmodules] = "true"
		addCap(&gi.Constraints, pb.CapSourceGitSkipSubmodules)
	}
	if gi.DebugCommands {
		attrs[pb.AttrGitDebugCommands] = "true"
	}

	if gi.MTime != "" {
		attrs[pb.AttrGitMTime] = gi.MTime
		addCap(&gi.Constraints, pb.CapSourceGitMTime)
	}

	if gi.Advice {
		attrs[pb.AttrGitAdvice] = "true"
	}

	if gi.FetchByCommit {
		attrs[pb.AttrGitFetchByCommit] = "true"
		addCap(&gi.Constraints, pb.CapSourceGitFetchByCommit)
	}
	if gi.Bundle != "" {
		attrs[pb.AttrGitBundle] = gi.Bundle
		addCap(&gi.Constraints, pb.CapSourceGitBundle)
	}
	if gi.BundleOCISessionID != "" {
		attrs[pb.AttrOCILayoutSessionID] = gi.BundleOCISessionID
	}
	if gi.BundleOCIStoreID != "" {
		attrs[pb.AttrOCILayoutStoreID] = gi.BundleOCIStoreID
	}

	if gi.CheckoutBundle {
		attrs[pb.AttrGitCheckoutBundle] = "true"
		addCap(&gi.Constraints, pb.CapSourceGitCheckoutBundle)
	}

	addCap(&gi.Constraints, pb.CapSourceGit)

	source := NewSource("git://"+id, attrs, gi.Constraints)
	return NewState(source.Output())
}

type GitOption interface {
	SetGitOption(*GitInfo)
}
type gitOptionFunc func(*GitInfo)

func (fn gitOptionFunc) SetGitOption(gi *GitInfo) {
	fn(gi)
}

type GitInfo struct {
	constraintsWrapper
	KeepGitDir         bool
	AuthTokenSecret    string
	AuthHeaderSecret   string
	addAuthCap         bool
	KnownSSHHosts      string
	MountSSHSock       string
	Checksum           string
	Ref                string
	SubDir             string
	SkipSubmodules     bool
	MTime              string
	Advice             bool
	DebugCommands      bool
	Bundle             string
	BundleOCISessionID string
	BundleOCIStoreID   string
	CheckoutBundle     bool
	FetchByCommit      bool
}

func GitRef(v string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.Ref = v
	})
}

func GitSubDir(v string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.SubDir = v
	})
}

func GitSkipSubmodules() GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.SkipSubmodules = true
	})
}

// GitMTimeCommit sets file modification times to the commit timestamp
// of the resolved commit, rather than the checkout time.
func GitMTimeCommit() GitOption {
	return GitMTime("commit")
}

// GitMTime sets the file modification time policy for git sources.
// Valid values are "checkout" (default) and "commit".
func GitMTime(v string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.MTime = v
	})
}

// GitAdvice controls whether Git advice messages are emitted while resolving
// this git source.
func GitAdvice(enabled bool) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.Advice = enabled
	})
}

// GitDebugCommands controls whether executed Git commands are logged while
// resolving this git source.
func GitDebugCommands(enabled bool) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.DebugCommands = enabled
	})
}

func KeepGitDir() GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.KeepGitDir = true
	})
}

func AuthTokenSecret(v string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.AuthTokenSecret = v
		gi.addAuthCap = true
	})
}

func KnownSSHHosts(key string) GitOption {
	key = strings.TrimSuffix(key, "\n")
	return gitOptionFunc(func(gi *GitInfo) {
		gi.KnownSSHHosts = gi.KnownSSHHosts + key + "\n"
	})
}

func MountSSHSock(sshID string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.MountSSHSock = sshID
	})
}

func GitChecksum(v string) GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.Checksum = v
	})
}

// GitFetchByCommit makes the git source trust the provided checksum as the
// commit to fetch, without resolving the ref against the remote. The ref, if
// set, is applied locally after the commit is fetched so that cache keys
// still depend on it. This is useful when the remote ref may have moved
// since the original resolution.
//
// Unqualified ref names are canonicalized to "refs/heads/<name>" to match
// the normal path's cache keys for branches. Tags must be passed fully
// qualified ("refs/tags/<name>").
func GitFetchByCommit() GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.FetchByCommit = true
	})
}

// GitBundleInfo carries scheme-specific configuration for [GitBundleURL].
// It is populated by [GitBundleOption] values and consumed by GitBundleURL.
type GitBundleInfo struct {
	// OCISessionID pins the OCI-layout bundle fetch to a specific client
	// session. Only meaningful when the locator uses the
	// "oci-layout+blob://" scheme.
	OCISessionID string
	// OCIStoreID overrides the OCI-layout store name derived from the
	// bundle locator body. Only meaningful when the locator uses the
	// "oci-layout+blob://" scheme.
	OCIStoreID string
}

// GitBundleOption configures bundle-specific behavior for [GitBundleURL].
type GitBundleOption interface {
	SetGitBundleOption(*GitBundleInfo)
}

type gitBundleOptionFunc func(*GitBundleInfo)

func (fn gitBundleOptionFunc) SetGitBundleOption(bi *GitBundleInfo) {
	fn(bi)
}

// GitBundleURL configures the git source to import the commit history from a
// pre-built git bundle instead of fetching from the upstream Git remote. The
// locator must use one of the following schemes:
//
//	docker-image+blob://<registry-ref>@sha256:<digest>
//	oci-layout+blob://<ref>@sha256:<digest>
//
// [GitChecksum] is required when GitBundleURL is used and the commit it
// identifies must be reachable from a ref inside the bundle (i.e. the bundle
// must contain that commit; BuildKit validates this after import). Auth and
// SSH options are ignored in bundle mode. Submodules, if present, still fetch
// from their own remotes. See [GitCheckoutBundle] for the checkout side; it
// is mutually exclusive with [KeepGitDir] and [GitSubDir].
//
// Scheme-specific behavior can be tuned via [GitBundleOption] values, e.g.
// [GitBundleOCIStore] for the "oci-layout+blob://" scheme.
func GitBundleURL(locator string, opts ...GitBundleOption) GitOption {
	bi := &GitBundleInfo{}
	for _, o := range opts {
		o.SetGitBundleOption(bi)
	}
	return gitOptionFunc(func(gi *GitInfo) {
		gi.Bundle = locator
		gi.BundleOCISessionID = bi.OCISessionID
		gi.BundleOCIStoreID = bi.OCIStoreID
	})
}

// GitBundleOCIStore configures the OCI-layout session and store id used to
// resolve a bundle locator with the "oci-layout+blob://" scheme. When unset,
// the locator body (the part before "@digest") is used as the store id and no
// session is pinned. This mirrors [ImageBlobOCIStore] for regular OCI-layout
// blobs.
//
// Only meaningful when passed to [GitBundleURL] together with a locator that
// uses the "oci-layout+blob://" scheme.
func GitBundleOCIStore(sessionID, storeID string) GitBundleOption {
	return gitBundleOptionFunc(func(bi *GitBundleInfo) {
		bi.OCISessionID = sessionID
		bi.OCIStoreID = storeID
	})
}

// GitCheckoutBundle produces a single-file git bundle at the checkout mount
// root (filename "bundle") containing the resolved commit, instead of a
// worktree. Mutually exclusive with [KeepGitDir] and [GitSubDir]. Submodules
// are not included in the bundle. Pair with [GitBundleURL] to re-import the
// bundle back into a later build.
func GitCheckoutBundle() GitOption {
	return gitOptionFunc(func(gi *GitInfo) {
		gi.CheckoutBundle = true
	})
}

// AuthOption can be used with either HTTP or Git sources.
type AuthOption interface {
	GitOption
	HTTPOption
}

// AuthHeaderSecret returns an AuthOption that defines the name of a
// secret to use for HTTP based authentication.
func AuthHeaderSecret(secretName string) AuthOption {
	return struct {
		GitOption
		HTTPOption
	}{
		GitOption: gitOptionFunc(func(gi *GitInfo) {
			gi.AuthHeaderSecret = secretName
			gi.addAuthCap = true
		}),
		HTTPOption: httpOptionFunc(func(hi *HTTPInfo) {
			hi.AuthHeaderSecret = secretName
		}),
	}
}

// Scratch returns a state that represents an empty filesystem.
func Scratch() State {
	return NewState(nil)
}

// Local returns a state that represents a directory local to the client.
func Local(name string, opts ...LocalOption) State {
	gi := &LocalInfo{}

	for _, o := range opts {
		o.SetLocalOption(gi)
	}
	attrs := map[string]string{}
	if gi.SessionID != "" {
		attrs[pb.AttrLocalSessionID] = gi.SessionID
		addCap(&gi.Constraints, pb.CapSourceLocalSessionID)
	}
	if gi.IncludePatterns != "" {
		attrs[pb.AttrIncludePatterns] = gi.IncludePatterns
		addCap(&gi.Constraints, pb.CapSourceLocalIncludePatterns)
	}
	if gi.FollowPaths != "" {
		attrs[pb.AttrFollowPaths] = gi.FollowPaths
		addCap(&gi.Constraints, pb.CapSourceLocalFollowPaths)
	}
	if gi.ExcludePatterns != "" {
		attrs[pb.AttrExcludePatterns] = gi.ExcludePatterns
		addCap(&gi.Constraints, pb.CapSourceLocalExcludePatterns)
	}
	if gi.SharedKeyHint != "" {
		attrs[pb.AttrSharedKeyHint] = gi.SharedKeyHint
		addCap(&gi.Constraints, pb.CapSourceLocalSharedKeyHint)
	}
	if gi.Differ.Type != "" {
		attrs[pb.AttrLocalDiffer] = string(gi.Differ.Type)
		if gi.Differ.Required {
			addCap(&gi.Constraints, pb.CapSourceLocalDiffer)
		}
	}
	if gi.MetadataOnlyCollector {
		attrs[pb.AttrMetadataTransfer] = "true"
		if gi.MetadataOnlyExceptions != "" {
			attrs[pb.AttrMetadataTransferExclude] = gi.MetadataOnlyExceptions
		}
		addCap(&gi.Constraints, pb.CapSourceMetadataTransfer)
	}

	addCap(&gi.Constraints, pb.CapSourceLocal)

	source := NewSource("local://"+name, attrs, gi.Constraints)
	return NewState(source.Output())
}

type LocalOption interface {
	SetLocalOption(*LocalInfo)
}

type localOptionFunc func(*LocalInfo)

func (fn localOptionFunc) SetLocalOption(li *LocalInfo) {
	fn(li)
}

func SessionID(id string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		li.SessionID = id
	})
}

func IncludePatterns(p []string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		if len(p) == 0 {
			li.IncludePatterns = ""
			return
		}
		dt, _ := json.Marshal(p) // empty on error
		li.IncludePatterns = string(dt)
	})
}

func FollowPaths(p []string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		if len(p) == 0 {
			li.FollowPaths = ""
			return
		}
		dt, _ := json.Marshal(p) // empty on error
		li.FollowPaths = string(dt)
	})
}

func ExcludePatterns(p []string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		if len(p) == 0 {
			li.ExcludePatterns = ""
			return
		}
		dt, _ := json.Marshal(p) // empty on error
		li.ExcludePatterns = string(dt)
	})
}

func SharedKeyHint(h string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		li.SharedKeyHint = h
	})
}

func Differ(t DiffType, required bool) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		li.Differ = DifferInfo{
			Type:     t,
			Required: required,
		}
	})
}

func MetadataOnlyTransfer(exceptions []string) LocalOption {
	return localOptionFunc(func(li *LocalInfo) {
		li.MetadataOnlyCollector = true
		if len(exceptions) == 0 {
			li.MetadataOnlyExceptions = ""
		} else {
			dt, _ := json.Marshal(exceptions) // empty on error
			li.MetadataOnlyExceptions = string(dt)
		}
	})
}

func OCILayout(ref string, opts ...OCILayoutOption) State {
	gi := &OCILayoutInfo{}

	for _, o := range opts {
		o.SetOCILayoutOption(gi)
	}
	attrs := map[string]string{}
	if gi.sessionID != "" {
		attrs[pb.AttrOCILayoutSessionID] = gi.sessionID
	}
	if gi.storeID != "" {
		attrs[pb.AttrOCILayoutStoreID] = gi.storeID
	}
	if gi.layerLimit != nil {
		attrs[pb.AttrOCILayoutLayerLimit] = strconv.FormatInt(int64(*gi.layerLimit), 10)
	}

	addCap(&gi.Constraints, pb.CapSourceOCILayout)

	source := NewSource("oci-layout://"+ref, attrs, gi.Constraints)
	return NewState(source.Output())
}

type OCILayoutOption interface {
	SetOCILayoutOption(*OCILayoutInfo)
}

type ociLayoutOptionFunc func(*OCILayoutInfo)

func (fn ociLayoutOptionFunc) SetOCILayoutOption(li *OCILayoutInfo) {
	fn(li)
}

func OCIStore(sessionID string, storeID string) OCILayoutOption {
	return ociLayoutOptionFunc(func(oi *OCILayoutInfo) {
		oi.sessionID = sessionID
		oi.storeID = storeID
	})
}

// ImageBlobOCIStore returns an [ImageBlobOption] that configures the OCI layout session/store used by [OCILayoutBlob].
func ImageBlobOCIStore(sessionID string, storeID string) ImageBlobOption {
	return imageBlobOptionFunc(func(ib *ImageBlobInfo) {
		ib.sessionID = sessionID
		ib.storeID = storeID
	})
}

func OCILayerLimit(limit int) OCILayoutOption {
	return ociLayoutOptionFunc(func(oi *OCILayoutInfo) {
		oi.layerLimit = &limit
	})
}

func OCIChecksum(dgst digest.Digest) OCILayoutOption {
	return ociLayoutOptionFunc(func(oi *OCILayoutInfo) {
		oi.checksum = dgst
	})
}

type OCILayoutInfo struct {
	constraintsWrapper
	sessionID  string
	storeID    string
	layerLimit *int
	checksum   digest.Digest
}

type DiffType string

const (
	// DiffNone will do no file comparisons, all files in the Local source will
	// be retransmitted.
	DiffNone DiffType = pb.AttrLocalDifferNone
	// DiffMetadata will compare file metadata (size, modified time, mode, owner,
	// group, device and link name) to determine if the files in the Local source need
	// to be retransmitted.  This is the default behavior.
	DiffMetadata DiffType = pb.AttrLocalDifferMetadata
)

type DifferInfo struct {
	Type     DiffType
	Required bool
}

type LocalInfo struct {
	constraintsWrapper
	SessionID              string
	IncludePatterns        string
	ExcludePatterns        string
	FollowPaths            string
	SharedKeyHint          string
	Differ                 DifferInfo
	MetadataOnlyCollector  bool
	MetadataOnlyExceptions string
}

func HTTP(url string, opts ...HTTPOption) State {
	hi := &HTTPInfo{}
	for _, o := range opts {
		o.SetHTTPOption(hi)
	}
	attrs := map[string]string{}
	if hi.Checksum != "" {
		attrs[pb.AttrHTTPChecksum] = hi.Checksum.String()
		addCap(&hi.Constraints, pb.CapSourceHTTPChecksum)
	}
	if hi.Filename != "" {
		attrs[pb.AttrHTTPFilename] = hi.Filename
	}
	if hi.Perm != 0 {
		attrs[pb.AttrHTTPPerm] = "0" + strconv.FormatInt(int64(hi.Perm), 8)
		addCap(&hi.Constraints, pb.CapSourceHTTPPerm)
	}
	if hi.UID != 0 {
		attrs[pb.AttrHTTPUID] = strconv.Itoa(hi.UID)
		addCap(&hi.Constraints, pb.CapSourceHTTPUIDGID)
	}
	if hi.GID != 0 {
		attrs[pb.AttrHTTPGID] = strconv.Itoa(hi.GID)
		addCap(&hi.Constraints, pb.CapSourceHTTPUIDGID)
	}
	if hi.AuthHeaderSecret != "" {
		attrs[pb.AttrHTTPAuthHeaderSecret] = hi.AuthHeaderSecret
		addCap(&hi.Constraints, pb.CapSourceHTTPAuth)
	}
	if hi.Header != nil {
		hi.Header.setAttrs(attrs)
		addCap(&hi.Constraints, pb.CapSourceHTTPHeader)
	}
	if hi.Signature != nil {
		if len(hi.Signature.PubKey) > 0 {
			attrs[pb.AttrHTTPSignatureVerifyPubKey] = string(hi.Signature.PubKey)
		}
		if len(hi.Signature.Signature) > 0 {
			attrs[pb.AttrHTTPSignatureVerify] = string(hi.Signature.Signature)
		}
		addCap(&hi.Constraints, pb.CapSourceHTTPSignatureVerify)
	}

	addCap(&hi.Constraints, pb.CapSourceHTTP)
	source := NewSource(url, attrs, hi.Constraints)
	return NewState(source.Output())
}

type fileInfo struct {
	Filename string
	Perm     int
	UID      int
	GID      int
}

type fileinfoWrapper struct {
	fileInfo
}

type fileInfoOptFunc func(f *fileInfo)

func (fn fileInfoOptFunc) SetHTTPOption(hi *HTTPInfo) {
	fn(&hi.fileInfo)
}

func (fn fileInfoOptFunc) SetImageBlobOption(ib *ImageBlobInfo) {
	fn(&ib.fileInfo)
}

// HTTPSignatureInfo configures detached-signature verification for HTTP
// sources. The current implementation uses inline armored signatures.
type HTTPSignatureInfo struct {
	PubKey []byte

	// Signature is an inline detached armored OpenPGP signature.
	Signature []byte
}

type HTTPInfo struct {
	constraintsWrapper
	fileinfoWrapper
	Checksum         digest.Digest
	AuthHeaderSecret string
	Header           *HTTPHeader
	Signature        *HTTPSignatureInfo
}

type HTTPOption interface {
	SetHTTPOption(*HTTPInfo)
}

type httpOptionFunc func(*HTTPInfo)

func (fn httpOptionFunc) SetHTTPOption(hi *HTTPInfo) {
	fn(hi)
}

func Checksum(dgst digest.Digest) HTTPOption {
	return httpOptionFunc(func(hi *HTTPInfo) {
		hi.Checksum = dgst
	})
}

func Chmod(perm os.FileMode) FileInfoOption {
	return fileInfoOptFunc(func(fi *fileInfo) {
		fi.Perm = int(perm) & 0777
	})
}

func Filename(name string) FileInfoOption {
	return fileInfoOptFunc(func(fi *fileInfo) {
		fi.Filename = name
	})
}

func Chown(uid, gid int) FileInfoOption {
	return fileInfoOptFunc(func(fi *fileInfo) {
		fi.UID = uid
		fi.GID = gid
	})
}

// VerifyPGPSignature returns an [HTTPOption] for detached OpenPGP signature
// verification of the downloaded HTTP payload.
func VerifyPGPSignature(info HTTPSignatureInfo) HTTPOption {
	return httpOptionFunc(func(hi *HTTPInfo) {
		hi.Signature = &HTTPSignatureInfo{
			PubKey:    slices.Clone(info.PubKey),
			Signature: slices.Clone(info.Signature),
		}
	})
}

// Header returns an [HTTPOption] that ensures additional request headers will
// be sent when retrieving the HTTP source.
func Header(header HTTPHeader) HTTPOption {
	return httpOptionFunc(func(hi *HTTPInfo) {
		hi.Header = &header
	})
}

type HTTPHeader struct {
	Accept    string
	UserAgent string
}

func (hh *HTTPHeader) setAttrs(attrs map[string]string) {
	if hh.Accept != "" {
		attrs[hh.attr("accept")] = hh.Accept
	}

	if hh.UserAgent != "" {
		attrs[hh.attr("user-agent")] = hh.UserAgent
	}
}

func (hh *HTTPHeader) attr(name string) string {
	return pb.AttrHTTPHeaderPrefix + name
}

func platformSpecificSource(id string) bool {
	return strings.HasPrefix(id, "docker-image://") || strings.HasPrefix(id, "oci-layout://")
}

func addCap(c *Constraints, id apicaps.CapID) {
	if c.Metadata.Caps == nil {
		c.Metadata.Caps = make(map[apicaps.CapID]bool)
	}
	c.Metadata.Caps[id] = true
}

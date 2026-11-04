package dockerui

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/attestations"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/patternmatcher/ignorefile"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	buildArgPrefix       = "build-arg:"
	labelPrefix          = "label:"
	localSessionIDPrefix = "local-sessionid:"

	keyTarget           = "target"
	keyCgroupParent     = "cgroup-parent"
	keyForceNetwork     = "force-network-mode"
	keyGlobalAddHosts   = "add-hosts"
	keyHostname         = "hostname"
	keyImageResolveMode = "image-resolve-mode"
	keyMultiPlatform    = "multi-platform"
	keyNoCache          = "no-cache"
	keyShmSize          = "shm-size"
	keyTargetPlatform   = "platform"
	keyUlimit           = "ulimit"
	keyMemory           = "memory"
	keyMemorySwap       = "memswap"
	keyCPUShares        = "cpushares"
	keyCPUPeriod        = "cpuperiod"
	keyCPUQuota         = "cpuquota"
	keyCpusetCpus       = "cpusetcpus"
	keyCpusetMems       = "cpusetmems"
	keyCacheFrom        = "cache-from"    // for registry only. deprecated in favor of keyCacheImports
	keyCacheImports     = "cache-imports" // JSON representation of []CacheOptionsEntry

	// Don't forget to update frontend documentation if you add
	// a new build-arg: frontend/dockerfile/docs/reference.md
	keyCacheNSArg           = "build-arg:BUILDKIT_CACHE_MOUNT_NS"
	keyMultiPlatformArg     = "build-arg:BUILDKIT_MULTI_PLATFORM"
	keyHostnameArg          = "build-arg:BUILDKIT_SANDBOX_HOSTNAME"
	keyDockerfileLintArg    = "build-arg:BUILDKIT_DOCKERFILE_CHECK"
	keyContextKeepGitDirArg = "build-arg:BUILDKIT_CONTEXT_KEEP_GIT_DIR"
	keyGitAdviceArg         = "build-arg:BUILDKIT_GIT_ADVICE"
	keyGitDebugCommandsArg  = "build-arg:BUILDKIT_GIT_DEBUG_COMMANDS"
)

type Config struct {
	BuildArgs        map[string]string
	CacheIDNamespace string
	CgroupParent     string
	ExtraHosts       []llb.HostIP
	Hostname         string
	ImageResolveMode llb.ResolveMode
	Labels           map[string]string
	NetworkMode      pb.NetMode
	GitAdvice        bool
	GitDebugCommands bool
	ShmSize          int64
	Target           string
	Ulimits          []*pb.Ulimit
	LinuxResources   *pb.LinuxResources
	Devices          []*pb.CDIDevice
	LinterConfig     *linter.Config

	CacheImports           []client.CacheOptionsEntry
	TargetPlatforms        []ocispecs.Platform // nil means default
	BuildPlatforms         []ocispecs.Platform
	MultiPlatformRequested bool
	SBOM                   *SBOM
}

type Client struct {
	Config
	client           client.Client
	ignoreCache      []string
	g                flightcontrol.CachedGroup[*buildContext]
	bopts            client.BuildOpts
	localsSessionIDs map[string]string

	dockerignore     []byte
	dockerignoreMu   sync.Mutex
	dockerignoreName string
}

type SBOM struct {
	Generator  string
	Parameters map[string]string
}

type Source struct {
	*llb.SourceMap
	Warn func(context.Context, string, client.WarnOpts)
}

type ContextOpt struct {
	NoDockerignore bool
	AsyncLocalOpts func() []llb.LocalOption
	Platform       *ocispecs.Platform
	ResolveMode    string
	CaptureDigest  *digest.Digest
}

func validateMinCaps(c client.Client) error {
	opts := c.BuildOpts().Opts
	caps := c.BuildOpts().LLBCaps

	if err := caps.Supports(pb.CapFileBase); err != nil {
		return errors.Wrap(err, "needs BuildKit 0.5 or later")
	}
	if opts["override-copy-image"] != "" {
		return errors.New("support for \"override-copy-image\" was removed in BuildKit 0.11")
	}
	if v, ok := opts["build-arg:BUILDKIT_DISABLE_FILEOP"]; ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			return errors.New("support for \"BUILDKIT_DISABLE_FILEOP\" build-arg was removed in BuildKit 0.11")
		}
	}
	return nil
}

func NewClient(c client.Client) (*Client, error) {
	if err := validateMinCaps(c); err != nil {
		return nil, err
	}

	bc := &Client{
		client: c,
		bopts:  c.BuildOpts(), // avoid grpc on every call
	}

	if err := bc.init(); err != nil {
		return nil, err
	}

	return bc, nil
}

func (bc *Client) BuildOpts() client.BuildOpts {
	return bc.bopts
}

func (bc *Client) GatewayClient() client.Client {
	return bc.client
}

func (bc *Client) init() error {
	opts := bc.bopts.Opts

	var defaultBuildPlatform ocispecs.Platform
	if workers := bc.bopts.Workers; len(workers) > 0 && len(workers[0].Platforms) > 0 {
		defaultBuildPlatform = workers[0].Platforms[0]
	} else {
		defaultBuildPlatform = platforms.Normalize(platforms.DefaultSpec())
	}
	buildPlatforms := []ocispecs.Platform{defaultBuildPlatform}
	targetPlatforms := []ocispecs.Platform{}
	if v := opts[keyTargetPlatform]; v != "" {
		var err error
		targetPlatforms, err = parsePlatforms(v)
		if err != nil {
			return err
		}
	}
	bc.BuildPlatforms = buildPlatforms
	bc.TargetPlatforms = targetPlatforms

	resolveMode, err := parseResolveMode(opts[keyImageResolveMode])
	if err != nil {
		return err
	}
	bc.ImageResolveMode = resolveMode

	extraHosts, err := parseExtraHosts(opts[keyGlobalAddHosts])
	if err != nil {
		return errors.Wrap(err, "failed to parse additional hosts")
	}
	bc.ExtraHosts = extraHosts

	shmSize, err := parseShmSize(opts[keyShmSize])
	if err != nil {
		return errors.Wrap(err, "failed to parse shm size")
	}
	bc.ShmSize = shmSize

	ulimits, err := parseUlimits(opts[keyUlimit])
	if err != nil {
		return errors.Wrap(err, "failed to parse ulimit")
	}
	bc.Ulimits = ulimits

	linuxRes, err := parseLinuxResources(opts)
	if err != nil {
		return errors.Wrap(err, "failed to parse resource limits")
	}
	bc.LinuxResources = linuxRes

	defaultNetMode, err := parseNetMode(opts[keyForceNetwork])
	if err != nil {
		return err
	}
	bc.NetworkMode = defaultNetMode

	var ignoreCache []string
	if v, ok := opts[keyNoCache]; ok {
		if v == "" {
			ignoreCache = []string{} // means all stages
		} else {
			ignoreCache = strings.Split(v, ",")
		}
	}
	bc.ignoreCache = ignoreCache

	multiPlatform := len(targetPlatforms) > 1
	if v := opts[keyMultiPlatformArg]; v != "" {
		opts[keyMultiPlatform] = v
	}
	if v := opts[keyMultiPlatform]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return errors.Errorf("invalid boolean value for multi-platform: %s", v)
		}
		if !b && multiPlatform {
			return errors.New("conflicting config: returning multiple target platforms is not allowed")
		}
		multiPlatform = b
	}
	bc.MultiPlatformRequested = multiPlatform

	var cacheImports []client.CacheOptionsEntry
	// new API
	if cacheImportsStr := opts[keyCacheImports]; cacheImportsStr != "" {
		var cacheImportsUM []*controlapi.CacheOptionsEntry
		if err := json.Unmarshal([]byte(cacheImportsStr), &cacheImportsUM); err != nil {
			return errors.Wrapf(err, "failed to unmarshal %s (%q)", keyCacheImports, cacheImportsStr)
		}
		for _, um := range cacheImportsUM {
			cacheImports = append(cacheImports, client.CacheOptionsEntry{Type: um.Type, Attrs: um.Attrs})
		}
	}
	// old API
	if cacheFromStr := opts[keyCacheFrom]; cacheFromStr != "" {
		cacheFrom := strings.SplitSeq(cacheFromStr, ",")
		for s := range cacheFrom {
			im := client.CacheOptionsEntry{
				Type: "registry",
				Attrs: map[string]string{
					"ref": s,
				},
			}
			// FIXME(AkihiroSuda): skip append if already exists
			cacheImports = append(cacheImports, im)
		}
	}
	bc.CacheImports = cacheImports

	attests, err := attestations.Parse(opts)
	if err != nil {
		return err
	}
	if attrs, ok := attests[attestations.KeyTypeSbom]; ok {
		params := make(map[string]string)
		var ref reference.Named
		for k, v := range attrs {
			if k == "generator" {
				ref, err = reference.ParseNormalizedNamed(v)
				if err != nil {
					return errors.Wrapf(err, "failed to parse sbom scanner %s", v)
				}
				ref = reference.TagNameOnly(ref)
			} else {
				params[k] = v
			}
		}
		if ref == nil {
			return errors.New("sbom scanner cannot be empty")
		}

		bc.SBOM = &SBOM{
			Generator:  ref.String(),
			Parameters: params,
		}
	}

	bc.BuildArgs = filter(opts, buildArgPrefix)
	bc.Labels = filter(opts, labelPrefix)
	bc.CacheIDNamespace = opts[keyCacheNSArg]
	bc.CgroupParent = opts[keyCgroupParent]
	bc.Target = opts[keyTarget]

	if v, ok := opts[keyHostnameArg]; ok && len(v) > 0 {
		opts[keyHostname] = v
	}
	bc.Hostname = opts[keyHostname]

	if v, ok := opts[keyDockerfileLintArg]; ok {
		bc.LinterConfig, err = linter.ParseLintOptions(v)
		if err != nil {
			return errors.Wrapf(err, "failed to parse %s", keyDockerfileLintArg)
		}
	}
	if v := opts[keyGitAdviceArg]; v != "" {
		bc.GitAdvice, err = strconv.ParseBool(v)
		if err != nil {
			return errors.Wrapf(err, "failed to parse %s", keyGitAdviceArg)
		}
	}
	if v := opts[keyGitDebugCommandsArg]; v != "" {
		bc.GitDebugCommands, err = strconv.ParseBool(v)
		if err != nil {
			return errors.Wrapf(err, "failed to parse %s", keyGitDebugCommandsArg)
		}
	}

	bc.localsSessionIDs = parseLocalSessionIDs(opts)

	return nil
}

func (bc *Client) buildContext(ctx context.Context) (*buildContext, error) {
	return bc.g.Do(ctx, "initcontext", func(ctx context.Context) (*buildContext, error) {
		return bc.initContext(ctx)
	})
}

func (bc *Client) ReadEntrypoint(ctx context.Context, lang string, opts ...llb.LocalOption) (*Source, error) {
	bctx, err := bc.buildContext(ctx)
	if err != nil {
		return nil, err
	}

	var src *llb.State

	if !bctx.forceLocalDockerfile {
		if bctx.dockerfile != nil {
			src = bctx.dockerfile
		}
	}

	if src == nil {
		name := "load build definition from " + bctx.filename

		filenames := []string{bctx.filename, bctx.filename + ".dockerignore"}

		// dockerfile is also supported casing moby/moby#10858
		if path.Base(bctx.filename) == DefaultDockerfileName {
			filenames = append(filenames, path.Join(path.Dir(bctx.filename), strings.ToLower(DefaultDockerfileName)))
		}

		sessionID := bc.bopts.SessionID
		if v, ok := bc.localsSessionIDs[bctx.dockerfileLocalName]; ok {
			sessionID = v
		}

		opts = append([]llb.LocalOption{
			llb.FollowPaths(filenames),
			llb.SessionID(sessionID),
			llb.SharedKeyHint(bctx.dockerfileLocalName),
			WithInternalName(name),
			llb.Differ(llb.DiffNone, false),
		}, opts...)

		lsrc := llb.Local(bctx.dockerfileLocalName, opts...)
		src = &lsrc
	}

	def, err := src.Marshal(ctx, bc.marshalOpts()...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal local source")
	}

	defVtx, err := def.Head()
	if err != nil {
		return nil, err
	}

	res, err := bc.client.Solve(ctx, client.SolveRequest{
		Definition: def.ToPB(),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve dockerfile")
	}

	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}

	dt, err := ReadFile(ctx, ref, bctx.filename)
	if err != nil {
		if path.Base(bctx.filename) == DefaultDockerfileName && !isFileTooLarge(err) {
			var err1 error
			dt, err1 = ReadFile(ctx, ref, path.Join(path.Dir(bctx.filename), strings.ToLower(DefaultDockerfileName)))
			if err1 == nil {
				err = nil
			}
		}
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read dockerfile")
		}
	}
	smap := llb.NewSourceMap(src, bctx.filename, lang, dt)
	smap.Definition = def

	// a missing ignore file is not an error, but an oversized one must not
	// be silently skipped
	dt, err = ReadFile(ctx, ref, bctx.filename+".dockerignore")
	if err == nil {
		bc.dockerignore = dt
		bc.dockerignoreName = bctx.filename + ".dockerignore"
	} else if isFileTooLarge(err) {
		return nil, err
	}

	return &Source{
		SourceMap: smap,
		Warn: func(ctx context.Context, msg string, opts client.WarnOpts) {
			if opts.Level == 0 {
				opts.Level = 1
			}
			if opts.SourceInfo == nil {
				opts.SourceInfo = &pb.SourceInfo{
					Data:       smap.Data,
					Filename:   smap.Filename,
					Language:   smap.Language,
					Definition: smap.Definition.ToPB(),
				}
			}
			bc.client.Warn(ctx, defVtx, msg, opts)
		},
	}, nil
}

func (bc *Client) MainContext(ctx context.Context, opts ...llb.LocalOption) (*llb.State, error) {
	bctx, err := bc.buildContext(ctx)
	if err != nil {
		return nil, err
	}

	if bctx.context != nil {
		return bctx.context, nil
	}

	excludes, err := bc.dockerIgnorePatterns(ctx, bctx)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read dockerignore patterns")
	}

	sessionID := bc.bopts.SessionID
	if v, ok := bc.localsSessionIDs[bctx.contextLocalName]; ok {
		sessionID = v
	}

	opts = append([]llb.LocalOption{
		llb.SessionID(sessionID),
		llb.ExcludePatterns(excludes),
		llb.SharedKeyHint(bctx.contextLocalName),
		WithInternalName("load build context"),
	}, opts...)

	st := llb.Local(bctx.contextLocalName, opts...)

	return &st, nil
}

func (bc *Client) NamedContext(name string, opt ContextOpt) (*NamedContext, error) {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid context name %s", name)
	}
	name = strings.TrimSuffix(reference.FamiliarString(named), ":latest")

	var pp ocispecs.Platform
	if opt.Platform != nil {
		pp = *opt.Platform
	} else {
		pp = platforms.DefaultSpec()
	}
	pname := name + "::" + platforms.FormatAll(platforms.Normalize(pp))
	nc, err := bc.namedContext(name, pname, opt)
	if err != nil || nc != nil {
		return nc, err
	}
	return bc.namedContext(name, name, opt)
}

func (bc *Client) IsNoCache(name string) bool {
	if len(bc.ignoreCache) == 0 {
		return bc.ignoreCache != nil
	}
	for _, n := range bc.ignoreCache {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func (bc *Client) DockerIgnorePatterns(ctx context.Context) ([]string, error) {
	if bc == nil {
		return nil, nil
	}
	bctx, err := bc.buildContext(ctx)
	if err != nil {
		return nil, err
	}
	if bctx.context != nil {
		return nil, nil
	}

	return bc.dockerIgnorePatterns(ctx, bctx)
}

func DefaultMainContext(opts ...llb.LocalOption) *llb.State {
	opts = append([]llb.LocalOption{
		llb.SharedKeyHint(DefaultLocalNameContext),
		WithInternalName("load build context"),
	}, opts...)
	st := llb.Local(DefaultLocalNameContext, opts...)
	return &st
}

func WithInternalName(name string) llb.ConstraintsOpt {
	return llb.WithCustomName("[internal] " + name)
}

func (bc *Client) dockerIgnorePatterns(ctx context.Context, bctx *buildContext) ([]string, error) {
	bc.dockerignoreMu.Lock()
	defer bc.dockerignoreMu.Unlock()
	if bc.dockerignore == nil {
		sessionID := bc.bopts.SessionID
		if v, ok := bc.localsSessionIDs[bctx.contextLocalName]; ok {
			sessionID = v
		}
		st := llb.Local(bctx.contextLocalName,
			llb.SessionID(sessionID),
			llb.FollowPaths([]string{DefaultDockerignoreName}),
			llb.SharedKeyHint(bctx.contextLocalName+"-"+DefaultDockerignoreName),
			WithInternalName("load "+DefaultDockerignoreName),
			llb.Differ(llb.DiffNone, false),
		)
		def, err := st.Marshal(ctx, bc.marshalOpts()...)
		if err != nil {
			return nil, err
		}
		res, err := bc.client.Solve(ctx, client.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		// a missing ignore file is not an error, but an oversized one must
		// not be silently skipped
		dt, err := ReadFile(ctx, ref, DefaultDockerignoreName)
		if isFileTooLarge(err) {
			return nil, err
		}
		if dt == nil {
			dt = []byte{}
		}
		bc.dockerignore = dt
		bc.dockerignoreName = DefaultDockerignoreName
	}
	var err error
	var excludes []string
	if len(bc.dockerignore) != 0 {
		excludes, err = ignorefile.ReadAll(bytes.NewBuffer(bc.dockerignore))
		if err != nil {
			return nil, errors.Wrapf(err, "failed parsing %s", bc.dockerignoreName)
		}
	}
	return excludes, nil
}

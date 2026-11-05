package gha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/labels"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/cache/remotecache/signing"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/tracing"
	bkversion "github.com/moby/buildkit/version"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	actionscache "github.com/tonistiigi/go-actions-cache"
	"golang.org/x/sync/errgroup"
)

func init() {
	actionscache.Log = bklog.L.Debugf
}

const (
	attrScope      = "scope"
	attrTimeout    = "timeout"
	attrToken      = "token"
	attrURL        = "url"
	attrURLV2      = "url_v2"
	attrRepository = "repository"
	attrGHToken    = "ghtoken"
	attrAPIVersion = "version"
	version        = "1"

	defaultTimeout = 10 * time.Minute
)

type Config struct {
	Scope      string
	URL        string
	Token      string // token for the Github Cache runtime API
	GHToken    string // token for the Github REST API
	Repository string
	Version    int
	Timeout    time.Duration

	*signing.Config
	verifier signing.Verifier
}

func getConfig(conf *signing.Config, v signing.VerifierProvider, attrs map[string]string) (*Config, error) {
	scope, ok := attrs[attrScope]
	if !ok {
		scope = "buildkit"
	}
	token, ok := attrs[attrToken]
	if !ok {
		return nil, errors.Errorf("token not set for github actions cache")
	}
	var apiVersionInt int
	apiVersion, ok := attrs[attrAPIVersion]
	if ok {
		i, err := strconv.ParseInt(apiVersion, 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse api version %q, expected positive integer", apiVersion)
		}
		apiVersionInt = int(i)
	}
	var url string
	if apiVersionInt != 1 {
		if v, ok := attrs[attrURLV2]; ok {
			url = v
			apiVersionInt = 2
		}
	}
	if v, ok := attrs[attrURL]; ok && url == "" {
		url = v
	}
	if url == "" {
		return nil, errors.Errorf("url not set for github actions cache")
	}
	// best effort on old clients
	if apiVersionInt == 0 {
		if strings.Contains(url, "results-receiver.actions.githubusercontent.com") {
			apiVersionInt = 2
		} else {
			apiVersionInt = 1
		}
	}

	timeout := defaultTimeout
	if v, ok := attrs[attrTimeout]; ok {
		var err error
		timeout, err = time.ParseDuration(v)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse timeout for github actions cache")
		}
	}

	if conf == nil {
		conf = &signing.Config{}
	}

	return &Config{
		Scope:      scope,
		URL:        url,
		Token:      token,
		Timeout:    timeout,
		GHToken:    attrs[attrGHToken],
		Repository: attrs[attrRepository],
		Version:    apiVersionInt,
		Config:     conf,
		verifier:   signing.NewVerifier(conf.Verify.Policy, v),
	}, nil
}

// ResolveCacheExporterFunc for Github actions cache exporter.
func ResolveCacheExporterFunc(conf *signing.Config, v signing.VerifierProvider) remotecache.ResolveCacheExporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (remotecache.Exporter, error) {
		cfg, err := getConfig(conf, v, attrs)
		if err != nil {
			return nil, err
		}
		return NewExporter(cfg)
	}
}

type exporter struct {
	solver.CacheExporterTarget
	chains     *v1.CacheChains
	cache      *actionscache.Cache
	config     *Config
	keyMapOnce sync.Once
	keyMap     map[string]struct{}
}

func NewExporter(c *Config) (remotecache.Exporter, error) {
	cc := v1.NewCacheChains()
	cache, err := actionscache.New(c.Token, c.URL, c.Version > 1, actionscache.Opt{
		Client:    tracing.DefaultClient,
		Timeout:   c.Timeout,
		UserAgent: bkversion.UserAgent(),
	})
	if err != nil {
		return nil, err
	}
	return &exporter{CacheExporterTarget: cc, chains: cc, cache: cache, config: c}, nil
}

func (*exporter) Name() string {
	return "exporting to GitHub Actions Cache"
}

func (ce *exporter) Config() remotecache.Config {
	return remotecache.Config{
		Compression: compression.New(compression.Default),
	}
}

func blobKeyPrefix() string {
	return "buildkit-blob-" + version + "-"
}

func blobKey(dgst digest.Digest) string {
	return blobKeyPrefix() + dgst.String()
}

func (ce *exporter) indexKey() string {
	scope := ""
	for _, s := range ce.cache.Scopes() {
		if s.Permission&actionscache.PermissionWrite != 0 {
			scope = s.Scope
		}
	}
	return indexKey(scope, ce.config)
}

func indexKey(scope string, config *Config) string {
	scope = digest.FromBytes([]byte(scope)).Hex()[:8]
	key := "index-" + config.Scope + "-" + version + "-" + scope
	// just to be sure lets namespace the signed vs unsigned caches
	if config.Sign != nil || config.Verify.Required {
		key += "-sig"
	}
	return key
}

func (ce *exporter) initActiveKeyMap(ctx context.Context) {
	ce.keyMapOnce.Do(func() {
		if ce.config.Repository == "" || ce.config.GHToken == "" {
			return
		}
		m, err := ce.initActiveKeyMapOnce(ctx)
		if err != nil {
			bklog.G(ctx).Errorf("error initializing active key map: %v", err)
			return
		}
		ce.keyMap = m
	})
}

func (ce *exporter) initActiveKeyMapOnce(ctx context.Context) (map[string]struct{}, error) {
	api, err := actionscache.NewRestAPI(ce.config.Repository, ce.config.GHToken, actionscache.Opt{
		Client:  tracing.DefaultClient,
		Timeout: ce.config.Timeout,
	})
	if err != nil {
		return nil, err
	}
	keys, err := ce.cache.AllKeys(ctx, api, blobKeyPrefix())
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (ce *exporter) Finalize(ctx context.Context) (_ map[string]string, err error) {
	// res := make(map[string]string)
	config, descs, err := ce.chains.Marshal(ctx)
	if err != nil {
		return nil, err
	}

	// TODO: push parallel
	for i, l := range config.Layers {
		dgstPair, ok := descs[l.Blob]
		if !ok {
			return nil, errors.Errorf("missing blob %s", l.Blob)
		}
		if dgstPair.Descriptor.Annotations == nil {
			return nil, errors.Errorf("invalid descriptor without annotations")
		}
		var diffID digest.Digest
		v, ok := dgstPair.Descriptor.Annotations[labels.LabelUncompressed]
		if !ok {
			return nil, errors.Errorf("invalid descriptor without uncompressed annotation")
		}
		dgst, err := digest.Parse(v)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse uncompressed annotation")
		}
		diffID = dgst
		ce.initActiveKeyMap(ctx)

		key := blobKey(dgstPair.Descriptor.Digest)

		exists := false
		if ce.keyMap != nil {
			if _, ok := ce.keyMap[key]; ok {
				exists = true
			}
		} else {
			b, err := ce.cache.Load(ctx, key)
			if err != nil {
				return nil, err
			}
			if b != nil {
				exists = true
			}
		}
		if !exists {
			layerDone := progress.OneOff(ctx, fmt.Sprintf("writing layer %s", l.Blob))
			ra, err := dgstPair.Provider.ReaderAt(ctx, dgstPair.Descriptor)
			if err != nil {
				return nil, layerDone(err)
			}
			if err := ce.cache.Save(ctx, key, ra); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return nil, layerDone(errors.Wrap(err, "error writing layer blob"))
				}
			}
			layerDone(nil)
		}
		la := &cacheimporttypes.LayerAnnotations{
			DiffID:    diffID,
			Size:      dgstPair.Descriptor.Size,
			MediaType: dgstPair.Descriptor.MediaType,
		}
		if v, ok := dgstPair.Descriptor.Annotations["buildkit/createdat"]; ok {
			var t time.Time
			if err := (&t).UnmarshalText([]byte(v)); err != nil {
				return nil, err
			}
			la.CreatedAt = t.UTC()
		}
		config.Layers[i].Annotations = la
	}

	dt, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}

	if err := ce.cache.SaveMutable(ctx, ce.indexKey(), 15*time.Second, func(old *actionscache.Entry) (actionscache.Blob, error) {
		return actionscache.NewBlob(dt), nil
	}); err != nil {
		return nil, err
	}

	if ce.config.Sign == nil {
		return nil, nil
	}

	args := ce.config.Sign.Command
	if len(args) == 0 {
		return nil, nil
	}

	dgst := digest.FromBytes(dt)
	signDone := progress.OneOff(ctx, fmt.Sprintf("signing cache index %s", dgst))
	defer func() { signDone(err) }()

	bundle, err := signing.NewSigner(args, ce.config.verifier).Sign(ctx, dt)
	if err != nil {
		return nil, err
	}

	key := blobKey(dgst + "-sig")
	if err := ce.cache.Save(ctx, key, actionscache.NewBlob(bundle)); err != nil {
		return nil, err
	}

	return nil, nil
}

// ResolveCacheImporterFunc for Github actions cache importer.
func ResolveCacheImporterFunc(conf *signing.Config, v signing.VerifierProvider) remotecache.ResolveCacheImporterFunc {
	return func(ctx context.Context, g session.Group, attrs map[string]string) (remotecache.Importer, ocispecs.Descriptor, error) {
		cfg, err := getConfig(conf, v, attrs)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}
		i, err := NewImporter(cfg)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}
		return i, ocispecs.Descriptor{}, nil
	}
}

type importer struct {
	cache  *actionscache.Cache
	config *Config
}

func NewImporter(c *Config) (remotecache.Importer, error) {
	cache, err := actionscache.New(c.Token, c.URL, c.Version > 1, actionscache.Opt{
		Client:    tracing.DefaultClient,
		Timeout:   c.Timeout,
		UserAgent: bkversion.UserAgent(),
	})
	if err != nil {
		return nil, err
	}
	return &importer{cache: cache, config: c}, nil
}

func (ci *importer) makeDescriptorProviderPair(l cacheimporttypes.CacheLayer) (*v1.DescriptorProviderPair, error) {
	if l.Annotations == nil {
		return nil, errors.Errorf("cache layer with missing annotations")
	}
	annotations := map[string]string{}
	if l.Annotations.DiffID == "" {
		return nil, errors.Errorf("cache layer with missing diffid")
	}
	annotations[labels.LabelUncompressed] = l.Annotations.DiffID.String()
	if !l.Annotations.CreatedAt.IsZero() {
		txt, err := l.Annotations.CreatedAt.MarshalText()
		if err != nil {
			return nil, err
		}
		annotations["buildkit/createdat"] = string(txt)
	}
	desc := ocispecs.Descriptor{
		MediaType:   l.Annotations.MediaType,
		Digest:      l.Blob,
		Size:        l.Annotations.Size,
		Annotations: annotations,
	}
	p := &ciProvider{desc: desc, ci: ci}
	return &v1.DescriptorProviderPair{
		Descriptor:   desc,
		Provider:     p,
		InfoProvider: p,
	}, nil
}

func (ci *importer) loadScope(ctx context.Context, scope string) (*v1.CacheChains, error) {
	key := indexKey(scope, ci.config)

	entry, err := ci.cache.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return v1.NewCacheChains(), nil
	}

	buf := &bytes.Buffer{}
	if err := entry.WriteTo(ctx, buf); err != nil {
		return nil, err
	}

	if ci.config.Verify.Required {
		dgst := digest.FromBytes(buf.Bytes())

		verifyDone := progress.OneOff(ctx, fmt.Sprintf("verifying signature of cache index %s", dgst))
		sigKey := blobKey(dgst) + "-sig"
		sigEntry, err := ci.cache.Load(ctx, sigKey)
		if err != nil {
			verifyDone(err)
			return nil, err
		}
		if sigEntry == nil {
			err := errors.Errorf("missing signature for github actions cache")
			verifyDone(err)
			return nil, err
		}
		sigBuf := &bytes.Buffer{}
		if err := sigEntry.WriteTo(ctx, sigBuf); err != nil {
			verifyDone(err)
			return nil, err
		}
		if err := ci.config.verifier.Verify(ctx, dgst, sigBuf.Bytes()); err != nil {
			verifyDone(err)
			return nil, err
		}
		verifyDone(nil)
	}

	var config cacheimporttypes.CacheConfig
	if err := json.Unmarshal(buf.Bytes(), &config); err != nil {
		return nil, errors.WithStack(err)
	}

	allLayers := v1.DescriptorProvider{}

	for _, l := range config.Layers {
		dpp, err := ci.makeDescriptorProviderPair(l)
		if err != nil {
			return nil, err
		}
		allLayers[l.Blob] = *dpp
	}

	cc := v1.NewCacheChains()
	if err := v1.ParseConfig(config, allLayers, cc); err != nil {
		return nil, err
	}
	return cc, nil
}

func (ci *importer) Resolve(ctx context.Context, _ ocispecs.Descriptor, id string, w worker.Worker) (solver.CacheManager, error) {
	eg, ctx := errgroup.WithContext(ctx)
	ccs := make([]*v1.CacheChains, len(ci.cache.Scopes()))

	for i, s := range ci.cache.Scopes() {
		func(i int, scope string) {
			eg.Go(func() error {
				cc, err := ci.loadScope(ctx, scope)
				if err != nil {
					return err
				}
				ccs[i] = cc
				return nil
			})
		}(i, s.Scope)
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	cms := make([]solver.CacheManager, 0, len(ccs))

	for _, cc := range ccs {
		keysStorage, resultStorage, err := v1.NewCacheKeyStorage(cc, w)
		if err != nil {
			return nil, err
		}
		cms = append(cms, solver.NewCacheManager(ctx, id, keysStorage, resultStorage))
	}

	return solver.NewCombinedCacheManager(cms, nil), nil
}

type ciProvider struct {
	ci      *importer
	desc    ocispecs.Descriptor
	mu      sync.Mutex
	entries map[digest.Digest]*actionscache.Entry
}

func (p *ciProvider) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	if dgst != p.desc.Digest {
		return content.Info{}, errors.Wrapf(cerrdefs.ErrNotFound, "blob %s", dgst)
	}

	if _, err := p.loadEntry(ctx, p.desc); err != nil {
		return content.Info{}, err
	}
	return content.Info{
		Digest: p.desc.Digest,
		Size:   p.desc.Size,
	}, nil
}

func (p *ciProvider) loadEntry(ctx context.Context, desc ocispecs.Descriptor) (*actionscache.Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ce, ok := p.entries[desc.Digest]; ok {
		return ce, nil
	}
	key := "buildkit-blob-" + version + "-" + desc.Digest.String()
	ce, err := p.ci.cache.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	if ce == nil {
		return nil, errors.Wrapf(cerrdefs.ErrNotFound, "blob %s", desc.Digest)
	}
	if p.entries == nil {
		p.entries = make(map[digest.Digest]*actionscache.Entry)
	}
	p.entries[desc.Digest] = ce
	return ce, nil
}

func (p *ciProvider) ReaderAt(ctx context.Context, desc ocispecs.Descriptor) (content.ReaderAt, error) {
	ce, err := p.loadEntry(ctx, desc)
	if err != nil {
		return nil, err
	}
	rac := ce.Download(context.TODO())
	return &readerAt{ReaderAtCloser: rac, desc: desc}, nil
}

type readerAt struct {
	actionscache.ReaderAtCloser
	desc ocispecs.Descriptor
}

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.desc.Size {
		return 0, io.EOF
	}
	return r.ReaderAtCloser.ReadAt(p, off)
}

func (r *readerAt) Size() int64 {
	return r.desc.Size
}

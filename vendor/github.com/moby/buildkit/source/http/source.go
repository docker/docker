package http

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/moby/buildkit/source/util/pathutil"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/cachedigest"
	"github.com/moby/buildkit/util/pgpsign"
	"github.com/moby/buildkit/util/resolver/retryhandler"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/buildkit/util/urlutil"
	"github.com/moby/buildkit/version"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	HTTPAuthHeaderSecretPrefix = "HTTP_AUTH_HEADER_"
	HTTPAuthTokenSecretPrefix  = "HTTP_AUTH_TOKEN_"
)

// supportedUserHeaders defines supported user-defined header fields. Fields
// not included here will be silently dropped.
var supportedUserDefinedHeaders = map[string]bool{
	http.CanonicalHeaderKey("accept"):     true,
	http.CanonicalHeaderKey("user-agent"): true,
}

type Opt struct {
	CacheAccessor cache.Accessor
	Transport     http.RoundTripper
}

type Source struct {
	cache     cache.Accessor
	transport http.RoundTripper
}

var _ source.Source = &Source{}

func NewSource(opt Opt) (*Source, error) {
	transport := opt.Transport
	if transport == nil {
		transport = tracing.DefaultTransport
	}
	hs := &Source{
		cache:     opt.CacheAccessor,
		transport: transport,
	}
	return hs, nil
}

func (hs *Source) Schemes() []string {
	return []string{srctypes.HTTPScheme, srctypes.HTTPSScheme}
}

func (hs *Source) Identifier(scheme, ref string, attrs map[string]string, platform *pb.Platform) (source.Identifier, error) {
	id, err := NewHTTPIdentifier(ref, scheme == "https")
	if err != nil {
		return nil, err
	}

	for k, v := range attrs {
		switch k {
		case pb.AttrHTTPChecksum:
			dgst, err := digest.Parse(v)
			if err != nil {
				return nil, err
			}
			id.Checksum = dgst
		case pb.AttrHTTPFilename:
			id.Filename = v
		case pb.AttrHTTPPerm:
			i, err := strconv.ParseInt(v, 0, 64)
			if err != nil {
				return nil, err
			}
			id.Perm = int(i)
		case pb.AttrHTTPUID:
			i, err := strconv.ParseInt(v, 0, 64)
			if err != nil {
				return nil, err
			}
			id.UID = int(i)
		case pb.AttrHTTPGID:
			i, err := strconv.ParseInt(v, 0, 64)
			if err != nil {
				return nil, err
			}
			id.GID = int(i)
		case pb.AttrHTTPAuthHeaderSecret:
			id.AuthHeaderSecret = v
		case pb.AttrHTTPSignatureVerifyPubKey:
			if id.VerifySignature == nil {
				id.VerifySignature = &HTTPSignatureVerifyOptions{}
			}
			id.VerifySignature.PubKey = []byte(v)
		case pb.AttrHTTPSignatureVerify:
			if id.VerifySignature == nil {
				id.VerifySignature = &HTTPSignatureVerifyOptions{}
			}
			id.VerifySignature.Signature = []byte(v)
		default:
			if name, found := strings.CutPrefix(k, pb.AttrHTTPHeaderPrefix); found {
				name = http.CanonicalHeaderKey(name)
				if supportedUserDefinedHeaders[name] {
					id.Header = append(id.Header, HeaderField{Name: name, Value: v})
				}
			}
		}
	}

	// Sort header fields to ensure consistent hashing (see urlHash() and
	// formatCacheKey())
	slices.SortFunc(id.Header, func(a, b HeaderField) int {
		return cmp.Compare(a.Name, b.Name)
	})

	if err := validateSignatureVerifyOptions(id.VerifySignature); err != nil {
		return nil, err
	}

	return id, nil
}

type Metadata struct {
	Digest           digest.Digest
	Filename         string
	LastModified     *time.Time
	ChecksumResponse *MetadataChecksumResponse
}

type MetadataOpts struct {
	ChecksumReq *MetadataChecksumRequest
}

type MetadataChecksumAlgo int

const (
	MetadataChecksumAlgoSHA256 MetadataChecksumAlgo = iota
	MetadataChecksumAlgoSHA384
	MetadataChecksumAlgoSHA512
	maxChecksumSuffixSize = 4 * 1024
)

type MetadataChecksumRequest struct {
	Algo   MetadataChecksumAlgo
	Suffix []byte
}

type MetadataChecksumResponse struct {
	Digest string
	Suffix []byte
}

type httpSourceHandler struct {
	*Source
	src      HTTPIdentifier
	resolved *metadataWithRef
	sm       *session.Manager
}

type metadataWithRef struct {
	Metadata
	refID string
}

func (hs *Source) ResolveMetadata(ctx context.Context, id *HTTPIdentifier, sm *session.Manager, jobCtx solver.JobContext, opt MetadataOpts) (*Metadata, error) {
	hsh := &httpSourceHandler{
		src:    *id,
		Source: hs,
		sm:     sm,
	}
	return hsh.resolveMetadata(ctx, jobCtx, opt)
}

func (hs *Source) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager, _ solver.Vertex) (source.SourceInstance, error) {
	httpIdentifier, ok := id.(*HTTPIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid http identifier %v", id)
	}

	return &httpSourceHandler{
		src:    *httpIdentifier,
		Source: hs,
		sm:     sm,
	}, nil
}

func (hs *httpSourceHandler) client(g session.Group) *http.Client {
	return &http.Client{Transport: newTransport(hs.transport, hs.sm, g)}
}

// doRequest retries transient network failures and HTTP 429/503 responses with
// jittered exponential backoff, honoring Retry-After as a minimum delay.
// Requests have no body and are safe to replay. Failures reading a successful
// response body and other HTTP statuses are left to the caller.
func doRequest(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	return retryhandler.WithRetry(ctx, nil, func(ctx context.Context) (*http.Response, error) {
		resp, err := client.Do(req.Clone(ctx))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			return resp, nil
		}
		// Do not drain an error body: it may stall independently of the headers.
		resp.Body.Close()
		return nil, &retryhandler.RetryAfterError{
			Err:   errors.Errorf("invalid response status %d", resp.StatusCode),
			Delay: retryAfterDelay(resp.Header.Get("Retry-After")),
		}
	})
}

// retryAfterDelay parses either delay-seconds or an HTTP-date. Invalid and past
// values fall back to the normal backoff. Saturate large values to avoid overflow.
func retryAfterDelay(value string) time.Duration {
	value = strings.TrimSpace(value)
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(time.Until(date), 0)
	}
	return 0
}

// urlHash is internal hash the etag is stored by that doesn't leak outside
// this package.
func (hs *httpSourceHandler) urlHash() (digest.Digest, error) {
	dt, err := json.Marshal(struct {
		Filename         []byte
		Perm, UID, GID   int
		AuthHeaderSecret string `json:",omitempty"`
		Header           []HeaderField
	}{
		Filename: bytes.Join([][]byte{
			[]byte(hs.src.URL),
			[]byte(hs.src.Filename),
		}, []byte{0}),
		Perm:             hs.src.Perm,
		UID:              hs.src.UID,
		GID:              hs.src.GID,
		AuthHeaderSecret: hs.src.AuthHeaderSecret,
		Header:           hs.src.Header,
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(dt), nil
}

func (hs *httpSourceHandler) formatCacheKey(filename string, dgst digest.Digest, lastModTime *time.Time) digest.Digest {
	var lastModTimeStr string
	if lastModTime != nil {
		lastModTimeStr = lastModTime.Format(http.TimeFormat)
	}
	dt, err := json.Marshal(struct {
		Filename         string
		Perm, UID, GID   int
		Checksum         digest.Digest
		LastModTime      string        `json:",omitempty"`
		AuthHeaderSecret string        `json:",omitempty"`
		Header           []HeaderField `json:",omitempty"`
	}{
		Filename:         filename,
		Perm:             hs.src.Perm,
		UID:              hs.src.UID,
		GID:              hs.src.GID,
		Checksum:         dgst,
		LastModTime:      lastModTimeStr,
		AuthHeaderSecret: hs.src.AuthHeaderSecret,
		Header:           hs.src.Header,
	})
	if err != nil {
		return dgst
	}
	if v, err := cachedigest.FromBytes(dt, cachedigest.TypeJSON); err == nil {
		return v
	}
	return dgst
}

func (hs *httpSourceHandler) resolveMetadata(ctx context.Context, jobCtx solver.JobContext, opt MetadataOpts) (*Metadata, error) {
	if err := validateChecksumRequest(opt.ChecksumReq); err != nil {
		return nil, err
	}
	if err := validateSignatureVerifyOptions(hs.src.VerifySignature); err != nil {
		return nil, err
	}
	md, err := hs.resolveMetadataStatic(ctx, jobCtx)
	if err != nil {
		return nil, err
	}
	md, err = hs.handleSignatureVerification(ctx, jobCtx, md)
	if err != nil {
		return nil, err
	}
	return hs.handleChecksumRequest(ctx, jobCtx, md, opt)
}

func (hs *httpSourceHandler) resolveMetadataStatic(ctx context.Context, jobCtx solver.JobContext) (*Metadata, error) {
	if hs.src.Checksum != "" {
		return &Metadata{
			Digest:   hs.src.Checksum,
			Filename: getFileName(hs.src.URL, hs.src.Filename, nil),
		}, nil
	}
	if hs.resolved != nil {
		return &hs.resolved.Metadata, nil
	}
	return hs.resolveMetadataRef(ctx, jobCtx)
}

func (hs *httpSourceHandler) resolveMetadataRef(ctx context.Context, jobCtx solver.JobContext) (md *Metadata, retErr error) {
	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}

	uh, err := hs.urlHash()
	if err != nil {
		return nil, err
	}

	if hs.resolved != nil && hs.resolved.refID != "" {
		return &hs.resolved.Metadata, nil
	}
	if jobCtx != nil {
		if rc := jobCtx.ResolverCache(); rc != nil {
			vals, release, err := rc.Lock(uh)
			if err != nil {
				return nil, err
			}
			saveResolved := true
			defer func() {
				var ret any
				if retErr == nil && saveResolved && hs.resolved != nil {
					ret = hs.resolved
				}
				if err := release(ret); err != nil {
					bklog.G(ctx).WithError(err).Warn("failed to release resolver cache lock")
				}
			}()
			for _, v := range vals {
				v2, ok := v.(*metadataWithRef)
				if !ok {
					return nil, errors.Errorf("invalid HTTP resolver cache value: %T", v)
				}
				if hs.src.Checksum != "" && v2.Digest != hs.src.Checksum {
					continue
				}
				if v2.refID == "" {
					continue
				}
				hs.resolved = v2
				saveResolved = false
				return &hs.resolved.Metadata, nil
			}
			if hs.src.Checksum != "" && len(vals) > 0 {
				return nil, errors.Errorf("digest mismatch for %s: %s (expected: %s)", urlutil.RedactCredentials(hs.src.URL), vals[0], hs.src.Checksum)
			}
		}
	}

	// look up metadata(previously stored headers) for that URL
	mds, err := searchHTTPURLDigest(ctx, hs.cache, uh)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to search metadata for %s", uh)
	}

	req, err := hs.newHTTPRequest(ctx, g)
	if err != nil {
		return nil, err
	}
	m := map[string]cacheRefMetadata{}

	// If we request a single ETag in 'If-None-Match', some servers omit the
	// unambiguous ETag in their response.
	// See: https://github.com/moby/buildkit/issues/905
	var onlyETag string

	if len(mds) > 0 {
		for _, md := range mds {
			// if metaDigest := getMetaDigest(si); metaDigest == hs.formatCacheKey("") {
			if etag := md.getETag(); etag != "" {
				if dgst := md.getHTTPChecksum(); dgst != "" {
					// check that ref still exists
					ref, err := hs.cache.Get(ctx, md.ID(), nil)
					if err == nil {
						m[etag] = md
						defer ref.Release(context.WithoutCancel(ctx))
					}
				}
			}
			// }
		}
		if len(m) > 0 {
			etags := make([]string, 0, len(m))
			for t := range m {
				etags = append(etags, t)
			}
			req.Header.Set("If-None-Match", strings.Join(etags, ", "))

			if len(etags) == 1 {
				onlyETag = etags[0]
			}
		}
	}

	client := hs.client(g)

	// Some servers seem to have trouble supporting If-None-Match properly even
	// though they return ETag-s. So first, optionally try a HEAD request with
	// manual ETag value comparison.
	if len(m) > 0 {
		req.Method = "HEAD"
		// we need to add accept-encoding header manually because stdlib only adds it to GET requests
		// some servers will return different etags if Accept-Encoding header is different
		req.Header.Set("Accept-Encoding", "gzip")
		// Best-effort: on any error we fall through to the GET below, which is retried.
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
				respETag := etagValue(resp.Header.Get("ETag"))

				// If a 304 is returned without an ETag and we had only sent one ETag,
				// the response refers to the ETag we asked about.
				if respETag == "" && onlyETag != "" && resp.StatusCode == http.StatusNotModified {
					respETag = onlyETag
				}
				md, ok := m[respETag]
				if ok {
					dgst := md.getHTTPChecksum()
					if dgst != "" {
						var modTime *time.Time
						if modTimeStr := md.getHTTPModTime(); modTimeStr != "" {
							if t, err := http.ParseTime(modTimeStr); err == nil {
								modTime = &t
							}
						}
						resp.Body.Close()
						m := &Metadata{
							Digest:       dgst,
							Filename:     getFileName(hs.src.URL, hs.src.Filename, resp),
							LastModified: modTime,
						}
						if err := hs.validatePinnedChecksum(m.Digest); err != nil {
							return nil, err
						}
						hs.resolved = &metadataWithRef{
							Metadata: *m,
							refID:    md.ID(),
						}
						return m, nil
					}
				}
			}
			resp.Body.Close()
		}
		req.Method = "GET"
		// Unset explicit Accept-Encoding for GET, otherwise the go http library will not
		// transparently decompress the response body when it is gzipped. It will still add
		// this header implicitly when the request is made though.
		req.Header.Del("Accept-Encoding")
	}

	resp, err := doRequest(ctx, client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, errors.Errorf("invalid response status %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotModified {
		respETag := etagValue(resp.Header.Get("ETag"))
		if respETag == "" && onlyETag != "" {
			respETag = onlyETag

			// Set the missing ETag header on the response so that it's available
			// to .save()
			resp.Header.Set("ETag", onlyETag)
		}
		md, ok := m[respETag]
		if !ok {
			return nil, errors.Errorf("invalid not-modified ETag: %v", respETag)
		}
		dgst := md.getHTTPChecksum()
		if dgst == "" {
			return nil, errors.Errorf("invalid metadata change")
		}

		var modTime *time.Time
		if modTimeStr := md.getHTTPModTime(); modTimeStr != "" {
			if t, err := http.ParseTime(modTimeStr); err == nil {
				modTime = &t
			}
		}
		m := &Metadata{
			Digest:       dgst,
			Filename:     getFileName(hs.src.URL, hs.src.Filename, resp),
			LastModified: modTime,
		}
		if err := hs.validatePinnedChecksum(m.Digest); err != nil {
			return nil, err
		}
		hs.resolved = &metadataWithRef{
			Metadata: *m,
			refID:    md.ID(),
		}
		return m, nil
	}

	ref, dgst, err := hs.save(ctx, resp, g)
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		return ref.Release(context.TODO())
	}
	if jobCtx != nil {
		if err := jobCtx.Cleanup(cleanup); err != nil {
			_ = cleanup()
			return nil, err
		}
	} else {
		cleanup()
	}

	var modTime *time.Time
	if modTimeStr := resp.Header.Get("Last-Modified"); modTimeStr != "" {
		if t, err := http.ParseTime(modTimeStr); err == nil {
			modTime = &t
		}
	}
	if err := hs.validatePinnedChecksum(dgst); err != nil {
		return nil, err
	}

	out := &Metadata{
		Digest:       dgst,
		Filename:     getFileName(hs.src.URL, hs.src.Filename, resp),
		LastModified: modTime,
	}
	hs.resolved = &metadataWithRef{
		Metadata: *out,
		refID:    ref.ID(),
	}
	return out, nil
}

func (hs *httpSourceHandler) CacheKey(ctx context.Context, jobCtx solver.JobContext, index int) (string, string, solver.CacheOpts, bool, error) {
	md, err := hs.resolveMetadata(ctx, jobCtx, MetadataOpts{})
	if err != nil {
		return "", "", nil, false, err
	}
	if hs.resolved == nil {
		hs.resolved = &metadataWithRef{
			Metadata: *md,
		}
	}
	return hs.formatCacheKey(md.Filename, md.Digest, md.LastModified).String(), md.Digest.String(), nil, true, nil
}

func (hs *httpSourceHandler) validatePinnedChecksum(got digest.Digest) error {
	if hs.src.Checksum == "" || got == hs.src.Checksum {
		return nil
	}
	return errors.Errorf("digest mismatch for %s: %s (expected: %s)", urlutil.RedactCredentials(hs.src.URL), got, hs.src.Checksum)
}

func validateChecksumRequest(req *MetadataChecksumRequest) error {
	if req == nil {
		return nil
	}
	if len(req.Suffix) > maxChecksumSuffixSize {
		return errors.Errorf("http checksum request suffix exceeds max size %d", maxChecksumSuffixSize)
	}
	switch req.Algo {
	case MetadataChecksumAlgoSHA256, MetadataChecksumAlgoSHA384, MetadataChecksumAlgoSHA512:
		return nil
	default:
		return errors.Errorf("unsupported checksum algorithm %d", req.Algo)
	}
}

func validateSignatureVerifyOptions(opt *HTTPSignatureVerifyOptions) error {
	if opt == nil {
		return nil
	}
	hasPubKey := len(opt.PubKey) > 0
	hasSignature := len(opt.Signature) > 0
	if hasPubKey == hasSignature {
		return nil
	}
	return errors.New("http signature verification requires both pubkey and signature")
}

func (hs *httpSourceHandler) handleSignatureVerification(ctx context.Context, jobCtx solver.JobContext, in *Metadata) (*Metadata, error) {
	out := *in
	if hs.src.VerifySignature == nil {
		return &out, nil
	}
	if hs.resolved == nil || hs.resolved.refID == "" {
		md, err := hs.resolveMetadataRef(ctx, jobCtx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to resolve source reference for signature verification")
		}
		out = *md
	}
	if err := hs.verifySignature(ctx, out.Filename, hs.resolved.refID, hs.src.VerifySignature); err != nil {
		return nil, err
	}
	return &out, nil
}

func (hs *httpSourceHandler) handleChecksumRequest(ctx context.Context, jobCtx solver.JobContext, in *Metadata, opt MetadataOpts) (*Metadata, error) {
	out := *in
	if opt.ChecksumReq == nil {
		out.ChecksumResponse = nil
		return &out, nil
	}
	if hs.resolved == nil || hs.resolved.refID == "" {
		md, err := hs.resolveMetadataRef(ctx, jobCtx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to resolve source reference for checksum request")
		}
		out = *md
	}
	resp, err := hs.computeChecksumResponse(ctx, out.Filename, hs.resolved.refID, opt.ChecksumReq)
	if err != nil {
		return nil, err
	}
	out.ChecksumResponse = resp
	return &out, nil
}

func (hs *httpSourceHandler) verifySignature(ctx context.Context, filename, refID string, opts *HTTPSignatureVerifyOptions) error {
	ref, err := hs.cache.Get(ctx, refID, nil)
	if err != nil {
		return errors.Wrap(err, "failed to load http source reference")
	}
	defer ref.Release(context.WithoutCancel(ctx))

	mount, err := ref.Mount(ctx, false, nil)
	if err != nil {
		return errors.Wrap(err, "failed to mount http source reference")
	}
	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return errors.Wrap(err, "failed to mount local http source reference")
	}
	defer lm.Unmount()

	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Wrap(err, "failed to open http source root")
	}
	defer root.Close()

	f, err := root.Open(filepath.Base(filepath.Join("/", filename)))
	if err != nil {
		return errors.Wrap(err, "failed to open http source payload")
	}
	defer f.Close()

	if err := pgpsign.VerifyArmoredDetachedSignature(f, opts.Signature, opts.PubKey, nil); err != nil {
		return errors.Wrap(err, "failed to verify pgp signature")
	}
	return nil
}

func (hs *httpSourceHandler) computeChecksumResponse(ctx context.Context, filename, refID string, req *MetadataChecksumRequest) (*MetadataChecksumResponse, error) {
	algo, err := checksumAlgo(req.Algo)
	if err != nil {
		return nil, err
	}

	ref, err := hs.cache.Get(ctx, refID, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load http source reference")
	}
	defer ref.Release(context.WithoutCancel(ctx))

	mount, err := ref.Mount(ctx, false, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to mount http source reference")
	}
	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return nil, errors.Wrap(err, "failed to mount local http source reference")
	}
	defer lm.Unmount()

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open http source root")
	}
	defer root.Close()

	f, err := root.Open(filepath.Base(filepath.Join("/", filename)))
	if err != nil {
		return nil, errors.Wrap(err, "failed to open http source payload")
	}
	defer f.Close()

	digester := algo.Digester()
	h := digester.Hash()
	if _, err := io.Copy(h, f); err != nil {
		return nil, errors.Wrap(err, "failed to hash http source payload")
	}
	if _, err := h.Write(req.Suffix); err != nil {
		return nil, errors.Wrap(err, "failed to hash checksum suffix")
	}
	return &MetadataChecksumResponse{
		Digest: digester.Digest().String(),
		Suffix: slices.Clone(req.Suffix),
	}, nil
}

func checksumAlgo(algo MetadataChecksumAlgo) (digest.Algorithm, error) {
	switch algo {
	case MetadataChecksumAlgoSHA256:
		return digest.SHA256, nil
	case MetadataChecksumAlgoSHA384:
		return digest.SHA384, nil
	case MetadataChecksumAlgoSHA512:
		return digest.SHA512, nil
	default:
		return "", errors.Errorf("unsupported checksum algorithm %d", algo)
	}
}

func (hs *httpSourceHandler) save(ctx context.Context, resp *http.Response, s session.Group) (ref cache.ImmutableRef, dgst digest.Digest, retErr error) {
	newRef, err := hs.cache.New(ctx, nil, s, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("http url %s", urlutil.RedactCredentials(hs.src.URL))))
	if err != nil {
		return nil, "", err
	}

	releaseRef := func() {
		newRef.Release(context.TODO())
	}

	defer func() {
		if retErr != nil && newRef != nil {
			releaseRef()
		}
	}()

	mount, err := newRef.Mount(ctx, false, s)
	if err != nil {
		return nil, "", err
	}

	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return nil, "", err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()
	perm := 0600
	if hs.src.Perm != 0 {
		perm = hs.src.Perm
	}

	name := getFileName(hs.src.URL, hs.src.Filename, resp)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(perm))
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	h := sha256.New()

	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return nil, "", err
	}

	if err := f.Close(); err != nil {
		return nil, "", err
	}
	f = nil

	uid := hs.src.UID
	gid := hs.src.GID
	if idmap := mount.IdentityMapping(); idmap != nil {
		uid, gid, err = idmap.ToHost(uid, gid)
		if err != nil {
			return nil, "", err
		}
	}

	if gid != 0 || uid != 0 {
		if err := root.Chown(name, uid, gid); err != nil {
			return nil, "", err
		}
	}

	mTime := time.Unix(0, 0)
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod != "" {
		if parsedMTime, err := http.ParseTime(lastMod); err == nil {
			mTime = parsedMTime
		}
	}

	if err := root.Chtimes(name, mTime, mTime); err != nil {
		return nil, "", err
	}

	lm.Unmount()
	lm = nil

	ref, err = newRef.Commit(ctx)
	if err != nil {
		return nil, "", err
	}
	newRef = nil
	md := cacheRefMetadata{ref}

	dgst = digest.NewDigest(digest.SHA256, h)

	if respETag := resp.Header.Get("ETag"); respETag != "" {
		respETag = etagValue(respETag)
		if err := md.setETag(respETag); err != nil {
			return nil, "", err
		}
		uh, err := hs.urlHash()
		if err != nil {
			return nil, "", err
		}
		if err := md.setHTTPChecksum(uh, dgst); err != nil {
			return nil, "", err
		}
	}

	if modTime := resp.Header.Get("Last-Modified"); modTime != "" {
		if err := md.setHTTPModTime(modTime); err != nil {
			return nil, "", err
		}
	}

	return ref, dgst, nil
}

func (hs *httpSourceHandler) Snapshot(ctx context.Context, jobCtx solver.JobContext) (cache.ImmutableRef, error) {
	refID := ""
	if hs.resolved != nil && hs.resolved.refID != "" {
		refID = hs.resolved.refID
	} else if jobCtx != nil {
		if rc := jobCtx.ResolverCache(); rc != nil {
			uh, err := hs.urlHash()
			if err != nil {
				return nil, err
			}
			refID, err = hs.snapshotRefIDFromResolverCache(rc, uh)
			if err != nil {
				return nil, err
			}
		}
	}

	if refID != "" {
		ref, err := hs.cache.Get(ctx, refID, nil)
		if err != nil {
			bklog.G(ctx).WithError(err).Warnf("failed to get HTTP snapshot for ref %s (%s)", refID, urlutil.RedactCredentials(hs.src.URL))
		} else {
			return ref, nil
		}
	}

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}

	req, err := hs.newHTTPRequest(ctx, g)
	if err != nil {
		return nil, err
	}

	client := hs.client(g)

	resp, err := doRequest(ctx, client, req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	ref, dgst, err := hs.save(ctx, resp, g)
	if err != nil {
		return nil, err
	}
	if hs.resolved != nil && dgst != hs.resolved.Digest {
		ref.Release(context.TODO())
		return nil, errors.Errorf("digest mismatch %s: %s", dgst, hs.resolved.Digest)
	}

	return ref, nil
}

func (hs *httpSourceHandler) snapshotRefIDFromResolverCache(rc solver.ResolverCache, uh digest.Digest) (string, error) {
	vals, release, err := rc.Lock(uh)
	if err != nil {
		return "", err
	}
	defer release(nil)

	refID := ""
	for _, v := range vals {
		v2, ok := v.(*metadataWithRef)
		if !ok || v2 == nil {
			return "", errors.Errorf("invalid HTTP resolver cache value: %T", v)
		}
		if hs.src.Checksum != "" && v2.Digest != hs.src.Checksum {
			continue
		}
		if v2.refID != "" {
			hs.resolved = v2
			refID = v2.refID
		}
	}
	if hs.src.Checksum != "" && len(vals) > 0 && refID == "" {
		return "", errors.Errorf("digest mismatch for %s: %s (expected: %s)", urlutil.RedactCredentials(hs.src.URL), vals[0], hs.src.Checksum)
	}
	return refID, nil
}

func (hs *httpSourceHandler) newHTTPRequest(ctx context.Context, g session.Group) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hs.src.URL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", version.UserAgent())
	for _, field := range hs.src.Header {
		req.Header.Set(field.Name, field.Value)
	}

	type authSecret struct {
		name  string
		token bool
	}

	var secretNames []authSecret
	if hs.src.AuthHeaderSecret != "" {
		secretNames = append(secretNames, authSecret{name: hs.src.AuthHeaderSecret})
	} else {
		u, err := url.Parse(hs.src.URL)
		if err == nil {
			secretNames = append(secretNames, authSecret{name: HTTPAuthHeaderSecretPrefix + u.Hostname()})
			secretNames = append(secretNames, authSecret{name: HTTPAuthTokenSecretPrefix + u.Hostname(), token: true})
		}
	}

	for _, secret := range secretNames {
		err := hs.sm.Any(ctx, g, func(ctx context.Context, _ string, caller session.Caller) error {
			dt, err := secrets.GetSecret(ctx, caller, secret.name)
			if err != nil {
				return err
			}
			v := string(dt)
			if secret.token {
				v = "Bearer " + v
			}
			req.Header.Set("Authorization", v)
			return nil
		})
		if err != nil && hs.src.AuthHeaderSecret != "" {
			return nil, errors.Wrapf(err, "failed to retrieve HTTP auth secret %s", hs.src.AuthHeaderSecret)
		}
	}

	return req, nil
}

func getFileName(urlStr, manualFilename string, resp *http.Response) string {
	if manualFilename != "" {
		return pathutil.SafeFileName(manualFilename)
	}
	if resp != nil {
		if contentDisposition := resp.Header.Get("Content-Disposition"); contentDisposition != "" {
			if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
				if params["filename"] != "" && !strings.HasSuffix(params["filename"], "/") {
					if filename := filepath.Base(filepath.FromSlash(params["filename"])); filename != "" {
						return pathutil.SafeFileName(filename)
					}
				}
			}
		}
	}
	u, err := url.Parse(urlStr)
	if err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			return pathutil.SafeFileName(base)
		}
	}
	return pathutil.SafeFileName("")
}

func searchHTTPURLDigest(ctx context.Context, store cache.MetadataStore, dgst digest.Digest) ([]cacheRefMetadata, error) {
	var results []cacheRefMetadata
	mds, err := store.Search(ctx, string(dgst), false)
	if err != nil {
		return nil, err
	}
	for _, md := range mds {
		results = append(results, cacheRefMetadata{md})
	}
	return results, nil
}

type cacheRefMetadata struct {
	cache.RefMetadata
}

const (
	keyHTTPChecksum = "http.checksum"
	keyETag         = "etag"
	keyModTime      = "http.modtime"
)

func (md cacheRefMetadata) getHTTPChecksum() digest.Digest {
	return digest.Digest(md.GetString(keyHTTPChecksum))
}

func (md cacheRefMetadata) setHTTPChecksum(urlDgst digest.Digest, d digest.Digest) error {
	return md.SetString(keyHTTPChecksum, d.String(), urlDgst.String())
}

func (md cacheRefMetadata) getETag() string {
	return md.GetString(keyETag)
}

func (md cacheRefMetadata) setETag(s string) error {
	return md.SetString(keyETag, s, "")
}

func (md cacheRefMetadata) getHTTPModTime() string {
	return md.GetString(keyModTime)
}

func (md cacheRefMetadata) setHTTPModTime(s string) error {
	return md.SetString(keyModTime, s, "")
}

func etagValue(v string) string {
	// remove weak for direct comparison
	return strings.TrimPrefix(v, "W/")
}

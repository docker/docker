package grpcclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gogo/googleapis/google/rpc"
	"github.com/moby/buildkit/frontend/gateway/client"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	fstypes "github.com/tonistiigi/fsutil/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

const frontendPrefix = "BUILDKIT_FRONTEND_OPT_"

type GrpcClient interface {
	Run(context.Context, client.BuildFunc) error
}

func New(ctx context.Context, opts map[string]string, session, product string, c pb.LLBBridgeClient, w []client.WorkerInfo) (GrpcClient, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.Ping(ctx, &pb.PingRequest{})
	if err != nil {
		return nil, err
	}

	if resp.FrontendAPICaps == nil {
		resp.FrontendAPICaps = defaultCaps()
	}

	if resp.LLBCaps == nil {
		resp.LLBCaps = defaultLLBCaps()
	}

	return &grpcClient{
		client:    c,
		opts:      opts,
		sessionID: session,
		workers:   w,
		product:   product,
		caps:      pb.Caps.CapSet(resp.FrontendAPICaps),
		llbCaps:   opspb.Caps.CapSet(resp.LLBCaps),
		requests:  map[string]*pb.SolveRequest{},
	}, nil
}

func current() (GrpcClient, error) {
	if ep := product(); ep != "" {
		apicaps.ExportedProduct = ep
	}

	ctx, conn, err := grpcClientConn(context.Background())
	if err != nil {
		return nil, err
	}

	return New(ctx, opts(), sessionID(), product(), pb.NewLLBBridgeClient(conn), workers())
}

func convertRef(ref client.Reference) (string, error) {
	if ref == nil {
		return "", nil
	}
	r, ok := ref.(*reference)
	if !ok {
		return "", errors.Errorf("invalid return reference type %T", ref)
	}
	return r.id, nil
}

func RunFromEnvironment(ctx context.Context, f client.BuildFunc) error {
	client, err := current()
	if err != nil {
		return errors.Wrapf(err, "failed to initialize client from environment")
	}
	return client.Run(ctx, f)
}

func (c *grpcClient) Run(ctx context.Context, f client.BuildFunc) (retError error) {
	export := c.caps.Supports(pb.CapReturnResult) == nil

	var (
		res *client.Result
		err error
	)
	if export {
		defer func() {
			req := &pb.ReturnRequest{}
			if retError == nil {
				if res == nil {
					res = &client.Result{}
				}
				pbRes := &pb.Result{
					Metadata: res.Metadata,
				}
				if res.Refs != nil {
					m := map[string]string{}
					for k, r := range res.Refs {
						id, err := convertRef(r)
						if err != nil {
							retError = err
							continue
						}
						m[k] = id
					}
					pbRes.Result = &pb.Result_Refs{Refs: &pb.RefMap{Refs: m}}
				} else {
					id, err := convertRef(res.Ref)
					if err != nil {
						retError = err
					} else {
						pbRes.Result = &pb.Result_Ref{Ref: id}
					}
				}
				if retError == nil {
					req.Result = pbRes
				}
			}
			if retError != nil {
				st, _ := status.FromError(retError)
				stp := st.Proto()
				req.Error = &rpc.Status{
					Code:    stp.Code,
					Message: stp.Message,
					// Details: stp.Details,
				}
			}
			if _, err := c.client.Return(ctx, req); err != nil && retError == nil {
				retError = err
			}
		}()
	}

	if res, err = f(ctx, c); err != nil {
		return err
	}

	if err := c.caps.Supports(pb.CapReturnMap); len(res.Refs) > 1 && err != nil {
		return err
	}

	if !export {
		exportedAttrBytes, err := json.Marshal(res.Metadata)
		if err != nil {
			return errors.Wrapf(err, "failed to marshal return metadata")
		}

		req, err := c.requestForRef(res.Ref)
		if err != nil {
			return errors.Wrapf(err, "failed to find return ref")
		}

		req.Final = true
		req.ExporterAttr = exportedAttrBytes

		if _, err := c.client.Solve(ctx, req); err != nil {
			return errors.Wrapf(err, "failed to solve")
		}
	}

	return nil
}

// defaultCaps returns the capabilities that were implemented when capabilities
// support was added. This list is frozen and should never be changed.
func defaultCaps() []apicaps.PBCap {
	return []apicaps.PBCap{
		{ID: string(pb.CapSolveBase), Enabled: true},
		{ID: string(pb.CapSolveInlineReturn), Enabled: true},
		{ID: string(pb.CapResolveImage), Enabled: true},
		{ID: string(pb.CapReadFile), Enabled: true},
	}
}

// defaultLLBCaps returns the LLB capabilities that were implemented when capabilities
// support was added. This list is frozen and should never be changed.
func defaultLLBCaps() []apicaps.PBCap {
	return []apicaps.PBCap{
		{ID: string(opspb.CapSourceImage), Enabled: true},
		{ID: string(opspb.CapSourceLocal), Enabled: true},
		{ID: string(opspb.CapSourceLocalUnique), Enabled: true},
		{ID: string(opspb.CapSourceLocalSessionID), Enabled: true},
		{ID: string(opspb.CapSourceLocalIncludePatterns), Enabled: true},
		{ID: string(opspb.CapSourceLocalFollowPaths), Enabled: true},
		{ID: string(opspb.CapSourceLocalExcludePatterns), Enabled: true},
		{ID: string(opspb.CapSourceLocalSharedKeyHint), Enabled: true},
		{ID: string(opspb.CapSourceGit), Enabled: true},
		{ID: string(opspb.CapSourceGitKeepDir), Enabled: true},
		{ID: string(opspb.CapSourceGitFullURL), Enabled: true},
		{ID: string(opspb.CapSourceHTTP), Enabled: true},
		{ID: string(opspb.CapSourceHTTPChecksum), Enabled: true},
		{ID: string(opspb.CapSourceHTTPPerm), Enabled: true},
		{ID: string(opspb.CapSourceHTTPUIDGID), Enabled: true},
		{ID: string(opspb.CapBuildOpLLBFileName), Enabled: true},
		{ID: string(opspb.CapExecMetaBase), Enabled: true},
		{ID: string(opspb.CapExecMetaProxy), Enabled: true},
		{ID: string(opspb.CapExecMountBind), Enabled: true},
		{ID: string(opspb.CapExecMountCache), Enabled: true},
		{ID: string(opspb.CapExecMountCacheSharing), Enabled: true},
		{ID: string(opspb.CapExecMountSelector), Enabled: true},
		{ID: string(opspb.CapExecMountTmpfs), Enabled: true},
		{ID: string(opspb.CapExecMountSecret), Enabled: true},
		{ID: string(opspb.CapConstraints), Enabled: true},
		{ID: string(opspb.CapPlatform), Enabled: true},
		{ID: string(opspb.CapMetaIgnoreCache), Enabled: true},
		{ID: string(opspb.CapMetaDescription), Enabled: true},
		{ID: string(opspb.CapMetaExportCache), Enabled: true},
	}
}

type grpcClient struct {
	client    pb.LLBBridgeClient
	opts      map[string]string
	sessionID string
	product   string
	workers   []client.WorkerInfo
	caps      apicaps.CapSet
	llbCaps   apicaps.CapSet
	requests  map[string]*pb.SolveRequest
}

func (c *grpcClient) requestForRef(ref client.Reference) (*pb.SolveRequest, error) {
	emptyReq := &pb.SolveRequest{
		Definition: &opspb.Definition{},
	}
	if ref == nil {
		return emptyReq, nil
	}
	r, ok := ref.(*reference)
	if !ok {
		return nil, errors.Errorf("return reference has invalid type %T", ref)
	}
	if r.id == "" {
		return emptyReq, nil
	}
	req, ok := c.requests[r.id]
	if !ok {
		return nil, errors.Errorf("did not find request for return reference %s", r.id)
	}
	return req, nil
}

func (c *grpcClient) Solve(ctx context.Context, creq client.SolveRequest) (*client.Result, error) {
	if creq.Definition != nil {
		for _, md := range creq.Definition.Metadata {
			for cap := range md.Caps {
				if err := c.llbCaps.Supports(cap); err != nil {
					return nil, err
				}
			}
		}
	}
	var (
		// old API
		legacyRegistryCacheImports []string
		// new API (CapImportCaches)
		cacheImports []*pb.CacheOptionsEntry
	)
	supportCapImportCaches := c.caps.Supports(pb.CapImportCaches) == nil
	for _, im := range creq.CacheImports {
		if !supportCapImportCaches && im.Type == "registry" {
			legacyRegistryCacheImports = append(legacyRegistryCacheImports, im.Attrs["ref"])
		} else {
			cacheImports = append(cacheImports, &pb.CacheOptionsEntry{
				Type:  im.Type,
				Attrs: im.Attrs,
			})
		}
	}

	req := &pb.SolveRequest{
		Definition:        creq.Definition,
		Frontend:          creq.Frontend,
		FrontendOpt:       creq.FrontendOpt,
		AllowResultReturn: true,
		// old API
		ImportCacheRefsDeprecated: legacyRegistryCacheImports,
		// new API
		CacheImports: cacheImports,
	}

	// backwards compatibility with inline return
	if c.caps.Supports(pb.CapReturnResult) != nil {
		req.ExporterAttr = []byte("{}")
	}

	resp, err := c.client.Solve(ctx, req)
	if err != nil {
		return nil, err
	}

	res := &client.Result{}

	if resp.Result == nil {
		if id := resp.Ref; id != "" {
			c.requests[id] = req
		}
		res.SetRef(&reference{id: resp.Ref, c: c})
	} else {
		res.Metadata = resp.Result.Metadata
		switch pbRes := resp.Result.Result.(type) {
		case *pb.Result_Ref:
			if id := pbRes.Ref; id != "" {
				res.SetRef(&reference{id: id, c: c})
			}
		case *pb.Result_Refs:
			for k, v := range pbRes.Refs.Refs {
				ref := &reference{id: v, c: c}
				if v == "" {
					ref = nil
				}
				res.AddRef(k, ref)
			}
		}
	}

	return res, nil
}

func (c *grpcClient) ResolveImageConfig(ctx context.Context, ref string, opt client.ResolveImageConfigOpt) (digest.Digest, []byte, error) {
	var p *opspb.Platform
	if platform := opt.Platform; platform != nil {
		p = &opspb.Platform{
			OS:           platform.OS,
			Architecture: platform.Architecture,
			Variant:      platform.Variant,
			OSVersion:    platform.OSVersion,
			OSFeatures:   platform.OSFeatures,
		}
	}
	resp, err := c.client.ResolveImageConfig(ctx, &pb.ResolveImageConfigRequest{Ref: ref, Platform: p, ResolveMode: opt.ResolveMode, LogName: opt.LogName})
	if err != nil {
		return "", nil, err
	}
	return resp.Digest, resp.Config, nil
}

func (c *grpcClient) BuildOpts() client.BuildOpts {
	return client.BuildOpts{
		Opts:      c.opts,
		SessionID: c.sessionID,
		Workers:   c.workers,
		Product:   c.product,
		LLBCaps:   c.llbCaps,
		Caps:      c.caps,
	}
}

type reference struct {
	id string
	c  *grpcClient
}

func (r *reference) ReadFile(ctx context.Context, req client.ReadRequest) ([]byte, error) {
	rfr := &pb.ReadFileRequest{FilePath: req.Filename, Ref: r.id}
	if r := req.Range; r != nil {
		rfr.Range = &pb.FileRange{
			Offset: int64(r.Offset),
			Length: int64(r.Length),
		}
	}
	resp, err := r.c.client.ReadFile(ctx, rfr)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (r *reference) ReadDir(ctx context.Context, req client.ReadDirRequest) ([]*fstypes.Stat, error) {
	if err := r.c.caps.Supports(pb.CapReadDir); err != nil {
		return nil, err
	}
	rdr := &pb.ReadDirRequest{
		DirPath:        req.Path,
		IncludePattern: req.IncludePattern,
		Ref:            r.id,
	}
	resp, err := r.c.client.ReadDir(ctx, rdr)
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

func (r *reference) StatFile(ctx context.Context, req client.StatRequest) (*fstypes.Stat, error) {
	if err := r.c.caps.Supports(pb.CapStatFile); err != nil {
		return nil, err
	}
	rdr := &pb.StatFileRequest{
		Path: req.Path,
		Ref:  r.id,
	}
	resp, err := r.c.client.StatFile(ctx, rdr)
	if err != nil {
		return nil, err
	}
	return resp.Stat, nil
}

func grpcClientConn(ctx context.Context) (context.Context, *grpc.ClientConn, error) {
	dialOpt := grpc.WithDialer(func(addr string, d time.Duration) (net.Conn, error) {
		return stdioConn(), nil
	})

	cc, err := grpc.DialContext(ctx, "", dialOpt, grpc.WithInsecure())
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create grpc client")
	}

	ctx, cancel := context.WithCancel(ctx)
	_ = cancel
	// go monitorHealth(ctx, cc, cancel)

	return ctx, cc, nil
}

func stdioConn() net.Conn {
	return &conn{os.Stdin, os.Stdout, os.Stdout}
}

type conn struct {
	io.Reader
	io.Writer
	io.Closer
}

func (s *conn) LocalAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) RemoteAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) SetDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetReadDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type dummyAddr struct {
}

func (d dummyAddr) Network() string {
	return "pipe"
}

func (d dummyAddr) String() string {
	return "localhost"
}

func opts() map[string]string {
	opts := map[string]string{}
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		k := parts[0]
		v := ""
		if len(parts) == 2 {
			v = parts[1]
		}
		if !strings.HasPrefix(k, frontendPrefix) {
			continue
		}
		parts = strings.SplitN(v, "=", 2)
		v = ""
		if len(parts) == 2 {
			v = parts[1]
		}
		opts[parts[0]] = v
	}
	return opts
}

func sessionID() string {
	return os.Getenv("BUILDKIT_SESSION_ID")
}

func workers() []client.WorkerInfo {
	var c []client.WorkerInfo
	if err := json.Unmarshal([]byte(os.Getenv("BUILDKIT_WORKERS")), &c); err != nil {
		return nil
	}
	return c
}

func product() string {
	return os.Getenv("BUILDKIT_EXPORTEDPRODUCT")
}

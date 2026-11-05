package grpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/defaults"
	distreference "github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/gateway/client"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/sys/signal"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const frontendPrefix = "BUILDKIT_FRONTEND_OPT_"

type GrpcClient interface {
	client.Client
	Run(context.Context, client.BuildFunc) error
}

func New(ctx context.Context, opts map[string]string, session, product string, c pb.LLBBridgeClient, w []client.WorkerInfo) (GrpcClient, error) {
	pingCtx, pingCancel := context.WithCancelCause(ctx)
	pingCtx, _ = context.WithTimeoutCause(pingCtx, 15*time.Second, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
	defer pingCancel(errors.WithStack(context.Canceled))
	resp, err := c.Ping(pingCtx, &pb.PingRequest{})
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
		execMsgs:  newMessageForwarder(ctx, c),
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

func convertRef(ref client.Reference) (*pb.Ref, error) {
	if ref == nil {
		return &pb.Ref{}, nil
	}
	r, ok := ref.(*reference)
	if !ok {
		return nil, errors.Errorf("invalid return reference type %T", ref)
	}
	// Return only needs the ID of the reference held by the server.
	return &pb.Ref{Id: r.id}, nil
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
					res = client.NewResult()
				}
				pbRes := &pb.Result{
					Metadata: res.Metadata,
				}
				if res.Refs != nil {
					if c.caps.Supports(pb.CapProtoRefArray) == nil {
						m := map[string]*pb.Ref{}
						for k, r := range res.Refs {
							pbRef, err := convertRef(r)
							if err != nil {
								retError = err
								continue
							}
							m[k] = pbRef
						}
						pbRes.Result = &pb.Result_Refs{Refs: &pb.RefMap{Refs: m}}
					} else {
						// Server doesn't support the new wire format for refs, so we construct
						// a deprecated result ref map.
						m := map[string]string{}
						for k, r := range res.Refs {
							pbRef, err := convertRef(r)
							if err != nil {
								retError = err
								continue
							}
							m[k] = pbRef.Id
						}
						pbRes.Result = &pb.Result_RefsDeprecated{RefsDeprecated: &pb.RefMapDeprecated{Refs: m}}
					}
				} else {
					pbRef, err := convertRef(res.Ref)
					if err != nil {
						retError = err
					} else {
						if c.caps.Supports(pb.CapProtoRefArray) == nil {
							pbRes.Result = &pb.Result_Ref{Ref: pbRef}
						} else {
							// Server doesn't support the new wire format for refs, so we construct
							// a deprecated result ref.
							pbRes.Result = &pb.Result_RefDeprecated{RefDeprecated: pbRef.Id}
						}
					}
				}

				if res.Attestations != nil {
					attestations := map[string]*pb.Attestations{}
					for k, as := range res.Attestations {
						for _, a := range as {
							pbAtt, err := client.AttestationToPB(&a)
							if err != nil {
								retError = err
								continue
							}
							pbRef, err := convertRef(a.Ref)
							if err != nil {
								retError = err
								continue
							}
							pbAtt.Ref = pbRef
							if attestations[k] == nil {
								attestations[k] = &pb.Attestations{}
							}
							attestations[k].Attestation = append(attestations[k].Attestation, pbAtt)
						}
					}
					pbRes.Attestations = attestations
				}

				if retError == nil {
					req.Result = pbRes
				}
			}
			if retError != nil {
				st, _ := status.FromError(grpcerrors.ToGRPC(ctx, retError))
				stp := st.Proto()
				req.Error = &spb.Status{
					Code:    stp.Code,
					Message: stp.Message,
					Details: stp.Details,
				}
			}
			if _, err := c.client.Return(ctx, req); err != nil && retError == nil {
				retError = err
			}
		}()
	}

	defer func() {
		err = c.execMsgs.Release()
		if err != nil && retError != nil {
			retError = err
		}
	}()

	if res, err = f(ctx, c); err != nil {
		return err
	}

	if res == nil {
		return nil
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
func defaultCaps() []*apicaps.PBCap {
	return []*apicaps.PBCap{
		{ID: string(pb.CapSolveBase), Enabled: true},
		{ID: string(pb.CapSolveInlineReturn), Enabled: true},
		{ID: string(pb.CapResolveImage), Enabled: true},
		{ID: string(pb.CapReadFile), Enabled: true},
	}
}

// defaultLLBCaps returns the LLB capabilities that were implemented when capabilities
// support was added. This list is frozen and should never be changed.
func defaultLLBCaps() []*apicaps.PBCap {
	return []*apicaps.PBCap{
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
	execMsgs  *messageForwarder
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

func (c *grpcClient) Warn(ctx context.Context, dgst digest.Digest, msg string, opts client.WarnOpts) error {
	_, err := c.client.Warn(ctx, &pb.WarnRequest{
		Digest: string(dgst),
		Level:  int64(opts.Level),
		Short:  []byte(msg),
		Info:   opts.SourceInfo,
		Ranges: opts.Range,
		Detail: opts.Detail,
		Url:    opts.URL,
	})
	return err
}

func (c *grpcClient) Solve(ctx context.Context, creq client.SolveRequest) (res *client.Result, err error) {
	if creq.Definition != nil {
		for _, md := range creq.Definition.Metadata {
			for cap := range md.Caps {
				if err := c.llbCaps.Supports(apicaps.CapID(cap)); err != nil {
					return nil, err
				}
			}
		}
	}
	var cacheImports []*pb.CacheOptionsEntry
	for _, im := range creq.CacheImports {
		cacheImports = append(cacheImports, &pb.CacheOptionsEntry{
			Type:  im.Type,
			Attrs: im.Attrs,
		})
	}

	// these options are added by go client in solve()
	if _, ok := creq.FrontendOpt["cache-imports"]; !ok {
		if v, ok := c.opts["cache-imports"]; ok {
			if creq.FrontendOpt == nil {
				creq.FrontendOpt = map[string]string{}
			}
			creq.FrontendOpt["cache-imports"] = v
		}
	}
	if _, ok := creq.FrontendOpt["cache-from"]; !ok {
		if v, ok := c.opts["cache-from"]; ok {
			if creq.FrontendOpt == nil {
				creq.FrontendOpt = map[string]string{}
			}
			creq.FrontendOpt["cache-from"] = v
		}
	}

	req := &pb.SolveRequest{
		Definition:          creq.Definition,
		Frontend:            creq.Frontend,
		FrontendOpt:         creq.FrontendOpt,
		FrontendInputs:      creq.FrontendInputs,
		AllowResultReturn:   true,
		AllowResultArrayRef: true,
		CacheImports:        cacheImports,
		SourcePolicies:      creq.SourcePolicies,
	}

	// backwards compatibility with inline return
	if c.caps.Supports(pb.CapReturnResult) != nil {
		req.ExporterAttr = []byte("{}")
	}

	if creq.Evaluate {
		if c.caps.Supports(pb.CapGatewayEvaluateSolve) == nil {
			req.Evaluate = creq.Evaluate
		} else {
			// If evaluate is not supported, fallback to running Stat(".") in
			// order to trigger an evaluation of the result.
			defer func() {
				if res == nil {
					return
				}
				err = res.EachRef(func(ref client.Reference) error {
					_, err := ref.StatFile(ctx, client.StatRequest{Path: "."})
					return err
				})
			}()
		}
	}

	resp, err := c.client.Solve(ctx, req)
	if err != nil {
		return nil, err
	}

	res = client.NewResult()
	if resp.Result == nil {
		if id := resp.Ref; id != "" {
			c.requests[id] = req
		}
		res.SetRef(&reference{id: resp.Ref, c: c})
	} else {
		res.Metadata = resp.Result.Metadata
		switch pbRes := resp.Result.Result.(type) {
		case *pb.Result_RefDeprecated:
			if id := pbRes.RefDeprecated; id != "" {
				res.SetRef(&reference{id: id, c: c})
			}
		case *pb.Result_RefsDeprecated:
			for k, v := range pbRes.RefsDeprecated.Refs {
				var ref client.Reference
				if v != "" {
					ref = &reference{id: v, c: c}
				}
				res.AddRef(k, ref)
			}
		case *pb.Result_Ref:
			if pbRes.Ref.Id != "" {
				res.SetRef(newReference(c, pbRes.Ref))
			}
		case *pb.Result_Refs:
			for k, v := range pbRes.Refs.Refs {
				var ref client.Reference
				if v.Id != "" {
					ref = newReference(c, v)
				}
				res.AddRef(k, ref)
			}
		}

		if resp.Result.Attestations != nil {
			for p, as := range resp.Result.Attestations {
				for _, a := range as.Attestation {
					att, err := client.AttestationFromPB[client.Reference](a)
					if err != nil {
						return nil, err
					}
					if a.Ref.Id != "" {
						att.Ref = newReference(c, a.Ref)
					}
					res.AddAttestation(p, *att)
				}
			}
		}
	}

	return res, nil
}

func (c *grpcClient) ResolveSourceMetadata(ctx context.Context, op *opspb.SourceOp, opt sourceresolver.Opt) (*sourceresolver.MetaResponse, error) {
	requiresImageAttestationResolve := opt.ImageOpt != nil && (opt.ImageOpt.AttestationChain || len(opt.ImageOpt.ResolveAttestations) > 0)
	requiresHTTPChecksumRequest := opt.HTTPOpt != nil && opt.HTTPOpt.ChecksumReq != nil

	if c.caps.Supports(pb.CapSourceMetaResolver) != nil {
		if requiresImageAttestationResolve {
			return nil, errors.New("image attestation resolution requires source metadata resolver support")
		}
		var ref string
		if v, ok := strings.CutPrefix(op.Identifier, "docker-image://"); ok {
			ref = v
		} else if v, ok := strings.CutPrefix(op.Identifier, "oci-layout://"); ok {
			ref = v
		} else {
			if requiresHTTPChecksumRequest {
				return nil, errors.New("http checksum request requires source metadata resolver support")
			}
			return &sourceresolver.MetaResponse{Op: op}, nil
		}
		retRef, dgst, config, err := c.ResolveImageConfig(ctx, ref, opt)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(op.Identifier, "docker-image://") {
			op.Identifier = "docker-image://" + retRef
		} else if strings.HasPrefix(op.Identifier, "oci-layout://") {
			op.Identifier = "oci-layout://" + retRef
		}

		return &sourceresolver.MetaResponse{
			Op: op,
			Image: &sourceresolver.ResolveImageResponse{
				Digest: dgst,
				Config: config,
			},
		}, nil
	}

	if requiresImageAttestationResolve {
		if err := c.caps.Supports(pb.CapSourceMetaResolverImageAttestations); err != nil {
			return nil, errors.Wrap(err, "image attestation resolution requires additional source metadata resolver support")
		}
	}
	if requiresHTTPChecksumRequest {
		if err := c.caps.Supports(pb.CapSourceMetaResolverHTTPChecksumRequest); err != nil {
			return nil, errors.Wrap(err, "http checksum request requires additional source metadata resolver support")
		}
	}

	var platform *ocispecs.Platform
	if imgOpt := opt.ImageOpt; imgOpt != nil && imgOpt.Platform != nil {
		platform = imgOpt.Platform
	} else if ociOpt := opt.OCILayoutOpt; ociOpt != nil && ociOpt.Platform != nil {
		platform = ociOpt.Platform
	}
	var p *opspb.Platform
	if platform != nil {
		p = &opspb.Platform{
			OS:           platform.OS,
			Architecture: platform.Architecture,
			Variant:      platform.Variant,
			OSVersion:    platform.OSVersion,
			OSFeatures:   platform.OSFeatures,
		}
	}

	req := &pb.ResolveSourceMetaRequest{
		Source:         op,
		Platform:       p,
		LogName:        opt.LogName,
		SourcePolicies: opt.SourcePolicies,
	}
	if opt.ImageOpt != nil {
		attestationChain := opt.ImageOpt.AttestationChain
		if len(opt.ImageOpt.ResolveAttestations) > 0 {
			attestationChain = true
		}
		req.ResolveMode = opt.ImageOpt.ResolveMode
		req.Image = &pb.ResolveSourceImageRequest{
			NoConfig:            opt.ImageOpt.NoConfig,
			AttestationChain:    attestationChain,
			ResolveAttestations: slices.Clone(opt.ImageOpt.ResolveAttestations),
		}
	}

	if opt.GitOpt != nil {
		req.Git = &pb.ResolveSourceGitRequest{
			ReturnObject: opt.GitOpt.ReturnObject,
		}
	}
	if requiresHTTPChecksumRequest {
		algo, err := toPBHTTPChecksumAlgo(opt.HTTPOpt.ChecksumReq.Algo)
		if err != nil {
			return nil, err
		}
		req.HTTP = &pb.ResolveSourceHTTPRequest{
			ChecksumRequest: &pb.ChecksumRequest{
				Algo:   algo,
				Suffix: slices.Clone(opt.HTTPOpt.ChecksumReq.Suffix),
			},
		}
	}

	resp, err := c.client.ResolveSourceMeta(ctx, req)
	if err != nil {
		return nil, err
	}

	r := &sourceresolver.MetaResponse{
		Op: resp.Source,
	}
	if resp.Image != nil {
		r.Image = imgResponseFromPB(resp.Image)
	}
	if resp.Git != nil {
		r.Git = &sourceresolver.ResolveGitResponse{
			Checksum:       resp.Git.Checksum,
			Ref:            resp.Git.Ref,
			CommitChecksum: resp.Git.CommitChecksum,
			CommitObject:   resp.Git.CommitObject,
			TagObject:      resp.Git.TagObject,
		}
	}
	if resp.HTTP != nil {
		dgst, err := digest.Parse(resp.HTTP.Checksum)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid http checksum digest %q", resp.HTTP.Checksum)
		}

		r.HTTP = &sourceresolver.ResolveHTTPResponse{
			Digest:   dgst,
			Filename: resp.HTTP.Filename,
		}
		if resp.HTTP.LastModified != nil {
			tm := resp.HTTP.LastModified.AsTime()
			r.HTTP.LastModified = &tm
		}
		if resp.HTTP.ChecksumResponse != nil {
			r.HTTP.ChecksumResponse = &sourceresolver.ResolveHTTPChecksumResponse{
				Digest: resp.HTTP.ChecksumResponse.Digest,
				Suffix: slices.Clone(resp.HTTP.ChecksumResponse.Suffix),
			}
		}
	}
	if requiresHTTPChecksumRequest {
		if resp.HTTP == nil || resp.HTTP.ChecksumResponse == nil {
			return nil, errors.New("http checksum request was sent but response did not include checksum response")
		}
	}
	return r, nil
}

func toPBHTTPChecksumAlgo(in sourceresolver.ResolveHTTPChecksumAlgo) (pb.ChecksumRequest_ChecksumAlgo, error) {
	switch in {
	case sourceresolver.ResolveHTTPChecksumAlgoSHA256:
		return pb.ChecksumRequest_CHECKSUM_ALGO_SHA256, nil
	case sourceresolver.ResolveHTTPChecksumAlgoSHA384:
		return pb.ChecksumRequest_CHECKSUM_ALGO_SHA384, nil
	case sourceresolver.ResolveHTTPChecksumAlgoSHA512:
		return pb.ChecksumRequest_CHECKSUM_ALGO_SHA512, nil
	default:
		return pb.ChecksumRequest_CHECKSUM_ALGO_SHA256, errors.Errorf("invalid http checksum algorithm: %d", in)
	}
}

func imgResponseFromPB(resp *pb.ResolveSourceImageResponse) *sourceresolver.ResolveImageResponse {
	r := &sourceresolver.ResolveImageResponse{
		Digest: digest.Digest(resp.Digest),
		Config: resp.Config,
	}
	if resp.AttestationChain != nil {
		ac := &sourceresolver.AttestationChain{
			Root:                digest.Digest(resp.AttestationChain.Root),
			ImageManifest:       digest.Digest(resp.AttestationChain.ImageManifest),
			AttestationManifest: digest.Digest(resp.AttestationChain.AttestationManifest),
			SignatureManifests:  []digest.Digest{},
			Blobs:               map[digest.Digest]sourceresolver.Blob{},
		}
		for _, sm := range resp.AttestationChain.SignatureManifests {
			ac.SignatureManifests = append(ac.SignatureManifests, digest.Digest(sm))
		}
		for k, v := range resp.AttestationChain.Blobs {
			ac.Blobs[digest.Digest(k)] = sourceresolver.Blob{
				Descriptor: descriptorFromPB(v.GetDescriptor_()),
				Data:       v.Data,
			}
		}
		r.AttestationChain = ac
	}
	return r
}

func descriptorFromPB(pbDesc *pb.Descriptor) ocispecs.Descriptor {
	if pbDesc == nil {
		return ocispecs.Descriptor{}
	}
	return ocispecs.Descriptor{
		MediaType:   pbDesc.GetMediaType(),
		Size:        pbDesc.GetSize(),
		Digest:      digest.Digest(pbDesc.GetDigest()),
		Annotations: maps.Clone(pbDesc.GetAnnotations()),
	}
}

func (c *grpcClient) resolveImageConfigViaSourceMetadata(ctx context.Context, ref string, opt sourceresolver.Opt, p *opspb.Platform) (string, digest.Digest, []byte, error) {
	op := &opspb.SourceOp{
		Identifier: "docker-image://" + ref,
	}
	if opt.OCILayoutOpt != nil {
		named, err := distreference.ParseNormalizedNamed(ref)
		if err != nil {
			return "", "", nil, err
		}
		op.Identifier = "oci-layout://" + named.String()
		op.Attrs = map[string]string{
			opspb.AttrOCILayoutSessionID: opt.OCILayoutOpt.Store.SessionID,
			opspb.AttrOCILayoutStoreID:   opt.OCILayoutOpt.Store.StoreID,
		}
	}

	req := &pb.ResolveSourceMetaRequest{
		Source:         op,
		Platform:       p,
		LogName:        opt.LogName,
		SourcePolicies: opt.SourcePolicies,
	}
	resp, err := c.client.ResolveSourceMeta(ctx, req)
	if err != nil {
		return "", "", nil, err
	}
	if resp.Image == nil {
		return "", "", nil, &imageutil.ResolveToNonImageError{Ref: ref, Updated: resp.Source.Identifier}
	}
	ref = strings.TrimPrefix(resp.Source.Identifier, "docker-image://")
	ref = strings.TrimPrefix(ref, "oci-layout://")
	return ref, digest.Digest(resp.Image.Digest), resp.Image.Config, nil
}

func (c *grpcClient) ResolveImageConfig(ctx context.Context, ref string, opt sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	var platform *ocispecs.Platform
	if imgOpt := opt.ImageOpt; imgOpt != nil && imgOpt.Platform != nil {
		platform = imgOpt.Platform
	} else if ociOpt := opt.OCILayoutOpt; ociOpt != nil && ociOpt.Platform != nil {
		platform = ociOpt.Platform
	}

	var p *opspb.Platform
	if platform != nil {
		p = &opspb.Platform{
			OS:           platform.OS,
			Architecture: platform.Architecture,
			Variant:      platform.Variant,
			OSVersion:    platform.OSVersion,
			OSFeatures:   platform.OSFeatures,
		}
	}

	if c.caps.Supports(pb.CapSourceMetaResolver) == nil {
		return c.resolveImageConfigViaSourceMetadata(ctx, ref, opt, p)
	}

	req := &pb.ResolveImageConfigRequest{
		Ref:            ref,
		LogName:        opt.LogName,
		SourcePolicies: opt.SourcePolicies,
		Platform:       p,
	}
	if iopt := opt.ImageOpt; iopt != nil {
		req.ResolveMode = iopt.ResolveMode
		req.ResolverType = int32(sourceresolver.ResolverTypeRegistry)
	}

	if iopt := opt.OCILayoutOpt; iopt != nil {
		req.ResolverType = int32(sourceresolver.ResolverTypeOCILayout)
		req.StoreID = iopt.Store.StoreID
		req.SessionID = iopt.Store.SessionID
	}

	resp, err := c.client.ResolveImageConfig(ctx, req)
	if err != nil {
		return "", "", nil, err
	}
	newRef := resp.Ref
	if newRef == "" {
		// No ref returned, use the original one.
		// This could occur if the version of buildkitd is too old.
		newRef = ref
	}
	return newRef, digest.Digest(resp.Digest), resp.Config, nil
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

func (c *grpcClient) CurrentFrontend() (*llb.State, error) {
	fp := "/run/config/buildkit/metadata/frontend.bin"
	if _, err := os.Stat(fp); err != nil {
		return nil, nil
	}
	dt, err := os.ReadFile(fp)
	if err != nil {
		return nil, err
	}
	var def opspb.Definition
	if err := def.UnmarshalVT(dt); err != nil {
		return nil, err
	}
	op, err := llb.NewDefinitionOp(&def)
	if err != nil {
		return nil, err
	}
	st := llb.NewState(op)
	return &st, nil
}

func (c *grpcClient) Inputs(ctx context.Context) (map[string]llb.State, error) {
	err := c.caps.Supports(pb.CapFrontendInputs)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Inputs(ctx, &pb.InputsRequest{})
	if err != nil {
		return nil, err
	}

	inputs := make(map[string]llb.State)
	for key, def := range resp.Definitions {
		op, err := llb.NewDefinitionOp(def)
		if err != nil {
			return nil, err
		}
		inputs[key] = llb.NewState(op)
	}
	return inputs, nil
}

// procMessageForwarder is created per container process to act as the
// communication channel between the process and the ExecProcess message
// stream.
type procMessageForwarder struct {
	ctx  context.Context
	done chan struct{}
	msgs chan *pb.ExecMessage
}

func newProcMessageForwarder(ctx context.Context) *procMessageForwarder {
	return &procMessageForwarder{
		ctx:  ctx,
		done: make(chan struct{}),
		msgs: make(chan *pb.ExecMessage),
	}
}

func (b *procMessageForwarder) Send(ctx context.Context, m *pb.ExecMessage) {
	select {
	case <-ctx.Done():
	case <-b.done:
	case b.msgs <- m:
	}
}

func (b *procMessageForwarder) Recv(ctx context.Context) (*pb.ExecMessage, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-b.ctx.Done():
		return nil, context.Cause(b.ctx)
	case <-b.done:
		return nil, nil
	case m := <-b.msgs:
		return m, nil
	}
}

func (b *procMessageForwarder) Close() {
	close(b.done)
}

// messageForwarder manages a single grpc stream for ExecProcess to facilitate
// a pub/sub message channel for each new process started from the client
// connection.
type messageForwarder struct {
	client pb.LLBBridgeClient
	ctx    context.Context
	cancel func(error)
	eg     *errgroup.Group
	mu     sync.Mutex
	pids   map[string]*procMessageForwarder
	stream pb.LLBBridge_ExecProcessClient
	// Keep send serialization separate from mu so the receive loop can deliver
	// process output while Send is blocked on flow control.
	sendMu sync.Mutex
	// startOnce used to only start the exec message forwarder once,
	// so we only have one exec stream per client
	startOnce sync.Once
	// startErr tracks the error when initializing the stream, it will
	// be returned on subsequent calls to Start
	startErr error
}

func newMessageForwarder(ctx context.Context, client pb.LLBBridgeClient) *messageForwarder {
	ctx, cancel := context.WithCancelCause(ctx)
	eg, ctx := errgroup.WithContext(ctx)
	return &messageForwarder{
		client: client,
		pids:   map[string]*procMessageForwarder{},
		ctx:    ctx,
		cancel: cancel,
		eg:     eg,
	}
}

func (m *messageForwarder) Start() (err error) {
	defer func() {
		if err != nil {
			m.startErr = err
		}
	}()

	if m.startErr != nil {
		return m.startErr
	}

	m.startOnce.Do(func() {
		m.stream, err = m.client.ExecProcess(m.ctx)
		if err != nil {
			return
		}
		m.eg.Go(func() error {
			for {
				msg, err := m.stream.Recv()
				if err != nil {
					// EOF and cancellation also end delivery to every process,
					// even though they are not errors for Release.
					m.cancel(errors.Wrap(err, "remote exec stream closed"))
				}
				if errors.Is(err, io.EOF) || grpcerrors.Code(err) == codes.Canceled {
					return nil
				}
				bklog.G(m.ctx).Debugf("|<--- %s", debugMessage(msg))

				if err != nil {
					return err
				}

				m.mu.Lock()
				msgs, ok := m.pids[msg.ProcessID]
				m.mu.Unlock()

				if !ok {
					bklog.G(m.ctx).Debugf("Received exec message for unregistered process: %s", msg.String())
					continue
				}
				msgs.Send(m.ctx, msg)
			}
		})
	})
	return err
}

func debugMessage(msg *pb.ExecMessage) string {
	switch m := msg.GetInput().(type) {
	case *pb.ExecMessage_Init:
		return fmt.Sprintf("Init Message %s", msg.ProcessID)
	case *pb.ExecMessage_File:
		if m.File.EOF {
			return fmt.Sprintf("File Message %s, fd=%d, EOF", msg.ProcessID, m.File.Fd)
		}
		return fmt.Sprintf("File Message %s, fd=%d, %d bytes", msg.ProcessID, m.File.Fd, len(m.File.Data))
	case *pb.ExecMessage_Resize:
		return fmt.Sprintf("Resize Message %s", msg.ProcessID)
	case *pb.ExecMessage_Signal:
		return fmt.Sprintf("Signal Message %s: %s", msg.ProcessID, m.Signal.Name)
	case *pb.ExecMessage_Started:
		return fmt.Sprintf("Started Message %s", msg.ProcessID)
	case *pb.ExecMessage_Exit:
		return fmt.Sprintf("Exit Message %s, code=%d, err=%s", msg.ProcessID, m.Exit.Code, m.Exit.Error)
	case *pb.ExecMessage_Done:
		return fmt.Sprintf("Done Message %s", msg.ProcessID)
	}
	return fmt.Sprintf("Unknown Message %s", msg.String())
}

func (m *messageForwarder) Send(msg *pb.ExecMessage) error {
	m.mu.Lock()
	_, ok := m.pids[msg.ProcessID]
	m.mu.Unlock()
	if !ok {
		return errors.Errorf("process %s has ended, not sending message %#v", msg.ProcessID, msg.Input)
	}
	bklog.G(m.ctx).Debugf("|---> %s", debugMessage(msg))
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return m.stream.Send(msg)
}

func (m *messageForwarder) Release() error {
	m.cancel(errors.WithStack(context.Canceled))
	return m.eg.Wait()
}

func (m *messageForwarder) Register(pid string) *procMessageForwarder {
	m.mu.Lock()
	defer m.mu.Unlock()
	sender := newProcMessageForwarder(m.ctx)
	m.pids[pid] = sender
	return sender
}

func (m *messageForwarder) Deregister(pid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sender, ok := m.pids[pid]
	if !ok {
		return
	}
	delete(m.pids, pid)
	sender.Close()
}

type msgWriter struct {
	mux       *messageForwarder
	fd        uint32
	processID string
}

func (w *msgWriter) Write(msg []byte) (int, error) {
	err := w.mux.Send(&pb.ExecMessage{
		ProcessID: w.processID,
		Input: &pb.ExecMessage_File{
			File: &pb.FdMessage{
				Fd:   w.fd,
				Data: msg,
			},
		},
	})
	if err != nil {
		return 0, err
	}
	return len(msg), nil
}

func (c *grpcClient) NewContainer(ctx context.Context, req client.NewContainerRequest) (client.Container, error) {
	err := c.caps.Supports(pb.CapGatewayExec)
	if err != nil {
		return nil, err
	}
	id := identity.NewID()
	var mounts []*opspb.Mount
	for _, m := range req.Mounts {
		resultID := m.ResultID
		if m.Ref != nil {
			ref, ok := m.Ref.(*reference)
			if !ok {
				return nil, errors.Errorf("unexpected type for reference, got %T", m.Ref)
			}
			resultID = ref.id
		}
		mounts = append(mounts, &opspb.Mount{
			Dest:      m.Dest,
			Selector:  m.Selector,
			Readonly:  m.Readonly,
			MountType: m.MountType,
			ResultID:  resultID,
			CacheOpt:  m.CacheOpt,
			SecretOpt: m.SecretOpt,
			SSHOpt:    m.SSHOpt,
		})
	}

	bklog.G(ctx).Debugf("|---> NewContainer %s", id)
	_, err = c.client.NewContainer(ctx, &pb.NewContainerRequest{
		ContainerID: id,
		Mounts:      mounts,
		Platform:    req.Platform,
		Constraints: req.Constraints,
		Network:     req.NetMode,
		ExtraHosts:  req.ExtraHosts,
		Hostname:    req.Hostname,
	})
	if err != nil {
		return nil, err
	}

	// ensure message forwarder is started, only sets up stream first time called
	err = c.execMsgs.Start()
	if err != nil {
		return nil, err
	}

	return &container{
		client:   c.client,
		caps:     c.caps,
		id:       id,
		execMsgs: c.execMsgs,
	}, nil
}

type container struct {
	client   pb.LLBBridgeClient
	caps     apicaps.CapSet
	id       string
	execMsgs *messageForwarder
}

func (ctr *container) Start(ctx context.Context, req client.StartRequest) (client.ContainerProcess, error) {
	pid := fmt.Sprintf("%s:%s", ctr.id, identity.NewID())
	msgs := ctr.execMsgs.Register(pid)

	if len(req.SecretEnv) > 0 {
		if err := ctr.caps.Supports(pb.CapGatewayExecSecretEnv); err != nil {
			return nil, err
		}
	}

	init := &pb.InitMessage{
		ContainerID: ctr.id,
		Meta: &opspb.Meta{
			Args: req.Args,
			Env:  req.Env,
			Cwd:  req.Cwd,
			User: req.User,
		},
		Tty:       req.Tty,
		Security:  req.SecurityMode,
		Secretenv: req.SecretEnv,
	}
	init.Meta.RemoveMountStubsRecursive = req.RemoveMountStubsRecursive
	if req.Stdin != nil {
		init.Fds = append(init.Fds, 0)
	}
	if req.Stdout != nil {
		init.Fds = append(init.Fds, 1)
	}
	if req.Stderr != nil {
		init.Fds = append(init.Fds, 2)
	}

	err := ctr.execMsgs.Send(&pb.ExecMessage{
		ProcessID: pid,
		Input: &pb.ExecMessage_Init{
			Init: init,
		},
	})
	if err != nil {
		return nil, err
	}

	msg, err := msgs.Recv(ctx)
	if err != nil {
		ctr.execMsgs.Deregister(pid)
		return nil, err
	}
	if msg == nil {
		return nil, errors.New("failed to receive started message")
	}
	started := msg.GetStarted()
	if started == nil {
		return nil, errors.Errorf("expecting started message, got %T", msg.GetInput())
	}

	eg, ctx := errgroup.WithContext(ctx)
	done := make(chan struct{})

	ctrProc := &containerProcess{
		execMsgs: ctr.execMsgs,
		id:       pid,
		eg:       eg,
	}

	var stdinReader *io.PipeReader
	ctrProc.eg.Go(func() error {
		<-done
		if stdinReader != nil {
			return stdinReader.Close()
		}
		return nil
	})

	if req.Stdin != nil {
		var stdinWriter io.WriteCloser
		stdinReader, stdinWriter = io.Pipe()
		// This go routine is intentionally not part of the errgroup because
		// if os.Stdin is used for req.Stdin then this will block until
		// the user closes the input, which will likely be after we are done
		// with the container, so we can't Wait on it.
		go func() {
			io.Copy(stdinWriter, req.Stdin)
			stdinWriter.Close()
		}()

		ctrProc.eg.Go(func() error {
			m := &msgWriter{
				mux:       ctr.execMsgs,
				processID: pid,
				fd:        0,
			}
			_, copyErr := io.Copy(m, stdinReader)
			if copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) {
				return copyErr
			}
			err := ctr.execMsgs.Send(&pb.ExecMessage{
				ProcessID: pid,
				Input: &pb.ExecMessage_File{
					File: &pb.FdMessage{
						Fd:  0,
						EOF: true,
					},
				},
			})
			// Cleanup still sends EOF to unblock remote stdin, but a failed send
			// must not replace the process result that triggered cleanup.
			if errors.Is(copyErr, io.ErrClosedPipe) {
				return nil
			}
			return err
		})
	}

	ctrProc.eg.Go(func() error {
		var closeDoneOnce sync.Once
		defer closeDoneOnce.Do(func() { close(done) })
		var exitError error
		for {
			msg, err := msgs.Recv(ctx)
			if err != nil {
				return err
			}
			if msg == nil {
				// no more messages, return
				return exitError
			}

			if file := msg.GetFile(); file != nil {
				var out io.WriteCloser
				switch file.Fd {
				case 1:
					out = req.Stdout
				case 2:
					out = req.Stderr
				}
				if out == nil {
					// if things are plumbed correctly this should never happen
					return errors.Errorf("missing writer for output fd %d", file.Fd)
				}
				if len(file.Data) > 0 {
					_, err := out.Write(file.Data)
					if err != nil {
						return err
					}
				}
			} else if exit := msg.GetExit(); exit != nil {
				// capture exit message to exitError so we can return it after
				// the server sends the Done message
				closeDoneOnce.Do(func() {
					close(done)
				})
				if exit.Code == 0 {
					continue
				}
				exitError = grpcerrors.FromGRPC(status.ErrorProto(&spb.Status{
					Code:    exit.Error.Code,
					Message: exit.Error.Message,
					Details: exit.Error.Details,
				}))
				if exit.Code != pb.UnknownExitStatus {
					exitError = &pb.ExitError{ExitCode: exit.Code, Err: exitError}
				}
			} else if serverDone := msg.GetDone(); serverDone != nil {
				return exitError
			} else {
				return errors.Errorf("unexpected Exec Message for pid %s: %T", pid, msg.GetInput())
			}
		}
	})

	return ctrProc, nil
}

func (ctr *container) Release(ctx context.Context) error {
	bklog.G(ctx).Debugf("|---> ReleaseContainer %s", ctr.id)
	_, err := ctr.client.ReleaseContainer(ctx, &pb.ReleaseContainerRequest{
		ContainerID: ctr.id,
	})
	return err
}

func (ctr *container) ReadFile(ctx context.Context, req client.ReadContainerRequest) ([]byte, error) {
	if err := ctr.caps.Supports(pb.CapGatewayExecFilesystem); err != nil {
		return nil, err
	}

	bklog.G(ctx).Debugf("|---> ReadFileContainer %s@%d", ctr.id, req.MountIndex)
	in := &pb.ReadFileRequest{
		Ref:        ctr.id,
		FilePath:   req.Filename,
		MountIndex: int32(req.MountIndex),
	}
	if req.Range != nil {
		in.Range = &pb.FileRange{
			Length: int64(req.Range.Length),
			Offset: int64(req.Range.Offset),
		}
	}

	resp, err := ctr.client.ReadFileContainer(ctx, in)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (ctr *container) ReadDir(ctx context.Context, req client.ReadDirContainerRequest) ([]*fstypes.Stat, error) {
	if err := ctr.caps.Supports(pb.CapGatewayExecFilesystem); err != nil {
		return nil, err
	}

	bklog.G(ctx).Debugf("|---> ReadDirContainer %s@%d", ctr.id, req.MountIndex)
	in := &pb.ReadDirRequest{
		Ref:            ctr.id,
		DirPath:        req.Path,
		IncludePattern: req.IncludePattern,
		MountIndex:     int32(req.MountIndex),
	}

	resp, err := ctr.client.ReadDirContainer(ctx, in)
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

func (ctr *container) StatFile(ctx context.Context, req client.StatContainerRequest) (*fstypes.Stat, error) {
	if err := ctr.caps.Supports(pb.CapGatewayExecFilesystem); err != nil {
		return nil, err
	}

	bklog.G(ctx).Debugf("|---> StatFileContainer %s@%d", ctr.id, req.MountIndex)
	in := &pb.StatFileRequest{
		Ref:        ctr.id,
		Path:       req.Path,
		MountIndex: int32(req.MountIndex),
	}

	resp, err := ctr.client.StatFileContainer(ctx, in)
	if err != nil {
		return nil, err
	}
	return resp.Stat, nil
}

type containerProcess struct {
	execMsgs *messageForwarder
	id       string
	eg       *errgroup.Group
}

func (ctrProc *containerProcess) Wait() error {
	defer ctrProc.execMsgs.Deregister(ctrProc.id)
	return ctrProc.eg.Wait()
}

func (ctrProc *containerProcess) Resize(_ context.Context, size client.WinSize) error {
	return ctrProc.execMsgs.Send(&pb.ExecMessage{
		ProcessID: ctrProc.id,
		Input: &pb.ExecMessage_Resize{
			Resize: &pb.ResizeMessage{
				Cols: size.Cols,
				Rows: size.Rows,
			},
		},
	})
}

var sigToName = map[syscall.Signal]string{}

func init() {
	for name, value := range signal.SignalMap {
		sigToName[value] = name
	}
}

func (ctrProc *containerProcess) Signal(_ context.Context, sig syscall.Signal) error {
	name := sigToName[sig]
	if name == "" {
		return errors.Errorf("unknown signal %v", sig)
	}
	return ctrProc.execMsgs.Send(&pb.ExecMessage{
		ProcessID: ctrProc.id,
		Input: &pb.ExecMessage_Signal{
			Signal: &pb.SignalMessage{
				Name: name,
			},
		},
	})
}

type reference struct {
	c   *grpcClient
	id  string
	def *opspb.Definition
}

func newReference(c *grpcClient, ref *pb.Ref) *reference {
	return &reference{c: c, id: ref.Id, def: ref.Def}
}

func (r *reference) ToState() (st llb.State, err error) {
	err = r.c.caps.Supports(pb.CapReferenceOutput)
	if err != nil {
		return st, err
	}

	if r.def == nil {
		return st, errors.New("gateway did not return reference with definition")
	}

	defop, err := llb.NewDefinitionOp(r.def)
	if err != nil {
		return st, err
	}

	return llb.NewState(defop), nil
}

func (r *reference) Evaluate(ctx context.Context) error {
	req := &pb.EvaluateRequest{Ref: r.id}
	_, err := r.c.client.Evaluate(ctx, req)
	if err != nil {
		return err
	}
	return nil
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
	dialOpts := []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return stdioConn(), nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(grpcerrors.UnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpcerrors.StreamClientInterceptor),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
	}

	//nolint:staticcheck // ignore SA1019 NewClient has different behavior and needs to be tested
	cc, err := grpc.DialContext(ctx, "localhost", dialOpts...)
	if err != nil {
		return ctx, nil, errors.Wrap(err, "failed to create grpc client")
	}

	ctx, cancel := context.WithCancelCause(ctx)
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

type dummyAddr struct{}

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

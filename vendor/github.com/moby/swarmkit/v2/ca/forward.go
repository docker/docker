package ca

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	certForwardedKey = "forwarded_cert"
	certCNKey        = "forwarded_cert_cn"
	certOUKey        = "forwarded_cert_ou"
	certOrgKey       = "forwarded_cert_org"
	remoteAddrKey    = "remote_addr"
)

// forwardedTLSInfoFromContext obtains forwarded TLS CN/OU from the grpc.MD
// object in ctx.
func forwardedTLSInfoFromContext(ctx context.Context) (remoteAddr string, cn string, org string, ous []string) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md[remoteAddrKey]) != 0 {
		remoteAddr = md[remoteAddrKey][0]
	}
	if len(md[certCNKey]) != 0 {
		cn = md[certCNKey][0]
	}
	if len(md[certOrgKey]) != 0 {
		org = md[certOrgKey][0]
	}
	ous = md[certOUKey]
	return
}

func isForwardedRequest(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md[certForwardedKey]) != 1 {
		return false
	}
	return md[certForwardedKey][0] == "true"
}

// WithMetadataForwardTLSInfo adds the client's TLS information to the outgoing
// gRPC context, replacing any TLS information already there and leaving the
// rest of the outgoing metadata alone.
//
// A request which arrived already forwarded is refused. Forwarding it again
// would present the node which forwarded it as the client.
func WithMetadataForwardTLSInfo(ctx context.Context) (context.Context, error) {
	if isForwardedRequest(ctx) {
		return ctx, status.Errorf(codes.ResourceExhausted, "more than one redirect to leader: request was already forwarded")
	}

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}

	ous := []string{}
	org := ""
	cn := ""

	certSubj, err := certSubjectFromContext(ctx)
	if err == nil {
		cn = certSubj.CommonName
		ous = certSubj.OrganizationalUnit
		if len(certSubj.Organization) > 0 {
			org = certSubj.Organization[0]
		}
	}

	// If there's no TLS cert, forward with blank TLS metadata.
	// Note that the presence of this blank metadata is extremely
	// important. Without it, it would look like manager is making
	// the request directly.
	md[certForwardedKey] = []string{"true"}
	md[certCNKey] = []string{cn}
	md[certOrgKey] = []string{org}
	md[certOUKey] = ous
	peer, ok := peer.FromContext(ctx)
	if ok {
		md[remoteAddrKey] = []string{peer.Addr.String()}
	} else {
		delete(md, remoteAddrKey)
	}

	return metadata.NewOutgoingContext(ctx, md), nil
}

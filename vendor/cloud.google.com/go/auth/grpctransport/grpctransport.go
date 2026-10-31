// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package grpctransport provides functionality for managing gRPC client
// connections to Google Cloud services.
package grpctransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/internal"
	"cloud.google.com/go/auth/internal/transport"
	"cloud.google.com/go/auth/internal/transport/headers"
	"github.com/googleapis/gax-go/v2"
	"github.com/googleapis/gax-go/v2/callctx"
	"github.com/googleapis/gax-go/v2/internallog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	grpcinsecure "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
)

const (
	// Check env to disable DirectPath traffic.
	disableDirectPathEnvVar = "GOOGLE_CLOUD_DISABLE_DIRECT_PATH"

	// Check env to decide if using google-c2p resolver for DirectPath traffic.
	enableDirectPathXdsEnvVar = "GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS"

	// Check env to decide if using DirectPath over Interconnect (on-premises xDS).
	enableDirectPathXdsOverInterconnectEnvVar = "GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS_OVER_INTERCONNECT"

	quotaProjectHeaderKey = "X-goog-user-project"
)

// codeToStr is a reversal of the `strToCode` map in
// https://github.com/grpc/grpc-go/blob/master/codes/codes.go
// The gRPC specification has exactly 17 status codes, defined
// as a contiguous block of integers from 0 to 16.
var codeToStr = [...]string{
	"OK",                  // codes.OK = 0
	"CANCELED",            // codes.Canceled = 1
	"UNKNOWN",             // codes.Unknown = 2
	"INVALID_ARGUMENT",    // codes.InvalidArgument = 3
	"DEADLINE_EXCEEDED",   // codes.DeadlineExceeded = 4
	"NOT_FOUND",           // codes.NotFound = 5
	"ALREADY_EXISTS",      // codes.AlreadyExists = 6
	"PERMISSION_DENIED",   // codes.PermissionDenied = 7
	"RESOURCE_EXHAUSTED",  // codes.ResourceExhausted = 8
	"FAILED_PRECONDITION", // codes.FailedPrecondition = 9
	"ABORTED",             // codes.Aborted = 10
	"OUT_OF_RANGE",        // codes.OutOfRange = 11
	"UNIMPLEMENTED",       // codes.Unimplemented = 12
	"INTERNAL",            // codes.Internal = 13
	"UNAVAILABLE",         // codes.Unavailable = 14
	"DATA_LOSS",           // codes.DataLoss = 15
	"UNAUTHENTICATED",     // codes.Unauthenticated = 16
}

var (
	// Set at init time by dial_socketopt.go. If nil, socketopt is not supported.
	timeoutDialerOption grpc.DialOption
)

// ClientCertProvider is a function that returns a TLS client certificate to be
// used when opening TLS connections. It follows the same semantics as
// [crypto/tls.Config.GetClientCertificate].
type ClientCertProvider = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)

// Options used to configure a [GRPCClientConnPool] from [Dial].
type Options struct {
	// DisableTelemetry disables default telemetry (OpenTelemetry). An example
	// reason to do so would be to bind custom telemetry that overrides the
	// defaults.
	DisableTelemetry bool
	// DisableAuthentication specifies that no authentication should be used. It
	// is suitable only for testing and for accessing public resources, like
	// public Google Cloud Storage buckets.
	DisableAuthentication bool
	// Endpoint overrides the default endpoint to be used for a service.
	Endpoint string
	// Metadata is extra gRPC metadata that will be appended to every outgoing
	// request.
	Metadata map[string]string
	// GRPCDialOpts are dial options that will be passed to `grpc.Dial` when
	// establishing a`grpc.Conn``
	GRPCDialOpts []grpc.DialOption
	// PoolSize is specifies how many connections to balance between when making
	// requests. If unset or less than 1, the value defaults to 1.
	PoolSize int
	// Credentials used to add Authorization metadata to all requests. If set
	// DetectOpts are ignored.
	Credentials *auth.Credentials
	// ClientCertProvider is a function that returns a TLS client certificate to
	// be used when opening TLS connections. It follows the same semantics as
	// crypto/tls.Config.GetClientCertificate.
	ClientCertProvider ClientCertProvider
	// DetectOpts configures settings for detect Application Default
	// Credentials.
	DetectOpts *credentials.DetectOptions
	// UniverseDomain is the default service domain for a given Cloud universe.
	// The default value is "googleapis.com". This is the universe domain
	// configured for the client, which will be compared to the universe domain
	// that is separately configured for the credentials.
	UniverseDomain string
	// APIKey specifies an API key to be used as the basis for authentication.
	// If set DetectOpts are ignored.
	APIKey string
	// Logger is used for debug logging. If provided, logging will be enabled
	// at the loggers configured level. By default logging is disabled unless
	// enabled by setting GOOGLE_SDK_GO_LOGGING_LEVEL in which case a default
	// logger will be used. Optional.
	Logger *slog.Logger

	// InternalOptions are NOT meant to be set directly by consumers of this
	// package, they should only be set by generated client code.
	InternalOptions *InternalOptions
}

// client returns the client a user set for the detect options or nil if one was
// not set.
func (o *Options) client() *http.Client {
	if o.DetectOpts != nil && o.DetectOpts.Client != nil {
		return o.DetectOpts.Client
	}
	return nil
}

func (o *Options) logger() *slog.Logger {
	return internallog.New(o.Logger)
}

func (o *Options) clientUniverseDomain() string {
	if o != nil && o.UniverseDomain != "" {
		return o.UniverseDomain
	}
	if envUD := os.Getenv(internal.UniverseDomainEnvVar); envUD != "" {
		return envUD
	}
	return internal.DefaultUniverseDomain
}

func (o *Options) validate() error {
	if o == nil {
		return errors.New("grpctransport: opts required to be non-nil")
	}
	if o.InternalOptions != nil && o.InternalOptions.SkipValidation {
		return nil
	}
	hasCreds := o.APIKey != "" ||
		o.Credentials != nil ||
		(o.DetectOpts != nil && len(o.DetectOpts.CredentialsJSON) > 0) ||
		(o.DetectOpts != nil && o.DetectOpts.CredentialsFile != "")
	if o.DisableAuthentication && hasCreds {
		return errors.New("grpctransport: DisableAuthentication is incompatible with options that set or detect credentials")
	}
	return nil
}

func (o *Options) resolveDetectOptions() *credentials.DetectOptions {
	io := o.InternalOptions
	// soft-clone these so we are not updating a ref the user holds and may reuse
	do := transport.CloneDetectOptions(o.DetectOpts)

	// If scoped JWTs are enabled user provided an aud, allow self-signed JWT.
	if (io != nil && io.EnableJWTWithScope) || do.Audience != "" {
		do.UseSelfSignedJWT = true
	}
	// Only default scopes if user did not also set an audience.
	if len(do.Scopes) == 0 && do.Audience == "" && io != nil && len(io.DefaultScopes) > 0 {
		do.Scopes = make([]string, len(io.DefaultScopes))
		copy(do.Scopes, io.DefaultScopes)
	}
	if len(do.Scopes) == 0 && do.Audience == "" && io != nil {
		do.Audience = o.InternalOptions.DefaultAudience
	}
	if o.ClientCertProvider != nil {
		tlsConfig := &tls.Config{
			GetClientCertificate: o.ClientCertProvider,
		}
		do.Client = transport.DefaultHTTPClientWithTLS(tlsConfig)
		do.TokenURL = credentials.GoogleMTLSTokenURL
	}
	if do.Logger == nil {
		do.Logger = o.logger()
	}
	return do
}

// InternalOptions are only meant to be set by generated client code. These are
// not meant to be set directly by consumers of this package. Configuration in
// this type is considered EXPERIMENTAL and may be removed at any time in the
// future without warning.
type InternalOptions struct {
	// EnableNonDefaultSAForDirectPath overrides the default requirement for
	// using the default service account for DirectPath.
	EnableNonDefaultSAForDirectPath bool
	// EnableDirectPath overrides the default attempt to use DirectPath.
	EnableDirectPath bool
	// EnableDirectPathXds overrides the default DirectPath type. It is only
	// valid when DirectPath is enabled.
	EnableDirectPathXds bool
	// EnableDirectPathXdsOverInterconnect enables DirectPath over Google Cloud
	// Interconnect (on-premises xDS name resolution via google-c2p with force-xds).
	EnableDirectPathXdsOverInterconnect bool
	// EnableJWTWithScope specifies if scope can be used with self-signed JWT.
	EnableJWTWithScope bool
	// AllowHardBoundTokens allows libraries to request a hard-bound token.
	// Obtaining hard-bound tokens requires the connection to be established
	// using either ALTS or mTLS with S2A.
	AllowHardBoundTokens []string
	// DefaultAudience specifies a default audience to be used as the audience
	// field ("aud") for the JWT token authentication.
	DefaultAudience string
	// DefaultEndpointTemplate combined with UniverseDomain specifies
	// the default endpoint.
	DefaultEndpointTemplate string
	// DefaultMTLSEndpoint specifies the default mTLS endpoint.
	DefaultMTLSEndpoint string
	// DefaultScopes specifies the default OAuth2 scopes to be used for a
	// service.
	DefaultScopes []string
	// SkipValidation bypasses validation on Options. It should only be used
	// internally for clients that need more control over their transport.
	SkipValidation bool
	// TelemetryAttributes specifies a map of telemetry attributes to be added
	// to all OpenTelemetry signals, such as tracing and metrics, for purposes
	// including representing the static identity of the client (e.g., service
	// name, version). These attributes are expected to be consistent across all
	// signals to enable cross-signal correlation.
	//
	// It should only be used internally by generated clients. Callers should not
	// modify the map after it is passed in.
	TelemetryAttributes map[string]string
}

// Dial returns a GRPCClientConnPool that can be used to communicate with a
// Google cloud service, configured with the provided [Options]. It
// automatically appends Authorization metadata to all outgoing requests.
func Dial(ctx context.Context, secure bool, opts *Options) (GRPCClientConnPool, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.PoolSize <= 1 {
		conn, err := dial(ctx, secure, opts)
		if err != nil {
			return nil, err
		}
		return &singleConnPool{conn}, nil
	}
	pool := &roundRobinConnPool{}
	for i := 0; i < opts.PoolSize; i++ {
		conn, err := dial(ctx, secure, opts)
		if err != nil {
			// ignore close error, if any
			defer pool.Close()
			return nil, err
		}
		pool.conns = append(pool.conns, conn)
	}
	return pool, nil
}

// return a GRPCClientConnPool if pool == 1 or else a pool of of them if >1
func dial(ctx context.Context, secure bool, opts *Options) (*grpc.ClientConn, error) {
	tOpts := &transport.Options{
		Endpoint:           opts.Endpoint,
		ClientCertProvider: opts.ClientCertProvider,
		Client:             opts.client(),
		UniverseDomain:     opts.UniverseDomain,
		Logger:             opts.logger(),
	}
	if io := opts.InternalOptions; io != nil {
		tOpts.DefaultEndpointTemplate = io.DefaultEndpointTemplate
		tOpts.DefaultMTLSEndpoint = io.DefaultMTLSEndpoint
		tOpts.EnableDirectPath = io.EnableDirectPath
		tOpts.EnableDirectPathXds = io.EnableDirectPathXds
		tOpts.EnableDirectPathXdsOverInterconnect = io.EnableDirectPathXdsOverInterconnect
	}
	transportCreds, err := transport.GetGRPCTransportCredsAndEndpoint(tOpts)
	if err != nil {
		return nil, err
	}

	if !secure {
		transportCreds.TransportCredentials = grpcinsecure.NewCredentials()
	}

	// Initialize gRPC dial options with transport-level security options.
	grpcOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transportCreds),
	}

	// Ensure the token exchange HTTP transport uses the same ClientCertProvider as the GRPC API transport.
	opts.ClientCertProvider, err = transport.GetClientCertificateProvider(tOpts)
	if err != nil {
		return nil, err
	}

	if opts.APIKey != "" {
		grpcOpts = append(grpcOpts,
			grpc.WithPerRPCCredentials(&grpcKeyProvider{
				apiKey:   opts.APIKey,
				metadata: opts.Metadata,
				secure:   secure,
			}),
		)
	} else if !opts.DisableAuthentication {
		metadata := opts.Metadata

		var creds *auth.Credentials
		if opts.Credentials != nil {
			creds = opts.Credentials
		} else {
			// This condition is only met for non-DirectPath clients because
			// TransportTypeMTLSS2A is used only when InternalOptions.EnableDirectPath
			// is false.
			optsClone := opts.resolveDetectOptions()
			if transportCreds.TransportType == transport.TransportTypeMTLSS2A {
				// Check that the client allows requesting hard-bound token for the transport type mTLS using S2A.
				for _, ev := range opts.InternalOptions.AllowHardBoundTokens {
					if ev == "MTLS_S2A" {
						optsClone.TokenBindingType = credentials.MTLSHardBinding
						break
					}
				}
			}
			var err error
			creds, err = credentials.DetectDefault(optsClone)
			if err != nil {
				return nil, err
			}
		}

		qp, err := creds.QuotaProjectID(ctx)
		if err != nil {
			return nil, err
		}
		if qp != "" {
			if metadata == nil {
				metadata = make(map[string]string, 1)
			}
			// Don't overwrite user specified quota
			if _, ok := metadata[quotaProjectHeaderKey]; !ok {
				metadata[quotaProjectHeaderKey] = qp
			}
		}
		grpcOpts = append(grpcOpts,
			grpc.WithPerRPCCredentials(&grpcCredentialsProvider{
				creds:                creds,
				metadata:             metadata,
				clientUniverseDomain: opts.UniverseDomain,
				endpoint:             transportCreds.Endpoint,
			}),
		)
		// Attempt Direct Path
		grpcOpts, transportCreds.Endpoint, err = configureDirectPath(grpcOpts, opts, transportCreds.Endpoint, creds, metadata)
		if err != nil {
			return nil, err
		}
	}

	// Add tracing, but before the other options, so that clients can override the
	// gRPC stats handler.
	// This assumes that gRPC options are processed in order, left to right.
	grpcOpts = addOpenTelemetryStatsHandler(grpcOpts, opts, transportCreds.Endpoint)
	grpcOpts = append(grpcOpts, opts.GRPCDialOpts...)

	return grpc.DialContext(ctx, transportCreds.Endpoint, grpcOpts...)
}

// grpcKeyProvider satisfies https://pkg.go.dev/google.golang.org/grpc/credentials#PerRPCCredentials.
type grpcKeyProvider struct {
	apiKey   string
	metadata map[string]string
	secure   bool
}

func (g *grpcKeyProvider) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	metadata := make(map[string]string, len(g.metadata)+1)
	metadata["X-goog-api-key"] = g.apiKey
	for k, v := range g.metadata {
		metadata[k] = v
	}
	return metadata, nil
}

func (g *grpcKeyProvider) RequireTransportSecurity() bool {
	return g.secure
}

// grpcCredentialsProvider satisfies https://pkg.go.dev/google.golang.org/grpc/credentials#PerRPCCredentials.
type grpcCredentialsProvider struct {
	creds *auth.Credentials

	secure bool

	// Additional metadata attached as headers.
	metadata             map[string]string
	clientUniverseDomain string
	endpoint             string
}

// getClientUniverseDomain returns the default service domain for a given Cloud
// universe, with the following precedence:
//
// 1. A non-empty option.WithUniverseDomain or similar client option.
// 2. A non-empty environment variable GOOGLE_CLOUD_UNIVERSE_DOMAIN.
// 3. The default value "googleapis.com".
//
// This is the universe domain configured for the client, which will be compared
// to the universe domain that is separately configured for the credentials.
func (c *grpcCredentialsProvider) getClientUniverseDomain() string {
	if c.clientUniverseDomain != "" {
		return c.clientUniverseDomain
	}
	if envUD := os.Getenv(internal.UniverseDomainEnvVar); envUD != "" {
		return envUD
	}
	return internal.DefaultUniverseDomain
}

func (c *grpcCredentialsProvider) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	token, err := c.creds.Token(ctx)
	if err != nil {
		return nil, err
	}
	if token.MetadataString("auth.google.tokenSource") != "compute-metadata" {
		credentialsUniverseDomain, err := c.creds.UniverseDomain(ctx)
		if err != nil {
			return nil, err
		}
		if err := transport.ValidateUniverseDomain(c.getClientUniverseDomain(), credentialsUniverseDomain); err != nil {
			return nil, err
		}
	}
	if c.secure {
		ri, _ := grpccreds.RequestInfoFromContext(ctx)
		if err = grpccreds.CheckSecurityLevel(ri.AuthInfo, grpccreds.PrivacyAndIntegrity); err != nil {
			return nil, fmt.Errorf("unable to transfer credentials PerRPCCredentials: %v", err)
		}
	}
	metadata := make(map[string]string, len(c.metadata)+1)
	headers.SetAuthMetadata(ctx, token, c.endpoint, metadata)
	for k, v := range c.metadata {
		metadata[k] = v
	}
	return metadata, nil
}

func (c *grpcCredentialsProvider) RequireTransportSecurity() bool {
	return c.secure
}

func addOpenTelemetryStatsHandler(dialOpts []grpc.DialOption, opts *Options, endpoint string) []grpc.DialOption {
	if opts.DisableTelemetry {
		return dialOpts
	}
	if gax.IsFeatureEnabled("METRICS") {
		host, port := extractHostPort(endpoint)
		dialOpts = append(dialOpts, grpc.WithChainUnaryInterceptor(openTelemetryUnaryClientInterceptor(host, port)))
	}
	if !gax.IsFeatureEnabled("TRACING") && !gax.IsFeatureEnabled("LOGGING") {
		return append(dialOpts, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	}
	var staticAttrs []attribute.KeyValue
	var scopedLogger *slog.Logger

	if gax.IsFeatureEnabled("LOGGING") && opts.Logger != nil {
		scopedLogger = opts.Logger
	}

	if opts.InternalOptions != nil {
		staticAttrs = transport.StaticTelemetryAttributes(opts.InternalOptions.TelemetryAttributes)
		if scopedLogger != nil {
			var staticLogAttrs []any
			for _, attr := range staticAttrs {
				staticLogAttrs = append(staticLogAttrs, slog.String(string(attr.Key), attr.Value.AsString()))
			}
			scopedLogger = scopedLogger.With(staticLogAttrs...)
		}
	}
	var otelOpts []otelgrpc.Option
	if gax.IsFeatureEnabled("TRACING") {
		otelOpts = append(otelOpts, otelgrpc.WithSpanAttributes(staticAttrs...))
	}
	return append(dialOpts, grpc.WithStatsHandler(&otelHandler{
		Handler: otelgrpc.NewClientHandler(otelOpts...),
		logger:  scopedLogger,
	}))
}

// Extract the host and port from a target address
func extractHostPort(target string) (string, int) {
	if idx := strings.Index(target, "://"); idx != -1 {
		target = target[idx+3:]
		// Ensure any leading authorities (like 8.8.8.8 in dns://8.8.8.8/foo) are stripped
		if slashIdx := strings.Index(target, "/"); slashIdx != -1 {
			target = target[slashIdx+1:]
		}
	}
	if qIdx := strings.Index(target, "?"); qIdx != -1 {
		target = target[:qIdx]
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return target, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}

// openTelemetryUnaryClientInterceptor returns an interceptor that populates
// TransportTelemetryData with the server peer address.
func openTelemetryUnaryClientInterceptor(host string, port int) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		transportData := gax.ExtractTransportTelemetry(ctx)
		if transportData != nil {
			if host != "" {
				transportData.SetServerAddress(host)
			}
			if port != 0 {
				transportData.SetServerPort(port)
			}
		}

		err := invoker(ctx, method, req, reply, cc, opts...)

		return err
	}
}

// otelHandler is a wrapper around the OpenTelemetry gRPC client handler that
// adds custom Google Cloud-specific attributes to spans and metrics.
type otelHandler struct {
	stats.Handler
	logger *slog.Logger
}

// TagRPC intercepts the RPC start to extract dynamic attributes like resource
// name and retry count from the outgoing context metadata and attach them to
// the current span.
func (h *otelHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = h.Handler.TagRPC(ctx, info)

	var span trace.Span
	if gax.IsFeatureEnabled("TRACING") {
		if s := trace.SpanFromContext(ctx); s != nil && s.IsRecording() {
			span = s
		}
	}

	if span == nil {
		return ctx
	}
	var attrs []attribute.KeyValue
	if resName, ok := callctx.TelemetryFromContext(ctx, "resource_name"); ok {
		attrs = append(attrs, attribute.String("gcp.resource.destination.id", resName))
	}
	if resendCountStr, ok := callctx.TelemetryFromContext(ctx, "resend_count"); ok {
		if count, err := strconv.Atoi(resendCountStr); err == nil {
			attrs = append(attrs, attribute.Int("gcp.grpc.resend_count", count))
		}
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx
}

// HandleRPC intercepts the RPC completion to capture and format error-related
// attributes ensuring they conform to Google Cloud observability standards.
func (h *otelHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	end, ok := s.(*stats.End)
	if !ok {
		h.Handler.HandleRPC(ctx, s)
		return
	}

	var span trace.Span
	if gax.IsFeatureEnabled("TRACING") {
		if s := trace.SpanFromContext(ctx); s != nil && s.IsRecording() {
			span = s
		}
	}

	var logger *slog.Logger
	if gax.IsFeatureEnabled("LOGGING") {
		if l := h.logger; l != nil && l.Enabled(ctx, slog.LevelDebug) {
			logger = l
		}
	}

	if span == nil && logger == nil {
		h.Handler.HandleRPC(ctx, s)
		return
	}

	if end.Error != nil {
		h.handleRPCError(ctx, span, logger, end)
	} else {
		h.handleRPCSuccess(span)
	}
	h.Handler.HandleRPC(ctx, s)
}

func (h *otelHandler) handleRPCError(ctx context.Context, span trace.Span, logger *slog.Logger, end *stats.End) {
	info := gax.ExtractTelemetryErrorInfo(ctx, end.Error)

	if logger != nil {
		logActionableError(ctx, logger, info)
	}

	if span != nil {
		attrs := []attribute.KeyValue{
			attribute.String("error.type", info.ErrorType),
			attribute.String("status.message", info.StatusMessage),
			attribute.String("rpc.response.status_code", info.StatusCode),
			attribute.String("exception.type", fmt.Sprintf("%T", end.Error)),
		}
		span.SetAttributes(attrs...)
	}
}

func (h *otelHandler) handleRPCSuccess(span trace.Span) {
	if span != nil {
		attrs := []attribute.KeyValue{
			attribute.String("rpc.response.status_code", "OK"),
		}
		span.SetAttributes(attrs...)
	}
}

func logActionableError(ctx context.Context, logger *slog.Logger, info gax.TelemetryErrorInfo) {
	baseLogAttrs := []slog.Attr{
		slog.String("rpc.system.name", "grpc"),
		slog.String("rpc.response.status_code", info.StatusCode),
	}

	if resendCountStr, ok := callctx.TelemetryFromContext(ctx, "resend_count"); ok {
		if count, e := strconv.Atoi(resendCountStr); e == nil {
			baseLogAttrs = append(baseLogAttrs, slog.Int64("gcp.grpc.resend_count", int64(count)))
		}
	}

	msg := info.StatusMessage
	if msg == "" {
		msg = "API call failed"
	}

	if rpcMethod, ok := callctx.TelemetryFromContext(ctx, "rpc_method"); ok {
		baseLogAttrs = append(baseLogAttrs, slog.String("rpc.method", rpcMethod))
	}

	if resName, ok := callctx.TelemetryFromContext(ctx, "resource_name"); ok {
		baseLogAttrs = append(baseLogAttrs, slog.String("gcp.resource.destination.id", resName))
	}

	if info.Domain != "" {
		baseLogAttrs = append(baseLogAttrs, slog.String("gcp.errors.domain", info.Domain))
	}
	for k, v := range info.Metadata {
		baseLogAttrs = append(baseLogAttrs, slog.String("gcp.errors.metadata."+k, v))
	}

	baseLogAttrs = append(baseLogAttrs, slog.String("error.type", info.ErrorType))

	logger.LogAttrs(ctx, slog.LevelDebug, msg, baseLogAttrs...)
}

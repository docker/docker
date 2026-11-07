// Copyright 2026 Google LLC.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internaloption

import (
	"google.golang.org/api/internal"
	"google.golang.org/api/option"
)

// UnsafeResolver provides a mechanism for introspecting values passed through
// during client instantiations, which are defined as functional options.  It
// is by its nature not meant for general use, as it requires understanding of
// internal implementations.  It is intended for use solely by internal Google
// client code, and provides no stability contract.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
type UnsafeResolver struct {
	ds *internal.DialSettings
}

// NewUnsafeResolver instantiates a new ClientOption resolver.  It is intended
// for user solely by internal Google client code.  See the corresponding
// UnsafeResolver type for more details.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func NewUnsafeResolver(opts ...option.ClientOption) (*UnsafeResolver, error) {
	ds := new(internal.DialSettings)
	for _, o := range opts {
		o.Apply(ds)
	}
	return &UnsafeResolver{
		ds: ds,
	}, nil
}

// ResolvedWithAPIKeyIsCustom returns whether the option to supply an API key was
// populate. This corresponds to the WithAPIKey ClientOption in
// google.golang.org/option.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedWithAPIKeyIsCustom() bool {
	return ur.ds.APIKey != ""
}

// ResolvedGRPCConnPoolSize provides the passed in value correspnding to the
// WithGRPCConnectionPool option in google.golang.org/option.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedGRPCConnPoolSize() int {
	return ur.ds.GRPCConnPoolSize
}

// ResolvedGRPCEndpoint returns the resolved endpoint address used for
// establishing gRPC connections.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedGRPCEndpoint() (string, error) {
	_, addr, err := internal.GetGRPCTransportConfigAndEndpoint(ur.ds)
	return addr, err
}

// ResolvedGRPCConnIsCustom exposes whether the provided options included
// directives for providing a customized transport, corresponding to the
// WithGRPCConn option.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedGRPCConnIsCustom() bool {
	return ur.ds.GRPCConn != nil
}

// ResolvedHTTPClientIsCustom returns whether the option to supply an API key was
// populate. This corresponds to the WithHTTPClient ClientOption in
// google.golang.org/option.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedHTTPClientIsCustom() bool {
	return ur.ds.HTTPClient != nil
}

// ResolvedEnableDirectPath returns whether DirectPath was explicitly enabled.
// This corresponds to the EnableDirectPath option in this package.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedEnableDirectPath() bool {
	return ur.ds.EnableDirectPath
}

// ResolvedEnableDirectPathXds returns whether extensible directory service (xDS)
// support was requested as part of direct path enablement.  This corresponds to the
// EnableDirectPathXds option in this package.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedEnableDirectPathXds() bool {
	return ur.ds.EnableDirectPathXds
}

// ResolvedEnableDirectPathXdsOverInterconnect returns whether DirectPath xDS over
// Cloud Interconnect was requested. This corresponds to the
// EnableDirectPathXdsOverInterconnect option in this package.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedEnableDirectPathXdsOverInterconnect() bool {
	return ur.ds.EnableDirectPathXdsOverInterconnect
}

// ResolvedWithoutAuthentication returns whether the option to explicitly disable
// authentication was requested.  This corresponds to the WithoutAuthentication
// ClientOption in google.golang.org/option.
//
// This is an EXPERIMENTAL API and may be changed or removed in the future.
func (ur *UnsafeResolver) ResolvedWithoutAuthentication() bool {
	return ur.ds.NoAuth
}

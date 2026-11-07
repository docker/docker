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

package grpctransport

import (
	"context"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/internal"
	"cloud.google.com/go/auth/internal/compute"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	grpcgoogle "google.golang.org/grpc/credentials/google"
)

const directPathInterconnectInfix = "-direct."

var logRateLimiter = rate.Sometimes{Interval: 1 * time.Second}

func parseTarget(endpoint string) (host string, query url.Values, err error) {
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, err
	}
	target := u.Host
	if target == "" {
		target = strings.TrimPrefix(u.Path, "/")
	}
	if slashIdx := strings.Index(target, "/"); slashIdx != -1 {
		target = target[:slashIdx]
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	} else {
		host = target
	}
	return host, u.Query(), nil
}

func encodeQuery(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	var buf strings.Builder
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vs := v[k]
		keyEscaped := url.QueryEscape(k)
		if len(vs) == 0 {
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(keyEscaped)
			continue
		}
		for _, val := range vs {
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(keyEscaped)
			if val != "" {
				buf.WriteByte('=')
				buf.WriteString(url.QueryEscape(val))
			}
		}
	}
	return buf.String()
}

func isDirectPathXdsOverInterconnectUsed(endpoint string, o *Options) bool {
	if valStr, ok := os.LookupEnv(enableDirectPathXdsOverInterconnectEnvVar); ok {
		if b, err := strconv.ParseBool(valStr); err == nil {
			return b
		}
	}
	if o != nil && o.InternalOptions != nil && o.InternalOptions.EnableDirectPathXdsOverInterconnect {
		return true
	}
	if strings.Contains(endpoint, directPathInterconnectInfix) || strings.Contains(endpoint, "force-xds") {
		return true
	}
	return false
}

func hasUniverseDomainHost(endpoint, universeDomain string) bool {
	host, _, err := parseTarget(endpoint)
	if err != nil {
		return false
	}
	return host == universeDomain || strings.HasSuffix(host, "."+universeDomain)
}

func canUseDirectPathWithUniverseDomain(endpoint string, opts *Options) bool {
	if opts != nil && opts.clientUniverseDomain() != internal.DefaultUniverseDomain {
		return false
	}
	if isDirectPathXdsOverInterconnectUsed(endpoint, opts) && strings.Contains(endpoint, ".") {
		return hasUniverseDomainHost(endpoint, internal.DefaultUniverseDomain)
	}
	return true
}

func isDirectPathEnabled(endpoint string, opts *Options) bool {
	if opts == nil || opts.InternalOptions == nil || !opts.InternalOptions.EnableDirectPath {
		return false
	}
	if !checkDirectPathEndPoint(endpoint) {
		return false
	}
	if !canUseDirectPathWithUniverseDomain(endpoint, opts) {
		return false
	}
	if b, _ := strconv.ParseBool(os.Getenv(disableDirectPathEnvVar)); b {
		return false
	}
	return true
}

func checkDirectPathEndPoint(endpoint string) bool {
	// Only [dns:///]host[:port] or google-c2p:/// is supported, not other schemes (e.g., "tcp://" or "unix://").
	if strings.Contains(endpoint, "://") &&
		!strings.HasPrefix(endpoint, "dns:///") &&
		!strings.HasPrefix(endpoint, "google-c2p:///") {
		return false
	}

	if endpoint == "" {
		return false
	}

	return true
}

func isTokenProviderComputeEngine(tp auth.TokenProvider) bool {
	if tp == nil {
		return false
	}
	tok, err := tp.Token(context.Background())
	if err != nil {
		return false
	}
	if tok == nil {
		return false
	}
	if tok.MetadataString("auth.google.tokenSource") != "compute-metadata" {
		return false
	}
	if tok.MetadataString("auth.google.serviceAccount") != "default" {
		return false
	}
	return true
}

func isTokenProviderDirectPathCompatible(tp auth.TokenProvider, o *Options) bool {
	if tp == nil {
		return false
	}
	if o != nil && o.InternalOptions != nil && o.InternalOptions.EnableNonDefaultSAForDirectPath {
		return true
	}
	return isTokenProviderComputeEngine(tp)
}

func isDirectPathXdsUsed(o *Options) bool {
	// Method 1: Enable DirectPath xDS by env;
	if b, _ := strconv.ParseBool(os.Getenv(enableDirectPathXdsEnvVar)); b {
		return true
	}
	// Method 2: Enable DirectPath xDS by option;
	if o != nil && o.InternalOptions != nil && o.InternalOptions.EnableDirectPathXds {
		return true
	}
	return false
}

func isDirectPathBoundTokenEnabled(opts *InternalOptions) bool {
	if opts == nil {
		return false
	}
	for _, ev := range opts.AllowHardBoundTokens {
		if ev == "ALTS" {
			return true
		}
	}
	return false
}

func canUseDirectPath(endpoint string, opts *Options, creds *auth.Credentials) bool {
	if !isDirectPathEnabled(endpoint, opts) {
		return false
	}
	interconnect := isDirectPathXdsOverInterconnectUsed(endpoint, opts)
	if !compute.OnComputeEngine() && !interconnect {
		return false
	}
	if interconnect {
		return creds != nil && creds.TokenProvider != nil
	}
	return isTokenProviderDirectPathCompatible(creds, opts)
}

// configureDirectPath returns some dial options and an endpoint to use if the
// configuration allows the use of direct path. If it does not the provided
// grpcOpts and endpoint are returned.
func configureDirectPath(grpcOpts []grpc.DialOption, opts *Options, endpoint string, creds *auth.Credentials, metadata map[string]string) ([]grpc.DialOption, string, error) {
	logRateLimiter.Do(func() {
		logDirectPathMisconfig(endpoint, creds, opts)
	})
	if canUseDirectPath(endpoint, opts, creds) {
		interconnect := isDirectPathXdsOverInterconnectUsed(endpoint, opts)
		// Overwrite all of the previously specific DialOptions, DirectPath uses its own set of credentials and certificates.
		perRPCCreds := &grpcCredentialsProvider{
			creds:    creds,
			endpoint: endpoint,
			metadata: metadata,
		}
		if opts != nil {
			perRPCCreds.clientUniverseDomain = opts.UniverseDomain
		}
		defaultCredetialsOptions := grpcgoogle.DefaultCredentialsOptions{
			PerRPCCreds: perRPCCreds,
		}
		if !interconnect && opts != nil && isDirectPathBoundTokenEnabled(opts.InternalOptions) && isTokenProviderComputeEngine(creds) {
			optsClone := opts.resolveDetectOptions()
			optsClone.TokenBindingType = credentials.ALTSHardBinding
			altsCreds, err := credentials.DetectDefault(optsClone)
			if err != nil {
				return nil, "", err
			}
			defaultCredetialsOptions.ALTSPerRPCCreds = &grpcCredentialsProvider{creds: altsCreds, endpoint: endpoint}
		}
		grpcOpts = []grpc.DialOption{
			grpc.WithCredentialsBundle(grpcgoogle.NewDefaultCredentialsWithOptions(defaultCredetialsOptions))}
		if timeoutDialerOption != nil {
			grpcOpts = append(grpcOpts, timeoutDialerOption)
		}
		cleanAddr, query, err := parseTarget(endpoint)
		if err != nil {
			return nil, "", err
		}
		if interconnect {
			if strings.HasSuffix(cleanAddr, ".googleapis.com") && !strings.Contains(cleanAddr, directPathInterconnectInfix) {
				authority := cleanAddr
				cleanAddr = strings.TrimSuffix(cleanAddr, ".googleapis.com") + directPathInterconnectInfix + "googleapis.com"
				grpcOpts = append(grpcOpts, grpc.WithAuthority(authority))
			} else if strings.Contains(cleanAddr, directPathInterconnectInfix) {
				authority := strings.Replace(cleanAddr, directPathInterconnectInfix, ".", 1)
				grpcOpts = append(grpcOpts, grpc.WithAuthority(authority))
			}
		}
		// Check if google-c2p resolver is enabled for DirectPath
		if strings.HasPrefix(endpoint, "google-c2p:///") {
			if interconnect {
				if !query.Has("force-xds") {
					query["force-xds"] = nil
				}
			}
			endpoint = "google-c2p:///" + cleanAddr
			if q := encodeQuery(query); q != "" {
				endpoint += "?" + q
			}
		} else if isDirectPathXdsUsed(opts) || interconnect {
			endpoint = "google-c2p:///" + cleanAddr
			if interconnect {
				endpoint += "?force-xds"
			}
		} else {
			if !strings.HasPrefix(endpoint, "dns:///") {
				endpoint = "dns:///" + endpoint
			}
			grpcOpts = append(grpcOpts,
				// For now all DirectPath go clients will be using the following lb config, but in future
				// when different services need different configs, then we should change this to a
				// per-service config.
				grpc.WithDisableServiceConfig(),
				grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"grpclb":{"childPolicy":[{"pick_first":{}}]}}]}`))
		}
		// TODO: add support for system parameters (quota project, request reason) via chained interceptor.
	}
	return grpcOpts, endpoint, nil
}

func logDirectPathMisconfig(endpoint string, creds *auth.Credentials, o *Options) {
	if o == nil {
		return
	}
	if isDirectPathXdsOverInterconnectUsed(endpoint, o) {
		if !canUseDirectPathWithUniverseDomain(endpoint, o) {
			o.logger().Warn("DirectPath over Interconnect is disabled. Non-default universe domain is not supported.")
		} else if creds == nil || creds.TokenProvider == nil {
			o.logger().Warn("DirectPath over Interconnect is disabled. Valid credentials are required.")
		} else if !isDirectPathEnabled(endpoint, o) {
			o.logger().Warn("DirectPath is disabled. To enable, please set the EnableDirectPath option along with the EnableDirectPathXds option.")
		}
		return
	}

	// Case 1: does not enable DirectPath
	if !isDirectPathEnabled(endpoint, o) {
		o.logger().Warn("DirectPath is disabled. To enable, please set the EnableDirectPath option along with the EnableDirectPathXds option.")
	} else {
		interconnect := isDirectPathXdsOverInterconnectUsed(endpoint, o)
		// Case 2: credential is not correctly set
		if !interconnect && !isTokenProviderDirectPathCompatible(creds, o) {
			o.logger().Warn("DirectPath is disabled. Please make sure the token source is fetched from GCE metadata server and the default service account is used.")
		}
		// Case 3: not running on GCE
		if !interconnect && !compute.OnComputeEngine() {
			o.logger().Warn("DirectPath is disabled. DirectPath is only available in a GCE environment.")
		}
	}
}

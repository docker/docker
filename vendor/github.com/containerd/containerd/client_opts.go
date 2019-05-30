/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containerd

import (
	"time"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes"
	"google.golang.org/grpc"
)

type clientOpts struct {
	defaultns      string
	defaultRuntime string
	services       *services
	dialOptions    []grpc.DialOption
	timeout        time.Duration
}

// ClientOpt allows callers to set options on the containerd client
type ClientOpt func(c *clientOpts) error

// WithDefaultNamespace sets the default namespace on the client
//
// Any operation that does not have a namespace set on the context will
// be provided the default namespace
func WithDefaultNamespace(ns string) ClientOpt {
	return func(c *clientOpts) error {
		c.defaultns = ns
		return nil
	}
}

// WithDefaultRuntime sets the default runtime on the client
func WithDefaultRuntime(rt string) ClientOpt {
	return func(c *clientOpts) error {
		c.defaultRuntime = rt
		return nil
	}
}

// WithDialOpts allows grpc.DialOptions to be set on the connection
func WithDialOpts(opts []grpc.DialOption) ClientOpt {
	return func(c *clientOpts) error {
		c.dialOptions = opts
		return nil
	}
}

// WithServices sets services used by the client.
func WithServices(opts ...ServicesOpt) ClientOpt {
	return func(c *clientOpts) error {
		c.services = &services{}
		for _, o := range opts {
			o(c.services)
		}
		return nil
	}
}

// WithTimeout sets the connection timeout for the client
func WithTimeout(d time.Duration) ClientOpt {
	return func(c *clientOpts) error {
		c.timeout = d
		return nil
	}
}

// RemoteOpt allows the caller to set distribution options for a remote
type RemoteOpt func(*Client, *RemoteContext) error

// WithPlatform allows the caller to specify a platform to retrieve
// content for
func WithPlatform(platform string) RemoteOpt {
	if platform == "" {
		platform = platforms.DefaultString()
	}
	return func(_ *Client, c *RemoteContext) error {
		for _, p := range c.Platforms {
			if p == platform {
				return nil
			}
		}

		c.Platforms = append(c.Platforms, platform)
		return nil
	}
}

// WithPlatformMatcher specifies the matcher to use for
// determining which platforms to pull content for.
// This value supersedes anything set with `WithPlatform`.
func WithPlatformMatcher(m platforms.MatchComparer) RemoteOpt {
	return func(_ *Client, c *RemoteContext) error {
		c.PlatformMatcher = m
		return nil
	}
}

// WithPullUnpack is used to unpack an image after pull. This
// uses the snapshotter, content store, and diff service
// configured for the client.
func WithPullUnpack(_ *Client, c *RemoteContext) error {
	c.Unpack = true
	return nil
}

// WithPullSnapshotter specifies snapshotter name used for unpacking
func WithPullSnapshotter(snapshotterName string) RemoteOpt {
	return func(_ *Client, c *RemoteContext) error {
		c.Snapshotter = snapshotterName
		return nil
	}
}

// WithPullLabel sets a label to be associated with a pulled reference
func WithPullLabel(key, value string) RemoteOpt {
	return func(_ *Client, rc *RemoteContext) error {
		if rc.Labels == nil {
			rc.Labels = make(map[string]string)
		}

		rc.Labels[key] = value
		return nil
	}
}

// WithPullLabels associates a set of labels to a pulled reference
func WithPullLabels(labels map[string]string) RemoteOpt {
	return func(_ *Client, rc *RemoteContext) error {
		if rc.Labels == nil {
			rc.Labels = make(map[string]string)
		}

		for k, v := range labels {
			rc.Labels[k] = v
		}
		return nil
	}
}

// WithSchema1Conversion is used to convert Docker registry schema 1
// manifests to oci manifests on pull. Without this option schema 1
// manifests will return a not supported error.
func WithSchema1Conversion(client *Client, c *RemoteContext) error {
	c.ConvertSchema1 = true
	return nil
}

// WithResolver specifies the resolver to use.
func WithResolver(resolver remotes.Resolver) RemoteOpt {
	return func(client *Client, c *RemoteContext) error {
		c.Resolver = resolver
		return nil
	}
}

// WithImageHandler adds a base handler to be called on dispatch.
func WithImageHandler(h images.Handler) RemoteOpt {
	return func(client *Client, c *RemoteContext) error {
		c.BaseHandlers = append(c.BaseHandlers, h)
		return nil
	}
}

// WithImageHandlerWrapper wraps the handlers to be called on dispatch.
func WithImageHandlerWrapper(w func(images.Handler) images.Handler) RemoteOpt {
	return func(client *Client, c *RemoteContext) error {
		c.HandlerWrapper = w
		return nil
	}
}

// WithMaxConcurrentDownloads sets max concurrent download limit.
func WithMaxConcurrentDownloads(max int) RemoteOpt {
	return func(client *Client, c *RemoteContext) error {
		c.MaxConcurrentDownloads = max
		return nil
	}
}

// WithAppendDistributionSourceLabel allows fetcher to add distribute source
// label for each blob content, which doesn't work for legacy schema1.
func WithAppendDistributionSourceLabel() RemoteOpt {
	return func(_ *Client, c *RemoteContext) error {
		c.AppendDistributionSourceLabel = true
		return nil
	}
}

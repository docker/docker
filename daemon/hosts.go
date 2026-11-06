package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	hostconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/pkg/errors"
)

const (
	defaultPath = "/v2"
)

// RegistryHosts returns the registry hosts configuration for the host component
// of a distribution image reference.
func (daemon *Daemon) RegistryHosts(host string) ([]docker.RegistryHost, error) {
	hosts, err := hostconfig.ConfigureHosts(context.Background(), hostconfig.HostOptions{
		// TODO: Also check containerd path when updating containerd to use multiple host directories
		HostDir: hostconfig.HostDirFromRoot(registry.CertsDir()),
		// Registries may take longer than containerd's default response header
		// timeout to finalize a large blob upload. Don't impose one, to match
		// the transport used by the graphdriver image store.
		UpdateClient: func(c *http.Client) error {
			if t, ok := c.Transport.(*http.Transport); ok {
				t.ResponseHeaderTimeout = 0
			}
			return nil
		},
	})(host)
	if err == nil {
		// Merge in legacy configuration if provided
		if cfg := daemon.config().Config; len(cfg.Mirrors) > 0 || len(cfg.InsecureRegistries) > 0 {
			hosts, err = daemon.mergeLegacyConfig(host, hosts)
		}
	}

	return hosts, err
}

func (daemon *Daemon) mergeLegacyConfig(host string, hosts []docker.RegistryHost) ([]docker.RegistryHost, error) {
	// If no hosts provided, nothing to do.
	// If multiple hosts provided, then a mirror configuration is already provided and
	// should not overwrite with legacy config.
	if len(hosts) == 0 || len(hosts) > 1 {
		return hosts, nil
	}
	sc := daemon.registryService.ServiceConfig()
	if host == "docker.io" && len(sc.Mirrors) > 0 {
		hosts = mirrorsToRegistryHosts(sc.Mirrors, hosts[0])
	}
	hostDir := hostconfig.HostDirFromRoot(registry.CertsDir())
	for i := range hosts {
		t, ok := hosts[i].Client.Transport.(*http.Transport)
		if !ok {
			continue
		}
		if t.TLSClientConfig == nil {
			certsDir, err := hostDir(host)
			if err != nil && !cerrdefs.IsNotFound(err) {
				return nil, err
			} else if err == nil {
				c, err := loadTLSConfig(certsDir)
				if err != nil {
					return nil, err
				}
				t.TLSClientConfig = c
			}
		}
		if daemon.registryService.IsInsecureRegistry(hosts[i].Host) {
			if t.TLSClientConfig == nil {
				t.TLSClientConfig = &tls.Config{} //nolint: gosec // G402: TLS MinVersion too low.
			}
			t.TLSClientConfig.InsecureSkipVerify = true

			hosts[i].Client.Transport = docker.NewHTTPFallback(hosts[i].Client.Transport)
		}
	}
	return hosts, nil
}

func mirrorsToRegistryHosts(mirrors []string, dHost docker.RegistryHost) []docker.RegistryHost {
	var mirrorHosts []docker.RegistryHost
	for _, mirror := range mirrors {
		h := dHost
		h.Capabilities = docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityReferrers

		u, err := url.Parse(mirror)
		if err != nil || u.Host == "" {
			u, err = url.Parse("dummy://" + mirror)
		}
		if err == nil && u.Host != "" {
			h.Host = u.Host
			h.Path = strings.TrimSuffix(u.Path, "/")

			// For compatibility with legacy mirrors, ensure ends with /v2
			// NOTE: Use newer configuration to completely override the path
			if !strings.HasSuffix(h.Path, defaultPath) {
				h.Path = path.Join(h.Path, defaultPath)
			}
			if u.Scheme != "dummy" {
				h.Scheme = u.Scheme
			}
		} else {
			h.Host = mirror
			h.Path = defaultPath
		}

		mirrorHosts = append(mirrorHosts, h)
	}
	return append(mirrorHosts, dHost)
}

func loadTLSConfig(d string) (*tls.Config, error) {
	fs, err := os.ReadDir(d)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
		return nil, errors.WithStack(err)
	}
	type keyPair struct {
		Certificate string
		Key         string
	}
	var (
		rootCAs  []string
		keyPairs []keyPair
	)
	for _, f := range fs {
		switch filepath.Ext(f.Name()) {
		case ".crt":
			rootCAs = append(rootCAs, filepath.Join(d, f.Name()))
		case ".cert":
			keyPairs = append(keyPairs, keyPair{
				Certificate: filepath.Join(d, f.Name()),
				Key:         filepath.Join(d, strings.TrimSuffix(f.Name(), ".cert")+".key"),
			})
		}
	}

	tc := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if len(rootCAs) > 0 {
		systemPool, err := x509.SystemCertPool()
		if err != nil {
			if runtime.GOOS == "windows" {
				systemPool = x509.NewCertPool()
			} else {
				return nil, errors.Wrapf(err, "unable to get system cert pool")
			}
		}
		tc.RootCAs = systemPool
	}

	for _, p := range rootCAs {
		dt, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		tc.RootCAs.AppendCertsFromPEM(dt)
	}

	for _, kp := range keyPairs {
		cert, err := tls.LoadX509KeyPair(kp.Certificate, kp.Key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load keypair for %s", kp.Certificate)
		}
		tc.Certificates = append(tc.Certificates, cert)
	}
	return tc, nil
}

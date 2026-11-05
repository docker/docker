package registry

import (
	"context"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/lazyregexp"
	"github.com/moby/moby/v2/daemon/internal/rootless"
)

// ServiceOptions holds command line options.
type ServiceOptions struct {
	Mirrors            []string `json:"registry-mirrors,omitempty"`
	InsecureRegistries []string `json:"insecure-registries,omitempty"`
}

// serviceConfig holds daemon configuration for the registry service.
type serviceConfig registry.ServiceConfig

// TODO(thaJeztah) both the "index.docker.io" and "registry-1.docker.io" domains
// are here for historic reasons and backward-compatibility. These domains
// are still supported by Docker Hub (and will continue to be supported), but
// there are new domains already in use, and plans to consolidate all legacy
// domains to new "canonical" domains. Once those domains are decided on, we
// should update these consts (but making sure to preserve compatibility with
// existing installs, clients, and user configuration).
const (
	// DefaultNamespace is the default namespace
	DefaultNamespace = "docker.io"
	// DefaultRegistryHost is the hostname for the default (Docker Hub) registry
	// used for pushing and pulling images. This hostname is hard-coded to handle
	// the conversion from image references without registry name (e.g. "ubuntu",
	// or "ubuntu:latest"), as well as references using the "docker.io" domain
	// name, which is used as canonical reference for images on Docker Hub, but
	// does not match the domain-name of Docker Hub's registry.
	DefaultRegistryHost = "registry-1.docker.io"
	// IndexHostname is the index hostname, used for authentication and image search.
	IndexHostname = "index.docker.io"
	// IndexServer is used for user auth and image search
	IndexServer = "https://" + IndexHostname + "/v1/"
	// IndexName is the name of the index
	IndexName = "docker.io"
)

var (
	// DefaultV2Registry is the URI of the default (Docker Hub) registry.
	DefaultV2Registry = &url.URL{
		Scheme: "https",
		Host:   DefaultRegistryHost,
	}

	validHostPortRegex = lazyregexp.New(`^` + reference.DomainRegexp.String() + `$`)
)

// CertsDir is the directory where certificates are stored.
//
// - Linux: "/etc/docker/certs.d/"
// - Linux (with rootlessKit): $XDG_CONFIG_HOME/docker/certs.d/" or "$HOME/.config/docker/certs.d/"
// - Windows: "%PROGRAMDATA%/docker/certs.d/"
//
// TODO(thaJeztah): certsDir but stored in our config, and passed when needed. For the CLI, we should also default to same path as rootless.
func CertsDir() string {
	certsDir := "/etc/docker/certs.d"
	if runtime.GOOS == "linux" && rootless.RunningWithRootlessKit() {
		if configHome, _ := os.UserConfigDir(); configHome != "" {
			certsDir = filepath.Join(configHome, "docker", "certs.d")
		}
	} else if runtime.GOOS == "windows" {
		certsDir = filepath.Join(os.Getenv("programdata"), "docker", "certs.d")
	}
	return certsDir
}

// newServiceConfig returns a new instance of ServiceConfig
func newServiceConfig(options ServiceOptions) (*serviceConfig, error) {
	config := &serviceConfig{}
	if err := config.loadMirrors(options.Mirrors); err != nil {
		return nil, err
	}
	if err := config.loadInsecureRegistries(options.InsecureRegistries); err != nil {
		return nil, err
	}

	return config, nil
}

// copy constructs a new ServiceConfig with a copy of the configuration in config.
func (config *serviceConfig) copy() *registry.ServiceConfig {
	ic := make(map[string]*registry.IndexInfo)
	maps.Copy(ic, config.IndexConfigs)
	return &registry.ServiceConfig{
		InsecureRegistryCIDRs: slices.Clone(config.InsecureRegistryCIDRs),
		IndexConfigs:          ic,
		Mirrors:               slices.Clone(config.Mirrors),
	}
}

// loadMirrors loads mirrors to config, after removing duplicates.
// Returns an error if mirrors contains an invalid mirror.
func (config *serviceConfig) loadMirrors(mirrors []string) error {
	mMap := map[string]struct{}{}
	unique := []string{}

	for _, mirror := range mirrors {
		m, err := ValidateMirror(mirror)
		if err != nil {
			return err
		}
		if _, exist := mMap[m]; !exist {
			mMap[m] = struct{}{}
			unique = append(unique, m)
		}
	}

	config.Mirrors = unique

	// Configure public registry since mirrors may have changed.
	config.IndexConfigs = map[string]*registry.IndexInfo{
		IndexName: {
			Name:     IndexName,
			Mirrors:  unique,
			Secure:   true,
			Official: true,
		},
	}

	return nil
}

// loadInsecureRegistries loads insecure registries to config
func (config *serviceConfig) loadInsecureRegistries(registries []string) error {
	if registries == nil {
		// Configure insecure registries if not configured. We intentionally only
		// set the defaults it the option is nil to allow disabling insecure registries
		// through setting an empty slice.
		//
		// If not configured, we consider localhost as an insecure registry.
		// This is a stop-gap for people who are running a private registry
		// on localhost.
		registries = append(registries, "::1/128", "127.0.0.0/8")
	}

	var (
		insecureRegistryCIDRs = make(map[netip.Prefix]struct{})
		indexConfigs          = make(map[string]*registry.IndexInfo)
	)

	for _, r := range registries {
		// validate insecure registry
		if _, err := ValidateIndexName(r); err != nil {
			return err
		}
		if scheme, host, ok := strings.Cut(r, "://"); ok {
			switch strings.ToLower(scheme) {
			case "http", "https":
				log.G(context.TODO()).Warnf("insecure registry %[1]s should not contain '%[2]s' and '%[2]ss' has been removed from the insecure registry config", r, scheme)
				r = host
			default:
				// unsupported scheme
				return invalidParamf("insecure registry %s should not contain '://'", r)
			}
		}
		// Check if CIDR was passed to --insecure-registry
		ipnet, err := netip.ParsePrefix(r)
		if err == nil {
			insecureRegistryCIDRs[ipnet.Masked()] = struct{}{}
		} else {
			if err := validateHostPort(r); err != nil {
				return invalidParamWrapf(err, "insecure registry %s is not valid", r)
			}
			// Assume `host:port` if not CIDR.
			indexConfigs[r] = &registry.IndexInfo{
				Name:     r,
				Mirrors:  []string{},
				Secure:   false,
				Official: false,
			}
		}
	}

	// Configure public registry.
	indexConfigs[IndexName] = &registry.IndexInfo{
		Name:     IndexName,
		Mirrors:  config.Mirrors,
		Secure:   true,
		Official: true,
	}
	config.InsecureRegistryCIDRs = slices.Collect(maps.Keys(insecureRegistryCIDRs))
	slices.SortFunc(config.InsecureRegistryCIDRs, netip.Prefix.Compare)
	config.IndexConfigs = indexConfigs

	return nil
}

// isSecureIndex returns false if the provided indexName is part of the list of insecure registries.
// Insecure registries accept HTTP and/or accept HTTPS with certificates from unknown CAs.
//
// The list of insecure registries can contain an element with CIDR notation to specify a whole subnet.
// If all IP addresses of the registry specified by indexName are contained in one or more of those
// subnets, the registry is considered insecure. If any resolved IP address is not contained in an
// insecure subnet, the registry is considered secure.
//
// indexName should be a URL.Host (`host:port` or `host`) where the `host` part can be either a domain name
// or an IP address. If it is a domain name, then it will be resolved in order to check its IP addresses
// against the configured subnets. If resolution is not successful, isSecureIndex will only try to match
// the hostname to an element of insecureRegistries.
func (config *serviceConfig) isSecureIndex(indexName string) bool {
	// Check for configured index, first.  This is needed in case isSecureIndex
	// is called from anything besides newIndexInfo, in order to honor per-index configurations.
	if index, ok := config.IndexConfigs[indexName]; ok {
		return index.Secure
	}

	return !isCIDRMatch(config.InsecureRegistryCIDRs, indexName)
}

// for mocking in unit tests.
var lookupIP = net.LookupIP

// isCIDRMatch returns true if all IP addresses for urlHost match an element of
// cidrs. urlHost is a URL.Host ("host:port" or "host"), where the host part can
// be either a domain name or an IP address. Domain names are resolved before
// matching. If resolution fails, or no addresses are returned, false is returned.
func isCIDRMatch(cidrs []netip.Prefix, urlHost string) bool {
	if len(cidrs) == 0 {
		return false
	}

	host := urlHost
	if h, _, err := net.SplitHostPort(urlHost); err == nil {
		// urlHost includes a port; use only the host for matching.
		host = h
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		// host is an IP-address.
		return matchesCIDR(cidrs, ip)
	}

	// Try to resolve the host's IP-address.
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		// We failed to resolve the host; assume there's no match.
		return false
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok || !matchesCIDR(cidrs, addr) {
			return false
		}
	}
	return true
}

func matchesCIDR(cidrs []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range cidrs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ValidateMirror validates and normalizes an HTTP(S) registry mirror. It
// returns an error if the given mirrorURL is invalid, or the normalized
// format for the URL otherwise.
//
// It is used by the daemon to validate the daemon configuration.
func ValidateMirror(mirrorURL string) (string, error) {
	// Fast path for missing scheme, as url.Parse splits by ":", which can
	// cause the hostname to be considered the "scheme" when using "hostname:port".
	if scheme, _, ok := strings.Cut(mirrorURL, "://"); !ok || scheme == "" {
		return "", invalidParamf("invalid mirror: no scheme specified for %q: must use either 'https://' or 'http://'", mirrorURL)
	}
	uri, err := url.Parse(mirrorURL)
	if err != nil {
		return "", invalidParamWrapf(err, "invalid mirror: %q is not a valid URI", mirrorURL)
	}
	if uri.Scheme != "http" && uri.Scheme != "https" {
		return "", invalidParamf("invalid mirror: unsupported scheme %q in %q: must use either 'https://' or 'http://'", uri.Scheme, uri)
	}
	if uri.RawQuery != "" || uri.Fragment != "" {
		return "", invalidParamf("invalid mirror: query or fragment at end of the URI %q", uri)
	}
	if uri.User != nil {
		// strip password from output
		uri.User = url.UserPassword(uri.User.Username(), "xxxxx")
		return "", invalidParamf("invalid mirror: username/password not allowed in URI %q", uri)
	}
	return strings.TrimSuffix(mirrorURL, "/") + "/", nil
}

// ValidateIndexName validates an index name. It is used by the daemon to
// validate the daemon configuration.
func ValidateIndexName(val string) (string, error) {
	val = normalizeIndexName(val)
	if strings.HasPrefix(val, "-") || strings.HasSuffix(val, "-") {
		return "", invalidParamf("invalid index name (%s). Cannot begin or end with a hyphen", val)
	}
	return val, nil
}

func normalizeIndexName(val string) string {
	// TODO(thaJeztah): consider normalizing other known options, such as "(https://)registry-1.docker.io", "https://index.docker.io/v1/".
	// TODO: upstream this to check to reference package
	if val == "index.docker.io" {
		return "docker.io"
	}
	return val
}

func validateHostPort(s string) error {
	// Split host and port, and in case s can not be split, assume host only
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host = s
		port = ""
	}
	// If match against the `host:port` pattern fails,
	// it might be `IPv6:port`, which will be captured by net.ParseIP(host)
	if !validHostPortRegex.MatchString(s) && net.ParseIP(host) == nil {
		return invalidParamf("invalid host %q", host)
	}
	if port != "" {
		v, err := strconv.Atoi(port)
		if err != nil {
			return err
		}
		if v < 0 || v > 65535 {
			return invalidParamf("invalid port %q", port)
		}
	}
	return nil
}

// getAuthConfigKey special-cases using the full index address of the official
// index as the AuthConfig key, and uses the (host)name[:port] for private indexes.
func getAuthConfigKey(index *registry.IndexInfo) string {
	if index.Official {
		return IndexServer
	}
	return index.Name
}

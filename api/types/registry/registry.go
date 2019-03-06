package registry // import "github.com/docker/docker/api/types/registry"

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	// DefaultEndpoint for docker.io
	DefaultEndpoint = Endpoint{
		Address: "https://registry-1.docker.io",
		url: url.URL{
			Scheme: "https",
			Host:   "registry-1.docker.io",
		},
	}
)

// ServiceConfig stores daemon registry services configuration.
type ServiceConfig struct {
	AllowNondistributableArtifactsCIDRs     []*NetIPNet
	AllowNondistributableArtifactsHostnames []string
	InsecureRegistryCIDRs                   []*NetIPNet           `json:"InsecureRegistryCIDRs"`
	IndexConfigs                            map[string]*IndexInfo `json:"IndexConfigs"`
	Mirrors                                 []string
	Registries                              Registries
}

// Registries is a slice of type Registry.
type Registries []Registry

// Registry includes all data relevant for the lookup of push and pull
// endpoints.
type Registry struct {
	// Prefix will be used for the lookup of push and pull endpoints.
	Prefix string `json:"prefix"`
	// Pull is a slice of registries serving as pull endpoints.
	Pull []Endpoint `json:"pull,omitempty"`
	// Push is a slice of registries serving as push endpoints.
	Push []Endpoint `json:"push,omitempty"`
	// prefixLength is the length of prefix and avoids redundant length
	// calculations.
	prefixLength int
}

// Endpoint includes all data associated with a given registry endpoint.
type Endpoint struct {
	// Address is the endpoints base URL when assembling a repository in a
	// registry (e.g., "registry.com:5000/v2").
	Address string `json:"address"`
	// url is used during endpoint lookup and avoids to redundantly parse
	// Address when the Endpoint is used.
	url url.URL
	// InsecureSkipVerify: if true, TLS accepts any certificate presented
	// by the server and any host name in that certificate. In this mode,
	// TLS is susceptible to man-in-the-middle attacks. This should be used
	// only for testing
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// RewriteReference strips the prefix from ref and appends it to registry.
// If the prefix is empty, ref remains unchanged.  An error is returned if
// prefix doesn't prefix ref.
func RewriteReference(ref reference.Named, prefix string, registry *url.URL) (reference.Named, error) {
	// Sanity check the provided arguments
	if ref == nil {
		return nil, fmt.Errorf("provided reference is nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("provided registry is nil")
	}

	// don't rewrite the default endpoints
	if *registry == DefaultEndpoint.url {
		return ref, nil
	}

	if prefix == "" {
		return ref, nil
	}

	baseAddress := strings.TrimPrefix(registry.String(), registry.Scheme+"://")

	refStr := ref.String()
	if !strings.HasPrefix(refStr, prefix) {
		return nil, fmt.Errorf("unable to rewrite reference %q with prefix %q", refStr, prefix)
	}
	remainder := strings.TrimPrefix(refStr, prefix)
	remainder = strings.TrimPrefix(remainder, "/")
	baseAddress = strings.TrimSuffix(baseAddress, "/")

	newRefStr := baseAddress + "/" + remainder
	newRef, err := reference.ParseNamed(newRefStr)
	if err != nil {
		return nil, fmt.Errorf("unable to rewrite reference %q with prefix %q to %q: %v", refStr, prefix, newRefStr, err)
	}
	return newRef, nil
}

// GetURL returns the Endpoint's URL.
func (r *Endpoint) GetURL() *url.URL {
	// return the pointer of a copy
	url := r.url
	return &url
}

// CalcPrefix checks if the registry prefixes reference and returns the length
// of the prefix.  It returns 0 if the registry does not prefix reference.
func (r *Registry) CalcPrefix(reference string) int {
	if strings.HasPrefix(reference, r.Prefix) {
		return r.prefixLength
	}
	return 0
}

// FindRegistry returns the Registry with the longest Prefix for reference or
// nil if no Registry prefixes reference.
func (r Registries) FindRegistry(reference string) *Registry {
	var max int
	var reg *Registry

	max = 0
	reg = nil
	for i := range r {
		lenPref := r[i].CalcPrefix(reference)
		if lenPref > max {
			max = lenPref
			reg = &r[i]
		}
	}

	return reg
}

// Prepare sets up the Endpoint.
func (r *Endpoint) Prepare() error {
	if !strings.HasPrefix(r.Address, "http://") && !strings.HasPrefix(r.Address, "https://") {
		return fmt.Errorf("%s: address must start with %q or %q", r.Address, "http://", "https://")
	}

	u, err := url.Parse(r.Address)
	if err != nil {
		return err
	}
	r.url = *u
	return nil
}

// Prepare must be called on each new Registry.  It sets up all specified push
// and pull endpoints, and fills in defaults for the official "docker.io"
// registry.
func (r *Registry) Prepare() error {
	if len(r.Prefix) == 0 {
		return fmt.Errorf("Registry requires a prefix")
	}

	// return an error with the preifx doesn't end with a '/'
	if !strings.HasSuffix(r.Prefix, "/") {
		return fmt.Errorf("Prefix must end with a '/': prefixes match only at path boundaries")
	}
	r.prefixLength = len(r.Prefix)

	official := false
	// any prefix pointing to "docker.io/" is considered official
	if strings.HasPrefix(r.Prefix, "docker.io/") {
		official = true
	}

	prepareEndpoints := func(endpoints []Endpoint) ([]Endpoint, error) {
		addDefaultEndpoint := official
		for i := range endpoints {
			if err := endpoints[i].Prepare(); err != nil {
				return nil, err
			}
			if official && addDefaultEndpoint {
				if endpoints[i].Address == DefaultEndpoint.Address {
					addDefaultEndpoint = false
				}
			}
		}
		// if the default endpoint isn't specified, add it
		if addDefaultEndpoint {
			r.Pull = append(endpoints, DefaultEndpoint)
		}
		return endpoints, nil
	}

	var err error
	if r.Pull, err = prepareEndpoints(r.Pull); err != nil {
		return err
	}

	if r.Push, err = prepareEndpoints(r.Push); err != nil {
		return err
	}

	if len(r.Pull) == 0 && len(r.Push) == 0 {
		return fmt.Errorf("Registry with prefix %q without push or pull endpoints", r.Prefix)
	}

	return nil
}

// NetIPNet is the net.IPNet type, which can be marshalled and
// unmarshalled to JSON
type NetIPNet net.IPNet

// String returns the CIDR notation of ipnet
func (ipnet *NetIPNet) String() string {
	return (*net.IPNet)(ipnet).String()
}

// MarshalJSON returns the JSON representation of the IPNet
func (ipnet *NetIPNet) MarshalJSON() ([]byte, error) {
	return json.Marshal((*net.IPNet)(ipnet).String())
}

// UnmarshalJSON sets the IPNet from a byte array of JSON
func (ipnet *NetIPNet) UnmarshalJSON(b []byte) (err error) {
	var ipnetStr string
	if err = json.Unmarshal(b, &ipnetStr); err == nil {
		var cidr *net.IPNet
		if _, cidr, err = net.ParseCIDR(ipnetStr); err == nil {
			*ipnet = NetIPNet(*cidr)
		}
	}
	return
}

// IndexInfo contains information about a registry
//
// RepositoryInfo Examples:
// {
//   "Index" : {
//     "Name" : "docker.io",
//     "Mirrors" : ["https://registry-2.docker.io/v1/", "https://registry-3.docker.io/v1/"],
//     "Secure" : true,
//     "Official" : true,
//   },
//   "RemoteName" : "library/debian",
//   "LocalName" : "debian",
//   "CanonicalName" : "docker.io/debian"
//   "Official" : true,
// }
//
// {
//   "Index" : {
//     "Name" : "127.0.0.1:5000",
//     "Mirrors" : [],
//     "Secure" : false,
//     "Official" : false,
//   },
//   "RemoteName" : "user/repo",
//   "LocalName" : "127.0.0.1:5000/user/repo",
//   "CanonicalName" : "127.0.0.1:5000/user/repo",
//   "Official" : false,
// }
type IndexInfo struct {
	// Name is the name of the registry, such as "docker.io"
	Name string
	// Mirrors is a list of mirrors, expressed as URIs
	Mirrors []string
	// Secure is set to false if the registry is part of the list of
	// insecure registries. Insecure registries accept HTTP and/or accept
	// HTTPS with certificates from unknown CAs.
	Secure bool
	// Official indicates whether this is an official registry
	Official bool
}

// SearchResult describes a search result returned from a registry
type SearchResult struct {
	// StarCount indicates the number of stars this repository has
	StarCount int `json:"star_count"`
	// IsOfficial is true if the result is from an official repository.
	IsOfficial bool `json:"is_official"`
	// Name is the name of the repository
	Name string `json:"name"`
	// IsAutomated indicates whether the result is automated
	IsAutomated bool `json:"is_automated"`
	// Description is a textual description of the repository
	Description string `json:"description"`
}

// SearchResults lists a collection search results returned from a registry
type SearchResults struct {
	// Query contains the query string that generated the search results
	Query string `json:"query"`
	// NumResults indicates the number of results the query returned
	NumResults int `json:"num_results"`
	// Results is a slice containing the actual results for the search
	Results []SearchResult `json:"results"`
}

// DistributionInspect describes the result obtained from contacting the
// registry to retrieve image metadata
type DistributionInspect struct {
	// Descriptor contains information about the manifest, including
	// the content addressable digest
	Descriptor v1.Descriptor
	// Platforms contains the list of platforms supported by the image,
	// obtained by parsing the manifest
	Platforms []v1.Platform
}

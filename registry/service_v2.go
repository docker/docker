package registry // import "github.com/docker/docker/registry"

import (
	"crypto/tls"
	"net/url"

	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/sirupsen/logrus"
)

// lookupV2Endpoints returns a slice of APIEndpoints.  `lookup` is used for the
// lookup of the registry.  If `pull` is true, the pull endpoints are used
// instead of the push endpoints.
func (s *DefaultService) lookupV2Endpoints(hostname string, lookup string, pull bool) (endpoints []APIEndpoint, err error) {
	reg := s.config.Registries.FindRegistry(lookup)
	logrus.Infof("lookupV2Endpoints(%s, %s, %v)", hostname, lookup, pull)
	logrus.Infof("registry: %v", reg)

	var tlsConfig *tls.Config
	if reg != nil {
		var regEndpoints []registrytypes.Endpoint
		if pull {
			regEndpoints = reg.Pull
		} else {
			regEndpoints = reg.Push
		}

		lastIndex := len(regEndpoints) - 1
		for i, regEP := range regEndpoints {
			official := regEP.Address == registrytypes.DefaultEndpoint.Address
			regURL := regEP.GetURL()

			if official {
				tlsConfig = tlsconfig.ServerDefault()
			} else {
				tlsConfig, err = s.tlsConfigForMirror(regURL)
				if err != nil {
					return nil, err
				}
			}
			tlsConfig.InsecureSkipVerify = regEP.InsecureSkipVerify
			endpoints = append(endpoints, APIEndpoint{
				URL:          regURL,
				Version:      APIVersion2,
				Official:     official,
				TrimHostname: true,
				TLSConfig:    tlsConfig,
				Prefix:       reg.Prefix,
				// the last endpoint is not considered a mirror
				Mirror: i != lastIndex,
			})
		}
		return endpoints, nil
	}

	ana := allowNondistributableArtifacts(s.config, hostname)

	tlsConfig, err = s.tlsConfig(hostname)
	if err != nil {
		return nil, err
	}

	endpoints = []APIEndpoint{
		{
			URL: &url.URL{
				Scheme: "https",
				Host:   hostname,
			},
			Version:                        APIVersion2,
			AllowNondistributableArtifacts: ana,
			TrimHostname:                   true,
			TLSConfig:                      tlsConfig,
		},
	}

	if tlsConfig.InsecureSkipVerify {
		endpoints = append(endpoints, APIEndpoint{
			URL: &url.URL{
				Scheme: "http",
				Host:   hostname,
			},
			Version:                        APIVersion2,
			AllowNondistributableArtifacts: ana,
			TrimHostname:                   true,
			TLSConfig:                      tlsConfig,
		})
	}

	return endpoints, nil
}

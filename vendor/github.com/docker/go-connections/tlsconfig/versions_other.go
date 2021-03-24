// +build !go1.13

package tlsconfig

import (
	"crypto/tls"
)

// allTLSVersions lists all the TLS versions and is used by the code that validates
// a uint16 value as a TLS version.
var allTLSVersions = map[uint16]struct{}{
	tls.VersionTLS10: {},
	tls.VersionTLS11: {},
	tls.VersionTLS12: {},
}

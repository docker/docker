package sockets

import (
	"net"
	"os"
	"strings"
)

// GetProxyEnv allows access to the uppercase and the lowercase forms of
// proxy-related variables.  See the Go specification for details on these
// variables. https://golang.org/pkg/net/http/
func GetProxyEnv(key string) string {
	proxyValue := os.Getenv(strings.ToUpper(key))
	if proxyValue == "" {
		return os.Getenv(strings.ToLower(key))
	}
	return proxyValue
}

// DialerFromEnvironment was previously used to configure a net.Dialer to route
// connections through a SOCKS proxy.
// DEPRECATED: SOCKS proxies are now supported by configuring only
// http.Transport.Proxy, and no longer require changing http.Transport.Dial.
// Therefore, only sockets.ConfigureTransport() needs to be called, and any
// sockets.DialerFromEnvironment() calls can be dropped.
func DialerFromEnvironment(direct *net.Dialer) (*net.Dialer, error) {
	return direct, nil
}

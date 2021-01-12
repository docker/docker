// +build go1.8

package ws

import "crypto/tls"

func tlsCloneConfig(c *tls.Config) *tls.Config {
	return c.Clone()
}

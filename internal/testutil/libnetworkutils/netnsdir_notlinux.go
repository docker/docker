//go:build !linux

// Package libnetworkutils provides helpers for tests which create libnetwork
// controllers.
package libnetworkutils

import (
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
)

// OptionTempNetnsDir returns nil on platforms other than Linux, which have
// no network namespaces.
func OptionTempNetnsDir(testing.TB) config.Option {
	return nil
}

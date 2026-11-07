// Package libnetworkutils provides helpers for tests which create libnetwork
// controllers.
package libnetworkutils

import (
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
)

// OptionTempNetnsDir returns an option which makes the controller keep the
// network namespaces of its sandboxes in a [netnsutils.TempNetnsDir].
func OptionTempNetnsDir(t testing.TB) config.Option {
	t.Helper()
	return config.OptionNetnsDir(netnsutils.TempNetnsDir(t))
}

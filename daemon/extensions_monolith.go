package daemon

import (
	"github.com/moby/extensions"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
)

// monolithExtensionID identifies the builtin extension that exposes daemon
// behavior as extension points.
const monolithExtensionID = "org.mobyproject.daemon.monolith.v0"

// newMonolithExtension declares a builtin extension that exposes behavior of
// the daemon god object as extension points.
// It bridges the daemon's current shape and the extension framework, so
// extensions can use daemon behavior before the daemon is broken up.
//
// Use it as a last resort.
// Prefer making a point independent of the daemon: give it its own package
// and extension that builds on other points, like the bbolt storage, which
// obtains its root directory through the daemon config point.
// The long-term goal is to break up the daemon god object so that its
// capabilities become independent extensions and this declaration goes
// away.
func newMonolithExtension(d *Daemon) extensions.Extension {
	return extensions.New(extensions.Declaration{
		ID: monolithExtensionID,
		Providers: []extensions.Provider{
			containerstatev0.Point.Provide(containerStateWatcher{daemon: d}),
		},
	})
}

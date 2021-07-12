package listeners // import "github.com/docker/docker/daemon/listeners"

import (
	"crypto/tls"
	"net"
	"os"

	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/go-connections/sockets"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Init creates new listeners for the server.
// TODO: Clean up the fact that socketGroup and tlsConfig aren't always used.
func Init(proto, addr, socketGroup string, tlsConfig *tls.Config) ([]net.Listener, error) {
	ls := []net.Listener{}

	switch proto {
	case "fd":
		fds, err := listenFD(addr, tlsConfig)
		if err != nil {
			return nil, err
		}
		ls = append(ls, fds...)
	case "tcp":
		l, err := sockets.NewTCPSocket(addr, tlsConfig)
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	case "unix":
		gid, err := lookupGID(socketGroup)
		if err != nil {
			if socketGroup != "" {
				if socketGroup != defaultSocketGroup {
					return nil, err
				}
				logrus.Warnf("could not change group %s to %s: %v", addr, defaultSocketGroup, err)
			}
			gid = os.Getgid()
		}
		l, err := sockets.NewUnixSocket(addr, gid)
		if err != nil {
			return nil, errors.Wrapf(err, "can't create unix socket %s", addr)
		}
		if _, err := homedir.StickRuntimeDirContents([]string{addr}); err != nil {
			// StickRuntimeDirContents returns nil error if XDG_RUNTIME_DIR is just unset
			logrus.WithError(err).Warnf("cannot set sticky bit on socket %s under XDG_RUNTIME_DIR", addr)
		}
		ls = append(ls, l)
	default:
		return nil, errors.Errorf("invalid protocol format: %q", proto)
	}

	return ls, nil
}

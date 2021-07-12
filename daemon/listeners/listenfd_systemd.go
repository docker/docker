// +build linux,!no_systemd

package listeners // import "github.com/docker/docker/daemon/listeners"

import (
	"crypto/tls"
	"net"
	"strconv"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/pkg/errors"
)

// listenFD returns the specified socket activated files as a slice of
// net.Listeners or all of the activated files if "*" is given.
func listenFD(addr string, tlsConfig *tls.Config) ([]net.Listener, error) {
	var (
		err       error
		listeners []net.Listener
	)
	// socket activation
	if tlsConfig != nil {
		listeners, err = activation.TLSListeners(tlsConfig)
	} else {
		listeners, err = activation.Listeners()
	}
	if err != nil {
		return nil, err
	}

	if len(listeners) == 0 {
		return nil, errors.New("no sockets found via socket activation: make sure the service was started by systemd")
	}

	// default to all fds just like unix:// and tcp://
	if addr == "" || addr == "*" {
		return listeners, nil
	}

	fdNum, err := strconv.Atoi(addr)
	if err != nil {
		return nil, errors.Errorf("failed to parse systemd fd address: should be a number: %v", addr)
	}
	fdOffset := fdNum - 3
	if len(listeners) < fdOffset+1 {
		return nil, errors.New("too few socket activated files passed in by systemd")
	}
	if listeners[fdOffset] == nil {
		return nil, errors.Errorf("failed to listen on systemd activated file: fd %d", fdOffset+3)
	}
	for i, ls := range listeners {
		if i == fdOffset || ls == nil {
			continue
		}
		if err := ls.Close(); err != nil {
			return nil, errors.Wrapf(err, "failed to close systemd activated file: fd %d", fdOffset+3)
		}
	}
	return []net.Listener{listeners[fdOffset]}, nil
}

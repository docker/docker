package listeners // import "github.com/docker/docker/daemon/listeners"

import (
	"bytes"
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/go-connections/sockets"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const defaultSocketGroup = "docker"

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
		gid := os.Getgid()
		l, err := sockets.NewUnixSocket(addr, gid)
		if err != nil {
			return nil, errors.Wrapf(err, "can't create unix socket %s", addr)
		}
		if socketGroup != "" {
			out, err := exec.Command("chgrp", socketGroup, addr).CombinedOutput()
			if err != nil {
				msg := err.Error()
				if len(out) > 0 {
					msg = string(bytes.TrimSpace(out))
				}
				err = errors.Errorf("can't change group of unix socket %s: %s", addr, msg)
				if socketGroup != defaultSocketGroup {
					return nil, err
				}
				// "docker" group does not exist? Don't fail, just warn
				logrus.Warn(err)
			}
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

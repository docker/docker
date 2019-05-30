package portmapper

import (
	"errors"
	"net"
)

func newProxyCommand(proto string, hostIP net.IP, hostPort int, containerIP net.IP, containerPort int, proxyPath string) (userlandProxy, error) {
	return nil, errors.New("proxy is unsupported on windows")
}

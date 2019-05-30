package client

import (
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pkg/errors"
)

func dialer(address string, timeout time.Duration) (net.Conn, error) {
	addrParts := strings.SplitN(address, "://", 2)
	if len(addrParts) != 2 {
		return nil, errors.Errorf("invalid address %s", address)
	}
	switch addrParts[0] {
	case "npipe":
		address = strings.Replace(addrParts[1], "/", "\\", -1)
		return winio.DialPipe(address, &timeout)
	default:
		return net.DialTimeout(addrParts[0], addrParts[1], timeout)
	}
}

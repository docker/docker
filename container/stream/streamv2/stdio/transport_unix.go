// +build !windows

package stdio

import (
	"net"
	"time"
)

func dial(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}

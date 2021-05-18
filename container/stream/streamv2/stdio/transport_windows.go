package stdio

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
)

func dial(addr string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(addr, &timeout)
}

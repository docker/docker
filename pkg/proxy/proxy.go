package proxy

import (
	"fmt"
	"io"
	"net"
)

type (
	closeReader interface {
		CloseRead() error
	}
	closeWriter interface {
		CloseWrite() error
	}
	Proxy interface {
		// Start forwarding traffic back and forth the front and back-end
		// addresses.
		Run()
		// Stop forwarding traffic and close both ends of the Proxy.
		Close() error
		// Return the address on which the proxy is listening.
		FrontendAddr() net.Addr
		// Return the proxied address.
		BackendAddr() net.Addr
	}
)

func NewProxy(frontendAddr, backendAddr net.Addr) (Proxy, error) {
	switch frontendAddr.(type) {
	case *net.UDPAddr:
		return NewUDPProxy(frontendAddr.(*net.UDPAddr), backendAddr.(*net.UDPAddr))
	case *net.TCPAddr:
		return NewTCPProxy(frontendAddr.(*net.TCPAddr), backendAddr.(*net.TCPAddr))
	default:
		panic(fmt.Errorf("Unsupported protocol"))
	}
}

func goTransfert(dst io.Writer, src io.Reader) chan error {
	c := make(chan error)
	go func() {
		_, err := io.Copy(dst, src)
		e1 := dst.(closeReader).CloseRead()
		if err != nil {
			c <- err
		} else {
			c <- e1
		}
	}()
	return c
}

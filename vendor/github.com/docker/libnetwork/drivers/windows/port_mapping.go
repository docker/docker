// +build windows

package windows

import (
	"bytes"
	"errors"
	"fmt"
	"net"

	"github.com/docker/libnetwork/portmapper"
	"github.com/docker/libnetwork/types"
	"github.com/ishidawataru/sctp"
	"github.com/sirupsen/logrus"
)

const (
	maxAllocatePortAttempts = 10
)

// ErrUnsupportedAddressType is returned when the specified address type is not supported.
type ErrUnsupportedAddressType string

func (uat ErrUnsupportedAddressType) Error() string {
	return fmt.Sprintf("unsupported address type: %s", string(uat))
}

// AllocatePorts allocates ports specified in bindings from the portMapper
func AllocatePorts(portMapper *portmapper.PortMapper, bindings []types.PortBinding, containerIP net.IP) ([]types.PortBinding, error) {
	bs := make([]types.PortBinding, 0, len(bindings))
	for _, c := range bindings {
		b := c.GetCopy()
		if err := allocatePort(portMapper, &b, containerIP); err != nil {
			// On allocation failure, release previously allocated ports. On cleanup error, just log a warning message
			if cuErr := ReleasePorts(portMapper, bs); cuErr != nil {
				logrus.Warnf("Upon allocation failure for %v, failed to clear previously allocated port bindings: %v", b, cuErr)
			}
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, nil
}

func allocatePort(portMapper *portmapper.PortMapper, bnd *types.PortBinding, containerIP net.IP) error {
	var (
		host net.Addr
		err  error
	)

	// Windows does not support a host ip for port bindings (this is validated in ConvertPortBindings()).
	// If the HostIP is nil, force it to be 0.0.0.0 for use as the key in portMapper.
	if bnd.HostIP == nil {
		bnd.HostIP = net.IPv4zero
	}

	// Store the container interface address in the operational binding
	bnd.IP = containerIP

	// Adjust HostPortEnd if this is not a range.
	if bnd.HostPortEnd == 0 {
		bnd.HostPortEnd = bnd.HostPort
	}

	// Construct the container side transport address
	container, err := bnd.ContainerAddr()
	if err != nil {
		return err
	}

	// Try up to maxAllocatePortAttempts times to get a port that's not already allocated.
	for i := 0; i < maxAllocatePortAttempts; i++ {
		if host, err = portMapper.MapRange(container, bnd.HostIP, int(bnd.HostPort), int(bnd.HostPortEnd), false); err == nil {
			break
		}
		// There is no point in immediately retrying to map an explicitly chosen port.
		if bnd.HostPort != 0 {
			logrus.Warnf("Failed to allocate and map port %d-%d: %s", bnd.HostPort, bnd.HostPortEnd, err)
			break
		}
		logrus.Warnf("Failed to allocate and map port: %s, retry: %d", err, i+1)
	}
	if err != nil {
		return err
	}

	// Save the host port (regardless it was or not specified in the binding)
	switch netAddr := host.(type) {
	case *net.TCPAddr:
		bnd.HostPort = uint16(host.(*net.TCPAddr).Port)
		break
	case *net.UDPAddr:
		bnd.HostPort = uint16(host.(*net.UDPAddr).Port)
		break
	case *sctp.SCTPAddr:
		bnd.HostPort = uint16(host.(*sctp.SCTPAddr).Port)
		break
	default:
		// For completeness
		return ErrUnsupportedAddressType(fmt.Sprintf("%T", netAddr))
	}
	//Windows does not support host port ranges.
	bnd.HostPortEnd = bnd.HostPort
	return nil
}

// ReleasePorts releases ports specified in bindings from the portMapper
func ReleasePorts(portMapper *portmapper.PortMapper, bindings []types.PortBinding) error {
	var errorBuf bytes.Buffer

	// Attempt to release all port bindings, do not stop on failure
	for _, m := range bindings {
		if err := releasePort(portMapper, m); err != nil {
			errorBuf.WriteString(fmt.Sprintf("\ncould not release %v because of %v", m, err))
		}
	}

	if errorBuf.Len() != 0 {
		return errors.New(errorBuf.String())
	}
	return nil
}

func releasePort(portMapper *portmapper.PortMapper, bnd types.PortBinding) error {
	// Construct the host side transport address
	host, err := bnd.HostAddr()
	if err != nil {
		return err
	}
	return portMapper.Unmap(host)
}

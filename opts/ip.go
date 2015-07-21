package opts

import (
	"fmt"
	"net"
)

// IPOpt type that hold an IP
type IPOpt struct {
	*net.IP
}

// NewIPOpt returns a new IPOpt from a string of an IP.
func NewIPOpt(ref *net.IP, defaultVal string) *IPOpt {
	o := &IPOpt{
		IP: ref,
	}
	o.Set(defaultVal)
	return o
}

// Set sets an IPv4 or IPv6 address from a given string. If the given
// string is not parsable as an IP address it will return an error.
func (o *IPOpt) Set(val string) error {
	ip := net.ParseIP(val)
	if ip == nil {
		return fmt.Errorf("%s is not an ip address", val)
	}
	*o.IP = ip
	return nil
}

// String returns the IP address stored in the IPOpt. If IPOpt is a
// nil pointer, it returns an empty string.
func (o *IPOpt) String() string {
	if *o.IP == nil {
		return ""
	}
	return o.IP.String()
}

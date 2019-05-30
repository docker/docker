package srslog

import (
	"crypto/tls"
	"net"
)

// dialerFunctionWrapper is a simple object that consists of a dialer function
// and its name. This is primarily for testing, so we can make sure that the
// getDialer method returns the correct dialer function. However, if you ever
// find that you need to check which dialer function you have, this would also
// be useful for you without having to use reflection.
type dialerFunctionWrapper struct {
	Name   string
	Dialer func() (serverConn, string, error)
}

// Call the wrapped dialer function and return its return values.
func (df dialerFunctionWrapper) Call() (serverConn, string, error) {
	return df.Dialer()
}

// getDialer returns a "dialer" function that can be called to connect to a
// syslog server.
//
// Each dialer function is responsible for dialing the remote host and returns
// a serverConn, the hostname (or a default if the Writer has not specified a
// hostname), and an error in case dialing fails.
//
// The reason for separate dialers is that different network types may need
// to dial their connection differently, yet still provide a net.Conn interface
// that you can use once they have dialed. Rather than an increasingly long
// conditional, we have a map of network -> dialer function (with a sane default
// value), and adding a new network type is as easy as writing the dialer
// function and adding it to the map.
func (w *Writer) getDialer() dialerFunctionWrapper {
	dialers := map[string]dialerFunctionWrapper{
		"":        dialerFunctionWrapper{"unixDialer", w.unixDialer},
		"tcp+tls": dialerFunctionWrapper{"tlsDialer", w.tlsDialer},
		"custom":  dialerFunctionWrapper{"customDialer", w.customDialer},
	}
	dialer, ok := dialers[w.network]
	if !ok {
		dialer = dialerFunctionWrapper{"basicDialer", w.basicDialer}
	}
	return dialer
}

// unixDialer uses the unixSyslog method to open a connection to the syslog
// daemon running on the local machine.
func (w *Writer) unixDialer() (serverConn, string, error) {
	sc, err := unixSyslog()
	hostname := w.hostname
	if hostname == "" {
		hostname = "localhost"
	}
	return sc, hostname, err
}

// tlsDialer connects to TLS over TCP, and is used for the "tcp+tls" network
// type.
func (w *Writer) tlsDialer() (serverConn, string, error) {
	c, err := tls.Dial("tcp", w.raddr, w.tlsConfig)
	var sc serverConn
	hostname := w.hostname
	if err == nil {
		sc = &netConn{conn: c}
		if hostname == "" {
			hostname = c.LocalAddr().String()
		}
	}
	return sc, hostname, err
}

// basicDialer is the most common dialer for syslog, and supports both TCP and
// UDP connections.
func (w *Writer) basicDialer() (serverConn, string, error) {
	c, err := net.Dial(w.network, w.raddr)
	var sc serverConn
	hostname := w.hostname
	if err == nil {
		sc = &netConn{conn: c}
		if hostname == "" {
			hostname = c.LocalAddr().String()
		}
	}
	return sc, hostname, err
}

// customDialer uses the custom dialer when the Writer was created
// giving developers total control over how connections are made and returned.
// Note it does not check if cdialer is nil, as it should only be referenced from getDialer.
func (w *Writer) customDialer() (serverConn, string, error) {
	c, err := w.customDial(w.network, w.raddr)
	var sc serverConn
	hostname := w.hostname
	if err == nil {
		sc = &netConn{conn: c}
		if hostname == "" {
			hostname = c.LocalAddr().String()
		}
	}
	return sc, hostname, err
}

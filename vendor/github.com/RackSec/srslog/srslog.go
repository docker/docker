package srslog

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"os"
)

// This interface allows us to work with both local and network connections,
// and enables Solaris support (see syslog_unix.go).
type serverConn interface {
	writeString(framer Framer, formatter Formatter, p Priority, hostname, tag, s string) error
	close() error
}

// DialFunc is the function signature to be used for a custom dialer callback
// with DialWithCustomDialer
type DialFunc func(string, string) (net.Conn, error)

// New establishes a new connection to the system log daemon.  Each
// write to the returned Writer sends a log message with the given
// priority and prefix.
func New(priority Priority, tag string) (w *Writer, err error) {
	return Dial("", "", priority, tag)
}

// Dial establishes a connection to a log daemon by connecting to
// address raddr on the specified network.  Each write to the returned
// Writer sends a log message with the given facility, severity and
// tag.
// If network is empty, Dial will connect to the local syslog server.
func Dial(network, raddr string, priority Priority, tag string) (*Writer, error) {
	return DialWithTLSConfig(network, raddr, priority, tag, nil)
}

// ErrNilDialFunc is returned from DialWithCustomDialer when a nil DialFunc is passed,
// avoiding a nil pointer deference panic.
var ErrNilDialFunc = errors.New("srslog: nil DialFunc passed to DialWithCustomDialer")

// DialWithCustomDialer establishes a connection by calling customDial.
// Each write to the returned Writer sends a log message with the given facility, severity and tag.
// Network must be "custom" in order for this package to use customDial.
// While network and raddr will be passed to customDial, it is allowed for customDial to ignore them.
// If customDial is nil, this function returns ErrNilDialFunc.
func DialWithCustomDialer(network, raddr string, priority Priority, tag string, customDial DialFunc) (*Writer, error) {
	if customDial == nil {
		return nil, ErrNilDialFunc
	}
	return dialAllParameters(network, raddr, priority, tag, nil, customDial)
}

// DialWithTLSCertPath establishes a secure connection to a log daemon by connecting to
// address raddr on the specified network. It uses certPath to load TLS certificates and configure
// the secure connection.
func DialWithTLSCertPath(network, raddr string, priority Priority, tag, certPath string) (*Writer, error) {
	serverCert, err := ioutil.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	return DialWithTLSCert(network, raddr, priority, tag, serverCert)
}

// DialWIthTLSCert establishes a secure connection to a log daemon by connecting to
// address raddr on the specified network. It uses serverCert to load a TLS certificate
// and configure the secure connection.
func DialWithTLSCert(network, raddr string, priority Priority, tag string, serverCert []byte) (*Writer, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(serverCert)
	config := tls.Config{
		RootCAs: pool,
	}

	return DialWithTLSConfig(network, raddr, priority, tag, &config)
}

// DialWithTLSConfig establishes a secure connection to a log daemon by connecting to
// address raddr on the specified network. It uses tlsConfig to configure the secure connection.
func DialWithTLSConfig(network, raddr string, priority Priority, tag string, tlsConfig *tls.Config) (*Writer, error) {
	return dialAllParameters(network, raddr, priority, tag, tlsConfig, nil)
}

// implementation of the various functions above
func dialAllParameters(network, raddr string, priority Priority, tag string, tlsConfig *tls.Config, customDial DialFunc) (*Writer, error) {
	if err := validatePriority(priority); err != nil {
		return nil, err
	}

	if tag == "" {
		tag = os.Args[0]
	}
	hostname, _ := os.Hostname()

	w := &Writer{
		priority:   priority,
		tag:        tag,
		hostname:   hostname,
		network:    network,
		raddr:      raddr,
		tlsConfig:  tlsConfig,
		customDial: customDial,
	}

	_, err := w.connect()
	if err != nil {
		return nil, err
	}
	return w, err
}

// NewLogger creates a log.Logger whose output is written to
// the system log service with the specified priority. The logFlag
// argument is the flag set passed through to log.New to create
// the Logger.
func NewLogger(p Priority, logFlag int) (*log.Logger, error) {
	s, err := New(p, "")
	if err != nil {
		return nil, err
	}
	return log.New(s, "", logFlag), nil
}

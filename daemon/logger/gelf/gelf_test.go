// +build linux

package gelf // import "github.com/docker/docker/daemon/logger/gelf"

import (
	"net"
	"testing"

	"github.com/docker/docker/daemon/logger"
)

// Validate parseAddress
func TestParseAddress(t *testing.T) {
	url, err := parseAddress("udp://127.0.0.1:12201")
	if err != nil {
		t.Fatal(err)
	}
	if url.String() != "udp://127.0.0.1:12201" {
		t.Fatalf("Expected address udp://127.0.0.1:12201, got %s", url.String())
	}

	_, err = parseAddress("127.0.0.1:12201")
	if err == nil {
		t.Fatal("Expected error requiring protocol")
	}

	_, err = parseAddress("http://127.0.0.1:12201")
	if err == nil {
		t.Fatal("Expected error restricting protocol")
	}
}

// Validate TCP options
func TestTCPValidateLogOpt(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"gelf-address": "tcp://127.0.0.1:12201",
	})
	if err != nil {
		t.Fatal("Expected TCP to be supported")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":           "tcp://127.0.0.1:12201",
		"gelf-compression-level": "9",
	})
	if err == nil {
		t.Fatal("Expected TCP to reject compression level")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":          "tcp://127.0.0.1:12201",
		"gelf-compression-type": "gzip",
	})
	if err == nil {
		t.Fatal("Expected TCP to reject compression type")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":             "tcp://127.0.0.1:12201",
		"gelf-tcp-max-reconnect":   "5",
		"gelf-tcp-reconnect-delay": "10",
	})
	if err != nil {
		t.Fatal("Expected TCP reconnect to be a valid parameters")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":             "tcp://127.0.0.1:12201",
		"gelf-tcp-max-reconnect":   "-1",
		"gelf-tcp-reconnect-delay": "-3",
	})
	if err == nil {
		t.Fatal("Expected negative TCP reconnect to be rejected")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":             "tcp://127.0.0.1:12201",
		"gelf-tcp-max-reconnect":   "invalid",
		"gelf-tcp-reconnect-delay": "invalid",
	})
	if err == nil {
		t.Fatal("Expected TCP reconnect to be required to be an int")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":             "udp://127.0.0.1:12201",
		"gelf-tcp-max-reconnect":   "1",
		"gelf-tcp-reconnect-delay": "3",
	})
	if err == nil {
		t.Fatal("Expected TCP reconnect to be invalid for UDP")
	}
}

// Validate UDP options
func TestUDPValidateLogOpt(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"gelf-address":           "udp://127.0.0.1:12201",
		"tag":                    "testtag",
		"labels":                 "testlabel",
		"labels-regex":           "testlabel-regex",
		"env":                    "testenv",
		"env-regex":              "testenv-regex",
		"gelf-compression-level": "9",
		"gelf-compression-type":  "gzip",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":           "udp://127.0.0.1:12201",
		"gelf-compression-level": "ultra",
		"gelf-compression-type":  "zlib",
	})
	if err == nil {
		t.Fatal("Expected compression level error")
	}

	err = ValidateLogOpt(map[string]string{
		"gelf-address":          "udp://127.0.0.1:12201",
		"gelf-compression-type": "rar",
	})
	if err == nil {
		t.Fatal("Expected compression type error")
	}

	err = ValidateLogOpt(map[string]string{
		"invalid": "invalid",
	})
	if err == nil {
		t.Fatal("Expected unknown option error")
	}

	err = ValidateLogOpt(map[string]string{})
	if err == nil {
		t.Fatal("Expected required parameter error")
	}
}

// Validate newGELFTCPWriter
func TestNewGELFTCPWriter(t *testing.T) {
	address := "127.0.0.1:0"
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}

	url := "tcp://" + listener.Addr().String()
	info := logger.Info{
		Config: map[string]string{
			"gelf-address":             url,
			"gelf-tcp-max-reconnect":   "0",
			"gelf-tcp-reconnect-delay": "0",
			"tag":                      "{{.ID}}",
		},
		ContainerID: "12345678901234567890",
	}

	writer, err := newGELFTCPWriter(listener.Addr().String(), info)
	if err != nil {
		t.Fatal(err)
	}

	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Validate newGELFUDPWriter
func TestNewGELFUDPWriter(t *testing.T) {
	address := "127.0.0.1:0"
	info := logger.Info{
		Config: map[string]string{
			"gelf-address":           "udp://127.0.0.1:0",
			"gelf-compression-level": "5",
			"gelf-compression-type":  "gzip",
		},
	}

	writer, err := newGELFUDPWriter(address, info)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Validate New for TCP
func TestNewTCP(t *testing.T) {
	address := "127.0.0.1:0"
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}

	url := "tcp://" + listener.Addr().String()
	info := logger.Info{
		Config: map[string]string{
			"gelf-address":             url,
			"gelf-tcp-max-reconnect":   "0",
			"gelf-tcp-reconnect-delay": "0",
		},
		ContainerID: "12345678901234567890",
	}

	logger, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	err = logger.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// Validate New for UDP
func TestNewUDP(t *testing.T) {
	info := logger.Info{
		Config: map[string]string{
			"gelf-address":           "udp://127.0.0.1:0",
			"gelf-compression-level": "5",
			"gelf-compression-type":  "gzip",
		},
		ContainerID: "12345678901234567890",
	}

	logger, err := New(info)
	if err != nil {
		t.Fatal(err)
	}

	err = logger.Close()
	if err != nil {
		t.Fatal(err)
	}
}

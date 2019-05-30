package syslog // import "github.com/docker/docker/daemon/logger/syslog"

import (
	"net"
	"reflect"
	"testing"

	syslog "github.com/RackSec/srslog"
)

func functionMatches(expectedFun interface{}, actualFun interface{}) bool {
	return reflect.ValueOf(expectedFun).Pointer() == reflect.ValueOf(actualFun).Pointer()
}

func TestParseLogFormat(t *testing.T) {
	formatter, framer, err := parseLogFormat("rfc5424", "udp")
	if err != nil || !functionMatches(rfc5424formatterWithAppNameAsTag, formatter) ||
		!functionMatches(syslog.DefaultFramer, framer) {
		t.Fatal("Failed to parse rfc5424 format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("rfc5424", "tcp+tls")
	if err != nil || !functionMatches(rfc5424formatterWithAppNameAsTag, formatter) ||
		!functionMatches(syslog.RFC5425MessageLengthFramer, framer) {
		t.Fatal("Failed to parse rfc5424 format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("rfc5424micro", "udp")
	if err != nil || !functionMatches(rfc5424microformatterWithAppNameAsTag, formatter) ||
		!functionMatches(syslog.DefaultFramer, framer) {
		t.Fatal("Failed to parse rfc5424 (microsecond) format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("rfc5424micro", "tcp+tls")
	if err != nil || !functionMatches(rfc5424microformatterWithAppNameAsTag, formatter) ||
		!functionMatches(syslog.RFC5425MessageLengthFramer, framer) {
		t.Fatal("Failed to parse rfc5424 (microsecond) format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("rfc3164", "")
	if err != nil || !functionMatches(syslog.RFC3164Formatter, formatter) ||
		!functionMatches(syslog.DefaultFramer, framer) {
		t.Fatal("Failed to parse rfc3164 format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("", "")
	if err != nil || !functionMatches(syslog.UnixFormatter, formatter) ||
		!functionMatches(syslog.DefaultFramer, framer) {
		t.Fatal("Failed to parse empty format", err, formatter, framer)
	}

	formatter, framer, err = parseLogFormat("invalid", "")
	if err == nil {
		t.Fatal("Failed to parse invalid format", err, formatter, framer)
	}
}

func TestValidateLogOptEmpty(t *testing.T) {
	emptyConfig := make(map[string]string)
	if err := ValidateLogOpt(emptyConfig); err != nil {
		t.Fatal("Failed to parse empty config", err)
	}
}

func TestValidateSyslogAddress(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"syslog-address": "this is not an uri",
	})
	if err == nil {
		t.Fatal("Expected error with invalid uri")
	}

	// File exists
	err = ValidateLogOpt(map[string]string{
		"syslog-address": "unix:///",
	})
	if err != nil {
		t.Fatal(err)
	}

	// File does not exist
	err = ValidateLogOpt(map[string]string{
		"syslog-address": "unix:///does_not_exist",
	})
	if err == nil {
		t.Fatal("Expected error when address is non existing file")
	}

	// accepts udp and tcp URIs
	err = ValidateLogOpt(map[string]string{
		"syslog-address": "udp://1.2.3.4",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = ValidateLogOpt(map[string]string{
		"syslog-address": "tcp://1.2.3.4",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseAddressDefaultPort(t *testing.T) {
	_, address, err := parseAddress("tcp://1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}

	_, port, _ := net.SplitHostPort(address)
	if port != "514" {
		t.Fatalf("Expected to default to port 514. It used port %s", port)
	}
}

func TestValidateSyslogFacility(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"syslog-facility": "Invalid facility",
	})
	if err == nil {
		t.Fatal("Expected error if facility level is invalid")
	}
}

func TestValidateLogOptSyslogFormat(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"syslog-format": "Invalid format",
	})
	if err == nil {
		t.Fatal("Expected error if format is invalid")
	}
}

func TestValidateLogOpt(t *testing.T) {
	err := ValidateLogOpt(map[string]string{
		"env":                    "http://127.0.0.1",
		"env-regex":              "abc",
		"labels":                 "labelA",
		"labels-regex":           "def",
		"syslog-address":         "udp://1.2.3.4:1111",
		"syslog-facility":        "daemon",
		"syslog-tls-ca-cert":     "/etc/ca-certificates/custom/ca.pem",
		"syslog-tls-cert":        "/etc/ca-certificates/custom/cert.pem",
		"syslog-tls-key":         "/etc/ca-certificates/custom/key.pem",
		"syslog-tls-skip-verify": "true",
		"tag":                    "true",
		"syslog-format":          "rfc3164",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = ValidateLogOpt(map[string]string{
		"not-supported-option": "a",
	})
	if err == nil {
		t.Fatal("Expecting error on unsupported options")
	}
}

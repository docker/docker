// +build linux,!no_systemd

package iptables

import (
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func initFirewalld() {
	if err := FirewalldInit(); err != nil {
		logrus.Debugf("Fail to initialize firewalld: %v, using raw iptables instead", err)
	}
}

// Raw calls 'iptables' system command, passing supplied arguments.
func (iptable IPTable) Raw(args ...string) ([]byte, error) {
	if firewalldRunning {
		// select correct IP version for firewalld
		ipv := Iptables
		if iptable.Version == IPv6 {
			ipv = IP6Tables
		}

		startTime := time.Now()
		output, err := Passthrough(ipv, args...)
		if err == nil || !strings.Contains(err.Error(), "was not provided by any .service files") {
			return filterOutput(startTime, output, args...), err
		}
	}
	return iptable.raw(args...)
}

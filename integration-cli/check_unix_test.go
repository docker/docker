package main

import (
	"testing"

	"gotest.tools/icmd"
)

func createInterface(c *testing.T, ifType string, ifName string, ipNet string) {
	icmd.RunCommand("ip", "link", "add", "name", ifName, "type", ifType).Assert(c, icmd.Success)
	icmd.RunCommand("ifconfig", ifName, ipNet, "up").Assert(c, icmd.Success)
}

func deleteInterface(c *testing.T, ifName string) {
	if icmd.RunCommand("ip", "link", "show", ifName).ExitCode != 0 {
		return
	}
	icmd.RunCommand("ip", "link", "delete", ifName).Assert(c, icmd.Success)
	icmd.RunCommand("iptables", "-t", "nat", "--flush").Assert(c, icmd.Success)
	icmd.RunCommand("iptables", "--flush").Assert(c, icmd.Success)
}

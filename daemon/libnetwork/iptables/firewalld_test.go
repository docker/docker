//go:build linux

package iptables

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"testing"

	"github.com/godbus/dbus/v5"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func skipIfNoFirewalld(t *testing.T) {
	t.Helper()
	conn, err := dbus.SystemBus()
	if err != nil {
		t.Skipf("cannot connect to D-bus system bus: %v", err)
	}
	var zone string
	err = conn.Object(dbusInterface, dbusPath).Call(dbusInterface+".getDefaultZone", 0).Store(&zone)
	if err != nil {
		t.Skipf("firewalld is not running: %v", err)
	}
}

func TestUsingFirewalld(t *testing.T) {
	skipIfNoFirewalld(t)
	if !UsingFirewalld() {
		t.Fatal("firewalld is running, but UsingFirewalld() is false")
	}
}

func TestReloaded(t *testing.T) {
	iptable := GetIptable(IPv4)
	fwdChain, err := iptable.NewChain("FWD", Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer fwdChain.Remove()

	// This jump from the FORWARD chain prevents FWD from being deleted by
	// "iptables -X", called from fwdChain.Remove().
	err = iptable.EnsureJumpRule("filter", "FORWARD", "FWD")
	if err != nil {
		t.Fatal(err)
	}
	defer iptable.Raw("-D", "FORWARD", "-j", "FWD")

	// copy-pasted from iptables_test:TestLink
	ip1 := netip.MustParseAddr("192.168.1.1")
	ip2 := netip.MustParseAddr("192.168.1.2")
	const port = 1234
	const proto = "tcp"

	err = fwdChain.Link(Append, ip1, ip2, port, proto, bridgeName)
	if err != nil {
		t.Fatal(err)
	} else {
		// to be re-called again later
		OnReloaded(func() { fwdChain.Link(Append, ip1, ip2, port, proto, bridgeName) })
	}

	rule1 := []string{
		"-i", bridgeName,
		"-o", bridgeName,
		"-p", proto,
		"-s", ip1.String(),
		"-d", ip2.String(),
		"--dport", strconv.Itoa(port),
		"-j", "ACCEPT",
	}

	if !iptable.Exists(fwdChain.Table, fwdChain.Name, rule1...) {
		t.Fatal("rule1 does not exist")
	}

	// flush all rules
	fwdChain.Remove()

	reloaded()

	// make sure the rules have been recreated
	if !iptable.Exists(fwdChain.Table, fwdChain.Name, rule1...) {
		t.Fatal("rule1 hasn't been recreated")
	}
}

func TestDBusConnectionChanged(t *testing.T) {
	saved := onReloaded
	t.Cleanup(func() { onReloaded = saved })

	tests := []struct {
		name               string
		service            string
		oldOwner, newOwner string
		expCalls           int
	}{
		{name: "firewalld started", service: dbusInterface, newOwner: ":1.42", expCalls: 1},
		{name: "firewalld stopped", service: dbusInterface, oldOwner: ":1.42", expCalls: 1},
		{name: "other service stopped", service: "org.example.Other", oldOwner: ":1.43", expCalls: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			onReloaded = nil
			var calls int
			OnReloaded(func() { calls++ })
			before := FirewalldReloadedAt()

			dbusConnectionChanged([]any{tc.service, tc.oldOwner, tc.newOwner})

			assert.Check(t, is.Equal(calls, tc.expCalls))
			assert.Check(t, FirewalldReloadedAt().After(before) == (tc.expCalls > 0))
		})
	}
}

func TestPassthrough(t *testing.T) {
	skipIfNoFirewalld(t)
	rule1 := []string{
		"-A", "INPUT",
		"-i", "lo",
		"-p", "udp",
		"--dport", "123",
		"-j", "ACCEPT",
	}

	if !UsingFirewalld() {
		t.Fatal("firewalld is running, but UsingFirewalld() is false")
	}
	_, err := passthrough(IPv4, rule1...)
	if err != nil {
		t.Fatal(err)
	}
	if !GetIptable(IPv4).Exists(Filter, rule1[1], rule1[2:]...) {
		t.Fatal("rule1 does not exist")
	}
	rule1[0] = "-D"
	_, err = passthrough(IPv4, rule1...)
	if err != nil {
		t.Fatal(err)
	}
	if GetIptable(IPv4).Exists(Filter, rule1[1], rule1[2:]...) {
		t.Fatal("rule1 still exists")
	}
}

func TestFirewalldErrorCode(t *testing.T) {
	fwErr := func(msg string) error {
		return dbus.Error{Name: dbusInterface + ".Exception", Body: []any{msg}}
	}
	testcases := []struct {
		name string
		err  error
		want string
	}{
		{name: "code and message", err: fwErr("ZONE_CONFLICT: 'br0' already bound to 'drop'"), want: "ZONE_CONFLICT"},
		{name: "code only", err: fwErr("NAME_CONFLICT"), want: "NAME_CONFLICT"},
		{name: "wrapped", err: fmt.Errorf("wrapped: %w", fwErr("ZONE_ALREADY_SET: br0")), want: "ZONE_ALREADY_SET"},
		{name: "other dbus error", err: dbus.Error{Name: dbus.ErrMsgUnknownMethod.Name, Body: []any{"NAME_CONFLICT: x"}}, want: ""},
		{name: "not a dbus error", err: errors.New("ZONE_CONFLICT: x"), want: ""},
		{name: "nil", err: nil, want: ""},
	}
	for _, tc := range testcases {
		assert.Check(t, is.Equal(firewalldErrorCode(tc.err), tc.want), tc.name)
	}
}

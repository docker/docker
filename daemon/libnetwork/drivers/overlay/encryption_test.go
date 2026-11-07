//go:build linux

package overlay

import (
	"encoding/binary"
	"hash/fnv"
	"net"
	"net/netip"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func legacyBuildSPI(src, dst net.IP, st uint32) int {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, st)
	h := fnv.New32a()
	h.Write(src)
	h.Write(b)
	h.Write(dst)
	return int(binary.BigEndian.Uint32(h.Sum(nil)))
}

func TestBuildSPI(t *testing.T) {
	cases := []struct {
		src, dst string
		st       uint32
	}{
		{"1.2.3.4", "5.6.7.8", 1234},
		{"::ffff:1.2.3.4", "::ffff:5.6.7.8", 1234},
		{"10.0.0.1", "2001:db8::1", 5678},
		{"2002::abcd:1", "172.15.14.13", 54321},
		{"2002:db8::42", "2002:db8::69", 9999},
	}
	for _, tc := range cases {
		// The legacy buildSPI function is sensitive to whether src and
		// dst are in 4-byte or 16-byte form. Versions of the driver
		// using this function always pass the results of net.ParseIP(),
		// which always parses the address into 16-byte form.
		expected := legacyBuildSPI(net.ParseIP(tc.src), net.ParseIP(tc.dst), tc.st)

		src, dst := netip.MustParseAddr(tc.src), netip.MustParseAddr(tc.dst)
		actual := buildSPI(src, dst, tc.st)
		if expected != actual {
			t.Errorf("buildSPI(%v, %v, %v) = %v; want %v", src, dst, tc.st, actual, expected)
		}
		actual = buildSPI(src.Unmap(), dst.Unmap(), tc.st)
		if expected != actual {
			t.Errorf("buildSPI(%v, %v, %v) = %v; want %v", src.Unmap(), dst.Unmap(), tc.st, actual, expected)
		}
	}
}

func TestMaxMTU(t *testing.T) {
	v4 := netip.MustParseAddr("192.0.2.1")
	v6 := netip.MustParseAddr("2001:db8::1")
	tests := []struct {
		name    string
		advAddr netip.Addr
		mtu     int
		secure  bool
		want    int
	}{
		{name: "ipv4", advAddr: v4, want: 1450},
		{name: "ipv6", advAddr: v6, want: 1430},
		{name: "unknown transport", want: 1430},
		{name: "ipv4 custom mtu", advAddr: v4, mtu: 9000, want: 8950},
		{name: "ipv6 custom mtu", advAddr: v6, mtu: 9000, want: 8930},
		{name: "ipv4 encrypted", advAddr: v4, secure: true, want: 1424},
		{name: "ipv6 encrypted", advAddr: v6, secure: true, want: 1404},
		{name: "unknown transport encrypted", secure: true, want: 1404},
		{name: "ipv4 encrypted unaligned", advAddr: v4, mtu: 1502, secure: true, want: 1424},
		{name: "ipv6 encrypted unaligned", advAddr: v6, mtu: 1502, secure: true, want: 1404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &network{
				driver: &driver{advertiseAddress: tc.advAddr},
				mtu:    tc.mtu,
				secure: tc.secure,
			}
			assert.Check(t, is.Equal(n.maxMTU(), tc.want))
		})
	}
}

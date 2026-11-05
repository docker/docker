package resolvconf

import (
	"bufio"
	"bytes"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/v2/internal/sliceutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"
)

func TestRCOption(t *testing.T) {
	testcases := []struct {
		name     string
		options  string
		search   string
		expFound bool
		expValue string
	}{
		{
			name:    "Empty options",
			options: "",
			search:  "ndots",
		},
		{
			name:    "Not found",
			options: "ndots:0 edns0",
			search:  "trust-ad",
		},
		{
			name:     "Found with value",
			options:  "ndots:0 edns0",
			search:   "ndots",
			expFound: true,
			expValue: "0",
		},
		{
			name:     "Found without value",
			options:  "ndots:0 edns0",
			search:   "edns0",
			expFound: true,
			expValue: "",
		},
		{
			name:     "Found last value",
			options:  "ndots:0 edns0 ndots:1",
			search:   "ndots",
			expFound: true,
			expValue: "1",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := Parse(bytes.NewBufferString("options "+tc.options), "")
			assert.NilError(t, err)
			value, found := rc.Option(tc.search)
			assert.Check(t, is.Equal(found, tc.expFound))
			assert.Check(t, is.Equal(value, tc.expValue))
		})
	}
}

func TestRCWrite(t *testing.T) {
	testcases := []struct {
		name            string
		fileName        string
		perm            os.FileMode
		hashFileName    string
		modify          bool
		expUserModified bool
	}{
		{
			name:         "Write with hash",
			fileName:     "testfile",
			hashFileName: "testfile.hash",
		},
		{
			name:            "Write with hash and modify",
			fileName:        "testfile",
			hashFileName:    "testfile.hash",
			modify:          true,
			expUserModified: true,
		},
		{
			name:            "Write without hash and modify",
			fileName:        "testfile",
			modify:          true,
			expUserModified: false,
		},
		{
			name:     "Write perm",
			fileName: "testfile",
			perm:     0o640,
		},
	}

	rc, err := Parse(bytes.NewBufferString("nameserver 1.2.3.4"), "")
	assert.NilError(t, err)

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			d := t.TempDir()
			path := filepath.Join(d, tc.fileName)
			var hashPath string
			if tc.hashFileName != "" {
				hashPath = filepath.Join(d, tc.hashFileName)
			}
			if tc.perm == 0 {
				tc.perm = 0o644
			}
			err := rc.WriteFile(path, hashPath, tc.perm)
			assert.NilError(t, err)

			fi, err := os.Stat(path)
			assert.NilError(t, err)
			// Windows files won't have the expected perms.
			if runtime.GOOS != "windows" {
				assert.Check(t, is.Equal(fi.Mode(), tc.perm))
			}

			if tc.modify {
				err := os.WriteFile(path, []byte("modified"), 0o644)
				assert.NilError(t, err)
			}

			um, err := UserModified(path, hashPath)
			assert.NilError(t, err)
			assert.Check(t, is.Equal(um, tc.expUserModified))
		})
	}
}

var (
	a2s = sliceutil.Mapper(netip.Addr.String)
	s2a = sliceutil.Mapper(netip.MustParseAddr)
)

// Test that a resolv.conf file can be modified using OverrideXXX() methods
// to modify nameservers/search/options directives, and tha options can be
// added via AddOption().
func TestRCModify(t *testing.T) {
	testcases := []struct {
		name            string
		inputNS         []string
		inputSearch     []string
		inputOptions    []string
		noOverrides     bool // Whether to apply overrides (empty lists are valid overrides).
		overrideNS      []string
		overrideSearch  []string
		overrideOptions []string
		addOption       string
	}{
		{
			name:    "No content no overrides",
			inputNS: []string{},
		},
		{
			name:         "No overrides",
			noOverrides:  true,
			inputNS:      []string{"1.2.3.4"},
			inputSearch:  []string{"invalid"},
			inputOptions: []string{"ndots:0"},
		},
		{
			name:         "Empty overrides",
			inputNS:      []string{"1.2.3.4"},
			inputSearch:  []string{"invalid"},
			inputOptions: []string{"ndots:0"},
		},
		{
			name:            "Overrides",
			inputNS:         []string{"1.2.3.4"},
			inputSearch:     []string{"invalid"},
			inputOptions:    []string{"ndots:0"},
			overrideNS:      []string{"2.3.4.5", "fdba:acdd:587c::53"},
			overrideSearch:  []string{"com", "invalid", "example"},
			overrideOptions: []string{"ndots:1", "edns0", "trust-ad"},
		},
		{
			name:         "Add option no overrides",
			noOverrides:  true,
			inputNS:      []string{"1.2.3.4"},
			inputSearch:  []string{"invalid"},
			inputOptions: []string{"ndots:0"},
			addOption:    "attempts:3",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			var input strings.Builder
			if len(tc.inputNS) != 0 {
				for _, ns := range tc.inputNS {
					input.WriteString("nameserver " + ns + "\n")
				}
			}
			if len(tc.inputSearch) != 0 {
				input.WriteString("search " + strings.Join(tc.inputSearch, " ") + "\n")
			}
			if len(tc.inputOptions) != 0 {
				input.WriteString("options " + strings.Join(tc.inputOptions, " ") + "\n")
			}
			rc, err := Parse(bytes.NewBufferString(input.String()), "")
			assert.NilError(t, err)
			assert.Check(t, is.DeepEqual(a2s(rc.NameServers()), tc.inputNS, cmpopts.EquateEmpty()))
			assert.Check(t, is.DeepEqual(rc.Search(), tc.inputSearch))
			assert.Check(t, is.DeepEqual(rc.Options(), tc.inputOptions))

			if !tc.noOverrides {
				overrideNS := s2a(tc.overrideNS)
				rc.OverrideNameServers(overrideNS)
				rc.OverrideSearch(tc.overrideSearch)
				rc.OverrideOptions(tc.overrideOptions)

				assert.Check(t, is.DeepEqual(rc.NameServers(), overrideNS, cmpopts.EquateEmpty(), cmpopts.EquateComparable(netip.Addr{})))
				assert.Check(t, is.DeepEqual(rc.Search(), tc.overrideSearch, cmpopts.EquateEmpty()))
				assert.Check(t, is.DeepEqual(rc.Options(), tc.overrideOptions, cmpopts.EquateEmpty()))
			}

			if tc.addOption != "" {
				options := rc.Options()
				rc.AddOption(tc.addOption)
				assert.Check(t, is.DeepEqual(rc.Options(), append(options, tc.addOption), cmpopts.EquateEmpty()))
			}

			content, err := rc.Generate(true)
			assert.NilError(t, err)
			assert.Check(t, golden.String(string(content), t.Name()+".golden"))
		})
	}
}

func TestRCTransformForLegacyNw(t *testing.T) {
	testcases := []struct {
		name       string
		input      string
		ipv6       bool
		overrideNS []string
	}{
		{
			name:  "Routable IPv4 only",
			input: "nameserver 10.0.0.1",
		},
		{
			name:  "Routable IPv4 and IPv6, ipv6 enabled",
			input: "nameserver 10.0.0.1\nnameserver fdb6:b8fe:b528::1",
			ipv6:  true,
		},
		{
			name:  "Routable IPv4 and IPv6, ipv6 disabled",
			input: "nameserver 10.0.0.1\nnameserver fdb6:b8fe:b528::1",
			ipv6:  false,
		},
		{
			name:  "IPv4 localhost, ipv6 disabled",
			input: "nameserver 127.0.0.53",
			ipv6:  false,
		},
		{
			name:  "IPv4 localhost, ipv6 enabled",
			input: "nameserver 127.0.0.53",
			ipv6:  true,
		},
		{
			name:  "IPv4 and IPv6 localhost, ipv6 disabled",
			input: "nameserver 127.0.0.53\nnameserver ::1",
			ipv6:  false,
		},
		{
			name:  "IPv4 and IPv6 localhost, ipv6 enabled",
			input: "nameserver 127.0.0.53\nnameserver ::1",
			ipv6:  true,
		},
		{
			name:  "IPv4 localhost, IPv6 routeable, ipv6 enabled",
			input: "nameserver 127.0.0.53\nnameserver fd3e:2d1a:1f5a::1",
			ipv6:  true,
		},
		{
			name:  "IPv4 localhost, IPv6 routeable, ipv6 disabled",
			input: "nameserver 127.0.0.53\nnameserver fd3e:2d1a:1f5a::1",
			ipv6:  false,
		},
		{
			name:       "Override nameservers",
			input:      "nameserver 127.0.0.53",
			overrideNS: []string{"127.0.0.1", "::1"},
			ipv6:       false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			rc, err := Parse(bytes.NewBufferString(tc.input), "/etc/resolv.conf")
			assert.NilError(t, err)
			if tc.overrideNS != nil {
				rc.OverrideNameServers(s2a(tc.overrideNS))
			}

			rc.TransformForLegacyNw(tc.ipv6)

			content, err := rc.Generate(true)
			assert.NilError(t, err)
			assert.Check(t, golden.String(string(content), t.Name()+".golden"))
		})
	}
}

func TestRCTransformForIntNS(t *testing.T) {
	mke := func(addr string, hostLoopback bool) ExtDNSEntry {
		return ExtDNSEntry{
			Addr:         netip.MustParseAddr(addr),
			HostLoopback: hostLoopback,
		}
	}

	testcases := []struct {
		name            string
		input           string
		intNameServer   string
		overrideNS      []string
		overrideOptions []string
		reqdOptions     []string
		expExtServers   []ExtDNSEntry
		expErr          string
	}{
		{
			name:          "IPv4 only",
			input:         "nameserver 10.0.0.1",
			expExtServers: []ExtDNSEntry{mke("10.0.0.1", true)},
		},
		{
			name:  "IPv4 and IPv6, ipv6 enabled",
			input: "nameserver 10.0.0.1\nnameserver fdb6:b8fe:b528::1",
			expExtServers: []ExtDNSEntry{
				mke("10.0.0.1", true),
				mke("fdb6:b8fe:b528::1", true),
			},
		},
		{
			name:          "IPv4 localhost",
			input:         "nameserver 127.0.0.53",
			expExtServers: []ExtDNSEntry{mke("127.0.0.53", true)},
		},
		{
			// Overriding the nameserver with a localhost address means use the container's
			// loopback interface, not the host's.
			name:          "IPv4 localhost override",
			input:         "nameserver 10.0.0.1",
			overrideNS:    []string{"127.0.0.53"},
			expExtServers: []ExtDNSEntry{mke("127.0.0.53", false)},
		},
		{
			name:          "IPv6 only",
			input:         "nameserver fd14:6e0e:f855::1",
			expExtServers: []ExtDNSEntry{mke("fd14:6e0e:f855::1", true)},
		},
		{
			name:  "IPv4 and IPv6 localhost",
			input: "nameserver 127.0.0.53\nnameserver ::1",
			expExtServers: []ExtDNSEntry{
				mke("127.0.0.53", true),
				mke("::1", true),
			},
		},
		{
			name:          "ndots present and required",
			input:         "nameserver 127.0.0.53\noptions ndots:1",
			reqdOptions:   []string{"ndots:0"},
			expExtServers: []ExtDNSEntry{mke("127.0.0.53", true)},
		},
		{
			name:          "ndots missing but required",
			input:         "nameserver 127.0.0.53",
			reqdOptions:   []string{"ndots:0"},
			expExtServers: []ExtDNSEntry{mke("127.0.0.53", true)},
		},
		{
			name:            "ndots host, override and required",
			input:           "nameserver 127.0.0.53",
			reqdOptions:     []string{"ndots:0"},
			overrideOptions: []string{"ndots:2"},
			expExtServers:   []ExtDNSEntry{mke("127.0.0.53", true)},
		},
		{
			name:          "Extra required options",
			input:         "nameserver 127.0.0.53\noptions trust-ad",
			reqdOptions:   []string{"ndots:0", "attempts:3", "edns0", "trust-ad"},
			expExtServers: []ExtDNSEntry{mke("127.0.0.53", true)},
		},
		{
			name:  "No config",
			input: "",
			expExtServers: []ExtDNSEntry{
				mke("127.0.0.1", true),
				mke("::1", true),
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			rc, err := Parse(bytes.NewBufferString(tc.input), "/etc/resolv.conf")
			assert.NilError(t, err)

			if tc.intNameServer == "" {
				tc.intNameServer = "127.0.0.11"
			}
			if len(tc.overrideNS) > 0 {
				rc.OverrideNameServers(s2a(tc.overrideNS))
			}
			if len(tc.overrideOptions) > 0 {
				rc.OverrideOptions(tc.overrideOptions)
			}
			intNS := netip.MustParseAddr(tc.intNameServer)
			extNameServers, err := rc.TransformForIntNS(intNS, tc.reqdOptions)
			if tc.expErr != "" {
				assert.Check(t, is.ErrorContains(err, tc.expErr))
				return
			}
			assert.NilError(t, err)

			content, err := rc.Generate(true)
			assert.NilError(t, err)
			assert.Check(t, golden.String(string(content), t.Name()+".golden"))
			assert.Check(t, is.DeepEqual(extNameServers, tc.expExtServers,
				cmpopts.EquateComparable(netip.Addr{})))
		})
	}
}

// Check that invalid ndots options in the host's file are ignored, unless
// starting the internal resolver (which requires an ndots option), in which
// case invalid ndots should be replaced.
func TestRCTransformForIntNSInvalidNdots(t *testing.T) {
	testcases := []struct {
		name         string
		options      string
		reqdOptions  []string
		expVal       string
		expOptions   []string
		expNDotsFrom string
	}{
		{
			name:         "Negative value",
			options:      "options ndots:-1",
			expOptions:   []string{"ndots:-1"},
			expVal:       "-1",
			expNDotsFrom: "host",
		},
		{
			name:         "Invalid values with reqd ndots",
			options:      "options ndots:-1 foo:bar ndots ndots:",
			reqdOptions:  []string{"ndots:2"},
			expVal:       "2",
			expNDotsFrom: "internal",
			expOptions:   []string{"foo:bar", "ndots:2"},
		},
		{
			name:         "Valid value with reqd ndots",
			options:      "options ndots:1 foo:bar ndots ndots:",
			reqdOptions:  []string{"ndots:2"},
			expVal:       "1",
			expNDotsFrom: "host",
			expOptions:   []string{"ndots:1", "foo:bar"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			content := "nameserver 8.8.8.8\n" + tc.options
			rc, err := Parse(bytes.NewBufferString(content), "/etc/resolv.conf")
			assert.NilError(t, err)
			_, err = rc.TransformForIntNS(netip.MustParseAddr("127.0.0.11"), tc.reqdOptions)
			assert.NilError(t, err)

			val, found := rc.Option("ndots")
			assert.Check(t, is.Equal(found, true))
			assert.Check(t, is.Equal(val, tc.expVal))
			assert.Check(t, is.Equal(rc.md.NDotsFrom, tc.expNDotsFrom))
			assert.Check(t, is.DeepEqual(rc.options, tc.expOptions))
		})
	}
}

func TestRCRead(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "resolv.conf")

	// Try to read a nonexistent file, equivalent to an empty file.
	_, err := Load(path)
	assert.Check(t, is.ErrorIs(err, fs.ErrNotExist))

	err = os.WriteFile(path, []byte("options edns0"), 0o644)
	assert.NilError(t, err)

	// Read that file in the constructor.
	rc, err := Load(path)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(rc.Options(), []string{"edns0"}))

	// Pass in an os.File, check the path is extracted.
	file, err := os.Open(path)
	assert.NilError(t, err)
	rc, err = Parse(file, "")
	_ = file.Close()
	assert.NilError(t, err)
	assert.Check(t, is.Equal(rc.md.SourcePath, path))
}

func TestRCInvalidNS(t *testing.T) {
	// A resolv.conf with an invalid nameserver address.
	rc, err := Parse(bytes.NewBufferString("nameserver 1.2.3.4.5"), "")
	assert.NilError(t, err)

	content, err := rc.Generate(true)
	assert.NilError(t, err)
	assert.Check(t, golden.String(string(content), t.Name()+".golden"))
}

func TestRCSetHeader(t *testing.T) {
	rc, err := Parse(bytes.NewBufferString("nameserver 127.0.0.53"), "/etc/resolv.conf")
	assert.NilError(t, err)

	rc.SetHeader("# This is a comment.")

	content, err := rc.Generate(true)
	assert.NilError(t, err)
	assert.Check(t, golden.String(string(content), t.Name()+".golden"))
}

func TestRCUnknownDirectives(t *testing.T) {
	const input = `
something unexpected
nameserver 127.0.0.53
options ndots:1
unrecognised thing
`
	rc, err := Parse(bytes.NewBufferString(input), "/etc/resolv.conf")
	assert.NilError(t, err)

	content, err := rc.Generate(true)
	assert.NilError(t, err)
	assert.Check(t, golden.String(string(content), t.Name()+".golden"))
}

// TestRCParseErrors tests that attempting to read a resolv.conf with a
// very long line produces a useful error.
// see https://github.com/moby/moby/issues/51679#issuecomment-3714403300
func TestRCParseErrors(t *testing.T) {
	input := strings.Repeat("a", bufio.MaxScanTokenSize+1) + "\n"

	_, err := Parse(bytes.NewBufferString(input), "/etc/resolv.conf")
	assert.Error(t, err, `failed to parse resolv.conf from /etc/resolv.conf: line too long (exceeds 65536)`)
}

func BenchmarkGenerate(b *testing.B) {
	rc := &ResolvConf{
		nameServers: []netip.Addr{
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("1.1.1.1"),
		},
		search:  []string{"example.com", "svc.local"},
		options: []string{"ndots:1", "ndots:2", "ndots:3"},
		other:   []string{"something", "something else", "something else"},
		md: metadata{
			Header: `# Generated by Docker Engine.
# This file can be edited; Docker Engine will not make further changes once it
# has been modified.`,
			NSOverride:      true,
			SearchOverride:  true,
			OptionsOverride: true,
			NDotsFrom:       "host",
			ExtNameServers: []ExtDNSEntry{
				{Addr: netip.MustParseAddr("127.0.0.53"), HostLoopback: true},
				{Addr: netip.MustParseAddr("2.2.2.2"), HostLoopback: false},
			},
			InvalidNSs: []string{"256.256.256.256"},
			Warnings:   []string{"bad nameserver ignored"},
		},
	}

	b.ReportAllocs()
	for b.Loop() {
		_, err := rc.Generate(true)
		if err != nil {
			b.Fatal(err)
		}
	}
}

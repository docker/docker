// Package resolvconf is used to generate a container's /etc/resolv.conf file.
//
// Constructor Load and Parse read a resolv.conf file from the filesystem or
// a reader respectively, and return a ResolvConf object.
//
// The ResolvConf object can then be updated with overrides for nameserver,
// search domains, and DNS options.
//
// ResolvConf can then be transformed to make it suitable for legacy networking,
// a network with an internal nameserver, or used as-is for host networking.
//
// This package includes methods to write the file for the container, along with
// a hash that can be used to detect modifications made by the user to avoid
// overwriting those updates.
package resolvconf

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/sys/atomicwriter"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// Fallback nameservers, to use if none can be obtained from the host or command
// line options.
var (
	defaultIPv4NSs = []netip.Addr{
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("8.8.4.4"),
	}
	defaultIPv6NSs = []netip.Addr{
		netip.MustParseAddr("2001:4860:4860::8888"),
		netip.MustParseAddr("2001:4860:4860::8844"),
	}
	defaultLoopbackNSs = []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("::1"),
	}
)

// ResolvConf represents a resolv.conf file. It can be constructed by
// reading a resolv.conf file, using method Parse().
type ResolvConf struct {
	nameServers []netip.Addr
	search      []string
	options     []string
	other       []string // Unrecognised directives from the host's file, if any.

	md metadata
}

// ExtDNSEntry represents a nameserver address that was removed from the
// container's resolv.conf when it was transformed by TransformForIntNS(). These
// are addresses read from the host's file, or applied via an override ('--dns').
type ExtDNSEntry struct {
	Addr         netip.Addr
	HostLoopback bool // The address is loopback, in the host's namespace.
}

func (ed ExtDNSEntry) String() string {
	if ed.HostLoopback {
		return "host(" + ed.Addr.String() + ")"
	}
	return ed.Addr.String()
}

// metadata is used to track where components of the generated file have come
// from, in order to generate comments in the file for debug/info. Struct members
// are exported for use by 'text/template'.
type metadata struct {
	SourcePath      string
	Header          string
	NSOverride      bool
	SearchOverride  bool
	OptionsOverride bool
	NDotsFrom       string
	Transform       string
	InvalidNSs      []string
	ExtNameServers  []ExtDNSEntry
	Warnings        []string
}

// Load opens a file at path and parses it as a resolv.conf file.
// On error, the returned ResolvConf will be zero-valued.
func Load(path string) (ResolvConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return ResolvConf{}, err
	}
	defer f.Close()
	return Parse(f, path)
}

// Parse parses a resolv.conf file from reader.
// path is optional if reader is an *os.File.
// On error, the returned ResolvConf will be zero-valued.
func Parse(reader io.Reader, path string) (ResolvConf, error) {
	var rc ResolvConf
	rc.md.SourcePath = path
	if path == "" {
		if namer, ok := reader.(interface{ Name() string }); ok {
			rc.md.SourcePath = namer.Name()
		}
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		rc.processLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		src := rc.md.SourcePath
		if errors.Is(err, bufio.ErrTooLong) {
			return ResolvConf{}, systemError{
				fmt.Errorf("failed to parse resolv.conf from %s: line too long (exceeds %d)", src, bufio.MaxScanTokenSize),
			}
		}
		return ResolvConf{}, systemError{
			fmt.Errorf("failed to parse resolv.conf from %s: %w", src, err),
		}
	}
	if _, ok := rc.Option("ndots"); ok {
		rc.md.NDotsFrom = "host"
	}
	return rc, nil
}

// SetHeader sets the content to be included verbatim at the top of the
// generated resolv.conf file. No formatting or checking is done on the
// string. It must be valid resolv.conf syntax. (Comments must have '#'
// or ';' in the first column of each line).
//
// For example:
//
//	SetHeader("# My resolv.conf\n# This file was generated.")
func (rc *ResolvConf) SetHeader(c string) {
	rc.md.Header = c
}

// NameServers returns addresses used in nameserver directives.
func (rc *ResolvConf) NameServers() []netip.Addr {
	return slices.Clone(rc.nameServers)
}

// OverrideNameServers replaces the current set of nameservers.
func (rc *ResolvConf) OverrideNameServers(nameServers []netip.Addr) {
	rc.nameServers = nameServers
	rc.md.NSOverride = true
}

// Search returns the current DNS search domains.
func (rc *ResolvConf) Search() []string {
	return slices.Clone(rc.search)
}

// OverrideSearch replaces the current DNS search domains.
func (rc *ResolvConf) OverrideSearch(search []string) {
	var filtered []string
	for _, s := range search {
		if s != "." {
			filtered = append(filtered, s)
		}
	}
	rc.search = filtered
	rc.md.SearchOverride = true
}

// Options returns the current options.
func (rc *ResolvConf) Options() []string {
	return slices.Clone(rc.options)
}

// Option finds the last option named search, and returns (value, true) if
// found, else ("", false). Options are treated as "name:value", where the
// ":value" may be omitted.
//
// For example, for "ndots:1 edns0":
//
//	Option("ndots") -> ("1", true)
//	Option("edns0") -> ("", true)
func (rc *ResolvConf) Option(search string) (string, bool) {
	for i := len(rc.options) - 1; i >= 0; i-- {
		k, v, _ := strings.Cut(rc.options[i], ":")
		if k == search {
			return v, true
		}
	}
	return "", false
}

// OverrideOptions replaces the current DNS options.
func (rc *ResolvConf) OverrideOptions(options []string) {
	rc.options = slices.Clone(options)
	rc.md.NDotsFrom = ""
	if _, exists := rc.Option("ndots"); exists {
		rc.md.NDotsFrom = "override"
	}
	rc.md.OptionsOverride = true
}

// AddOption adds a single DNS option.
func (rc *ResolvConf) AddOption(option string) {
	if len(option) > 6 && option[:6] == "ndots:" {
		rc.md.NDotsFrom = "internal"
	}
	rc.options = append(rc.options, option)
}

// TransformForLegacyNw makes sure the resolv.conf file will be suitable for
// use in a legacy network (one that has no internal resolver).
//   - Remove loopback addresses inherited from the host's resolv.conf, because
//     they'll only work in the host's namespace.
//   - Remove IPv6 addresses if !ipv6.
//   - Add default nameservers if there are no addresses left.
func (rc *ResolvConf) TransformForLegacyNw(ipv6 bool) {
	rc.md.Transform = "legacy"
	if rc.md.NSOverride {
		return
	}
	var filtered []netip.Addr
	for _, addr := range rc.nameServers {
		if !addr.IsLoopback() && (!addr.Is6() || ipv6) {
			filtered = append(filtered, addr)
		}
	}
	rc.nameServers = filtered
	if len(rc.nameServers) == 0 {
		log.G(context.TODO()).Info("No non-localhost DNS nameservers are left in resolv.conf. Using default external servers")
		rc.nameServers = defaultNSAddrs(ipv6)
		rc.md.Warnings = append(rc.md.Warnings, "Used default nameservers.")
	}
}

// TransformForIntNS makes sure the resolv.conf file will be suitable for
// use in a network sandbox that has an internal DNS resolver.
//   - Add internalNS as a nameserver.
//   - Remove other nameservers, stashing them as ExtNameServers for the
//     internal resolver to use.
//   - Mark ExtNameServers that must be accessed from the host namespace.
//   - If no ExtNameServer addresses are found, use the defaults.
//   - Ensure there's an 'options' value for each entry in reqdOptions. If the
//     option includes a ':', and an option with a matching prefix exists, it
//     is not modified.
func (rc *ResolvConf) TransformForIntNS(
	internalNS netip.Addr,
	reqdOptions []string,
) ([]ExtDNSEntry, error) {
	// Add each of the nameservers read from the host's /etc/hosts or supplied as an
	// override to ExtNameServers, for the internal resolver to talk to. Addresses
	// read from host config should be accessed from the host's network namespace
	// (HostLoopback=true). Addresses supplied as overrides are accessed from the
	// container's namespace.
	rc.md.ExtNameServers = nil
	for _, addr := range rc.nameServers {
		rc.md.ExtNameServers = append(rc.md.ExtNameServers, ExtDNSEntry{
			Addr:         addr,
			HostLoopback: !rc.md.NSOverride,
		})
	}

	// The transformed config only lists the internal nameserver.
	rc.nameServers = []netip.Addr{internalNS}

	// For each option required by the nameserver, add it if not already present. If
	// the option is already present, don't override it. Apart from ndots - if the
	// ndots value is invalid and an ndots option is required, replace the existing
	// value.
	for _, opt := range reqdOptions {
		optName, _, _ := strings.Cut(opt, ":")
		if optName == "ndots" {
			rc.options = removeInvalidNDots(rc.options)
			// No need to update rc.md.NDotsFrom, if there is no ndots option remaining,
			// it'll be set to "internal" when the required value is added.
		}
		if _, exists := rc.Option(optName); !exists {
			rc.AddOption(opt)
		}
	}

	rc.md.Transform = "internal resolver"
	if len(rc.md.ExtNameServers) == 0 {
		for _, addr := range defaultLoopbackNSs {
			rc.md.ExtNameServers = append(rc.md.ExtNameServers, ExtDNSEntry{
				Addr:         addr,
				HostLoopback: true,
			})
		}
		rc.md.Warnings = append(rc.md.Warnings, "Used default nameservers.")
	}
	return slices.Clone(rc.md.ExtNameServers), nil
}

// Generate returns content suitable for writing to a resolv.conf file. If comments
// is true, the file will include header information if supplied, and a trailing
// comment that describes how the file was constructed and lists external resolvers.
func (rc *ResolvConf) Generate(comments bool) ([]byte, error) {
	var b bytes.Buffer
	b.Grow(512) // estimated size for a regular resolv.conf we produce.

	if comments && rc.md.Header != "" {
		b.WriteString(rc.md.Header + "\n")
		b.WriteByte('\n')
	}
	for _, ns := range rc.nameServers {
		b.WriteString("nameserver ")
		b.WriteString(ns.String())
		b.WriteByte('\n')
	}
	if len(rc.search) > 0 {
		b.WriteString("search ")
		for i, s := range rc.search {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(s)
		}
		b.WriteByte('\n')
	}
	if len(rc.options) > 0 {
		b.WriteString("options ")
		for i, s := range rc.options {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(s)
		}
		b.WriteByte('\n')
	}
	for _, o := range rc.other {
		b.WriteString(o)
		b.WriteByte('\n')
	}

	if comments {
		b.WriteByte('\n')
		b.WriteString("# Based on host file: '" + rc.md.SourcePath + "'")
		if rc.md.Transform != "" {
			b.WriteString(" (" + rc.md.Transform + ")")
		}
		b.WriteByte('\n')
		for _, w := range rc.md.Warnings {
			b.WriteString("# ")
			b.WriteString(w)
			b.WriteByte('\n')
		}
		if len(rc.md.ExtNameServers) > 0 {
			b.WriteString("# ExtServers: [")
			for i, ext := range rc.md.ExtNameServers {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(ext.String())
			}
			b.WriteByte(']')
			b.WriteByte('\n')
		}
		if len(rc.md.InvalidNSs) > 0 {
			b.WriteString("# Invalid nameservers: [")
			for i, ext := range rc.md.InvalidNSs {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(ext)
			}
			b.WriteByte(']')
			b.WriteByte('\n')
		}

		b.WriteString("# Overrides: [")
		var overrides int
		if rc.md.NSOverride {
			b.WriteString("nameservers")
			overrides++
		}
		if rc.md.SearchOverride {
			if overrides > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("search")
			overrides++
		}
		if rc.md.OptionsOverride {
			if overrides > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("options")
		}
		b.WriteByte(']')
		b.WriteByte('\n')

		if rc.md.NDotsFrom != "" {
			b.WriteString("# Option ndots from: " + rc.md.NDotsFrom + "\n")
		}
	}

	return b.Bytes(), nil
}

// WriteFile generates content and writes it to path. If hashPath is non-zero, it
// also writes a file containing a hash of the content, to enable UserModified()
// to determine whether the file has been modified.
func (rc *ResolvConf) WriteFile(path, hashPath string, perm os.FileMode) error {
	content, err := rc.Generate(true)
	if err != nil {
		return err
	}

	// Write the resolv.conf file - it's bind-mounted into the container, so can't
	// move a temp file into place, just have to truncate and write it.
	if err := os.WriteFile(path, content, perm); err != nil {
		return systemError{err}
	}

	// Write the hash file.
	if hashPath != "" {
		hashFile, err := atomicwriter.New(hashPath, perm)
		if err != nil {
			return systemError{err}
		}
		defer hashFile.Close()

		if _, err = hashFile.Write([]byte(digest.FromBytes(content))); err != nil {
			return err
		}
	}

	return nil
}

// UserModified can be used to determine whether the resolv.conf file has been
// modified since it was generated. It returns false with no error if the file
// matches the hash, true with no error if the file no longer matches the hash,
// and false with an error if the result cannot be determined.
func UserModified(rcPath, rcHashPath string) (bool, error) {
	currRCHash, err := os.ReadFile(rcHashPath)
	if err != nil {
		// If the hash file doesn't exist, can only assume it hasn't been written
		// yet (so, the user hasn't modified the file it hashes).
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to read hash file %s", rcHashPath)
	}
	expected, err := digest.Parse(string(currRCHash))
	if err != nil {
		return false, errors.Wrapf(err, "failed to parse hash file %s", rcHashPath)
	}
	v := expected.Verifier()
	currRC, err := os.Open(rcPath)
	if err != nil {
		return false, errors.Wrapf(err, "failed to open %s to check for modifications", rcPath)
	}
	defer currRC.Close()
	if _, err := io.Copy(v, currRC); err != nil {
		return false, errors.Wrapf(err, "failed to hash %s to check for modifications", rcPath)
	}
	return !v.Verified(), nil
}

func (rc *ResolvConf) processLine(line string) {
	// Strip blank lines and comments.
	if line == "" || line[0] == '#' || line[0] == ';' {
		return
	}

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return
	}

	switch fields[0] {
	case "nameserver":
		if len(fields) < 2 {
			return
		}
		if addr, err := netip.ParseAddr(fields[1]); err != nil {
			rc.md.InvalidNSs = append(rc.md.InvalidNSs, fields[1])
		} else {
			rc.nameServers = append(rc.nameServers, addr)
		}
	case "domain":
		// 'domain' is an obsolete name for 'search'.
		fallthrough
	case "search":
		if len(fields) < 2 {
			return
		}
		// Only the last 'search' directive is used.
		rc.search = fields[1:]
	case "options":
		if len(fields) < 2 {
			return
		}
		// Accumulate options.
		rc.options = append(rc.options, fields[1:]...)
	default:
		// Copy anything that's not a recognised directive.
		rc.other = append(rc.other, line)
	}
}

func defaultNSAddrs(ipv6 bool) []netip.Addr {
	var addrs []netip.Addr
	addrs = append(addrs, defaultIPv4NSs...)
	if ipv6 {
		addrs = append(addrs, defaultIPv6NSs...)
	}
	return addrs
}

// removeInvalidNDots filters ill-formed "ndots" settings from options.
// The backing array of the options slice is reused.
func removeInvalidNDots(options []string) []string {
	n := 0
	for _, opt := range options {
		k, v, hasSep := strings.Cut(opt, ":")
		if k == "ndots" {
			if !hasSep || v == "" {
				continue
			}
			ndots, err := strconv.Atoi(v)
			if err != nil || ndots < 0 {
				continue
			}
		}
		options[n] = opt
		n++
	}
	clear(options[n:]) // Zero out the obsolete elements, for GC.
	return options[:n]
}

// systemError implements [github.com/moby/moby/v2/errdefs.ErrSystem].
type systemError struct{ error }

func (systemError) System() {}

func (e systemError) Unwrap() error {
	return e.error
}

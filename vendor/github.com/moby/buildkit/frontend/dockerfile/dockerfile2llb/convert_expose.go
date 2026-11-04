package dockerfile2llb

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/linter"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/pkg/errors"
)

func dispatchExpose(d *dispatchState, c *instructions.ExposeCommand, opt *dispatchOpt) error {
	ports := []string{}
	env := getEnv(d.state)
	for _, p := range c.Ports {
		ps, err := opt.shlex.ProcessWords(p, env)
		if err != nil {
			return err
		}
		ports = append(ports, ps...)
	}
	c.Ports = ports

	ps := newPortSpecs(
		withLocation(c.Location()),
		withLint(opt.lint),
	)

	psp, err := ps.parsePorts(c.Ports)
	if err != nil {
		return err
	}

	if d.image.Config.ExposedPorts == nil {
		d.image.Config.ExposedPorts = make(map[string]struct{})
	}
	for _, p := range psp {
		d.image.Config.ExposedPorts[p] = struct{}{}
	}

	return commitToHistory(&d.image, fmt.Sprintf("EXPOSE %s", strings.Join(psp, " ")), false, nil, d.epoch)
}

type portSpecs struct {
	location []parser.Range
	lint     *linter.Linter
}

type portSpecsOption func(ps *portSpecs)

func withLocation(location []parser.Range) portSpecsOption {
	return func(ps *portSpecs) {
		ps.location = location
	}
}

func withLint(lint *linter.Linter) portSpecsOption {
	return func(ps *portSpecs) {
		ps.lint = lint
	}
}

func newPortSpecs(opts ...portSpecsOption) *portSpecs {
	ps := &portSpecs{}
	for _, opt := range opts {
		opt(ps)
	}
	return ps
}

// parsePorts receives port specs in the format of [ip:]public:private/proto
// and returns them as a list of "port/proto".
func (ps *portSpecs) parsePorts(ports []string) (exposedPorts []string, _ error) {
	for _, p := range ports {
		portProtos, err := ps.parsePort(p)
		if err != nil {
			return nil, err
		}
		exposedPorts = append(exposedPorts, portProtos...)
	}
	return exposedPorts, nil
}

// parsePort parses a port specification string into a slice of "<portnum>/[<proto>]"
func (ps *portSpecs) parsePort(rawPort string) (portProto []string, _ error) {
	ip, hostPort, containerPort := ps.splitParts(rawPort)
	proto, containerPort, err := ps.splitProtoPort(containerPort)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid port: %q", rawPort)
	}
	if ps.lint != nil {
		if proto != strings.ToLower(proto) {
			msg := linter.RuleExposeProtoCasing.Format(rawPort)
			ps.lint.Run(&linter.RuleExposeProtoCasing, ps.location, msg)
		}
		if ip != "" || hostPort != "" {
			msg := linter.RuleExposeInvalidFormat.Format(rawPort)
			ps.lint.Run(&linter.RuleExposeInvalidFormat, ps.location, msg)
		}
	}

	// TODO(thaJeztah): mapping IP-addresses should not be allowed for EXPOSE; see https://github.com/moby/buildkit/issues/2173
	if ip != "" && ip[0] == '[' {
		// Strip [] from IPV6 addresses
		rawIP, _, err := net.SplitHostPort(ip + ":")
		if err != nil {
			return nil, errors.Wrapf(err, "invalid IP address %v", ip)
		}
		ip = rawIP
	}
	if ip != "" && net.ParseIP(ip) == nil {
		return nil, errors.New("invalid IP address: " + ip)
	}

	startPort, endPort, err := ps.parsePortRange(containerPort)
	if err != nil {
		return nil, errors.New("invalid containerPort: " + containerPort)
	}

	// TODO(thaJeztah): mapping host-ports should not be allowed for EXPOSE; see https://github.com/moby/buildkit/issues/2173
	if hostPort != "" {
		startHostPort, endHostPort, err := ps.parsePortRange(hostPort)
		if err != nil {
			return nil, errors.New("invalid hostPort: " + hostPort)
		}
		if (endPort - startPort) != (endHostPort - startHostPort) {
			// Allow host port range iff containerPort is not a range.
			// In this case, use the host port range as the dynamic
			// host port range to allocate into.
			if endPort != startPort {
				return nil, errors.Errorf("invalid ranges specified for container and host Ports: %s and %s", containerPort, hostPort)
			}
		}
	}

	count := endPort - startPort + 1
	ports := make([]string, 0, count)

	for i := range count {
		ports = append(ports, strconv.Itoa(startPort+i)+"/"+strings.ToLower(proto))
	}
	return ports, nil
}

// parsePortRange parses and validates the specified string as a port range (e.g., "8000-9000").
func (ps *portSpecs) parsePortRange(ports string) (startPort, endPort int, _ error) {
	if ports == "" {
		return 0, 0, errors.New("empty string specified for ports")
	}
	start, end, ok := strings.Cut(ports, "-")

	startPort, err := ps.parsePortNumber(start)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "invalid start port '%s'", start)
	}
	if !ok || start == end {
		return startPort, startPort, nil
	}

	endPort, err = ps.parsePortNumber(end)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "invalid end port '%s'", end)
	}
	if endPort < startPort {
		return 0, 0, errors.New("invalid port range: " + ports)
	}
	return startPort, endPort, nil
}

// parsePortNumber parses rawPort into an int, unwrapping strconv errors
// and returning a single "out of range" error for any value outside 0–65535.
func (ps *portSpecs) parsePortNumber(rawPort string) (int, error) {
	if rawPort == "" {
		return 0, errors.New("value is empty")
	}
	port, err := strconv.ParseInt(rawPort, 10, 0)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) {
			err = numErr.Err
		}
		return 0, err
	}
	if port < 0 || port > 65535 {
		return 0, errors.New("value out of range (0–65535)")
	}

	return int(port), nil
}

// splitProtoPort splits a port(range) and protocol, formatted as "<portnum>/[<proto>]"
// "<startport-endport>/[<proto>]". It returns an error if no port(range) or
// an invalid proto is provided. If no protocol is provided, the default ("tcp")
// protocol is returned.
func (ps *portSpecs) splitProtoPort(rawPort string) (proto string, port string, _ error) {
	port, proto, _ = strings.Cut(rawPort, "/")
	if port == "" {
		return "", "", errors.New("no port specified")
	}
	switch strings.ToLower(proto) {
	case "":
		return "tcp", port, nil
	case "tcp", "udp", "sctp":
		return proto, port, nil
	default:
		return "", "", errors.New("invalid proto: " + proto)
	}
}

func (ps *portSpecs) splitParts(rawport string) (hostIP, hostPort, containerPort string) {
	parts := strings.Split(rawport, ":")

	switch len(parts) {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		n := len(parts)
		return strings.Join(parts[:n-2], ":"), parts[n-2], parts[n-1]
	}
}

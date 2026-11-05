package oci

import (
	"context"
	"net/netip"
	"os"
	"sync/atomic"

	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/resolvconf"
	"github.com/moby/sys/user"
	"github.com/pkg/errors"
)

const (
	// defaultPath is the default path to the resolv.conf that contains
	// information to resolve DNS.
	defaultPath = "/etc/resolv.conf"
	// alternatePath is a path different from defaultPath, that may be used to
	// resolve DNS.
	alternatePath = "/run/systemd/resolve/resolv.conf"
)

var (
	g flightcontrol.Group[struct{}]
	// accessed concurrently by calls with different keys
	notFirstRun  atomic.Bool
	lastNotEmpty atomic.Bool
)

// overridden by tests
var resolvconfPath = func(netMode pb.NetMode) string {
	// Directly return /etc/resolv.conf if the networking mode is set to host
	// since the locally (127.0.0.53) running resolved daemon is accessible
	// from inside a host networked container. For details of the
	// implementation see https://github.com/moby/buildkit/pull/5207#discussion_r1705362230.
	if netMode == pb.NetMode_HOST {
		return defaultPath
	}
	// When /etc/resolv.conf contains 127.0.0.53 as the only nameserver, then
	// it is assumed systemd-resolved manages DNS. Because inside the container
	// 127.0.0.53 is not a valid DNS server, then return /run/systemd/resolve/resolv.conf
	// which is the resolv.conf that systemd-resolved generates and manages.
	// Otherwise, return /etc/resolv.conf.
	rc, err := resolvconf.Load(defaultPath)
	if err != nil {
		return defaultPath
	}
	ns := rc.NameServers()
	if len(ns) == 1 && ns[0] == netip.MustParseAddr("127.0.0.53") {
		bklog.G(context.TODO()).Infof("detected 127.0.0.53 nameserver, assuming systemd-resolved, so using resolv.conf: %s", alternatePath)
		return alternatePath
	}
	return defaultPath
}

type DNSConfig struct {
	Nameservers   []string
	Options       []string
	SearchDomains []string
}

func GetResolvConf(ctx context.Context, root *os.Root, idmap *user.IdentityMapping, dns *DNSConfig, netMode pb.NetMode) (string, error) {
	name := "resolv.conf"
	if netMode == pb.NetMode_HOST {
		name = "resolv-host.conf"
	}

	_, err := g.Do(ctx, root.Name()+"/"+name, func(ctx context.Context) (struct{}, error) {
		generate := !notFirstRun.Swap(true)

		if !generate {
			fi, err := root.Stat(name)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return struct{}{}, errors.WithStack(err)
				}
				generate = true
			}
			if !generate {
				fiMain, err := os.Stat(resolvconfPath(netMode))
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						return struct{}{}, err
					}
					if lastNotEmpty.CompareAndSwap(true, false) {
						generate = true
					}
				} else if fi.ModTime().Before(fiMain.ModTime()) {
					generate = true
				}
			}
		}

		if !generate {
			return struct{}{}, nil
		}

		rc, err := resolvconf.Load(resolvconfPath(netMode))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return struct{}{}, errors.WithStack(err)
		}

		if dns != nil {
			if len(dns.Nameservers) > 0 {
				var ns []netip.Addr
				for _, addr := range dns.Nameservers {
					ipAddr, err := netip.ParseAddr(addr)
					if err != nil {
						return struct{}{}, errors.WithStack(errors.Wrap(err, "bad nameserver address"))
					}
					ns = append(ns, ipAddr)
				}
				rc.OverrideNameServers(ns)
			}
			if len(dns.SearchDomains) > 0 {
				rc.OverrideSearch(dns.SearchDomains)
			}
			if len(dns.Options) > 0 {
				rc.OverrideOptions(dns.Options)
			}
		}

		if netMode != pb.NetMode_HOST || len(rc.NameServers()) == 0 {
			rc.TransformForLegacyNw(true)
		}

		dt, err := rc.Generate(false)
		if err != nil {
			return struct{}{}, errors.WithStack(err)
		}

		tmpName := name + ".tmp"

		if err := root.WriteFile(tmpName, dt, 0644); err != nil {
			return struct{}{}, errors.WithStack(err)
		}

		if idmap != nil {
			uid, gid := idmap.RootPair()
			if err := root.Chown(tmpName, uid, gid); err != nil {
				return struct{}{}, errors.WithStack(err)
			}
		}

		// TODO(thaJeztah): can we avoid the write -> chown -> rename?
		if err := root.Rename(tmpName, name); err != nil {
			return struct{}{}, errors.WithStack(err)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

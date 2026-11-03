//go:build linux

package iptables

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	dbus "github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
)

const (
	dbusInterface   = "org.fedoraproject.FirewallD1"
	dbusPath        = "/org/fedoraproject/FirewallD1"
	dbusConfigPath  = "/org/fedoraproject/FirewallD1/config"
	dockerZone      = "docker"
	dockerFwdPolicy = "docker-forwarding"
)

// Conn is a connection to firewalld dbus endpoint.
type Conn struct {
	sysconn    *dbus.Conn
	sysObj     dbus.BusObject
	sysConfObj dbus.BusObject
	signal     chan *dbus.Signal
}

var (
	connection *Conn

	firewalldRunning atomic.Bool // is firewalld service running
	firewalldOnce    sync.Once
	// Time of the last firewalld reload.
	firewalldReloadedAt atomic.Value
	// Mutex to serialise firewalld reload callbacks.
	firewalldReloadMu sync.Mutex
	onReloaded        []*func() // callbacks when Firewalld has been reloaded
)

// UsingFirewalld returns true if iptables rules will be applied via firewalld's
// passthrough interface.
func UsingFirewalld() bool {
	firewalldOnce.Do(initFirewalld)
	return firewalldRunning.Load()
}

// FirewalldReloadedAt returns the time at which the daemon last completed a
// firewalld reload, or a zero-valued time.Time if it has not been reloaded
// since the daemon started.
func FirewalldReloadedAt() time.Time {
	val := firewalldReloadedAt.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// firewalldInit initializes firewalld management code.
func firewalldInit() error {
	var err error

	// DOCKER_TEST_NO_FIREWALLD is used by integration tests to disable firewalld integration to make sure that the
	// daemon started by the 'test-integration' script won't recreate iptables / nftables rules upon receiving the
	// firewalld reload signal, otherwise it'll race against the daemon-under-test started by networking integration
	// tests. This is an internal implementation detail and users shall never rely on this.
	if disable := os.Getenv("DOCKER_TEST_NO_FIREWALLD"); disable != "" {
		return nil
	}

	if connection, err = newConnection(); err != nil {
		return fmt.Errorf("Failed to connect to D-Bus system bus: %v", err)
	}

	// firewalld does not handle method calls until it has finished starting,
	// so we can start calling its methods as soon as it owns its name.
	running, err := connection.nameHasOwner()
	if err != nil {
		connection.sysconn.Close()
		connection = nil
		return err
	}
	firewalldRunning.Store(running)
	// Keep the connection when firewalld is not running, to start using
	// firewalld if it is started later.
	go signalHandler()
	if !running {
		return nil
	}
	return setupFirewalld()
}

// setupFirewalld creates the docker zone and forwarding policy in firewalld,
// if they do not already exist.
func setupFirewalld() error {
	zoneAdded, err := setupDockerZone()
	if err != nil {
		return err
	}
	policyAdded, policyAddErr := setupDockerForwardingPolicy()
	if policyAddErr != nil {
		// Log the error, but still reload firewalld if necessary.
		log.G(context.TODO()).WithError(policyAddErr).Warnf("Firewalld: failed to add policy %s", dockerFwdPolicy)
	}
	if zoneAdded || policyAdded {
		// Reload for changes to take effect.
		if err := connection.sysObj.Call(dbusInterface+".reload", 0).Err; err != nil {
			return err
		}
	}
	return nil
}

// newConnection establishes a connection to the system bus.
func newConnection() (*Conn, error) {
	c := &Conn{}

	var err error
	c.sysconn, err = dbus.SystemBus()
	if err != nil {
		return nil, err
	}

	// This never fails, even if the service is not running atm.
	c.sysObj = c.sysconn.Object(dbusInterface, dbusPath)
	c.sysConfObj = c.sysconn.Object(dbusInterface, dbusConfigPath)

	c.signal = make(chan *dbus.Signal, 10)
	c.sysconn.Signal(c.signal)

	if err := c.sysconn.AddMatchSignal(
		dbus.WithMatchSender(dbusInterface),
		dbus.WithMatchObjectPath(dbusPath),
		dbus.WithMatchInterface(dbusInterface),
		dbus.WithMatchMember("Reloaded"),
	); err != nil {
		return nil, fmt.Errorf("failed to subscribe to firewalld reload signal: %w", err)
	}
	if err := c.sysconn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, dbusInterface),
	); err != nil {
		return nil, fmt.Errorf("failed to subscribe to firewalld name owner changes: %w", err)
	}
	return c, nil
}

// nameHasOwner reports whether firewalld owns its D-Bus name. Call it after
// subscribing to NameOwnerChanged, so that no change of owner is missed
// between the two.
func (c *Conn) nameHasOwner() (bool, error) {
	var hasOwner bool
	if err := c.sysconn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, dbusInterface).Store(&hasOwner); err != nil {
		return false, fmt.Errorf("failed to check whether firewalld is running: %w", err)
	}
	return hasOwner, nil
}

func signalHandler() {
	for signal := range connection.signal {
		switch signal.Name {
		case "org.freedesktop.DBus.NameOwnerChanged":
			dbusConnectionChanged(signal.Body)
		case dbusInterface + ".Reloaded":
			reloaded()
		}
	}
}

func dbusConnectionChanged(args []any) {
	var name, oldOwner, newOwner string
	if err := dbus.Store(args, &name, &oldOwner, &newOwner); err != nil {
		log.G(context.TODO()).WithError(err).Error("Could not parse D-Bus NameOwnerChanged signal")
		return
	}

	if name != dbusInterface {
		return
	}

	running := newOwner != ""
	firewalldRunning.Store(running)
	if running {
		// firewalld may have been started for the first time since the daemon
		// started, or its permanent configuration may have changed.
		// firewalld does not handle method calls until it has finished starting,
		// so we can set up the docker zone as soon as it owns its name.
		if err := setupFirewalld(); err != nil {
			log.G(context.TODO()).WithError(err).Error("Firewalld: failed to set up docker zone")
		}
	}
	// With CleanupOnExit=yes, firewalld deletes the iptables rules before it
	// releases its D-Bus name. Restore our iptables rules when firewalld
	// starts or exits.
	reloaded()
}

// call all callbacks
func reloaded() {
	firewalldReloadMu.Lock()
	defer firewalldReloadMu.Unlock()
	for _, pf := range onReloaded {
		(*pf)()
	}
	firewalldReloadedAt.Store(time.Now())
}

// OnReloaded add callback
func OnReloaded(callback func()) {
	if slices.Contains(onReloaded, &callback) {
		return
	}
	onReloaded = append(onReloaded, &callback)
}

// passthrough method simply passes args through to iptables/ip6tables
func passthrough(ipv IPVersion, args ...string) ([]byte, error) {
	var output string
	log.G(context.TODO()).Debugf("Firewalld passthrough: %s, %s", ipv, args)
	if err := connection.sysObj.Call(dbusInterface+".direct.passthrough", 0, ipv, args).Store(&output); err != nil {
		return nil, err
	}
	return []byte(output), nil
}

// firewalldZone holds the firewalld zone settings.
//
// Documented in https://firewalld.org/documentation/man-pages/firewalld.dbus.html#FirewallD1.zone
type firewalldZone struct {
	version            string
	name               string
	description        string
	unused             bool
	target             string
	services           []string
	ports              [][]any
	icmpBlocks         []string
	masquerade         bool
	forwardPorts       [][]any
	interfaces         []string
	sourceAddresses    []string
	richRules          []string
	protocols          []string
	sourcePorts        [][]any
	icmpBlockInversion bool
}

// settings returns the firewalldZone struct as an interface slice, which can be
// passed to "org.fedoraproject.FirewallD1.config.addZone". Note that 'addZone',
// which is deprecated, requires this whole struct. Its replacement, 'addZone2'
// (introduced in firewalld 0.9.0) accepts a dictionary where only non-default
// values need to be specified.
func (z firewalldZone) settings() []any {
	return []any{
		z.version,
		z.name,
		z.description,
		z.unused,
		z.target,
		z.services,
		z.ports,
		z.icmpBlocks,
		z.masquerade,
		z.forwardPorts,
		z.interfaces,
		z.sourceAddresses,
		z.richRules,
		z.protocols,
		z.sourcePorts,
		z.icmpBlockInversion,
	}
}

// setupDockerZone creates a zone called docker in firewalld which includes docker interfaces to allow
// container networking. The bool return value is true if a firewalld reload is required.
func setupDockerZone() (bool, error) {
	var zones []string
	// Check if zone exists
	if err := connection.sysObj.Call(dbusInterface+".zone.getZones", 0).Store(&zones); err != nil {
		return false, err
	}
	if contains(zones, dockerZone) {
		log.G(context.TODO()).Infof("Firewalld: %s zone already exists, returning", dockerZone)
		return false, nil
	}
	log.G(context.TODO()).Debugf("Firewalld: creating %s zone", dockerZone)

	// Permanent
	dz := firewalldZone{
		version:     "1.0",
		name:        dockerZone,
		description: "zone for docker bridge network interfaces",
		target:      "ACCEPT",
	}
	if err := connection.sysConfObj.Call(dbusInterface+".config.addZone", 0, dockerZone, dz.settings()).Err; err != nil {
		return false, err
	}
	return true, nil
}

// setupDockerForwardingPolicy creates a policy to allow forwarding to anywhere to the docker
// zone (where packets will be dealt with by docker's usual/non-firewalld configuration).
// The bool return value is true if a firewalld reload is required.
func setupDockerForwardingPolicy() (bool, error) {
	// https://firewalld.org/documentation/man-pages/firewalld.dbus.html#FirewallD1.config
	policy := map[string]any{
		"version":       "1.0",
		"description":   "allow forwarding to the docker zone",
		"ingress_zones": []string{"ANY"},
		"egress_zones":  []string{dockerZone},
		"target":        "ACCEPT",
	}
	if err := connection.sysConfObj.Call(dbusInterface+".config.addPolicy", 0, dockerFwdPolicy, policy).Err; err != nil {
		if firewalldErrorCode(err) == "NAME_CONFLICT" {
			log.G(context.TODO()).Debugf("Firewalld: %s policy already exists", dockerFwdPolicy)
			return false, nil
		}
		var derr dbus.Error
		if errors.As(err, &derr) && derr.Name == dbus.ErrMsgUnknownMethod.Name {
			log.G(context.TODO()).Debugf("Firewalld: addPolicy %s: unknown method", dockerFwdPolicy)
			return false, nil
		}
		return false, err
	}
	log.G(context.TODO()).Infof("Firewalld: created %s policy", dockerFwdPolicy)
	return true, nil
}

// AddInterfaceFirewalld adds the interface to the trusted zone. It is a
// no-op if firewalld is not running, or if the interface is already bound
// to another zone.
func AddInterfaceFirewalld(intf string) error {
	if !UsingFirewalld() {
		return nil
	}

	log.G(context.TODO()).Debugf("Firewalld: adding %s interface to %s zone", intf, dockerZone)
	// Runtime
	if err := connection.sysObj.Call(dbusInterface+".zone.addInterface", 0, dockerZone, intf).Err; err != nil {
		switch firewalldErrorCode(err) {
		case "ZONE_ALREADY_SET":
			log.G(context.TODO()).Infof("Firewalld: interface %s already part of %s zone, returning", intf, dockerZone)
			return nil
		case "ZONE_CONFLICT":
			// The interface has been bound to another zone outside the daemon.
			// Respect that, rather than failing to set up the network.
			log.G(context.TODO()).WithError(err).Infof("Firewalld: not adding interface %s to %s zone", intf, dockerZone)
			return nil
		}
		return err
	}
	return nil
}

// DelInterfaceFirewalld removes the interface from the trusted zone It is a
// no-op if firewalld is not running.
func DelInterfaceFirewalld(intf string) error {
	if !UsingFirewalld() {
		return nil
	}

	log.G(context.TODO()).Debugf("Firewalld: removing %s interface from %s zone", intf, dockerZone)
	// Runtime
	if err := connection.sysObj.Call(dbusInterface+".zone.removeInterface", 0, dockerZone, intf).Err; err != nil {
		switch firewalldErrorCode(err) {
		case "UNKNOWN_INTERFACE", "ZONE_CONFLICT":
			// The interface is in no zone, or in another zone.
			return &interfaceNotFound{fmt.Errorf("firewalld: interface %q not found in %s zone: %w", intf, dockerZone, err)}
		}
		return err
	}
	return nil
}

// firewalldErrorCode returns the code of an error raised by firewalld, such
// as "ZONE_CONFLICT", or "" if err is not a firewalld error.
func firewalldErrorCode(err error) string {
	var derr dbus.Error
	if !errors.As(err, &derr) || derr.Name != dbusInterface+".Exception" {
		return ""
	}
	// firewalld formats its errors as "CODE: message", or just "CODE".
	code, _, _ := strings.Cut(derr.Error(), ":")
	return code
}

type interfaceNotFound struct{ error }

func (interfaceNotFound) NotFound() {}

func contains(list []string, val string) bool {
	return slices.Contains(list, val)
}

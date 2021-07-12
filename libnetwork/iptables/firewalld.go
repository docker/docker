// +build linux,!no_systemd

package iptables

import (
	"fmt"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	"github.com/sirupsen/logrus"
)

// IPV defines the table string
type IPV string

const (
	// Iptables point ipv4 table
	Iptables IPV = "ipv4"
	// IP6Tables point to ipv6 table
	IP6Tables IPV = "ipv6"
	// Ebtables point to bridge table
	Ebtables IPV = "eb"
)

const (
	dbusInterface  = "org.fedoraproject.FirewallD1"
	dbusPath       = "/org/fedoraproject/FirewallD1"
	dbusConfigPath = "/org/fedoraproject/FirewallD1/config"
	dockerZone     = "docker"
)

// Conn is a connection to firewalld dbus endpoint.
type Conn struct {
	sysconn    *dbus.Conn
	sysObj     dbus.BusObject
	sysConfObj dbus.BusObject
	signal     chan *dbus.Signal
}

// ZoneSettings holds the firewalld zone settings, documented in
// https://firewalld.org/documentation/man-pages/firewalld.dbus.html
type ZoneSettings struct {
	version            string
	name               string
	description        string
	unused             bool
	target             string
	services           []string
	ports              [][]interface{}
	icmpBlocks         []string
	masquerade         bool
	forwardPorts       [][]interface{}
	interfaces         []string
	sourceAddresses    []string
	richRules          []string
	protocols          []string
	sourcePorts        [][]interface{}
	icmpBlockInversion bool
}

var (
	connection *Conn

	firewalldRunning bool      // is Firewalld service running
	onReloaded       []*func() // callbacks when Firewalld has been reloaded
)

// FirewalldInit initializes firewalld management code.
func FirewalldInit() error {
	var err error

	if connection, err = newConnection(); err != nil {
		return fmt.Errorf("Failed to connect to D-Bus system bus: %v", err)
	}
	firewalldRunning = checkRunning()
	if !firewalldRunning {
		connection.sysconn.Close()
		connection = nil
	}
	if connection != nil {
		go signalHandler()
		if err := setupDockerZone(); err != nil {
			return err
		}
	}

	return nil
}

// New() establishes a connection to the system bus.
func newConnection() (*Conn, error) {
	c := new(Conn)
	if err := c.initConnection(); err != nil {
		return nil, err
	}

	return c, nil
}

// Initialize D-Bus connection.
func (c *Conn) initConnection() error {
	var err error

	c.sysconn, err = dbus.SystemBus()
	if err != nil {
		return err
	}

	// This never fails, even if the service is not running atm.
	c.sysObj = c.sysconn.Object(dbusInterface, dbus.ObjectPath(dbusPath))
	c.sysConfObj = c.sysconn.Object(dbusInterface, dbus.ObjectPath(dbusConfigPath))
	rule := fmt.Sprintf("type='signal',path='%s',interface='%s',sender='%s',member='Reloaded'",
		dbusPath, dbusInterface, dbusInterface)
	c.sysconn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule)

	rule = fmt.Sprintf("type='signal',interface='org.freedesktop.DBus',member='NameOwnerChanged',path='/org/freedesktop/DBus',sender='org.freedesktop.DBus',arg0='%s'",
		dbusInterface)
	c.sysconn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule)

	c.signal = make(chan *dbus.Signal, 10)
	c.sysconn.Signal(c.signal)

	return nil
}

func signalHandler() {
	for signal := range connection.signal {
		if strings.Contains(signal.Name, "NameOwnerChanged") {
			firewalldRunning = checkRunning()
			dbusConnectionChanged(signal.Body)
		} else if strings.Contains(signal.Name, "Reloaded") {
			reloaded()
		}
	}
}

func dbusConnectionChanged(args []interface{}) {
	name := args[0].(string)
	oldOwner := args[1].(string)
	newOwner := args[2].(string)

	if name != dbusInterface {
		return
	}

	if len(newOwner) > 0 {
		connectionEstablished()
	} else if len(oldOwner) > 0 {
		connectionLost()
	}
}

func connectionEstablished() {
	reloaded()
}

func connectionLost() {
	// Doesn't do anything for now. Libvirt also doesn't react to this.
}

// call all callbacks
func reloaded() {
	for _, pf := range onReloaded {
		(*pf)()
	}
}

// OnReloaded add callback
func OnReloaded(callback func()) {
	for _, pf := range onReloaded {
		if pf == &callback {
			return
		}
	}
	onReloaded = append(onReloaded, &callback)
}

// Call some remote method to see whether the service is actually running.
func checkRunning() bool {
	var zone string
	var err error

	if connection != nil {
		err = connection.sysObj.Call(dbusInterface+".getDefaultZone", 0).Store(&zone)
		return err == nil
	}
	return false
}

// Passthrough method simply passes args through to iptables/ip6tables
func Passthrough(ipv IPV, args ...string) ([]byte, error) {
	var output string
	logrus.Debugf("Firewalld passthrough: %s, %s", ipv, args)
	if err := connection.sysObj.Call(dbusInterface+".direct.passthrough", 0, ipv, args).Store(&output); err != nil {
		return nil, err
	}
	return []byte(output), nil
}

// getDockerZoneSettings converts the ZoneSettings struct into a interface slice
func getDockerZoneSettings() []interface{} {
	settings := ZoneSettings{
		version:     "1.0",
		name:        dockerZone,
		description: "zone for docker bridge network interfaces",
		target:      "ACCEPT",
	}
	slice := []interface{}{
		settings.version,
		settings.name,
		settings.description,
		settings.unused,
		settings.target,
		settings.services,
		settings.ports,
		settings.icmpBlocks,
		settings.masquerade,
		settings.forwardPorts,
		settings.interfaces,
		settings.sourceAddresses,
		settings.richRules,
		settings.protocols,
		settings.sourcePorts,
		settings.icmpBlockInversion,
	}
	return slice

}

// setupDockerZone creates a zone called docker in firewalld which includes docker interfaces to allow
// container networking
func setupDockerZone() error {
	var zones []string
	// Check if zone exists
	if err := connection.sysObj.Call(dbusInterface+".zone.getZones", 0).Store(&zones); err != nil {
		return err
	}
	if contains(zones, dockerZone) {
		logrus.Infof("Firewalld: %s zone already exists, returning", dockerZone)
		return nil
	}
	logrus.Debugf("Firewalld: creating %s zone", dockerZone)

	settings := getDockerZoneSettings()
	// Permanent
	if err := connection.sysConfObj.Call(dbusInterface+".config.addZone", 0, dockerZone, settings).Err; err != nil {
		return err
	}
	// Reload for change to take effect
	if err := connection.sysObj.Call(dbusInterface+".reload", 0).Err; err != nil {
		return err
	}

	return nil
}

// AddInterfaceFirewalld adds the interface to the trusted zone
func AddInterfaceFirewalld(intf string) error {
	var intfs []string
	// Check if interface is already added to the zone
	if err := connection.sysObj.Call(dbusInterface+".zone.getInterfaces", 0, dockerZone).Store(&intfs); err != nil {
		return err
	}
	// Return if interface is already part of the zone
	if contains(intfs, intf) {
		logrus.Infof("Firewalld: interface %s already part of %s zone, returning", intf, dockerZone)
		return nil
	}

	logrus.Debugf("Firewalld: adding %s interface to %s zone", intf, dockerZone)
	// Runtime
	if err := connection.sysObj.Call(dbusInterface+".zone.addInterface", 0, dockerZone, intf).Err; err != nil {
		return err
	}
	return nil
}

// DelInterfaceFirewalld removes the interface from the trusted zone
func DelInterfaceFirewalld(intf string) error {
	var intfs []string
	// Check if interface is part of the zone
	if err := connection.sysObj.Call(dbusInterface+".zone.getInterfaces", 0, dockerZone).Store(&intfs); err != nil {
		return err
	}
	// Remove interface if it exists
	if !contains(intfs, intf) {
		return fmt.Errorf("Firewalld: unable to find interface %s in %s zone", intf, dockerZone)
	}

	logrus.Debugf("Firewalld: removing %s interface from %s zone", intf, dockerZone)
	// Runtime
	if err := connection.sysObj.Call(dbusInterface+".zone.removeInterface", 0, dockerZone, intf).Err; err != nil {
		return err
	}
	return nil
}

func contains(list []string, val string) bool {
	for _, v := range list {
		if v == val {
			return true
		}
	}
	return false
}

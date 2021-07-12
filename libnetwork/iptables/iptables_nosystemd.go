// +build linux,no_systemd

package iptables

const firewalldRunning = false

func initFirewalld() {
}

// Raw calls 'iptables' system command, passing supplied arguments.
func (iptable IPTable) Raw(args ...string) ([]byte, error) {
	return iptable.raw(args...)
}

func AddInterfaceFirewalld(intf string) error {
	return nil
}

func DelInterfaceFirewalld(intf string) error {
	return nil
}

// OnReloaded add callback
func OnReloaded(callback func()) {
}

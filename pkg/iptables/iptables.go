package iptables

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
)

type Action string

const (
	Add    Action = "-A"
	Delete Action = "-D"
)

var (
	ErrIptablesNotFound = errors.New("Iptables not found")
	nat                 = []string{"-t", "nat"}
	supportsXlock       = false
)

type Table struct {
	ipv6 bool
}

type Chain struct {
	Table  *Table
	Name   string
	Bridge string
}

func init() {
	supportsXlock = exec.Command("iptables", "--wait", "-L", "-n").Run() == nil
}

func GetTable(ipv6 bool) *Table {
	return &Table{ipv6: ipv6}
}

func (t *Table) NewChain(name, bridge string) (*Chain, error) {
	if output, err := t.Raw("-t", "nat", "-N", name); err != nil {
		return nil, err
	} else if len(output) != 0 {
		return nil, fmt.Errorf("Error creating new iptables chain: %s", output)
	}
	chain := &Chain{
		Table:  t,
		Name:   name,
		Bridge: bridge,
	}

	if err := chain.Prerouting(Add, "-m", "addrtype", "--dst-type", "LOCAL"); err != nil {
		return nil, fmt.Errorf("Failed to inject docker in PREROUTING chain: %s", err)
	}
	if err := chain.Output(Add, "-m", "addrtype", "--dst-type", "LOCAL", "!", "--dst", t.loopbackCidr()); err != nil {
		return nil, fmt.Errorf("Failed to inject docker in OUTPUT chain: %s", err)
	}
	return chain, nil
}

func (t *Table) RemoveExistingChain(name string) error {
	chain := &Chain{
		Table: t,
		Name:  name,
	}
	return chain.Remove()
}

func (c *Chain) Forward(action Action, ip net.IP, port int, proto, dest_addr string, dest_port int) error {
	daddr := ip.String()
	if ip.IsUnspecified() {
		// iptables interprets "0.0.0.0" as "0.0.0.0/32", whereas we
		// want "0.0.0.0/0". "0/0" is correctly interpreted as "any
		// value" by both iptables and ip6tables.
		daddr = "0/0"
	}
	if output, err := c.Raw("-t", "nat", fmt.Sprint(action), c.Name,
		"-p", proto,
		"-d", daddr,
		"--dport", strconv.Itoa(port),
		"!", "-i", c.Bridge,
		"-j", "DNAT",
		"--to-destination", net.JoinHostPort(dest_addr, strconv.Itoa(dest_port))); err != nil {
		return err
	} else if len(output) != 0 {
		return fmt.Errorf("Error iptables forward: %s", output)
	}

	fAction := action
	if fAction == Add {
		fAction = "-I"
	}
	if output, err := c.Raw(string(fAction), "FORWARD",
		"!", "-i", c.Bridge,
		"-o", c.Bridge,
		"-p", proto,
		"-d", dest_addr,
		"--dport", strconv.Itoa(dest_port),
		"-j", "ACCEPT"); err != nil {
		return err
	} else if len(output) != 0 {
		return fmt.Errorf("Error iptables forward: %s", output)
	}

	return nil
}

func (c *Chain) Prerouting(action Action, args ...string) error {
	a := append(nat, fmt.Sprint(action), "PREROUTING")
	if len(args) > 0 {
		a = append(a, args...)
	}
	if output, err := c.Raw(append(a, "-j", c.Name)...); err != nil {
		return err
	} else if len(output) != 0 {
		return fmt.Errorf("Error iptables prerouting: %s", output)
	}
	return nil
}

func (c *Chain) Output(action Action, args ...string) error {
	a := append(nat, fmt.Sprint(action), "OUTPUT")
	if len(args) > 0 {
		a = append(a, args...)
	}
	if output, err := c.Raw(append(a, "-j", c.Name)...); err != nil {
		return err
	} else if len(output) != 0 {
		return fmt.Errorf("Error iptables output: %s", output)
	}
	return nil
}

func (t *Table) loopbackCidr() string {
	if t.ipv6 {
		return "::1/128"
	} else {
		return "127.0.0.0/8"
	}
}

func (c *Chain) Remove() error {
	// Ignore errors - This could mean the chains were never set up
	c.Prerouting(Delete, "-m", "addrtype", "--dst-type", "LOCAL")
	c.Output(Delete, "-m", "addrtype", "--dst-type", "LOCAL", "!", "--dst", c.Table.loopbackCidr())
	c.Output(Delete, "-m", "addrtype", "--dst-type", "LOCAL") // Created in versions <= 0.1.6

	c.Prerouting(Delete)
	c.Output(Delete)

	c.Raw("-t", "nat", "-F", c.Name)
	c.Raw("-t", "nat", "-X", c.Name)

	return nil
}

// Check if an existing rule exists
func (t *Table) Exists(args ...string) bool {
	// iptables -C, --check option was added in v.1.4.11
	// http://ftp.netfilter.org/pub/iptables/changes-iptables-1.4.11.txt

	// try -C
	// if exit status is 0 then return true, the rule exists
	if _, err := t.Raw(append([]string{"-C"}, args...)...); err == nil {
		return true
	}

	// parse iptables-save for the rule
	rule := strings.Replace(strings.Join(args, " "), "-t nat ", "", -1)
	existingRules, _ := exec.Command("iptables-save").Output()

	// regex to replace ips in rule
	// because MASQUERADE rule will not be exactly what was passed
	re := regexp.MustCompile(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\/[0-9]{1,2}`)

	return strings.Contains(
		re.ReplaceAllString(string(existingRules), "?"),
		re.ReplaceAllString(rule, "?"),
	)
}

func (c *Chain) Raw(args ...string) ([]byte, error) {
	return c.Table.Raw(args...)
}

func (t *Table) Raw(args ...string) ([]byte, error) {
	var cmd string
	if t.ipv6 {
		cmd = "ip6tables"
	} else {
		cmd = "iptables"
	}

	path, err := exec.LookPath(cmd)
	if err != nil {
		return nil, ErrIptablesNotFound
	}

	if supportsXlock {
		args = append([]string{"--wait"}, args...)
	}

	log.Debugf("%s, %v", path, args)

	output, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v failed: %v %v: %s (%s)", cmd, cmd, strings.Join(args, " "), output, err)
	}

	// ignore iptables' message about xtables lock
	if strings.Contains(string(output), "waiting for it to exit") {
		output = []byte("")
	}

	return output, err
}

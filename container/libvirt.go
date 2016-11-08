// +build linux

package container

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
        "os"
	"os/exec"
	"strconv"
        "strings"

	libvirtgo "github.com/rgbkrk/libvirt-go"
        "github.com/Sirupsen/logrus"
)

var LibvirtdAddress = "qemu:///system"

type BootConfig struct {
	CPU              int
        DefaultMaxCpus   int
        DefaultMaxMem    int
	Memory           int
        DiskPath         string
        OriginalDiskPath string
}

func (container *Container) InitDriver() *LibvirtDriver {
	conn, err := libvirtgo.NewVirConnection(LibvirtdAddress)
	if err != nil {
		logrus.Error("fail to connect to libvirtd ", LibvirtdAddress, err)
		return nil
	}

	return &LibvirtDriver{
		conn: conn,
	}
}

func (ld *LibvirtDriver) InitContext(c *Container) *LibvirtContext {
	return &LibvirtContext{
		driver:    ld,
                container: c,
	}
}

func (ld *LibvirtDriver) checkConnection() error {
	if alive, _ := ld.conn.IsAlive(); !alive {
		logrus.Info("libvirt disconnected, reconnect")
		conn, err := libvirtgo.NewVirConnection(LibvirtdAddress)
		if err != nil {
			return err
		}
		ld.conn.CloseConnection()
		ld.conn = conn
		return nil
	}
	return fmt.Errorf("connection is alive")
}

type memory struct {
	Unit    string `xml:"unit,attr"`
	Content int    `xml:",chardata"`
}

type maxmem struct {
	Unit    string `xml:"unit,attr"`
	Slots   string `xml:"slots,attr"`
	Content int    `xml:",chardata"`
}

type vcpu struct {
	Placement string `xml:"placement,attr"`
	Current   string `xml:"current,attr"`
	Content   int    `xml:",chardata"`
}

type cell struct {
	Id     string `xml:"id,attr"`
	Cpus   string `xml:"cpus,attr"`
	Memory string `xml:"memory,attr"`
	Unit   string `xml:"unit,attr"`
}

type cpu struct {
	Mode  string    `xml:"mode,attr"`
}

type ostype struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Content string `xml:",chardata"`
}

type domainos struct {
	Supported string    `xml:"supported,attr"`
	Type      ostype    `xml:"type"`
}

type fspath struct {
	Dir string `xml:"dir,attr"`
}

type filesystem struct {
	Type       string   `xml:"type,attr"`
	Accessmode string   `xml:"accessmode,attr"`
	Source     fspath   `xml:"source"`
	Target     fspath   `xml:"target"`
}

type diskdriver struct {
	Type   string `xml:"type,attr"`
        Name   string `xml:"name,attr"`
}

type disksource struct {
	File string `xml:"file,attr"`
}

type diskformat struct {
	Type string `xml:"type,attr"`
}

type backingstore struct {
	Type   string     `xml:"type,attr"`
        Index  string     `xml:"index,attr"`
        Format diskformat `xml:"format"`
	Source disksource `xml:"source"`
}

type disktarget struct {
	Dev  string     `xml:"dev,attr"`
        Bus  string     `xml:"bus,attr"`
}

type readonly struct {
}

type disk struct {
	Type         string       `xml:"type,attr"`
	Device       string       `xml:"device,attr"`
	Driver       diskdriver   `xml:"driver"`
	Source       disksource   `xml:"source"`
        BackingStore *backingstore `xml:"backingstore,omitempty"`
	Target       disktarget   `xml:"target"`
        Readonly     *readonly     `xml:"readonly,omitempty"`
}

type channsrc struct {
	Mode string `xml:"mode,attr"`
	Path string `xml:"path,attr"`
}

type constgt struct {
	Type string `xml:"type,attr,omitempty"`
	Port string `xml:"port,attr"`
}

type console struct {
	Type   string   `xml:"type,attr"`
	Source channsrc `xml:"source"`
        Target constgt `xml:"target"`
}

type device struct {
	Emulator    string       `xml:"emulator"`
        Filesystems []filesystem `xml:"filesystem"`
	Disks       []disk       `xml:"disk"`
	Consoles    []console    `xml:"console"`
}

type seclab struct {
	Type string `xml:"type,attr"`
}

type domain struct {
	XMLName    xml.Name `xml:"domain"`
	Type       string   `xml:"type,attr"`
	Name       string   `xml:"name"`
	Memory     memory   `xml:"memory"`
	MaxMem     *maxmem  `xml:"maxMemory,omitempty"`
	VCpu       vcpu     `xml:"vcpu"`
	OS         domainos `xml:"os"`
	CPU        cpu      `xml:"cpu"`
	OnPowerOff string   `xml:"on_poweroff"`
	OnReboot   string   `xml:"on_reboot"`
	OnCrash    string   `xml:"on_crash"`
	Devices    device   `xml:"devices"`
	SecLabel   seclab   `xml:"seclabel"`
}

func (lc *LibvirtContext) createSeedImage() (string, error) {
        cloudLocaladsPath, err := exec.LookPath("cloud-localds")
	if err != nil {
		return "", fmt.Errorf("cloud-localads is not installed on your PATH. Please, install it to run isolated container")
	}

        // Create directory for seed.img
        seedDirectory := fmt.Sprintf("/var/run/docker-qemu/%s", lc.container.ID)

        err = os.MkdirAll(seedDirectory, 0777)
	if err != nil {
		return "", fmt.Errorf("Could not create directory /var/run/docker-qemu/%s", lc.container.ID)
	}

        // Create user-data to be included in seed.img
        data := `#cloud-config
password: passw0rd
chpasswd: { expire: False }
ssh_pwauth: True
runcmd:
 - mount -t 9p -o trans=virtio share_dir /mnt
 - chroot /mnt %s > /dev/hvc1
 
`

        path := lc.container.Path

        args := strings.TrimPrefix(fmt.Sprint(lc.container.Args), "[")
        args = strings.TrimSuffix(args, "]")

        entrypoint := path + " " + args

        logrus.Infof("The data is: %s", fmt.Sprintf(data, entrypoint))
        userData := []byte(fmt.Sprintf(data, entrypoint))

        currentDir, err := os.Getwd()
        if err != nil {
                return "", fmt.Errorf("Could not determine the current directory")
        }

        err = os.Chdir(seedDirectory)
        if err != nil {
                return "", fmt.Errorf("Could not changed to directory %s", seedDirectory)
        }

        writeError := ioutil.WriteFile("user-data", userData, 0700)
	if writeError != nil {
		return "", fmt.Errorf("Could not write user-data to /var/run/docker-qemu/%s", lc.container.ID)
	}
        logrus.Infof("cloud-localads path: %s", cloudLocaladsPath)

        err = exec.Command(cloudLocaladsPath, "seed.img", "user-data").Run()
        if err != nil {
                return "", fmt.Errorf("Could not execute cloud-localads")
        }

        err = os.Chdir(currentDir)
        if err != nil {
                return "", fmt.Errorf("Could not changed to directory %s", currentDir)
        }

        return seedDirectory+"/seed.img", nil
}

func (lc *LibvirtContext) domainXml() (string, error) {
        seedImageLocation, err := lc.createSeedImage()
        if err != nil {
                return "", fmt.Errorf("Could not create seed image")
        }

        logrus.Infof("Seed image location: %s", seedImageLocation)

	boot := &BootConfig{
	         CPU:              1,
                 DefaultMaxCpus:   2,
                 DefaultMaxMem:    128,
		 Memory:           128,
                 DiskPath:         "/home/abhishek/Documents/Works/cloudInit/disk.img",
                 OriginalDiskPath: "/home/abhishek/Documents/Works/cloudInit/disk.img.orig",
		}

	dom := &domain{
		Type: "kvm",
		Name: lc.container.ID[0:12],
	}

	dom.Memory.Unit = "MiB"
	dom.Memory.Content = boot.Memory

	dom.VCpu.Placement = "static"
	dom.VCpu.Current = strconv.Itoa(boot.CPU)
	dom.VCpu.Content = boot.CPU

	dom.OS.Supported = "yes"
	dom.OS.Type.Arch = "x86_64"
	dom.OS.Type.Machine = "pc-i440fx-2.0"
	dom.OS.Type.Content = "hvm"

	dom.SecLabel.Type = "none"

	dom.CPU.Mode = "host-passthrough"

	cmd, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return "", fmt.Errorf("cannot find qemu-system-x86_64 binary")
	}
	dom.Devices.Emulator = cmd

	dom.OnPowerOff = "destroy"
	dom.OnReboot = "destroy"
	dom.OnCrash = "destroy"

	diskimage := disk{
		Type:       "file",
		Device:     "disk",
		Driver: diskdriver{
                        Name: "qemu",
			Type: "qcow2",
		},
		Source: disksource{
			File: boot.DiskPath,
		},
		BackingStore: &backingstore{
			      Type: "file",
                              Index: "1",
                              Format: diskformat{
                                      Type: "raw",
                              },
                              Source: disksource{
                                      File: boot.OriginalDiskPath,
                              },
		},
		Target: disktarget{
			Dev:     "hda",
			Bus:     "ide",
		},
	}
	dom.Devices.Disks = append(dom.Devices.Disks, diskimage)

	seedimage := disk{
		Type:       "file",
		Device:     "cdrom",
		Driver: diskdriver{
                        Name: "qemu",
			Type: "raw",
		},
		Source: disksource{
			File: seedImageLocation,
		},
		Target: disktarget{
			Dev:     "hdb",
			Bus:     "ide",
		},
                Readonly: &readonly{},
	}
	dom.Devices.Disks = append(dom.Devices.Disks, seedimage)

	fs := filesystem{
		Type:       "mount",
		Accessmode:     "passthrough",
		Source: fspath{
			Dir: lc.container.BaseFS,
		},
		Target: fspath{
			Dir:     "share_dir",
		},
	}
	dom.Devices.Filesystems = append(dom.Devices.Filesystems, fs)

        serialConsole := console{
             Type: "unix",
	     Source: channsrc{
		Mode: "bind",
		Path: lc.container.Config.SerialConsoleSockName,
		},
	     Target: constgt{
		Type: "serial",
		Port: "0",
	     },
        }
	dom.Devices.Consoles = append(dom.Devices.Consoles, serialConsole)
        logrus.Infof("Serial console socket location: %s", lc.container.Config.SerialConsoleSockName)

        ubuntuConsole := console{
             Type: "unix",
	     Source: channsrc{
		Mode: "bind",
		Path: "/arbritary.sock",
		},
	     Target: constgt{
		Type: "virtio",
		Port: "1",
	     },
        }
	dom.Devices.Consoles = append(dom.Devices.Consoles, ubuntuConsole)

        appConsole := console{
             Type: "unix",
	     Source: channsrc{
		Mode: "bind",
		Path: lc.container.Config.AppConsoleSockName,
		},
	     Target: constgt{
		Type: "virtio",
		Port: "2",
	     },
        }
	dom.Devices.Consoles = append(dom.Devices.Consoles, appConsole)

        logrus.Infof("Serial console socket location: %s", lc.container.Config.AppConsoleSockName)

	data, err := xml.Marshal(dom)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (lc *LibvirtContext) Create() {
        domainXml, err := lc.domainXml()
        if err != nil {
                logrus.Error("Fail to get domain xml configuration:", err)
                return
        }
        logrus.Infof("domainXML: %v", domainXml)
        var domain libvirtgo.VirDomain

        domain, err = lc.driver.conn.DomainDefineXML(domainXml)
        if err != nil {
                logrus.Error("Fail to launch domain ", err)
                return
        }

        lc.domain = &domain
        err = lc.domain.SetMemoryStatsPeriod(1, 0)
        if err != nil {
                logrus.Errorf("SetMemoryStatsPeriod failed for domain %v", lc.container.ID)
        }
}

func (lc *LibvirtContext) Launch() {
        if lc.domain == nil {
                logrus.Error("Failed to launch domain as no domain in LibvirtContext")
                return
        }

        err := lc.domain.Create()
	if err != nil {
		logrus.Error("Fail to start isolated container ", err)
		return
	}
}

func (lc *LibvirtContext) Shutdown() {
	if lc.domain == nil {
		return
	}

	lc.domain.DestroyFlags(libvirtgo.VIR_DOMAIN_DESTROY_DEFAULT)
	logrus.Infof("Domain shutdown")
}

func (lc *LibvirtContext) Close() {
	lc.domain = nil
}

func (lc *LibvirtContext) Pause(pause bool) error {
	if lc.domain == nil {
		return fmt.Errorf("Cannot find domain")
	}

	if pause {
		logrus.Infof("Domain suspended:", lc.domain.Suspend())
                return nil
	} else {
		logrus.Infof("Domain resumed:", lc.domain.Resume())
                return nil
	}
}

/*type nicmac struct {
	Address string `xml:"address,attr"`
}

type nicsrc struct {
	Bridge string `xml:"bridge,attr"`
}

type nictgt struct {
	Device string `xml:"dev,attr,omitempty"`
}

type nicmodel fsdriver

type nicBound struct {
	// in kilobytes/second
	Average string `xml:"average,attr"`
	Peak    string `xml:"peak,attr"`
}

type bandwidth struct {
	XMLName  xml.Name  `xml:"bandwidth"`
	Inbound  *nicBound `xml:"inbound,omitempty"`
	Outbound *nicBound `xml:"outbound,omitempty"`
}

type nic struct {
	XMLName   xml.Name   `xml:"interface"`
	Type      string     `xml:"type,attr"`
	Mac       nicmac     `xml:"mac"`
	Source    nicsrc     `xml:"source"`
	Target    *nictgt    `xml:"target,omitempty"`
	Model     nicmodel   `xml:"model"`
	Address   *address   `xml:"address"`
	Bandwidth *bandwidth `xml:"bandwidth,omitempty"`
}

func nicXml(container *container.Container, config *BootConfig) (string, error) {
	slot := fmt.Sprintf("0x%x", addr)

	n := nic{
		Type: "bridge",
		Mac: nicmac{
			Address: mac,
		},
		Source: nicsrc{
			Bridge: bridge,
		},
		Target: &nictgt{
			Device: device,
		},
		Model: nicmodel{
			Type: "virtio",
		},
		Address: &address{
			Type:     "pci",
			Domain:   "0x0000",
			Bus:      "0x00",
			Slot:     slot,
			Function: "0x0",
		},
	}

	if config.InboundAverage != "" || config.OutboundAverage != "" {
		b := &bandwidth{}
		if config.InboundAverage != "" {
			b.Inbound = &nicBound{Average: config.InboundAverage, Peak: config.InboundPeak}
		}
		if config.OutboundAverage != "" {
			b.Outbound = &nicBound{Average: config.OutboundAverage, Peak: config.OutboundPeak}
		}
		n.Bandwidth = b
	}

	data, err := xml.Marshal(n)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (lc *LibvirtContext) AddNic(ctx *hypervisor.VmContext, host *hypervisor.HostNicInfo, guest *hypervisor.GuestNicInfo, result chan<- hypervisor.VmEvent) {
	if lc.domain == nil {
		logrus.Error("Cannot find domain")
		result <- &hypervisor.DeviceFailed{
			Session: nil,
		}
		return
	}

	nicXml, err := nicXml(host.Bridge, host.Device, host.Mac, guest.Busaddr, ctx.Boot)
	if err != nil {
		logrus.Error("generate attach-nic-xml failed, ", err.Error())
		result <- &hypervisor.DeviceFailed{
			Session: nil,
		}
		return
	}
	logrus.Infof("nicxml: %s", nicXml)

	err = lc.domain.AttachDeviceFlags(nicXml, libvirtgo.VIR_DOMAIN_DEVICE_MODIFY_LIVE)
	if err != nil {
		logrus.Error("attach nic failed, ", err.Error())
		result <- &hypervisor.DeviceFailed{
			Session: nil,
		}
		return
	}

	result <- &hypervisor.NetDevInsertedEvent{
		Index:      guest.Index,
		DeviceName: guest.Device,
		Address:    guest.Busaddr,
	}
}

func (lc *LibvirtContext) RemoveNic(ctx *hypervisor.VmContext, n *hypervisor.InterfaceCreated, callback hypervisor.VmEvent) {
	if lc.domain == nil {
		logrus.Error("Cannot find domain")
		ctx.Hub <- &hypervisor.DeviceFailed{
			Session: nil,
		}
		return
	}

	nicXml, err := nicXml(n.Bridge, n.HostDevice, n.MacAddr, n.PCIAddr, ctx.Boot)
	if err != nil {
		logrus.Error("generate detach-nic-xml failed, ", err.Error())
		ctx.Hub <- &hypervisor.DeviceFailed{
			Session: callback,
		}
		return
	}

	err = lc.domain.DetachDeviceFlags(nicXml, libvirtgo.VIR_DOMAIN_DEVICE_MODIFY_LIVE)
	if err != nil {
		logrus.Error("detach nic failed, ", err.Error())
		ctx.Hub <- &hypervisor.DeviceFailed{
			Session: callback,
		}
		return
	}
	ctx.Hub <- callback
}

func (lc *LibvirtContext) SetCpus(ctx *hypervisor.VmContext, cpus int, result chan<- error) {
	logrus.Infof("setcpus %d", cpus)
	if lc.domain == nil {
		result <- fmt.Errorf("Cannot find domain")
		return
	}

	err := lc.domain.SetVcpusFlags(uint(cpus), libvirtgo.VIR_DOMAIN_VCPU_LIVE)
	result <- err
}

func (lc *LibvirtContext) AddMem(ctx *hypervisor.VmContext, slot, size int, result chan<- error) {
	memdevXml := fmt.Sprintf("<memory model='dimm'><target><size unit='MiB'>%d</size><node>0</node></target></memory>", size)
	logrus.Infof("memdevXml: %s", memdevXml)
	if lc.domain == nil {
		result <- fmt.Errorf("Cannot find domain")
		return
	}

	err := lc.domain.AttachDeviceFlags(memdevXml, libvirtgo.VIR_DOMAIN_DEVICE_MODIFY_LIVE)
	result <- err
}

func (lc *LibvirtContext) Save(ctx *hypervisor.VmContext, path string, result chan<- error) {
	logrus.Infof("save domain to: %s", path)

	if ctx.Boot.BootToBeTemplate {
		err := exec.Command("virsh", "-c", LibvirtdAddress, "qemu-monitor-command", ctx.Id, "--hmp", "migrate_set_capability bypass-shared-memory on").Run()
		if err != nil {
			result <- err
			return
		}
	}

	// lc.domain.Save(path) will have libvirt header and will destroy the vm
	// TODO: use virsh qemu-monitor-event to query until completed
	err := exec.Command("virsh", "-c", LibvirtdAddress, "qemu-monitor-command", ctx.Id, "--hmp", fmt.Sprintf("migrate exec:cat>%s", path)).Run()
	result <- err
}*/

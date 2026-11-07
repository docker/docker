package daemon

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/osversion"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/system"
	"github.com/moby/moby/v2/daemon/libnetwork"
	nwconfig "github.com/moby/moby/v2/daemon/libnetwork/config"
	winlibnetwork "github.com/moby/moby/v2/daemon/libnetwork/drivers/windows"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/options"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/pkg/parsers/operatingsystem"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/moby/sys/user"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsMinCPUShares  = 1
	windowsMaxCPUShares  = 10000
	windowsMinCPUPercent = 1
	windowsMaxCPUPercent = 100
)

// Windows containers are much larger than Linux containers and each of them
// have > 20 system processes which why we use much smaller parallelism value.
func adjustParallelLimit(n int, limit int) int {
	return int(math.Max(1, math.Floor(float64(runtime.NumCPU())*.8)))
}

func setHostGatewayIP(*libnetwork.Controller, *config.Config) {
}

// Windows has no concept of an execution state directory. So use config.Root here.
func getPluginExecRoot(cfg *config.Config) string {
	return filepath.Join(cfg.Root, "plugins")
}

func (daemon *Daemon) parseSecurityOpt(daemonCfg *config.Config, securityOptions *container.SecurityOptions, hostConfig *containertypes.HostConfig) error {
	return nil
}

func setupInitLayer(uid int, gid int) func(string) error {
	return nil
}

// adaptContainerSettings is called during container creation to modify any
// settings necessary in the HostConfig structure.
func (daemon *Daemon) adaptContainerSettings(daemonCfg *config.Config, hostConfig *containertypes.HostConfig) error {
	return nil
}

// verifyPlatformContainerResources performs platform-specific validation of the container's resource-configuration
func verifyPlatformContainerResources(resources *containertypes.Resources, isHyperv bool) (warnings []string, _ error) {
	fixMemorySwappiness(resources)
	if !isHyperv {
		// The processor resource controls are mutually exclusive on
		// Windows Server Containers, the order of precedence is
		// CPUCount first, then CPUShares, and CPUPercent last.
		if resources.CPUCount > 0 {
			if resources.CPUShares > 0 {
				warnings = append(warnings, "Conflicting options: CPU count takes priority over CPU shares on Windows Server Containers. CPU shares discarded")
				resources.CPUShares = 0
			}
			if resources.CPUPercent > 0 {
				warnings = append(warnings, "Conflicting options: CPU count takes priority over CPU percent on Windows Server Containers. CPU percent discarded")
				resources.CPUPercent = 0
			}
		} else if resources.CPUShares > 0 {
			if resources.CPUPercent > 0 {
				warnings = append(warnings, "Conflicting options: CPU shares takes priority over CPU percent on Windows Server Containers. CPU percent discarded")
				resources.CPUPercent = 0
			}
		}
	}

	if resources.CPUShares < 0 || resources.CPUShares > windowsMaxCPUShares {
		return warnings, fmt.Errorf("range of CPUShares is from %d to %d", windowsMinCPUShares, windowsMaxCPUShares)
	}
	if resources.CPUPercent < 0 || resources.CPUPercent > windowsMaxCPUPercent {
		return warnings, fmt.Errorf("range of CPUPercent is from %d to %d", windowsMinCPUPercent, windowsMaxCPUPercent)
	}
	if resources.CPUCount < 0 {
		return warnings, fmt.Errorf("invalid CPUCount: CPUCount cannot be negative")
	}

	if resources.NanoCPUs > 0 && resources.CPUPercent > 0 {
		return warnings, fmt.Errorf("conflicting options: Nano CPUs and CPU Percent cannot both be set")
	}
	if resources.NanoCPUs > 0 && resources.CPUShares > 0 {
		return warnings, fmt.Errorf("conflicting options: Nano CPUs and CPU Shares cannot both be set")
	}
	// The precision we could get is 0.01, because on Windows we have to convert to CPUPercent.
	// We don't set the lower limit here and it is up to the underlying platform (e.g., Windows) to return an error.
	if resources.NanoCPUs != 0 {
		nc := runtime.NumCPU()
		if resources.NanoCPUs < 0 || resources.NanoCPUs > int64(nc)*1e9 {
			return warnings, fmt.Errorf("range of CPUs is from 0.01 to %[1]d.00, as there are only %[1]d CPUs available", nc)
		}
	}

	if len(resources.BlkioDeviceReadBps) > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioDeviceReadBps")
	}
	if len(resources.BlkioDeviceReadIOps) > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioDeviceReadIOps")
	}
	if len(resources.BlkioDeviceWriteBps) > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioDeviceWriteBps")
	}
	if len(resources.BlkioDeviceWriteIOps) > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioDeviceWriteIOps")
	}
	if resources.BlkioWeight > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioWeight")
	}
	if len(resources.BlkioWeightDevice) > 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support BlkioWeightDevice")
	}
	if resources.CgroupParent != "" {
		return warnings, fmt.Errorf("invalid option: Windows does not support CgroupParent")
	}
	if resources.CPUPeriod != 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support CPUPeriod")
	}
	if resources.CpusetCpus != "" {
		return warnings, fmt.Errorf("invalid option: Windows does not support CpusetCpus")
	}
	if resources.CpusetMems != "" {
		return warnings, fmt.Errorf("invalid option: Windows does not support CpusetMems")
	}
	if resources.MemoryReservation != 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support MemoryReservation")
	}
	if resources.MemorySwap != 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support MemorySwap")
	}
	if resources.MemorySwappiness != nil {
		return warnings, fmt.Errorf("invalid option: Windows does not support MemorySwappiness")
	}
	if resources.OomKillDisable != nil && *resources.OomKillDisable {
		return warnings, fmt.Errorf("invalid option: Windows does not support OomKillDisable")
	}
	if resources.PidsLimit != nil && *resources.PidsLimit != 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support PidsLimit")
	}
	if len(resources.Ulimits) != 0 {
		return warnings, fmt.Errorf("invalid option: Windows does not support Ulimits")
	}
	return warnings, nil
}

// verifyPlatformContainerSettings performs platform-specific validation of the
// hostconfig and config structures.
func verifyPlatformContainerSettings(daemon *Daemon, daemonCfg *configStore, hostConfig *containertypes.HostConfig, config *containertypes.Config, update bool) (warnings []string, _ error) {
	if hostConfig == nil {
		return nil, nil
	}
	if hostConfig.Umask != nil {
		return nil, errors.New("invalid option: Windows does not support Umask")
	}
	return verifyPlatformContainerResources(&hostConfig.Resources, daemon.runAsHyperVContainer(hostConfig))
}

// verifyDaemonSettings performs validation of daemon config struct
func verifyDaemonSettings(config *config.Config) error {
	return nil
}

// checkSystem validates platform-specific requirements
func checkSystem() error {
	// Validate the OS version. Note that dockerd.exe must be manifested for this
	// call to return the correct version.
	if osversion.Get().MajorVersion < 10 || osversion.Build() < osversion.RS5 {
		return fmt.Errorf("this version of Windows does not support the docker daemon (Windows build %d or higher is required)", osversion.RS5)
	}

	vmcompute := windows.NewLazySystemDLL("vmcompute.dll")
	if vmcompute.Load() != nil {
		return fmt.Errorf("failed to load vmcompute.dll, ensure that the Containers feature is installed")
	}

	// Ensure that the required Host Network Service and vmcompute services
	// are running. Docker will fail in unexpected ways if this is not present.
	requiredServices := []string{"hns", "vmcompute"}
	if err := ensureServicesInstalled(requiredServices); err != nil {
		return errors.Wrap(err, "a required service is not installed, ensure the Containers feature is installed")
	}

	return nil
}

func ensureServicesInstalled(services []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	for _, service := range services {
		s, err := m.OpenService(service)
		if err != nil {
			return errors.Wrapf(err, "failed to open service %s", service)
		}
		s.Close()
	}
	return nil
}

// configureKernelSecuritySupport configures and validate security support for the kernel
func configureKernelSecuritySupport(config *config.Config, driverName string) error {
	return nil
}

// configureMaxThreads sets the Go runtime max threads threshold
func configureMaxThreads(_ context.Context) error {
	return nil
}

// initOCIHooks is a no-op on Windows, where nothing installs OCI hooks.
func (daemon *Daemon) initOCIHooks(*config.Config) error {
	return nil
}

func (daemon *Daemon) initNetworkController(daemonCfg *config.Config, activeSandboxes map[string]any) error {
	netOptions, err := daemon.networkOptions(daemonCfg, nil, daemon.id, nil)
	if err != nil {
		return err
	}
	if err := daemon.newNetworkController(context.TODO(), daemonCfg, netOptions); err != nil {
		return errors.Wrap(err, "error obtaining controller instance")
	}

	hnsresponse, err := hcsshim.HNSListNetworkRequest(http.MethodGet, "", "")
	if err != nil {
		return err
	}

	ctx := context.TODO()

	// Remove networks not present in HNS
	for _, v := range daemon.netController.Networks(ctx) {
		hnsid := v.DriverOptions()[winlibnetwork.HNSID]
		found := false

		for _, v := range hnsresponse {
			if v.Id == hnsid {
				found = true
				break
			}
		}

		if !found {
			// non-default nat networks should be re-created if missing from HNS
			if v.Type() == "nat" && v.Name() != networktypes.NetworkNat {
				ipamDriver, ipamOptions, v4Conf, v6Conf := v.IpamConfig()
				netOption := map[string]string{}
				for k, v := range v.DriverOptions() {
					if k != winlibnetwork.HNSID {
						netOption[k] = v
					}
				}
				name := v.Name()
				id := v.ID()

				err = v.Delete()
				if err != nil {
					log.G(context.TODO()).Errorf("Error occurred when removing network %v", err)
				}

				_, err := daemon.netController.NewNetwork(context.TODO(), "nat", name, id,
					libnetwork.NetworkOptionGeneric(options.Generic{
						netlabel.GenericData: netOption,
					}),
					libnetwork.NetworkOptionIpam(ipamDriver, "", v4Conf, v6Conf, ipamOptions),
					libnetwork.NetworkOptionLabels(v.Labels()),
				)
				if err != nil {
					log.G(context.TODO()).Errorf("Error occurred when creating network %v", err)
				}
				continue
			}

			// global networks should not be deleted by local HNS
			if v.Scope() != scope.Global {
				err = v.Delete()
				if err != nil {
					log.G(context.TODO()).Errorf("Error occurred when removing network %v", err)
				}
			}
		}
	}

	_, err = daemon.netController.NewNetwork(context.TODO(), "null", "none", "", libnetwork.NetworkOptionPersist(false))
	if err != nil {
		return err
	}

	defaultNetworkExists := false

	if network, err := daemon.netController.NetworkByName(network.DefaultNetwork); err == nil {
		hnsid := network.DriverOptions()[winlibnetwork.HNSID]
		for _, v := range hnsresponse {
			if hnsid == v.Id {
				defaultNetworkExists = true
				break
			}
		}
	}

	// discover and add HNS networks to windows
	// network that exist are removed and added again
	for _, v := range hnsresponse {
		// Ignore HNS network types that are not supported by the Docker driver. This
		// avoids trying to load a plugin named after the network type, which adds a 15s
		// startup delay per network of this type on the host.
		if !winlibnetwork.IsAdoptableNetworkType(v.Type) {
			continue
		}
		var n *libnetwork.Network
		daemon.netController.WalkNetworks(func(current *libnetwork.Network) bool {
			hnsid := current.DriverOptions()[winlibnetwork.HNSID]
			if hnsid == v.Id {
				n = current
				return true
			}
			return false
		})

		drvOptions := make(map[string]string)
		var labels map[string]string
		nid := ""
		ipamDriver := "default"
		var ipamOptions map[string]string
		if n != nil {
			nid = n.ID()

			// global networks should not be deleted by local HNS
			if n.Scope() == scope.Global {
				continue
			}
			ipamDriver, ipamOptions, _, _ = n.IpamConfig()

			// This will not cause network delete from HNS as the network
			// is not yet populated in the libnetwork windows driver

			// restore option if it existed before
			drvOptions = n.DriverOptions()
			labels = n.Labels()
			n.Delete()
		}
		netOption := map[string]string{
			winlibnetwork.NetworkName: v.Name,
			winlibnetwork.HNSID:       v.Id,
		}

		// If this network wasn't in the store, it's being adopted by docker - which
		// makes it possible to run containers in networks that were defined on the host.
		// Add a label to say that's what happened.
		//
		// Networks not created by docker should not be deleted by "docker network
		// prune". (They can still be deleted by "docker network rm", but that's more
		// likely to be intentional - preventing it may be a breaking change, it would
		// become impossible to use docker commands to delete a network that was created
		// by docker but got forgotten because the store somehow got deleted.)
		if n == nil {
			netOption[winlibnetwork.HNSOwned] = "true"
		}

		// add persisted driver options
		for k, v := range drvOptions {
			if k != winlibnetwork.NetworkName && k != winlibnetwork.HNSID {
				netOption[k] = v
			}
		}

		v4Conf := []*libnetwork.IpamConf{}
		for _, subnet := range v.Subnets {
			ipamV4Conf := libnetwork.IpamConf{}
			ipamV4Conf.PreferredPool = subnet.AddressPrefix
			ipamV4Conf.Gateway = subnet.GatewayAddress
			v4Conf = append(v4Conf, &ipamV4Conf)
		}

		name := v.Name
		if n != nil {
			name = n.Name()
		}

		// If there is no nat network create one from the first NAT network
		// encountered if it doesn't already exist
		if !defaultNetworkExists && strings.EqualFold(network.DefaultNetwork, v.Type) && n == nil {
			name = network.DefaultNetwork
			defaultNetworkExists = true
		}

		v6Conf := []*libnetwork.IpamConf{}
		_, err := daemon.netController.NewNetwork(context.TODO(), strings.ToLower(v.Type), name, nid,
			libnetwork.NetworkOptionGeneric(options.Generic{
				netlabel.GenericData: netOption,
				netlabel.EnableIPv4:  true,
			}),
			libnetwork.NetworkOptionIpam(ipamDriver, "", v4Conf, v6Conf, ipamOptions),
			libnetwork.NetworkOptionLabels(labels),
		)
		if err != nil {
			log.G(context.TODO()).Errorf("Error occurred when creating network %v", err)
		}
	}

	if !daemonCfg.DisableBridge {
		// Initialize default driver "bridge"
		if err := initBridgeDriver(daemon.netController, daemonCfg.BridgeConfig); err != nil {
			return err
		}
	}

	return nil
}

func initBridgeDriver(controller *libnetwork.Controller, config config.BridgeConfig) error {
	if _, err := controller.NetworkByName(network.DefaultNetwork); err == nil {
		return nil
	}

	netOption := map[string]string{
		winlibnetwork.NetworkName: network.DefaultNetwork,
	}

	var ipamOption libnetwork.NetworkOption
	var subnetPrefix string

	if config.FixedCIDR != "" {
		subnetPrefix = config.FixedCIDR
	}

	if subnetPrefix != "" {
		ipamV4Conf := libnetwork.IpamConf{PreferredPool: subnetPrefix}
		v4Conf := []*libnetwork.IpamConf{&ipamV4Conf}
		v6Conf := []*libnetwork.IpamConf{}
		ipamOption = libnetwork.NetworkOptionIpam("default", "", v4Conf, v6Conf, nil)
	}

	_, err := controller.NewNetwork(context.TODO(), network.DefaultNetwork, network.DefaultNetwork, "",
		libnetwork.NetworkOptionGeneric(options.Generic{
			netlabel.GenericData: netOption,
			netlabel.EnableIPv4:  true,
		}),
		ipamOption,
	)
	if err != nil {
		return errors.Wrap(err, "error creating default network")
	}

	return nil
}

// registerLinks sets up links between containers and writes the
// configuration out for persistence. As of Windows TP4, links are not supported.
func (daemon *Daemon) registerLinks(container *container.Container) error {
	return nil
}

func (daemon *Daemon) cleanupMountsByID(in string) error {
	return nil
}

func (daemon *Daemon) cleanupMounts(*config.Config) error {
	return nil
}

func recursiveUnmount(_ string) error {
	return nil
}

func setupRemappedRoot(config *config.Config) (user.IdentityMapping, error) {
	return user.IdentityMapping{}, nil
}

func setupDaemonRoot(config *config.Config, rootDir string, uid, gid int) error {
	config.Root = rootDir
	// Create the root directory if it doesn't exists
	if err := system.MkdirAllWithACL(config.Root, system.SddlAdministratorsLocalSystem); err != nil {
		return err
	}
	return nil
}

// runasHyperVContainer returns true if we are going to run as a Hyper-V container
func (daemon *Daemon) runAsHyperVContainer(hostConfig *containertypes.HostConfig) bool {
	if hostConfig.Isolation.IsDefault() {
		// Container is set to use the default, so take the default from the daemon configuration
		return daemon.defaultIsolation.IsHyperV()
	}

	// Container is requesting an isolation mode. Honour it.
	return hostConfig.Isolation.IsHyperV()
}

// conditionalMountOnStart is a platform specific helper function during the
// container start to call mount.
func (daemon *Daemon) conditionalMountOnStart(container *container.Container) error {
	if daemon.runAsHyperVContainer(container.HostConfig) {
		// We do not mount if a Hyper-V container as it needs to be mounted inside the
		// utility VM, not the host.
		return nil
	}
	return daemon.Mount(container)
}

// conditionalUnmountOnCleanup is a platform specific helper function called
// during the cleanup of a container to unmount.
func (daemon *Daemon) conditionalUnmountOnCleanup(container *container.Container) error {
	if daemon.runAsHyperVContainer(container.HostConfig) {
		// We do not unmount if a Hyper-V container
		return nil
	}
	return daemon.Unmount(container)
}

func networkPlatformOptions(_ *config.Config) []nwconfig.Option {
	return nil
}

// setDefaultIsolation determine the default isolation mode for the
// daemon to run in. This is only applicable on Windows
func (daemon *Daemon) setDefaultIsolation(config *config.Config) error {
	// On client SKUs, default to Hyper-V. @engine maintainers. This
	// should not be removed. Ping Microsoft folks is there are PRs to
	// to change this.
	if operatingsystem.IsWindowsClient() {
		daemon.defaultIsolation = containertypes.IsolationHyperV
	} else {
		daemon.defaultIsolation = containertypes.IsolationProcess
	}
	val, ok, err := config.GetExecOpt("isolation")
	if err != nil {
		return err
	}
	if ok {
		isolation := containertypes.Isolation(strings.ToLower(val))
		if !isolation.IsValid() {
			return fmt.Errorf("invalid exec-opt value for 'isolation':'%s'", val)
		}
		if isolation.IsHyperV() {
			daemon.defaultIsolation = containertypes.IsolationHyperV
		}
		if isolation.IsProcess() {
			daemon.defaultIsolation = containertypes.IsolationProcess
		}
	}

	log.G(context.TODO()).Infof("Windows default isolation mode: %s", daemon.defaultIsolation)
	return nil
}

func (daemon *Daemon) setupSeccompProfile(*config.Config) error {
	return nil
}

func setupResolvConf(config *config.Config) {}

func getSysInfo(*config.Config) *sysinfo.SysInfo {
	return sysinfo.New()
}

func (daemon *Daemon) runInNetNS(f func() error) error {
	return f()
}

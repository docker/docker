package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/cli/debug"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/docker/pkg/parsers/operatingsystem"
	"github.com/docker/docker/pkg/platform"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/registry"
	"github.com/docker/go-connections/sockets"
	"github.com/sirupsen/logrus"
)

// SystemInfo returns information about the host server the daemon is running on.
func (daemon *Daemon) SystemInfo() (*types.Info, error) {
	sysInfo := sysinfo.New(true)
	cRunning, cPaused, cStopped := stateCtr.get()

	v := &types.Info{
		ID:                 daemon.ID,
		Containers:         cRunning + cPaused + cStopped,
		ContainersRunning:  cRunning,
		ContainersPaused:   cPaused,
		ContainersStopped:  cStopped,
		Images:             daemon.imageService.CountImages(),
		IPv4Forwarding:     !sysInfo.IPv4ForwardingDisabled,
		BridgeNfIptables:   !sysInfo.BridgeNFCallIPTablesDisabled,
		BridgeNfIP6tables:  !sysInfo.BridgeNFCallIP6TablesDisabled,
		Debug:              debug.IsEnabled(),
		Name:               hostName(),
		NFd:                fileutils.GetTotalUsedFds(),
		NGoroutines:        runtime.NumGoroutine(),
		SystemTime:         time.Now().Format(time.RFC3339Nano),
		LoggingDriver:      daemon.defaultLogConfig.Type,
		CgroupDriver:       daemon.getCgroupDriver(),
		NEventsListener:    daemon.EventsService.SubscribersCount(),
		KernelVersion:      kernelVersion(),
		OperatingSystem:    operatingSystem(),
		IndexServerAddress: registry.IndexServer,
		OSType:             platform.OSType,
		Architecture:       platform.Architecture,
		RegistryConfig:     daemon.RegistryService.ServiceConfig(),
		NCPU:               sysinfo.NumCPU(),
		MemTotal:           memInfo().MemTotal,
		GenericResources:   daemon.genericResources,
		DockerRootDir:      daemon.configStore.Root,
		Labels:             daemon.configStore.Labels,
		ExperimentalBuild:  daemon.configStore.Experimental,
		ServerVersion:      dockerversion.Version,
		ClusterStore:       daemon.configStore.ClusterStore,
		ClusterAdvertise:   daemon.configStore.ClusterAdvertise,
		HTTPProxy:          maskCredentials(sockets.GetProxyEnv("http_proxy")),
		HTTPSProxy:         maskCredentials(sockets.GetProxyEnv("https_proxy")),
		NoProxy:            sockets.GetProxyEnv("no_proxy"),
		LiveRestoreEnabled: daemon.configStore.LiveRestoreEnabled,
		Isolation:          daemon.defaultIsolation,
	}

	daemon.fillAPIInfo(v)
	// Retrieve platform specific info
	daemon.fillPlatformInfo(v, sysInfo)
	daemon.fillDriverInfo(v)
	daemon.fillPluginsInfo(v)
	daemon.fillSecurityOptions(v, sysInfo)
	daemon.fillLicense(v)

	return v, nil
}

// SystemVersion returns version information about the daemon.
func (daemon *Daemon) SystemVersion() types.Version {
	kernelVersion := kernelVersion()

	v := types.Version{
		Components: []types.ComponentVersion{
			{
				Name:    "Engine",
				Version: dockerversion.Version,
				Details: map[string]string{
					"GitCommit":     dockerversion.GitCommit,
					"ApiVersion":    api.DefaultVersion,
					"MinAPIVersion": api.MinVersion,
					"GoVersion":     runtime.Version(),
					"Os":            runtime.GOOS,
					"Arch":          runtime.GOARCH,
					"BuildTime":     dockerversion.BuildTime,
					"KernelVersion": kernelVersion,
					"Experimental":  fmt.Sprintf("%t", daemon.configStore.Experimental),
				},
			},
		},

		// Populate deprecated fields for older clients
		Version:       dockerversion.Version,
		GitCommit:     dockerversion.GitCommit,
		APIVersion:    api.DefaultVersion,
		MinAPIVersion: api.MinVersion,
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		BuildTime:     dockerversion.BuildTime,
		KernelVersion: kernelVersion,
		Experimental:  daemon.configStore.Experimental,
	}

	v.Platform.Name = dockerversion.PlatformName

	daemon.fillPlatformVersion(&v)
	return v
}

func (daemon *Daemon) fillDriverInfo(v *types.Info) {
	var ds [][2]string
	drivers := ""
	statuses := daemon.imageService.LayerStoreStatus()
	for os, gd := range daemon.graphDrivers {
		ds = append(ds, statuses[os]...)
		drivers += gd
		if len(daemon.graphDrivers) > 1 {
			drivers += fmt.Sprintf(" (%s) ", os)
		}
		switch gd {
		case "aufs", "devicemapper", "overlay":
			v.Warnings = append(v.Warnings, fmt.Sprintf("WARNING: the %s storage-driver is deprecated, and will be removed in a future release.", gd))
		}
	}
	drivers = strings.TrimSpace(drivers)

	v.Driver = drivers
	v.DriverStatus = ds

	fillDriverWarnings(v)
}

func (daemon *Daemon) fillPluginsInfo(v *types.Info) {
	v.Plugins = types.PluginsInfo{
		Volume:  daemon.volumes.GetDriverList(),
		Network: daemon.GetNetworkDriverList(),

		// The authorization plugins are returned in the order they are
		// used as they constitute a request/response modification chain.
		Authorization: daemon.configStore.AuthorizationPlugins,
		Log:           logger.ListDrivers(),
	}
}

func (daemon *Daemon) fillSecurityOptions(v *types.Info, sysInfo *sysinfo.SysInfo) {
	var securityOptions []string
	if sysInfo.AppArmor {
		securityOptions = append(securityOptions, "name=apparmor")
	}
	if sysInfo.Seccomp && supportsSeccomp {
		profile := daemon.seccompProfilePath
		if profile == "" {
			profile = "default"
		}
		securityOptions = append(securityOptions, fmt.Sprintf("name=seccomp,profile=%s", profile))
	}
	if selinuxEnabled() {
		securityOptions = append(securityOptions, "name=selinux")
	}
	if rootIDs := daemon.idMapping.RootPair(); rootIDs.UID != 0 || rootIDs.GID != 0 {
		securityOptions = append(securityOptions, "name=userns")
	}
	if daemon.Rootless() {
		securityOptions = append(securityOptions, "name=rootless")
	}
	if daemon.cgroupNamespacesEnabled(sysInfo) {
		securityOptions = append(securityOptions, "name=cgroupns")
	}

	v.SecurityOptions = securityOptions
}

func (daemon *Daemon) fillAPIInfo(v *types.Info) {
	const warn string = `
         Access to the remote API is equivalent to root access on the host. Refer
         to the 'Docker daemon attack surface' section in the documentation for
         more information: https://docs.docker.com/engine/security/security/#docker-daemon-attack-surface`

	cfg := daemon.configStore
	for _, host := range cfg.Hosts {
		// cnf.Hosts is normalized during startup, so should always have a scheme/proto
		h := strings.SplitN(host, "://", 2)
		proto := h[0]
		addr := h[1]
		if proto != "tcp" {
			continue
		}
		if !cfg.TLS {
			v.Warnings = append(v.Warnings, fmt.Sprintf("WARNING: API is accessible on http://%s without encryption.%s", addr, warn))
			continue
		}
		if !cfg.TLSVerify {
			v.Warnings = append(v.Warnings, fmt.Sprintf("WARNING: API is accessible on https://%s without TLS client verification.%s", addr, warn))
			continue
		}
	}
}

func hostName() string {
	hostname := ""
	if hn, err := os.Hostname(); err != nil {
		logrus.Warnf("Could not get hostname: %v", err)
	} else {
		hostname = hn
	}
	return hostname
}

func kernelVersion() string {
	var kernelVersion string
	if kv, err := kernel.GetKernelVersion(); err != nil {
		logrus.Warnf("Could not get kernel version: %v", err)
	} else {
		kernelVersion = kv.String()
	}
	return kernelVersion
}

func memInfo() *system.MemInfo {
	memInfo, err := system.ReadMemInfo()
	if err != nil {
		logrus.Errorf("Could not read system memory info: %v", err)
		memInfo = &system.MemInfo{}
	}
	return memInfo
}

func operatingSystem() string {
	var operatingSystem string
	if s, err := operatingsystem.GetOperatingSystem(); err != nil {
		logrus.Warnf("Could not get operating system name: %v", err)
	} else {
		operatingSystem = s
	}
	// Don't do containerized check on Windows
	if runtime.GOOS != "windows" {
		if inContainer, err := operatingsystem.IsContainerized(); err != nil {
			logrus.Errorf("Could not determine if daemon is containerized: %v", err)
			operatingSystem += " (error determining if containerized)"
		} else if inContainer {
			operatingSystem += " (containerized)"
		}
	}
	return operatingSystem
}

func maskCredentials(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.User == nil {
		return rawURL
	}
	parsedURL.User = url.UserPassword("xxxxx", "xxxxx")
	maskedURL := parsedURL.String()
	return maskedURL
}

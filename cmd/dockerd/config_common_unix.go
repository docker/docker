// +build linux freebsd

package main

import (
	"path/filepath"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/homedir"
	"github.com/spf13/pflag"
)

func getDefaultPidFile() (string, error) {
	if !honorXDG {
		return "/var/run/docker.pid", nil
	}
	runtimeDir, err := homedir.GetRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "docker.pid"), nil
}

func getDefaultDataRoot() (string, error) {
	if !honorXDG {
		return "/var/lib/docker", nil
	}
	dataHome, err := homedir.GetDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "docker"), nil
}

func getDefaultExecRoot() (string, error) {
	if !honorXDG {
		return "/var/run/docker", nil
	}
	runtimeDir, err := homedir.GetRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "docker"), nil
}

// installUnixConfigFlags adds command-line options to the top-level flag parser for
// the current process that are common across Unix platforms.
func installUnixConfigFlags(conf *config.Config, flags *pflag.FlagSet) {
	conf.Runtimes = make(map[string]types.Runtime)

	flags.StringVarP(&conf.SocketGroup, "group", "G", "docker", "Group for the unix socket")
	flags.StringVar(&conf.BridgeConfig.IP, "bip", "", "Specify network bridge IP")
	flags.StringVarP(&conf.BridgeConfig.Iface, "bridge", "b", "", "Attach containers to a network bridge")
	flags.StringVar(&conf.BridgeConfig.FixedCIDR, "fixed-cidr", "", "IPv4 subnet for fixed IPs")
	flags.Var(opts.NewIPOpt(&conf.BridgeConfig.DefaultGatewayIPv4, ""), "default-gateway", "Container default gateway IPv4 address")
	flags.Var(opts.NewIPOpt(&conf.BridgeConfig.DefaultGatewayIPv6, ""), "default-gateway-v6", "Container default gateway IPv6 address")
	flags.BoolVar(&conf.BridgeConfig.InterContainerCommunication, "icc", true, "Enable inter-container communication")
	flags.Var(opts.NewIPOpt(&conf.BridgeConfig.DefaultIP, "0.0.0.0"), "ip", "Default IP when binding container ports")
	flags.Var(opts.NewNamedRuntimeOpt("runtimes", &conf.Runtimes, config.StockRuntimeName), "add-runtime", "Register an additional OCI compatible runtime")
	flags.StringVar(&conf.DefaultRuntime, "default-runtime", config.StockRuntimeName, "Default OCI runtime for containers")

}

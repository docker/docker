package main

import (
	"runtime"

	"github.com/docker/docker/daemon"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/plugin/executor/containerd"
	"github.com/docker/docker/registry"
	"github.com/spf13/pflag"
)

const (
	// defaultShutdownTimeout is the default shutdown timeout for the daemon
	defaultShutdownTimeout = 15
	// defaultTrustKeyFile is the default filename for the trust key
	defaultTrustKeyFile = "key.json"
)

// installCommonConfigFlags adds flags to the pflag.FlagSet to configure the daemon
func installCommonConfigFlags(conf *config.Config, flags *pflag.FlagSet) error {
	var maxConcurrentDownloads, maxConcurrentUploads, maxDownloadAttempts int
	defaultPidFile, err := getDefaultPidFile()
	if err != nil {
		return err
	}
	defaultDataRoot, err := getDefaultDataRoot()
	if err != nil {
		return err
	}
	defaultExecRoot, err := getDefaultExecRoot()
	if err != nil {
		return err
	}

	installRegistryServiceFlags(&conf.ServiceOptions, flags)

	flags.Var(opts.NewNamedListOptsRef("storage-opts", &conf.GraphOptions, nil), "storage-opt", "Storage driver options")
	flags.Var(opts.NewNamedListOptsRef("authorization-plugins", &conf.AuthorizationPlugins, nil), "authorization-plugin", "Authorization plugins to load")
	flags.Var(opts.NewNamedListOptsRef("exec-opts", &conf.ExecOptions, nil), "exec-opt", "Runtime execution options")
	flags.StringVarP(&conf.Pidfile, "pidfile", "p", defaultPidFile, "Path to use for daemon PID file")
	flags.StringVarP(&conf.Root, "graph", "g", defaultDataRoot, "Root of the Docker runtime")
	flags.StringVar(&conf.ExecRoot, "exec-root", defaultExecRoot, "Root directory for execution state files")
	flags.StringVar(&conf.ContainerdAddr, "containerd", "", "containerd grpc address")
	flags.BoolVar(&conf.CriContainerd, "cri-containerd", false, "start containerd with cri")

	// "--graph" is "soft-deprecated" in favor of "data-root". This flag was added
	// before Docker 1.0, so won't be removed, only hidden, to discourage its usage.
	_ = flags.MarkHidden("graph")

	flags.StringVar(&conf.Root, "data-root", defaultDataRoot, "Root directory of persistent Docker state")

	flags.BoolVarP(&conf.AutoRestart, "restart", "r", true, "--restart on the daemon has been deprecated in favor of --restart policies on docker run")
	_ = flags.MarkDeprecated("restart", "Please use a restart policy on docker run")

	// Windows doesn't support setting the storage driver - there is no choice as to which ones to use.
	if runtime.GOOS != "windows" {
		flags.StringVarP(&conf.GraphDriver, "storage-driver", "s", "", "Storage driver to use")
	}

	flags.IntVar(&conf.Mtu, "mtu", 0, "Set the containers network MTU")
	flags.BoolVar(&conf.RawLogs, "raw-logs", false, "Full timestamps without ANSI coloring")
	flags.Var(opts.NewListOptsRef(&conf.DNS, opts.ValidateIPAddress), "dns", "DNS server to use")
	flags.Var(opts.NewNamedListOptsRef("dns-opts", &conf.DNSOptions, nil), "dns-opt", "DNS options to use")
	flags.Var(opts.NewListOptsRef(&conf.DNSSearch, opts.ValidateDNSSearch), "dns-search", "DNS search domains to use")
	flags.Var(opts.NewIPOpt(&conf.HostGatewayIP, ""), "host-gateway-ip", "IP address that the special 'host-gateway' string in --add-host resolves to. Defaults to the IP address of the default bridge")
	flags.Var(opts.NewNamedListOptsRef("labels", &conf.Labels, opts.ValidateLabel), "label", "Set key=value labels to the daemon")
	flags.StringVar(&conf.LogConfig.Type, "log-driver", "json-file", "Default driver for container logs")
	flags.Var(opts.NewNamedMapOpts("log-opts", conf.LogConfig.Config, nil), "log-opt", "Default log driver options for containers")

	flags.StringVar(&conf.ClusterAdvertise, "cluster-advertise", "", "Address or interface name to advertise")
	_ = flags.MarkDeprecated("cluster-advertise", "Swarm classic is deprecated. Please use Swarm-mode (docker swarm init)")
	flags.StringVar(&conf.ClusterStore, "cluster-store", "", "URL of the distributed storage backend")
	_ = flags.MarkDeprecated("cluster-store", "Swarm classic is deprecated. Please use Swarm-mode (docker swarm init)")
	flags.Var(opts.NewNamedMapOpts("cluster-store-opts", conf.ClusterOpts, nil), "cluster-store-opt", "Set cluster store options")
	_ = flags.MarkDeprecated("cluster-store-opt", "Swarm classic is deprecated. Please use Swarm-mode (docker swarm init)")

	flags.StringVar(&conf.CorsHeaders, "api-cors-header", "", "Set CORS headers in the Engine API")
	flags.IntVar(&maxConcurrentDownloads, "max-concurrent-downloads", config.DefaultMaxConcurrentDownloads, "Set the max concurrent downloads for each pull")
	flags.IntVar(&maxConcurrentUploads, "max-concurrent-uploads", config.DefaultMaxConcurrentUploads, "Set the max concurrent uploads for each push")
	flags.IntVar(&maxDownloadAttempts, "max-download-attempts", config.DefaultDownloadAttempts, "Set the max download attempts for each pull")
	flags.IntVar(&conf.ShutdownTimeout, "shutdown-timeout", defaultShutdownTimeout, "Set the default shutdown timeout")
	flags.IntVar(&conf.NetworkDiagnosticPort, "network-diagnostic-port", 0, "TCP port number of the network diagnostic server")
	_ = flags.MarkHidden("network-diagnostic-port")

	flags.StringVar(&conf.SwarmDefaultAdvertiseAddr, "swarm-default-advertise-addr", "", "Set default address or interface for swarm advertised address")
	flags.BoolVar(&conf.Experimental, "experimental", false, "Enable experimental features")
	flags.StringVar(&conf.MetricsAddress, "metrics-addr", "", "Set default address and port to serve the metrics api on")

	flags.Var(opts.NewNamedListOptsRef("node-generic-resources", &conf.NodeGenericResources, opts.ValidateSingleGenericResource), "node-generic-resource", "Advertise user-defined resource")

	flags.IntVar(&conf.NetworkControlPlaneMTU, "network-control-plane-mtu", config.DefaultNetworkMtu, "Network Control plane MTU")

	conf.MaxConcurrentDownloads = &maxConcurrentDownloads
	conf.MaxConcurrentUploads = &maxConcurrentUploads
	conf.MaxDownloadAttempts = &maxDownloadAttempts

	flags.StringVar(&conf.ContainerdNamespace, "containerd-namespace", daemon.ContainersNamespace, "Containerd namespace to use")
	flags.StringVar(&conf.ContainerdPluginNamespace, "containerd-plugins-namespace", containerd.PluginNamespace, "Containerd namespace to use for plugins")

	return nil
}

func installRegistryServiceFlags(options *registry.ServiceOptions, flags *pflag.FlagSet) {
	ana := opts.NewNamedListOptsRef("allow-nondistributable-artifacts", &options.AllowNondistributableArtifacts, registry.ValidateIndexName)
	mirrors := opts.NewNamedListOptsRef("registry-mirrors", &options.Mirrors, registry.ValidateMirror)
	insecureRegistries := opts.NewNamedListOptsRef("insecure-registries", &options.InsecureRegistries, registry.ValidateIndexName)

	flags.Var(ana, "allow-nondistributable-artifacts", "Allow push of nondistributable artifacts to registry")
	flags.Var(mirrors, "registry-mirror", "Preferred Docker registry mirror")
	flags.Var(insecureRegistries, "insecure-registry", "Enable insecure registry communication")
}

package command

import (
	"fmt"
	"runtime"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/config"
	dopts "github.com/moby/moby/v2/daemon/internal/opts"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/spf13/pflag"
)

// installCommonConfigFlags adds flags to the pflag.FlagSet to configure the daemon
func installCommonConfigFlags(conf *config.Config, flags *pflag.FlagSet) {
	var (
		registryMirrors    = opts.NewNamedListOptsRef("registry-mirrors", &conf.Mirrors, registry.ValidateMirror)
		insecureRegistries = opts.NewNamedListOptsRef("insecure-registries", &conf.InsecureRegistries, registry.ValidateIndexName)
	)
	flags.Var(registryMirrors, "registry-mirror", "Preferred Docker registry mirror")
	flags.Var(insecureRegistries, "insecure-registry", "Enable insecure registry communication")

	flags.Var(opts.NewNamedListOptsRef("storage-opts", &conf.GraphOptions, nil), "storage-opt", "Storage driver options")
	flags.Var(opts.NewNamedListOptsRef("authorization-plugins", &conf.AuthorizationPlugins, nil), "authorization-plugin", "Authorization plugins to load")
	flags.Var(opts.NewNamedListOptsRef("extensions.dirs", &conf.Extensions.Dirs, nil), "extension-dir", "Directories to load extension binaries from")
	flags.Var(opts.NewNamedListOptsRef("extensions.enable", &conf.Extensions.Enable, nil), "extension-enable", "Built-in extensions to enable, by extension ID")
	flags.Var(opts.NewNamedListOptsRef("extensions.disable", &conf.Extensions.Disable, nil), "extension-disable", "Built-in extensions to disable, by extension ID")
	flags.Var(opts.NewNamedListOptsRef("exec-opts", &conf.ExecOptions, nil), "exec-opt", "Runtime execution options")
	flags.StringVarP(&conf.Pidfile, "pidfile", "p", conf.Pidfile, "Path to use for daemon PID file")
	flags.StringVar(&conf.Root, "data-root", conf.Root, "Root directory of persistent Docker state")
	flags.StringVar(&conf.ExecRoot, "exec-root", conf.ExecRoot, "Root directory for execution state files")
	flags.StringVar(&conf.ContainerdAddr, "containerd", "", "containerd grpc address")
	flags.BoolVar(&conf.CriContainerd, "cri-containerd", false, "start containerd with cri")
	flags.Var(dopts.NewNamedSetOpts("features", conf.Features), "feature", "Enable feature in the daemon")

	flags.Var(opts.NewNamedMapMapOpts("default-network-opts", conf.DefaultNetworkOpts, nil), "default-network-opt", "Default network options")
	flags.IntVar(&conf.MTU, "mtu", conf.MTU, `Set the MTU for the default "bridge" network`)
	if runtime.GOOS == "windows" {
		// The mtu option is not used on Windows, but it has been available since
		// "forever" (and always silently ignored). We hide the flag for now,
		// to discourage using it (and print a warning if it's set), but not
		// "hard-deprecating" it, to not break users, and in case it will be
		// supported on Windows in future.
		flags.MarkHidden("mtu")
	}

	flags.IntVar(&conf.NetworkControlPlaneMTU, "network-control-plane-mtu", conf.NetworkControlPlaneMTU, "Network Control plane MTU")
	flags.IntVar(&conf.NetworkDiagnosticPort, "network-diagnostic-port", 0, "TCP port number of the network diagnostic server")
	_ = flags.MarkHidden("network-diagnostic-port")

	// Daemon log config
	flags.BoolVar(&conf.DaemonLogConfig.RawLogs, "raw-logs", conf.DaemonLogConfig.RawLogs, "Full timestamps without ANSI coloring")
	flags.StringVarP(&conf.DaemonLogConfig.LogLevel, "log-level", "l", conf.DaemonLogConfig.LogLevel, `Set the logging level ("debug"|"info"|"warn"|"error"|"fatal")`)
	flags.Var(&stringVar[log.OutputFormat]{val: &conf.DaemonLogConfig.LogFormat}, "log-format", fmt.Sprintf(`Set the logging format (%q|%q)`, log.TextFormat, log.JSONFormat))

	flags.Var(dopts.NewNamedIPListOptsRef("dns", &conf.DNS), "dns", "DNS server to use")
	flags.Var(opts.NewNamedListOptsRef("dns-opts", &conf.DNSOptions, nil), "dns-opt", "DNS options to use")
	flags.Var(opts.NewListOptsRef(&conf.DNSSearch, opts.ValidateDNSSearch), "dns-search", "DNS search domains to use")
	flags.Var(dopts.NewNamedIPListOptsRef("host-gateway-ips", &conf.HostGatewayIPs), "host-gateway-ip", "IP addresses that the special 'host-gateway' string in --add-host resolves to. Defaults to the IP addresses of the default bridge")
	flags.Var(opts.NewNamedListOptsRef("labels", &conf.Labels, opts.ValidateLabel), "label", "Set key=value labels to the daemon")
	flags.StringVar(&conf.LogConfig.Type, "log-driver", conf.LogConfig.Type, "Default driver for container logs")
	flags.Var(opts.NewNamedMapOpts("log-opts", conf.LogConfig.Config, nil), "log-opt", "Default log driver options for containers")

	flags.IntVar(&conf.MaxConcurrentDownloads, "max-concurrent-downloads", conf.MaxConcurrentDownloads, "Set the max concurrent downloads")
	flags.IntVar(&conf.MaxConcurrentUploads, "max-concurrent-uploads", conf.MaxConcurrentUploads, "Set the max concurrent uploads")
	flags.IntVar(&conf.MaxDownloadAttempts, "max-download-attempts", conf.MaxDownloadAttempts, "Set the max download attempts for each pull")
	flags.IntVar(&conf.ShutdownTimeout, "shutdown-timeout", conf.ShutdownTimeout, "Set the default shutdown timeout")
	flags.IntVar(&conf.DefaultStopTimeout, "default-stop-timeout", conf.DefaultStopTimeout, "Set the default container stop timeout in seconds (0 to terminate immediately)")
	flags.StringVar(&conf.DefaultContainerProfile, "default-container-profile", conf.DefaultContainerProfile, "Default container profile")

	flags.StringVar(&conf.SwarmDefaultAdvertiseAddr, "swarm-default-advertise-addr", "", "Set default address or interface for swarm advertised address")
	flags.BoolVar(&conf.Experimental, "experimental", false, "Enable experimental features")
	flags.StringVar(&conf.MetricsAddress, "metrics-addr", "", "Set default address and port to serve the metrics api on")
	flags.Var(opts.NewNamedListOptsRef("node-generic-resources", &conf.NodeGenericResources, opts.ValidateSingleGenericResource), "node-generic-resource", "Advertise user-defined resource")

	flags.StringVar(&conf.ContainerdNamespace, "containerd-namespace", conf.ContainerdNamespace, "Containerd namespace to use")
	flags.StringVar(&conf.ContainerdPluginNamespace, "containerd-plugins-namespace", conf.ContainerdPluginNamespace, "Containerd namespace to use for plugins")
	flags.StringVar(&conf.DefaultRuntime, "default-runtime", conf.DefaultRuntime, "Default OCI runtime for containers")

	flags.StringVar(&conf.HTTPProxy, "http-proxy", "", "HTTP proxy URL to use for outgoing traffic")
	flags.StringVar(&conf.HTTPSProxy, "https-proxy", "", "HTTPS proxy URL to use for outgoing traffic")
	flags.StringVar(&conf.NoProxy, "no-proxy", "", "Comma-separated list of hosts or IP addresses for which the proxy is skipped")

	flags.Var(opts.NewNamedListOptsRef("cdi-spec-dirs", &conf.CDISpecDirs, nil), "cdi-spec-dir", "CDI specification directories to use")

	flags.Var(opts.NewNamedNRIOptsRef(&conf.NRIOpts), "nri-opts", "Node Resource Interface configuration")

	// Deprecated flags / options
	flags.BoolVarP(&conf.AutoRestart, "restart", "r", true, "--restart on the daemon has been deprecated in favor of --restart policies on docker run")
	_ = flags.MarkDeprecated("restart", "Please use a restart policy on docker run")
}

// stringVar is a bare-bones implementation of a [pflag.Value] using
// generics to create flags for typed string values / enums.
type stringVar[T ~string] struct{ val *T }

func (v *stringVar[T]) Set(s string) error {
	*v.val = T(s)
	return nil
}

func (v *stringVar[T]) String() string {
	return string(*v.val)
}

func (v *stringVar[T]) Type() string { return "string" }

var _ pflag.Value = (*stringVar[string])(nil)

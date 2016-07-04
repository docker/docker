package service

import (
	"encoding/csv"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/opts"
	runconfigopts "github.com/docker/docker/runconfig/opts"
	"github.com/docker/engine-api/types/swarm"
	"github.com/docker/go-connections/nat"
	units "github.com/docker/go-units"
	"github.com/spf13/cobra"
)

var (
	// DefaultReplicas is the default replicas to use for a replicated service
	DefaultReplicas uint64 = 1
)

type int64Value interface {
	Value() int64
}

type memBytes int64

func (m *memBytes) String() string {
	return units.BytesSize(float64(m.Value()))
}

func (m *memBytes) Set(value string) error {
	val, err := units.RAMInBytes(value)
	*m = memBytes(val)
	return err
}

func (m *memBytes) Type() string {
	return "MemoryBytes"
}

func (m *memBytes) Value() int64 {
	return int64(*m)
}

type nanoCPUs int64

func (c *nanoCPUs) String() string {
	return big.NewRat(c.Value(), 1e9).FloatString(3)
}

func (c *nanoCPUs) Set(value string) error {
	cpu, ok := new(big.Rat).SetString(value)
	if !ok {
		return fmt.Errorf("Failed to parse %v as a rational number", value)
	}
	nano := cpu.Mul(cpu, big.NewRat(1e9, 1))
	if !nano.IsInt() {
		return fmt.Errorf("value is too precise")
	}
	*c = nanoCPUs(nano.Num().Int64())
	return nil
}

func (c *nanoCPUs) Type() string {
	return "NanoCPUs"
}

func (c *nanoCPUs) Value() int64 {
	return int64(*c)
}

// DurationOpt is an option type for time.Duration that uses a pointer. This
// allows us to get nil values outside, instead of defaulting to 0
type DurationOpt struct {
	value *time.Duration
}

// Set a new value on the option
func (d *DurationOpt) Set(s string) error {
	v, err := time.ParseDuration(s)
	d.value = &v
	return err
}

// Type returns the type of this option
func (d *DurationOpt) Type() string {
	return "duration-ptr"
}

// String returns a string repr of this option
func (d *DurationOpt) String() string {
	if d.value != nil {
		return d.value.String()
	}
	return "none"
}

// Value returns the time.Duration
func (d *DurationOpt) Value() *time.Duration {
	return d.value
}

// Uint64Opt represents a uint64.
type Uint64Opt struct {
	value *uint64
}

// Set a new value on the option
func (i *Uint64Opt) Set(s string) error {
	v, err := strconv.ParseUint(s, 0, 64)
	i.value = &v
	return err
}

// Type returns the type of this option
func (i *Uint64Opt) Type() string {
	return "uint64-ptr"
}

// String returns a string repr of this option
func (i *Uint64Opt) String() string {
	if i.value != nil {
		return fmt.Sprintf("%v", *i.value)
	}
	return "none"
}

// Value returns the uint64
func (i *Uint64Opt) Value() *uint64 {
	return i.value
}

// MountOpt is a Value type for parsing mounts
type MountOpt struct {
	values []swarm.Mount
}

// Set a new mount value
func (m *MountOpt) Set(value string) error {
	csvReader := csv.NewReader(strings.NewReader(value))
	fields, err := csvReader.Read()
	if err != nil {
		return err
	}

	mount := swarm.Mount{}

	mount.Type, fields = getMountType(fields)
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)

		if len(parts) == 1 {
			if err := m.setKeylessValue(&mount, field); err != nil {
				return err
			}
			continue
		}

		key, value := parts[0], parts[1]
		ok, err := m.setFieldValue(&mount, key, value)
		if err != nil {
			return err
		}
		if ok {
			continue
		}

		switch mount.Type {
		case swarm.MountType("BIND"):
			ok, err = m.setBindValue(&mount, key, value)
		case swarm.MountType("VOLUME"):
			ok, err = m.setVolumeValue(&mount, key, value)
		}
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		return fmt.Errorf("unexpected key '%s' in '%s'", key, field)
	}

	if mount.Type == "" {
		return fmt.Errorf("type is required")
	}

	if mount.Target == "" {
		return fmt.Errorf("target is required")
	}

	if mount.VolumeOptions != nil && mount.Source == "" {
		return fmt.Errorf("name is required when specifying volume-* options")
	}

	m.values = append(m.values, mount)
	return nil
}

func (m *MountOpt) setKeylessValue(mount *swarm.Mount, field string) error {
	switch strings.ToLower(field) {
	case "readonly":
		mount.ReadOnly = true
	case "volume-nocopy":
		defaultVolumeOptions(mount).NoCopy = true
	case "bind", "volume":
		return fmt.Errorf("mount type %q must be the first field", field)
	default:
		return fmt.Errorf("invalid field %q must be a key=value pair", field)
	}
	return nil
}

func defaultVolumeOptions(mount *swarm.Mount) *swarm.VolumeOptions {
	if mount.VolumeOptions == nil {
		mount.VolumeOptions = &swarm.VolumeOptions{
			Labels: make(map[string]string),
		}
	}
	if mount.VolumeOptions.DriverConfig == nil {
		mount.VolumeOptions.DriverConfig = &swarm.Driver{}
	}
	return mount.VolumeOptions
}

func (m *MountOpt) setFieldValue(mount *swarm.Mount, key, value string) (bool, error) {
	switch strings.ToLower(key) {
	case "target", "dst", "path":
		mount.Target = value
	case "readonly":
		var err error
		mount.ReadOnly, err = strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("invalid value for readonly: %s", value)
		}
	default:
		return false, nil
	}
	return true, nil
}

func (m *MountOpt) setBindValue(mount *swarm.Mount, key, value string) (bool, error) {
	bindOptions := func() *swarm.BindOptions {
		if mount.BindOptions == nil {
			mount.BindOptions = new(swarm.BindOptions)
		}
		return mount.BindOptions
	}

	switch strings.ToLower(key) {
	case "source", "src":
		mount.Source = value
	case "propagation":
		bindOptions().Propagation = swarm.MountPropagation(strings.ToUpper(value))
	default:
		return false, nil
	}
	return true, nil
}

func (m *MountOpt) setVolumeValue(mount *swarm.Mount, key, value string) (bool, error) {
	setValueOnMap := func(target map[string]string, value string) {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 1 {
			target[value] = ""
		} else {
			target[parts[0]] = parts[1]
		}
	}

	var err error
	switch strings.ToLower(key) {
	case "name":
		mount.Source = value
	case "volume-label":
		setValueOnMap(defaultVolumeOptions(mount).Labels, value)
	case "volume-driver":
		defaultVolumeOptions(mount).DriverConfig.Name = value
	case "volume-opt":
		if defaultVolumeOptions(mount).DriverConfig.Options == nil {
			defaultVolumeOptions(mount).DriverConfig.Options = make(map[string]string)
		}
		setValueOnMap(defaultVolumeOptions(mount).DriverConfig.Options, value)
	case "volume-nocopy":
		defaultVolumeOptions(mount).NoCopy, err = strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("invalid value for populate: %s", value)
		}
	default:
		return false, nil
	}

	return true, nil
}

// Type returns the type of this option
func (m *MountOpt) Type() string {
	return "mount"
}

// String returns a string repr of this option
func (m *MountOpt) String() string {
	mounts := []string{}
	for _, mount := range m.values {
		repr := fmt.Sprintf("%s %s %s", mount.Type, mount.Source, mount.Target)
		mounts = append(mounts, repr)
	}
	return strings.Join(mounts, ", ")
}

// Value returns the mounts
func (m *MountOpt) Value() []swarm.Mount {
	return m.values
}

func getMountType(fields []string) (swarm.MountType, []string) {
	if len(fields) == 0 {
		return swarm.MountType("VOLUME"), fields
	}

	switch strings.ToLower(fields[0]) {
	case "bind":
		return swarm.MountType("BIND"), fields[1:]
	case "volume":
		return swarm.MountType("VOLUME"), fields[1:]
	default:
		return swarm.MountType("VOLUME"), fields
	}
}

type updateOptions struct {
	parallelism uint64
	delay       time.Duration
}

type resourceOptions struct {
	limitCPU      nanoCPUs
	limitMemBytes memBytes
	resCPU        nanoCPUs
	resMemBytes   memBytes
}

func (r *resourceOptions) ToResourceRequirements() *swarm.ResourceRequirements {
	return &swarm.ResourceRequirements{
		Limits: &swarm.Resources{
			NanoCPUs:    r.limitCPU.Value(),
			MemoryBytes: r.limitMemBytes.Value(),
		},
		Reservations: &swarm.Resources{
			NanoCPUs:    r.resCPU.Value(),
			MemoryBytes: r.resMemBytes.Value(),
		},
	}
}

type restartPolicyOptions struct {
	condition   string
	delay       DurationOpt
	maxAttempts Uint64Opt
	window      DurationOpt
}

func (r *restartPolicyOptions) ToRestartPolicy() *swarm.RestartPolicy {
	return &swarm.RestartPolicy{
		Condition:   swarm.RestartPolicyCondition(r.condition),
		Delay:       r.delay.Value(),
		MaxAttempts: r.maxAttempts.Value(),
		Window:      r.window.Value(),
	}
}

func convertNetworks(networks []string) []swarm.NetworkAttachmentConfig {
	nets := []swarm.NetworkAttachmentConfig{}
	for _, network := range networks {
		nets = append(nets, swarm.NetworkAttachmentConfig{Target: network})
	}
	return nets
}

type endpointOptions struct {
	mode  string
	ports opts.ListOpts
}

func (e *endpointOptions) ToEndpointSpec() *swarm.EndpointSpec {
	portConfigs := []swarm.PortConfig{}
	// We can ignore errors because the format was already validated by ValidatePort
	ports, portBindings, _ := nat.ParsePortSpecs(e.ports.GetAll())

	for port := range ports {
		portConfigs = append(portConfigs, convertPortToPortConfig(port, portBindings)...)
	}

	return &swarm.EndpointSpec{
		Mode:  swarm.ResolutionMode(strings.ToLower(e.mode)),
		Ports: portConfigs,
	}
}

func convertPortToPortConfig(
	port nat.Port,
	portBindings map[nat.Port][]nat.PortBinding,
) []swarm.PortConfig {
	ports := []swarm.PortConfig{}

	for _, binding := range portBindings[port] {
		hostPort, _ := strconv.ParseUint(binding.HostPort, 10, 16)
		ports = append(ports, swarm.PortConfig{
			//TODO Name: ?
			Protocol:      swarm.PortConfigProtocol(strings.ToLower(port.Proto())),
			TargetPort:    uint32(port.Int()),
			PublishedPort: uint32(hostPort),
		})
	}
	return ports
}

// ValidatePort validates a string is in the expected format for a port definition
func ValidatePort(value string) (string, error) {
	portMappings, err := nat.ParsePortSpec(value)
	for _, portMapping := range portMappings {
		if portMapping.Binding.HostIP != "" {
			return "", fmt.Errorf("HostIP is not supported by a service.")
		}
	}
	return value, err
}

type serviceOptions struct {
	name    string
	labels  opts.ListOpts
	image   string
	args    []string
	env     opts.ListOpts
	workdir string
	user    string
	mounts  MountOpt

	resources resourceOptions
	stopGrace DurationOpt

	replicas Uint64Opt
	mode     string

	restartPolicy restartPolicyOptions
	constraints   []string
	update        updateOptions
	networks      []string
	endpoint      endpointOptions

	registryAuth bool
}

func newServiceOptions() *serviceOptions {
	return &serviceOptions{
		labels: opts.NewListOpts(runconfigopts.ValidateEnv),
		env:    opts.NewListOpts(runconfigopts.ValidateEnv),
		endpoint: endpointOptions{
			ports: opts.NewListOpts(ValidatePort),
		},
	}
}

func (opts *serviceOptions) ToService() (swarm.ServiceSpec, error) {
	var service swarm.ServiceSpec

	service = swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   opts.name,
			Labels: runconfigopts.ConvertKVStringsToMap(opts.labels.GetAll()),
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: swarm.ContainerSpec{
				Image:           opts.image,
				Args:            opts.args,
				Env:             opts.env.GetAll(),
				Dir:             opts.workdir,
				User:            opts.user,
				Mounts:          opts.mounts.Value(),
				StopGracePeriod: opts.stopGrace.Value(),
			},
			Resources:     opts.resources.ToResourceRequirements(),
			RestartPolicy: opts.restartPolicy.ToRestartPolicy(),
			Placement: &swarm.Placement{
				Constraints: opts.constraints,
			},
		},
		Mode: swarm.ServiceMode{},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism: opts.update.parallelism,
			Delay:       opts.update.delay,
		},
		Networks:     convertNetworks(opts.networks),
		EndpointSpec: opts.endpoint.ToEndpointSpec(),
	}

	switch opts.mode {
	case "global":
		if opts.replicas.Value() != nil {
			return service, fmt.Errorf("replicas can only be used with replicated mode")
		}

		service.Mode.Global = &swarm.GlobalService{}
	case "replicated":
		service.Mode.Replicated = &swarm.ReplicatedService{
			Replicas: opts.replicas.Value(),
		}
	default:
		return service, fmt.Errorf("Unknown mode: %s", opts.mode)
	}
	return service, nil
}

// addServiceFlags adds all flags that are common to both `create` and `update`.
// Any flags that are not common are added separately in the individual command
func addServiceFlags(cmd *cobra.Command, opts *serviceOptions) {
	flags := cmd.Flags()
	flags.StringVar(&opts.name, flagName, "", "Service name")

	flags.StringVarP(&opts.workdir, "workdir", "w", "", "Working directory inside the container")
	flags.StringVarP(&opts.user, flagUser, "u", "", "Username or UID")

	flags.Var(&opts.resources.limitCPU, flagLimitCPU, "Limit CPUs")
	flags.Var(&opts.resources.limitMemBytes, flagLimitMemory, "Limit Memory")
	flags.Var(&opts.resources.resCPU, flagReserveCPU, "Reserve CPUs")
	flags.Var(&opts.resources.resMemBytes, flagReserveMemory, "Reserve Memory")
	flags.Var(&opts.stopGrace, flagStopGracePeriod, "Time to wait before force killing a container")

	flags.Var(&opts.replicas, flagReplicas, "Number of tasks")

	flags.StringVar(&opts.restartPolicy.condition, flagRestartCondition, "", "Restart when condition is met (none, on-failure, or any)")
	flags.Var(&opts.restartPolicy.delay, flagRestartDelay, "Delay between restart attempts")
	flags.Var(&opts.restartPolicy.maxAttempts, flagRestartMaxAttempts, "Maximum number of restarts before giving up")
	flags.Var(&opts.restartPolicy.window, flagRestartWindow, "Window used to evaluate the restart policy")

	flags.Uint64Var(&opts.update.parallelism, flagUpdateParallelism, 0, "Maximum number of tasks updated simultaneously")
	flags.DurationVar(&opts.update.delay, flagUpdateDelay, time.Duration(0), "Delay between updates")

	flags.StringVar(&opts.endpoint.mode, flagEndpointMode, "", "Endpoint mode (vip or dnsrr)")

	flags.BoolVar(&opts.registryAuth, flagRegistryAuth, false, "Send registry authentication details to Swarm agents")
}

const (
	flagConstraint         = "constraint"
	flagConstraintRemove   = "constraint-rm"
	flagConstraintAdd      = "constraint-add"
	flagEndpointMode       = "endpoint-mode"
	flagEnv                = "env"
	flagEnvRemove          = "env-rm"
	flagEnvAdd             = "env-add"
	flagLabel              = "label"
	flagLabelRemove        = "label-rm"
	flagLabelAdd           = "label-add"
	flagLimitCPU           = "limit-cpu"
	flagLimitMemory        = "limit-memory"
	flagMode               = "mode"
	flagMount              = "mount"
	flagMountRemove        = "mount-rm"
	flagMountAdd           = "mount-add"
	flagName               = "name"
	flagNetwork            = "network"
	flagNetworkRemove      = "network-rm"
	flagNetworkAdd         = "network-add"
	flagPublish            = "publish"
	flagPublishRemove      = "publish-rm"
	flagPublishAdd         = "publish-add"
	flagReplicas           = "replicas"
	flagReserveCPU         = "reserve-cpu"
	flagReserveMemory      = "reserve-memory"
	flagRestartCondition   = "restart-condition"
	flagRestartDelay       = "restart-delay"
	flagRestartMaxAttempts = "restart-max-attempts"
	flagRestartWindow      = "restart-window"
	flagStopGracePeriod    = "stop-grace-period"
	flagUpdateDelay        = "update-delay"
	flagUpdateParallelism  = "update-parallelism"
	flagUser               = "user"
	flagRegistryAuth       = "registry-auth"
)

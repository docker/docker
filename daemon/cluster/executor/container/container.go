package container

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/distribution/reference"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	enginemount "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/v2/daemon/cluster/convert"
	executorpkg "github.com/moby/moby/v2/daemon/cluster/executor"
	clustertypes "github.com/moby/moby/v2/daemon/cluster/provider"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/internal/sliceutil"
	"github.com/moby/swarmkit/v2/agent/exec"
	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/api/genericresource"
	"github.com/moby/swarmkit/v2/template"
)

const (
	// systemLabelPrefix represents the reserved namespace for system labels.
	systemLabelPrefix = "com.docker.swarm"
)

// containerConfig converts task properties into docker container compatible
// components.
type containerConfig struct {
	task     *api.Task
	networks map[string]*api.Network
}

// newContainerConfig returns a validated container config. No methods should
// return an error if this function returns without error.
func newContainerConfig(t *api.Task, node *api.NodeDescription) (*containerConfig, error) {
	var c containerConfig
	return &c, c.setTask(t, node)
}

func (c *containerConfig) setTask(t *api.Task, node *api.NodeDescription) error {
	if t.Spec.GetContainer() == nil && t.Spec.GetAttachment() == nil {
		return exec.ErrRuntimeUnsupported
	}

	ctr := t.Spec.GetContainer()
	if ctr != nil {
		if ctr.Image == "" {
			return ErrImageRequired
		}

		if err := validateMounts(ctr.Mounts); err != nil {
			return err
		}
	}

	// index the networks by name
	c.networks = make(map[string]*api.Network, len(t.Networks))
	for _, attachment := range t.Networks {
		// It looks like using a map is only for convenience, but not used
		// for validation, nor for looking up the network by name. The name
		// is part of the Network's properties (Network.Spec.Annotations.Name),
		// and effectively only used for debugging; we should consider to
		// change it to a slice.
		//
		// TODO(thaJeztah): should this check for empty and duplicate names?
		c.networks[attachment.Network.Spec.Annotations.Name] = attachment.Network
	}

	c.task = t

	if t.Spec.GetContainer() != nil {
		preparedSpec, err := template.ExpandContainerSpec(node, t)
		if err != nil {
			return err
		}
		c.task.Spec.Runtime = &api.TaskSpec_Container{
			Container: preparedSpec,
		}
	}

	return nil
}

func (c *containerConfig) networkAttachmentContainerID() string {
	attachment := c.task.Spec.GetAttachment()
	if attachment == nil {
		return ""
	}

	return attachment.ContainerID
}

func (c *containerConfig) taskID() string {
	return c.task.ID
}

func (c *containerConfig) spec() *api.ContainerSpec {
	return c.task.Spec.GetContainer()
}

func (c *containerConfig) nameOrID() string {
	if c.task.Spec.GetContainer() != nil {
		return c.name()
	}

	return c.networkAttachmentContainerID()
}

func (c *containerConfig) name() string {
	if c.task.Annotations.Name != "" {
		// if set, use the container Annotations.Name field, set in the orchestrator.
		return c.task.Annotations.Name
	}

	slot := strconv.FormatUint(c.task.Slot, 10)
	if slot == "" || c.task.Slot == 0 {
		slot = c.task.NodeID
	}

	// fallback to service.slot.id.
	return fmt.Sprintf("%s.%s.%s", c.task.ServiceAnnotations.Name, slot, c.task.ID)
}

func (c *containerConfig) image() string {
	raw := c.spec().Image
	ref, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return raw
	}
	return reference.FamiliarString(reference.TagNameOnly(ref))
}

func (c *containerConfig) portBindings() network.PortMap {
	portBindings := network.PortMap{}
	if c.task.Endpoint == nil {
		return portBindings
	}

	for _, portConfig := range c.task.Endpoint.Ports {
		if portConfig.PublishMode != api.PublishModeHost {
			continue
		}

		if portConfig.TargetPort > math.MaxUint16 {
			continue
		}

		port, ok := network.PortFrom(uint16(portConfig.TargetPort), network.IPProtocol(portConfig.Protocol.String()))
		if !ok {
			continue
		}

		binding := []network.PortBinding{
			{},
		}

		if portConfig.PublishedPort != 0 {
			binding[0].HostPort = strconv.Itoa(int(portConfig.PublishedPort))
		}
		portBindings[port] = binding
	}

	return portBindings
}

func (c *containerConfig) isolation() container.Isolation {
	return convert.IsolationFromGRPC(c.spec().Isolation)
}

func (c *containerConfig) init() *bool {
	if c.spec().Init == nil {
		return nil
	}
	init := c.spec().Init.GetValue()
	return &init
}

func (c *containerConfig) exposedPorts() map[network.Port]struct{} {
	exposedPorts := make(map[network.Port]struct{})
	if c.task.Endpoint == nil {
		return exposedPorts
	}

	for _, portConfig := range c.task.Endpoint.Ports {
		if portConfig.PublishMode != api.PublishModeHost {
			continue
		}

		if portConfig.TargetPort > math.MaxUint16 {
			continue
		}

		port, ok := network.PortFrom(uint16(portConfig.TargetPort), network.IPProtocol(portConfig.Protocol.String()))
		if !ok {
			continue
		}

		exposedPorts[port] = struct{}{}
	}

	return exposedPorts
}

func (c *containerConfig) config() *container.Config {
	genericEnvs := genericresource.EnvFormat(c.task.AssignedGenericResources, "DOCKER_RESOURCE")
	env := append(c.spec().Env, genericEnvs...)

	config := &container.Config{
		Labels:       c.labels(),
		StopSignal:   c.spec().StopSignal,
		Tty:          c.spec().TTY,
		OpenStdin:    c.spec().OpenStdin,
		User:         c.spec().User,
		Env:          env,
		Hostname:     c.spec().Hostname,
		WorkingDir:   c.spec().Dir,
		Image:        c.image(),
		ExposedPorts: c.exposedPorts(),
		Healthcheck:  c.healthcheck(),
	}

	if len(c.spec().Command) > 0 {
		// If Command is provided, we replace the whole invocation with Command
		// by replacing Entrypoint and specifying Cmd. Args is ignored in this
		// case.
		config.Entrypoint = append(config.Entrypoint, c.spec().Command...)
		config.Cmd = append(config.Cmd, c.spec().Args...)
	} else if len(c.spec().Args) > 0 {
		// In this case, we assume the image has an Entrypoint and Args
		// specifies the arguments for that entrypoint.
		config.Cmd = c.spec().Args
	}

	return config
}

func (c *containerConfig) labels() map[string]string {
	var (
		system = map[string]string{
			"task":         "", // mark as cluster task
			"task.id":      c.task.ID,
			"task.name":    c.name(),
			"node.id":      c.task.NodeID,
			"service.id":   c.task.ServiceID,
			"service.name": c.task.ServiceAnnotations.Name,
		}
		labels = make(map[string]string)
	)

	// base labels are those defined in the spec.
	maps.Copy(labels, c.spec().Labels)

	// we then apply the overrides from the task, which may be set via the
	// orchestrator.
	maps.Copy(labels, c.task.Annotations.Labels)

	// finally, we apply the system labels, which override all labels.
	for k, v := range system {
		labels[systemLabelPrefix+"."+k] = v
	}

	return labels
}

func (c *containerConfig) mounts(deps exec.VolumeGetter) []enginemount.Mount {
	var r []enginemount.Mount
	for _, mount := range c.spec().Mounts {
		if mount.Type == api.MountTypeCluster {
			r = append(r, c.convertCSIMount(mount, deps))
		} else {
			r = append(r, convertMount(mount))
		}
	}
	return r
}

// convertCSIMount matches the CSI mount with the path of the CSI volume.
//
// technically quadratic with respect to the number of CSI mounts, but that
// number shouldn't ever be large enough for quadratic to matter.
//
// TODO(dperny): figure out a scheme for errors? or maybe add code to
// checkMounts?
func (c *containerConfig) convertCSIMount(m api.Mount, deps exec.VolumeGetter) enginemount.Mount {
	var mount enginemount.Mount

	// these are actually bind mounts
	mount.Type = enginemount.TypeBind

	for _, attach := range c.task.Volumes {
		if attach.Source == m.Source && attach.Target == m.Target {
			// we should not get an error here, because we should have checked
			// already that the volume is ready
			path, _ := deps.Get(attach.ID)
			mount.Source = path
			mount.Target = m.Target
		}
	}

	return mount
}

func convertMount(m api.Mount) enginemount.Mount {
	mount := enginemount.Mount{
		Source:   m.Source,
		Target:   m.Target,
		ReadOnly: m.ReadOnly,
	}

	switch m.Type {
	case api.MountTypeBind:
		mount.Type = enginemount.TypeBind
	case api.MountTypeVolume:
		mount.Type = enginemount.TypeVolume
	case api.MountTypeTmpfs:
		mount.Type = enginemount.TypeTmpfs
	case api.MountTypeNamedPipe:
		mount.Type = enginemount.TypeNamedPipe
	case api.MountTypeCluster:
		mount.Type = enginemount.TypeCluster
	}

	if m.BindOptions != nil {
		mount.BindOptions = &enginemount.BindOptions{
			NonRecursive:           m.BindOptions.NonRecursive,
			CreateMountpoint:       m.BindOptions.CreateMountpoint,
			ReadOnlyNonRecursive:   m.BindOptions.ReadOnlyNonRecursive,
			ReadOnlyForceRecursive: m.BindOptions.ReadOnlyForceRecursive,
		}
		switch m.BindOptions.Propagation {
		case api.MountPropagationRPrivate:
			mount.BindOptions.Propagation = enginemount.PropagationRPrivate
		case api.MountPropagationPrivate:
			mount.BindOptions.Propagation = enginemount.PropagationPrivate
		case api.MountPropagationRSlave:
			mount.BindOptions.Propagation = enginemount.PropagationRSlave
		case api.MountPropagationSlave:
			mount.BindOptions.Propagation = enginemount.PropagationSlave
		case api.MountPropagationRShared:
			mount.BindOptions.Propagation = enginemount.PropagationRShared
		case api.MountPropagationShared:
			mount.BindOptions.Propagation = enginemount.PropagationShared
		}
	}

	if m.VolumeOptions != nil {
		mount.VolumeOptions = &enginemount.VolumeOptions{
			NoCopy:  m.VolumeOptions.NoCopy,
			Subpath: m.VolumeOptions.Subpath,
		}
		if m.VolumeOptions.Labels != nil {
			mount.VolumeOptions.Labels = make(map[string]string, len(m.VolumeOptions.Labels))
			maps.Copy(mount.VolumeOptions.Labels, m.VolumeOptions.Labels)
		}
		if m.VolumeOptions.DriverConfig != nil {
			mount.VolumeOptions.DriverConfig = &enginemount.Driver{
				Name: m.VolumeOptions.DriverConfig.Name,
			}
			if m.VolumeOptions.DriverConfig.Options != nil {
				mount.VolumeOptions.DriverConfig.Options = make(map[string]string, len(m.VolumeOptions.DriverConfig.Options))
				maps.Copy(mount.VolumeOptions.DriverConfig.Options, m.VolumeOptions.DriverConfig.Options)
			}
		}
	}

	if m.TmpfsOptions != nil {
		var options [][]string
		// see daemon/cluster/convert/container.go, tmpfsOptionsFromGRPC for
		// details on error handling.
		_ = json.Unmarshal([]byte(m.TmpfsOptions.Options), &options)
		mount.TmpfsOptions = &enginemount.TmpfsOptions{
			SizeBytes: m.TmpfsOptions.SizeBytes,
			Mode:      m.TmpfsOptions.Mode,
			Options:   options,
		}
	}

	return mount
}

func (c *containerConfig) healthcheck() *container.HealthConfig {
	hcSpec := c.spec().Healthcheck
	if hcSpec == nil {
		return nil
	}
	interval, _ := gogotypes.DurationFromProto(hcSpec.Interval)
	timeout, _ := gogotypes.DurationFromProto(hcSpec.Timeout)
	startPeriod, _ := gogotypes.DurationFromProto(hcSpec.StartPeriod)
	startInterval, _ := gogotypes.DurationFromProto(hcSpec.StartInterval)
	return &container.HealthConfig{
		Test:          hcSpec.Test,
		Interval:      interval,
		Timeout:       timeout,
		Retries:       int(hcSpec.Retries),
		StartPeriod:   startPeriod,
		StartInterval: startInterval,
	}
}

func (c *containerConfig) hostConfig(deps exec.VolumeGetter) *container.HostConfig {
	hc := &container.HostConfig{
		Resources:      c.resources(),
		GroupAdd:       c.spec().Groups,
		PortBindings:   c.portBindings(),
		Mounts:         c.mounts(deps),
		ReadonlyRootfs: c.spec().ReadOnly,
		Isolation:      c.isolation(),
		Init:           c.init(),
		Sysctls:        c.spec().Sysctls,
		CapAdd:         c.spec().CapabilityAdd,
		CapDrop:        c.spec().CapabilityDrop,
		OomScoreAdj:    int(c.spec().OomScoreAdj),
	}

	if c.spec().DNSConfig != nil {
		hc.DNS = sliceutil.Map(c.spec().DNSConfig.Nameservers, func(ns string) netip.Addr { a, _ := netip.ParseAddr(ns); return a })
		hc.DNSSearch = c.spec().DNSConfig.Search
		hc.DNSOptions = c.spec().DNSConfig.Options
	}

	c.applyPrivileges(hc)

	// The format of extra hosts on swarmkit is specified in:
	// http://man7.org/linux/man-pages/man5/hosts.5.html
	//    IP_address canonical_hostname [aliases...]
	// However, the format of ExtraHosts in HostConfig is
	//    <host>:<ip>
	// We need to do the conversion here.
	for _, entry := range c.spec().Hosts {
		parts := strings.Fields(entry)
		if len(parts) > 1 {
			hc.ExtraHosts = append(hc.ExtraHosts, fmt.Sprintf("%s:%s", strings.Join(parts[1:], " "), parts[0]))
		}
	}

	if c.task.LogDriver != nil {
		hc.LogConfig = container.LogConfig{
			Type:   c.task.LogDriver.Name,
			Config: c.task.LogDriver.Options,
		}
	}

	if len(c.task.Networks) > 0 {
		labels := c.task.Networks[0].Network.Spec.Annotations.Labels
		name := c.task.Networks[0].Network.Spec.Annotations.Name
		if v, ok := labels["com.docker.swarm.predefined"]; ok && v == "true" {
			hc.NetworkMode = container.NetworkMode(name)
		}
	}

	return hc
}

// This handles the case of volumes that are defined inside a service Mount
func (c *containerConfig) volumeCreateRequest(mount *api.Mount) *volume.CreateRequest {
	var (
		driverName string
		driverOpts map[string]string
		labels     map[string]string
	)

	if mount.VolumeOptions != nil && mount.VolumeOptions.DriverConfig != nil {
		driverName = mount.VolumeOptions.DriverConfig.Name
		driverOpts = mount.VolumeOptions.DriverConfig.Options
		labels = mount.VolumeOptions.Labels
	}

	if mount.VolumeOptions != nil {
		return &volume.CreateRequest{
			Name:       mount.Source,
			Driver:     driverName,
			DriverOpts: driverOpts,
			Labels:     labels,
		}
	}
	return nil
}

func (c *containerConfig) resources() container.Resources {
	resources := container.Resources{}

	// set pids limit
	pidsLimit := c.spec().PidsLimit
	if pidsLimit > 0 {
		resources.PidsLimit = &pidsLimit
	}

	resources.Ulimits = make([]*container.Ulimit, len(c.spec().Ulimits))
	for i, ulimit := range c.spec().Ulimits {
		resources.Ulimits[i] = &container.Ulimit{
			Name: ulimit.Name,
			Soft: ulimit.Soft,
			Hard: ulimit.Hard,
		}
	}

	// If no limits are specified let the engine use its defaults.
	//
	// TODO(aluzzardi): We might want to set some limits anyway otherwise
	// "unlimited" tasks will step over the reservation of other tasks.
	r := c.task.Spec.Resources
	if r == nil || r.Limits == nil {
		return resources
	}

	if r.Limits.MemoryBytes > 0 {
		resources.Memory = r.Limits.MemoryBytes

		if r.SwapBytes != nil {
			if swapBytes := r.SwapBytes.Value; swapBytes == -1 {
				// means unlimited
				resources.MemorySwap = -1
			} else if swapBytes >= 0 {
				// resources.MemorySwap is actually the sum of the memory + the swap
				resources.MemorySwap = resources.Memory + swapBytes
			}
		}
	}

	if r.MemorySwappiness != nil {
		resources.MemorySwappiness = &r.MemorySwappiness.Value
	}

	if r.Limits.NanoCPUs > 0 {
		resources.NanoCPUs = r.Limits.NanoCPUs
	}

	return resources
}

func (c *containerConfig) createNetworkingConfig(b executorpkg.Backend) *network.NetworkingConfig {
	var networks []*api.NetworkAttachment
	if c.task.Spec.GetContainer() != nil || c.task.Spec.GetAttachment() != nil {
		networks = c.task.Networks
	}

	epConfig := make(map[string]*network.EndpointSettings)
	for _, na := range networks {
		epConfig[na.Network.Spec.Annotations.Name] = getEndpointConfig(na, b)
	}

	return &network.NetworkingConfig{EndpointsConfig: epConfig}
}

func getEndpointConfig(na *api.NetworkAttachment, b executorpkg.Backend) *network.EndpointSettings {
	var ipv4, ipv6 netip.Addr
	for _, addr := range na.Addresses {
		pfx, err := netiputil.ParseCIDR(addr)
		if err != nil {
			continue
		}
		ip := pfx.Addr()

		if ip.Is4() {
			ipv4 = ip
		} else {
			ipv6 = ip
		}
	}

	n := &network.EndpointSettings{
		NetworkID: na.Network.ID,
		IPAMConfig: &network.EndpointIPAMConfig{
			IPv4Address: ipv4,
			IPv6Address: ipv6,
		},
		DriverOpts: na.DriverAttachmentOpts,
	}
	if v, ok := na.Network.Spec.Annotations.Labels["com.docker.swarm.predefined"]; ok && v == "true" {
		if ln, err := b.FindNetwork(na.Network.Spec.Annotations.Name); err == nil {
			n.NetworkID = ln.ID()
		}
	}
	return n
}

func (c *containerConfig) virtualIP(networkID string) string {
	if c.task.Endpoint == nil {
		return ""
	}

	for _, eVip := range c.task.Endpoint.VirtualIPs {
		// We only support IPv4 VIPs for now.
		if eVip.NetworkID == networkID {
			vip, _, err := net.ParseCIDR(eVip.Addr)
			if err != nil {
				return ""
			}

			return vip.String()
		}
	}

	return ""
}

func (c *containerConfig) serviceConfig() *clustertypes.ServiceConfig {
	if len(c.task.Networks) == 0 {
		return nil
	}

	log.G(context.TODO()).Debugf("Creating service config in agent for t = %+v", c.task)
	svcCfg := &clustertypes.ServiceConfig{
		Name:             c.task.ServiceAnnotations.Name,
		Aliases:          make(map[string][]string),
		ID:               c.task.ServiceID,
		VirtualAddresses: make(map[string]*clustertypes.VirtualAddress),
	}

	for _, na := range c.task.Networks {
		svcCfg.VirtualAddresses[na.Network.ID] = &clustertypes.VirtualAddress{
			// We support only IPv4 virtual IP for now.
			IPv4: c.virtualIP(na.Network.ID),
		}
		if len(na.Aliases) > 0 {
			svcCfg.Aliases[na.Network.ID] = na.Aliases
		}
	}

	if c.task.Endpoint != nil {
		for _, ePort := range c.task.Endpoint.Ports {
			if ePort.PublishMode != api.PublishModeIngress {
				continue
			}

			svcCfg.ExposedPorts = append(svcCfg.ExposedPorts, &clustertypes.PortConfig{
				Name:          ePort.Name,
				Protocol:      int32(ePort.Protocol),
				TargetPort:    ePort.TargetPort,
				PublishedPort: ePort.PublishedPort,
			})
		}
	}

	return svcCfg
}

func networkCreateRequest(name string, nw *api.Network) clustertypes.NetworkCreateRequest {
	ipv4Enabled := true
	ipv6Enabled := nw.Spec.Ipv6Enabled
	req := network.CreateRequest{
		Name:       name, // TODO(thaJeztah): this is the same as [nw.Spec.Annotations.Name]; consider using that instead
		Scope:      scope.Swarm,
		EnableIPv4: &ipv4Enabled,
		EnableIPv6: &ipv6Enabled,
		Internal:   nw.Spec.Internal,
		Attachable: nw.Spec.Attachable,
		Ingress:    convert.IsIngressNetwork(nw),
		Labels:     nw.Spec.Annotations.Labels,
	}

	if nw.Spec.GetNetwork() != "" {
		req.ConfigFrom = &network.ConfigReference{
			Network: nw.Spec.GetNetwork(),
		}
	}

	if nw.DriverState != nil {
		req.Driver = nw.DriverState.Name
		req.Options = nw.DriverState.Options
	}

	if nw.IPAM != nil {
		req.IPAM = &network.IPAM{
			Driver:  nw.IPAM.Driver.Name,
			Options: nw.IPAM.Driver.Options,
		}
		for _, ic := range nw.IPAM.Configs {
			// The daemon validates the IPAM configs before creating
			// the network in Swarm's Raft store, so these values
			// should always either be empty strings or well-formed
			// values.
			cfg, err := ipamConfig(ic)
			if err != nil {
				log.G(context.TODO()).WithFields(log.Fields{
					"network": name,
					"error":   err,
				}).Warn("invalid Swarm network IPAM config")
			}
			req.IPAM.Config = append(req.IPAM.Config, cfg)
		}
	}

	return clustertypes.NetworkCreateRequest{
		ID:            nw.ID,
		CreateRequest: req,
	}
}

func (c *containerConfig) applyPrivileges(hc *container.HostConfig) {
	privileges := c.spec().Privileges
	if privileges == nil {
		return
	}

	credentials := privileges.CredentialSpec
	if credentials != nil {
		switch credentials.Source.(type) {
		case *api.Privileges_CredentialSpec_File:
			hc.SecurityOpt = append(hc.SecurityOpt, "credentialspec=file://"+credentials.GetFile())
		case *api.Privileges_CredentialSpec_Registry:
			hc.SecurityOpt = append(hc.SecurityOpt, "credentialspec=registry://"+credentials.GetRegistry())
		case *api.Privileges_CredentialSpec_Config:
			hc.SecurityOpt = append(hc.SecurityOpt, "credentialspec=config://"+credentials.GetConfig())
		}
	}

	selinux := privileges.SELinuxContext
	if selinux != nil {
		if selinux.Disable {
			hc.SecurityOpt = append(hc.SecurityOpt, "label=disable")
		}
		if selinux.User != "" {
			hc.SecurityOpt = append(hc.SecurityOpt, "label=user:"+selinux.User)
		}
		if selinux.Role != "" {
			hc.SecurityOpt = append(hc.SecurityOpt, "label=role:"+selinux.Role)
		}
		if selinux.Level != "" {
			hc.SecurityOpt = append(hc.SecurityOpt, "label=level:"+selinux.Level)
		}
		if selinux.Type != "" {
			hc.SecurityOpt = append(hc.SecurityOpt, "label=type:"+selinux.Type)
		}
	}

	// variable to make the lines shorter and easier to read
	if seccomp := privileges.Seccomp; seccomp != nil {
		switch seccomp.Mode {
		// case api.Privileges_SeccompOpts_DEFAULT:
		//   if the setting is default, nothing needs to be set here. we leave
		//   the option empty.
		case api.Privileges_SeccompOpts_UNCONFINED:
			hc.SecurityOpt = append(hc.SecurityOpt, "seccomp=unconfined")
		case api.Privileges_SeccompOpts_CUSTOM:
			// Profile is bytes, but those bytes are actually a string. This is
			// basically verbatim what happens in the cli after a file is read.
			hc.SecurityOpt = append(hc.SecurityOpt, fmt.Sprintf("seccomp=%s", seccomp.Profile))
		default:
			// TODO(thaJeztah): make switch exhaustive; add api.Privileges_SeccompOpts_DEFAULT
		}
	}

	// if the setting is DEFAULT, then nothing to be done. If it's DISABLED,
	// we set that. Custom not supported yet. When custom *is* supported, make
	// it look like the above.
	if apparmor := privileges.Apparmor; apparmor != nil && apparmor.Mode == api.Privileges_AppArmorOpts_DISABLED {
		hc.SecurityOpt = append(hc.SecurityOpt, "apparmor=unconfined")
	}

	if privileges.NoNewPrivileges {
		hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges=true")
	}
}

func (c *containerConfig) eventFilter() filters.Args {
	return filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("name", c.name()),
		filters.Arg("label", fmt.Sprintf("%v.task.id=%v", systemLabelPrefix, c.task.ID)),
	)
}

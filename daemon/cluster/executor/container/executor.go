package container // import "github.com/docker/docker/daemon/cluster/executor/container"

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/daemon/cluster/controllers/plugin"
	"github.com/docker/docker/daemon/cluster/convert"
	executorpkg "github.com/docker/docker/daemon/cluster/executor"
	clustertypes "github.com/docker/docker/daemon/cluster/provider"
	networktypes "github.com/docker/libnetwork/types"
	"github.com/docker/swarmkit/agent"
	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/naming"
	"github.com/docker/swarmkit/template"
	"github.com/sirupsen/logrus"
)

type executor struct {
	backend       executorpkg.Backend
	imageBackend  executorpkg.ImageBackend
	pluginBackend plugin.Backend
	volumeBackend executorpkg.VolumeBackend
	dependencies  exec.DependencyManager
	mutex         sync.Mutex // This mutex protects the following node field
	node          *api.NodeDescription
}

// NewExecutor returns an executor from the docker client.
func NewExecutor(b executorpkg.Backend, p plugin.Backend, i executorpkg.ImageBackend, v executorpkg.VolumeBackend) exec.Executor {
	return &executor{
		backend:       b,
		pluginBackend: p,
		imageBackend:  i,
		volumeBackend: v,
		dependencies:  agent.NewDependencyManager(),
	}
}

// Describe returns the underlying node description from the docker client.
func (e *executor) Describe(ctx context.Context) (*api.NodeDescription, error) {
	info, err := e.backend.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}

	plugins := map[api.PluginDescription]struct{}{}
	addPlugins := func(typ string, names []string) {
		for _, name := range names {
			plugins[api.PluginDescription{
				Type: typ,
				Name: name,
			}] = struct{}{}
		}
	}

	// add v1 plugins
	addPlugins("Volume", info.Plugins.Volume)
	// Add builtin driver "overlay" (the only builtin multi-host driver) to
	// the plugin list by default.
	addPlugins("Network", append([]string{"overlay"}, info.Plugins.Network...))
	addPlugins("Authorization", info.Plugins.Authorization)
	addPlugins("Log", info.Plugins.Log)

	// add v2 plugins
	v2Plugins, err := e.backend.PluginManager().List(filters.NewArgs())
	if err == nil {
		for _, plgn := range v2Plugins {
			for _, typ := range plgn.Config.Interface.Types {
				if typ.Prefix != "docker" || !plgn.Enabled {
					continue
				}
				plgnTyp := typ.Capability
				switch typ.Capability {
				case "volumedriver":
					plgnTyp = "Volume"
				case "networkdriver":
					plgnTyp = "Network"
				case "logdriver":
					plgnTyp = "Log"
				}

				plugins[api.PluginDescription{
					Type: plgnTyp,
					Name: plgn.Name,
				}] = struct{}{}
			}
		}
	}

	pluginFields := make([]api.PluginDescription, 0, len(plugins))
	for k := range plugins {
		pluginFields = append(pluginFields, k)
	}

	sort.Sort(sortedPlugins(pluginFields))

	// parse []string labels into a map[string]string
	labels := map[string]string{}
	for _, l := range info.Labels {
		stringSlice := strings.SplitN(l, "=", 2)
		// this will take the last value in the list for a given key
		// ideally, one shouldn't assign multiple values to the same key
		if len(stringSlice) > 1 {
			labels[stringSlice[0]] = stringSlice[1]
		}
	}

	description := &api.NodeDescription{
		Hostname: info.Name,
		Platform: &api.Platform{
			Architecture: info.Architecture,
			OS:           info.OSType,
		},
		Engine: &api.EngineDescription{
			EngineVersion: info.ServerVersion,
			Labels:        labels,
			Plugins:       pluginFields,
		},
		Resources: &api.Resources{
			NanoCPUs:    int64(info.NCPU) * 1e9,
			MemoryBytes: info.MemTotal,
			Generic:     convert.GenericResourcesToGRPC(info.GenericResources),
		},
	}

	// Save the node information in the executor field
	e.mutex.Lock()
	e.node = description
	e.mutex.Unlock()

	return description, nil
}

func (e *executor) Configure(ctx context.Context, node *api.Node) error {
	var ingressNA *api.NetworkAttachment
	attachments := make(map[string]string)

	for _, na := range node.Attachments {
		if na == nil || na.Network == nil || len(na.Addresses) == 0 {
			// this should not happen, but we got a panic here and don't have a
			// good idea about what the underlying data structure looks like.
			logrus.WithField("NetworkAttachment", fmt.Sprintf("%#v", na)).
				Warnf("skipping nil or malformed node network attachment entry")
			continue
		}

		if na.Network.Spec.Ingress {
			ingressNA = na
		}

		attachments[na.Network.ID] = na.Addresses[0]
	}

	if (ingressNA == nil) && (node.Attachment != nil) && (len(node.Attachment.Addresses) > 0) {
		ingressNA = node.Attachment
		attachments[ingressNA.Network.ID] = ingressNA.Addresses[0]
	}

	if ingressNA == nil {
		e.backend.ReleaseIngress()
		return e.backend.GetAttachmentStore().ResetAttachments(attachments)
	}

	options := types.NetworkCreate{
		Driver: ingressNA.Network.DriverState.Name,
		IPAM: &network.IPAM{
			Driver: ingressNA.Network.IPAM.Driver.Name,
		},
		Options:        ingressNA.Network.DriverState.Options,
		Ingress:        true,
		CheckDuplicate: true,
	}

	for _, ic := range ingressNA.Network.IPAM.Configs {
		c := network.IPAMConfig{
			Subnet:  ic.Subnet,
			IPRange: ic.Range,
			Gateway: ic.Gateway,
		}
		options.IPAM.Config = append(options.IPAM.Config, c)
	}

	_, err := e.backend.SetupIngress(clustertypes.NetworkCreateRequest{
		ID: ingressNA.Network.ID,
		NetworkCreateRequest: types.NetworkCreateRequest{
			Name:          ingressNA.Network.Spec.Annotations.Name,
			NetworkCreate: options,
		},
	}, ingressNA.Addresses[0])
	if err != nil {
		return err
	}

	return e.backend.GetAttachmentStore().ResetAttachments(attachments)
}

// Controller returns a docker container runner.
func (e *executor) Controller(t *api.Task) (exec.Controller, error) {
	dependencyGetter := template.NewTemplatedDependencyGetter(agent.Restrict(e.dependencies, t), t, nil)

	// Get the node description from the executor field
	e.mutex.Lock()
	nodeDescription := e.node
	e.mutex.Unlock()

	if t.Spec.GetAttachment() != nil {
		return newNetworkAttacherController(e.backend, e.imageBackend, e.volumeBackend, t, nodeDescription, dependencyGetter)
	}

	var ctlr exec.Controller
	switch r := t.Spec.GetRuntime().(type) {
	case *api.TaskSpec_Generic:
		logrus.WithFields(logrus.Fields{
			"kind":     r.Generic.Kind,
			"type_url": r.Generic.Payload.TypeUrl,
		}).Debug("custom runtime requested")
		runtimeKind, err := naming.Runtime(t.Spec)
		if err != nil {
			return ctlr, err
		}
		switch runtimeKind {
		case string(swarmtypes.RuntimePlugin):
			if !e.backend.HasExperimental() {
				return ctlr, fmt.Errorf("runtime type %q only supported in experimental", swarmtypes.RuntimePlugin)
			}
			c, err := plugin.NewController(e.pluginBackend, t)
			if err != nil {
				return ctlr, err
			}
			ctlr = c
		default:
			return ctlr, fmt.Errorf("unsupported runtime type: %q", runtimeKind)
		}
	case *api.TaskSpec_Container:
		c, err := newController(e.backend, e.imageBackend, e.volumeBackend, t, nodeDescription, dependencyGetter)
		if err != nil {
			return ctlr, err
		}
		ctlr = c
	default:
		return ctlr, fmt.Errorf("unsupported runtime: %q", r)
	}

	return ctlr, nil
}

func (e *executor) SetNetworkBootstrapKeys(keys []*api.EncryptionKey) error {
	nwKeys := []*networktypes.EncryptionKey{}
	for _, key := range keys {
		nwKey := &networktypes.EncryptionKey{
			Subsystem:   key.Subsystem,
			Algorithm:   int32(key.Algorithm),
			Key:         make([]byte, len(key.Key)),
			LamportTime: key.LamportTime,
		}
		copy(nwKey.Key, key.Key)
		nwKeys = append(nwKeys, nwKey)
	}
	e.backend.SetNetworkBootstrapKeys(nwKeys)

	return nil
}

func (e *executor) Secrets() exec.SecretsManager {
	return e.dependencies.Secrets()
}

func (e *executor) Configs() exec.ConfigsManager {
	return e.dependencies.Configs()
}

type sortedPlugins []api.PluginDescription

func (sp sortedPlugins) Len() int { return len(sp) }

func (sp sortedPlugins) Swap(i, j int) { sp[i], sp[j] = sp[j], sp[i] }

func (sp sortedPlugins) Less(i, j int) bool {
	if sp[i].Type != sp[j].Type {
		return sp[i].Type < sp[j].Type
	}
	return sp[i].Name < sp[j].Name
}

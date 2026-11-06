package daemon

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/system"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/pkg/errors"
)

func (daemon *Daemon) setupLinkedContainers(ctr *container.Container) ([]string, error) {
	return nil, nil
}

func (daemon *Daemon) addLegacyLinks(
	ctx context.Context,
	cfg *config.Config,
	ctr *container.Container,
	epConfig *network.EndpointSettings,
	sb *libnetwork.Sandbox,
) error {
	return nil
}

func (daemon *Daemon) setupConfigDir(ctr *container.Container) (setupErr error) {
	if len(ctr.ConfigReferences) == 0 {
		return nil
	}

	localPath := ctr.ConfigsDirPath()
	log.G(context.TODO()).Debugf("configs: setting up config dir: %s", localPath)

	// create local config root
	if err := system.MkdirAllWithACL(localPath, system.SddlAdministratorsLocalSystem); err != nil {
		return errors.Wrap(err, "error creating config dir")
	}

	defer func() {
		if setupErr != nil {
			if err := os.RemoveAll(localPath); err != nil {
				log.G(context.TODO()).Errorf("error cleaning up config dir: %s", err)
			}
		}
	}()

	if ctr.DependencyStore == nil {
		return fmt.Errorf("config store is not initialized")
	}

	for _, configRef := range ctr.ConfigReferences {
		// TODO (ehazlett): use type switch when more are supported
		if configRef.File == nil {
			// Runtime configs are not mounted into the container, but they're
			// a valid type of config so we should not error when we encounter
			// one.
			if configRef.Runtime == nil {
				log.G(context.TODO()).Error("config target type is not a file or runtime target")
			}
			// However, in any case, this isn't a file config, so we have no
			// further work to do
			continue
		}

		fPath, err := ctr.ConfigFilePath(*configRef)
		if err != nil {
			return errors.Wrap(err, "error getting config file path for container")
		}
		log := log.G(context.TODO()).WithFields(log.Fields{"name": configRef.File.Name, "path": fPath})

		log.Debug("injecting config")
		config, err := ctr.DependencyStore.Configs().Get(configRef.ConfigID)
		if err != nil {
			return errors.Wrap(err, "unable to get config from config store")
		}
		if err := os.WriteFile(fPath, config.Spec.Data, configRef.File.Mode); err != nil {
			return errors.Wrap(err, "error injecting config")
		}
	}

	return nil
}

func (daemon *Daemon) setupIpcDirs(ctr *container.Container) error {
	return nil
}

// TODO Windows: Fix Post-TP5. This is a hack to allow docker cp to work
// against containers which have volumes. You will still be able to cp
// to somewhere on the container drive, but not to any mounted volumes
// inside the container. Without this fix, docker cp is broken to any
// container which has a volume, regardless of where the file is inside the
// container.
func (daemon *Daemon) mountVolumes(ctr *container.Container) error {
	return nil
}

func (daemon *Daemon) setupSecretDir(ctr *container.Container) (setupErr error) {
	if len(ctr.SecretReferences) == 0 {
		return nil
	}

	localMountPath, err := ctr.SecretMountPath()
	if err != nil {
		return err
	}
	log.G(context.TODO()).Debugf("secrets: setting up secret dir: %s", localMountPath)

	// create local secret root
	if err := system.MkdirAllWithACL(localMountPath, system.SddlAdministratorsLocalSystem); err != nil {
		return errors.Wrap(err, "error creating secret local directory")
	}

	defer func() {
		if setupErr != nil {
			if err := os.RemoveAll(localMountPath); err != nil {
				log.G(context.TODO()).Errorf("error cleaning up secret mount: %s", err)
			}
		}
	}()

	if ctr.DependencyStore == nil {
		return fmt.Errorf("secret store is not initialized")
	}

	for _, s := range ctr.SecretReferences {
		// TODO (ehazlett): use type switch when more are supported
		if s.File == nil {
			log.G(context.TODO()).Error("secret target type is not a file target")
			continue
		}

		// secrets are created in the SecretMountPath on the host, at a
		// single level
		fPath, err := ctr.SecretFilePath(*s)
		if err != nil {
			return err
		}
		log.G(context.TODO()).WithFields(log.Fields{
			"name": s.File.Name,
			"path": fPath,
		}).Debug("injecting secret")
		secret, err := ctr.DependencyStore.Secrets().Get(s.SecretID)
		if err != nil {
			return errors.Wrap(err, "unable to get secret from secret store")
		}
		if err := os.WriteFile(fPath, secret.Spec.Data, s.File.Mode); err != nil {
			return errors.Wrap(err, "error injecting secret")
		}
	}

	return nil
}

func killProcessDirectly(ctr *container.Container) error {
	return nil
}

func enableIPOnPredefinedNetwork() bool {
	return true
}

// serviceDiscoveryOnDefaultNetwork indicates if service discovery is supported on the default network
func serviceDiscoveryOnDefaultNetwork() bool {
	return true
}

func buildSandboxPlatformOptions(ctr *container.Container, cfg *config.Config) ([]libnetwork.SandboxOption, error) {
	return nil, nil
}

func (daemon *Daemon) initializeNetworkingPaths(ctr *container.Container, nc *container.Container) error {
	if nc.HostConfig.Isolation.IsHyperV() {
		return fmt.Errorf("sharing of hyperv containers network is not supported")
	}

	// HCS identifies the donor compute system by its current runtime ID.
	nc.Lock()
	ctr.NetworkSharedContainerID = nc.C8dContainerID()
	nc.Unlock()

	if nc.NetworkSettings != nil {
		for n := range nc.NetworkSettings.Networks {
			sn, err := daemon.FindNetwork(n)
			if err != nil {
				continue
			}

			ep, err := getEndpointInNetwork(nc.Name, sn)
			if err != nil {
				continue
			}

			data, err := ep.DriverInfo()
			if err != nil {
				continue
			}

			if data["GW_INFO"] != nil {
				gwInfo := data["GW_INFO"].(map[string]any)
				if gwInfo["hnsid"] != nil {
					ctr.SharedEndpointList = append(ctr.SharedEndpointList, gwInfo["hnsid"].(string))
				}
			}

			if data["hnsid"] != nil {
				ctr.SharedEndpointList = append(ctr.SharedEndpointList, data["hnsid"].(string))
			}
		}
	}

	return nil
}

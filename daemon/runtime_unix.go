// +build !windows

package daemon

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/cgroups"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	v2runcoptions "github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	defaultRuntimeName = "runc"

	linuxShimV1 = "io.containerd.runtime.v1.linux"
	linuxShimV2 = "io.containerd.runc.v2"
)

func configureRuntimes(conf *config.Config) {
	if conf.DefaultRuntime == "" {
		conf.DefaultRuntime = config.StockRuntimeName
	}
	if conf.Runtimes == nil {
		conf.Runtimes = make(map[string]types.Runtime)
	}
	conf.Runtimes[config.LinuxV1RuntimeName] = types.Runtime{Path: defaultRuntimeName, Shim: defaultV1ShimConfig(conf, defaultRuntimeName)}
	conf.Runtimes[config.LinuxV2RuntimeName] = types.Runtime{Path: defaultRuntimeName, Shim: defaultV2ShimConfig(conf, linuxShimV2, defaultRuntimeName)}
	conf.Runtimes[config.StockRuntimeName] = conf.Runtimes[config.LinuxV2RuntimeName]
}

func defaultV2ShimConfig(conf *config.Config, shimRuntime, runtimePath string) *types.ShimConfig {
	// non-empty runtime path means using an OCI compatible runtime implementation.
	// otherwise a shim v2 compatible runtime will be set
	if runtimePath != "" {
		shimRuntime = linuxShimV2
	}
	return &types.ShimConfig{
		Binary: shimRuntime,
		Opts: &v2runcoptions.Options{
			BinaryName:    runtimePath,
			Root:          filepath.Join(conf.ExecRoot, "runtime-"+defaultRuntimeName),
			SystemdCgroup: UsingSystemd(conf),
			NoPivotRoot:   os.Getenv("DOCKER_RAMDISK") != "",
		},
	}
}

func defaultV1ShimConfig(conf *config.Config, runtimePath string) *types.ShimConfig {
	return &types.ShimConfig{
		Binary: linuxShimV1,
		Opts: &runctypes.RuncOptions{
			Runtime:       runtimePath,
			RuntimeRoot:   filepath.Join(conf.ExecRoot, "runtime-"+defaultRuntimeName),
			SystemdCgroup: UsingSystemd(conf),
		},
	}
}

func (daemon *Daemon) loadRuntimes() error {
	return daemon.initRuntimes(daemon.configStore.Runtimes)
}

func (daemon *Daemon) initRuntimes(runtimes map[string]types.Runtime) (err error) {
	runtimeDir := filepath.Join(daemon.configStore.Root, "runtimes")
	// Remove old temp directory if any
	os.RemoveAll(runtimeDir + "-old")
	tmpDir, err := ioutils.TempDir(daemon.configStore.Root, "gen-runtimes")
	if err != nil {
		return errors.Wrap(err, "failed to get temp dir to generate runtime scripts")
	}
	defer func() {
		if err != nil {
			if err1 := os.RemoveAll(tmpDir); err1 != nil {
				logrus.WithError(err1).WithField("dir", tmpDir).
					Warn("failed to remove tmp dir")
			}
			return
		}

		if err = os.Rename(runtimeDir, runtimeDir+"-old"); err != nil {
			return
		}
		if err = os.Rename(tmpDir, runtimeDir); err != nil {
			err = errors.Wrap(err, "failed to setup runtimes dir, new containers may not start")
			return
		}
		if err = os.RemoveAll(runtimeDir + "-old"); err != nil {
			logrus.WithError(err).WithField("dir", tmpDir).
				Warn("failed to remove old runtimes dir")
		}
	}()

	for name, rt := range runtimes {
		if len(rt.Args) > 0 {
			script := filepath.Join(tmpDir, name)
			content := fmt.Sprintf("#!/bin/sh\n%s %s $@\n", rt.Path, strings.Join(rt.Args, " "))
			if err := ioutil.WriteFile(script, []byte(content), 0700); err != nil {
				return err
			}
		}
		if rt.Shim == nil {
			rt.Shim = defaultV2ShimConfig(daemon.configStore, name, rt.Path)
		}
	}
	return nil
}

// rewriteRuntimePath is used for runtimes which have custom arguments supplied.
// This is needed because the containerd API only calls the OCI runtime binary, there is no options for extra arguments.
// To support this case, the daemon wraps the specified runtime in a script that passes through those arguments.
func (daemon *Daemon) rewriteRuntimePath(name, p string, args []string) (string, error) {
	if len(args) == 0 {
		return p, nil
	}

	// Check that the runtime path actually exists here so that we can return a well known error.
	if _, err := exec.LookPath(p); err != nil {
		return "", errors.Wrap(err, "error while looking up the specified runtime path")
	}

	return filepath.Join(daemon.configStore.Root, "runtimes", name), nil
}

func (daemon *Daemon) getRuntime(name string) (*types.Runtime, error) {
	rt := daemon.configStore.GetRuntime(name)
	if rt == nil {
		return nil, errdefs.InvalidParameter(errors.Errorf("runtime not found in config: %s", name))
	}

	if len(rt.Args) > 0 {
		p, err := daemon.rewriteRuntimePath(name, rt.Path, rt.Args)
		if err != nil {
			return nil, err
		}
		rt.Path = p
		rt.Args = nil
	}

	if rt.Shim == nil {
		rt.Shim = defaultV2ShimConfig(daemon.configStore, name, rt.Path)
	}

	if rt.Shim.Binary == linuxShimV1 {
		if cgroups.Mode() == cgroups.Unified {
			return nil, errdefs.InvalidParameter(errors.Errorf("runtime %q is not supported while cgroups v2 (unified hierarchy) is being used", name))
		}
		logrus.Warnf("Configured runtime %q is deprecated and will be removed in the next release", name)
	}

	return rt, nil
}

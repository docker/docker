package daemon

import (
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/pkg/errors"
)

type rootlessSystemdCgroupInfo struct {
	groupPath   string
	controllers []string
}

func rootlessSystemdCgroupControllerInfo() (rootlessSystemdCgroupInfo, error) {
	return rootlessSystemdCgroupInfo{}, errors.New("rootless systemd cgroups are not supported on FreeBSD")
}

func rootlessSystemdSysInfoOptions(cfg *config.Config) ([]sysinfo.Opt, error) {
	if cfg.Rootless && cgroupDriver(cfg) == cgroupSystemdDriver {
		return nil, errors.New("rootless systemd cgroups are not supported on FreeBSD")
	}
	return nil, nil
}

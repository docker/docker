// +build no_systemd

package main

import (
	"github.com/docker/docker/daemon/config"
)

func newCgroupParent(config *config.Config) string {
	cgroupParent := "docker"
	if config.CgroupParent != "" {
		cgroupParent = config.CgroupParent
	}
	return cgroupParent
}

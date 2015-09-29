// +build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"

	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/volume"
	"github.com/opencontainers/runc/libcontainer/label"
)

// createContainerPlatformSpecificSettings performs platform specific container create functionality
func createContainerPlatformSpecificSettings(container *Container, config *runconfig.Config, hostConfig *runconfig.HostConfig, img *image.Image) error {
	for spec := range config.Volumes {
		var (
			name, destination string
			parts             = strings.Split(spec, ":")
			mode              = true
		)

		switch len(parts) {
		case 2:
			if parts[1] == "ro" {
				mode = false
				parts = parts[:1]
			} else if parts[1] == "rw" {
				parts = parts[:1]
			}
		case 3:
			if parts[2] == "ro" {
				mode = false
				parts = parts[:2]
			} else if parts[2] == "rw" {
				parts = parts[:2]
			}
		}

		switch len(parts) {
		case 2:
			name, destination = parts[0], filepath.Clean(parts[1])
		default:
			name = stringid.GenerateNonCryptoID()
			destination = filepath.Clean(parts[0])
		}
		// Skip volumes for which we already have something mounted on that
		// destination because of a --volume-from.
		if container.isDestinationMounted(destination) {
			continue
		}
		path, err := container.GetResourcePath(destination)
		if err != nil {
			return err
		}

		stat, err := os.Stat(path)
		if err == nil && !stat.IsDir() {
			return derr.ErrorCodeMountOverFile.WithArgs(path)
		}

		volumeDriver := hostConfig.VolumeDriver
		if destination != "" && img != nil {
			if _, ok := img.ContainerConfig.Volumes[destination]; ok {
				// check for whether bind is not specified and then set to local
				if _, ok := container.MountPoints[destination]; !ok {
					volumeDriver = volume.DefaultDriverName
				}
			}
		}

		v, err := container.daemon.createVolume(name, volumeDriver, nil)
		if err != nil {
			return err
		}

		if err := label.Relabel(v.Path(), container.MountLabel, true); err != nil {
			return err
		}

		// never attempt to copy existing content in a container FS to a shared volume
		if v.DriverName() == volume.DefaultDriverName {
			if err := container.copyImagePathContent(v, destination); err != nil {
				return err
			}
		}

		container.addMountPointWithVolume(destination, v, mode)
	}
	return nil
}

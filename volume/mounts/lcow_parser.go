package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"path"

	"github.com/docker/docker/api/types/mount"
)

// NewLCOWParser creates a parser with Linux Containers on Windows semantics.
func NewLCOWParser() Parser {
	return &lcowParser{
		windowsParser{
			fi: defaultFileInfoProvider{},
		},
	}
}

// rxLCOWDestination is the regex expression for the mount destination for LCOW
//
// Destination (aka container path):
//    -  Variation on hostdir but can be a drive followed by colon as well
//    -  If a path, must be absolute. Can include spaces
//    -  Drive cannot be c: (explicitly checked in code, not RegEx)
const rxLCOWDestination = `(?P<destination>/(?:[^\\/:*?"<>\r\n]+[/]?)*)`

var lcowSpecificValidators mountValidator = func(m *mount.Mount) error {
	if path.Clean(m.Target) == "/" {
		return ErrVolumeTargetIsRoot
	}
	if m.Type == mount.TypeNamedPipe {
		return errors.New("Linux containers on Windows do not support named pipe mounts")
	}
	return nil
}

type lcowParser struct {
	windowsParser
}

func (p *lcowParser) ValidateMountConfig(mnt *mount.Mount) error {
	return p.validateMountConfigReg(mnt, rxLCOWDestination, lcowSpecificValidators)
}

func (p *lcowParser) ParseMountRaw(raw, volumeDriver string) (*MountPoint, error) {
	return p.parseMountRaw(raw, volumeDriver, rxLCOWDestination, false, lcowSpecificValidators)
}

func (p *lcowParser) ParseMountSpec(cfg mount.Mount) (*MountPoint, error) {
	return p.parseMountSpec(cfg, rxLCOWDestination, false, lcowSpecificValidators)
}

package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"os"

	"github.com/docker/docker/api/types/mount"
)

// read-write modes
var rwModes = map[string]bool{
	"rw": true,
	"ro": true,
}

type fileInfoProvider interface {
	fileInfo(path string) (exist, isDir bool, err error)
}

type defaultFileInfoProvider struct {
}

func (defaultFileInfoProvider) fileInfo(path string) (exist, isDir bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, false, err
		}
		return false, false, nil
	}
	return true, fi.IsDir(), nil
}

var currentFileInfoProvider fileInfoProvider = defaultFileInfoProvider{}

// Parser represents a platform specific parser for mount expressions
type Parser interface {
	ParseMountRaw(raw, volumeDriver string) (*MountPoint, error)
	ParseMountSpec(cfg mount.Mount) (*MountPoint, error)
	ParseVolumesFrom(spec string) (string, string, error)
	DefaultPropagationMode() mount.Propagation
	ConvertTmpfsOptions(opt *mount.TmpfsOptions, readOnly bool) (string, error)
	DefaultCopyMode() bool
	ValidateVolumeName(name string) error
	ReadWrite(mode string) bool
	IsBackwardCompatible(m *MountPoint) bool
	HasResource(m *MountPoint, absPath string) bool
	ValidateTmpfsMountDestination(dest string) error
	ValidateMountConfig(mt *mount.Mount) error
}

// NewParser creates a parser for the current host OS
func NewParser() Parser {
	return newParser()
}

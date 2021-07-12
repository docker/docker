package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"runtime"

	"github.com/docker/docker/api/types/mount"
)

// ErrVolumeTargetIsRoot is returned when the target destination is root.
// It's used by both LCOW and Linux parsers.
var ErrVolumeTargetIsRoot = errors.New("invalid specification: destination can't be '/'")

// read-write modes
var rwModes = map[string]bool{
	"rw": true,
	"ro": true,
}

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
	if runtime.GOOS == "windows" {
		return NewWindowsParser()
	}
	return NewLinuxParser()
}

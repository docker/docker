package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/volume"
)

type linuxParser struct {
}

func linuxSplitRawSpec(raw string) ([]string, error) {
	if strings.Count(raw, ":") > 2 {
		return nil, errInvalidSpec(raw)
	}

	arr := strings.SplitN(raw, ":", 3)
	if arr[0] == "" {
		return nil, errInvalidSpec(raw)
	}
	return arr, nil
}

func linuxValidateNotRoot(p string) error {
	p = path.Clean(strings.Replace(p, `\`, `/`, -1))
	if p == "/" {
		return ErrVolumeTargetIsRoot
	}
	return nil
}
func linuxValidateAbsolute(p string) error {
	p = strings.Replace(p, `\`, `/`, -1)
	if path.IsAbs(p) {
		return nil
	}
	return fmt.Errorf("invalid mount path: '%s' mount path must be absolute", p)
}
func (p *linuxParser) ValidateMountConfig(mnt *mount.Mount) error {
	// there was something looking like a bug in existing codebase:
	// - validateMountConfig on linux was called with options skipping bind source existence when calling ParseMountRaw
	// - but not when calling ParseMountSpec directly... nor when the unit test called it directly
	return p.validateMountConfigImpl(mnt, true)
}
func (p *linuxParser) validateMountConfigImpl(mnt *mount.Mount, validateBindSourceExists bool) error {
	if len(mnt.Target) == 0 {
		return &errMountConfig{mnt, errMissingField("Target")}
	}

	if err := linuxValidateNotRoot(mnt.Target); err != nil {
		return &errMountConfig{mnt, err}
	}

	if err := linuxValidateAbsolute(mnt.Target); err != nil {
		return &errMountConfig{mnt, err}
	}

	switch mnt.Type {
	case mount.TypeBind:
		if len(mnt.Source) == 0 {
			return &errMountConfig{mnt, errMissingField("Source")}
		}
		// Don't error out just because the propagation mode is not supported on the platform
		if opts := mnt.BindOptions; opts != nil {
			if len(opts.Propagation) > 0 && len(linuxPropagationModes) > 0 {
				if _, ok := linuxPropagationModes[opts.Propagation]; !ok {
					return &errMountConfig{mnt, fmt.Errorf("invalid propagation mode: %s", opts.Propagation)}
				}
			}
		}
		if mnt.VolumeOptions != nil {
			return &errMountConfig{mnt, errExtraField("VolumeOptions")}
		}

		if err := linuxValidateAbsolute(mnt.Source); err != nil {
			return &errMountConfig{mnt, err}
		}

		if validateBindSourceExists {
			exists, _, err := currentFileInfoProvider.fileInfo(mnt.Source)
			if err != nil {
				return &errMountConfig{mnt, err}
			}
			if !exists {
				return &errMountConfig{mnt, errBindSourceDoesNotExist(mnt.Source)}
			}
		}

	case mount.TypeVolume:
		if mnt.BindOptions != nil {
			return &errMountConfig{mnt, errExtraField("BindOptions")}
		}

		if len(mnt.Source) == 0 && mnt.ReadOnly {
			return &errMountConfig{mnt, fmt.Errorf("must not set ReadOnly mode when using anonymous volumes")}
		}
	case mount.TypeTmpfs:
		if mnt.BindOptions != nil {
			return &errMountConfig{mnt, errExtraField("BindOptions")}
		}
		if len(mnt.Source) != 0 {
			return &errMountConfig{mnt, errExtraField("Source")}
		}
		if _, err := p.ConvertTmpfsOptions(mnt.TmpfsOptions, mnt.ReadOnly); err != nil {
			return &errMountConfig{mnt, err}
		}
	default:
		return &errMountConfig{mnt, errors.New("mount type unknown")}
	}
	return nil
}

// read-write modes
var rwModes = map[string]bool{
	"rw": true,
	"ro": true,
}

// label modes
var linuxLabelModes = map[string]bool{
	"Z": true,
	"z": true,
}

// consistency modes
var linuxConsistencyModes = map[mount.Consistency]bool{
	mount.ConsistencyFull:      true,
	mount.ConsistencyCached:    true,
	mount.ConsistencyDelegated: true,
}
var linuxPropagationModes = map[mount.Propagation]bool{
	mount.PropagationPrivate:  true,
	mount.PropagationRPrivate: true,
	mount.PropagationSlave:    true,
	mount.PropagationRSlave:   true,
	mount.PropagationShared:   true,
	mount.PropagationRShared:  true,
}

const linuxDefaultPropagationMode = mount.PropagationRPrivate

func linuxGetPropagation(mode string) mount.Propagation {
	for _, o := range strings.Split(mode, ",") {
		prop := mount.Propagation(o)
		if linuxPropagationModes[prop] {
			return prop
		}
	}
	return linuxDefaultPropagationMode
}

func linuxHasPropagation(mode string) bool {
	for _, o := range strings.Split(mode, ",") {
		if linuxPropagationModes[mount.Propagation(o)] {
			return true
		}
	}
	return false
}

func linuxValidMountMode(mode string) bool {
	if mode == "" {
		return true
	}

	rwModeCount := 0
	labelModeCount := 0
	propagationModeCount := 0
	copyModeCount := 0
	consistencyModeCount := 0

	for _, o := range strings.Split(mode, ",") {
		switch {
		case rwModes[o]:
			rwModeCount++
		case linuxLabelModes[o]:
			labelModeCount++
		case linuxPropagationModes[mount.Propagation(o)]:
			propagationModeCount++
		case copyModeExists(o):
			copyModeCount++
		case linuxConsistencyModes[mount.Consistency(o)]:
			consistencyModeCount++
		default:
			return false
		}
	}

	// Only one string for each mode is allowed.
	if rwModeCount > 1 || labelModeCount > 1 || propagationModeCount > 1 || copyModeCount > 1 || consistencyModeCount > 1 {
		return false
	}
	return true
}

func (p *linuxParser) ReadWrite(mode string) bool {
	if !linuxValidMountMode(mode) {
		return false
	}

	for _, o := range strings.Split(mode, ",") {
		if o == "ro" {
			return false
		}
	}
	return true
}

func (p *linuxParser) ParseMountRaw(raw, volumeDriver string) (*MountPoint, error) {
	arr, err := linuxSplitRawSpec(raw)
	if err != nil {
		return nil, err
	}

	var spec mount.Mount
	var mode string
	switch len(arr) {
	case 1:
		// Just a destination path in the container
		spec.Target = arr[0]
	case 2:
		if linuxValidMountMode(arr[1]) {
			// Destination + Mode is not a valid volume - volumes
			// cannot include a mode. e.g. /foo:rw
			return nil, errInvalidSpec(raw)
		}
		// Host Source Path or Name + Destination
		spec.Source = arr[0]
		spec.Target = arr[1]
	case 3:
		// HostSourcePath+DestinationPath+Mode
		spec.Source = arr[0]
		spec.Target = arr[1]
		mode = arr[2]
	default:
		return nil, errInvalidSpec(raw)
	}

	if !linuxValidMountMode(mode) {
		return nil, errInvalidMode(mode)
	}

	if path.IsAbs(spec.Source) {
		spec.Type = mount.TypeBind
	} else {
		spec.Type = mount.TypeVolume
	}

	spec.ReadOnly = !p.ReadWrite(mode)

	// cannot assume that if a volume driver is passed in that we should set it
	if volumeDriver != "" && spec.Type == mount.TypeVolume {
		spec.VolumeOptions = &mount.VolumeOptions{
			DriverConfig: &mount.Driver{Name: volumeDriver},
		}
	}

	if copyData, isSet := getCopyMode(mode, p.DefaultCopyMode()); isSet {
		if spec.VolumeOptions == nil {
			spec.VolumeOptions = &mount.VolumeOptions{}
		}
		spec.VolumeOptions.NoCopy = !copyData
	}
	if linuxHasPropagation(mode) {
		spec.BindOptions = &mount.BindOptions{
			Propagation: linuxGetPropagation(mode),
		}
	}

	mp, err := p.parseMountSpec(spec, false)
	if mp != nil {
		mp.Mode = mode
	}
	if err != nil {
		err = fmt.Errorf("%v: %v", errInvalidSpec(raw), err)
	}
	return mp, err
}
func (p *linuxParser) ParseMountSpec(cfg mount.Mount) (*MountPoint, error) {
	return p.parseMountSpec(cfg, true)
}
func (p *linuxParser) parseMountSpec(cfg mount.Mount, validateBindSourceExists bool) (*MountPoint, error) {
	if err := p.validateMountConfigImpl(&cfg, validateBindSourceExists); err != nil {
		return nil, err
	}
	mp := &MountPoint{
		RW:          !cfg.ReadOnly,
		Destination: path.Clean(filepath.ToSlash(cfg.Target)),
		Type:        cfg.Type,
		Spec:        cfg,
	}

	switch cfg.Type {
	case mount.TypeVolume:
		if cfg.Source == "" {
			mp.Name = stringid.GenerateNonCryptoID()
		} else {
			mp.Name = cfg.Source
		}
		mp.CopyData = p.DefaultCopyMode()

		if cfg.VolumeOptions != nil {
			if cfg.VolumeOptions.DriverConfig != nil {
				mp.Driver = cfg.VolumeOptions.DriverConfig.Name
			}
			if cfg.VolumeOptions.NoCopy {
				mp.CopyData = false
			}
		}
	case mount.TypeBind:
		mp.Source = path.Clean(filepath.ToSlash(cfg.Source))
		if cfg.BindOptions != nil && len(cfg.BindOptions.Propagation) > 0 {
			mp.Propagation = cfg.BindOptions.Propagation
		} else {
			// If user did not specify a propagation mode, get
			// default propagation mode.
			mp.Propagation = linuxDefaultPropagationMode
		}
	case mount.TypeTmpfs:
		// NOP
	}
	return mp, nil
}

func (p *linuxParser) ParseVolumesFrom(spec string) (string, string, error) {
	if len(spec) == 0 {
		return "", "", fmt.Errorf("volumes-from specification cannot be an empty string")
	}

	specParts := strings.SplitN(spec, ":", 2)
	id := specParts[0]
	mode := "rw"

	if len(specParts) == 2 {
		mode = specParts[1]
		if !linuxValidMountMode(mode) {
			return "", "", errInvalidMode(mode)
		}
		// For now don't allow propagation properties while importing
		// volumes from data container. These volumes will inherit
		// the same propagation property as of the original volume
		// in data container. This probably can be relaxed in future.
		if linuxHasPropagation(mode) {
			return "", "", errInvalidMode(mode)
		}
		// Do not allow copy modes on volumes-from
		if _, isSet := getCopyMode(mode, p.DefaultCopyMode()); isSet {
			return "", "", errInvalidMode(mode)
		}
	}
	return id, mode, nil
}

func (p *linuxParser) DefaultPropagationMode() mount.Propagation {
	return linuxDefaultPropagationMode
}

func (p *linuxParser) ConvertTmpfsOptions(opt *mount.TmpfsOptions, readOnly bool) (string, error) {
	var rawOpts []string
	if readOnly {
		rawOpts = append(rawOpts, "ro")
	}

	if opt != nil && opt.Mode != 0 {
		rawOpts = append(rawOpts, fmt.Sprintf("mode=%o", opt.Mode))
	}

	if opt != nil && opt.SizeBytes != 0 {
		// calculate suffix here, making this linux specific, but that is
		// okay, since API is that way anyways.

		// we do this by finding the suffix that divides evenly into the
		// value, returning the value itself, with no suffix, if it fails.
		//
		// For the most part, we don't enforce any semantic to this values.
		// The operating system will usually align this and enforce minimum
		// and maximums.
		var (
			size   = opt.SizeBytes
			suffix string
		)
		for _, r := range []struct {
			suffix  string
			divisor int64
		}{
			{"g", 1 << 30},
			{"m", 1 << 20},
			{"k", 1 << 10},
		} {
			if size%r.divisor == 0 {
				size = size / r.divisor
				suffix = r.suffix
				break
			}
		}

		rawOpts = append(rawOpts, fmt.Sprintf("size=%d%s", size, suffix))
	}
	return strings.Join(rawOpts, ","), nil
}

func (p *linuxParser) DefaultCopyMode() bool {
	return true
}
func (p *linuxParser) ValidateVolumeName(name string) error {
	return nil
}

func (p *linuxParser) IsBackwardCompatible(m *MountPoint) bool {
	return len(m.Source) > 0 || m.Driver == volume.DefaultDriverName
}

func (p *linuxParser) ValidateTmpfsMountDestination(dest string) error {
	if err := linuxValidateNotRoot(dest); err != nil {
		return err
	}
	return linuxValidateAbsolute(dest)
}

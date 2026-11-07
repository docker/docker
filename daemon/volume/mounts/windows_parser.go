package mounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/v2/daemon/internal/lazyregexp"
)

// NewWindowsParser creates a parser with Windows semantics.
func NewWindowsParser() Parser {
	return &windowsParser{
		fi: defaultFileInfoProvider{},
	}
}

type windowsParser struct {
	fi fileInfoProvider
}

const (
	// Spec should be in the format [source:]destination[:mode]
	//
	// Examples: c:\foo bar:d:rw
	//           c:\foo:d:\bar
	//           myname:d:
	//           d:\
	//
	// Explanation of this regex! Thanks @thaJeztah on IRC and gist for help. See
	// https://gist.github.com/thaJeztah/6185659e4978789fb2b2. A good place to
	// test is https://regex-golang.appspot.com/assets/html/index.html
	//
	// Useful link for referencing named capturing groups:
	// http://stackoverflow.com/questions/20750843/using-named-matches-from-go-regex
	//
	// There are three match groups: source, destination and mode.
	//

	// rxHostDir is the first option of a source
	rxHostDir = `(?:\\\\\?\\)?[a-z]:[\\/](?:[^\\/:*?"<>|\r\n]+[\\/]?)*`
	// rxName is the second option of a source
	rxName = `[^\\/:*?"<>|\r\n]+`

	// RXReservedNames are reserved names not possible on Windows
	rxReservedNames = `(con|prn|nul|aux|com[1-9]|lpt[1-9])`

	// rxPipe is a named path pipe (starts with `\\.\pipe\`, possibly with / instead of \)
	rxPipe = `[/\\]{2}.[/\\]pipe[/\\][^:*?"<>|\r\n]+`
	// rxSource is the combined possibilities for a source
	rxSource = `((?P<source>((` + rxHostDir + `)|(` + rxName + `)|(` + rxPipe + `))):)?`

	// Source. Can be either a host directory, a name, or omitted:
	//  HostDir:
	//    -  Essentially using the folder solution from
	//       https://www.safaribooksonline.com/library/view/regular-expressions-cookbook/9781449327453/ch08s18.html
	//       but adding case insensitivity.
	//    -  Must be an absolute path such as c:\path
	//    -  Can include spaces such as `c:\program files`
	//    -  And then followed by a colon which is not in the capture group
	//    -  And can be optional
	//  Name:
	//    -  Must not contain invalid NTFS filename characters (https://msdn.microsoft.com/en-us/library/windows/desktop/aa365247(v=vs.85).aspx)
	//    -  And then followed by a colon which is not in the capture group
	//    -  And can be optional

	// rxDestination is the regex expression for the mount destination
	rxDestination = `(?P<destination>((?:\\\\\?\\)?([a-z]):((?:[\\/][^\\/:*?"<>\r\n]+)*[\\/]?))|(` + rxPipe + `))`

	// rxMode is the regex expression for the mode of the mount
	// Mode (optional):
	//    -  Hopefully self explanatory in comparison to above regex's.
	//    -  Colon is not in the capture group
	rxMode = `(:(?P<mode>(?i)ro|rw))?`
)

var (
	volumeNameRegexp          = lazyregexp.New(`^` + rxName + `$`)
	reservedNameRegexp        = lazyregexp.New(`^` + rxReservedNames + `$`)
	hostDirRegexp             = lazyregexp.New(`^` + rxHostDir + `$`)
	mountDestinationRegexp    = lazyregexp.New(`^` + rxDestination + `$`)
	windowsSplitRawSpecRegexp = lazyregexp.New(`^` + rxSource + rxDestination + rxMode + `$`)
)

type mountValidator func(mnt *mount.Mount) error

func (p *windowsParser) splitRawSpec(raw string, splitRegexp *lazyregexp.Regexp) ([]string, error) {
	match := splitRegexp.FindStringSubmatch(strings.ToLower(raw))
	if len(match) == 0 {
		return nil, errInvalidSpec(raw)
	}

	var split []string
	matchgroups := make(map[string]string)
	// Pull out the sub expressions from the named capture groups
	for i, name := range splitRegexp.SubexpNames() {
		matchgroups[name] = strings.ToLower(match[i])
	}
	if source, exists := matchgroups["source"]; exists {
		if source != "" {
			split = append(split, source)
		}
	}
	if destination, exists := matchgroups["destination"]; exists {
		if destination != "" {
			split = append(split, destination)
		}
	}
	if mode, exists := matchgroups["mode"]; exists {
		if mode != "" {
			split = append(split, mode)
		}
	}
	// Fix #26329. If the destination appears to be a file, and the source is null,
	// it may be because we've fallen through the possible naming regex and hit a
	// situation where the user intention was to map a file into a container through
	// a local volume, but this is not supported by the platform.
	if matchgroups["source"] == "" && matchgroups["destination"] != "" {
		if volumeNameRegexp.MatchString(matchgroups["destination"]) {
			if reservedNameRegexp.MatchString(matchgroups["destination"]) {
				return nil, fmt.Errorf("volume name %q cannot be a reserved word for Windows filenames", matchgroups["destination"])
			}
		} else {
			exists, isDir, _ := p.fi.fileInfo(matchgroups["destination"])
			if exists && !isDir {
				return nil, fmt.Errorf("file '%s' cannot be mapped. Only directories can be mapped on this platform", matchgroups["destination"])
			}
		}
	}
	return split, nil
}

func windowsValidMountMode(mode string) bool {
	if mode == "" {
		return true
	}
	// TODO should windows mounts produce an error if any mode was provided (they're a no-op on windows)
	return rwModes[strings.ToLower(mode)]
}

func windowsValidateNotRoot(p string) error {
	p = strings.ToLower(strings.ReplaceAll(p, `/`, `\`))
	if p == "c:" || p == `c:\` {
		return fmt.Errorf(`destination path (%v) cannot be 'c:' or 'c:\'`, p)
	}
	return nil
}

var windowsValidators mountValidator = func(m *mount.Mount) error {
	if err := windowsValidateNotRoot(m.Target); err != nil {
		return err
	}
	if !mountDestinationRegexp.MatchString(strings.ToLower(m.Target)) {
		return fmt.Errorf("invalid mount path: '%s'", m.Target)
	}
	return nil
}

func windowsValidateAbsolute(p string) error {
	if !mountDestinationRegexp.MatchString(strings.ToLower(p)) {
		return fmt.Errorf("invalid mount path: '%s' mount path must be absolute", p)
	}
	return nil
}

func windowsDetectMountType(p string) mount.Type {
	switch {
	case strings.HasPrefix(p, `\\.\pipe\`):
		return mount.TypeNamedPipe
	case hostDirRegexp.MatchString(p):
		return mount.TypeBind
	default:
		return mount.TypeVolume
	}
}

func (p *windowsParser) ReadWrite(mode string) bool {
	return strings.ToLower(mode) != "ro"
}

// ValidateVolumeName checks a volume name in a platform specific manner.
func (p *windowsParser) ValidateVolumeName(name string) error {
	if !volumeNameRegexp.MatchString(name) {
		return errors.New("invalid volume name")
	}
	if reservedNameRegexp.MatchString(name) {
		return fmt.Errorf("volume name %q cannot be a reserved word for Windows filenames", name)
	}
	return nil
}

func (p *windowsParser) ValidateMountConfig(mnt *mount.Mount) error {
	return p.validateMountConfigReg(mnt, windowsValidators)
}

type fileInfoProvider interface {
	fileInfo(path string) (exist, isDir bool, err error)
}

type defaultFileInfoProvider struct{}

func (defaultFileInfoProvider) fileInfo(path string) (exist, isDir bool, _ error) {
	fi, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, false, err
		}
		return false, false, nil
	}
	return true, fi.IsDir(), nil
}

func (p *windowsParser) validateMountConfigReg(mnt *mount.Mount, additionalValidators ...mountValidator) error {
	if err := validateExclusiveOptions(mnt); err != nil {
		return &errMountConfig{mount: mnt, err: err}
	}

	if mnt.Target == "" {
		return &errMountConfig{mnt, errMissingField("Target")}
	}
	for _, v := range additionalValidators {
		if err := v(mnt); err != nil {
			return &errMountConfig{mnt, err}
		}
	}

	switch mnt.Type {
	case mount.TypeBind:
		if mnt.Source == "" {
			return &errMountConfig{mnt, errMissingField("Source")}
		}
		// Don't error out just because the propagation mode is not supported on the platform
		if opts := mnt.BindOptions; opts != nil {
			if len(opts.Propagation) > 0 {
				return &errMountConfig{mnt, fmt.Errorf("invalid propagation mode: %s", opts.Propagation)}
			}
		}

		if err := windowsValidateAbsolute(mnt.Source); err != nil {
			return &errMountConfig{mnt, err}
		}

		exists, isdir, err := p.fi.fileInfo(mnt.Source)
		if err != nil {
			return &errMountConfig{mnt, err}
		}
		if !exists {
			return &errMountConfig{mnt, errBindSourceDoesNotExist(mnt.Source)}
		}
		if !isdir {
			return &errMountConfig{mnt, errors.New("source path must be a directory")}
		}

	case mount.TypeVolume:
		anonymousVolume := mnt.Source == ""
		if mnt.VolumeOptions != nil && mnt.VolumeOptions.Subpath != "" {
			if anonymousVolume {
				return errAnonymousVolumeWithSubpath
			}

			// Check if path is relative but without any back traversals
			if !filepath.IsLocal(mnt.VolumeOptions.Subpath) {
				return &errMountConfig{mnt, errInvalidSubpath}
			}
		}

		if mnt.Source != "" {
			if err := p.ValidateVolumeName(mnt.Source); err != nil {
				return &errMountConfig{mnt, err}
			}
		}
	case mount.TypeNamedPipe:
		if mnt.Source == "" {
			return &errMountConfig{mnt, errMissingField("Source")}
		}

		if mnt.ReadOnly {
			return &errMountConfig{mnt, errExtraField("ReadOnly")}
		}

		if windowsDetectMountType(mnt.Source) != mount.TypeNamedPipe {
			return &errMountConfig{mnt, fmt.Errorf("'%s' is not a valid pipe path", mnt.Source)}
		}

		if windowsDetectMountType(mnt.Target) != mount.TypeNamedPipe {
			return &errMountConfig{mnt, fmt.Errorf("'%s' is not a valid pipe path", mnt.Target)}
		}
	default:
		return &errMountConfig{mnt, errors.New("mount type unknown")}
	}
	return nil
}

func (p *windowsParser) ParseMountRaw(raw, volumeDriver string) (*MountPoint, error) {
	arr, err := p.splitRawSpec(raw, windowsSplitRawSpecRegexp)
	if err != nil {
		return nil, err
	}
	return p.parseMount(arr, raw, volumeDriver, true, windowsValidators)
}

func (p *windowsParser) parseMount(arr []string, raw, volumeDriver string, convertTargetToBackslash bool, additionalValidators ...mountValidator) (*MountPoint, error) {
	var spec mount.Mount
	var mode string
	switch len(arr) {
	case 1:
		// Just a destination path in the container
		spec.Target = arr[0]
	case 2:
		if windowsValidMountMode(arr[1]) {
			// Destination + Mode is not a valid volume - volumes
			// cannot include a mode. e.g. /foo:rw
			return nil, errInvalidSpec(raw)
		}
		// Host Source Path or Name + Destination
		spec.Source = strings.ReplaceAll(arr[0], `/`, `\`)
		spec.Target = arr[1]
	case 3:
		// HostSourcePath+DestinationPath+Mode
		spec.Source = strings.ReplaceAll(arr[0], `/`, `\`)
		spec.Target = arr[1]
		mode = arr[2]
	default:
		return nil, errInvalidSpec(raw)
	}
	if convertTargetToBackslash {
		spec.Target = strings.ReplaceAll(spec.Target, `/`, `\`)
	}

	if !windowsValidMountMode(mode) {
		return nil, errInvalidMode(mode)
	}

	spec.Type = windowsDetectMountType(spec.Source)
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

	mp, err := p.parseMountSpec(spec, convertTargetToBackslash, additionalValidators...)
	if mp != nil {
		mp.Mode = mode
	}
	if err != nil {
		err = fmt.Errorf("%v: %v", errInvalidSpec(raw), err)
	}
	return mp, err
}

func (p *windowsParser) ParseMountSpec(cfg mount.Mount) (*MountPoint, error) {
	return p.parseMountSpec(cfg, true, windowsValidators)
}

func (p *windowsParser) parseMountSpec(cfg mount.Mount, convertTargetToBackslash bool, additionalValidators ...mountValidator) (*MountPoint, error) {
	if err := p.validateMountConfigReg(&cfg, additionalValidators...); err != nil {
		return nil, err
	}
	mp := &MountPoint{
		RW:          !cfg.ReadOnly,
		Destination: cfg.Target,
		Type:        cfg.Type,
		Spec:        cfg,
	}
	if convertTargetToBackslash {
		mp.Destination = strings.ReplaceAll(cfg.Target, `/`, `\`)
	}

	switch cfg.Type {
	case mount.TypeVolume:
		if cfg.Source != "" {
			// non-anonymous volume
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
		mp.Source = strings.ReplaceAll(cfg.Source, `/`, `\`)
	case mount.TypeNamedPipe:
		mp.Source = strings.ReplaceAll(cfg.Source, `/`, `\`)
	default:
		// TODO(thaJeztah): make switch exhaustive: anything to do for mount.TypeTmpfs, mount.TypeCluster, mount.TypeImage ?
	}
	// cleanup trailing `\` except for root drives like `c:\` or UNC root drives like `\\?\c:\`
	if len(mp.Source) > 3 && mp.Source[len(mp.Source)-1] == '\\' {
		// Verify this is not a UNC root drive (e.g. \\?\c:\) which requires the trailing slash
		isUNCRoot := len(mp.Source) == 7 && strings.HasPrefix(mp.Source, `\\?\`) && mp.Source[5] == ':'
		if !isUNCRoot {
			mp.Source = mp.Source[:len(mp.Source)-1]
		}
	}
	if len(mp.Destination) > 3 && mp.Destination[len(mp.Destination)-1] == '\\' {
		mp.Destination = mp.Destination[:len(mp.Destination)-1]
	}
	return mp, nil
}

func (p *windowsParser) ParseVolumesFrom(spec string) (string, string, error) {
	if spec == "" {
		return "", "", errors.New("volumes-from specification cannot be an empty string")
	}

	id, mode, _ := strings.Cut(spec, ":")
	if mode == "" {
		return id, "rw", nil
	}

	if !windowsValidMountMode(mode) {
		return "", "", errInvalidMode(mode)
	}

	// Do not allow copy modes on volumes-from
	if _, isSet := getCopyMode(mode, p.DefaultCopyMode()); isSet {
		return "", "", errInvalidMode(mode)
	}
	return id, mode, nil
}

func (p *windowsParser) DefaultPropagationMode() mount.Propagation {
	return ""
}

func (p *windowsParser) ConvertTmpfsOptions(opt *mount.TmpfsOptions, readOnly bool) (string, error) {
	return "", errors.New("windows does not support tmpfs")
}

func (p *windowsParser) DefaultCopyMode() bool {
	return false
}

func (p *windowsParser) IsBackwardCompatible(m *MountPoint) bool {
	return false
}

func (p *windowsParser) ValidateTmpfsMountDestination(dest string) error {
	return errors.New("windows does not support tmpfs")
}

func (p *windowsParser) HasResource(m *MountPoint, absolutePath string) bool {
	return false
}

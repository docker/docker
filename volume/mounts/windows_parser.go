package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stringid"
)

type windowsParser struct {
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
	rxReservedNames = `(con)|(prn)|(nul)|(aux)|(com[1-9])|(lpt[1-9])`

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

	rxLCOWDestination = `(?P<destination>/(?:[^\\/:*?"<>\r\n]+[/]?)*)`
	// Destination (aka container path):
	//    -  Variation on hostdir but can be a drive followed by colon as well
	//    -  If a path, must be absolute. Can include spaces
	//    -  Drive cannot be c: (explicitly checked in code, not RegEx)

	// rxMode is the regex expression for the mode of the mount
	// Mode (optional):
	//    -  Hopefully self explanatory in comparison to above regex's.
	//    -  Colon is not in the capture group
	rxMode = `(:(?P<mode>(?i)ro|rw))?`
)

type mountValidator func(mnt *mount.Mount) error

func windowsSplitRawSpec(raw, destRegex string) ([]string, error) {
	specExp := regexp.MustCompile(`^` + rxSource + destRegex + rxMode + `$`)
	match := specExp.FindStringSubmatch(strings.ToLower(raw))

	// Must have something back
	if len(match) == 0 {
		return nil, errInvalidSpec(raw)
	}

	var split []string
	matchgroups := make(map[string]string)
	// Pull out the sub expressions from the named capture groups
	for i, name := range specExp.SubexpNames() {
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
		volExp := regexp.MustCompile(`^` + rxName + `$`)
		reservedNameExp := regexp.MustCompile(`^` + rxReservedNames + `$`)

		if volExp.MatchString(matchgroups["destination"]) {
			if reservedNameExp.MatchString(matchgroups["destination"]) {
				return nil, fmt.Errorf("volume name %q cannot be a reserved word for Windows filenames", matchgroups["destination"])
			}
		} else {

			exists, isDir, _ := currentFileInfoProvider.fileInfo(matchgroups["destination"])
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
	return rwModes[strings.ToLower(mode)]
}
func windowsValidateNotRoot(p string) error {
	p = strings.ToLower(strings.Replace(p, `/`, `\`, -1))
	if p == "c:" || p == `c:\` {
		return fmt.Errorf("destination path cannot be `c:` or `c:\\`: %v", p)
	}
	return nil
}

var windowsSpecificValidators mountValidator = func(mnt *mount.Mount) error {
	return windowsValidateNotRoot(mnt.Target)
}

func windowsValidateRegex(p, r string) error {
	if regexp.MustCompile(`^` + r + `$`).MatchString(strings.ToLower(p)) {
		return nil
	}
	return fmt.Errorf("invalid mount path: '%s'", p)
}
func windowsValidateAbsolute(p string) error {
	if err := windowsValidateRegex(p, rxDestination); err != nil {
		return fmt.Errorf("invalid mount path: '%s' mount path must be absolute", p)
	}
	return nil
}

func windowsDetectMountType(p string) mount.Type {
	if strings.HasPrefix(p, `\\.\pipe\`) {
		return mount.TypeNamedPipe
	} else if regexp.MustCompile(`^` + rxHostDir + `$`).MatchString(p) {
		return mount.TypeBind
	} else {
		return mount.TypeVolume
	}
}

func (p *windowsParser) ReadWrite(mode string) bool {
	return strings.ToLower(mode) != "ro"
}

// IsVolumeNameValid checks a volume name in a platform specific manner.
func (p *windowsParser) ValidateVolumeName(name string) error {
	nameExp := regexp.MustCompile(`^` + rxName + `$`)
	if !nameExp.MatchString(name) {
		return errors.New("invalid volume name")
	}
	nameExp = regexp.MustCompile(`^` + rxReservedNames + `$`)
	if nameExp.MatchString(name) {
		return fmt.Errorf("volume name %q cannot be a reserved word for Windows filenames", name)
	}
	return nil
}
func (p *windowsParser) ValidateMountConfig(mnt *mount.Mount) error {
	return p.validateMountConfigReg(mnt, rxDestination, windowsSpecificValidators)
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

func (p *windowsParser) validateMountConfigReg(mnt *mount.Mount, destRegex string, additionalValidators ...mountValidator) error {

	for _, v := range additionalValidators {
		if err := v(mnt); err != nil {
			return &errMountConfig{mnt, err}
		}
	}
	if len(mnt.Target) == 0 {
		return &errMountConfig{mnt, errMissingField("Target")}
	}

	if err := windowsValidateRegex(mnt.Target, destRegex); err != nil {
		return &errMountConfig{mnt, err}
	}

	switch mnt.Type {
	case mount.TypeBind:
		if len(mnt.Source) == 0 {
			return &errMountConfig{mnt, errMissingField("Source")}
		}
		// Don't error out just because the propagation mode is not supported on the platform
		if opts := mnt.BindOptions; opts != nil {
			if len(opts.Propagation) > 0 {
				return &errMountConfig{mnt, fmt.Errorf("invalid propagation mode: %s", opts.Propagation)}
			}
		}
		if mnt.VolumeOptions != nil {
			return &errMountConfig{mnt, errExtraField("VolumeOptions")}
		}

		if err := windowsValidateAbsolute(mnt.Source); err != nil {
			return &errMountConfig{mnt, err}
		}

		exists, isdir, err := currentFileInfoProvider.fileInfo(mnt.Source)
		if err != nil {
			return &errMountConfig{mnt, err}
		}
		if !exists {
			return &errMountConfig{mnt, errBindSourceDoesNotExist(mnt.Source)}
		}
		if !isdir {
			return &errMountConfig{mnt, fmt.Errorf("source path must be a directory")}
		}

	case mount.TypeVolume:
		if mnt.BindOptions != nil {
			return &errMountConfig{mnt, errExtraField("BindOptions")}
		}

		if len(mnt.Source) == 0 && mnt.ReadOnly {
			return &errMountConfig{mnt, fmt.Errorf("must not set ReadOnly mode when using anonymous volumes")}
		}

		if len(mnt.Source) != 0 {
			if err := p.ValidateVolumeName(mnt.Source); err != nil {
				return &errMountConfig{mnt, err}
			}
		}
	case mount.TypeNamedPipe:
		if len(mnt.Source) == 0 {
			return &errMountConfig{mnt, errMissingField("Source")}
		}

		if mnt.BindOptions != nil {
			return &errMountConfig{mnt, errExtraField("BindOptions")}
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
	return p.parseMountRaw(raw, volumeDriver, rxDestination, true, windowsSpecificValidators)
}

func (p *windowsParser) parseMountRaw(raw, volumeDriver, destRegex string, convertTargetToBackslash bool, additionalValidators ...mountValidator) (*MountPoint, error) {
	arr, err := windowsSplitRawSpec(raw, destRegex)
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
		if windowsValidMountMode(arr[1]) {
			// Destination + Mode is not a valid volume - volumes
			// cannot include a mode. e.g. /foo:rw
			return nil, errInvalidSpec(raw)
		}
		// Host Source Path or Name + Destination
		spec.Source = strings.Replace(arr[0], `/`, `\`, -1)
		spec.Target = arr[1]
	case 3:
		// HostSourcePath+DestinationPath+Mode
		spec.Source = strings.Replace(arr[0], `/`, `\`, -1)
		spec.Target = arr[1]
		mode = arr[2]
	default:
		return nil, errInvalidSpec(raw)
	}
	if convertTargetToBackslash {
		spec.Target = strings.Replace(spec.Target, `/`, `\`, -1)
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

	mp, err := p.parseMountSpec(spec, destRegex, convertTargetToBackslash, additionalValidators...)
	if mp != nil {
		mp.Mode = mode
	}
	if err != nil {
		err = fmt.Errorf("%v: %v", errInvalidSpec(raw), err)
	}
	return mp, err
}

func (p *windowsParser) ParseMountSpec(cfg mount.Mount) (*MountPoint, error) {
	return p.parseMountSpec(cfg, rxDestination, true, windowsSpecificValidators)
}
func (p *windowsParser) parseMountSpec(cfg mount.Mount, destRegex string, convertTargetToBackslash bool, additionalValidators ...mountValidator) (*MountPoint, error) {
	if err := p.validateMountConfigReg(&cfg, destRegex, additionalValidators...); err != nil {
		return nil, err
	}
	mp := &MountPoint{
		RW:          !cfg.ReadOnly,
		Destination: cfg.Target,
		Type:        cfg.Type,
		Spec:        cfg,
	}
	if convertTargetToBackslash {
		mp.Destination = strings.Replace(cfg.Target, `/`, `\`, -1)
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
			if cfg.VolumeOptions.SubPath != "" {
				mp.SubPath = strings.Replace(cfg.VolumeOptions.SubPath, `/`, `\`, -1)
			}
		}
	case mount.TypeBind:
		mp.Source = strings.Replace(cfg.Source, `/`, `\`, -1)
	case mount.TypeNamedPipe:
		mp.Source = strings.Replace(cfg.Source, `/`, `\`, -1)
	}
	// cleanup trailing `\` except for paths like `c:\`
	if len(mp.Source) > 3 && mp.Source[len(mp.Source)-1] == '\\' {
		mp.Source = mp.Source[:len(mp.Source)-1]
	}
	if len(mp.Destination) > 3 && mp.Destination[len(mp.Destination)-1] == '\\' {
		mp.Destination = mp.Destination[:len(mp.Destination)-1]
	}
	return mp, nil
}

func (p *windowsParser) ParseVolumesFrom(spec string) (string, string, error) {
	if len(spec) == 0 {
		return "", "", fmt.Errorf("volumes-from specification cannot be an empty string")
	}

	specParts := strings.SplitN(spec, ":", 2)
	id := specParts[0]
	mode := "rw"

	if len(specParts) == 2 {
		mode = specParts[1]
		if !windowsValidMountMode(mode) {
			return "", "", errInvalidMode(mode)
		}

		// Do not allow copy modes on volumes-from
		if _, isSet := getCopyMode(mode, p.DefaultCopyMode()); isSet {
			return "", "", errInvalidMode(mode)
		}
	}
	return id, mode, nil
}

func (p *windowsParser) DefaultPropagationMode() mount.Propagation {
	return mount.Propagation("")
}

func (p *windowsParser) ConvertTmpfsOptions(opt *mount.TmpfsOptions, readOnly bool) (string, error) {
	return "", fmt.Errorf("%s does not support tmpfs", runtime.GOOS)
}
func (p *windowsParser) DefaultCopyMode() bool {
	return false
}
func (p *windowsParser) IsBackwardCompatible(m *MountPoint) bool {
	return false
}

func (p *windowsParser) ValidateTmpfsMountDestination(dest string) error {
	return errors.New("Platform does not support tmpfs")
}

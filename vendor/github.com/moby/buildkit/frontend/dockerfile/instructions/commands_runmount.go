// +build dfrunmount

package instructions

import (
	"encoding/csv"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

const MountTypeBind = "bind"
const MountTypeCache = "cache"
const MountTypeTmpfs = "tmpfs"
const MountTypeSecret = "secret"
const MountTypeSSH = "ssh"

var allowedMountTypes = map[string]struct{}{
	MountTypeBind:   {},
	MountTypeCache:  {},
	MountTypeTmpfs:  {},
	MountTypeSecret: {},
	MountTypeSSH:    {},
}

const MountSharingShared = "shared"
const MountSharingPrivate = "private"
const MountSharingLocked = "locked"

var allowedSharingTypes = map[string]struct{}{
	MountSharingShared:  {},
	MountSharingPrivate: {},
	MountSharingLocked:  {},
}

type mountsKeyT string

var mountsKey = mountsKeyT("dockerfile/run/mounts")

func init() {
	parseRunPreHooks = append(parseRunPreHooks, runMountPreHook)
	parseRunPostHooks = append(parseRunPostHooks, runMountPostHook)
}

func isValidMountType(s string) bool {
	if s == "secret" {
		if !isSecretMountsSupported() {
			return false
		}
	}
	if s == "ssh" {
		if !isSSHMountsSupported() {
			return false
		}
	}
	_, ok := allowedMountTypes[s]
	return ok
}

func runMountPreHook(cmd *RunCommand, req parseRequest) error {
	st := &mountState{}
	st.flag = req.flags.AddStrings("mount")
	cmd.setExternalValue(mountsKey, st)
	return nil
}

func runMountPostHook(cmd *RunCommand, req parseRequest) error {
	st := getMountState(cmd)
	if st == nil {
		return errors.Errorf("no mount state")
	}
	var mounts []*Mount
	for _, str := range st.flag.StringValues {
		m, err := parseMount(str)
		if err != nil {
			return err
		}
		mounts = append(mounts, m)
	}
	st.mounts = mounts
	return nil
}

func getMountState(cmd *RunCommand) *mountState {
	v := cmd.getExternalValue(mountsKey)
	if v == nil {
		return nil
	}
	return v.(*mountState)
}

func GetMounts(cmd *RunCommand) []*Mount {
	return getMountState(cmd).mounts
}

type mountState struct {
	flag   *Flag
	mounts []*Mount
}

type Mount struct {
	Type         string
	From         string
	Source       string
	Target       string
	ReadOnly     bool
	CacheID      string
	CacheSharing string
	Required     bool
	Mode         *uint64
	UID          *uint64
	GID          *uint64
}

func parseMount(value string) (*Mount, error) {
	csvReader := csv.NewReader(strings.NewReader(value))
	fields, err := csvReader.Read()
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse csv mounts")
	}

	m := &Mount{Type: MountTypeBind}

	roAuto := true

	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		key := strings.ToLower(parts[0])

		if len(parts) == 1 {
			switch key {
			case "readonly", "ro":
				m.ReadOnly = true
				roAuto = false
				continue
			case "readwrite", "rw":
				m.ReadOnly = false
				roAuto = false
				continue
			case "required":
				if m.Type == "secret" || m.Type == "ssh" {
					m.Required = true
					continue
				}
			}
		}

		if len(parts) != 2 {
			return nil, errors.Errorf("invalid field '%s' must be a key=value pair", field)
		}

		value := parts[1]
		switch key {
		case "type":
			if !isValidMountType(strings.ToLower(value)) {
				return nil, errors.Errorf("unsupported mount type %q", value)
			}
			m.Type = strings.ToLower(value)
		case "from":
			m.From = value
		case "source", "src":
			m.Source = value
		case "target", "dst", "destination":
			m.Target = value
		case "readonly", "ro":
			m.ReadOnly, err = strconv.ParseBool(value)
			if err != nil {
				return nil, errors.Errorf("invalid value for %s: %s", key, value)
			}
			roAuto = false
		case "readwrite", "rw":
			rw, err := strconv.ParseBool(value)
			if err != nil {
				return nil, errors.Errorf("invalid value for %s: %s", key, value)
			}
			m.ReadOnly = !rw
			roAuto = false
		case "id":
			m.CacheID = value
		case "sharing":
			if _, ok := allowedSharingTypes[strings.ToLower(value)]; !ok {
				return nil, errors.Errorf("unsupported sharing value %q", value)
			}
			m.CacheSharing = strings.ToLower(value)
		case "mode":
			mode, err := strconv.ParseUint(value, 8, 32)
			if err != nil {
				return nil, errors.Errorf("invalid value %s for mode", value)
			}
			m.Mode = &mode
		case "uid":
			uid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return nil, errors.Errorf("invalid value %s for uid", value)
			}
			m.UID = &uid
		case "gid":
			gid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return nil, errors.Errorf("invalid value %s for gid", value)
			}
			m.GID = &gid
		default:
			return nil, errors.Errorf("unexpected key '%s' in '%s'", key, field)
		}
	}

	fileInfoAllowed := m.Type == MountTypeSecret || m.Type == MountTypeSSH

	if m.Mode != nil && !fileInfoAllowed {
		return nil, errors.Errorf("mode not allowed for %q type mounts")
	}

	if m.UID != nil && !fileInfoAllowed {
		return nil, errors.Errorf("uid not allowed for %q type mounts")
	}

	if m.GID != nil && !fileInfoAllowed {
		return nil, errors.Errorf("gid not allowed for %q type mounts")
	}

	if roAuto {
		if m.Type == MountTypeCache || m.Type == MountTypeTmpfs {
			m.ReadOnly = false
		} else {
			m.ReadOnly = true
		}
	}

	if m.CacheSharing != "" && m.Type != MountTypeCache {
		return nil, errors.Errorf("invalid cache sharing set for %v mount", m.Type)
	}

	if m.Type == MountTypeSecret {
		if m.From != "" {
			return nil, errors.Errorf("secret mount should not have a from")
		}
		if m.CacheSharing != "" {
			return nil, errors.Errorf("secret mount should not define sharing")
		}
		if m.Source == "" && m.Target == "" && m.CacheID == "" {
			return nil, errors.Errorf("invalid secret mount. one of source, target required")
		}
		if m.Source != "" && m.CacheID != "" {
			return nil, errors.Errorf("both source and id can't be set")
		}
	}

	return m, nil
}

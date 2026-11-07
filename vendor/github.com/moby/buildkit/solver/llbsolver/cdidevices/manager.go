package cdidevices

import (
	"context"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/locker"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/pkg/parser"
)

const (
	deviceAnnotationClass     = "org.mobyproject.buildkit.device.class"
	deviceAnnotationAutoAllow = "org.mobyproject.buildkit.device.autoallow"
)

var installers = map[string]Setup{}

type Setup interface {
	Validate() error
	Run(ctx context.Context) error
}

// Register registers new setup for a device.
func Register(name string, setup Setup) {
	installers[name] = setup
}

type Device struct {
	Name        string
	AutoAllow   bool
	OnDemand    bool
	Annotations map[string]string
}

type Manager struct {
	cache         *cdi.Cache
	locker        *locker.Locker
	autoAllowedMu sync.RWMutex
	autoAllowed   map[string]struct{}
}

func NewManager(cache *cdi.Cache, autoAllowed []string) *Manager {
	m := make(map[string]struct{})
	for _, d := range autoAllowed {
		m[d] = struct{}{}
	}
	return &Manager{
		cache:       cache,
		locker:      locker.New(),
		autoAllowed: m,
	}
}

func (m *Manager) isAutoAllowed(kind, name string, annotations map[string]string) bool {
	m.autoAllowedMu.RLock()
	defer m.autoAllowedMu.RUnlock()
	if _, ok := m.autoAllowed[name]; ok {
		return true
	}
	if _, ok := m.autoAllowed[kind]; ok {
		return true
	}
	if v, ok := annotations[deviceAnnotationAutoAllow]; ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			return true
		}
	}
	return false
}

func (m *Manager) ListDevices() []Device {
	devs := m.cache.ListDevices()
	out := make([]Device, 0, len(devs))
	kinds := make(map[string]struct{})
	for _, dev := range devs {
		kind, _, _ := strings.Cut(dev, "=")
		dd := m.cache.GetDevice(dev)
		annotations := deviceAnnotations(dd)
		out = append(out, Device{
			Name:        dev,
			AutoAllow:   m.isAutoAllowed(kind, dev, annotations),
			Annotations: annotations,
		})
		kinds[kind] = struct{}{}
	}

	for k, setup := range installers {
		if _, ok := kinds[k]; ok {
			continue
		}
		if err := setup.Validate(); err != nil {
			continue
		}
		out = append(out, Device{
			Name:      k,
			OnDemand:  true,
			AutoAllow: true,
		})
	}

	return out
}

func (m *Manager) GetDevice(name string) Device {
	kind, _, _ := strings.Cut(name, "=")
	annotations := deviceAnnotations(m.cache.GetDevice(name))
	return Device{
		Name:        name,
		AutoAllow:   m.isAutoAllowed(kind, name, annotations),
		Annotations: annotations,
	}
}

func (m *Manager) Refresh() error {
	return m.cache.Refresh()
}

func (m *Manager) InjectDevices(spec *specs.Spec, devs ...*pb.CDIDevice) error {
	pdevs, err := m.FindDevices(devs...)
	if err != nil {
		return err
	} else if len(pdevs) == 0 {
		return nil
	}
	bklog.G(context.TODO()).Debugf("Injecting devices %v", pdevs)
	_, err = m.cache.InjectDevices(spec, pdevs...)
	return err
}

func (m *Manager) FindDevices(devs ...*pb.CDIDevice) ([]string, error) {
	var out []string
	if len(devs) == 0 {
		return out, nil
	}
	list := m.cache.ListDevices()
	for _, dev := range devs {
		if dev == nil {
			continue
		}
		pdev, err := m.parseDevice(dev, list)
		if err != nil {
			return nil, err
		} else if len(pdev) == 0 {
			continue
		}
		out = append(out, pdev...)
	}
	return dedupSlice(out), nil
}

// MatchDevices returns the registered devices that the request resolves to,
// including matches by class annotation. Unlike FindDevices it does not fail
// or warn when nothing matches.
func (m *Manager) MatchDevices(dev *pb.CDIDevice) []string {
	return m.matchDevice(dev, m.cache.ListDevices())
}

func (m *Manager) parseDevice(dev *pb.CDIDevice, all []string) ([]string, error) {
	out := m.matchDevice(dev, all)
	if len(out) == 0 {
		if !dev.Optional {
			return nil, errors.Errorf("required device %q is not registered", dev.Name)
		}
		bklog.G(context.TODO()).Warnf("Optional device %q is not registered", dev.Name)
	}
	return out, nil
}

func (m *Manager) matchDevice(dev *pb.CDIDevice, all []string) []string {
	var out []string

	kind, name, _ := strings.Cut(dev.Name, "=")

	vendor, _ := parser.ParseQualifier(kind)
	if vendor != "" {
		switch name {
		case "":
			// first device of kind if no name is specified
			for _, d := range all {
				if strings.HasPrefix(d, kind+"=") {
					out = append(out, d)
					break
				}
			}
		case "*":
			// all devices of kind if the name is a wildcard
			for _, d := range all {
				if strings.HasPrefix(d, kind+"=") {
					out = append(out, d)
				}
			}
		default:
			// the specified device
			for _, d := range all {
				if d == dev.Name {
					out = append(out, d)
					break
				}
			}
		}
	}

	// check class annotation if device qualifier invalid or no device found
	if vendor == "" || len(out) == 0 {
		for _, d := range all {
			if a := deviceAnnotations(m.cache.GetDevice(d)); a != nil {
				if class, ok := a[deviceAnnotationClass]; ok && class == dev.Name {
					out = append(out, d)
				}
			}
		}
	}

	return out
}

func (m *Manager) hasDevice(name string) bool {
	for _, d := range m.cache.ListDevices() {
		kind, _, _ := strings.Cut(d, "=")
		if kind == name {
			return true
		}
	}
	return false
}

func (m *Manager) OnDemandInstaller(name string) (func(context.Context) error, bool) {
	name, _, _ = strings.Cut(name, "=")

	installer, ok := installers[name]
	if !ok {
		return nil, false
	}

	if m.hasDevice(name) {
		return nil, false
	}

	return func(ctx context.Context) error {
		m.locker.Lock(name)
		defer m.locker.Unlock(name)

		if m.hasDevice(name) {
			return nil
		}

		if err := installer.Validate(); err != nil {
			return errors.Wrapf(err, "failed to find preconditions for %s device", name)
		}

		if err := installer.Run(ctx); err != nil {
			return errors.Wrapf(err, "failed to create %s device", name)
		}

		if err := m.cache.Refresh(); err != nil {
			return errors.Wrapf(err, "failed to refresh CDI cache")
		}

		// TODO: this needs to be set as annotation to survive reboot
		m.autoAllowedMu.Lock()
		m.autoAllowed[name] = struct{}{}
		m.autoAllowedMu.Unlock()

		return nil
	}, true
}

func deviceAnnotations(dev *cdi.Device) map[string]string {
	if dev == nil {
		return nil
	}
	out := make(map[string]string)
	// spec annotations
	maps.Copy(out, dev.GetSpec().Annotations)
	// device annotations
	maps.Copy(out, dev.Annotations)
	return out
}

func dedupSlice(s []string) []string {
	if len(s) == 0 {
		return s
	}
	var res []string
	seen := make(map[string]struct{})
	for _, val := range s {
		if _, ok := seen[val]; !ok {
			res = append(res, val)
			seen[val] = struct{}{}
		}
	}
	return res
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/internal/capabilities"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// TODO: nvidia should not be hard-coded, and should be a device plugin instead on the daemon object.
// TODO: add list of device capabilities in daemon/node info

var errConflictCountDeviceIDs = errors.New("cannot set both Count and DeviceIDs on device request")

const (
	nvidiaContainerRuntimeHookExecutableName = "nvidia-container-runtime-hook"
	nvidiaCDIHookExecutableName              = "nvidia-cdi-hook"
)

// These are NVIDIA-specific capabilities stolen from github.com/containerd/containerd/contrib/nvidia.allCaps
var allNvidiaCaps = map[string]struct{}{
	"compute":  {},
	"compat32": {},
	"graphics": {},
	"utility":  {},
	"video":    {},
	"display":  {},
}

func getNVIDIADeviceDrivers() map[string]*deviceDriver {
	var composite firstSuccessfulUpdater
	nvidiaDrivers := make(map[string]*deviceDriver)

	if _, err := exec.LookPath(nvidiaContainerRuntimeHookExecutableName); err == nil {
		// Register a driver specific to the nvidia-container-runtime-hook if present.
		// This has no capabilities associated to not inadvertently match requests.
		runtimeHookDeviceDriver := &deviceDriver{
			updateSpec: injectNVIDIARuntimeHook,
		}
		nvidiaDrivers["nvidia.runtime-hook"] = runtimeHookDeviceDriver
		composite = append(composite, runtimeHookDeviceDriver.updateSpec)
	}

	if _, err := exec.LookPath(nvidiaCDIHookExecutableName); err == nil {
		// Register a driver specific to CDI if present.
		// This has no capabilities associated to not inadvertently match requests.
		cdiDeviceDriver := &deviceDriver{
			updateSpec: (&cdiDeviceInjector{
				defaultCDIDeviceKind: "nvidia.com/gpu",
			}).injectDevices,
		}
		nvidiaDrivers["nvidia.cdi"] = cdiDeviceDriver
		composite = append(composite, cdiDeviceDriver.updateSpec)
	}

	if len(nvidiaDrivers) == 0 {
		return nil
	}

	// We associate all NVIDIA capabilities with the composite updater
	capset := capabilities.Set{"gpu": struct{}{}, "nvidia": struct{}{}}
	for c := range allNvidiaCaps {
		capset[c] = struct{}{}
	}
	nvidiaDrivers["nvidia"] = &deviceDriver{
		capset:     capset,
		updateSpec: composite.updateSpec,
	}

	return nvidiaDrivers
}

// specUpdaters refer to a list of functions used updated an OCI spec for a
// given device instance.
type firstSuccessfulUpdater []func(*specs.Spec, *deviceInstance) error

// updateSpec returns on the first successful spec update.
func (us firstSuccessfulUpdater) updateSpec(s *specs.Spec, dev *deviceInstance) error {
	var errs []error
	for _, u := range us {
		if u == nil {
			continue
		}
		if err := u(s, dev); err != nil {
			errs = append(errs, err)
			continue
		}
		if len(errs) > 0 {
			log.G(context.TODO()).WithError(errors.Join(errs...)).Warning("Ignoring previous errors updating spec")
		}
		return nil
	}
	return errors.Join(errs...)
}

// injectNVIDIARuntimeHook handles requests for NVIDIA GPUs.
// This is done by updating the OCI runtime spec to include the correct value
// for the NVIDIA_VISIBLE_DEVICES environment variable and injecting the
// NVIDIA Container Runtime Hook as a container prestart hook.
func injectNVIDIARuntimeHook(s *specs.Spec, dev *deviceInstance) error {
	deviceIDs, err := getRequestedDevicesIDs(dev.req)
	if err != nil {
		return err
	}
	if len(deviceIDs) == 0 {
		return nil
	}
	s.Process.Env = append(s.Process.Env, "NVIDIA_VISIBLE_DEVICES="+strings.Join(deviceIDs, ","))

	var nvidiaCaps []string
	// req.Capabilities contains device capabilities, some but not all are NVIDIA driver capabilities.
	for _, c := range dev.selectedCaps {
		if _, isNvidiaCap := allNvidiaCaps[c]; isNvidiaCap {
			nvidiaCaps = append(nvidiaCaps, c)
			continue
		}
		// TODO: nvidia.WithRequiredCUDAVersion
		// for now we let the prestart hook verify cuda versions but errors are not pretty.
	}

	if nvidiaCaps != nil {
		s.Process.Env = append(s.Process.Env, "NVIDIA_DRIVER_CAPABILITIES="+strings.Join(nvidiaCaps, ","))
	}

	path, err := exec.LookPath(nvidiaContainerRuntimeHookExecutableName)
	if err != nil {
		return err
	}

	if s.Hooks == nil {
		s.Hooks = &specs.Hooks{}
	}

	// This implementation uses prestart hooks, which are deprecated.
	// CreateRuntime is the closest equivalent, and executed in the same
	// locations as prestart-hooks, but depending on what these hooks do,
	// possibly one of the other hooks could be used instead (such as
	// CreateContainer or StartContainer).
	s.Hooks.Prestart = append(s.Hooks.Prestart, specs.Hook{ //nolint:staticcheck // FIXME(thaJeztah); replace prestart hook with a non-deprecated one.
		Path: path,
		Args: []string{
			nvidiaContainerRuntimeHookExecutableName,
			"prestart",
		},
		Env: os.Environ(),
	})

	return nil
}

// getRequestedDeviceIDs returns the list of requested devices by ID based on
// the device request.
func getRequestedDevicesIDs(req container.DeviceRequest) ([]string, error) {
	if req.Count != 0 && len(req.DeviceIDs) > 0 {
		return nil, errConflictCountDeviceIDs
	}

	switch {
	case len(req.DeviceIDs) > 0:
		return req.DeviceIDs, nil
	case req.Count > 0:
		return countToDevices(req.Count), nil
	case req.Count < 0:
		return []string{"all"}, nil
	case req.Count == 0:
		return nil, nil
	}
	return nil, nil
}

// countToDevices returns the list 0, 1, ... count-1 of deviceIDs.
func countToDevices(count int) []string {
	devices := make([]string, count)
	for i := range devices {
		devices[i] = strconv.Itoa(i)
	}
	return devices
}

// A cdiDeviceInjector is used to map regular device requests to CDI device
// requests.
type cdiDeviceInjector struct {
	defaultCDIDeviceKind string
}

// injectDevices converts an incoming device request to a request for devices
// using CDI.
// The requested device IDs are converted to CDI device names if required using
// the specified default kind.
func (i *cdiDeviceInjector) injectDevices(s *specs.Spec, dev *deviceInstance) error {
	deviceIDs, err := getRequestedDevicesIDs(dev.req)
	if err != nil {
		return err
	}
	if len(deviceIDs) == 0 {
		return nil
	}

	// If the cdi device driver is not available then we return an error.
	cdiDeviceDriver := deviceDrivers["cdi"]
	if cdiDeviceDriver == nil {
		return fmt.Errorf("no CDI device driver registered: %w", incompatibleDeviceRequest{dev.req.Driver, dev.req.Capabilities})
	}

	var cdiDeviceIDs []string
	for _, deviceID := range deviceIDs {
		cdiDeviceIDs = append(cdiDeviceIDs, i.normalizeDeviceID(deviceID))
	}

	// We construct a device instance using the CDI device IDs and forward this
	// to the cdiDeviceDriver.
	return cdiDeviceDriver.updateSpec(s, &deviceInstance{
		req: container.DeviceRequest{
			Driver:       dev.req.Driver,
			DeviceIDs:    cdiDeviceIDs,
			Capabilities: dev.req.Capabilities,
		},
		selectedCaps: nil,
	})
}

// normalizeDeviceID ensures that the specified deviceID is a fully-qualified
// CDI device name.
// If the deviceID is already a fully-qualified CDI device name it is returned
// as-is, otherwise, the default CDI device kind (vendor/class) is used to
// construct a fully qualified CDI device name.
func (i *cdiDeviceInjector) normalizeDeviceID(deviceID string) string {
	// if deviceID is of the form vendor.com/class=name, we return it as-is.
	// TODO: We should ideally use the parser from the tags.cncf.io/cdi packages.
	if _, _, ok := strings.Cut(deviceID, "="); ok {
		return deviceID
	}

	return i.defaultCDIDeviceKind + "=" + deviceID
}

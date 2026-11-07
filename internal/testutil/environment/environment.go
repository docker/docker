package environment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/internal/testutil/fixtures/load"
	"github.com/pkg/errors"
	"gotest.tools/v3/assert"
)

// Execution contains information about the current test execution and daemon
// under test
type Execution struct {
	client              client.APIClient
	DaemonInfo          system.Info
	DaemonMinAPIVersion string
	PlatformDefaults    PlatformDefaults
	protectedElements   protectedElements
}

// PlatformDefaults are defaults values for the platform of the daemon under test
type PlatformDefaults struct {
	BaseImage            string
	VolumesConfigPath    string
	ContainerStoragePath string
}

// New creates a new Execution struct
// This is configured using the env client (see client.FromEnv)
func New(ctx context.Context) (*Execution, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create client")
	}
	return FromClient(ctx, c)
}

// FromClient creates a new Execution environment from the passed in client
func FromClient(ctx context.Context, c *client.Client) (*Execution, error) {
	_, err := c.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to ping daemon to negotiate api version")
	}

	result, err := c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get info from daemon")
	}

	version, err := c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get version from daemon")
	}

	return &Execution{
		client:              c,
		DaemonInfo:          result.Info,
		DaemonMinAPIVersion: version.MinAPIVersion,
		PlatformDefaults:    getPlatformDefaults(result.Info),
		protectedElements:   newProtectedElements(),
	}, nil
}

func getPlatformDefaults(info system.Info) PlatformDefaults {
	volumesPath := filepath.Join(info.DockerRootDir, "volumes")
	containersPath := filepath.Join(info.DockerRootDir, "containers")

	switch info.OSType {
	case "linux":
		return PlatformDefaults{
			BaseImage:            "scratch",
			VolumesConfigPath:    toSlash(volumesPath),
			ContainerStoragePath: toSlash(containersPath),
		}
	case "windows":
		baseImage := "mcr.microsoft.com/windows/servercore:ltsc2022"
		if overrideBaseImage := os.Getenv("WINDOWS_BASE_IMAGE"); overrideBaseImage != "" {
			baseImage = overrideBaseImage
			if overrideBaseImageTag := os.Getenv("WINDOWS_BASE_IMAGE_TAG"); overrideBaseImageTag != "" {
				baseImage = baseImage + ":" + overrideBaseImageTag
			}
		}
		fmt.Println("INFO: Windows Base image is ", baseImage)
		return PlatformDefaults{
			BaseImage:            baseImage,
			VolumesConfigPath:    filepath.FromSlash(volumesPath),
			ContainerStoragePath: filepath.FromSlash(containersPath),
		}
	default:
		panic("unknown OSType for daemon: " + info.OSType)
	}
}

// Make sure in context of daemon, not the local platform. Note we can't
// use filepath.ToSlash here as that is a no-op on Unix.
func toSlash(path string) string {
	return strings.ReplaceAll(path, `\`, `/`)
}

// IsLocalDaemon is true if the daemon under test is on the same
// host as the test process.
//
// Deterministically working out the environment in which CI is running
// to evaluate whether the daemon is local or remote is not possible through
// a build tag.
//
// For example Windows to Linux CI under Jenkins tests the 64-bit
// Windows binary build with the daemon build tag, but calls a remote
// Linux daemon.
//
// We can't just say if Windows then assume the daemon is local as at
// some point, we will be testing the Windows CLI against a Windows daemon.
//
// Similarly, it will be perfectly valid to also run CLI tests from
// a Linux CLI (built with the daemon tag) against a Windows daemon.
func (e *Execution) IsLocalDaemon() bool {
	return os.Getenv("DOCKER_REMOTE_DAEMON") == ""
}

// IsRemoteDaemon is true if the daemon under test is on different host
// as the test process.
func (e *Execution) IsRemoteDaemon() bool {
	return !e.IsLocalDaemon()
}

// DaemonAPIVersion returns the negotiated daemon api version
func (e *Execution) DaemonAPIVersion() string {
	return e.APIClient().ClientVersion()
}

// Print the execution details to stdout
// TODO: print everything
func (e *Execution) Print() {
	if e.IsLocalDaemon() {
		fmt.Println("INFO: Testing against a local daemon")
	} else {
		fmt.Println("INFO: Testing against a remote daemon")
	}
}

// APIClient returns an APIClient connected to the daemon under test
func (e *Execution) APIClient() client.APIClient {
	return e.client
}

// IsUserNamespace returns whether the user namespace remapping is enabled
func (e *Execution) IsUserNamespace() bool {
	root := os.Getenv("DOCKER_REMAP_ROOT")
	return root != ""
}

// RuntimeIsWindowsContainerd returns whether the containerd runtime is used on Windows.
// It is true when either the legacy DOCKER_WINDOWS_CONTAINERD_RUNTIME=1 env
// var is set, or when the embedded-containerd feature is enabled via
// TEST_INTEGRATION_CONTAINERD_EMBEDDED (which also uses containerd).
func (e *Execution) RuntimeIsWindowsContainerd() bool {
	return os.Getenv("DOCKER_WINDOWS_CONTAINERD_RUNTIME") == "1" ||
		(runtime.GOOS == "windows" && os.Getenv("TEST_INTEGRATION_CONTAINERD_EMBEDDED") != "")
}

// IsRootless returns whether the rootless mode is enabled
func (e *Execution) IsRootless() bool {
	return os.Getenv("DOCKER_ROOTLESS") != ""
}

// IsNonRootClient returns whether the client is running as a non-root user.
// Not to be confused with [Execution.IsRootless], which returns whether the
// daemon is running as a non-root user.
func (e *Execution) IsNonRootClient() bool {
	return os.Geteuid() != 0
}

// IsUserNamespaceInKernel returns whether the kernel supports user namespaces
func (e *Execution) IsUserNamespaceInKernel() bool {
	if _, err := os.Stat("/proc/self/uid_map"); os.IsNotExist(err) {
		/*
		 * This kernel-provided file only exists if user namespaces are
		 * supported
		 */
		return false
	}

	// We need extra check on redhat based distributions
	if f, err := os.Open("/sys/module/user_namespace/parameters/enable"); err == nil {
		defer f.Close()
		b := make([]byte, 1)
		_, _ = f.Read(b)
		return string(b) != "N"
	}

	return true
}

// UsingSnapshotter returns whether containerd snapshotters are used for the
// tests by checking if the "TEST_INTEGRATION_USE_GRAPHDRIVER" is empty
func (e *Execution) UsingSnapshotter() bool {
	return os.Getenv("TEST_INTEGRATION_USE_GRAPHDRIVER") == ""
}

// HasExistingImage checks whether there is an image with the given reference.
// Note that this is done by filtering and then checking whether there were any
// results -- so ambiguous references might result in false-positives.
func (e *Execution) HasExistingImage(t testing.TB, reference string) bool {
	imageList, err := e.APIClient().ImageList(t.Context(), client.ImageListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("dangling", "false").
			Add("reference", reference),
	})
	assert.NilError(t, err, "failed to list images")

	return len(imageList.Items) > 0
}

// IsKernelModuleLoadable returns whether the given kernel module is loadable.
// Returns true when the module is already loaded or built-in into the kernel.
func (e *Execution) IsKernelModuleLoadable(module string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	// modprobe is aware of `/lib/modules/$(uname -r)/modules.builtin`,
	// so it does not fail for already built-in modules.
	return exec.Command("/sbin/modprobe", "-n", module).Run() == nil
}

// EnsureFrozenImagesLinux loads frozen test images into the daemon
// if they aren't already loaded
func EnsureFrozenImagesLinux(ctx context.Context, testEnv *Execution) error {
	if testEnv.DaemonInfo.OSType == "linux" {
		err := load.FrozenImagesLinux(ctx, testEnv.APIClient(), frozenImages...)
		if err != nil {
			return errors.Wrap(err, "error loading frozen images")
		}
	}
	return nil
}

// GitHubActions is true if test is executed on a GitHub Runner.
func (e *Execution) GitHubActions() bool {
	return os.Getenv("GITHUB_ACTIONS") != ""
}

// NotAmd64 returns true if the daemon's architecture is not amd64
func (e *Execution) NotAmd64() bool {
	return e.DaemonInfo.Architecture != "amd64"
}

// FirewallBackendDriver returns the value of FirewallBackend.Driver from
// system Info if set, else the empty string.
func (e *Execution) FirewallBackendDriver() string {
	if e.DaemonInfo.FirewallBackend == nil {
		return ""
	}
	return e.DaemonInfo.FirewallBackend.Driver
}

package environment // import "github.com/docker/docker/internal/test/environment"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/test"
	"github.com/docker/docker/internal/test/fixtures/load"
	"github.com/pkg/errors"
	"gotest.tools/assert"
)

// Execution contains information about the current test execution and daemon
// under test
type Execution struct {
	client            client.APIClient
	DaemonInfo        types.Info
	OSType            string
	PlatformDefaults  PlatformDefaults
	protectedElements protectedElements
}

// PlatformDefaults are defaults values for the platform of the daemon under test
type PlatformDefaults struct {
	BaseImage            string
	VolumesConfigPath    string
	ContainerStoragePath string
}

// New creates a new Execution struct
func New() (*Execution, error) {
	client, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create client")
	}

	info, err := client.Info(context.Background())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get info from daemon")
	}

	osType := getOSType(info)

	return &Execution{
		client:            client,
		DaemonInfo:        info,
		OSType:            osType,
		PlatformDefaults:  getPlatformDefaults(info, osType),
		protectedElements: newProtectedElements(),
	}, nil
}

func getOSType(info types.Info) string {
	// Docker EE does not set the OSType so allow the user to override this value.
	userOsType := os.Getenv("TEST_OSTYPE")
	if userOsType != "" {
		return userOsType
	}
	return info.OSType
}

func getPlatformDefaults(info types.Info, osType string) PlatformDefaults {
	volumesPath := filepath.Join(info.DockerRootDir, "volumes")
	containersPath := filepath.Join(info.DockerRootDir, "containers")

	switch osType {
	case "linux":
		return PlatformDefaults{
			BaseImage:            "scratch",
			VolumesConfigPath:    toSlash(volumesPath),
			ContainerStoragePath: toSlash(containersPath),
		}
	case "windows":
		baseImage := "microsoft/windowsservercore"
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
		panic(fmt.Sprintf("unknown OSType for daemon: %s", osType))
	}
}

// Make sure in context of daemon, not the local platform. Note we can't
// use filepath.FromSlash or ToSlash here as they are a no-op on Unix.
func toSlash(path string) string {
	return strings.Replace(path, `\`, `/`, -1)
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
	version, err := e.APIClient().ServerVersion(context.TODO())
	if err != nil {
		return ""
	}
	return version.APIVersion
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

// HasExistingImage checks whether there is an image with the given reference.
// Note that this is done by filtering and then checking whether there were any
// results -- so ambiguous references might result in false-positives.
func (e *Execution) HasExistingImage(t assert.TestingT, reference string) bool {
	if ht, ok := t.(test.HelperT); ok {
		ht.Helper()
	}
	client := e.APIClient()
	filter := filters.NewArgs()
	filter.Add("dangling", "false")
	filter.Add("reference", reference)
	imageList, err := client.ImageList(context.Background(), types.ImageListOptions{
		All:     true,
		Filters: filter,
	})
	assert.NilError(t, err, "failed to list images")

	return len(imageList) > 0
}

// EnsureFrozenImagesLinux loads frozen test images into the daemon
// if they aren't already loaded
func EnsureFrozenImagesLinux(testEnv *Execution) error {
	if testEnv.OSType == "linux" {
		err := load.FrozenImagesLinux(testEnv.APIClient(), frozenImages...)
		if err != nil {
			return errors.Wrap(err, "error loading frozen images")
		}
	}
	return nil
}

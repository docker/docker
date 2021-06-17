package system // import "github.com/docker/docker/pkg/system"

import "github.com/Microsoft/hcsshim/osversion"

var (
	// containerdRuntimeSupported determines if ContainerD should be the runtime.
	containerdRuntimeSupported = false
)

// InitContainerdRuntime sets whether to use ContainerD for runtime on Windows.
func InitContainerdRuntime(cdPath string) {
	if len(cdPath) > 0 {
		containerdRuntimeSupported = true
	}

	// Always use containerd runtime on Windows Server 2022 preview and later
	if len(cdPath) == 0 && osversion.Build() >= 20295 {
		containerdRuntimeSupported = true
	}
}

// ContainerdRuntimeSupported returns true if the use of ContainerD runtime is supported.
func ContainerdRuntimeSupported() bool {
	return containerdRuntimeSupported
}

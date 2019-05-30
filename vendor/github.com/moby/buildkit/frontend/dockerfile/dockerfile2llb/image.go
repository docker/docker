package dockerfile2llb

import (
	"time"

	"github.com/docker/docker/api/types/strslice"
	"github.com/moby/buildkit/util/system"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// HealthConfig holds configuration settings for the HEALTHCHECK feature.
type HealthConfig struct {
	// Test is the test to perform to check that the container is healthy.
	// An empty slice means to inherit the default.
	// The options are:
	// {} : inherit healthcheck
	// {"NONE"} : disable healthcheck
	// {"CMD", args...} : exec arguments directly
	// {"CMD-SHELL", command} : run command with system's default shell
	Test []string `json:",omitempty"`

	// Zero means to inherit. Durations are expressed as integer nanoseconds.
	Interval    time.Duration `json:",omitempty"` // Interval is the time to wait between checks.
	Timeout     time.Duration `json:",omitempty"` // Timeout is the time to wait before considering the check to have hung.
	StartPeriod time.Duration `json:",omitempty"` // The start period for the container to initialize before the retries starts to count down.

	// Retries is the number of consecutive failures needed to consider a container as unhealthy.
	// Zero means inherit.
	Retries int `json:",omitempty"`
}

// ImageConfig is a docker compatible config for an image
type ImageConfig struct {
	specs.ImageConfig

	Healthcheck *HealthConfig `json:",omitempty"` // Healthcheck describes how to check the container is healthy
	ArgsEscaped bool          `json:",omitempty"` // True if command is already escaped (Windows specific)

	//	NetworkDisabled bool                `json:",omitempty"` // Is network disabled
	//	MacAddress      string              `json:",omitempty"` // Mac Address of the container
	OnBuild     []string          // ONBUILD metadata that were defined on the image Dockerfile
	StopTimeout *int              `json:",omitempty"` // Timeout (in seconds) to stop a container
	Shell       strslice.StrSlice `json:",omitempty"` // Shell for shell-form of RUN, CMD, ENTRYPOINT
}

// Image is the JSON structure which describes some basic information about the image.
// This provides the `application/vnd.oci.image.config.v1+json` mediatype when marshalled to JSON.
type Image struct {
	specs.Image

	// Config defines the execution parameters which should be used as a base when running a container using the image.
	Config ImageConfig `json:"config,omitempty"`

	// Variant defines platform variant. To be added to OCI.
	Variant string `json:"variant,omitempty"`
}

func clone(src Image) Image {
	img := src
	img.Config = src.Config
	img.Config.Env = append([]string{}, src.Config.Env...)
	img.Config.Cmd = append([]string{}, src.Config.Cmd...)
	img.Config.Entrypoint = append([]string{}, src.Config.Entrypoint...)
	return img
}

func emptyImage(platform specs.Platform) Image {
	img := Image{
		Image: specs.Image{
			Architecture: platform.Architecture,
			OS:           platform.OS,
		},
		Variant: platform.Variant,
	}
	img.RootFS.Type = "layers"
	img.Config.WorkingDir = "/"
	img.Config.Env = []string{"PATH=" + system.DefaultPathEnv}
	return img
}

package containerd

import (
	"slices"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/image"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerOciImageToDockerImagePartial creates an image.Image from the imagespec.DockerOCIImage
// It doesn't set:
// - V1Image.ContainerConfig
// - V1Image.Container
// - Details
func dockerOciImageToDockerImagePartial(id image.ID, img dockerspec.DockerOCIImage) *image.Image {
	v1Image := image.V1Image{
		Config:       dockerOCIImageConfigToContainerConfig(img.Config),
		Architecture: img.Platform.Architecture,
		Variant:      img.Platform.Variant,
		OS:           img.Platform.OS,
		Author:       img.Author,
		Created:      img.Created,
	}

	out := image.NewImage(id)
	out.V1Image = v1Image
	out.RootFS = &image.RootFS{
		Type:    img.RootFS.Type,
		DiffIDs: slices.Clone(img.RootFS.DiffIDs),
	}
	out.History = img.History
	out.OSFeatures = img.OSFeatures
	out.OSVersion = img.OSVersion
	return out
}

func dockerImageToDockerOCIImage(img image.Image) dockerspec.DockerOCIImage {
	return dockerspec.DockerOCIImage{
		Image: ocispec.Image{
			Created: img.Created,
			Author:  img.Author,
			Platform: ocispec.Platform{
				Architecture: img.Architecture,
				Variant:      img.Variant,
				OS:           img.OS,
				OSVersion:    img.OSVersion,
				OSFeatures:   img.OSFeatures,
			},
			RootFS: ocispec.RootFS{
				Type:    img.RootFS.Type,
				DiffIDs: slices.Clone(img.RootFS.DiffIDs),
			},
			History: img.History,
		},
		Config: containerConfigToDockerOCIImageConfig(img.Config),
	}
}

func containerConfigToDockerOCIImageConfig(cfg *container.Config) dockerspec.DockerOCIImageConfig {
	var ociCfg ocispec.ImageConfig
	var ext dockerspec.DockerOCIImageConfigExt

	if cfg != nil {
		ociCfg = ocispec.ImageConfig{
			User:        cfg.User,
			Env:         cfg.Env,
			Entrypoint:  cfg.Entrypoint,
			Cmd:         cfg.Cmd,
			Volumes:     cfg.Volumes,
			WorkingDir:  cfg.WorkingDir,
			Labels:      cfg.Labels,
			StopSignal:  cfg.StopSignal,
			ArgsEscaped: cfg.ArgsEscaped, //nolint:staticcheck // Ignore SA1019. Need to keep it in image.
		}

		if len(cfg.ExposedPorts) > 0 {
			ociCfg.ExposedPorts = map[string]struct{}{}
			for k := range cfg.ExposedPorts {
				ociCfg.ExposedPorts[k.String()] = struct{}{}
			}
		}
		ext.Healthcheck = cfg.Healthcheck
		ext.OnBuild = cfg.OnBuild
		ext.Shell = cfg.Shell
	}

	return dockerspec.DockerOCIImageConfig{
		ImageConfig:             ociCfg,
		DockerOCIImageConfigExt: ext,
	}
}

func dockerOCIImageConfigToContainerConfig(cfg dockerspec.DockerOCIImageConfig) *container.Config {
	exposedPorts := make(network.PortSet, len(cfg.ExposedPorts))
	for k := range cfg.ExposedPorts {
		portRange, err := network.ParsePortRange(k)
		if err != nil {
			continue
		}
		for p := range portRange.All() {
			exposedPorts[p] = struct{}{}
		}
	}

	return &container.Config{
		Entrypoint:   cfg.Entrypoint,
		Env:          cfg.Env,
		Cmd:          cfg.Cmd,
		User:         cfg.User,
		WorkingDir:   cfg.WorkingDir,
		ExposedPorts: exposedPorts,
		Volumes:      cfg.Volumes,
		Labels:       cfg.Labels,
		ArgsEscaped:  cfg.ArgsEscaped, //nolint:staticcheck // Ignore SA1019. Need to keep it in image.
		StopSignal:   cfg.StopSignal,
		Healthcheck:  cfg.Healthcheck,
		OnBuild:      cfg.OnBuild,
		Shell:        cfg.Shell,
	}
}

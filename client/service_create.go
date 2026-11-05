package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/api/types/swarm"
	"github.com/opencontainers/go-digest"
)

// ServiceCreateOptions contains the options to use when creating a service.
type ServiceCreateOptions struct {
	Spec swarm.ServiceSpec

	// EncodedRegistryAuth is the encoded registry authorization credentials to
	// use when updating the service.
	//
	// This field follows the format of the X-Registry-Auth header.
	EncodedRegistryAuth string

	// QueryRegistry indicates whether the service update requires
	// contacting a registry. A registry may be contacted to retrieve
	// the image digest and manifest, which in turn can be used to update
	// platform or other information about the service.
	QueryRegistry bool
}

// ServiceCreateResult represents the result of creating a service.
type ServiceCreateResult struct {
	// ID is the ID of the created service.
	ID string

	// Warnings is a list of warnings that occurred during service creation.
	Warnings []string
}

// ServiceCreate creates a new service.
func (cli *Client) ServiceCreate(ctx context.Context, options ServiceCreateOptions) (ServiceCreateResult, error) {
	// Make sure containerSpec is not nil when no runtime is set or the runtime is set to container
	if options.Spec.TaskTemplate.ContainerSpec == nil && (options.Spec.TaskTemplate.Runtime == "" || options.Spec.TaskTemplate.Runtime == swarm.RuntimeContainer) {
		options.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
	}

	if err := validateServiceSpec(options.Spec); err != nil {
		return ServiceCreateResult{}, err
	}

	// ensure that the image is tagged
	var warnings []string
	switch {
	case options.Spec.TaskTemplate.ContainerSpec != nil:
		if taggedImg := imageWithTagString(options.Spec.TaskTemplate.ContainerSpec.Image); taggedImg != "" {
			options.Spec.TaskTemplate.ContainerSpec.Image = taggedImg
		}
		if options.QueryRegistry {
			if warning := resolveContainerSpecImage(ctx, cli, &options.Spec.TaskTemplate, options.EncodedRegistryAuth); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	case options.Spec.TaskTemplate.PluginSpec != nil:
		if taggedImg := imageWithTagString(options.Spec.TaskTemplate.PluginSpec.Remote); taggedImg != "" {
			options.Spec.TaskTemplate.PluginSpec.Remote = taggedImg
		}
		if options.QueryRegistry {
			if warning := resolvePluginSpecRemote(ctx, cli, &options.Spec.TaskTemplate, options.EncodedRegistryAuth); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}

	headers := http.Header{}
	if options.EncodedRegistryAuth != "" {
		headers[registry.AuthHeader] = []string{options.EncodedRegistryAuth}
	}
	resp, err := cli.post(ctx, "/services/create", nil, headers, options.Spec)
	defer ensureReaderClosed(resp)
	if err != nil {
		return ServiceCreateResult{}, err
	}

	var response swarm.ServiceCreateResponse
	err = json.NewDecoder(resp.Body).Decode(&response)
	warnings = append(warnings, response.Warnings...)

	return ServiceCreateResult{
		ID:       response.ID,
		Warnings: warnings,
	}, err
}

func resolveContainerSpecImage(ctx context.Context, cli DistributionAPIClient, taskSpec *swarm.TaskSpec, encodedAuth string) string {
	img, imgPlatforms, err := imageDigestAndPlatforms(ctx, cli, taskSpec.ContainerSpec.Image, encodedAuth)
	if err != nil {
		return digestWarning(taskSpec.ContainerSpec.Image)
	}
	taskSpec.ContainerSpec.Image = img
	if len(imgPlatforms) > 0 {
		if taskSpec.Placement == nil {
			taskSpec.Placement = &swarm.Placement{}
		}
		taskSpec.Placement.Platforms = imgPlatforms
	}
	return ""
}

func resolvePluginSpecRemote(ctx context.Context, cli DistributionAPIClient, taskSpec *swarm.TaskSpec, encodedAuth string) string {
	img, imgPlatforms, err := imageDigestAndPlatforms(ctx, cli, taskSpec.PluginSpec.Remote, encodedAuth)
	if err != nil {
		return digestWarning(taskSpec.PluginSpec.Remote)
	}
	taskSpec.PluginSpec.Remote = img
	if len(imgPlatforms) > 0 {
		if taskSpec.Placement == nil {
			taskSpec.Placement = &swarm.Placement{}
		}
		taskSpec.Placement.Platforms = imgPlatforms
	}
	return ""
}

func imageDigestAndPlatforms(ctx context.Context, cli DistributionAPIClient, image, encodedAuth string) (string, []swarm.Platform, error) {
	distributionInspect, err := cli.DistributionInspect(ctx, image, DistributionInspectOptions{
		EncodedRegistryAuth: encodedAuth,
	})
	var platforms []swarm.Platform
	if err != nil {
		return "", nil, err
	}

	imageWithDigest := imageWithDigestString(image, distributionInspect.Descriptor.Digest)

	if len(distributionInspect.Platforms) > 0 {
		platforms = make([]swarm.Platform, 0, len(distributionInspect.Platforms))
		seen := make(map[swarm.Platform]struct{}, len(distributionInspect.Platforms))
		for _, p := range distributionInspect.Platforms {
			// skip attestations and other data included in the manifest index.
			if p.OS == "unknown" || p.Architecture == "unknown" {
				continue
			}
			// clear architecture field for arm. This is a temporary patch to address
			// https://github.com/moby/swarmkit/issues/2294. The issue is that while
			// image manifests report "arm" as the architecture, the node reports
			// something like "armv7l" (includes the variant), which causes arm images
			// to stop working with swarm mode. This patch removes the architecture
			// constraint for arm images to ensure tasks get scheduled.
			arch := p.Architecture
			if strings.EqualFold(arch, "arm") {
				arch = ""
			}
			platform := swarm.Platform{
				Architecture: arch,
				OS:           p.OS,
			}

			// Skip duplicates. Swarm currently only uses os/arch, and doesn't
			// support other fields (Variant, OSVersion, OSFeatures), which
			// usually results in duplicate "os/arch" combinations coming from
			// the image.
			if _, ok := seen[platform]; ok {
				continue
			}
			seen[platform] = struct{}{}

			platforms = append(platforms, platform)
		}
	}
	return imageWithDigest, platforms, err
}

// imageWithDigestString takes an image string and a digest, and updates
// the image string if it didn't originally contain a digest. It returns
// image unmodified in other situations.
func imageWithDigestString(image string, dgst digest.Digest) string {
	namedRef, err := reference.ParseNormalizedNamed(image)
	if err == nil {
		if _, hasDigest := namedRef.(reference.Digested); !hasDigest {
			// ensure that image gets a default tag if none is provided
			img, err := reference.WithDigest(namedRef, dgst)
			if err == nil {
				return reference.FamiliarString(img)
			}
		}
	}
	return image
}

// imageWithTagString takes an image string, and returns a tagged image
// string, adding a 'latest' tag if one was not provided. It returns an
// empty string if a canonical reference was provided
func imageWithTagString(image string) string {
	namedRef, err := reference.ParseNormalizedNamed(image)
	if err == nil {
		return reference.FamiliarString(reference.TagNameOnly(namedRef))
	}
	return ""
}

// digestWarning constructs a formatted warning string using the
// image name that could not be pinned by digest. The formatting
// is hardcoded, but could me made smarter in the future
func digestWarning(image string) string {
	return fmt.Sprintf("image %s could not be accessed on a registry to record\nits digest. Each node will access %s independently,\npossibly leading to different nodes running different versions of the image.\n", image, image)
}

func validateServiceSpec(s swarm.ServiceSpec) error {
	if s.TaskTemplate.ContainerSpec != nil && s.TaskTemplate.PluginSpec != nil {
		return errors.New("must not specify both a container spec and a plugin spec in the task template")
	}
	if s.TaskTemplate.PluginSpec != nil && s.TaskTemplate.Runtime != swarm.RuntimePlugin {
		return errors.New("mismatched runtime with plugin spec")
	}
	if s.TaskTemplate.ContainerSpec != nil && (s.TaskTemplate.Runtime != "" && s.TaskTemplate.Runtime != swarm.RuntimeContainer) {
		return errors.New("mismatched runtime with container spec")
	}
	return nil
}

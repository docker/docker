//go:generate go tool mobyextgen

// Package containernamegeneratorv0 defines the experimental point for
// generating default container names.
package containernamegeneratorv0

import (
	"context"

	"github.com/moby/extensions"
)

// ContainerNameGenerator generates a default container name.
type ContainerNameGenerator interface {
	GenerateContainerName(ctx context.Context, req *GenerateContainerNameRequest) (*GenerateContainerNameReply, error)
}

// GenerateContainerNameRequest describes a container-name generation attempt.
type GenerateContainerNameRequest struct {
	// Retry is the number of prior generated names that collided.
	Retry int64 `pb:"1"`
	// ContainerID is the ID assigned to the container.
	ContainerID string `pb:"2"`
	// Image is the image reference used to create the container.
	Image string `pb:"3"`
}

// GenerateContainerNameReply contains a generated container name.
type GenerateContainerNameReply struct {
	// Name must contain 2 to 63 ASCII letters, digits, hyphens, or underscores.
	// It must start and end with a letter or digit.
	Name string `pb:"1"`
}

// Point is the single-provider container name-generator point.
var Point = extensions.DefineSinglePoint[ContainerNameGenerator]("org.mobyproject.extension.containernamegenerator.v0")

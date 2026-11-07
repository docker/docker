//go:generate go tool mobyextgen

// Package servicenamegeneratorv0 defines the experimental point for generating
// default Swarm service names.
package servicenamegeneratorv0

import (
	"context"

	"github.com/moby/extensions"
)

// ServiceNameGenerator generates a default Swarm service name.
type ServiceNameGenerator interface {
	GenerateServiceName(ctx context.Context, req *GenerateServiceNameRequest) (*GenerateServiceNameReply, error)
}

// GenerateServiceNameRequest describes a service-name generation attempt.
type GenerateServiceNameRequest struct {
	// Retry is the number of prior generated names that collided.
	Retry int64 `pb:"1"`
	// Image is the container image used by the service.
	// It is empty for services that use another runtime.
	Image string `pb:"2"`
}

// GenerateServiceNameReply contains a generated Swarm service name.
type GenerateServiceNameReply struct {
	// Name must contain 2 to 63 ASCII letters, digits, hyphens, or underscores.
	// It must start and end with a letter or digit.
	Name string `pb:"1"`
}

// Point is the single-provider service name-generator point.
var Point = extensions.DefineSinglePoint[ServiceNameGenerator]("org.mobyproject.extension.servicenamegenerator.v0")

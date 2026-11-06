// Package petname generates random adjective_animal names for containers and
// services. Each request, including retries, generates a new random pair;
// callers remain responsible for checking uniqueness.
//
// This extension is not registered in the daemon's built-in extensions.
package petname

import (
	"context"
	"errors"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/moby/extensions"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	servicenamegeneratorv0 "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0"
)

const extensionID extensions.ExtensionID = "org.mobyproject.namesgenerator.petname.v1"

// Extension provides random two-word container and service names.
var Extension = extensions.New(extensions.Declaration{
	ID: extensionID,
	Providers: []extensions.Provider{
		containernamegeneratorv0.Point.Provide(provider{}),
		servicenamegeneratorv0.Point.Provide(provider{}),
	},
})

type provider struct{}

func (provider) GenerateContainerName(_ context.Context, req *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
	if req == nil {
		return nil, errors.New("generate container name request is required")
	}
	return &containernamegeneratorv0.GenerateContainerNameReply{Name: petname.Generate(2, "_")}, nil
}

func (provider) GenerateServiceName(_ context.Context, req *servicenamegeneratorv0.GenerateServiceNameRequest) (*servicenamegeneratorv0.GenerateServiceNameReply, error) {
	if req == nil {
		return nil, errors.New("generate service name request is required")
	}
	return &servicenamegeneratorv0.GenerateServiceNameReply{Name: petname.Generate(2, "_")}, nil
}

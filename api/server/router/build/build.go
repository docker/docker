package build // import "github.com/docker/docker/api/server/router/build"

import (
	"github.com/docker/docker/api/server/router"
	"github.com/docker/docker/api/types"
	buildkit "github.com/docker/docker/builder/builder-next"
)

// buildRouter is a router to talk with the build controller
type buildRouter struct {
	backend  Backend
	daemon   experimentalProvider
	routes   []router.Route
	builder  *buildkit.Builder
	features *map[string]bool
}

// NewRouter initializes a new build router
func NewRouter(b Backend, d experimentalProvider, builder *buildkit.Builder, features *map[string]bool) router.Router {
	r := &buildRouter{
		backend:  b,
		daemon:   d,
		builder:  builder,
		features: features,
	}
	r.initRoutes()
	return r
}

// Routes returns the available routers to the build controller
func (r *buildRouter) Routes() []router.Route {
	return r.routes
}

func (r *buildRouter) initRoutes() {
	r.routes = []router.Route{
		router.NewPostRoute("/build", r.postBuild),
		router.NewPostRoute("/build/prune", r.postPrune),
		router.NewPostRoute("/build/cancel", r.postCancel),
	}
}

// BuilderVersion derives the default docker builder version from the config
// Note: it is valid to have BuilderVersion unset which means it is up to the
// client to choose which builder to use.
func BuilderVersion(features map[string]bool) types.BuilderVersion {
	var bv types.BuilderVersion
	if v, ok := features["buildkit"]; ok {
		if v {
			bv = types.BuilderBuildKit
		} else {
			bv = types.BuilderV1
		}
	}
	return bv
}

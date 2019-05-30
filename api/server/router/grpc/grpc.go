package grpc // import "github.com/docker/docker/api/server/router/grpc"

import (
	"github.com/docker/docker/api/server/router"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

type grpcRouter struct {
	routes     []router.Route
	grpcServer *grpc.Server
	h2Server   *http2.Server
}

// NewRouter initializes a new grpc http router
func NewRouter(backends ...Backend) router.Router {
	r := &grpcRouter{
		h2Server:   &http2.Server{},
		grpcServer: grpc.NewServer(),
	}
	for _, b := range backends {
		b.RegisterGRPC(r.grpcServer)
	}
	r.initRoutes()
	return r
}

// Routes returns the available routers to the session controller
func (r *grpcRouter) Routes() []router.Route {
	return r.routes
}

func (r *grpcRouter) initRoutes() {
	r.routes = []router.Route{
		router.NewPostRoute("/grpc", r.serveGRPC),
	}
}

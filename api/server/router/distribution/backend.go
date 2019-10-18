package distribution // import "github.com/docker/docker/api/server/router/distribution"

// Backend is all the methods that need to be implemented
// to provide image specific functionality.
type Backend interface {
	// TODO: containerd content store or manifest returned from Named and AuthConfig
}

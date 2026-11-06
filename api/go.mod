module github.com/moby/moby/api

go 1.24

// Docker, Moby, and golang.org/x packages are trusted
require (
	github.com/docker/go-units v0.5.0 //gosocialcheck:trusted
	github.com/moby/docker-image-spec v1.3.1 //gosocialcheck:trusted
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
)

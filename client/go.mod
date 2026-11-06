module github.com/moby/moby/client

go 1.24

// Docker, Moby, and golang.org/x packages are trusted
require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/errdefs/pkg v0.3.0
	github.com/distribution/reference v0.6.0
	github.com/docker/go-connections v0.8.2 //gosocialcheck:trusted
	github.com/docker/go-units v0.5.0 //gosocialcheck:trusted
	github.com/google/go-cmp v0.7.0
	github.com/moby/moby/api v1.56.1 //gosocialcheck:trusted
	github.com/moby/term v0.5.2 //gosocialcheck:trusted
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.60.0
	go.opentelemetry.io/otel/trace v1.35.0
	golang.org/x/time v0.11.0 //gosocialcheck:trusted
	gotest.tools/v3 v3.5.2
)

// Docker, Moby, and golang.org/x packages are trusted
require (
	github.com/Azure/go-ansiterm v0.0.0-20261006220739-8c912ac31dd4 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	//gosocialcheck:trusted
	github.com/moby/docker-image-spec v1.3.1 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.35.0 // indirect
	go.opentelemetry.io/otel/metric v1.35.0 // indirect
	//gosocialcheck:trusted
	golang.org/x/sys v0.33.0 // indirect
)

replace github.com/moby/moby/api => ../api

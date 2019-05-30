[![asciicinema example](https://asciinema.org/a/gPEIEo1NzmDTUu2bEPsUboqmU.png)](https://asciinema.org/a/gPEIEo1NzmDTUu2bEPsUboqmU)


## BuildKit

[![GoDoc](https://godoc.org/github.com/moby/buildkit?status.svg)](https://godoc.org/github.com/moby/buildkit/client/llb)
[![Build Status](https://travis-ci.org/moby/buildkit.svg?branch=master)](https://travis-ci.org/moby/buildkit)
[![Go Report Card](https://goreportcard.com/badge/github.com/moby/buildkit)](https://goreportcard.com/report/github.com/moby/buildkit)


BuildKit is a toolkit for converting source code to build artifacts in an efficient, expressive and repeatable manner.

Key features:
- Automatic garbage collection
- Extendable frontend formats
- Concurrent dependency resolution
- Efficient instruction caching
- Build cache import/export
- Nested build job invocations
- Distributable workers
- Multiple output formats
- Pluggable architecture
- Execution without root privileges


Read the proposal from https://github.com/moby/moby/issues/32925

Introductory blog post https://blog.mobyproject.org/introducing-buildkit-17e056cc5317

:information_source: If you are visiting this repo for the usage of experimental Dockerfile features like `RUN --mount=type=(bind|cache|tmpfs|secret|ssh)`, please refer to [`frontend/dockerfile/docs/experimental.md`](frontend/dockerfile/docs/experimental.md).

### Used by

BuildKit is used by the following projects:

- [Moby & Docker](https://github.com/moby/moby/pull/37151)
- [img](https://github.com/genuinetools/img)
- [OpenFaaS Cloud](https://github.com/openfaas/openfaas-cloud)
- [container build interface](https://github.com/containerbuilding/cbi)
- [Knative Build Templates](https://github.com/knative/build-templates)
- [vab](https://github.com/stellarproject/vab)
- [Rio](https://github.com/rancher/rio) (on roadmap)

### Quick start

Dependencies:
- [runc](https://github.com/opencontainers/runc)
- [containerd](https://github.com/containerd/containerd) (if you want to use containerd worker)


The following command installs `buildkitd` and `buildctl` to `/usr/local/bin`:

```bash
$ make && sudo make install
```

You can also use `make binaries-all` to prepare `buildkitd.containerd_only` and `buildkitd.oci_only`.

#### Starting the buildkitd daemon:

```
buildkitd --debug --root /var/lib/buildkit
```

The buildkitd daemon supports two worker backends: OCI (runc) and containerd.

By default, the OCI (runc) worker is used.
You can set `--oci-worker=false --containerd-worker=true` to use the containerd worker.

We are open to adding more backends.

#### Exploring LLB

BuildKit builds are based on a binary intermediate format called LLB that is used for defining the dependency graph for processes running part of your build. tl;dr: LLB is to Dockerfile what LLVM IR is to C.

- Marshaled as Protobuf messages
- Concurrently executable
- Efficiently cacheable
- Vendor-neutral (i.e. non-Dockerfile languages can be easily implemented)

See [`solver/pb/ops.proto`](./solver/pb/ops.proto) for the format definition.

Currently, following high-level languages has been implemented for LLB:

- Dockerfile (See [Exploring Dockerfiles](#exploring-dockerfiles))
- [Buildpacks](https://github.com/tonistiigi/buildkit-pack)
- (open a PR to add your own language)

For understanding the basics of LLB, `examples/buildkit*` directory contains scripts that define how to build different configurations of BuildKit itself and its dependencies using the `client` package. Running one of these scripts generates a protobuf definition of a build graph. Note that the script itself does not execute any steps of the build.

You can use `buildctl debug dump-llb` to see what data is in this definition. Add `--dot` to generate dot layout.

```bash
go run examples/buildkit0/buildkit.go | buildctl debug dump-llb | jq .
```

To start building use `buildctl build` command. The example script accepts `--with-containerd` flag to choose if containerd binaries and support should be included in the end result as well. 

```bash
go run examples/buildkit0/buildkit.go | buildctl build
```

`buildctl build` will show interactive progress bar by default while the build job is running. If the path to the trace file is specified, the trace file generated will contain all information about the timing of the individual steps and logs.

Different versions of the example scripts show different ways of describing the build definition for this project to show the capabilities of the library. New versions have been added when new features have become available.

- `./examples/buildkit0` - uses only exec operations, defines a full stage per component.
- `./examples/buildkit1` - cloning git repositories has been separated for extra concurrency.
- `./examples/buildkit2` - uses git sources directly instead of running `git clone`, allowing better performance and much safer caching.
- `./examples/buildkit3` - allows using local source files for separate components eg. `./buildkit3 --runc=local | buildctl build --local runc-src=some/local/path`  
- `./examples/dockerfile2llb` - can be used to convert a Dockerfile to LLB for debugging purposes
- `./examples/gobuild` - shows how to use nested invocation to generate LLB for Go package internal dependencies


#### Exploring Dockerfiles

Frontends are components that run inside BuildKit and convert any build definition to LLB. There is a special frontend called gateway (gateway.v0) that allows using any image as a frontend.

During development, Dockerfile frontend (dockerfile.v0) is also part of the BuildKit repo. In the future, this will be moved out, and Dockerfiles can be built using an external image.

##### Building a Dockerfile with `buildctl`

```
buildctl build --frontend=dockerfile.v0 --local context=. --local dockerfile=.
buildctl build --frontend=dockerfile.v0 --local context=. --local dockerfile=. --opt target=foo --opt build-arg:foo=bar
```

`--local` exposes local source files from client to the builder. `context` and `dockerfile` are the names Dockerfile frontend looks for build context and Dockerfile location.

##### build-using-dockerfile utility

For people familiar with `docker build` command, there is an example wrapper utility in `./examples/build-using-dockerfile` that allows building Dockerfiles with BuildKit using a syntax similar to `docker build`.

```
go build ./examples/build-using-dockerfile && sudo install build-using-dockerfile /usr/local/bin

build-using-dockerfile -t myimage .
build-using-dockerfile -t mybuildkit -f ./hack/dockerfiles/test.Dockerfile .

# build-using-dockerfile will automatically load the resulting image to Docker
docker inspect myimage
```

##### Building a Dockerfile using [external frontend](https://hub.docker.com/r/docker/dockerfile/tags/):

External versions of the Dockerfile frontend are pushed to https://hub.docker.com/r/docker/dockerfile-upstream and https://hub.docker.com/r/docker/dockerfile and can be used with the gateway frontend. The source for the external frontend is currently located in `./frontend/dockerfile/cmd/dockerfile-frontend` but will move out of this repository in the future ([#163](https://github.com/moby/buildkit/issues/163)). For automatic build from master branch of this repository `docker/dockerfile-upsteam:master` or `docker/dockerfile-upstream:master-experimental` image can be used.

```
buildctl build --frontend gateway.v0 --opt source=docker/dockerfile --local context=. --local dockerfile=.
buildctl build --frontend gateway.v0 --opt source=docker/dockerfile --opt context=git://github.com/moby/moby --opt build-arg:APT_MIRROR=cdn-fastly.deb.debian.org
````

##### Building a Dockerfile with experimental features like `RUN --mount=type=(bind|cache|tmpfs|secret|ssh)`

See [`frontend/dockerfile/docs/experimental.md`](frontend/dockerfile/docs/experimental.md).

### Output

By default, the build result and intermediate cache will only remain internally in BuildKit. An output needs to be specified to retrieve the result.

##### Exporting resulting image to containerd

The containerd worker needs to be used

```
buildctl build ... --output type=image,name=docker.io/username/image
ctr --namespace=buildkit images ls
```

##### Push resulting image to registry

```
buildctl build ... --output type=image,name=docker.io/username/image,push=true
```

If credentials are required, `buildctl` will attempt to read Docker configuration file.


##### Exporting build result back to client

The local client will copy the files directly to the client. This is useful if BuildKit is being used for building something else than container images.

```
buildctl build ... --output type=local,dest=path/to/output-dir
```

Tar exporter is similar to local exporter but transfers the files through a tarball.

```
buildctl build ... --output type=tar,dest=out.tar
buildctl build ... --output type=tar > out.tar
```


##### Exporting built image to Docker

```
# exported tarball is also compatible with OCI spec
buildctl build ... --output type=docker,name=myimage | docker load
```

##### Exporting [OCI Image Format](https://github.com/opencontainers/image-spec) tarball to client

```
buildctl build ... --output type=oci,dest=path/to/output.tar
buildctl build ... --output type=oci > output.tar
```

### Exporting/Importing build cache (not image itself)

#### To/From registry

```
buildctl build ... --export-cache type=registry,ref=localhost:5000/myrepo:buildcache
buildctl build ... --import-cache type=registry,ref=localhost:5000/myrepo:buildcache
```

#### To/From local filesystem

```
buildctl build ... --export-cache type=local,dest=path/to/output-dir
buildctl build ... --import-cache type=local,src=path/to/input-dir
```

The directory layout conforms to OCI Image Spec v1.0.

#### `--export-cache` options
* `mode=min` (default): only export layers for the resulting image
* `mode=max`: export all the layers of all intermediate steps
* `ref=docker.io/user/image:tag`: reference for `registry` cache exporter
* `dest=path/to/output-dir`: directory for `local` cache exporter

#### `--import-cache` options
* `ref=docker.io/user/image:tag`: reference for `registry` cache importer
* `src=path/to/input-dir`: directory for `local` cache importer
* `digest=sha256:deadbeef`: digest of the manifest list to import for `local` cache importer. Defaults to the digest of "latest" tag in `index.json`

### Other

#### View build cache

```
buildctl du -v
```

#### Show enabled workers

```
buildctl debug workers -v
```

### Running containerized buildkit

BuildKit can also be used by running the `buildkitd` daemon inside a Docker container and accessing it remotely. The client tool `buildctl` is also available for Mac and Windows.

We provide `buildkitd` container images as [`moby/buildkit`](https://hub.docker.com/r/moby/buildkit/tags/):

* `moby/buildkit:latest`: built from the latest regular [release](https://github.com/moby/buildkit/releases)
* `moby/buildkit:rootless`: same as `latest` but runs as an unprivileged user, see [`docs/rootless.md`](docs/rootless.md)
* `moby/buildkit:master`: built from the master branch
* `moby/buildkit:master-rootless`: same as master but runs as an unprivileged user, see [`docs/rootless.md`](docs/rootless.md)

To run daemon in a container:

```
docker run -d --privileged -p 1234:1234 moby/buildkit:latest --addr tcp://0.0.0.0:1234
export BUILDKIT_HOST=tcp://0.0.0.0:1234
buildctl build --help
```

The images can be also built locally using `./hack/dockerfiles/test.Dockerfile` (or `./hack/dockerfiles/test.buildkit.Dockerfile` if you already have BuildKit).
Run `make images` to build the images as `moby/buildkit:local` and `moby/buildkit:local-rootless`.

#### Connection helpers

If you are running `moby/buildkit:master` or `moby/buildkit:master-rootless` as a Docker/Kubernetes container, you can use special `BUILDKIT_HOST` URL for connecting to the BuildKit daemon in the container:

```
export BUILDKIT_HOST=docker://<container>
```

```
export BUILDKIT_HOST=kube-pod://<pod>
```

### Opentracing support

BuildKit supports opentracing for buildkitd gRPC API and buildctl commands. To capture the trace to [Jaeger](https://github.com/jaegertracing/jaeger), set `JAEGER_TRACE` environment variable to the collection address.


```
docker run -d -p6831:6831/udp -p16686:16686 jaegertracing/all-in-one:latest
export JAEGER_TRACE=0.0.0.0:6831
# restart buildkitd and buildctl so they know JAEGER_TRACE
# any buildctl command should be traced to http://127.0.0.1:16686/
```


### Supported runc version

During development, BuildKit is tested with the version of runc that is being used by the containerd repository. Please refer to [runc.md](https://github.com/containerd/containerd/blob/v1.2.1/RUNC.md) for more information.

### Running BuildKit without root privileges

Please refer to [`docs/rootless.md`](docs/rootless.md).

### Contributing

Want to contribute to BuildKit? Awesome! You can find information about
contributing to this project in the [CONTRIBUTING.md](/.github/CONTRIBUTING.md)

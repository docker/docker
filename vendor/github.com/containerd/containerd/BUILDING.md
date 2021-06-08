# Build containerd from source

This guide is useful if you intend to contribute on containerd. Thanks for your
effort. Every contribution is very appreciated.

This doc includes:
* [Build requirements](#build-requirements)
* [Build the development environment](#build-the-development-environment)
* [Build containerd](#build-containerd)
* [Via docker container](#via-docker-container)
* [Testing](#testing-containerd)

## Build requirements

To build the `containerd` daemon, and the `ctr` simple test client, the following build system dependencies are required:

* Go 1.13.x or above except 1.14.x
* Protoc 3.x compiler and headers (download at the [Google protobuf releases page](https://github.com/google/protobuf/releases))
* Btrfs headers and libraries for your distribution. Note that building the btrfs driver can be disabled via the build tag `no_btrfs`, removing this dependency.

## Build the development environment

First you need to setup your Go development environment. You can follow this
guideline [How to write go code](https://golang.org/doc/code.html) and at the
end you have `go` command in your `PATH`.

You need `git` to checkout the source code:

```sh
git clone https://github.com/containerd/containerd
```

For proper results, install the `protoc` release into `/usr/local` on your build system. For example, the following commands will download and install the 3.11.4 release for a 64-bit Linux host:

```
$ wget -c https://github.com/google/protobuf/releases/download/v3.11.4/protoc-3.11.4-linux-x86_64.zip
$ sudo unzip protoc-3.11.4-linux-x86_64.zip -d /usr/local
```

`containerd` uses [Btrfs](https://en.wikipedia.org/wiki/Btrfs) it means that you
need to satisfy these dependencies in your system:

* CentOS/Fedora: `yum install btrfs-progs-devel`
* Debian/Ubuntu: `apt-get install btrfs-progs libbtrfs-dev`
  * Debian(before Buster)/Ubuntu(before 19.10): `apt-get install btrfs-tools`

At this point you are ready to build `containerd` yourself!

## Build runc

`runc` is the default container runtime used by `containerd` and is required to
run containerd. While it is okay to download a runc binary and install that on
the system, sometimes it is necessary to build runc directly when working with
container runtime development. You can skip this step if you already have the
correct version of `runc` installed.

`runc` requires `libseccomp`. You may need to install the missing dependencies:

* CentOS/Fedora: `yum install libseccomp libseccomp-devel`
* Debian/Ubuntu: `apt-get install libseccomp libseccomp-dev`


For the quick and dirty installation, you can use the following:

```
git clone https://github.com/opencontainers/runc
cd runc
make
sudo make install
```

Make sure to follow the guidelines for versioning in [RUNC.md](/docs/RUNC.md) for the
best results.

## Build containerd

`containerd` uses `make` to create a repeatable build flow. It means that you
can run:

```
cd containerd
make
```

This is going to build all the project binaries in the `./bin/` directory.

You can move them in your global path, `/usr/local/bin` with:

```sudo
sudo make install
```

When making any changes to the gRPC API, you can use the installed `protoc`
compiler to regenerate the API generated code packages with:

```sudo
make generate
```

> *Note*: Several build tags are currently available:
> * `no_btrfs`: A build tag disables building the btrfs snapshot driver.
> * `no_cri`: A build tag disables building Kubernetes [CRI](http://blog.kubernetes.io/2016/12/container-runtime-interface-cri-in-kubernetes.html) support into containerd.
> See [here](https://github.com/containerd/cri-containerd#build-tags) for build tags of CRI plugin.
> * `no_devmapper`: A build tag disables building the device mapper snapshot driver.
>
> For example, adding `BUILDTAGS=no_btrfs` to your environment before calling the **binaries**
> Makefile target will disable the btrfs driver within the containerd Go build.

Vendoring of external imports uses the [Go Modules](https://golang.org/ref/mod#vendoring). You need
to use `go mod` command to modify the dependencies. After modifition, you should run `go mod tidy`
and `go mod vendor` to ensure the `go.mod`, `go.sum` files and `vendor` directory are up to date.
Changes to these files should become a single commit for a PR which relies on vendored updates.

Please refer to [RUNC.md](/docs/RUNC.md) for the currently supported version of `runc` that is used by containerd.

### Static binaries

You can build static binaries by providing a few variables to `make`:

```sudo
make EXTRA_FLAGS="-buildmode pie" \
	EXTRA_LDFLAGS='-linkmode external -extldflags "-fno-PIC -static"' \
	BUILDTAGS="netgo osusergo static_build"
```

> *Note*:
> - static build is discouraged
> - static containerd binary does not support loading shared object plugins (`*.so`)

# Via Docker container

The following instructions assume you are at the parent directory of containerd source directory.

## Build containerd

You can build `containerd` via a Linux-based Docker container.
You can build an image from this `Dockerfile`:

```
FROM golang

RUN apt-get update && \
    apt-get install -y libbtrfs-dev
```

Let's suppose that you built an image called `containerd/build`. From the
containerd source root directory you can run the following command:

```sh
docker run -it \
    -v ${PWD}/containerd:/go/src/github.com/containerd/containerd \
    -e GOPATH=/go \
    -w /go/src/github.com/containerd/containerd containerd/build sh
```

This mounts `containerd` repository

You are now ready to [build](#build-containerd):

```sh
 make && make install
```

## Build containerd and runc
To have complete core container runtime, you will need both `containerd` and `runc`. It is possible to build both of these via Docker container.

You can use `git` to checkout `runc`:

```sh
git clone https://github.com/opencontainers/runc
```

We can build an image from this `Dockerfile`:

```sh
FROM golang

RUN apt-get update && \
    apt-get install -y libbtrfs-dev libseccomp-dev

```

In our Docker container we will build `runc` build, which includes
[seccomp](https://en.wikipedia.org/wiki/seccomp), [SELinux](https://en.wikipedia.org/wiki/Security-Enhanced_Linux),
and [AppArmor](https://en.wikipedia.org/wiki/AppArmor) support. Seccomp support
in runc requires `libseccomp-dev` as a dependency (AppArmor and SELinux support
do not require external libraries at build time). Refer to [RUNC.md](docs/RUNC.md)
in the docs directory to for details about building runc, and to learn about
supported versions of `runc` as used by containerd.

Let's suppose you build an image called `containerd/build` from the above Dockerfile. You can run the following command:

```sh
docker run -it --privileged \
    -v /var/lib/containerd \
    -v ${PWD}/runc:/go/src/github.com/opencontainers/runc \
    -v ${PWD}/containerd:/go/src/github.com/containerd/containerd \
    -e GOPATH=/go \
    -w /go/src/github.com/containerd/containerd containerd/build sh
```

This mounts both `runc` and `containerd` repositories in our Docker container.

From within our Docker container let's build `containerd`:

```sh
cd /go/src/github.com/containerd/containerd
make && make install
```

These binaries can be found in the `./bin` directory in your host.
`make install` will move the binaries in your `$PATH`.

Next, let's build `runc`:

```sh
cd /go/src/github.com/opencontainers/runc
make && make install
```

For further details about building runc, refer to [RUNC.md](docs/RUNC.md) in the
docs directory.

When working with `ctr`, the simple test client we just built, don't forget to start the daemon!

```sh
containerd --config config.toml
```

# Testing containerd

During the automated CI the unit tests and integration tests are run as part of the PR validation. As a developer you can run these tests locally by using any of the following `Makefile` targets:
 - `make test`: run all non-integration tests that do not require `root` privileges
 - `make root-test`: run all non-integration tests which require `root`
 - `make integration`: run all tests, including integration tests and those which require `root`. `TESTFLAGS_PARALLEL` can be used to control parallelism. For example, `TESTFLAGS_PARALLEL=1 make integration` will lead a non-parallel execution. The default value of `TESTFLAGS_PARALLEL` is **8**.

To execute a specific test or set of tests you can use the `go test` capabilities
without using the `Makefile` targets. The following examples show how to specify a test
name and also how to use the flag directly against `go test` to run root-requiring tests.

```sh
# run the test <TEST_NAME>:
go test	-v -run "<TEST_NAME>" .
# enable the root-requiring tests:
go test -v -run . -test.root
```

Example output from directly running `go test` to execute the `TestContainerList` test:
```sh
sudo go test -v -run "TestContainerList" . -test.root
INFO[0000] running tests against containerd revision=f2ae8a020a985a8d9862c9eb5ab66902c2888361 version=v1.0.0-beta.2-49-gf2ae8a0
=== RUN   TestContainerList
--- PASS: TestContainerList (0.00s)
PASS
ok  	github.com/containerd/containerd	4.778s
```

## Additional tools

### containerd-stress
In addition to `go test`-based testing executed via the `Makefile` targets, the `containerd-stress` tool is available and built with the `all` or `binaries` targets and installed during `make install`.

With this tool you can stress a running containerd daemon for a specified period of time, selecting a concurrency level to generate stress against the daemon. The following command is an example of having five workers running for two hours against a default containerd gRPC socket address:

```sh
containerd-stress -c 5 -t 120
```

For more information on this tool's options please run `containerd-stress --help`.

### bucketbench
[Bucketbench](https://github.com/estesp/bucketbench) is an external tool which can be used to drive load against a container runtime, specifying a particular set of lifecycle operations to run with a specified amount of concurrency. Bucketbench is more focused on generating performance details than simply inducing load against containerd.

Bucketbench differs from the `containerd-stress` tool in a few ways:
 - Bucketbench has support for testing the Docker engine, the `runc` binary, and containerd 0.2.x (via `ctr`) and 1.0 (via the client library) branches.
 - Bucketbench is driven via configuration file that allows specifying a list of lifecycle operations to execute. This can be used to generate detailed statistics per-command (e.g. start, stop, pause, delete).
 - Bucketbench generates detailed reports and timing data at the end of the configured test run.

More details on how to install and run `bucketbench` are available at the [GitHub project page](https://github.com/estesp/bucketbench).

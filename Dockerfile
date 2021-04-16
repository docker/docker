# syntax=docker/dockerfile:1.2

ARG CROSS="false"
ARG SYSTEMD="false"
# IMPORTANT: When updating this please note that stdlib archive/tar pkg is vendored
ARG GO_VERSION=1.13.15
ARG DEBIAN_FRONTEND=noninteractive

#-------------------------------------------------------------------------------
# Package versions
#-------------------------------------------------------------------------------

# CRIU_VERSION specifies the version of CRIU to download from the
# https://github.com/checkpoint-restore/criu repository. CRIU is used
# in the integration tests to test the experimental checkpoint/restore support
ARG CRIU_VERSION=v3.14

# REGISTRY_COMMIT specifies the version of the registry to build and install
# from the https://github.com/docker/distribution repository. This version of
# the registry is used to test both schema 1 and schema 2 manifests.
#
# Generally, the commit specified here should match a current, tagged release.
#
# v2.3.0-rc.0
ARG REGISTRY_COMMIT=47a064d4195a9b56133891bbb13620c3ac83a827

# REGISTRY_COMMIT_SCHEMA1 specifies the version of the regsitry to build and
# install from the https://github.com/docker/distribution repository. This is
# an older (pre v2.3.0) version of the registry that only supports schema1
# manifests. This version of the registry is not working on arm64, so installation
# is skipped on that architecture.

# v2.2.0 + ec87e9b6971d831f0eff752ddb54fb64693e51cd (docker/1.10-dev branch)
ARG REGISTRY_COMMIT_SCHEMA1=ec87e9b6971d831f0eff752ddb54fb64693e51cd

# GO_SWAGGER_COMMIT specifies the version of the go-swagger binary to build and
# install. Go-swagger is used in CI for validating swagger.yaml in hack/validate/swagger-gen
#
# Currently uses a fork from https://github.com/kolyshkin/go-swagger/tree/golang-1.13-fix,
# TODO: move to under moby/ or fix upstream go-swagger to work for us.
ARG GO_SWAGGER_COMMIT=5e6cb12f7c82ce78e45ba71fa6cb1928094db050

# GOTOML_VERSION specifies the version of the tomll binary to build and install
# from the https://github.com/pelletier/go-toml repository. This binary is used
# in CI in the hack/validate/toml script.
#
# When updating this version, consider updating the github.com/pelletier/go-toml
# dependency in vendor.conf accordingly.
#
ARG GOTOML_VERSION=v1.8.1

# VNDR_COMMIT specifies the version of the vndr tool to build and install
# from the https://github.com/LK4D4/vndr repository.
#
# The vndr tool is used to manage vendored go packages in the vendor directory,
# and is pinned to a fixed version because different versions of this tool
# can result in differences in the (go) files that are considered for vendoring.
#
# v0.1.2
ARG VNDR_COMMIT=f12b881cb8f081a5058408a58f429b9014833fc6

# CONTAINERD_COMMIT specifies the version of the containerd runtime binary
# to install from the https://github.com/containerd/containerd repository.
#
# This version is used to build statically compiled containerd binaries, and
# used for the integration tests. The distributed docker .deb and .rpm packages
# depend on a separate (containerd.io) package, which may be a different version
# as is specified here.
#
# Generally, the commit specified here should match a tagged release.

# The containerd golang package is also pinned in vendor.conf. When updating
# the binary version you may also need to update the vendor version to pick up
# bug fixes or new APIs, however, usually the Go packages are built from a
# commit from the master branch.
#
# v1.4.4
ARG CONTAINERD_COMMIT=05f951a3781f4f2c1911b05e61c160e9c30eaa8e

# LIBNETWORK_COMMIT is used to build the docker-userland-proxy binary. When
# updating the binary version, consider updating github.com/docker/libnetwork
# in vendor.conf accordingly
ARG LIBNETWORK_COMMIT=b3507428be5b458cb0e2b4086b13531fb0706e46

ARG GOLANGCI_LINT_COMMIT=v1.23.8
ARG GOTESTSUM_COMMIT=v0.5.3

# DOCKERCLI_CHANNEL and DOCKERCLI_VERSION specify the version of the CLI to
# install for use in the integration-cli tests. The integration-cli testsuite
# is frozen, and no new tests should be added. The version of the CLI is pinned
# to the CLI version that was current at the time the integration-cli suite
# was frozen.
ARG DOCKERCLI_CHANNEL=stable
ARG DOCKERCLI_VERSION=17.06.2-ce

# RUNC_COMMIT specifies the version of runc to install from the
# https://github.com/opencontainers/runc repository.
#
# The version of runc should match the version that is used by the containerd
# version that is used. If you need to update runc, open a pull request in
# the containerd project first, and update both after that is merged.
#
# When updating RUNC_COMMIT, also update runc in vendor.conf accordingly
#
# v1.0.0-rc93
ARG RUNC_COMMIT=12644e614e25b05da6fd08a38ffa0cfe1903fdec

# TINI_COMMIT specifies the version of tini (docker-init) to build, and install
# from the https://github.com/krallin/tini.git repository. This binary is used
# when starting containers with the `--init` option.
#
# v0.19.0
ARG TINI_COMMIT=de40ad007797e0dcd8b7126f27bb87401d224240

# ROOTLESSKIT_COMMIT specifies the version of rootlesskit to install from the
# https://github.com/rootless-containers/rootlesskit.git repository
#
# v0.14.1
ARG ROOTLESSKIT_COMMIT=ed9b8c5cc48d29d0a979dae52a24f6e886795abd

# VPNKIT_VERSION specifies the tag of the VPNKit (djs55/vpnkit) image to
# use from Docker Hub, which contains the vpnkit binary. It's included in the
# Dockerfile for all architectures, but currently only contains x86_64 and arm64
# binaries. VPNKit is used for networking when running the daemon in rootless mode.
ARG VPNKIT_VERSION=0.5.0
ARG DOCKER_BUILDTAGS="apparmor seccomp"

ARG BASE_DEBIAN_DISTRO="buster"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"

FROM ${GOLANG_IMAGE} AS base
RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
ARG APT_MIRROR
RUN sed -ri "s/(httpredir|deb).debian.org/${APT_MIRROR:-deb.debian.org}/g" /etc/apt/sources.list \
 && sed -ri "s/(security).debian.org/${APT_MIRROR:-security.debian.org}/g" /etc/apt/sources.list
ENV GO111MODULE=off

FROM base AS criu
ARG DEBIAN_FRONTEND
# Install dependency packages specific to criu
RUN --mount=type=cache,sharing=locked,id=moby-criu-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-criu-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libcap-dev \
            libnet-dev \
            libnl-3-dev \
            libprotobuf-c-dev \
            libprotobuf-dev \
            protobuf-c-compiler \
            protobuf-compiler \
            python-protobuf

# Install CRIU for checkpoint/restore support
ARG CRIU_VERSION
RUN mkdir -p /usr/src/criu \
    && curl -sSL https://github.com/checkpoint-restore/criu/archive/${CRIU_VERSION}.tar.gz | tar -C /usr/src/criu/ -xz --strip-components=1 \
    && cd /usr/src/criu \
    && make \
    && make PREFIX=/build/ install-criu

FROM base AS registry
WORKDIR /go/src/github.com/docker/distribution
# Install two versions of the registry. The first one is a recent version that
# supports both schema 1 and 2 manifests. The second one is an older version that
# only supports schema1 manifests. This allows integration-cli tests to cover
# push/pull with both schema1 and schema2 manifests.
# The old version of the registry is not working on arm64, so installation is
# skipped on that architecture.
ARG REGISTRY_COMMIT
ARG REGISTRY_COMMIT_SCHEMA1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
        set -x \
        && git clone https://github.com/docker/distribution.git . \
        && git checkout -q "$REGISTRY_COMMIT" \
        && GOPATH="/go/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH" \
           go build -buildmode=pie -o /build/registry-v2 github.com/docker/distribution/cmd/registry \
        && case $(dpkg --print-architecture) in \
               amd64|armhf|ppc64*|s390x) \
               git checkout -q "$REGISTRY_COMMIT_SCHEMA1"; \
               GOPATH="/go/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH"; \
                   go build -buildmode=pie -o /build/registry-v2-schema1 github.com/docker/distribution/cmd/registry; \
                ;; \
           esac

FROM base AS swagger
WORKDIR $GOPATH/src/github.com/go-swagger/go-swagger
ARG GO_SWAGGER_COMMIT
# TODO: this is currently using https://github.com/kolyshkin/go-swagger/tree/golang-1.13-fix
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
        set -x \
        && git clone https://github.com/kolyshkin/go-swagger.git . \
        && git checkout -q "$GO_SWAGGER_COMMIT" \
        && go build -o /build/swagger github.com/go-swagger/go-swagger/cmd/swagger

FROM debian:${BASE_DEBIAN_DISTRO} AS frozen-images
ARG DEBIAN_FRONTEND
RUN --mount=type=cache,sharing=locked,id=moby-frozen-images-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-frozen-images-aptcache,target=/var/cache/apt \
       apt-get update && apt-get install -y --no-install-recommends \
           ca-certificates \
           curl \
           jq
# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
COPY contrib/download-frozen-image-v2.sh /
ARG TARGETARCH
RUN /download-frozen-image-v2.sh /build \
        buildpack-deps:buster@sha256:d0abb4b1e5c664828b93e8b6ac84d10bce45ee469999bef88304be04a2709491 \
        busybox:latest@sha256:95cf004f559831017cdf4628aaf1bb30133677be8702a8c5f2994629f637a209 \
        busybox:glibc@sha256:1f81263701cddf6402afe9f33fca0266d9fff379e59b1748f33d3072da71ee85 \
        debian:bullseye@sha256:7190e972ab16aefea4d758ebe42a293f4e5c5be63595f4d03a5b9bf6839a4344 \
        hello-world:latest@sha256:d58e752213a51785838f9eed2b7a498ffa1cb3aa7f946dda11af39286c3db9a9 \
        arm32v7/hello-world:latest@sha256:50b8560ad574c779908da71f7ce370c0a2471c098d44d1c8f6b513c5a55eeeb1
# See also frozenImages in "testutil/environment/protect.go" (which needs to be updated when adding images to this list)

FROM base AS cross-false

FROM --platform=linux/amd64 base AS cross-true
ARG DEBIAN_FRONTEND
RUN dpkg --add-architecture arm64
RUN dpkg --add-architecture armel
RUN dpkg --add-architecture armhf
RUN --mount=type=cache,sharing=locked,id=moby-cross-true-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-true-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            crossbuild-essential-arm64 \
            crossbuild-essential-armel \
            crossbuild-essential-armhf

FROM cross-${CROSS} as dev-base

FROM dev-base AS runtime-dev-cross-false
ARG DEBIAN_FRONTEND
RUN echo 'deb http://deb.debian.org/debian buster-backports main' > /etc/apt/sources.list.d/backports.list
RUN --mount=type=cache,sharing=locked,id=moby-cross-false-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-false-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            binutils-mingw-w64 \
            g++-mingw-w64-x86-64 \
            libapparmor-dev \
            libbtrfs-dev \
            libdevmapper-dev \
            libseccomp-dev/buster-backports \
            libsystemd-dev \
            libudev-dev

FROM --platform=linux/amd64 runtime-dev-cross-false AS runtime-dev-cross-true
ARG DEBIAN_FRONTEND
# These crossbuild packages rely on gcc-<arch>, but this doesn't want to install
# on non-amd64 systems.
# Additionally, the crossbuild-amd64 is currently only on debian:buster, so
# other architectures cannnot crossbuild amd64.
RUN echo 'deb http://deb.debian.org/debian buster-backports main' > /etc/apt/sources.list.d/backports.list
RUN --mount=type=cache,sharing=locked,id=moby-cross-true-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-true-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libapparmor-dev:arm64 \
            libapparmor-dev:armel \
            libapparmor-dev:armhf \
            libseccomp-dev:arm64/buster-backports \
            libseccomp-dev:armel/buster-backports \
            libseccomp-dev:armhf/buster-backports

FROM runtime-dev-cross-${CROSS} AS runtime-dev

FROM base AS tomll
ARG GOTOML_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install/tomll.installer,target=/tmp/install/tomll.installer \
        . /tmp/install/tomll.installer && PREFIX=/build install_tomll

FROM base AS vndr
ARG VNDR_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh vndr

FROM dev-base AS containerd
ARG DEBIAN_FRONTEND
RUN --mount=type=cache,sharing=locked,id=moby-containerd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-containerd-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libbtrfs-dev
ARG CONTAINERD_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh containerd

FROM dev-base AS proxy
ARG LIBNETWORK_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh proxy

FROM base AS golangci_lint
ARG GOLANGCI_LINT_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh golangci_lint

FROM base AS gotestsum
ARG GOTESTSUM_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh gotestsum

FROM base AS shfmt
ARG SHFMT_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh shfmt

FROM dev-base AS dockercli
ARG DOCKERCLI_CHANNEL
ARG DOCKERCLI_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh dockercli

FROM runtime-dev AS runc
ARG RUNC_COMMIT
ARG RUNC_BUILDTAGS
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh runc

FROM dev-base AS tini
ARG DEBIAN_FRONTEND
ARG TINI_COMMIT
RUN --mount=type=cache,sharing=locked,id=moby-tini-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-tini-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            cmake \
            vim-common
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh tini

FROM dev-base AS rootlesskit
ARG ROOTLESSKIT_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
        PREFIX=/build /tmp/install/install.sh rootlesskit
COPY ./contrib/dockerd-rootless.sh /build
COPY ./contrib/dockerd-rootless-setuptool.sh /build

FROM --platform=amd64 djs55/vpnkit:${VPNKIT_VERSION} AS vpnkit-amd64

FROM --platform=arm64 djs55/vpnkit:${VPNKIT_VERSION} AS vpnkit-arm64

FROM scratch AS vpnkit
COPY --from=vpnkit-amd64 /vpnkit /build/vpnkit.x86_64
COPY --from=vpnkit-arm64 /vpnkit /build/vpnkit.aarch64

# TODO: Some of this is only really needed for testing, it would be nice to split this up
FROM runtime-dev AS dev-systemd-false
ARG DEBIAN_FRONTEND
RUN groupadd -r docker
RUN useradd --create-home --gid docker unprivilegeduser \
 && mkdir -p /home/unprivilegeduser/.local/share/docker \
 && chown -R unprivilegeduser /home/unprivilegeduser
# Let us use a .bashrc file
RUN ln -sfv /go/src/github.com/docker/docker/.bashrc ~/.bashrc
# Activate bash completion and include Docker's completion if mounted with DOCKER_BASH_COMPLETION_PATH
RUN echo "source /usr/share/bash-completion/bash_completion" >> /etc/bash.bashrc
RUN ln -s /usr/local/completion/bash/docker /etc/bash_completion.d/docker
RUN ldconfig
# This should only install packages that are specifically needed for the dev environment and nothing else
# Do you really need to add another package here? Can it be done in a different build stage?
RUN --mount=type=cache,sharing=locked,id=moby-dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            apparmor \
            aufs-tools \
            bash-completion \
            bzip2 \
            iptables \
            jq \
            libcap2-bin \
            libnet1 \
            libnl-3-200 \
            libprotobuf-c1 \
            net-tools \
            pigz \
            python3-pip \
            python3-setuptools \
            python3-wheel \
            sudo \
            thin-provisioning-tools \
            uidmap \
            vim \
            vim-common \
            xfsprogs \
            xz-utils \
            zip


# Switch to use iptables instead of nftables (to match the CI hosts)
# TODO use some kind of runtime auto-detection instead if/when nftables is supported (https://github.com/moby/moby/issues/26824)
RUN update-alternatives --set iptables  /usr/sbin/iptables-legacy  || true \
 && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy || true \
 && update-alternatives --set arptables /usr/sbin/arptables-legacy || true

RUN pip3 install yamllint==1.26.1

COPY --from=dockercli     /build/ /usr/local/cli
COPY --from=frozen-images /build/ /docker-frozen-images
COPY --from=swagger       /build/ /usr/local/bin/
COPY --from=tomll         /build/ /usr/local/bin/
COPY --from=tini          /build/ /usr/local/bin/
COPY --from=registry      /build/ /usr/local/bin/
COPY --from=criu          /build/ /usr/local/
COPY --from=vndr          /build/ /usr/local/bin/
COPY --from=gotestsum     /build/ /usr/local/bin/
COPY --from=golangci_lint /build/ /usr/local/bin/
COPY --from=shfmt         /build/ /usr/local/bin/
COPY --from=runc          /build/ /usr/local/bin/
COPY --from=containerd    /build/ /usr/local/bin/
COPY --from=rootlesskit   /build/ /usr/local/bin/
COPY --from=vpnkit        /build/ /usr/local/bin/
COPY --from=proxy         /build/ /usr/local/bin/
ENV PATH=/usr/local/cli:$PATH
ARG DOCKER_BUILDTAGS
ENV DOCKER_BUILDTAGS="${DOCKER_BUILDTAGS}"
WORKDIR /go/src/github.com/docker/docker
VOLUME /var/lib/docker
VOLUME /home/unprivilegeduser/.local/share/docker
# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

FROM dev-systemd-false AS dev-systemd-true
RUN --mount=type=cache,sharing=locked,id=moby-dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            dbus \
            dbus-user-session \
            systemd \
            systemd-sysv
RUN mkdir -p hack \
  && curl -o hack/dind-systemd https://raw.githubusercontent.com/AkihiroSuda/containerized-systemd/b70bac0daeea120456764248164c21684ade7d0d/docker-entrypoint.sh \
  && chmod +x hack/dind-systemd
ENTRYPOINT ["hack/dind-systemd"]

FROM dev-systemd-${SYSTEMD} AS dev

FROM runtime-dev AS binary-base

# These build-args are used by hack/make/.go-autogen. We "bake" then into
# environment variables, so that they are also available in `make shell`
ARG DOCKER_GITCOMMIT=HEAD
ENV DOCKER_GITCOMMIT=${DOCKER_GITCOMMIT}
ARG VERSION
ENV VERSION=${VERSION}
ARG PLATFORM
ENV PLATFORM=${PLATFORM}
ARG PRODUCT
ENV PRODUCT=${PRODUCT}
ARG DEFAULT_PRODUCT_LICENSE
ENV DEFAULT_PRODUCT_LICENSE=${DEFAULT_PRODUCT_LICENSE}
ARG DOCKER_BUILDTAGS
ENV DOCKER_BUILDTAGS="${DOCKER_BUILDTAGS}"
ARG TINI_COMMIT
ENV TINI_COMMIT=${TINI_COMMIT}
ENV PREFIX=/build
# TODO: This is here because hack/make.sh binary copies these extras binaries
# from $PATH into the bundles dir.
# It would be nice to handle this in a different way.
COPY --from=tini        /build/ /usr/local/bin/
COPY --from=runc        /build/ /usr/local/bin/
COPY --from=containerd  /build/ /usr/local/bin/
COPY --from=rootlesskit /build/ /usr/local/bin/
COPY --from=proxy       /build/ /usr/local/bin/
COPY --from=vpnkit      /build/ /usr/local/bin/
WORKDIR /go/src/github.com/docker/docker

FROM binary-base AS build-binary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
        hack/make.sh binary

FROM binary-base AS build-dynbinary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
        hack/make.sh dynbinary

FROM binary-base AS build-cross
ARG DOCKER_CROSSPLATFORMS
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
    --mount=type=tmpfs,target=/go/src/github.com/docker/docker/autogen \
        hack/make.sh cross

FROM scratch AS binary
COPY --from=build-binary /build/bundles/ /

FROM scratch AS dynbinary
COPY --from=build-dynbinary /build/bundles/ /

FROM scratch AS cross
COPY --from=build-cross /build/bundles/ /

FROM runtime-dev  AS integration-tests
WORKDIR /go/src/github.com/docker/docker
ENV PREFIX=/build

# Copy test sources so that tests that use assert can print errors
RUN --mount=type=bind,target=/go/src/github.com/docker/docker \
        mkdir -p /build${PWD} \
        && find integration integration-cli -name \*_test.go -exec cp --parents '{}' /build${PWD} \;

# Build the integration tests and copy the resulting binaries to /build/tests
ARG DOCKER_GITCOMMIT=HEAD
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker,readwrite \
        hack/make.sh build-integration-test-binary \
        && mkdir -p /build/tests \
        && find . -name test.main -exec cp --parents '{}' /build/tests \;

# Build DockerSuite.TestBuild* dependencies
FROM runtime-dev AS contrib
WORKDIR /go/src/github.com/docker/docker
COPY contrib/syscall-test           /build/syscall-test
COPY contrib/httpserver/Dockerfile  /build/httpserver/Dockerfile
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
        CGO_ENABLED=0 go build -buildmode=pie -o /build/httpserver/httpserver ./contrib/httpserver


## Generate testing image
FROM alpine:3.10 AS e2e-runner

ENV DOCKER_REMOTE_DAEMON=1
ENV DOCKER_INTEGRATION_DAEMON_DEST=/
ENTRYPOINT ["/scripts/run.sh"]

# Add an unprivileged user to be used for tests which need it
RUN addgroup docker && adduser -D -G docker unprivilegeduser -s /bin/ash

# GNU tar is used for generating the emptyfs image
RUN apk --no-cache add \
    bash \
    ca-certificates \
    g++ \
    git \
    iptables \
    pigz \
    tar \
    xz

COPY hack/test/e2e-run.sh       /scripts/run.sh
COPY hack/make/.ensure-emptyfs  /scripts/ensure-emptyfs.sh

COPY integration/testdata       /tests/integration/testdata
COPY integration/build/testdata /tests/integration/build/testdata
COPY integration-cli/fixtures   /tests/integration-cli/fixtures

COPY --from=frozen-images       /build/ /docker-frozen-images
COPY --from=dockercli           /build/ /usr/bin/
COPY --from=contrib             /build/ /tests/contrib/
COPY --from=integration-tests   /build/ /

FROM dev AS final
# These build-args are used by hack/make/.go-autogen. We "bake" then into
# environment variables, so that they are also available in `make shell`
ARG DOCKER_GITCOMMIT=HEAD
ENV DOCKER_GITCOMMIT=${DOCKER_GITCOMMIT}
ARG VERSION
ENV VERSION=${VERSION}
ARG PLATFORM
ENV PLATFORM=${PLATFORM}
ARG PRODUCT
ENV PRODUCT=${PRODUCT}
ARG DEFAULT_PRODUCT_LICENSE
ENV DEFAULT_PRODUCT_LICENSE=${DEFAULT_PRODUCT_LICENSE}
ARG DOCKER_BUILDTAGS
ENV DOCKER_BUILDTAGS="${DOCKER_BUILDTAGS}"
ARG TINI_COMMIT
ENV TINI_COMMIT=${TINI_COMMIT}
COPY . /go/src/github.com/docker/docker

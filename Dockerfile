# syntax=docker/dockerfile:1.1.3-experimental

ARG CROSS="false"
ARG GO_VERSION=1.13.5
ARG DEBIAN_FRONTEND=noninteractive
ARG VPNKIT_DIGEST=e508a17cfacc8fd39261d5b4e397df2b953690da577e2c987a47630cd0c42f8e
ARG DOCKER_BUILDTAGS="apparmor seccomp selinux"

FROM golang:${GO_VERSION}-buster AS base
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
ENV CRIU_VERSION 3.12
RUN mkdir -p /usr/src/criu \
    && curl -sSL https://github.com/checkpoint-restore/criu/archive/v${CRIU_VERSION}.tar.gz | tar -C /usr/src/criu/ -xz --strip-components=1 \
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
ENV REGISTRY_COMMIT_SCHEMA1 ec87e9b6971d831f0eff752ddb54fb64693e51cd
ENV REGISTRY_COMMIT 47a064d4195a9b56133891bbb13620c3ac83a827
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
# Install go-swagger for validating swagger.yaml
# This is https://github.com/kolyshkin/go-swagger/tree/golang-1.13-fix
# TODO: move to under moby/ or fix upstream go-swagger to work for us.
ENV GO_SWAGGER_COMMIT 5793aa66d4b4112c2602c716516e24710e4adbb5
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
        set -x \
        && git clone https://github.com/kolyshkin/go-swagger.git . \
        && git checkout -q "$GO_SWAGGER_COMMIT" \
        && go build -o /build/swagger github.com/go-swagger/go-swagger/cmd/swagger

FROM base AS frozen-images
ARG DEBIAN_FRONTEND
RUN --mount=type=cache,sharing=locked,id=moby-frozen-images-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-frozen-images-aptcache,target=/var/cache/apt \
       apt-get update && apt-get install -y --no-install-recommends \
           ca-certificates \
           jq
# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
COPY contrib/download-frozen-image-v2.sh /
RUN /download-frozen-image-v2.sh /build \
        buildpack-deps:jessie@sha256:dd86dced7c9cd2a724e779730f0a53f93b7ef42228d4344b25ce9a42a1486251 \
        busybox:latest@sha256:bbc3a03235220b170ba48a157dd097dd1379299370e1ed99ce976df0355d24f0 \
        busybox:glibc@sha256:0b55a30394294ab23b9afd58fab94e61a923f5834fba7ddbae7f8e0c11ba85e6 \
        debian:jessie@sha256:287a20c5f73087ab406e6b364833e3fb7b3ae63ca0eb3486555dc27ed32c6e60 \
        hello-world:latest@sha256:be0cd392e45be79ffeffa6b05338b98ebb16c87b255f48e297ec7f98e123905c
# See also ensureFrozenImagesLinux() in "integration-cli/fixtures_linux_daemon_test.go" (which needs to be updated when adding images to this list)

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
RUN --mount=type=cache,sharing=locked,id=moby-cross-false-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-false-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            binutils-mingw-w64 \
            g++-mingw-w64-x86-64 \
            libapparmor-dev \
            libbtrfs-dev \
            libdevmapper-dev \
            libseccomp-dev \
            libsystemd-dev \
            libudev-dev

FROM --platform=linux/amd64 runtime-dev-cross-false AS runtime-dev-cross-true
ARG DEBIAN_FRONTEND
# These crossbuild packages rely on gcc-<arch>, but this doesn't want to install
# on non-amd64 systems.
# Additionally, the crossbuild-amd64 is currently only on debian:buster, so
# other architectures cannnot crossbuild amd64.
RUN --mount=type=cache,sharing=locked,id=moby-cross-true-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-true-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libapparmor-dev:arm64 \
            libapparmor-dev:armel \
            libapparmor-dev:armhf \
            libseccomp-dev:arm64 \
            libseccomp-dev:armel \
            libseccomp-dev:armhf

FROM runtime-dev-cross-${CROSS} AS runtime-dev

FROM base AS tomlv
ENV INSTALL_BINARY_NAME=tomlv
ARG TOMLV_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS vndr
ENV INSTALL_BINARY_NAME=vndr
ARG VNDR_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS containerd
ARG DEBIAN_FRONTEND
ARG CONTAINERD_COMMIT
RUN --mount=type=cache,sharing=locked,id=moby-containerd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-containerd-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libbtrfs-dev
ENV INSTALL_BINARY_NAME=containerd
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS proxy
ENV INSTALL_BINARY_NAME=proxy
ARG LIBNETWORK_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS golangci_lint
ENV INSTALL_BINARY_NAME=golangci_lint
ARG GOLANGCI_LINT_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS gotestsum
ENV INSTALL_BINARY_NAME=gotestsum
ARG GOTESTSUM_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS dockercli
ENV INSTALL_BINARY_NAME=dockercli
ARG DOCKERCLI_CHANNEL
ARG DOCKERCLI_VERSION
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM runtime-dev AS runc
ENV INSTALL_BINARY_NAME=runc
ARG RUNC_COMMIT
ARG RUNC_BUILDTAGS
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS tini
ARG DEBIAN_FRONTEND
ARG TINI_COMMIT
RUN --mount=type=cache,sharing=locked,id=moby-tini-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-tini-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            cmake \
            vim-common
COPY hack/dockerfile/install/install.sh ./install.sh
ENV INSTALL_BINARY_NAME=tini
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS rootlesskit
ENV INSTALL_BINARY_NAME=rootlesskit
ARG ROOTLESSKIT_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        PREFIX=/build/ ./install.sh $INSTALL_BINARY_NAME
COPY ./contrib/dockerd-rootless.sh /build

FROM djs55/vpnkit@sha256:${VPNKIT_DIGEST} AS vpnkit

# TODO: Some of this is only really needed for testing, it would be nice to split this up
FROM runtime-dev AS dev
ARG DEBIAN_FRONTEND
RUN groupadd -r docker
RUN useradd --create-home --gid docker unprivilegeduser
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
            thin-provisioning-tools \
            vim \
            vim-common \
            xfsprogs \
            xz-utils \
            zip


# Switch to use iptables instead of nftables (to match the host machine)
RUN update-alternatives --set iptables  /usr/sbin/iptables-legacy  || true \
 && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy || true \
 && update-alternatives --set arptables /usr/sbin/arptables-legacy || true

RUN pip3 install yamllint==1.16.0

COPY --from=dockercli     /build/ /usr/local/cli
COPY --from=frozen-images /build/ /docker-frozen-images
COPY --from=swagger       /build/ /usr/local/bin/
COPY --from=tomlv         /build/ /usr/local/bin/
COPY --from=tini          /build/ /usr/local/bin/
COPY --from=registry      /build/ /usr/local/bin/
COPY --from=criu          /build/ /usr/local/
COPY --from=vndr          /build/ /usr/local/bin/
COPY --from=gotestsum     /build/ /usr/local/bin/
COPY --from=golangci_lint /build/ /usr/local/bin/
COPY --from=runc          /build/ /usr/local/bin/
COPY --from=containerd    /build/ /usr/local/bin/
COPY --from=rootlesskit   /build/ /usr/local/bin/
COPY --from=vpnkit        /vpnkit /usr/local/bin/vpnkit.x86_64
COPY --from=proxy         /build/ /usr/local/bin/
ENV PATH=/usr/local/cli:$PATH
ARG DOCKER_BUILDTAGS
ENV DOCKER_BUILDTAGS="${DOCKER_BUILDTAGS}"
WORKDIR /go/src/github.com/docker/docker
VOLUME /var/lib/docker
# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

FROM runtime-dev AS binary-base
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
ENV PREFIX=/build
# TODO: This is here because hack/make.sh binary copies these extras binaries
# from $PATH into the bundles dir.
# It would be nice to handle this in a different way.
COPY --from=tini        /build/ /usr/local/bin/
COPY --from=runc        /build/ /usr/local/bin/
COPY --from=containerd  /build/ /usr/local/bin/
COPY --from=rootlesskit /build/ /usr/local/bin/
COPY --from=proxy       /build/ /usr/local/bin/
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
        mkdir -p "${PREFIX}${PWD}" \
        && find integration integration-cli -name \*_test.go -exec cp --parents '{}' "${PREFIX}${PWD}" \;

# Build the integration tests and copy the resulting binaries to /build/tests
ARG DOCKER_GITCOMMIT=HEAD

# TODO allow overriding output directory for test.main binaries, so that we don't have to mount the source readwrite
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


# Generate testing image for running the integration tests on a pre-installed
# daemon on the host.
FROM alpine:3.11 AS e2e-runner

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
COPY . /go/src/github.com/docker/docker

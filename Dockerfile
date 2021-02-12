# This file describes the standard way to build Docker, using docker
#
# Usage:
#
# # Use make to build a development environment image and run it in a container.
# # This is slow the first time.
# make BIND_DIR=. shell
#
# The following commands are executed inside the running container.

# # Make a dockerd binary.
# # hack/make.sh binary
#
# # Install dockerd to /usr/local/bin
# # make install
#
# # Run unit tests
# # hack/test/unit
#
# # Run tests e.g. integration, py
# # hack/make.sh binary test-integration test-docker-py
#
# Note: AppArmor used to mess with privileged mode, but this is no longer
# the case. Therefore, you don't have to disable it anymore.
#

ARG CROSS="false"
# IMPORTANT: When updating this please note that stdlib archive/tar pkg is vendored
ARG GO_VERSION=1.13.15
ARG DEBIAN_FRONTEND=noninteractive
ARG VPNKIT_DIGEST=e508a17cfacc8fd39261d5b4e397df2b953690da577e2c987a47630cd0c42f8e

FROM golang:${GO_VERSION}-buster AS base
ARG APT_MIRROR
RUN sed -ri "s/(httpredir|deb).debian.org/${APT_MIRROR:-deb.debian.org}/g" /etc/apt/sources.list \
 && sed -ri "s/(security).debian.org/${APT_MIRROR:-security.debian.org}/g" /etc/apt/sources.list
ENV GO111MODULE=off

FROM base AS criu
ARG DEBIAN_FRONTEND
# Install dependency packages specific to criu
RUN apt-get update && apt-get install -y --no-install-recommends \
        libcap-dev \
        libnet-dev \
        libnl-3-dev \
        libprotobuf-c-dev \
        libprotobuf-dev \
        protobuf-c-compiler \
        protobuf-compiler \
        python-protobuf \
    && rm -rf /var/lib/apt/lists/*

# Install CRIU for checkpoint/restore support
ARG CRIU_VERSION=3.14
RUN mkdir -p /usr/src/criu \
    && curl -sSL https://github.com/checkpoint-restore/criu/archive/v${CRIU_VERSION}.tar.gz | tar -C /usr/src/criu/ -xz --strip-components=1 \
    && cd /usr/src/criu \
    && make \
    && make PREFIX=/build/ install-criu

FROM base AS registry
# Install two versions of the registry. The first is an older version that
# only supports schema1 manifests. The second is a newer version that supports
# both. This allows integration-cli tests to cover push/pull with both schema1
# and schema2 manifests.
ENV REGISTRY_COMMIT_SCHEMA1 ec87e9b6971d831f0eff752ddb54fb64693e51cd
ENV REGISTRY_COMMIT 47a064d4195a9b56133891bbb13620c3ac83a827
RUN set -x \
    && export GOPATH="$(mktemp -d)" \
    && git clone https://github.com/docker/distribution.git "$GOPATH/src/github.com/docker/distribution" \
    && (cd "$GOPATH/src/github.com/docker/distribution" && git checkout -q "$REGISTRY_COMMIT") \
    && GOPATH="$GOPATH/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH" \
        go build -buildmode=pie -o /build/registry-v2 github.com/docker/distribution/cmd/registry \
    && case $(dpkg --print-architecture) in \
        amd64|ppc64*|s390x) \
        (cd "$GOPATH/src/github.com/docker/distribution" && git checkout -q "$REGISTRY_COMMIT_SCHEMA1"); \
        GOPATH="$GOPATH/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH"; \
            go build -buildmode=pie -o /build/registry-v2-schema1 github.com/docker/distribution/cmd/registry; \
        ;; \
       esac \
    && rm -rf "$GOPATH"

FROM base AS swagger
# Install go-swagger for validating swagger.yaml
# This is https://github.com/kolyshkin/go-swagger/tree/golang-1.13-fix
# TODO: move to under moby/ or fix upstream go-swagger to work for us.
ENV GO_SWAGGER_COMMIT 5e6cb12f7c82ce78e45ba71fa6cb1928094db050
RUN set -x \
    && export GOPATH="$(mktemp -d)" \
    && git clone https://github.com/kolyshkin/go-swagger.git "$GOPATH/src/github.com/go-swagger/go-swagger" \
    && (cd "$GOPATH/src/github.com/go-swagger/go-swagger" && git checkout -q "$GO_SWAGGER_COMMIT") \
    && go build -o /build/swagger github.com/go-swagger/go-swagger/cmd/swagger \
    && rm -rf "$GOPATH"

FROM base AS frozen-images
ARG DEBIAN_FRONTEND
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        jq \
    && rm -rf /var/lib/apt/lists/*
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

FROM base AS cross-true
ARG DEBIAN_FRONTEND
RUN dpkg --add-architecture arm64
RUN dpkg --add-architecture armel
RUN dpkg --add-architecture armhf
RUN if [ "$(go env GOHOSTARCH)" = "amd64" ]; then \
        apt-get update && apt-get install -y --no-install-recommends \
        crossbuild-essential-arm64 \
        crossbuild-essential-armel \
        crossbuild-essential-armhf \
        && rm -rf /var/lib/apt/lists/*; \
    fi

FROM cross-${CROSS} as dev-base

FROM dev-base AS runtime-dev-cross-false
ARG DEBIAN_FRONTEND
RUN apt-get update && apt-get install -y --no-install-recommends \
        libapparmor-dev \
        libseccomp-dev \
    && rm -rf /var/lib/apt/lists/*

FROM cross-true AS runtime-dev-cross-true
ARG DEBIAN_FRONTEND
# These crossbuild packages rely on gcc-<arch>, but this doesn't want to install
# on non-amd64 systems.
# Additionally, the crossbuild-amd64 is currently only on debian:buster, so
# other architectures cannnot crossbuild amd64.
RUN if [ "$(go env GOHOSTARCH)" = "amd64" ]; then \
        apt-get update && apt-get install -y --no-install-recommends \
            libapparmor-dev:arm64 \
            libapparmor-dev:armel \
            libapparmor-dev:armhf \
            libseccomp-dev:arm64 \
            libseccomp-dev:armel \
            libseccomp-dev:armhf \
            # install this arches seccomp here due to compat issues with the v0 builder
            # This is as opposed to inheriting from runtime-dev-cross-false
            libapparmor-dev \
            libseccomp-dev \
        && rm -rf /var/lib/apt/lists/*; \
    fi

FROM runtime-dev-cross-${CROSS} AS runtime-dev

FROM base AS tomlv
ENV INSTALL_BINARY_NAME=tomlv
ARG TOMLV_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS vndr
ENV INSTALL_BINARY_NAME=vndr
ARG VNDR_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS containerd
ARG DEBIAN_FRONTEND
ARG CONTAINERD_COMMIT
RUN apt-get update && apt-get install -y --no-install-recommends \
        libbtrfs-dev \
    && rm -rf /var/lib/apt/lists/*
ENV INSTALL_BINARY_NAME=containerd
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS proxy
ENV INSTALL_BINARY_NAME=proxy
ARG LIBNETWORK_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS gometalinter
ENV INSTALL_BINARY_NAME=gometalinter
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS gotestsum
ENV INSTALL_BINARY_NAME=gotestsum
ARG GOTESTSUM_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS dockercli
ENV INSTALL_BINARY_NAME=dockercli
ARG DOCKERCLI_CHANNEL
ARG DOCKERCLI_VERSION
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM runtime-dev AS runc
ENV INSTALL_BINARY_NAME=runc
ARG RUNC_COMMIT
ARG RUNC_BUILDTAGS
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS tini
ARG DEBIAN_FRONTEND
ARG TINI_COMMIT
RUN apt-get update && apt-get install -y --no-install-recommends \
        cmake \
        vim-common \
    && rm -rf /var/lib/apt/lists/*
COPY hack/dockerfile/install/install.sh ./install.sh
ENV INSTALL_BINARY_NAME=tini
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS rootlesskit
ENV INSTALL_BINARY_NAME=rootlesskit
ARG ROOTLESSKIT_COMMIT
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build/ ./install.sh $INSTALL_BINARY_NAME
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
RUN apt-get update && apt-get install -y --no-install-recommends \
        apparmor \
        aufs-tools \
        bash-completion \
        binutils-mingw-w64 \
        libbtrfs-dev \
        bzip2 \
        g++-mingw-w64-x86-64 \
        iptables \
        jq \
        libcap2-bin \
        libdevmapper-dev \
        libnet1 \
        libnl-3-200 \
        libprotobuf-c1 \
        libsystemd-dev \
        libudev-dev \
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
        zip \
    && rm -rf /var/lib/apt/lists/*

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
COPY --from=gometalinter  /build/ /usr/local/bin/
COPY --from=runc          /build/ /usr/local/bin/
COPY --from=containerd    /build/ /usr/local/bin/
COPY --from=rootlesskit   /build/ /usr/local/bin/
COPY --from=vpnkit        /vpnkit /usr/local/bin/vpnkit.x86_64
COPY --from=proxy         /build/ /usr/local/bin/

ENV PATH=/usr/local/cli:$PATH
ENV DOCKER_BUILDTAGS apparmor seccomp selinux
WORKDIR /go/src/github.com/docker/docker
VOLUME /var/lib/docker
# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

FROM dev AS final
# Upload docker source
COPY . /go/src/github.com/docker/docker

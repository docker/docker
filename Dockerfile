# syntax=docker/dockerfile:1.1.7-experimental

ARG CROSS="false"
ARG SYSTEMD="false"
# IMPORTANT: When updating this please note that stdlib archive/tar pkg is vendored
ARG GO_VERSION=1.13.15
ARG DEBIAN_FRONTEND=noninteractive
ARG VPNKIT_VERSION=0.4.0
ARG DOCKER_BUILDTAGS="apparmor seccomp selinux"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-buster"
ARG BASE_IMAGE="debian:buster-slim"

FROM ${GOLANG_IMAGE} AS go

FROM ${BASE_IMAGE} AS base
RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
ARG APT_MIRROR
RUN sed -ri "s/(httpredir|deb).debian.org/${APT_MIRROR:-deb.debian.org}/g" /etc/apt/sources.list \
 && sed -ri "s/(security).debian.org/${APT_MIRROR:-security.debian.org}/g" /etc/apt/sources.list
ENV PATH=/usr/local/go/bin:${PATH}
ENV GO111MODULE=off
ENV GOPATH=/go
RUN --mount=type=cache,sharing=locked,id=moby-criu-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-criu-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install -y  --no-install-recommends \
        ca-certificates \
        curl \
        gcc \
        git \
        libtool \
        make \
        pkg-config \
        procps

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
ARG CRIU_VERSION=3.14
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
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
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
ENV GO_SWAGGER_COMMIT 5e6cb12f7c82ce78e45ba71fa6cb1928094db050
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=tmpfs,target=/go/src/ \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
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
RUN --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        /download-frozen-image-v2.sh /build \
            buildpack-deps:buster@sha256:d0abb4b1e5c664828b93e8b6ac84d10bce45ee469999bef88304be04a2709491 \
            busybox:latest@sha256:95cf004f559831017cdf4628aaf1bb30133677be8702a8c5f2994629f637a209 \
            busybox:glibc@sha256:1f81263701cddf6402afe9f33fca0266d9fff379e59b1748f33d3072da71ee85 \
            debian:buster@sha256:46d659005ca1151087efa997f1039ae45a7bf7a2cbbe2d17d3dcbda632a3ee9a \
            hello-world:latest@sha256:d58e752213a51785838f9eed2b7a498ffa1cb3aa7f946dda11af39286c3db9a9
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
            libudev-dev \
            pkg-config

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

FROM dev-base AS containerd
ARG DEBIAN_FRONTEND
RUN --mount=type=cache,sharing=locked,id=moby-containerd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-containerd-aptcache,target=/var/cache/apt \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        apt-get update && apt-get install -y --no-install-recommends \
            libbtrfs-dev
ARG CONTAINERD_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        PREFIX=/build /tmp/install/install.sh containerd

FROM dev-base AS proxy
ARG LIBNETWORK_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        PREFIX=/build /tmp/install/install.sh proxy

FROM base AS gotestsum
ARG GOTESTSUM_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        PREFIX=/build /tmp/install/install.sh gotestsum

FROM dev-base AS dockercli
ARG DOCKERCLI_CHANNEL
ARG DOCKERCLI_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        PREFIX=/build /tmp/install/install.sh dockercli

FROM runtime-dev AS runc
ARG RUNC_COMMIT
ARG RUNC_BUILDTAGS
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,src=hack/dockerfile/install,target=/tmp/install \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
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
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        PREFIX=/build /tmp/install/install.sh rootlesskit
COPY ./contrib/dockerd-rootless.sh /build
COPY ./contrib/dockerd-rootless-setuptool.sh /build

FROM djs55/vpnkit:${VPNKIT_VERSION} AS vpnkit

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
            iproute2 \
            iptables \
            iputils-ping \
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

RUN pip3 install yamllint==1.16.0

COPY --from=dockercli     /build/ /usr/local/cli
COPY --from=frozen-images /build/ /docker-frozen-images
COPY --from=swagger       /build/ /usr/local/bin/
COPY --from=tini          /build/ /usr/local/bin/
COPY --from=registry      /build/ /usr/local/bin/
COPY --from=criu          /build/ /usr/local/
COPY --from=gotestsum     /build/ /usr/local/bin/
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
COPY --from=go            /usr/local/go /usr/local/go


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
COPY --from=vpnkit      /vpnkit /usr/local/bin/vpnkit.x86_64
WORKDIR /go/src/github.com/docker/docker

FROM binary-base AS build-binary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        hack/make.sh binary

FROM binary-base AS build-dynbinary
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        hack/make.sh dynbinary

FROM binary-base AS build-cross
ARG DOCKER_CROSSPLATFORMS
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=/go/src/github.com/docker/docker \
    --mount=type=tmpfs,target=/go/src/github.com/docker/docker/autogen \
    --mount=from=go,source=/usr/local/go,target=/usr/local/go \
        hack/make.sh cross

FROM scratch AS binary
COPY --from=build-binary /build/bundles/ /

FROM scratch AS dynbinary
COPY --from=build-dynbinary /build/bundles/ /

FROM scratch AS cross
COPY --from=build-cross /build/bundles/ /

FROM dev AS final
COPY . /go/src/github.com/docker/docker

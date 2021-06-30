# syntax=docker/dockerfile:1.2

ARG CROSS="false"
ARG SYSTEMD="false"
# IMPORTANT: When updating this please note that stdlib archive/tar pkg is vendored
ARG GO_VERSION=1.16.5
ARG DEBIAN_FRONTEND=noninteractive
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
ADD --chmod=0644 https://download.opensuse.org/repositories/devel:/tools:/criu/Debian_10/Release.key /etc/apt/trusted.gpg.d/criu.gpg.asc
# FIXME: temporarily doing a manual chmod as workaround for https://github.com/moby/buildkit/issues/2114
RUN --mount=type=cache,sharing=locked,id=moby-criu-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-criu-aptcache,target=/var/cache/apt \
        chmod 0644 /etc/apt/trusted.gpg.d/criu.gpg.asc \
        && echo 'deb https://download.opensuse.org/repositories/devel:/tools:/criu/Debian_10/ /' > /etc/apt/sources.list.d/criu.list \
        && apt-get update \
        && apt-get install -y --no-install-recommends criu \
        && install -D /usr/sbin/criu /build/criu

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
ENV GO_SWAGGER_COMMIT 5e6cb12f7c82ce78e45ba71fa6cb1928094db050
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
RUN dpkg --add-architecture ppc64el
RUN dpkg --add-architecture s390x
RUN --mount=type=cache,sharing=locked,id=moby-cross-true-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-true-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            crossbuild-essential-arm64 \
            crossbuild-essential-armel \
            crossbuild-essential-armhf \
            crossbuild-essential-ppc64el \
            crossbuild-essential-s390x

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
RUN --mount=type=cache,sharing=locked,id=moby-cross-true-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-cross-true-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            libapparmor-dev:arm64 \
            libapparmor-dev:armel \
            libapparmor-dev:armhf \
            libapparmor-dev:ppc64el \
            libapparmor-dev:s390x

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
            patch \
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
COPY --from=criu          /build/ /usr/local/bin/
COPY --from=vndr          /build/ /usr/local/bin/
COPY --from=gotestsum     /build/ /usr/local/bin/
COPY --from=shfmt         /build/ /usr/local/bin/
COPY --from=runc          /build/ /usr/local/bin/
COPY --from=containerd    /build/ /usr/local/bin/
COPY --from=rootlesskit   /build/ /usr/local/bin/
COPY --from=vpnkit        /build/ /usr/local/bin/
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

FROM dev AS final
COPY . /go/src/github.com/docker/docker

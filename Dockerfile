# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.10
ARG BASE_DEBIAN_DISTRO=trixie
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"

# XX_VERSION specifies the version of the xx utility to use.
# It must be a valid tag in the docker.io/tonistiigi/xx image repository.
ARG XX_VERSION=1.10.0

# DOCKERCLI_VERSION is the version of the CLI to install in the dev-container.
# renovate: datasource=github-tags depName=docker/cli versioning=semver
ARG DOCKERCLI_VERSION=v29.9.2
ARG DOCKERCLI_REPOSITORY="https://github.com/docker/cli.git"

# cli version used for integration-cli tests
ARG DOCKERCLI_INTEGRATION_REPOSITORY="https://github.com/docker/cli.git"
ARG DOCKERCLI_INTEGRATION_VERSION=v25.0.5

# BUILDX_VERSION is the version of buildx to install in the dev container.
ARG BUILDX_VERSION=0.38.1

# COMPOSE_VERSION is the version of compose to install in the dev container.
ARG COMPOSE_VERSION=v5.7.0

ARG SYSTEMD="false"
ARG FIREWALLD="false"
ARG DOCKER_STATIC=1

# REGISTRY_VERSION is the version of the registry to use for integration tests.
# It must be a valid tag in the docker.io/library/registry image repository.
ARG REGISTRY_VERSION=3.1.2

# delve is currently only supported on linux/amd64, linux/arm64, and linux/ppc64le;
# https://github.com/go-delve/delve/blob/v1.26.0/pkg/proc/native/support_sentinel.go#L1
# https://github.com/go-delve/delve/blob/v1.26.0/pkg/proc/native/support_sentinel_linux.go#L1
#
# Remove supported, non-experimental platforms; if anything remains, it's unsupported.
ARG DELVE_SUPPORTED=${TARGETPLATFORM#linux/amd64}
ARG DELVE_SUPPORTED=${DELVE_SUPPORTED#linux/arm64}
ARG DELVE_SUPPORTED=${DELVE_SUPPORTED#linux/ppc64le}
ARG DELVE_SUPPORTED=${DELVE_SUPPORTED#linux/riscv64}
ARG DELVE_SUPPORTED=${DELVE_SUPPORTED:+"unsupported"}
ARG DELVE_SUPPORTED=${DELVE_SUPPORTED:-"supported"}

# cross compilation helper
FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

# dummy stage to make sure the image is built for deps that don't support some
# architectures
FROM --platform=$BUILDPLATFORM busybox AS build-dummy
RUN mkdir -p /build
FROM scratch AS binary-dummy
COPY --from=build-dummy /build /build

# base
FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS base
COPY --from=xx / /
# Disable collecting local telemetry, as collected by Go and Delve;
#
# - https://github.com/go-delve/delve/blob/v1.24.1/CHANGELOG.md#1231-2024-09-23
# - https://go.dev/doc/telemetry#background
RUN go telemetry off && [ "$(go telemetry)" = "off" ] || { echo "Failed to disable Go telemetry"; exit 1; }
RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN apt-get update && apt-get install --no-install-recommends -y file
ENV GOTOOLCHAIN=local

FROM base AS criu
ADD --chmod=0644 https://download.opensuse.org/repositories/devel:/tools:/criu/Debian_11/Release.key /etc/apt/trusted.gpg.d/criu.gpg.asc
RUN --mount=type=cache,sharing=locked,id=moby-criu-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-criu-aptcache,target=/var/cache/apt \
        echo 'deb https://download.opensuse.org/repositories/devel:/tools:/criu/Debian_12/ /' > /etc/apt/sources.list.d/criu.list \
        && apt-get update \
        && apt-get install -y --no-install-recommends criu \
        && install -D /usr/sbin/criu /build/criu \
        && /build/criu --version

# registry
FROM distribution/distribution:${REGISTRY_VERSION} AS registry
RUN mkdir /build && mv /bin/registry /build/registry

# frozen-images
# See also frozenImages in "testutil/environment/protect.go" (which needs to
# be updated when adding images to this list)
FROM debian:${BASE_DEBIAN_DISTRO} AS frozen-images
RUN --mount=type=cache,sharing=locked,id=moby-frozen-images-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-frozen-images-aptcache,target=/var/cache/apt \
       apt-get update && apt-get install -y --no-install-recommends \
           ca-certificates \
           curl \
           jq
# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
COPY contrib/download-frozen-image-v2.sh /
ARG TARGETARCH
ARG TARGETVARIANT
RUN /download-frozen-image-v2.sh /build \
        busybox:latest@sha256:95cf004f559831017cdf4628aaf1bb30133677be8702a8c5f2994629f637a209 \
        busybox:glibc@sha256:1f81263701cddf6402afe9f33fca0266d9fff379e59b1748f33d3072da71ee85 \
        debian:trixie-slim@sha256:c85a2732e97694ea77237c61304b3bb410e0e961dd6ee945997a06c788c545bb \
        hello-world:latest@sha256:d58e752213a51785838f9eed2b7a498ffa1cb3aa7f946dda11af39286c3db9a9 \
        arm32v7/hello-world:latest@sha256:50b8560ad574c779908da71f7ce370c0a2471c098d44d1c8f6b513c5a55eeeb1 \
        hello-world:amd64@sha256:90659bf80b44ce6be8234e6ff90a1ac34acbeb826903b02cfa0da11c82cbc042 \
        hello-world:arm64@sha256:963612c5503f3f1674f315c67089dee577d8cc6afc18565e0b4183ae355fb343

# delve
FROM base AS delve-src
WORKDIR /usr/src/delve
# DELVE_VERSION specifies the version of the Delve debugger binary
# from the https://github.com/go-delve/delve repository.
# It can be used to run Docker with a possibility of
# attaching debugger to it.
ARG DELVE_VERSION=v1.26.3
ADD https://github.com/go-delve/delve.git?ref=${DELVE_VERSION}&keep-git-dir=1 .

FROM base AS delve-supported
WORKDIR /usr/src/delve
ARG TARGETPLATFORM
RUN --mount=from=delve-src,src=/usr/src/delve,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=delve-build-$TARGETPLATFORM \
    --mount=type=cache,target=/go/pkg/mod <<EOT
  set -ex
  xx-go build -o /build/dlv ./cmd/dlv
  xx-verify /build/dlv
EOT

FROM binary-dummy AS delve-unsupported
FROM delve-${DELVE_SUPPORTED} AS delve

FROM base AS gowinres
# GOWINRES_VERSION defines go-winres tool version
ARG GOWINRES_VERSION=v0.3.1
ADD https://github.com/tc-hib/go-winres.git?ref=${GOWINRES_VERSION}&keep-git-dir=1 /go/src/github.com/tc-hib/go-winres
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        cd /go/src/github.com/tc-hib/go-winres && \
        GOBIN=/build CGO_ENABLED=0 go install . \
     && /build/go-winres --help

# containerd
FROM base AS containerd-src
WORKDIR /usr/src/containerd
# CONTAINERD_VERSION is the version of containerd to use for CI and static binaries.
# It is used to build containerd binaries, and used for the integration tests.
# The distributed docker .deb and .rpm packages depend on a separate (containerd.io)
# package, which may be a different version than specified here.
ARG CONTAINERD_VERSION=v2.4.2
ADD https://github.com/containerd/containerd.git?ref=${CONTAINERD_VERSION}&keep-git-dir=1 .

FROM base AS containerd-build
WORKDIR /go/src/github.com/containerd/containerd
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=moby-containerd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-containerd-aptcache,target=/var/cache/apt \
        apt-get update && xx-apt-get install -y --no-install-recommends \
            gcc \
            pkg-config
ARG DOCKER_STATIC
RUN --mount=from=containerd-src,src=/usr/src/containerd,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=containerd-build-$TARGETPLATFORM <<EOT
  set -e

  make_flags=
  verify_flags=
  cgo_enabled=1
  if [ "$DOCKER_STATIC" = "1" ]; then
      make_flags=STATIC=1
      verify_flags=--static
      cgo_enabled=0
  fi

  set -x
  xx-go --wrap
  binaries="bin/containerd bin/ctr"
  exe_suffix=
  if [ "$(xx-info os)" = "windows" ]; then
      exe_suffix=.exe
  else
      binaries="$binaries bin/containerd-shim-runc-v2"
  fi
  CC="$(xx-info)-gcc" CGO_ENABLED=$cgo_enabled make $make_flags BUILDTAGS=grpcnotrace $binaries
  mkdir /build
  for binary in $binaries; do
      xx-verify $verify_flags "$binary"
      mv "$binary" "/build/${binary##*/}$exe_suffix"
  done
EOT

FROM base AS runhcs-src
WORKDIR /usr/src/hcsshim
# Override the shim version tested with the selected containerd release.
ARG RUNHCS_REF
RUN --mount=from=containerd-src,src=/usr/src/containerd,target=/usr/src/containerd <<EOT
  set -ex
  if [ -z "$RUNHCS_REF" ]; then
      RUNHCS_REF=$(cat /usr/src/containerd/script/setup/runhcs-version)
  fi
  git init .
  git remote add origin https://github.com/Microsoft/hcsshim.git
  git fetch --depth=1 origin "$RUNHCS_REF"
  git checkout -q FETCH_HEAD
EOT

FROM base AS runhcs-build
WORKDIR /usr/src/hcsshim
ARG TARGETPLATFORM
RUN --mount=from=runhcs-src,src=/usr/src/hcsshim,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=runhcs-build-$TARGETPLATFORM <<EOT
  set -ex
  CGO_ENABLED=0 xx-go build -mod=vendor -o /build/containerd-shim-runhcs-v1.exe ./cmd/containerd-shim-runhcs-v1
  xx-verify --static /build/containerd-shim-runhcs-v1.exe
EOT

FROM containerd-build AS containerd-linux
FROM containerd-build AS containerd-windows
COPY --from=runhcs-build /build/ /build/
FROM containerd-${TARGETOS} AS containerd

FROM base AS golangci_lint
ARG GOLANGCI_LINT_VERSION=v2.14.0
ADD https://github.com/golangci/golangci-lint.git?ref=${GOLANGCI_LINT_VERSION}&keep-git-dir=1 /go/src/github.com/golangci/golangci-lint
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        cd /go/src/github.com/golangci/golangci-lint && \
        GOBIN=/build CGO_ENABLED=0 go install ./cmd/golangci-lint \
     && /build/golangci-lint --version

FROM base AS gotestsum
# GOTESTSUM_VERSION is the version of gotest.tools/gotestsum to install.
ARG GOTESTSUM_VERSION=v1.13.0
ADD https://github.com/gotestyourself/gotestsum.git?ref=${GOTESTSUM_VERSION}&keep-git-dir=1 /go/src/gotest.tools/gotestsum
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        cd /go/src/gotest.tools/gotestsum && \
        GOBIN=/build CGO_ENABLED=0 go install . \
     && /build/gotestsum --version

FROM base AS shfmt
ARG SHFMT_VERSION=v3.14.1
ADD https://github.com/mvdan/sh.git?ref=${SHFMT_VERSION}&keep-git-dir=1 /go/src/mvdan.cc/sh
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        cd /go/src/mvdan.cc/sh && \
        GOBIN=/build CGO_ENABLED=0 go install ./cmd/shfmt \
     && /build/shfmt --version

FROM base AS gopls
# No ARG GOPLS_VERSION, as gopls is only used for devcontainer
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
        GOBIN=/build CGO_ENABLED=0 go install "golang.org/x/tools/gopls@latest" \
     && /build/gopls version

FROM base AS dockercli
WORKDIR /go/src/github.com/docker/cli
ARG DOCKERCLI_REPOSITORY
ARG DOCKERCLI_VERSION
ARG TARGETPLATFORM
RUN --mount=source=hack/dockerfile/cli.sh,target=/download-or-build-cli.sh \
    --mount=type=cache,id=dockercli-git-$TARGETPLATFORM,sharing=locked,target=./.git \
    --mount=type=cache,target=/root/.cache/go-build,id=dockercli-build-$TARGETPLATFORM \
        rm -f ./.git/*.lock \
     && /download-or-build-cli.sh ${DOCKERCLI_VERSION} ${DOCKERCLI_REPOSITORY} /build \
     && /build/docker --version \
     && /build/docker completion bash >/completion.bash

FROM base AS dockercli-integration
WORKDIR /go/src/github.com/docker/cli
ARG DOCKERCLI_INTEGRATION_REPOSITORY
ARG DOCKERCLI_INTEGRATION_VERSION
ARG TARGETPLATFORM
RUN --mount=source=hack/dockerfile/cli.sh,target=/download-or-build-cli.sh \
    --mount=type=cache,id=dockercli-git-$TARGETPLATFORM,sharing=locked,target=./.git \
    --mount=type=cache,target=/root/.cache/go-build,id=dockercli-build-$TARGETPLATFORM \
        rm -f ./.git/*.lock \
     && /download-or-build-cli.sh ${DOCKERCLI_INTEGRATION_VERSION} ${DOCKERCLI_INTEGRATION_REPOSITORY} /build \
     && /build/docker --version

# runc
FROM base AS runc-src
WORKDIR /usr/src/runc
# RUNC_VERSION sets the version of runc to install in the dev-container.
# This version should usually match the version that is used by the containerd version
# that is used. If you need to update runc, open a pull request in the containerd
# project first, and update both after that is merged.
# renovate: datasource=github-tags depName=opencontainers/runc
ARG RUNC_VERSION=v1.5.2
ADD https://github.com/opencontainers/runc.git?ref=${RUNC_VERSION}&keep-git-dir=1 .

FROM base AS runc-build
WORKDIR /go/src/github.com/opencontainers/runc
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=moby-runc-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-runc-aptcache,target=/var/cache/apt \
        apt-get update && xx-apt-get install -y --no-install-recommends \
            gcc \
            libc6-dev \
            linux-libc-dev \
            libseccomp-dev \
            pkg-config
ARG DOCKER_STATIC
RUN --mount=from=runc-src,src=/usr/src/runc,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=runc-build-$TARGETPLATFORM <<EOT
  set -e
  xx-go --wrap

  target=runc
  verify_flags=
  if [ "$DOCKER_STATIC" = "1" ]; then
    target=static
    verify_flags=--static
  fi

  set -x
  CGO_ENABLED=1 make RUNC_BUILDTAGS="-libpathrs" "$target"
  xx-verify $verify_flags runc
  mkdir /build
  mv runc /build/
EOT

FROM runc-build AS runc-linux
FROM binary-dummy AS runc-windows
FROM runc-${TARGETOS} AS runc

# tini
FROM base AS tini-src
WORKDIR /usr/src/tini
# TINI_VERSION specifies the version of tini (docker-init) to build. This
# binary is used when starting containers with the `--init` option.
ARG TINI_VERSION=v0.19.0
ADD https://github.com/krallin/tini.git?ref=${TINI_VERSION}&keep-git-dir=1 .

FROM base AS tini-build
WORKDIR /go/src/github.com/krallin/tini
RUN --mount=type=cache,sharing=locked,id=moby-tini-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-tini-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends cmake
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=moby-tini-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-tini-aptcache,target=/var/cache/apt \
        xx-apt-get install -y --no-install-recommends \
            gcc \
            libc6-dev \
            linux-libc-dev \
            pkg-config
RUN --mount=from=tini-src,src=/usr/src/tini,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=tini-build-$TARGETPLATFORM <<EOT
  set -ex
  CC=$(xx-info)-gcc cmake .
  make tini-static
  xx-verify --static tini-static
  mkdir /build
  mv tini-static /build/docker-init
EOT

FROM tini-build AS tini-linux
FROM binary-dummy AS tini-windows
FROM tini-${TARGETOS} AS tini

# rootlesskit
FROM base AS rootlesskit-src
WORKDIR /usr/src/rootlesskit

# ROOTLESSKIT_VERSION is the version of rootlesskit to install in the dev-container.
# renovate: datasource=github-tags depName=rootless-containers/rootlesskit
ARG ROOTLESSKIT_VERSION=v3.3.0
ADD https://github.com/rootless-containers/rootlesskit.git?ref=${ROOTLESSKIT_VERSION}&keep-git-dir=1 .

FROM base AS rootlesskit-build
WORKDIR /go/src/github.com/rootless-containers/rootlesskit
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=moby-rootlesskit-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-rootlesskit-aptcache,target=/var/cache/apt \
        apt-get update && xx-apt-get install -y --no-install-recommends \
            gcc \
            libc6-dev \
            linux-libc-dev \
            pkg-config
ARG DOCKER_STATIC
RUN --mount=from=rootlesskit-src,src=/usr/src/rootlesskit,rw \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=rootlesskit-build-$TARGETPLATFORM <<EOT
  set -e

  if [ "$DOCKER_STATIC" = "1" ]; then
      set -x
      export CGO_ENABLED=0
      xx-go build -tags=urfave_cli_no_docs  -o /build/rootlesskit ./cmd/rootlesskit
      xx-verify --static /build/rootlesskit
  else
      set -x
      export CGO_ENABLED=1
      xx-go build -o /build/rootlesskit -ldflags='-linkmode=external' ./cmd/rootlesskit
      xx-verify /build/rootlesskit
  fi
EOT
COPY --link ./contrib/dockerd-rootless.sh /build/
COPY --link ./contrib/dockerd-rootless-setuptool.sh /build/

FROM rootlesskit-build AS rootlesskit-linux
FROM binary-dummy AS rootlesskit-windows
FROM rootlesskit-${TARGETOS} AS rootlesskit

FROM base AS crun
# CRUN_VERSION is the version of crun to install in the dev-container.
# renovate: datasource=github-tags depName=containers/crun
ARG CRUN_VERSION=1.30.1
RUN --mount=type=cache,sharing=locked,id=moby-crun-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-crun-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            autoconf \
            automake \
            build-essential \
            libcap-dev \
            libjson-c-dev \
            libprotobuf-c-dev \
            libseccomp-dev \
            libsystemd-dev \
            libtool \
            pkgconf \
            python3 \
            ;
WORKDIR /tmp/crun-build
ADD https://github.com/containers/crun.git?ref=${CRUN_VERSION}&keep-git-dir=1 .
RUN ./autogen.sh && \
    ./configure --bindir=/build && \
    make -j install

# containerutility
FROM base AS containerutil-src
WORKDIR /usr/src/containerutil
ARG CONTAINERUTILITY_VERSION=aa1ba87e99b68e0113bd27ec26c60b88f9d4ccd9
ADD https://github.com/docker-archive/windows-container-utility.git?commit=${CONTAINERUTILITY_VERSION}&keep-git-dir=1 .

FROM base AS containerutil-build
WORKDIR /usr/src/containerutil
ARG TARGETPLATFORM
RUN xx-apt-get install -y --no-install-recommends \
        gcc \
        g++ \
        libc6-dev \
        pkg-config
RUN --mount=from=containerutil-src,src=/usr/src/containerutil,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=containerutil-build-$TARGETPLATFORM <<EOT
  set -ex
  CC="$(xx-info)-gcc" CXX="$(xx-info)-g++" make
  xx-verify --static containerutility.exe
  mkdir /build
  mv containerutility.exe /build/
EOT

FROM binary-dummy AS containerutil-linux
FROM containerutil-build AS containerutil-windows-amd64
FROM containerutil-windows-${TARGETARCH} AS containerutil-windows
FROM containerutil-${TARGETOS} AS containerutil
FROM docker/buildx-bin:${BUILDX_VERSION} AS buildx
FROM docker/compose-bin:${COMPOSE_VERSION} AS compose

FROM base AS dev-systemd-false
COPY --link --from=frozen-images /build/ /docker-frozen-images
COPY --link --from=delve         /build/ /usr/local/bin/
COPY --link --from=gowinres      /build/ /usr/local/bin/
COPY --link --from=tini          /build/ /usr/local/bin/
COPY --link --from=registry      /build/ /usr/local/bin/

# Skip the CRIU stage for now, as the opensuse package repository is sometimes
# unstable, and we're currently not using it in CI.
#
# FIXME(thaJeztah): re-enable this stage when https://github.com/moby/moby/issues/38963 is resolved (see https://github.com/moby/moby/pull/38984)
# COPY --link --from=criu          /build/ /usr/local/bin/
COPY --link --from=gotestsum     /build/ /usr/local/bin/
COPY --link --from=golangci_lint /build/ /usr/local/bin/
COPY --link --from=shfmt         /build/ /usr/local/bin/
COPY --link --from=runc          /build/ /usr/local/bin/
COPY --link --from=containerd    /build/ /usr/local/bin/
COPY --link --from=rootlesskit   /build/ /usr/local/bin/
COPY --link --from=containerutil /build/ /usr/local/bin/
COPY --link --from=crun          /build/ /usr/local/bin/
COPY --link hack/dockerfile/etc/docker/  /etc/docker/
COPY --link --from=buildx        /buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --link --from=compose       /docker-compose /usr/libexec/docker/cli-plugins/docker-compose

ENV PATH=/usr/local/cli:$PATH
ENV TEST_CLIENT_BINARY=/usr/local/cli-integration/docker
ENV CONTAINERD_ADDRESS=/run/docker/containerd/containerd.sock
ENV CONTAINERD_NAMESPACE=moby
WORKDIR /usr/src/moby
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
ENTRYPOINT ["hack/dind-systemd"]

FROM dev-systemd-${SYSTEMD} AS dev-firewalld-false

FROM dev-systemd-true AS dev-firewalld-true
RUN --mount=type=cache,sharing=locked,id=moby-dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            firewalld

FROM dev-firewalld-${FIREWALLD} AS dev-base
RUN groupadd -r docker
RUN useradd --create-home --gid docker unprivilegeduser \
 && mkdir -p /home/unprivilegeduser/.local/share/docker \
 && chown -R unprivilegeduser /home/unprivilegeduser
# Let us use a .bashrc file
RUN ln -sfv /usr/src/moby/.bashrc ~/.bashrc
# Activate bash completion
RUN echo "source /usr/share/bash-completion/bash_completion" >> /etc/bash.bashrc
RUN ldconfig
# Set dev environment as safe git directory to prevent "dubious ownership" errors
# when bind-mounting the source into the dev-container. See https://github.com/moby/moby/pull/44930
RUN git config --global --add safe.directory /usr/src/moby
# This should only install packages that are specifically needed for the dev environment and nothing else
# Do you really need to add another package here? Can it be done in a different build stage?
RUN --mount=type=cache,sharing=locked,id=moby-dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends \
            apparmor \
            bash-completion \
            bzip2 \
            e2fsprogs \
            fuse-overlayfs \
            inetutils-ping \
            iproute2 \
            iptables \
            nftables \
            jq \
            libcap2-bin \
            libnet1 \
            libnftables-dev \
            libnl-3-200 \
            libprotobuf-c1 \
            libyajl2 \
            nano \
            net-tools \
            netcat-openbsd \
            patch \
            pigz \
            sudo \
            systemd-journal-remote \
            thin-provisioning-tools \
            uidmap \
            vim \
            vim-common \
            xfsprogs \
            xz-utils \
            zip \
            zstd
RUN --mount=type=cache,sharing=locked,id=moby-dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install --no-install-recommends -y \
            gcc \
            pkg-config \
            libseccomp-dev \
            libsystemd-dev \
            yamllint
COPY --link --from=dockercli             /build/ /usr/local/cli
COPY --link --from=dockercli             /completion.bash /etc/bash_completion.d/docker
COPY --link --from=dockercli-integration /build/ /usr/local/cli-integration

FROM base AS build
COPY --from=gowinres /build/ /usr/local/bin/
WORKDIR /usr/src/moby
ENV CGO_ENABLED=1
RUN --mount=type=cache,sharing=locked,id=moby-build-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-build-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install --no-install-recommends -y \
            clang \
            lld \
            llvm \
            icoutils
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=moby-build-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=moby-build-aptcache,target=/var/cache/apt \
        xx-apt-get install --no-install-recommends -y \
            gcc \
            libc6-dev \
            linux-libc-dev \
            libnftables-dev \
            libseccomp-dev \
            libsystemd-dev \
            pkg-config
ARG DOCKER_BUILDTAGS
ARG DOCKER_DEBUG
ARG DOCKER_GITCOMMIT=HEAD
ARG DOCKER_LDFLAGS
ARG DOCKER_STATIC
ARG VERSION
ARG PLATFORM
ARG PRODUCT
ARG DEFAULT_PRODUCT_LICENSE
ARG PACKAGER_NAME
# PREFIX overrides DEST dir in make.sh script otherwise it fails because of
# read only mount in current work dir
ENV PREFIX=/tmp
RUN <<EOT
  set -ex
  # in bullseye arm64 target does not link with lld so configure it to use ld instead
  if [ "$(xx-info arch)" = "arm64" ]; then
    XX_CC_PREFER_LINKER=ld xx-clang --setup-target-triple
  fi
EOT
RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=moby-build-$TARGETPLATFORM <<EOT
  set -e

  target=dynbinary
  verify_flags=
  exe_suffix=
  if [ "$DOCKER_STATIC" = "1" ]; then
      target=binary
      verify_flags=--static
  fi
  if [ "$(xx-info os)" = "windows" ]; then
      exe_suffix=.exe
  fi

  set -x
  xx-go --wrap
  PKG_CONFIG=$(xx-go env PKG_CONFIG) ./hack/make.sh "$target"
  xx-verify $verify_flags "/tmp/bundles/${target}-daemon/dockerd${exe_suffix}"
  if [ "$(xx-info os)" = "linux" ]; then
      xx-verify $verify_flags "/tmp/bundles/${target}-daemon/docker-proxy"
  fi
  mkdir /build
  mv /tmp/bundles/${target}-daemon/* /build/
EOT

# usage:
# > docker buildx bake binary
# > DOCKER_STATIC=0 docker buildx bake binary
# or
# > make binary
# > make dynbinary
FROM scratch AS binary
COPY --from=build /build/ /

# usage:
# > docker buildx bake all
FROM scratch AS all
COPY --link --from=tini          /build/ /
COPY --link --from=runc          /build/ /
COPY --link --from=containerd    /build/ /
COPY --link --from=rootlesskit   /build/ /
COPY --link --from=containerutil /build/ /
COPY --link --from=build         /build  /

# Tools and fixtures for running integration tests directly on a Linux host.
# Build this for the host architecture; the Go toolchain and gotestsum run there.
# The daemon and tests are compiled on the host by hack/make.sh.
FROM scratch AS test-integration-deps
COPY --link --from=base          /usr/local/go/ /usr/local/go/
COPY --link --from=tini          /build/ /usr/local/bin/
COPY --link --from=runc          /build/ /usr/local/bin/
COPY --link --from=containerd    /build/ /usr/local/bin/
COPY --link --from=rootlesskit   /build/ /usr/local/bin/
COPY --link --from=dockercli     /build/ /usr/local/bin/
COPY --link --from=gotestsum     /build/ /usr/local/bin/
COPY --link --from=registry      /build/ /usr/local/bin/
COPY --link --from=frozen-images /build/ /docker-frozen-images/

# smoke tests
# usage:
# > docker buildx bake binary-smoketest
FROM base AS smoketest
WORKDIR /usr/local/bin
COPY --from=build /build .
RUN <<EOT
  set -ex
  file dockerd
  dockerd --version
  file docker-proxy
  docker-proxy --version
EOT

# devcontainer is a stage used by .devcontainer/devcontainer.json
FROM dev-base AS devcontainer
COPY --link . .
COPY --link --from=gopls         /build/ /usr/local/bin/

# usage:
# > docker buildx bake dind
# > docker run -d --restart always --privileged --name devdind -p 12375:2375 docker-dind --debug --host=tcp://0.0.0.0:2375 --tlsverify=false
FROM docker:dind AS dind
COPY --link --from=dockercli /build/docker /usr/local/bin/
COPY --link --from=buildx    /buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --link --from=compose   /docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --link --from=all       / /usr/local/bin/

# usage:
# > make shell
# > SYSTEMD=true make shell
FROM dev-base AS dev
COPY --link . .

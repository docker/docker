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
ARG GO_VERSION=1.13.1
ARG DEBIAN_FRONTEND=noninteractive

FROM golang:${GO_VERSION}-stretch AS base
ARG APT_MIRROR
RUN sed -ri "s/(httpredir|deb).debian.org/${APT_MIRROR:-deb.debian.org}/g" /etc/apt/sources.list \
 && sed -ri "s/(security).debian.org/${APT_MIRROR:-security.debian.org}/g" /etc/apt/sources.list
ENV GO111MODULE=off

FROM base AS criu
ARG DEBIAN_FRONTEND
# Install CRIU for checkpoint/restore support
ENV CRIU_VERSION 3.12
# Install dependency packages specific to criu
RUN apt-get update && apt-get install -y --no-install-recommends \
	libnet-dev \
	libprotobuf-c-dev \
	libprotobuf-dev \
	libnl-3-dev \
	libcap-dev \
	protobuf-compiler \
	protobuf-c-compiler \
	python-protobuf \
	&& rm -rf /var/lib/apt/lists/*
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
ENV GO_SWAGGER_COMMIT c28258affb0b6251755d92489ef685af8d4ff3eb
RUN set -x \
	&& export GOPATH="$(mktemp -d)" \
	&& git clone https://github.com/go-swagger/go-swagger.git "$GOPATH/src/github.com/go-swagger/go-swagger" \
	&& (cd "$GOPATH/src/github.com/go-swagger/go-swagger" && git checkout -q "$GO_SWAGGER_COMMIT") \
# FIXME dirty hack to update the "header" template, which is not allowed by code ("Cannot overwrite protected template header")
	&& sed -i 's#package {{.Package}}#package {{.Package}} // import "github.com/docker/docker/api/{{.Package}}"#' "$GOPATH/src/github.com/go-swagger/go-swagger/generator/templates/header.gotmpl" \
	&& go get -d github.com/kevinburke/go-bindata/go-bindata \
	&& go build -o /usr/local/bin/go-bindata github.com/kevinburke/go-bindata/go-bindata \
	&& go generate $GOPATH/src/github.com/go-swagger/go-swagger/generator \
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
RUN dpkg --add-architecture armhf
RUN dpkg --add-architecture arm64
RUN dpkg --add-architecture armel
RUN if [ "$(go env GOHOSTARCH)" = "amd64" ]; then \
	apt-get update && apt-get install -y --no-install-recommends \
		crossbuild-essential-armhf \
		crossbuild-essential-arm64 \
		crossbuild-essential-armel \
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
		libseccomp-dev:armhf \
		libseccomp-dev:arm64 \
		libseccomp-dev:armel \
		libapparmor-dev:armhf \
		libapparmor-dev:arm64 \
		libapparmor-dev:armel \
		# install this arches seccomp here due to compat issues with the v0 builder
		# This is as opposed to inheriting from runtime-dev-cross-false
		libapparmor-dev \
		libseccomp-dev \
		&& rm -rf /var/lib/apt/lists/*; \
	fi

FROM runtime-dev-cross-${CROSS} AS runtime-dev

FROM base AS tomlv
ENV INSTALL_BINARY_NAME=tomlv
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS vndr
ENV INSTALL_BINARY_NAME=vndr
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS containerd
ARG DEBIAN_FRONTEND
RUN apt-get update && apt-get install -y --no-install-recommends \
	btrfs-tools \
	&& rm -rf /var/lib/apt/lists/*
ENV INSTALL_BINARY_NAME=containerd
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS proxy
ENV INSTALL_BINARY_NAME=proxy
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS golangci_lint
ENV INSTALL_BINARY_NAME=golangci_lint
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS gotestsum
ENV INSTALL_BINARY_NAME=gotestsum
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS dockercli
ENV INSTALL_BINARY_NAME=dockercli
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM runtime-dev AS runc
ENV INSTALL_BINARY_NAME=runc
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS tini
ARG DEBIAN_FRONTEND
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
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build/ ./install.sh $INSTALL_BINARY_NAME
COPY ./contrib/dockerd-rootless.sh /build

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
	btrfs-tools \
	iptables \
	jq \
	libcap2-bin \
	libdevmapper-dev \
	libudev-dev \
	libsystemd-dev \
	binutils-mingw-w64 \
	g++-mingw-w64-x86-64 \
	net-tools \
	pigz \
	python3-pip \
	python3-setuptools \
	python3-wheel \
	thin-provisioning-tools \
	vim \
	vim-common \
	xfsprogs \
	zip \
	bzip2 \
	xz-utils \
	libprotobuf-c1 \
	libnet1 \
	libnl-3-200 \
	&& rm -rf /var/lib/apt/lists/*

RUN pip3 install yamllint==1.16.0

COPY --from=swagger /build/swagger* /usr/local/bin/
COPY --from=frozen-images /build/ /docker-frozen-images
COPY --from=golangci_lint /build/ /usr/local/bin/
COPY --from=gotestsum /build/ /usr/local/bin/
COPY --from=tomlv /build/ /usr/local/bin/
COPY --from=vndr /build/ /usr/local/bin/
COPY --from=tini /build/ /usr/local/bin/
COPY --from=runc /build/ /usr/local/bin/
COPY --from=containerd /build/ /usr/local/bin/
COPY --from=proxy /build/ /usr/local/bin/
COPY --from=dockercli /build/ /usr/local/cli
COPY --from=registry /build/registry* /usr/local/bin/
COPY --from=criu /build/ /usr/local/
COPY --from=rootlesskit /build/ /usr/local/bin/
COPY --from=djs55/vpnkit@sha256:e508a17cfacc8fd39261d5b4e397df2b953690da577e2c987a47630cd0c42f8e /vpnkit /usr/local/bin/vpnkit.x86_64

ENV PATH=/usr/local/cli:$PATH
ENV DOCKER_BUILDTAGS apparmor seccomp selinux
WORKDIR /go/src/github.com/docker/docker
VOLUME /var/lib/docker
# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

FROM dev AS final
# Upload docker source
COPY . /go/src/github.com/docker/docker

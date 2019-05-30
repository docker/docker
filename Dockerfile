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

FROM golang:1.12.5 AS base
# allow replacing httpredir or deb mirror
ARG APT_MIRROR=deb.debian.org
RUN sed -ri "s/(httpredir|deb).debian.org/$APT_MIRROR/g" /etc/apt/sources.list

FROM base AS criu
# Install CRIU for checkpoint/restore support
ENV CRIU_VERSION 3.11
# Install dependency packages specific to criu
RUN apt-get update && apt-get install -y \
	libnet-dev \
	libprotobuf-c0-dev \
	libprotobuf-dev \
	libnl-3-dev \
	libcap-dev \
	protobuf-compiler \
	protobuf-c-compiler \
	python-protobuf \
	&& mkdir -p /usr/src/criu \
	&& curl -sSL https://github.com/checkpoint-restore/criu/archive/v${CRIU_VERSION}.tar.gz | tar -C /usr/src/criu/ -xz --strip-components=1 \
	&& cd /usr/src/criu \
	&& make \
	&& make PREFIX=/build/ install-criu

FROM base AS registry
ENV REGISTRY_COMMIT 47a064d4195a9b56133891bbb13620c3ac83a827
RUN set -x \
	&& export GOPATH="$(mktemp -d)" \
	&& git clone https://github.com/docker/distribution.git "$GOPATH/src/github.com/docker/distribution" \
	&& (cd "$GOPATH/src/github.com/docker/distribution" && git checkout -q "$REGISTRY_COMMIT") \
	&& GOPATH="$GOPATH/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH" \
		go build -buildmode=pie -o /build/registry-v2 github.com/docker/distribution/cmd/registry \
	&& rm -rf "$GOPATH"



FROM base AS docker-py
# Get the "docker-py" source so we can run their integration tests
ENV DOCKER_PY_COMMIT ac922192959870774ad8428344d9faa0555f7ba6
RUN git clone https://github.com/docker/docker-py.git /build \
	&& cd /build \
	&& git checkout -q $DOCKER_PY_COMMIT



FROM base AS swagger
# Install go-swagger for validating swagger.yaml
ENV GO_SWAGGER_COMMIT c28258affb0b6251755d92489ef685af8d4ff3eb
RUN set -x \
	&& export GOPATH="$(mktemp -d)" \
	&& git clone https://github.com/go-swagger/go-swagger.git "$GOPATH/src/github.com/go-swagger/go-swagger" \
	&& (cd "$GOPATH/src/github.com/go-swagger/go-swagger" && git checkout -q "$GO_SWAGGER_COMMIT") \
	&& go build -o /build/swagger github.com/go-swagger/go-swagger/cmd/swagger \
	&& rm -rf "$GOPATH"


FROM base AS frozen-images
RUN apt-get update && apt-get install -y jq ca-certificates --no-install-recommends
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
RUN dpkg --add-architecture armhf
RUN dpkg --add-architecture arm64
RUN dpkg --add-architecture armel
RUN if [ "$(go env GOHOSTARCH)" = "amd64" ]; then \
	apt-get update \
	&& apt-get install -y --no-install-recommends \
		crossbuild-essential-armhf \
		crossbuild-essential-arm64 \
		crossbuild-essential-armel; \
	fi

FROM cross-${CROSS} as dev-base

FROM dev-base AS runtime-dev-cross-false
RUN apt-get update && apt-get install -y \
	libapparmor-dev \
	libseccomp-dev

FROM cross-true AS runtime-dev-cross-true
# These crossbuild packages rely on gcc-<arch>, but this doesn't want to install
# on non-amd64 systems.
# Additionally, the crossbuild-amd64 is currently only on debian:buster, so
# other architectures cannnot crossbuild amd64.
RUN if [ "$(go env GOHOSTARCH)" = "amd64" ]; then \
	apt-get update \
	&& apt-get install -y \
		libseccomp-dev:armhf \
		libseccomp-dev:arm64 \
		libseccomp-dev:armel \
		libapparmor-dev:armhf \
		libapparmor-dev:arm64 \
		libapparmor-dev:armel \
		# install this arches seccomp here due to compat issues with the v0 builder
		# This is as opposed to inheriting from runtime-dev-cross-false
		libapparmor-dev \
		libseccomp-dev; \
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
RUN apt-get update && apt-get install -y btrfs-tools
ENV INSTALL_BINARY_NAME=containerd
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM dev-base AS proxy
ENV INSTALL_BINARY_NAME=proxy
COPY hack/dockerfile/install/install.sh ./install.sh
COPY hack/dockerfile/install/$INSTALL_BINARY_NAME.installer ./
RUN PREFIX=/build ./install.sh $INSTALL_BINARY_NAME

FROM base AS gometalinter
ENV INSTALL_BINARY_NAME=gometalinter
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
RUN apt-get update && apt-get install -y cmake vim-common
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
RUN apt-get update && apt-get install -y \
	apparmor \
	aufs-tools \
	bash-completion \
	btrfs-tools \
	iptables \
	jq \
	libcap2-bin \
	libdevmapper-dev \
# libffi-dev and libssl-dev appear to be required for compiling paramiko on s390x/ppc64le
	libffi-dev \
	libssl-dev \
	libudev-dev \
	libsystemd-dev \
	binutils-mingw-w64 \
	g++-mingw-w64-x86-64 \
	net-tools \
	pigz \
	python-backports.ssl-match-hostname \
	python-dev \
# python-cffi appears to be required for compiling paramiko on s390x/ppc64le
	python-cffi \
	python-mock \
	python-pip \
	python-requests \
	python-setuptools \
	python-websocket \
	python-wheel \
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
	--no-install-recommends
COPY --from=swagger /build/swagger* /usr/local/bin/
COPY --from=frozen-images /build/ /docker-frozen-images
COPY --from=gometalinter /build/ /usr/local/bin/
COPY --from=tomlv /build/ /usr/local/bin/
COPY --from=vndr /build/ /usr/local/bin/
COPY --from=tini /build/ /usr/local/bin/
COPY --from=runc /build/ /usr/local/bin/
COPY --from=containerd /build/ /usr/local/bin/
COPY --from=proxy /build/ /usr/local/bin/
COPY --from=dockercli /build/ /usr/local/cli
COPY --from=registry /build/registry* /usr/local/bin/
COPY --from=criu /build/ /usr/local/
COPY --from=docker-py /build/ /docker-py
# TODO: This is for the docker-py tests, which shouldn't really be needed for
# this image, but currently CI is expecting to run this image. This should be
# split out into a separate image, including all the `python-*` deps installed
# above.
RUN cd /docker-py \
	&& pip install docker-pycreds==0.4.0 \
	&& pip install paramiko==2.4.2 \
	&& pip install yamllint==1.5.0 \
	&& pip install -r test-requirements.txt
COPY --from=rootlesskit /build/ /usr/local/bin/
COPY --from=djs55/vpnkit@sha256:e508a17cfacc8fd39261d5b4e397df2b953690da577e2c987a47630cd0c42f8e /vpnkit /usr/local/bin/vpnkit.x86_64

ENV PATH=/usr/local/cli:$PATH
ENV DOCKER_BUILDTAGS apparmor seccomp selinux
# Options for hack/validate/gometalinter
ENV GOMETALINTER_OPTS="--deadline=2m"
WORKDIR /go/src/github.com/docker/docker
VOLUME /var/lib/docker
# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

FROM dev AS final
# Upload docker source
COPY . /go/src/github.com/docker/docker

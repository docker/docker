#!/bin/bash
set -e

cat <<-'EOF'
#
# DO NOT EDIT - GENERATED BY project/generate-dockerfile.sh
#
# This file describes the standard way to build Docker, using docker
#
# Usage:
#
# # Assemble the full dev environment. This is slow the first time.
# docker build -t docker .
#
# # Mount your source in an interactive container for quick testing:
# docker run -v `pwd`:/go/src/github.com/docker/docker --privileged -i -t docker bash
#
# # Run the test suite:
# docker run --privileged docker hack/make.sh test
#
# # Publish a release:
# docker run --privileged \
#  -e AWS_S3_BUCKET=baz \
#  -e AWS_ACCESS_KEY=foo \
#  -e AWS_SECRET_KEY=bar \
#  -e GPG_PASSPHRASE=gloubiboulga \
#  docker hack/release.sh
#
# Note: Apparmor used to mess with privileged mode, but this is no longer
# the case. Therefore, you don't have to disable it anymore.
#

EOF

if [ "$ARCH" == "s390x" ] ; then
	cat <<-'EOF'
		FROM ibmcom/gccgo_z:latest
		ENV CGO_ENABLED 1
		ENV USE_GCCGO  1
	EOF
elif [ "$ARCH" == "ppc64le" ] ; then
	cat <<-'EOF'
		FROM ibmcom/gccgo_p:latest
		ENV CGO_ENABLED 1
		ENV USE_GCCGO  1
	EOF
elif [ "$ARCH" == "x86_64" ] ; then
	cat <<-'EOF'
		FROM ibmcom/gccgo:latest
		ENV CGO_ENABLED 1
		ENV USE_GCCGO  1
	EOF
else
	cat <<-'EOF'
		FROM ubuntu:14.04
	EOF
fi

cat <<-'EOF'
MAINTAINER Tianon Gravi <admwiggin@gmail.com> (@tianon)
EOF

if [ -z "$ARCH" ]; then
	cat <<-'EOF'

		RUN    apt-key adv --keyserver hkp://p80.pool.sks-keyservers.net:80 --recv-keys E871F18B51E0147C77796AC81196BA81F6B0FC61
		RUN    echo deb http://ppa.launchpad.net/zfs-native/stable/ubuntu trusty main > /etc/apt/sources.list.d/zfs.list

	EOF
fi

cat <<-'EOF'
# Packaged dependencies
RUN apt-get update && apt-get install -y \
       apparmor \
       aufs-tools \
       automake \
       bash-completion \
       btrfs-tools \
       build-essential \
       createrepo \
       curl \
       dpkg-sig \
       git \
       iptables \
       libapparmor-dev \
       libcap-dev \
       libsqlite3-dev \
       mercurial \
       parallel \
       python-mock \
       python-pip \
       python-websocket \
       reprepro \
       ruby \
       ruby-dev \
       s3cmd \
       --no-install-recommends

# Get lvm2 source for compiling statically
RUN git clone -b v2_02_103 https://git.fedorahosted.org/git/lvm2.git /usr/local/lvm2
# see https://git.fedorahosted.org/cgit/lvm2.git/refs/tags for release tags

EOF

if [ -z "$ARCH" ]; then
	cat <<-'EOF'
	       RUN apt-get update && apt-get install -y \
	       ubuntu-zfs \
	       libzfs-dev \
	       --no-install-recommends
	EOF
fi
if [ -n "$ARCH" ]; then
	cat <<-'EOF'
		RUN rm -rf /usr/local/lvm2
		RUN git clone --no-checkout git://git.fedorahosted.org/git/lvm2.git /usr/local/lvm2 && cd /usr/local/lvm2 && git checkout -q v2_02_103
		RUN curl -o /usr/local/lvm2/autoconf/config.guess 'http://git.savannah.gnu.org/gitweb/?p=config.git;a=blob_plain;f=config.guess;hb=HEAD'
		RUN curl -o /usr/local/lvm2/autoconf/config.sub 'http://git.savannah.gnu.org/gitweb/?p=config.git;a=blob_plain;f=config.sub;hb=HEAD'
	EOF
fi

cat <<-'EOF'
# Compile and install lvm2
RUN cd /usr/local/lvm2 \
       && ./configure --enable-static_link \
       && make device-mapper \
       && make install_device-mapper
# see https://git.fedorahosted.org/cgit/lvm2.git/tree/INSTALL

# Install lxc
ENV LXC_VERSION 1.1.2
RUN mkdir -p /usr/src/lxc \
       && curl -sSL https://linuxcontainers.org/downloads/lxc/lxc-${LXC_VERSION}.tar.gz | tar -v -C /usr/src/lxc/ -xz --strip-components=1
RUN cd /usr/src/lxc \
       && ./configure \
       && make \
       && make install \
       && ldconfig

ENV GOPATH /go:/go/src/github.com/docker/docker/vendor

EOF

if [ -z "$ARCH" ]; then 
	cat <<-'EOF'
		# Install Go
		ENV GO_VERSION 1.4.2
		RUN curl -sSL https://golang.org/dl/go${GO_VERSION}.src.tar.gz | tar -v -C /usr/local -xz \
		       && mkdir -p /go/bin
		ENV PATH /go/bin:/usr/local/go/bin:$PATH
		RUN cd /usr/local/go/src && ./make.bash --no-clean 2>&1
    
		# Compile Go for cross compilation
		ENV DOCKER_CROSSPLATFORMS \
		       linux/386 linux/arm \
		       darwin/amd64 darwin/386 \
		       freebsd/amd64 freebsd/386 freebsd/arm \
		       windows/amd64 windows/386

		# (set an explicit GOARM of 5 for maximum compatibility)
		ENV GOARM 5
		RUN cd /usr/local/go/src \
	        && set -x \
	        && for platform in $DOCKER_CROSSPLATFORMS; do \
	                GOOS=${platform%/*} \
	                GOARCH=${platform##*/} \
	                       ./make.bash --no-clean 2>&1; \
	        done
 
		# This has been commented out and kept as reference because we don't support compiling with older Go anymore.
		# ENV GOFMT_VERSION 1.3.3
		# RUN curl -sSL https://storage.googleapis.com/golang/go${GOFMT_VERSION}.$(go env GOOS)-$(go env GOARCH).tar.gz | tar -C /go/bin -xz --strip-components=2 go/bin/gofmt
	EOF
fi

cat <<-'EOF'

# Update this sha when we upgrade to go 1.5.0
ENV GO_TOOLS_COMMIT 069d2f3bcb68257b627205f0486d6cc69a231ff9
# Grab Go's cover tool for dead-simple code coverage testing
# Grab Go's vet tool for examining go code to find suspicious constructs
# and help prevent errors that the compiler might not catch
RUN git clone https://github.com/golang/tools.git /go/src/golang.org/x/tools \
       && (cd /go/src/golang.org/x/tools && git checkout -q $GO_TOOLS_COMMIT) \
       && go install -v golang.org/x/tools/cmd/cover \
       && go install -v golang.org/x/tools/cmd/vet

# Grab Go's lint tool
ENV GO_LINT_COMMIT f42f5c1c440621302702cb0741e9d2ca547ae80f
RUN git clone https://github.com/golang/lint.git /go/src/github.com/golang/lint \
       && (cd /go/src/github.com/golang/lint && git checkout -q $GO_LINT_COMMIT) \
       && go install -v github.com/golang/lint/golint

# TODO replace FPM with some very minimal debhelper stuff
EOF

if [ -n "$ARCH" ]; then
	cat <<-'EOF'
		RUN apt-get install -y libffi-dev
	EOF
fi

cat <<-'EOF'
RUN gem install --no-rdoc --no-ri fpm --version 1.3.2

EOF
cat <<-'EOF'
# Install registry
ENV REGISTRY_COMMIT 2317f721a3d8428215a2b65da4ae85212ed473b4
RUN set -x \
       && export GOPATH="$(mktemp -d)" \
       && git clone https://github.com/docker/distribution.git "$GOPATH/src/github.com/docker/distribution" \
       && (cd "$GOPATH/src/github.com/docker/distribution" && git checkout -q "$REGISTRY_COMMIT") \
       && GOPATH="$GOPATH/src/github.com/docker/distribution/Godeps/_workspace:$GOPATH" \
               go build -o /usr/local/bin/registry-v2 github.com/docker/distribution/cmd/registry \
       && rm -rf "$GOPATH"

# Install notary server
ENV NOTARY_COMMIT 8e8122eb5528f621afcd4e2854c47302f17392f7
RUN set -x \
       && export GOPATH="$(mktemp -d)" \
       && git clone https://github.com/docker/notary.git "$GOPATH/src/github.com/docker/notary" \
       && (cd "$GOPATH/src/github.com/docker/notary" && git checkout -q "$NOTARY_COMMIT") \
       && GOPATH="$GOPATH/src/github.com/docker/notary/Godeps/_workspace:$GOPATH" \
               go build -o /usr/local/bin/notary-server github.com/docker/notary/cmd/notary-server \
       && rm -rf "$GOPATH"

# Get the "docker-py" source so we can run their integration tests
ENV DOCKER_PY_COMMIT 8a87001d09852058f08a807ab6e8491d57ca1e88
RUN git clone https://github.com/docker/docker-py.git /docker-py \
       && cd /docker-py \
       && git checkout -q $DOCKER_PY_COMMIT

# Setup s3cmd config
RUN { \
               echo '[default]'; \
               echo 'access_key=$AWS_ACCESS_KEY'; \
               echo 'secret_key=$AWS_SECRET_KEY'; \
       } > ~/.s3cfg

# Set user.email so crosbymichael's in-container merge commits go smoothly
RUN git config --global user.email 'docker-dummy@example.com'

# Add an unprivileged user to be used for tests which need it
RUN groupadd -r docker
RUN useradd --create-home --gid docker unprivilegeduser

VOLUME /var/lib/docker
WORKDIR /go/src/github.com/docker/docker
ENV DOCKER_BUILDTAGS apparmor selinux 

# Let us use a .bashrc file
RUN ln -sfv $PWD/.bashrc ~/.bashrc

# Register Docker's bash completion.
RUN ln -sv $PWD/contrib/completion/bash/docker /etc/bash_completion.d/docker
EOF
if [ "$ARCH" == "ppc64le" ]; then
	cat <<-'EOF'
		# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
		COPY contrib/download-frozen-image.sh /go/src/github.com/docker/docker/contrib/
		RUN ./contrib/download-frozen-image.sh /docker-frozen-images \
		         ibmcom/busybox_p:latest \
		         ibmcom/hello-world_p:frozen \
		         ibmcom/unshare_p:latest
		# see also "hack/make/.ensure-frozen-images" (which needs to be updated any time this list is)
	EOF
elif [ "$ARCH" == "s390x" ]; then
	cat <<-'EOF'
		# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
		COPY contrib/download-frozen-image.sh /go/src/github.com/docker/docker/contrib/
		RUN ./contrib/download-frozen-image.sh /docker-frozen-images \
		         ibmcom/busybox_z:latest \
		         ibmcom/hello-world_z:frozen \
		         ibmcom/unshare_z:latest
		# see also "hack/make/.ensure-frozen-images" (which needs to be updated any time this list is)
	EOF
else
	cat <<-'EOF'
		# Get useful and necessary Hub images so we can "docker load" locally instead of pulling
		COPY contrib/download-frozen-image.sh /go/src/github.com/docker/docker/contrib/
		RUN ./contrib/download-frozen-image.sh /docker-frozen-images \
		       busybox:latest@8c2e06607696bd4afb3d03b687e361cc43cf8ec1a4a725bc96e39f05ba97dd55 \
		       hello-world:frozen@91c95931e552b11604fea91c2f537284149ec32fff0f700a4769cfd31d7696ae \
		       jess/unshare@5c9f6ea50341a2a8eb6677527f2bdedbf331ae894a41714fda770fb130f3314d
		# see also "hack/make/.ensure-frozen-images" (which needs to be updated any time this list is)
	EOF
fi

cat <<-'EOF'

# Download man page generator
RUN set -x \
       && export GOPATH="$(mktemp -d)" \
       && git clone -b v1.0.1 https://github.com/cpuguy83/go-md2man.git "$GOPATH/src/github.com/cpuguy83/go-md2man" \
       && git clone -b v1.2 https://github.com/russross/blackfriday.git "$GOPATH/src/github.com/russross/blackfriday" \
       && go get -v -d github.com/cpuguy83/go-md2man \
       && go build -v -o /usr/local/bin/go-md2man github.com/cpuguy83/go-md2man \
       && rm -rf "$GOPATH"

# Download toml validator
ENV TOMLV_COMMIT 9baf8a8a9f2ed20a8e54160840c492f937eeaf9a
RUN set -x \
       && export GOPATH="$(mktemp -d)" \
       && git clone https://github.com/BurntSushi/toml.git "$GOPATH/src/github.com/BurntSushi/toml" \
       && (cd "$GOPATH/src/github.com/BurntSushi/toml" && git checkout -q "$TOMLV_COMMIT") \
       && go build -v -o /usr/local/bin/tomlv github.com/BurntSushi/toml/cmd/tomlv \
       && rm -rf "$GOPATH"

# Build/install the tool for embedding resources in Windows binaries
ENV RSRC_COMMIT e48dbf1b7fc464a9e85fcec450dddf80816b76e0
RUN set -x \
    && git clone https://github.com/akavel/rsrc.git /go/src/github.com/akavel/rsrc \
    && cd /go/src/github.com/akavel/rsrc \
    && git checkout -q $RSRC_COMMIT \
    && go install -v

# Wrap all commands in the "docker-in-docker" script to allow nested containers
ENTRYPOINT ["hack/dind"]

# Upload docker source
COPY . /go/src/github.com/docker/docker
EOF


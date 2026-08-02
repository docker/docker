# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.3
ARG BASE_DEBIAN_DISTRO=trixie
ARG PROTOC_VERSION=3.11.4

# protoc is dynamically linked to glibc so can't use alpine base
FROM golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO} AS base
RUN apt-get update && apt-get --no-install-recommends install -y git unzip
ARG PROTOC_VERSION
ARG TARGETOS
ARG TARGETARCH
ENV GOTOOLCHAIN=local
RUN <<EOT
  set -e
  arch=$(echo $TARGETARCH | sed -e s/amd64/x86_64/ -e s/arm64/aarch_64/)
  wget -q https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${TARGETOS}-${arch}.zip
  unzip protoc-${PROTOC_VERSION}-${TARGETOS}-${arch}.zip -d /usr/local
EOT
WORKDIR /usr/src/moby

FROM base AS src
WORKDIR /out
COPY . .
RUN <<EOT
  set -ex
  git config --global user.email "moby@example.com"
  git config --global user.name "moby"
  git init .
  git add .
  git commit -m 'init'
EOT

FROM base AS tools
RUN --mount=from=src,source=/out,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build <<EOT
  set -ex
  # install all tools defined in go.mod
  go install -v tool
  go build -v \
    -o /usr/bin/pluginrpc-gen \
    ./pkg/plugins/pluginrpc-gen
EOT

FROM tools AS generated
RUN --mount=from=src,source=/out,target=.,rw <<EOT
  set -ex
  go generate -v ./...
  mkdir /out
  git ls-files -m --others -- ':!vendor' 'profiles/seccomp/default.json' '**/*.pb.go' | tar -cf - --files-from - | tar -C /out -xf -
EOT

FROM scratch AS update
COPY --from=generated /out /

FROM base AS validate
RUN --mount=from=src,source=/out,target=.,rw \
    --mount=type=bind,from=generated,source=/out,target=/generated-files <<EOT
  set -e
  git add -A
  if [ "$(ls -A /generated-files)" ]; then
    cp -rf /generated-files/* .
  fi
  diff=$(git status --porcelain -- ':!vendor' 'profiles/seccomp/default.json' '**/*.pb.go')
  if [ -n "$diff" ]; then
    echo >&2 'ERROR: The result of "go generate" differs. Please update with "make generate-files"'
    echo "$diff"
    exit 1
  fi
EOT

# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.3
ARG GOVULNCHECK_VERSION=v1.1.4
ARG FORMAT=text

FROM golang:${GO_VERSION}-alpine AS base
WORKDIR /usr/src/moby
RUN apk add --no-cache jq moreutils
ARG GOVULNCHECK_VERSION
ADD https://github.com/golang/vuln.git?ref=${GOVULNCHECK_VERSION}&keep-git-dir=1 /go/src/golang.org/x/vuln
RUN --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg/mod \
    cd /go/src/golang.org/x/vuln && \
    go install ./cmd/govulncheck

FROM base AS run
ARG FORMAT
RUN --mount=type=bind,target=.,rw <<EOT
  set -ex
  mkdir /out
  govulncheck -format ${FORMAT} ./... | tee /out/govulncheck.out
EOT

FROM scratch AS output
COPY --from=run /out /

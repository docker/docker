#syntax=docker/dockerfile:1.1-experimental
ARG GO_VERSION=1.13

FROM --platform=amd64 tonistiigi/xx:golang AS goxx

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS base
RUN apk add --no-cache gcc musl-dev
COPY --from=goxx / /
WORKDIR /src

FROM base AS build
ARG TARGETPLATFORM
RUN --mount=target=. \
    --mount=target=/root/.cache,type=cache \
    go build ./...

FROM base AS test
RUN --mount=target=. \
    --mount=target=/root/.cache,type=cache \
    go test -test.v ./...

FROM base AS test-noroot
RUN mkdir /go/pkg && chmod 0777 /go/pkg
USER 1000:1000
RUN --mount=target=. \
    --mount=target=/tmp/.cache,type=cache \
    GOCACHE=/tmp/gocache go test -test.v ./...

FROM build

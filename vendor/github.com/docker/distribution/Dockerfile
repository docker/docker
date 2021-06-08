FROM golang:1.11-alpine AS build

ENV DISTRIBUTION_DIR /go/src/github.com/docker/distribution
ENV BUILDTAGS include_oss include_gcs

ARG GOOS=linux
ARG GOARCH=amd64
ARG GOARM=6

RUN set -ex \
    && apk add --no-cache make git file

WORKDIR $DISTRIBUTION_DIR
COPY . $DISTRIBUTION_DIR
RUN CGO_ENABLED=0 make PREFIX=/go clean binaries && file ./bin/registry | grep "statically linked"

FROM alpine
COPY cmd/registry/config-dev.yml /etc/docker/registry/config.yml
COPY --from=build /go/src/github.com/docker/distribution/bin/registry /bin/registry
VOLUME ["/var/lib/registry"]
EXPOSE 5000
ENTRYPOINT ["registry"]
CMD ["serve", "/etc/docker/registry/config.yml"]

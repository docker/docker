---
aliases: ["/engine/misc/deprecated/"]
description: "Deprecated Features."
keywords: "docker, documentation, about, technology, deprecate"
---

<!-- This file is maintained within the docker/cli GitHub
     repository at https://github.com/docker/cli/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Deprecated Engine Features

The following list of features are deprecated in Engine.
To learn more about Docker Engine's deprecation policy,
see [Feature Deprecation Policy](https://docs.docker.com/engine/#feature-deprecation-policy).

### Reserved namespaces in engine labels

**Deprecated in Release: v18.06.0**

The namespaces `com.docker.*`, `io.docker.*`, and `org.dockerproject.*` in engine labels
were always documented to be reserved, but there was never any enforcement.

Usage of these namespaces will now cause a warning in the engine logs to discourage their
use, and will error instead in 18.12 and above.

### Asynchronous `service create` and `service update`

**Deprecated In Release: v17.05.0**

**Disabled by default in release: [v17.10](https://github.com/docker/docker-ce/releases/tag/v17.10.0-ce)**

Docker 17.05.0 added an optional `--detach=false` option to make the
`docker service create` and `docker service update` work synchronously. This
option will be enabled by default in Docker 17.10, at which point the `--detach`
flag can be used to use the previous (asynchronous) behavior.

The default for this option will also be changed accordingly for `docker service rollback`
and `docker service scale` in Docker 17.10.

### `-g` and `--graph` flags on `dockerd`

**Deprecated In Release: v17.05.0**

The `-g` or `--graph` flag for the `dockerd` or `docker daemon` command was
used to indicate the directory in which to store persistent data and resource
configuration and has been replaced with the more descriptive `--data-root`
flag.

These flags were added before Docker 1.0, so will not be _removed_, only
_hidden_, to discourage their use.

### Top-level network properties in NetworkSettings

**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Target For Removal In Release: v17.12**

When inspecting a container, `NetworkSettings` contains top-level information
about the default ("bridge") network;

`EndpointID`, `Gateway`, `GlobalIPv6Address`, `GlobalIPv6PrefixLen`, `IPAddress`,
`IPPrefixLen`, `IPv6Gateway`, and `MacAddress`.

These properties are deprecated in favor of per-network properties in
`NetworkSettings.Networks`. These properties were already "deprecated" in
docker 1.9, but kept around for backward compatibility.

Refer to [#17538](https://github.com/docker/docker/pull/17538) for further
information.

### `filter` param for `/images/json` endpoint
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Target For Removal In Release: v17.12**

The `filter` param to filter the list of image by reference (name or name:tag) is now implemented as a regular filter, named `reference`.

### `repository:shortid` image references
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Removed In Release: v17.12**

The `repository:shortid` syntax for referencing images is very little used,
collides with tag references, and can be confused with digest references.

Support for the `repository:shortid` notation to reference images was removed
in Docker 17.12.

### `docker daemon` subcommand
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Removed In Release: v17.12**

The daemon is moved to a separate binary (`dockerd`), and should be used instead.

### Duplicate keys with conflicting values in engine labels
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Removed In Release: v17.12**

When setting duplicate keys with conflicting values, an error will be produced, and the daemon
will fail to start.

### `MAINTAINER` in Dockerfile
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

`MAINTAINER` was an early very limited form of `LABEL` which should be used instead.

### API calls without a version
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Target For Removal In Release: v17.12**

API versions should be supplied to all API calls to ensure compatibility with
future Engine versions. Instead of just requesting, for example, the URL
`/containers/json`, you must now request `/v1.25/containers/json`.

### Backing filesystem without `d_type` support for overlay/overlay2
**Deprecated In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

**Removed In Release: v17.12**

The overlay and overlay2 storage driver does not work as expected if the backing
filesystem does not support `d_type`. For example, XFS does not support `d_type`
if it is formatted with the `ftype=0` option.

Starting with Docker 17.12, new installations will not support running overlay2 on
a backing filesystem without `d_type` support. For existing installations that upgrade
to 17.12, a warning will be printed.

Please also refer to [#27358](https://github.com/docker/docker/issues/27358) for
further information.

### Three arguments form in `docker import`
**Deprecated In Release: [v0.6.7](https://github.com/docker/docker/releases/tag/v0.6.7)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

The `docker import` command format `file|URL|- [REPOSITORY [TAG]]` is deprecated since November 2013. It's no more supported.

### `-h` shorthand for `--help`

**Deprecated In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

**Target For Removal In Release: v17.09**

The shorthand (`-h`) is less common than `--help` on Linux and cannot be used
on all subcommands (due to it conflicting with, e.g. `-h` / `--hostname` on
`docker create`). For this reason, the `-h` shorthand was not printed in the
"usage" output of subcommands, nor documented, and is now marked "deprecated".

### `-e` and `--email` flags on `docker login`
**Deprecated In Release: [v1.11.0](https://github.com/docker/docker/releases/tag/v1.11.0)**

**Removed In Release: [v17.06](https://github.com/docker/docker-ce/releases/tag/v17.06.0-ce)**

The docker login command is removing the ability to automatically register for an account with the target registry if the given username doesn't exist. Due to this change, the email flag is no longer required, and will be deprecated.

### Separator (`:`) of `--security-opt` flag on `docker run`
**Deprecated In Release: [v1.11.0](https://github.com/docker/docker/releases/tag/v1.11.0)**

**Target For Removal In Release: v17.06**

The flag `--security-opt` doesn't use the colon separator(`:`) anymore to divide keys and values, it uses the equal symbol(`=`) for consistency with other similar flags, like `--storage-opt`.

### `/containers/(id or name)/copy` endpoint

**Deprecated In Release: [v1.8.0](https://github.com/docker/docker/releases/tag/v1.8.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

The endpoint `/containers/(id or name)/copy` is deprecated in favor of `/containers/(id or name)/archive`.

### Ambiguous event fields in API
**Deprecated In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

The fields `ID`, `Status` and `From` in the events API have been deprecated in favor of a more rich structure.
See the events API documentation for the new format.

### `-f` flag on `docker tag`
**Deprecated In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

To make tagging consistent across the various `docker` commands, the `-f` flag on the `docker tag` command is deprecated. It is not longer necessary to specify `-f` to move a tag from one image to another. Nor will `docker` generate an error if the `-f` flag is missing and the specified tag is already in use.

### HostConfig at API container start
**Deprecated In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

Passing an `HostConfig` to `POST /containers/{name}/start` is deprecated in favor of
defining it at container creation (`POST /containers/create`).

### `--before` and `--since` flags on `docker ps`

**Deprecated In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

The `docker ps --before` and `docker ps --since` options are deprecated.
Use `docker ps --filter=before=...` and `docker ps --filter=since=...` instead.

### `--automated` and `--stars` flags on `docker search`

**Deprecated in Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

**Target For Removal In Release: v17.09**

The `docker search --automated` and `docker search --stars` options are deprecated.
Use `docker search --filter=is-automated=...` and `docker search --filter=stars=...` instead.

### Driver Specific Log Tags
**Deprecated In Release: [v1.9.0](https://github.com/docker/docker/releases/tag/v1.9.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

Log tags are now generated in a standard way across different logging drivers.
Because of which, the driver specific log tag options `syslog-tag`, `gelf-tag` and
`fluentd-tag` have been deprecated in favor of the generic `tag` option.

```bash
{% raw %}
docker --log-driver=syslog --log-opt tag="{{.ImageName}}/{{.Name}}/{{.ID}}"
{% endraw %}
```

### LXC built-in exec driver
**Deprecated In Release: [v1.8.0](https://github.com/docker/docker/releases/tag/v1.8.0)**

**Removed In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

The built-in LXC execution driver, the lxc-conf flag, and API fields have been removed.

### Old Command Line Options
**Deprecated In Release: [v1.8.0](https://github.com/docker/docker/releases/tag/v1.8.0)**

**Removed In Release: [v1.10.0](https://github.com/docker/docker/releases/tag/v1.10.0)**

The flags `-d` and `--daemon` are deprecated in favor of the `daemon` subcommand:

    docker daemon -H ...

The following single-dash (`-opt`) variant of certain command line options
are deprecated and replaced with double-dash options (`--opt`):

    docker attach -nostdin
    docker attach -sig-proxy
    docker build -no-cache
    docker build -rm
    docker commit -author
    docker commit -run
    docker events -since
    docker history -notrunc
    docker images -notrunc
    docker inspect -format
    docker ps -beforeId
    docker ps -notrunc
    docker ps -sinceId
    docker rm -link
    docker run -cidfile
    docker run -dns
    docker run -entrypoint
    docker run -expose
    docker run -link
    docker run -lxc-conf
    docker run -n
    docker run -privileged
    docker run -volumes-from
    docker search -notrunc
    docker search -stars
    docker search -t
    docker search -trusted
    docker tag -force

The following double-dash options are deprecated and have no replacement:

    docker run --cpuset
    docker run --networking
    docker ps --since-id
    docker ps --before-id
    docker search --trusted

**Deprecated In Release: [v1.5.0](https://github.com/docker/docker/releases/tag/v1.5.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

The single-dash (`-help`) was removed, in favor of the double-dash `--help`

    docker -help
    docker [COMMAND] -help

### `--run` flag on docker commit

**Deprecated In Release: [v0.10.0](https://github.com/docker/docker/releases/tag/v0.10.0)**

**Removed In Release: [v1.13.0](https://github.com/docker/docker/releases/tag/v1.13.0)**

The flag `--run` of the docker commit (and its short version `-run`) were deprecated in favor
of the `--changes` flag that allows to pass `Dockerfile` commands.


### Interacting with V1 registries

**Disabled By Default In Release: v17.06**

**Removed In Release: v17.12**

Version 1.8.3 added a flag (`--disable-legacy-registry=false`) which prevents the
docker daemon from `pull`, `push`, and `login` operations against v1
registries.  Though enabled by default, this signals the intent to deprecate
the v1 protocol.

Support for the v1 protocol to the public registry was removed in 1.13. Any
mirror configurations using v1 should be updated to use a
[v2 registry mirror](https://docs.docker.com/registry/recipes/mirror/).

Starting with Docker 17.12, support for V1 registries has been removed, and the
`--disable-legacy-registry` flag can no longer be used, and `dockerd` will fail to
start when set.

### `--disable-legacy-registry` override daemon option

**Disabled In Release: v17.12**

**Target For Removal In Release: v18.03**

The `--disable-legacy-registry` flag was disabled in Docker 17.12 and will print
an error when used. For this error to be printed, the flag itself is still present,
but hidden. The flag will be removed in Docker 18.03.


### Docker Content Trust ENV passphrase variables name change
**Deprecated In Release: [v1.9.0](https://github.com/docker/docker/releases/tag/v1.9.0)**

**Removed In Release: [v1.12.0](https://github.com/docker/docker/releases/tag/v1.12.0)**

Since 1.9, Docker Content Trust Offline key has been renamed to Root key and the Tagging key has been renamed to Repository key. Due to this renaming, we're also changing the corresponding environment variables

- DOCKER_CONTENT_TRUST_OFFLINE_PASSPHRASE is now named DOCKER_CONTENT_TRUST_ROOT_PASSPHRASE
- DOCKER_CONTENT_TRUST_TAGGING_PASSPHRASE is now named DOCKER_CONTENT_TRUST_REPOSITORY_PASSPHRASE

### `--api-enable-cors` flag on dockerd

**Deprecated In Release: [v1.6.0](https://github.com/docker/docker/releases/tag/v1.6.0)**

**Removed In Release: [v17.09](https://github.com/docker/docker-ce/releases/tag/v17.09.0-ce)**

The flag `--api-enable-cors` is deprecated since v1.6.0. Use the flag
`--api-cors-header` instead.

---
title: "Engine API version history"
description: "Documentation of changes that have been made to Engine API."
keywords: "API, Docker, rcli, REST, documentation"
---

<!-- This file is maintained within the moby/moby GitHub
     repository at https://github.com/moby/moby/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

## v1.57 API changes

* `GET /events`: `die` events for containers now include a `restarting`
  attribute, set to `"true"` when the container is going to be restarted,
  either by its restart policy or by a manual restart in progress. The
  attribute is omitted when the container is not going to be restarted,
  including when the daemon is shutting down. It announces the restart but
  does not guarantee it: the restart can still be cancelled (for example by
  stopping the container) or fail to start the container again.
* The API specification now uses OpenAPI 3.2.0 instead of Swagger 2.0.
  A generated Swagger 2.0 version is still available for existing tooling.

## v1.56 API changes

* `GET /containers/json` now supports an `annotation` filter to filter
  containers by annotation, either by key (`annotation=key`) or by key and
  value (`annotation="key=value"`), similar to the existing `label` filter.
* `POST /containers/create` now supports `HostConfig.Umask` to set the initial
  umask for a Unix container. When set, the daemon includes the value in the OCI
  process configuration for the container's entrypoint, exec processes, and
  healthchecks. When omitted, the runtime's default behavior applies.

## v1.55 API changes

* `GET /images/{name}/attestations` is a new endpoint that returns the in-toto
  attestation statements attached to an image. The `platform` query parameter
  selects the image variant (defaults to the daemon's host platform); it is
  declared as a repeatable parameter for forward compatibility, but only one
  value is currently accepted. The `type` query parameter may be repeated to
  filter the returned statements by in-toto predicate type URI; if omitted,
  all statements are returned. The `statement` query parameter (default
  `false`) controls whether the verbatim statement body is included; when
  omitted or `false`, each entry contains only the descriptor and predicate
  type, and statement blobs are not read. The response is a JSON array of
  `AttestationStatement` objects.
* `POST /containers/{id}/update` now supports per-device blkio resource
  settings through the `BlkioWeightDevice`, `BlkioDeviceReadBps`,
  `BlkioDeviceWriteBps`, `BlkioDeviceReadIOps`, and `BlkioDeviceWriteIOps`
  fields. These fields were previously present in the request schema but were
  ignored by the daemon. Omit a field or set it to `null` to leave the current
  per-device rules unchanged. Set it to an empty array to clear the current
  per-device rules for that resource type.

## v1.54 API changes

* `GET /images/json` now supports an `identity` query parameter. When set,
  the response includes manifest summaries and may include an `Identity` field
  for each manifest with trusted identity and origin information.
* `POST /networks/{id}/connect` now correctly applies the `MacAddress` field in
  `EndpointSettings`. This field was added in API v1.44, but was previously ignored.

## v1.53 API changes

* `GET /info` now includes an `NRI` field. If the Node Resource Interface (NRI)
  is enabled, this field contains information describing it.
* `GET /events` now also supports [`application/jsonl`](https://jsonlines.org/)
  when negotiating content-type.
* `GET /images/{name}/json` now includes an `Identity` field with trusted
  identity and origin information for the image.
* Deprecated: The `POST /grpc` and `POST /session` endpoints are deprecated and
  will be removed in a future version.

## v1.52 API changes

* `GET /images/{name}/get` now accepts multiple `platform` query-arguments
  to allow selecting which platform(s) of a multi-platform image must be
  saved.
* `POST /images/load` now accepts multiple `platform` query-arguments
  to allow selecting which platform(s) of a multi-platform image to load.
* `GET /events` no longer includes the deprecated `status`, `id`, and `from`
  fields. These fields were removed in API v1.22, but still included
  in the response.
* `GET /networks/{id}` now includes a `Status` field, providing statistics
  about IPAM allocations for the subnets assigned to the network.
* Deprecated: the Engine was automatically backfilling empty `PortBindings` lists with
  a PortBinding with an empty HostIP and HostPort when calling `POST /containers/{id}/start`.
  This behavior is now deprecated, and a warning is returned by `POST /containers/create`.
  A future API version will drop empty `PortBindings` list altogether.
* `GET /images/{name}/json` now omits the following `Config` fields when
  not set, to closer align with the implementation of the [OCI Image Specification](https://github.com/opencontainers/image-spec/blob/v1.1.1/specs-go/v1/config.go#L23-L62)
  `Cmd`, `Entrypoint`, `Env`, `Labels`, `OnBuild`, `User`, `Volumes`, and `WorkingDir`.
* `GET /images/{name}/json` now omits the following fields if their value
  is empty: `Parent`, `Comment`, `DockerVersion`, `Author`. The `Parent`
  and `DockerVersion` fields were set by the legacy builder, and are no
  longer set when using BuildKit. The `Author` field is set through the
  `MAINTAINER` Dockerfile instruction, which is deprecated, and the `Comment`
  field is option, and may not be set depending on how the image was created.
* `GET /container/{id}/json` now omits `Config.OnBuild` if its value is empty.
* `GET /containers/{id}/json`: the `NetworkSettings` no longer returns the deprecated
  `Bridge`, `HairpinMode`, `LinkLocalIPv6Address`, `LinkLocalIPv6PrefixLen`,
  `SecondaryIPAddresses`, `SecondaryIPv6Addresses`, `EndpointID`, `Gateway`,
  `GlobalIPv6Address`, `GlobalIPv6PrefixLen`, `IPAddress`, `IPPrefixLen`,
  `IPv6Gateway`, and `MacAddress` fields. These fields were deprecated in
  API v1.21 (docker v1.9.0) but kept around for backward compatibility.
* Removed the `KernelMemoryTCP` field from the `POST /containers/{id}/update` and
  `GET /containers/{id}/json` endpoints, any value it is set to will be ignored
  on API version `v1.52` and up. Older API versions still accept this field, but
  may take no effect, depending on the kernel version and OCI runtime in use.
* Removed the `KernelMemoryTCP` field from the `GET /info` endpoint.
* `GET /events` supports content-type negotiation and can produce either `application/x-ndjson` 
  (Newline delimited JSON object stream) or `application/json-seq` (RFC7464).
* `POST /containers/create` no longer supports configuring a container-wide MAC address
  via the container's `Config.MacAddress` field. A container's MAC address can now only 
  be configured via endpoint settings when connecting to a network.
* `GET /services` now returns `SwapBytes` and `MemorySwappiness` fields as part
  of the `Resource` requirements.
* `GET /services/{id}` now returns `SwapBytes` and `MemorySwappiness` fields as
  part of the `Resource` requirements.
* `POST /services/create` now accepts `SwapBytes` and `MemorySwappiness` fields
  as part of the `Resource` requirements.
* `POST /services/{id}/update` now accepts `SwapBytes` and `MemorySwappiness`
  fields as part of the `Resource` requirements.
* `GET /tasks` now  returns `SwapBytes` and `MemorySwappiness` fields as part
  of the `Resource` requirements.
* `GET /tasks/{id}` now  returns `SwapBytes` and `MemorySwappiness` fields as
  part of the `Resource` requirements.
* `GET /containers/{id}/stats` now returns an `os_type` field to allow platform-
  specific handling of the stats.
* `GET /system/df` returns `ImagesUsage`, `ContainersUsage`, `VolumesUsage`, and
  `BuildCacheUsage` fields with brief system disk usage data for each system object type.
  The endpoint supports the `?verbose=1` query to return verbose system disk usage information.
* Deprecated: `GET /system/df` response fields `LayersSize`, `Images`, `Containers`,
  `Volumes`, and `BuildCache` have been removed in favor of the-type specific usage fields.
  API v1.52 returns both the legacy and current fields to help existing integrations
  to transition to the new response. The legacy fields are not populated if the
  `verbose` query parameter is used. Starting with API v1.53, the legacy fields
  will no longer be returned.

## v1.51 API changes

* `GET /images/json` now sets the value of `Containers` field for all images
  to the count of containers using the image.
  This field was previously always -1.
* Deprecated: The field `NetworkSettings.Bridge` returned by `GET /containers/{id}/json`
  is deprecated and will be removed in the next API version.
* Deprecated: The field `KernelMemoryTCP` as part of `POST /containers/{id}/update`
  and returned by `GET /containers/{id}/json` is deprecated and will be removed
  in the next API version.
* Deprecated: The field `KernelMemoryTCP` as part of `GET /info` is deprecated
  and will be removed in the next API version.
* Deprecated: the `Parent` and `DockerVersion` fields returned by the
  `GET /images/{name}/json` endpoint are deprecated. These fields are set by
  the legacy builder, and are no longer set when using BuildKit. The API
  continues returning these fields when set for informational purposes, but
  they should not be depended on as they will be omitted once the legacy builder
  is removed.
* Deprecated: the `Config.DockerVersion` field returned by the `GET /plugins`
  and `GET /images/{name}/json` endpoints is deprecated. The field is no
  longer set, and is omitted when empty.

## v1.50 API changes

* `GET /info` now includes a `DiscoveredDevices` field. This is an array of
  `DeviceInfo` objects, each providing details about a device discovered by a
  device driver.
  Currently only the CDI device driver is supported.
* `DELETE /images/{name}` now supports a `platforms` query parameter. It accepts
  an array of JSON-encoded OCI Platform objects, allowing for selecting specific
  platforms to delete content for.
* Deprecated: The `BridgeNfIptables` and `BridgeNfIp6tables` fields in the
  `GET /info` response were deprecated in API v1.48, and are now omitted
  in API v1.50.
* Deprecated: `GET /images/{name}/json` no longer returns the following `Config`
  fields; `Hostname`, `Domainname`, `AttachStdin`, `AttachStdout`, `AttachStderr`
  `Tty`, `OpenStdin`, `StdinOnce`, `Image`, `NetworkDisabled` (already omitted unless set),
  `MacAddress` (already omitted unless set), `StopTimeout` (already omitted unless set).
  These additional fields were included in the response due to an implementation
  detail but not part of the image's Configuration. These fields were marked
  deprecated in API v1.46, and are now omitted. Older versions of the API still
  return these fields, but they are always empty.

## v1.49 API changes

* `GET /images/{name}/json` now supports a `platform` parameter (JSON
  encoded OCI Platform type) allowing to specify a platform of the multi-platform
  image to inspect.
  This option is mutually exclusive with the `manifests` option.
* `GET /info` now returns a `FirewallBackend` containing information about
  the daemon's firewalling configuration.
* Deprecated: The  `AllowNondistributableArtifactsCIDRs` and `AllowNondistributableArtifactsHostnames`
  fields in the `RegistryConfig` struct in the `GET /info` response are omitted
  in API v1.49.
* Deprecated: The `ContainerdCommit.Expected`, `RuncCommit.Expected`, and
  `InitCommit.Expected` fields in the `GET /info` endpoint were deprecated
  in API v1.48, and are now omitted in API v1.49.

## v1.48 API changes

* Deprecated: The "error" and "progress" fields in streaming responses for
  endpoints that return a JSON progress response, such as `POST /images/create`,
  `POST /images/{name}/push`, and `POST /build` are deprecated. These fields
  were marked deprecated in API v1.4 (docker v0.6.0) and API v1.8 (docker v0.7.1)
  respectively, but still returned. These fields will be left empty or will
  be omitted in a future API version. Users should use the information in the
  `errorDetail` and `progressDetail` fields instead.
* Deprecated: The "allow-nondistributable-artifacts" daemon configuration is
  deprecated and enabled by default. The  `AllowNondistributableArtifactsCIDRs`
  and `AllowNondistributableArtifactsHostnames` fields in the `RegistryConfig`
  struct in the `GET /info` response will now always be `null` and will be
  omitted in API v1.49.
* Deprecated: The `BridgeNfIptables` and `BridgeNfIp6tables` fields in the 
  `GET /info` response are now always be `false` and will be omitted in API
  v1.49. The netfilter module is now loaded on-demand, and no longer during
  daemon startup, making these fields obsolete.
* Deprecated: The `POST /build/prune` `keep-storage` query parameter has been
  renamed to `reserved-space`. `keep-storage` support will be removed in API v1.52.
* `GET /images/{name}/history` now supports a `platform` parameter (JSON
  encoded OCI Platform type) that allows to specify a platform to show the
  history of.
* `POST /images/{name}/load` and `GET /images/{name}/get` now support a
  `platform` parameter (JSON encoded OCI Platform type) that allows to specify
  a platform to load/save. Not passing this parameter will result in
  loading/saving the full multi-platform image.
* `POST /containers/create` now includes a warning in the response when setting
  the container-wide `Config.VolumeDriver` option in combination with volumes
  defined through `Mounts` because the `VolumeDriver` option has no effect on
  those volumes. This warning was previously generated by the CLI, but now
  moved to the daemon so that other clients can also get this warning.
* `POST /containers/create` now supports `Mount` of type `image` for mounting
  an image inside a container.
* Deprecated: The `ContainerdCommit.Expected`, `RuncCommit.Expected`, and
  `InitCommit.Expected` fields in the `GET /info` endpoint are deprecated
  and will be omitted in API v1.49.
* `Sysctls` in `HostConfig` (top level `--sysctl` settings) for `eth0` are
  no longer migrated to `DriverOpts`, as described in the changes for v1.46.
* `GET /images/json` and `GET /images/{name}/json` responses now include
  `Descriptor` field, which contains an OCI descriptor of the image target.
  The new field will only be populated if the daemon provides a multi-platform
  image store.
  WARNING: This is experimental and may change at any time without any backward
  compatibility.
* `GET /images/{name}/json` response now will return the `Manifests` field
  containing information about the sub-manifests contained in the image index.
  This includes things like platform-specific manifests and build attestations.
  The new field will only be populated if the request also sets the `manifests`
  query parameter to `true`.
  This acts the same as in the `GET /images/json` endpoint.
  WARNING: This is experimental and may change at any time without any backward compatibility.
* `GET /containers/{name}/json` now returns an `ImageManifestDescriptor` field
  containing the OCI descriptor of the platform-specific image manifest of the
  image that was used to create the container.
  This field is only populated if the daemon provides a multi-platform image
  store.
* `POST /networks/create` now has an `EnableIPv4` field. Setting it to `false`
  disables IPv4 IPAM for the network. It can only be set to `false` if the
  daemon has experimental features enabled.
* `GET /networks/{id}` now returns an `EnableIPv4` field showing whether the
  network has IPv4 IPAM enabled.
* `POST /networks/{id}/connect` and `POST /containers/create` now accept a
  `GwPriority` field in `EndpointsConfig`. This value is used to determine which
  network endpoint provides the default gateway for the container. The endpoint
  with the highest priority is selected. If multiple endpoints have the same
  priority, endpoints are sorted lexicographically by their network name, and
  the one that sorts first is picked.
* `GET /containers/json` now returns a `GwPriority` field in `NetworkSettings`
  for each network endpoint.
* API debug endpoints (`GET /debug/vars`, `GET /debug/pprof/`, `GET /debug/pprof/cmdline`,
  `GET /debug/pprof/profile`, `GET /debug/pprof/symbol`, `GET /debug/pprof/trace`,
  `GET /debug/pprof/{name}`) are now also accessible through the versioned-API
  paths (`/v<API-version>/<endpoint>`).
* `POST /build/prune` renames `keep-storage` to `reserved-space` and now supports
  additional prune parameters `max-used-space` and `min-free-space`.
* `GET /containers/json` now returns an `ImageManifestDescriptor` field
  matching the same field in `/containers/{name}/json`.
  This field is only populated if the daemon provides a multi-platform image
  store.

## v1.47 API changes

* `GET /images/json` response now includes `Manifests` field, which contains
  information about the sub-manifests included in the image index. This
  includes things like platform-specific manifests and build attestations.
  The new field will only be populated if the request also sets the `manifests`
  query parameter to `true`.
  WARNING: This is experimental and may change at any time without any backward
  compatibility.
* `GET /info` no longer includes warnings when `bridge-nf-call-iptables` or
  `bridge-nf-call-ip6tables` are disabled when the daemon was started. The
  `br_netfilter` module is now attempted to be loaded when needed, making those
  warnings inaccurate. This change is not versioned, and affects all API versions
  if the daemon has this patch.

## v1.46 API changes

* `GET /info` now includes a `Containerd` field containing information about
  the location of the containerd API socket and containerd namespaces used
  by the daemon to run containers and plugins.
* `POST /containers/create` field `NetworkingConfig.EndpointsConfig.DriverOpts`,
  and `POST /networks/{id}/connect` field `EndpointsConfig.DriverOpts`, now
  support label `com.docker.network.endpoint.sysctls` for setting per-interface
  sysctls. The value is a comma separated list of sysctl assignments, the
  interface name must be "IFNAME". For example, to set
  `net.ipv4.config.eth0.log_martians=1`, use
  `net.ipv4.config.IFNAME.log_martians=1`. In API versions up-to 1.46, top level
  `--sysctl` settings for `eth0` will be migrated to `DriverOpts` when possible. 
  This automatic migration will be removed in a future release.
* `GET /containers/json` now returns the annotations of containers.
* `POST /images/{name}/push` now supports a `platform` parameter (JSON encoded
  OCI Platform type) that allows selecting a specific platform manifest from
  the multi-platform image.
* `POST /containers/create` now takes `Options` as part of `HostConfig.Mounts.TmpfsOptions` to set options for tmpfs mounts.
* `POST /services/create` now takes `Options` as part of `ContainerSpec.Mounts.TmpfsOptions`, to set options for tmpfs mounts.
* `GET /events` now supports image `create` event that is emitted when a new
  image is built regardless if it was tagged or not.

#### Deprecated Config fields in `GET /images/{name}/json` response

The `Config` field returned by this endpoint (used for "image inspect") returns
additional fields that are not part of the image's configuration and not part of
the [Docker Image Spec] and the [OCI Image Spec].

These additional fields are included in the response, due to an
implementation detail, where the [api/types.ImageInspec] type used
for the response is using the [container.Config] type.

The [container.Config] type is a superset of the image config, and while the
image's Config is used as a _template_ for containers created from the image,
the additional fields are set at runtime (from options passed when creating
the container) and not taken from the image Config.

These fields are never set (and always return the default value for the type),
but are not omitted in the response when left empty. As these fields were not
intended to be part of the image configuration response, they are deprecated,
and will be removed from the API.

The following fields are currently included in the API response, but
are not part of the underlying image's Config, and deprecated:

- `Hostname`
- `Domainname`
- `AttachStdin`
- `AttachStdout`
- `AttachStderr`
- `Tty`
- `OpenStdin`
- `StdinOnce`
- `Image`
- `NetworkDisabled` (already omitted unless set)
- `MacAddress` (already omitted unless set)
- `StopTimeout` (already omitted unless set)

[Docker image spec]: https://github.com/moby/docker-image-spec/blob/v1.3.1/specs-go/v1/image.go#L19-L32
[OCI Image Spec]: https://github.com/opencontainers/image-spec/blob/v1.1.0/specs-go/v1/config.go#L24-L62
[api/types.ImageInspec]: https://github.com/moby/moby/blob/v26.1.4/api/types/types.go#L87-L104
[container.Config]: https://github.com/moby/moby/blob/v26.1.4/api/types/container/config.go#L47-L82

* `POST /services/create` and `POST /services/{id}/update` now support OomScoreAdj

## v1.45 API changes

* `POST /containers/create` now supports `VolumeOptions.Subpath` which allows a
  subpath of a named volume to be mounted.
* `POST /images/search` will always assume a `false` value for the `is-automated`
  field. Consequently, searching for `is-automated=true` will yield no results,
  while `is-automated=false` will be a no-op.
* `GET /images/{name}/json` no longer includes the `Container` and
  `ContainerConfig` fields. To access image configuration, use `Config` field
  instead.
* The `Aliases` field returned in calls to `GET /containers/{name:.*}/json` no
  longer contains the short container ID, but instead will reflect exactly the
  values originally submitted to the `POST /containers/create` endpoint. The
  newly introduced `DNSNames` should now be used instead when short container
  IDs are needed.

## v1.44 API changes

* GET `/images/json` now accepts an `until` filter. This accepts a timestamp and
  lists all images created before it. The `<timestamp>` can be Unix timestamps,
  date formatted timestamps, or Go duration strings (e.g. `10m`, `1h30m`)
  computed relative to the daemon machine’s time. This change is not versioned,
  and affects all API versions if the daemon has this patch.
* The `VirtualSize` field in the `GET /images/{name}/json`, `GET /images/json`,
  and `GET /system/df` responses is now omitted. Use the `Size` field instead,
  which contains the same information.
* Deprecated: The `is_automated` field in the `GET /images/search` response has
  been deprecated and will always be set to false in the future because Docker
  Hub is deprecating the `is_automated` field in its search API. The deprecation
  is not versioned, and applies to all API versions.
* Deprecated: The `is-automated` filter for the `GET /images/search` endpoint.
  The `is_automated` field has been deprecated by Docker Hub's search API.
  Consequently, searching for `is-automated=true` will yield no results. The
  deprecation is not versioned, and applies to all API versions.
* Read-only bind mounts are now made recursively read-only on kernel >= 5.12
  with runtimes which support the feature.
  `POST /containers/create`, `GET /containers/{id}/json`, and `GET /containers/json` now supports
  `BindOptions.ReadOnlyNonRecursive` and `BindOptions.ReadOnlyForceRecursive` to customize the behavior.
* `POST /containers/create` now accepts a `HealthConfig.StartInterval` to set the
  interval for health checks during the start period.
* `GET /info` now includes a `CDISpecDirs` field indicating the configured CDI
  specifications directories. The use of the applied setting requires the daemon
  to have experimental enabled, and for non-experimental daemons an empty list is
  always returned.
* `POST /networks/create` now returns a 400 if the `IPAMConfig` has invalid
  values. Note that this change is _unversioned_ and applied to all API
  versions on daemon that support version 1.44.
* `POST /networks/create` with a duplicated name now fails systematically. As
  such, the `CheckDuplicate` field is now deprecated. Note that this change is
  _unversioned_ and applied to all API versions on daemon that support version
  1.44.
* `POST /containers/create` now accepts multiple `EndpointSettings` in
  `NetworkingConfig.EndpointSettings`.
* `POST /containers/create` and `POST /networks/{id}/connect` will now catch
  validation errors that were previously only returned during `POST /containers/{id}/start`.
  These endpoints will also return the full set of validation errors they find,
  instead of returning only the first one.
  Note that this change is _unversioned_ and applies to all API versions.
* `POST /services/create` and `POST /services/{id}/update` now accept `Seccomp`
  and `AppArmor` fields in the `ContainerSpec.Privileges` object. This allows
  some configuration of Seccomp and AppArmor in Swarm services.
* A new endpoint-specific `MacAddress` field has been added to `NetworkSettings.EndpointSettings`
  on `POST /containers/create`, and to `EndpointConfig` on `POST /networks/{id}/connect`.
  The container-wide `MacAddress` field in `Config`, on `POST /containers/create`, is now deprecated.
* The field `Networks` in the `POST /services/create` and `POST /services/{id}/update`
  requests is now deprecated. You should instead use the field `TaskTemplate.Networks`.
* The `Container` and `ContainerConfig` fields in the `GET /images/{name}/json`
  response are deprecated and will no longer be included in API v1.45.
* `GET /info` now includes `status` properties in `Runtimes`.
* A new field named `DNSNames` and containing all non-fully qualified DNS names
  a container takes on a specific network has been added to `GET /containers/{name:.*}/json`.
* The `Aliases` field returned in calls to `GET /containers/{name:.*}/json` in v1.44 and older
  versions contains the short container ID. This will change in the next API version, v1.45.
  Starting with that API version, this specific value will be removed from the `Aliases` field
  such that this field will reflect exactly the values originally submitted to the
  `POST /containers/create` endpoint. The newly introduced `DNSNames` should now be used instead.
* The fields `HairpinMode`, `LinkLocalIPv6Address`, `LinkLocalIPv6PrefixLen`, `SecondaryIPAddresses`,
  `SecondaryIPv6Addresses` available in `NetworkSettings` when calling `GET /containers/{id}/json` are
  deprecated and will be removed in a future release. You should instead look for the default network in
  `NetworkSettings.Networks`.
* `GET /images/{id}/json` omits the `Created` field (previously it was `0001-01-01T00:00:00Z`)
  if the `Created` field is missing from the image config.

## v1.43 API changes

* `POST /containers/create` now accepts `Annotations` as part of `HostConfig`.
  Can be used to attach arbitrary metadata to the container, which will also be
  passed to the runtime when the container is started.
* `GET /images/json` no longer includes hardcoded `<none>:<none>` and
  `<none>@<none>` in `RepoTags` and`RepoDigests` for untagged images.
  In such cases, empty arrays will be produced instead.
* The `VirtualSize` field in the `GET /images/{name}/json`, `GET /images/json`,
  and `GET /system/df` responses is deprecated and will no longer be included
  in API v1.44. Use the `Size` field instead, which contains the same information.
* `GET /info` now includes `no-new-privileges` in the `SecurityOptions` string
  list when this option is enabled globally. This change is not versioned, and
  affects all API versions if the daemon has this patch.

## v1.42 API changes

* Removed the `BuilderSize` field on the `GET /system/df` endpoint. This field
  was introduced in API 1.31 as part of an experimental feature, and no longer
  used since API 1.40.
  Use field `BuildCache` instead to track storage used by the builder component.
* `POST /containers/{id}/stop` and `POST /containers/{id}/restart` now accept a
  `signal` query parameter, which allows overriding the container's default stop-
  signal.
* `GET /images/json` now accepts query parameter `shared-size`. When set `true`,
  images returned will include `SharedSize`, which provides the size on disk shared
  with other images present on the system.
* `GET /system/df` now accepts query parameter `type`. When set,
  computes and returns data only for the specified object type.
  The parameter can be specified multiple times to select several object types.
  Supported values are: `container`, `image`, `volume`, `build-cache`.
* `GET /system/df` can now be used concurrently. If a request is made while a
  previous request is still being processed, the request will receive the result
  of the already running calculation, once completed. Previously, an error
  (`a disk usage operation is already running`) would be returned in this
  situation. This change is not versioned, and affects all API versions if the
  daemon has this patch.
* The `POST /images/create` now supports both the operating system and architecture
  that is passed through the `platform` query parameter when using the `fromSrc`
  option to import an image from an archive. Previously, only the operating system
  was used and the architecture was ignored. If no `platform` option is set, the
  host's operating system and architecture as used as default. This change is not
  versioned, and affects all API versions if the daemon has this patch.
* The `POST /containers/{id}/wait` endpoint now returns a `400` status code if an
  invalid `condition` is provided (on API 1.30 and up).
* Removed the `KernelMemory` field from the `POST /containers/create` and
  `POST /containers/{id}/update` endpoints, any value it is set to will be ignored
  on API version `v1.42` and up. Older API versions still accept this field, but
  may take no effect, depending on the kernel version and OCI runtime in use.
* `GET /containers/{id}/json` now omits the `KernelMemory` and `KernelMemoryTCP`
  if they are not set.
* `GET /info` now omits the `KernelMemory` and `KernelMemoryTCP` if they are not
  supported by the host or host's configuration (if cgroups v2 are in use).
* `GET /_ping` and `HEAD /_ping` now return `Builder-Version` by default.
  This header contains the default builder to use, and is a recommendation as
  advertised by the daemon. However, it is up to the client to choose which builder
  to use.

  The default value on Linux is version "2" (BuildKit), but the daemon can be
  configured to recommend version "1" (classic Builder). Windows does not yet
  support BuildKit for native Windows images, and uses "1" (classic builder) as
  a default.

  This change is not versioned, and affects all API versions if the daemon has
  this patch.
* `GET /_ping` and `HEAD /_ping` now return a `Swarm` header, which allows a
  client to detect if Swarm is enabled on the daemon, without having to call
  additional endpoints.
  This change is not versioned, and affects all API versions if the daemon has
  this patch. Clients must consider this header "optional", and fall back to
  using other endpoints to get this information if the header is not present.

  The `Swarm` header can contain one of the following values:

    - "inactive"
    - "pending"
    - "error"
    - "locked"
    - "active/worker"
    - "active/manager"
* `POST /containers/create` for Windows containers now accepts a new syntax in
  `HostConfig.Resources.Devices.PathOnHost`. As well as the existing `class/<GUID>`
  syntax, `<IDType>://<ID>` is now recognised. Support for specific `<IDType>` values
  depends on the underlying implementation and Windows version. This change is not
  versioned, and affects all API versions if the daemon has this patch.
* `GET /containers/{id}/attach`, `GET /exec/{id}/start`, `GET /containers/{id}/logs`
  `GET /services/{id}/logs` and `GET /tasks/{id}/logs` now set Content-Type header
  to `application/vnd.docker.multiplexed-stream` when a multiplexed stdout/stderr
  stream is sent to client, `application/vnd.docker.raw-stream` otherwise.
* `POST /volumes/create` now accepts a new `ClusterVolumeSpec` to create a cluster
  volume (CNI). This option can only be used if the daemon is a Swarm manager.
  The Volume response on creation now also can contain a `ClusterVolume` field
  with information about the created volume.
* The `BuildCache.Parent` field, as returned by `GET /system/df` is deprecated
  and is now omitted. API versions before v1.42 continue to include this field.
* `GET /system/df` now includes a new `Parents` field, for "build-cache" records,
  which contains a list of parent IDs for the build-cache record.
* Volume information returned by `GET /volumes/{name}`, `GET /volumes` and
  `GET /system/df` can now contain a `ClusterVolume` if the volume is a cluster
  volume (requires the daemon to be a Swarm manager).
* The `Volume` type, as returned by `Added new `ClusterVolume` fields
* Added a new `PUT /volumes{name}` endpoint to update cluster volumes (CNI).
  Cluster volumes are only supported if the daemon is a Swarm manager.
* `GET /containers/{name}/attach/ws` endpoint now accepts `stdin`, `stdout` and
  `stderr` query parameters to only attach to configured streams.

  NOTE: These parameters were documented before in older API versions, but not
  actually supported. API versions before v1.42 continue to ignore these parameters
  and default to attaching to all streams. To preserve the pre-v1.42 behavior,
  set all three query parameters (`?stdin=1,stdout=1,stderr=1`).
* `POST /containers/create` on Linux now respects the `HostConfig.ConsoleSize` property.
  Container is immediately created with the desired terminal size and clients no longer
  need to set the desired size on their own.
* `POST /containers/create` allow to set `CreateMountpoint` for host path to be
  created if missing. This brings parity with `Binds`
* `POST /containers/create` rejects request if BindOptions|VolumeOptions|TmpfsOptions
  is set with a non-matching mount Type.
* `POST /containers/{id}/exec` now accepts an optional `ConsoleSize` parameter.
  It allows to set the console size of the executed process immediately when it's created.
* `POST /volumes/prune` will now only prune "anonymous" volumes (volumes which were not given a name) by default. A new filter parameter `all` can be set to a truth-y value (`true`, `1`) to get the old behavior.

## v1.41 API changes

* `GET /events` now returns `prune` events after pruning resources have completed.
  Prune events are returned for `container`, `network`, `volume`, `image`, and
  `builder`, and have a `reclaimed` attribute, indicating the amount of space
  reclaimed (in bytes).
* `GET /info` now returns a `CgroupVersion` field, containing the cgroup version.
* `GET /info` now returns a `DefaultAddressPools` field, containing a list of
  custom default address pools for local networks, which can be specified in the
  `daemon.json` file or `--default-address-pool` dockerd option.
* `POST /services/create` and `POST /services/{id}/update` now supports `BindOptions.NonRecursive`.
* The `ClusterStore` and `ClusterAdvertise` fields in `GET /info` are deprecated
  and are now omitted if they contain an empty value. This change is not versioned,
  and affects all API versions if the daemon has this patch.
* The `filter` (singular) query parameter, which was deprecated in favor of the
  `filters` option in Docker 1.13, has now been removed from the `GET /images/json`
  endpoint. The parameter remains available when using API version 1.40 or below.
* `GET /services` now returns `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `GET /services/{id}` now returns `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `POST /services/create` now accepts `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `POST /services/{id}/update` now accepts `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `GET /tasks` now  returns `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `GET /tasks/{id}` now  returns `CapAdd` and `CapDrop` as part of the `ContainerSpec`.
* `GET /services` now returns `Pids` in `TaskTemplate.Resources.Limits`.
* `GET /services/{id}` now returns `Pids` in `TaskTemplate.Resources.Limits`.
* `POST /services/create` now accepts `Pids` in `TaskTemplate.Resources.Limits`.
* `POST /services/{id}/update` now accepts `Pids` in `TaskTemplate.Resources.Limits`
  to limit the maximum number of PIDs.
* `GET /tasks` now  returns `Pids` in `TaskTemplate.Resources.Limits`.
* `GET /tasks/{id}` now  returns `Pids` in `TaskTemplate.Resources.Limits`.
* `POST /containers/create` now accepts a `platform` query parameter in the format
  `os[/arch[/variant]]`.

  When set, the daemon checks if the requested image is present in the local image
  cache with the given OS and Architecture, and otherwise returns a `404` status.

  If the option is _not_ set, the host's native OS and Architecture are used to
  look up the image in the image cache. However, if no platform is passed and the
  given image _does_ exist in the local image cache, but its OS or architecture
  do not match, the container is created with the available image, and a warning
  is added to the `Warnings` field in the response, for example;

      WARNING: The requested image's platform (linux/arm64/v8) does not
               match the detected host platform (linux/amd64) and no
               specific platform was requested

* `POST /containers/create` on Linux now accepts the `HostConfig.CgroupnsMode` property.
  Set the property to `host` to create the container in the daemon's cgroup namespace, or
  `private` to create the container in its own private cgroup namespace.  The per-daemon
  default is `host`, and can be changed by using the`CgroupNamespaceMode` daemon configuration
  parameter.
* `GET /info` now  returns an `OSVersion` field, containing the operating system's
  version. This change is not versioned, and affects all API versions if the daemon
  has this patch.
* `GET /info` no longer returns the `SystemStatus` field if it does not have a
  value set. This change is not versioned, and affects all API versions if the
  daemon has this patch.
* `GET /services` now accepts query parameter `status`. When set `true`,
  services returned will include `ServiceStatus`, which provides Desired,
  Running, and Completed task counts for the service.
* `GET /services` may now include `ReplicatedJob` or `GlobalJob` as the `Mode`
  in a `ServiceSpec`.
* `GET /services/{id}` may now include `ReplicatedJob` or `GlobalJob` as the
  `Mode` in a `ServiceSpec`.
* `POST /services/create` now accepts `ReplicatedJob or `GlobalJob` as the `Mode`
  in the `ServiceSpec.
* `POST /services/{id}/update` accepts updating the fields of the
  `ReplicatedJob` object in the `ServiceSpec.Mode`. The service mode still
  cannot be changed, however.
* `GET /services` now includes `JobStatus` on Services with mode
  `ReplicatedJob` or `GlobalJob`.
* `GET /services/{id}` now includes `JobStatus` on Services with mode
  `ReplicatedJob` or `GlobalJob`.
* `GET /tasks` now includes `JobIteration` on Tasks spawned from a job-mode
  service.
* `GET /tasks/{id}` now includes `JobIteration` on the task if spawned from a
  job-mode service.
* `GET /containers/{id}/stats` now accepts a query param (`one-shot`) which, when used with `stream=false` fetches a
  single set of stats instead of waiting for two collection cycles to have 2 CPU stats over a 1 second period.
* The `KernelMemory` field in `HostConfig.Resources` is now deprecated.
* The `KernelMemory` field in `Info` is now deprecated.
* `GET /services` now returns `Ulimits` as part of `ContainerSpec`.
* `GET /services/{id}` now returns `Ulimits` as part of `ContainerSpec`.
* `POST /services/create` now accepts `Ulimits` as part of `ContainerSpec`.
* `POST /services/{id}/update` now accepts `Ulimits` as part of `ContainerSpec`.

## v1.40 API changes

* The `/_ping` endpoint can now be accessed both using `GET` or `HEAD` requests.
  when accessed using a `HEAD` request, all headers are returned, but the body
  is empty (`Content-Length: 0`). This change is not versioned, and affects all
  API versions if the daemon has this patch. Clients are recommended to try
  using `HEAD`, but fallback to `GET` if the `HEAD` requests fails.
* `GET /_ping` and `HEAD /_ping` now set `Cache-Control` and `Pragma` headers to
  prevent the result from being cached. This change is not versioned, and affects
  all API versions if the daemon has this patch.
* `GET /services` now returns `Sysctls` as part of the `ContainerSpec`.
* `GET /services/{id}` now returns `Sysctls` as part of the `ContainerSpec`.
* `POST /services/create` now accepts `Sysctls` as part of the `ContainerSpec`.
* `POST /services/{id}/update` now accepts `Sysctls` as part of the `ContainerSpec`.
* `POST /services/create` now accepts `Config` as part of `ContainerSpec.Privileges.CredentialSpec`.
* `POST /services/{id}/update` now accepts `Config` as part of `ContainerSpec.Privileges.CredentialSpec`.
* `POST /services/create` now includes `Runtime` as an option in `ContainerSpec.Configs`
* `POST /services/{id}/update` now includes `Runtime` as an option in `ContainerSpec.Configs`
* `GET /tasks` now  returns `Sysctls` as part of the `ContainerSpec`.
* `GET /tasks/{id}` now  returns `Sysctls` as part of the `ContainerSpec`.
* `GET /networks` now supports a `dangling` filter type. When set to `true` (or
  `1`), the endpoint returns all networks that are not in use by a container. When
  set to `false` (or `0`), only networks that are in use by one or more containers
  are returned.
* `GET /nodes` now supports a filter type `node.label` filter to filter nodes based
  on the node.label. The format of the label filter is `node.label=<key>`/`node.label=<key>=<value>`
  to return those with the specified labels, or `node.label!=<key>`/`node.label!=<key>=<value>`
  to return those without the specified labels.
* `POST /containers/create` now accepts a `fluentd-async` option in `HostConfig.LogConfig.Config`
  when using the Fluentd logging driver. This option deprecates the `fluentd-async-connect`
  option, which remains functional, but will be removed in a future release. Users
  are encouraged to use the `fluentd-async` option going forward. This change is
  not versioned, and affects all API versions if the daemon has this patch.
* `POST /containers/create` now accepts a `fluentd-request-ack` option in
  `HostConfig.LogConfig.Config` when using the Fluentd logging driver. If enabled,
  the Fluentd logging driver sends the chunk option with a unique ID. The server
  will respond with an acknowledgement. This option improves the reliability of
  the message transmission. This change is not versioned, and affects all API
  versions if the daemon has this patch.
* `POST /containers/create`, `GET /containers/{id}/json`, and `GET /containers/json` now supports
  `BindOptions.NonRecursive`.
* `POST /swarm/init` now accepts a `DataPathPort` property to set data path port number.
* `GET /info` now returns information about `DataPathPort` that is currently used in swarm
* `GET /info` now returns `PidsLimit` boolean to indicate if the host kernel has
  PID limit support enabled.
* `GET /info` now includes `name=rootless` in `SecurityOptions` when the daemon is running in
  rootless mode.  This change is not versioned, and affects all API versions if the daemon has
  this patch.
* `GET /info` now returns `none` as `CgroupDriver` when the daemon is running in rootless mode.
  This change is not versioned, and affects all API versions if the daemon has this patch.
* `POST /containers/create` now accepts `DeviceRequests` as part of `HostConfig`.
  Can be used to set Nvidia GPUs.
* `GET /swarm` endpoint now returns DataPathPort info
* `POST /containers/create` now takes `KernelMemoryTCP` field to set hard limit for kernel TCP buffer memory.
* `GET /service` now  returns `MaxReplicas` as part of the `Placement`.
* `GET /service/{id}` now  returns `MaxReplicas` as part of the `Placement`.
* `POST /service/create` and `POST /services/(id or name)/update` now take the field `MaxReplicas`
  as part of the service `Placement`, allowing to specify maximum replicas per node for the service.
* `POST /containers/create` on Linux now creates a container with `HostConfig.IpcMode=private`
  by default, if IpcMode is not explicitly specified. The per-daemon default can be changed
  back to `shareable` by using `DefaultIpcMode` daemon configuration parameter.
* `POST /containers/{id}/update` now accepts a `PidsLimit` field to tune a container's
  PID limit. Set `0` or `-1` for unlimited. Leave `null` to not change the current value.
* `POST /build` now accepts `outputs` key for configuring build outputs when using BuildKit mode.

## V1.39 API changes

* `GET /info` now returns an empty string, instead of `<unknown>` for `KernelVersion`
  and `OperatingSystem` if the daemon was unable to obtain this information.
* `GET /info` now returns information about the product license, if a license
  has been applied to the daemon.
* `GET /info` now returns a `Warnings` field, containing warnings and informational
  messages about missing features, or issues related to the daemon configuration.
* `POST /swarm/init` now accepts a `DefaultAddrPool` property to set global scope default address pool
* `POST /swarm/init` now accepts a `SubnetSize` property to set global scope networks by giving the
  length of the subnet masks for every such network
* `POST /session` (added in [V1.31](#v131-api-changes) is no longer experimental.
  This endpoint can be used to run interactive long-running protocols between the
  client and the daemon.

## V1.38 API changes

* `GET /tasks` and `GET /tasks/{id}` now return a `NetworkAttachmentSpec` field,
  containing the `ContainerID` for non-service containers connected to "attachable"
  swarm-scoped networks.

## v1.37 API changes

* `POST /containers/create` and `POST /services/create` now supports exposing SCTP ports.
* `POST /configs/create` and `POST /configs/{id}/create` now accept a `Templating` driver.
* `GET /configs` and `GET /configs/{id}` now return the `Templating` driver of the config.
* `POST /secrets/create` and `POST /secrets/{id}/create` now accept a `Templating` driver.
* `GET /secrets` and `GET /secrets/{id}` now return the `Templating` driver of the secret.

## v1.36 API changes

* `Get /events` now return `exec_die` event when an exec process terminates.


## v1.35 API changes

* `POST /services/create` and `POST /services/(id)/update` now accepts an
  `Isolation` field on container spec to set the Isolation technology of the
  containers running the service (`default`, `process`, or `hyperv`). This
  configuration is only used for Windows containers.
* `GET /containers/(name)/logs` now supports an additional query parameter: `until`,
  which returns log lines that occurred before the specified timestamp.
* `POST /containers/{id}/exec` now accepts a `WorkingDir` property to set the
  work-dir for the exec process, independent of the container's work-dir.
* `Get /version` now returns a `Platform.Name` field, which can be used by products
  using Moby as a foundation to return information about the platform.
* `Get /version` now returns a `Components` field, which can be used to return
  information about the components used. Information about the engine itself is
  now included as a "Component" version, and contains all information from the
  top-level `Version`, `GitCommit`, `APIVersion`, `MinAPIVersion`, `GoVersion`,
  `Os`, `Arch`, `BuildTime`, `KernelVersion`, and `Experimental` fields. Going
  forward, the information from the `Components` section is preferred over their
  top-level counterparts.


## v1.34 API changes

* `POST /containers/(name)/wait?condition=removed` now also also returns
  in case of container removal failure. A pointer to a structure named
  `Error` added to the response JSON in order to indicate a failure.
  If `Error` is `null`, container removal has succeeded, otherwise
  the test of an error message indicating why container removal has failed
  is available from `Error.Message` field.

## v1.33 API changes

* `GET /events` now supports filtering 4 more kinds of events: `config`, `node`,
`secret` and `service`.

## v1.32 API changes

* `POST /images/create` now accepts a `platform` parameter in the form of `os[/arch[/variant]]`.
* `POST /containers/create` now accepts additional values for the
  `HostConfig.IpcMode` property. New values are `private`, `shareable`,
  and `none`.
* `DELETE /networks/{id or name}` fixed issue where a `name` equal to another
  network's name was able to mask that `id`. If both a network with the given
  _name_ exists, and a network with the given _id_, the network with the given
  _id_ is now deleted. This change is not versioned, and affects all API versions
  if the daemon has this patch.

## v1.31 API changes

* `DELETE /secrets/(name)` now returns status code 404 instead of 500 when the secret does not exist.
* `POST /secrets/create` now returns status code 409 instead of 500 when creating an already existing secret.
* `POST /secrets/create` now accepts a `Driver` struct, allowing the
  `Name` and driver-specific `Options` to be passed to store a secrets
  in an external secrets store. The `Driver` property can be omitted
  if the default (internal) secrets store is used.
* `GET /secrets/(id)` and `GET /secrets` now return a `Driver` struct,
  containing the `Name` and driver-specific `Options` of the external
  secrets store used to store the secret. The `Driver` property is
  omitted if no external store is used.
* `POST /secrets/(name)/update` now returns status code 400 instead of 500 when updating a secret's content which is not the labels.
* `POST /nodes/(name)/update` now returns status code 400 instead of 500 when demoting last node fails.
* `GET /networks/(id or name)` now takes an optional query parameter `scope` that will filter the network based on the scope (`local`, `swarm`, or `global`).
* `POST /session` is a new endpoint that can be used for running interactive long-running protocols between client and
  the daemon. This endpoint is experimental and only available if the daemon is started with experimental features
  enabled.
* `GET /images/(name)/get` now includes an `ImageMetadata` field which contains image metadata that is local to the engine and not part of the image config.
* `POST /services/create` now accepts a `PluginSpec` when `TaskTemplate.Runtime` is set to `plugin`
* `GET /events` now supports config events `create`, `update` and `remove` that are emitted when users create, update or remove a config
* `GET /volumes/` and `GET /volumes/{name}` now return a `CreatedAt` field,
  containing the date/time the volume was created. This field is omitted if the
  creation date/time for the volume is unknown. For volumes with scope "global",
  this field represents the creation date/time of the local _instance_ of the
  volume, which may differ from instances of the same volume on different nodes.
* `GET /system/df` now returns a `CreatedAt` field for `Volumes`. Refer to the
  `/volumes/` endpoint for a description of this field.

## v1.30 API changes

* `GET /info` now returns the list of supported logging drivers, including plugins.
* `GET /info` and `GET /swarm` now returns the cluster-wide swarm CA info if the node is in a swarm: the cluster root CA certificate, and the cluster TLS
 leaf certificate issuer's subject and public key. It also displays the desired CA signing certificate, if any was provided as part of the spec.
* `POST /build/` now (when not silent) produces an `Aux` message in the JSON output stream with payload `types.BuildResult` for each image produced. The final such message will reference the image resulting from the build.
* `GET /nodes` and `GET /nodes/{id}` now returns additional information about swarm TLS info if the node is part of a swarm: the trusted root CA, and the
 issuer's subject and public key.
* `GET /distribution/(name)/json` is a new endpoint that returns a JSON output stream with payload `types.DistributionInspect` for an image name. It includes a descriptor with the digest, and supported platforms retrieved from directly contacting the registry.
* `POST /swarm/update` now accepts 3 additional parameters as part of the swarm spec's CA configuration; the desired CA certificate for
 the swarm, the desired CA key for the swarm (if not using an external certificate), and an optional parameter to force swarm to
 generate and rotate to a new CA certificate/key pair.
* `POST /service/create` and `POST /services/(id or name)/update` now take the field `Platforms` as part of the service `Placement`, allowing to specify platforms supported by the service.
* `POST /containers/(name)/wait` now accepts a `condition` query parameter to indicate which state change condition to wait for. Also, response headers are now returned immediately to acknowledge that the server has registered a wait callback for the client.
* `POST /swarm/init` now accepts a `DataPathAddr` property to set the IP-address or network interface to use for data traffic
* `POST /swarm/join` now accepts a `DataPathAddr` property to set the IP-address or network interface to use for data traffic
* `GET /events` now supports service, node and secret events which are emitted when users create, update and remove service, node and secret
* `GET /events` now supports network remove event which is emitted when users remove a swarm scoped network
* `GET /events` now supports a filter type `scope` in which supported value could be swarm and local
* `PUT /containers/(name)/archive` now accepts a `copyUIDGID` parameter to allow copy UID/GID maps to dest file or dir.

## v1.29 API changes

* `DELETE /networks/(name)` now allows to remove the ingress network, the one used to provide the routing-mesh.
* `POST /networks/create` now supports creating the ingress network, by specifying an `Ingress` boolean field. As of now this is supported only when using the overlay network driver.
* `GET /networks/(name)` now returns an `Ingress` field showing whether the network is the ingress one.
* `GET /networks/` now supports a `scope` filter to filter networks based on the network mode (`swarm`, `global`, or `local`).
* `POST /containers/create`, `POST /service/create` and `POST /services/(id or name)/update` now takes the field `StartPeriod` as a part of the `HealthConfig` allowing for specification of a period during which the container should not be considered unhealthy even if health checks do not pass.
* `GET /services/(id)` now accepts an `insertDefaults` query-parameter to merge default values into the service inspect output.
* `POST /containers/prune`, `POST /images/prune`, `POST /volumes/prune`, and `POST /networks/prune` now support a `label` filter to filter containers, images, volumes, or networks based on the label. The format of the label filter could be `label=<key>`/`label=<key>=<value>` to remove those with the specified labels, or `label!=<key>`/`label!=<key>=<value>` to remove those without the specified labels.
* `POST /services/create` now accepts `Privileges` as part of `ContainerSpec`. Privileges currently include
  `CredentialSpec` and `SELinuxContext`.

## v1.28 API changes

* `POST /containers/create` now includes a `Consistency` field to specify the consistency level for each `Mount`, with possible values `default`, `consistent`, `cached`, or `delegated`.
* `GET /containers/create` now takes a `DeviceCgroupRules` field in `HostConfig` allowing to set custom device cgroup rules for the created container.
* Optional query parameter `verbose` for `GET /networks/(id or name)` will now list all services with all the tasks, including the non-local tasks on the given network.
* `GET /containers/(id or name)/attach/ws` now returns WebSocket in binary frame format for API version >= v1.28, and returns WebSocket in text frame format for API version< v1.28, for the purpose of backward-compatibility.
* `GET /networks` is optimised only to return list of all networks and network specific information. List of all containers attached to a specific network is removed from this API and is only available using the network specific `GET /networks/{network-id}`.
* `GET /containers/json` now supports `publish` and `expose` filters to filter containers that expose or publish certain ports.
* `POST /services/create` and `POST /services/(id or name)/update` now accept the `ReadOnly` parameter, which mounts the container's root filesystem as read only.
* `POST /build` now accepts `extrahosts` parameter to specify a host to ip mapping to use during the build.
* `POST /services/create` and `POST /services/(id or name)/update` now accept a `rollback` value for `FailureAction`.
* `POST /services/create` and `POST /services/(id or name)/update` now accept an optional `RollbackConfig` object which specifies rollback options.
* `GET /services` now supports a `mode` filter to filter services based on the service mode (either `global` or `replicated`).
* `POST /containers/(name)/update` now supports updating `NanoCpus` that represents CPU quota in units of 10<sup>-9</sup> CPUs.
* `POST /plugins/{name}/disable` now accepts a `force` query-parameter to disable a plugin even if still in use.

## v1.27 API changes

* `GET /containers/(id or name)/stats` now includes an `online_cpus` field in both `precpu_stats` and `cpu_stats`. If this field is `nil` then for compatibility with older daemons the length of the corresponding `cpu_usage.percpu_usage` array should be used.

## v1.26 API changes

* `POST /plugins/(plugin name)/upgrade` upgrade a plugin.

## v1.25 API changes

* The API version is now required in all API calls. Instead of just requesting, for example, the URL `/containers/json`, you must now request `/v1.25/containers/json`.
* `GET /version` now returns `MinAPIVersion`.
* `POST /build` accepts `networkmode` parameter to specify network used during build.
* `GET /images/(name)/json` now returns `OsVersion` if populated
* `GET /images/(name)/json` no longer contains the `RootFS.BaseLayer` field. This
  field was used for Windows images that used a base-image that was pre-installed
  on the host (`RootFS.Type` `layers+base`), which is no longer supported, and
  the `RootFS.BaseLayer` field has been removed.
* `GET /info` now returns `Isolation`.
* `POST /containers/create` now takes `AutoRemove` in HostConfig, to enable auto-removal of the container on daemon side when the container's process exits.
* `GET /containers/json` and `GET /containers/(id or name)/json` now return `"removing"` as a value for the `State.Status` field if the container is being removed. Previously, "exited" was returned as status.
* `GET /containers/json` now accepts `removing` as a valid value for the `status` filter.
* `GET /containers/json` now supports filtering containers by `health` status.
* `DELETE /volumes/(name)` now accepts a `force` query parameter to force removal of volumes that were already removed out of band by the volume driver plugin.
* `POST /containers/create/` and `POST /containers/(name)/update` now validates restart policies.
* `POST /containers/create` now validates IPAMConfig in NetworkingConfig, and returns error for invalid IPv4 and IPv6 addresses (`--ip` and `--ip6` in `docker create/run`).
* `POST /containers/create` now takes a `Mounts` field in `HostConfig` which replaces `Binds`, `Volumes`, and `Tmpfs`. *note*: `Binds`, `Volumes`, and `Tmpfs` are still available and can be combined with `Mounts`.
* `POST /build` now performs a preliminary validation of the `Dockerfile` before starting the build, and returns an error if the syntax is incorrect. Note that this change is _unversioned_ and applied to all API versions.
* `POST /build` accepts `cachefrom` parameter to specify images used for build cache.
* `GET /networks/` endpoint now correctly returns a list of *all* networks,
  instead of the default network if a trailing slash is provided, but no `name`
  or `id`.
* `DELETE /containers/(name)` endpoint now returns an error of `removal of container name is already in progress` with status code of 400, when container name is in a state of removal in progress.
* `GET /containers/json` now supports a `is-task` filter to filter
  containers that are tasks (part of a service in swarm mode).
* `POST /containers/create` now takes `StopTimeout` field.
* `POST /services/create` and `POST /services/(id or name)/update` now accept `Monitor` and `MaxFailureRatio` parameters, which control the response to failures during service updates.
* `POST /services/(id or name)/update` now accepts a `ForceUpdate` parameter inside the `TaskTemplate`, which causes the service to be updated even if there are no changes which would ordinarily trigger an update.
* `POST /services/create` and `POST /services/(id or name)/update` now return a `Warnings` array.
* `GET /networks/(name)` now returns field `Created` in response to show network created time.
* `POST /containers/(id or name)/exec` now accepts an `Env` field, which holds a list of environment variables to be set in the context of the command execution.
* `GET /volumes`, `GET /volumes/(name)`, and `POST /volumes/create` now return the `Options` field which holds the driver specific options to use for when creating the volume.
* `GET /exec/(id)/json` now returns `Pid`, which is the system pid for the exec'd process.
* `POST /containers/prune` prunes stopped containers.
* `POST /images/prune` prunes unused images.
* `POST /volumes/prune` prunes unused volumes.
* `POST /networks/prune` prunes unused networks.
* Every API response now includes a `Docker-Experimental` header specifying if experimental features are enabled (value can be `true` or `false`).
* Every API response now includes a `API-Version` header specifying the default API version of the server.
* The `hostConfig` option now accepts the fields `CpuRealtimePeriod` and `CpuRtRuntime` to allocate cpu runtime to rt tasks when `CONFIG_RT_GROUP_SCHED` is enabled in the kernel.
* The `SecurityOptions` field within the `GET /info` response now includes `userns` if user namespaces are enabled in the daemon.
* `GET /nodes` and `GET /node/(id or name)` now return `Addr` as part of a node's `Status`, which is the address that that node connects to the manager from.
* The `HostConfig` field now includes `NanoCpus` that represents CPU quota in units of 10<sup>-9</sup> CPUs.
* `GET /info` now returns more structured information about security options.
* The `HostConfig` field now includes `CpuCount` that represents the number of CPUs available for execution by the container. Windows daemon only.
* `POST /services/create` and `POST /services/(id or name)/update` now accept the `TTY` parameter, which allocate a pseudo-TTY in container.
* `POST /services/create` and `POST /services/(id or name)/update` now accept the `DNSConfig` parameter, which specifies DNS related configurations in resolver configuration file (resolv.conf) through `Nameservers`, `Search`, and `Options`.
* `POST /services/create` and `POST /services/(id or name)/update` now support
  `node.platform.arch` and `node.platform.os` constraints in the services
  `TaskSpec.Placement.Constraints` field.
* `GET /networks/(id or name)` now includes IP and name of all peers nodes for swarm mode overlay networks.
* `GET /plugins` list plugins.
* `POST /plugins/pull?name=<plugin name>` pulls a plugin.
* `GET /plugins/(plugin name)` inspect a plugin.
* `POST /plugins/(plugin name)/set` configure a plugin.
* `POST /plugins/(plugin name)/enable` enable a plugin.
* `POST /plugins/(plugin name)/disable` disable a plugin.
* `POST /plugins/(plugin name)/push` push a plugin.
* `POST /plugins/create?name=(plugin name)` create a plugin.
* `DELETE /plugins/(plugin name)` delete a plugin.
* `POST /node/(id or name)/update` now accepts both `id` or `name` to identify the node to update.
* `GET /images/json` now support a `reference` filter.
* `GET /secrets` returns information on the secrets.
* `POST /secrets/create` creates a secret.
* `DELETE /secrets/{id}` removes the secret `id`.
* `GET /secrets/{id}` returns information on the secret `id`.
* `POST /secrets/{id}/update` updates the secret `id`.
* `POST /services/(id or name)/update` now accepts service name or prefix of service id as a parameter.
* `POST /containers/create` added 2 built-in log-opts that work on all logging drivers,
  `mode` (`blocking`|`non-blocking`), and `max-buffer-size` (e.g. `2m`) which enables a non-blocking log buffer.
* `POST /containers/create` now takes `HostConfig.Init` field to run an init
  inside the container that forwards signals and reaps processes.

## v1.24 API changes

* `POST /containers/create` now takes `StorageOpt` field.
* `GET /info` now returns `SecurityOptions` field, showing if `apparmor`, `seccomp`, or `selinux` is supported.
* `GET /info` no longer returns the `ExecutionDriver` property. This property was no longer used after integration
  with ContainerD in Docker 1.11.
* `GET /networks` now supports filtering by `label` and `driver`.
* `GET /containers/json` now supports filtering containers by `network` name or id.
* `POST /containers/create` now takes `IOMaximumBandwidth` and `IOMaximumIOps` fields. Windows daemon only.
* `POST /containers/create` now returns an HTTP 400 "bad parameter" message
  if no command is specified (instead of an HTTP 500 "server error")
* `GET /images/search` now takes a `filters` query parameter.
* `GET /events` now supports a `reload` event that is emitted when the daemon configuration is reloaded.
* `GET /events` now supports filtering by daemon name or ID.
* `GET /events` now supports a `detach` event that is emitted on detaching from container process.
* `GET /events` now supports an `exec_detach ` event that is emitted on detaching from exec process.
* `GET /images/json` now supports filters `since` and `before`.
* `POST /containers/(id or name)/start` no longer accepts a `HostConfig`.
* `POST /images/(name)/tag` no longer has a `force` query parameter.
* `GET /images/search` now supports maximum returned search results `limit`.
* `POST /containers/{name:.*}/copy` is now removed and errors out starting from this API version.
* API errors are now returned as JSON instead of plain text.
* `POST /containers/create` and `POST /containers/(id)/start` allow you to configure kernel parameters (sysctls) for use in the container.
* `POST /containers/<container ID>/exec` and `POST /exec/<exec ID>/start`
  no longer expects a "Container" field to be present. This property was not used
  and is no longer sent by the docker client.
* `POST /containers/create/` now validates the hostname (should be a valid RFC 1123 hostname).
* `POST /containers/create/` `HostConfig.PidMode` field now accepts `container:<name|id>`,
  to have the container join the PID namespace of an existing container.

## v1.23 API changes

* `GET /containers/json` returns the state of the container, one of `created`, `restarting`, `running`, `paused`, `exited` or `dead`.
* `GET /containers/json` returns the mount points for the container.
* `GET /networks/(name)` now returns an `Internal` field showing whether the network is internal or not.
* `GET /networks/(name)` now returns an `EnableIPv6` field showing whether the network has ipv6 enabled or not.
* `POST /containers/(name)/update` now supports updating container's restart policy.
* `POST /networks/create` now supports enabling ipv6 on the network by setting the `EnableIPv6` field (doing this with a label will no longer work).
* `GET /info` now returns `CgroupDriver` field showing what cgroup driver the daemon is using; `cgroupfs` or `systemd`.
* `GET /info` now returns `KernelMemory` field, showing if "kernel memory limit" is supported.
* `POST /containers/create` now takes `PidsLimit` field, if the kernel is >= 4.3 and the pids cgroup is supported.
* `GET /containers/(id or name)/stats` now returns `pids_stats`, if the kernel is >= 4.3 and the pids cgroup is supported.
* `POST /containers/create` now allows you to override usernamespaces remapping and use privileged options for the container.
* `POST /containers/create` now allows specifying `nocopy` for named volumes, which disables automatic copying from the container path to the volume.
* `POST /auth` now returns an `IdentityToken` when supported by a registry.
* `POST /containers/create` with both `Hostname` and `Domainname` fields specified will result in the container's hostname being set to `Hostname`, rather than `Hostname.Domainname`.
* `GET /volumes` now supports more filters, new added filters are `name` and `driver`.
* `GET /containers/(id or name)/logs` now accepts a `details` query parameter to stream the extra attributes that were provided to the containers `LogOpts`, such as environment variables and labels, with the logs.
* `POST /images/load` now returns progress information as a JSON stream, and has a `quiet` query parameter to suppress progress details.

## v1.22 API changes

* The `HostConfig.LxcConf` field has been removed, and is no longer available on
  `POST /containers/create` and `GET /containers/(id)/json`.
* `POST /container/(name)/update` updates the resources of a container.
* `GET /containers/json` supports filter `isolation` on Windows.
* `GET /containers/json` now returns the list of networks of containers.
* `GET /info` Now returns `Architecture` and `OSType` fields, providing information
  about the host architecture and operating system type that the daemon runs on.
* `GET /networks/(name)` now returns a `Name` field for each container attached to the network.
* `GET /version` now returns the `BuildTime` field in RFC3339Nano format to make it
  consistent with other date/time values returned by the API.
* `AuthConfig` now supports a `registrytoken` for token based authentication
* `POST /containers/create` now has a 4M minimum value limit for `HostConfig.KernelMemory`
* Pushes initiated with `POST /images/(name)/push` and pulls initiated with `POST /images/create`
  will be cancelled if the HTTP connection making the API request is closed before
  the push or pull completes.
* `POST /containers/create` now allows you to set a read/write rate limit for a
  device (in bytes per second or IO per second).
* `GET /networks` now supports filtering by `name`, `id` and `type`.
* `POST /containers/create` now allows you to set the static IPv4 and/or IPv6 address for the container.
* `POST /networks/(id)/connect` now allows you to set the static IPv4 and/or IPv6 address for the container.
* `GET /info` now includes the number of containers running, stopped, and paused.
* `POST /networks/create` now supports restricting external access to the network by setting the `Internal` field.
* `POST /networks/(id)/disconnect` now includes a `Force` option to forcefully disconnect a container from network
* `GET /containers/(id)/json` now returns the `NetworkID` of containers.
* `POST /networks/create` Now supports an options field in the IPAM config that provides options
  for custom IPAM plugins.
* `GET /networks/{network-id}` Now returns IPAM config options for custom IPAM plugins if any
  are available.
* `GET /networks/<network-id>` now returns subnets info for user-defined networks.
* `GET /info` can now return a `SystemStatus` field useful for returning additional information about applications
  that are built on top of engine.

## v1.21 API changes

* `GET /volumes` lists volumes from all volume drivers.
* `POST /volumes/create` to create a volume.
* `GET /volumes/(name)` get low-level information about a volume.
* `DELETE /volumes/(name)` remove a volume with the specified name.
* `VolumeDriver` was moved from `config` to `HostConfig` to make the configuration portable.
* `GET /images/(name)/json` now returns information about an image's `RepoTags` and `RepoDigests`.
* The `config` option now accepts the field `StopSignal`, which specifies the signal to use to kill a container.
* `GET /containers/(id)/stats` will return networking information respectively for each interface.
* The `HostConfig` option now includes the `DnsOptions` field to configure the container's DNS options.
* `POST /build` now optionally takes a serialized map of build-time variables.
* `GET /events` now includes a `timenano` field, in addition to the existing `time` field.
* `GET /events` now supports filtering by image and container labels.
* `GET /info` now lists engine version information and return the information of `CPUShares` and `Cpuset`.
* `GET /containers/json` will return `ImageID` of the image used by container.
* `POST /exec/(name)/start` will now return an HTTP 409 when the container is either stopped or paused.
* `POST /containers/create` now takes `KernelMemory` in HostConfig to specify kernel memory limit.
* `GET /containers/(name)/json` now accepts a `size` parameter. Setting this parameter to '1' returns container size information in the `SizeRw` and `SizeRootFs` fields.
* `GET /containers/(name)/json` now returns a `NetworkSettings.Networks` field,
  detailing network settings per network. This field deprecates the
  `NetworkSettings.EndpointID`, `NetworkSettings.Gateway`, `NetworkSettings.GlobalIPv6Address`,
  `NetworkSettings.GlobalIPv6PrefixLen` `NetworkSettings.IPAddress`, `NetworkSettings.IPPrefixLen`,
  `NetworkSettings.IPv6Gateway`, `NetworkSettings.MacAddress` fields, which are
  still returned for backward-compatibility, but will be removed in a future version.
* `GET /exec/(id)/json` now returns a `NetworkSettings.Networks` field,
  detailing networksettings per network. This field deprecates the
  `NetworkSettings.Gateway`, `NetworkSettings.IPAddress`,
  `NetworkSettings.IPPrefixLen`, and `NetworkSettings.MacAddress` fields, which
  are still returned for backward-compatibility, but will be removed in a future version.
* The `HostConfig` option now includes the `OomScoreAdj` field for adjusting the
  badness heuristic. This heuristic selects which processes the OOM killer kills
  under out-of-memory conditions.

## v1.20 API changes

* `GET /containers/(id)/archive` get an archive of filesystem content from a container.
* `PUT /containers/(id)/archive` upload an archive of content to be extracted to
an existing directory inside a container's filesystem.
* `POST /containers/(id)/copy` is deprecated in favor of the above `archive`
endpoint which can be used to download files and directories from a container.
* The `hostConfig` option now accepts the field `GroupAdd`, which specifies a
list of additional groups that the container process will run as.

## v1.19 API changes

* When the daemon detects a version mismatch with the client, usually when
the client is newer than the daemon, an HTTP 400 is now returned instead
of a 404.
* `GET /containers/(id)/stats` now accepts `stream` bool to get only one set of stats and disconnect.
* `GET /containers/(id)/logs` now accepts a `since` timestamp parameter.
* `GET /info` The fields `Debug`, `IPv4Forwarding`, `MemoryLimit`, and
`SwapLimit` are now returned as boolean instead of as an int. In addition, the
end point now returns the new boolean fields `CpuCfsPeriod`, `CpuCfsQuota`, and
`OomKillDisable`.
* The `hostConfig` option now accepts the fields `CpuPeriod` and `CpuQuota`
* `POST /build` accepts `cpuperiod` and `cpuquota` options

## v1.18 API changes

* `GET /version` now returns `Os`, `Arch` and `KernelVersion`.
* `POST /containers/create` and `POST /containers/(id)/start`allow you to  set ulimit settings for use in the container.
* `GET /info` now returns `SystemTime`, `HttpProxy`,`HttpsProxy` and `NoProxy`.
* `GET /images/json` added a `RepoDigests` field to include image digest information.
* `POST /build` can now set resource constraints for all containers created for the build.
* `CgroupParent` can be passed in the host config to setup container cgroups under a specific cgroup.
* `POST /build` closing the HTTP request cancels the build
* `POST /containers/(id)/exec` includes `Warnings` field to response.

### v1.17 API changes

* The build supports `LABEL` command. Use this to add metadata to an image. For
  example you could add data describing the content of an image. `LABEL
"com.example.vendor"="ACME Incorporated"`
* `POST /containers/(id)/attach` and `POST /exec/(id)/start`
* The Docker client now hints potential proxies about connection hijacking using HTTP Upgrade headers.
* `POST /containers/create` sets labels on container create describing the container.
* `GET /containers/json` returns the labels associated with the containers (`Labels`).
* `GET /containers/(id)/json` returns the list current execs associated with the
  container (`ExecIDs`). This endpoint now returns the container labels
  (`Config.Labels`).
* `POST /containers/(id)/rename` renames a container `id` to a new name.*
* `POST /containers/create` and `POST /containers/(id)/start` callers can pass
  `ReadonlyRootfs` in the host config to mount the container's root filesystem as
  read only.
* `GET /containers/(id)/stats` returns a live stream of a container's resource usage statistics.
* `GET /images/json` returns the labels associated with each image (`Labels`).


### v1.16 API changes

* `GET /info` returns the number of CPUs available on the machine (`NCPU`),
  total memory available (`MemTotal`), a user-friendly name describing the running Docker daemon (`Name`), a unique ID identifying the daemon (`ID`), and
  a list of daemon labels (`Labels`).
* `POST /containers/create` callers can set the new container's MAC address explicitly.
* Volumes are now initialized when the container is created.
* `POST /containers/(id)/copy` copies data which is contained in a volume.

### v1.15 API changes

* `POST /containers/create` can now set a container's `HostConfig` when creating a
  container. Previously this was only available when starting a container.

### v1.14 API changes

* `DELETE /containers/(id)` when using `force`, the container will be immediately killed with SIGKILL.
* `POST /containers/(id)/start` the `HostConfig` option accepts the field `CapAdd`, which specifies a list of capabilities
  to add, and the field `CapDrop`, which specifies a list of capabilities to drop.
* `POST /images/create` th `fromImage` and `repo` parameters support the
  `repo:tag` format. Consequently,  the `tag` parameter is now obsolete. Using the
  new format and the `tag` parameter at the same time will return an error.

## v1.13 API changes

* `GET /containers/(name)/json`

**New!**
The `HostConfig.Links` field is now filled correctly

**New!**
`Sockets` parameter added to the `/info` endpoint listing all the sockets the
daemon is configured to listen on.

`POST /containers/(name)/start`
`POST /containers/(name)/stop`

**New!**
`start` and `stop` will now return 304 if the container's status is not modified

`POST /commit`

**New!**
Added a `pause` parameter (default `true`) to pause the container during commit

## v1.12 API changes

- `POST /build` now supports a `forcerm` parameter to always remove containers.
- `GET /containers/(name)/json`,`GET /images/(name)/json`: JSON keys are now in CamelCase.
- `GET /images/search`: Trusted builds are now Automated Builds, and the `is_trusted`
  field was renamed to `is_automated`.
- The `POST /images/(name)/insert` endpoint has been removed.

## v1.11 API changes

### What's new

- Add new `GET /_ping` endpoint to check if the API server is ready to accept connections.
- `GET /events` now supports an `until` parameter to close connection after the given timestamp.
- `GET /containers/(id)/logs` is now the preferred method for getting container logs.

## v1.10 API changes

- `DELETE /images/(name)` now provides a `force` parameter to force delete of an
  image, even if it's tagged in multiple repositories.
- `DELETE /images/(name)` now provides a `noprune` parameter to prevent the
  deletion of parent images.
- `DELETE /containers/(id)` now provides a `force` parameter to force deleting
  a container, even if it is currently running.

## v1.9 API changes

- `POST /build` now takes a serialized ConfigFile which it uses to resolve the
  proper registry auth credentials for pulling the base image. Clients which
  previously implemented the version accepting an AuthConfig object must be
  updated.

## v1.8 API changes

- `POST /build` now returns build status as JSON stream. In  case of a build error,
  it returns the exit status of the failed command.
- `GET /containers/(id)/json` now returns the host config for the container.
- `POST /images/create`, `POST /images/(name)/insert` and `POST /images/(name)/push`
  now include a `progressDetail` object in the JSON. It's now possible to get the
  current value and the total of the progress without having to parse the string.

## v1.7 API changes

- The `GET /images/viz` endpoint was removed. The `images --viz` output is now
  generated in the client, using the `GET /images/json` endpoint.
- The `GET /images/json` response now returns a single entry per image with a 
  nested attribute indicating the repo/tags that apply to that image.

Instead of:

    HTTP/1.1 200 OK
    Content-Type: application/json

    [
      {
        "VirtualSize": 131506275,
        "Size": 131506275,
        "Created": 1365714795,
        "Id": "8dbd9e392a964056420e5d58ca5cc376ef18e2de93b5cc90e868a1bbc8318c1c",
        "Tag": "12.04",
        "Repository": "ubuntu"
      },
      {
        "VirtualSize": 131506275,
        "Size": 131506275,
        "Created": 1365714795,
        "Id": "8dbd9e392a964056420e5d58ca5cc376ef18e2de93b5cc90e868a1bbc8318c1c",
        "Tag": "latest",
        "Repository": "ubuntu"
      }
    ]

The returned json looks like this:

    HTTP/1.1 200 OK
    Content-Type: application/json

    [
      {
         "RepoTags": [
           "ubuntu:12.04",
           "ubuntu:latest"
         ],
         "Id": "8dbd9e392a964056420e5d58ca5cc376ef18e2de93b5cc90e868a1bbc8318c1c",
         "Created": 1365714795,
         "Size": 131506275,
         "VirtualSize": 131506275
      }
    ]

## v1.6 API changes

### What's new

- `POST /containers/(id)/attach` now provides a multiplexed response to allow
  splitting stderr from stdout. This is done by prefixing a header to each
  transmission. See the `POST /containers/(id)/attach` endpoint. The WebSocket
  attach is unchanged. Note that attach calls on the previous API version didn't
  change. Stdout and stderr are merged.

## v1.5 API changes

- `POST /images/create` now accepts registry credentials via an AuthConfig object
  sent through the `X-Registry-Auth` header.
- `POST /images/(name)/push` now requires the `AuthConfig` object to be passed
  through the `X-Registry-Auth` instead of the request body.
- `GET /containers/json` changed the format of the Ports entry to a list of dicts,
  each containing PublicPort, PrivatePort and Type describing a port mapping.

## v1.4 API changes

- `POST /images/create` now downloads all images in parallel when pulling a repo.
- `GET /containers/(id)/top` now accepts `ps` args, which is used by `docker top`,
  for example, `docker top <container_id> aux`.
- `GET /events` now includes the image's name.

## v1.3 API changes

- Add `GET /containers/(id)/top` endpoint to list the processes running inside
  the container.
- Add `GET /events` endpoint to monitor docker's events via streaming or via polling.
- `GET /containers/json` now provides a `size=1` option to get the size of the containers.
- `POST /containers/<id>/start` now accepts host-specific configuration (e.g., bind
  mounts) in the POST body for start calls.
- `POST /build`:
  - Simplify the upload of the build context
  - Simply stream a tarball instead of multipart upload with 4
    intermediary buffers
  - Simpler, less memory usage, less disk usage and faster

> **Warning**:
> The `POST /build` improvements are not reverse-compatible. Pre 1.3 clients will
> break on `POST /build`.

## v1.2 API changes

- The auth configuration is now handled by the client, and clients must send
  authConfig as body on `POST /images/(name)/push`.
- `GET /auth` is now deprecated.
- `POST /auth` now only checks the configuration but doesn't store it on the server
- `POST /images/<name>/delete` now only untags the image if it has children and
  removes all the untagged parents if has any.
- `POST /images/<name>/delete` now returns a JSON structure with the list of 
  images deleted/untagged.

## v1.1 API changes

`POST /images/create`, `POST /images/(name)/insert`, and `POST /images/(name)/push`
now use a JSON stream instead of HTML hijack, it looks like this:

    HTTP/1.1 200 OK
    Content-Type: application/json
    
    {"status":"Pushing..."}
    {"status":"Pushing", "progress":"1/? (n/a)"}
    {"error":"Invalid..."}
    ...

## v1.0 API changes

Initial version (docker [v0.3.3](https://github.com/docker/docker/commit/822056094aa31c224e78cd568e02fe5458a0eecc))

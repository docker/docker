---
description: "How to develop and use a plugin with the managed plugin system"
keywords: "API, Usage, plugins, documentation, developer"
title: Plugin Config Version 1 of Plugin V2
---

This document outlines the format of the V0 plugin configuration.

Plugin configs describe the various constituents of a Docker engine plugin.
Plugin configs can be serialized to JSON format with the following media types:

| Config Type | Media Type                              |
|-------------|-----------------------------------------|
| config      | `application/vnd.docker.plugin.v1+json` |

## Config Field Descriptions

Config provides the base accessible fields for working with V0 plugin format in
the registry.

- `description` string

  Description of the plugin

- `documentation` string

  Link to the documentation about the plugin

- `interface` PluginInterface

  Interface implemented by the plugins, struct consisting of the following fields:

  - `types` string array

    Types indicate what interface(s) the plugin currently implements.

    Supported types:

    - `docker.volumedriver/1.0`

    - `docker.networkdriver/1.0`

    - `docker.ipamdriver/1.0`

    - `docker.authz/1.0`

    - `docker.logdriver/1.0`

    - `docker.metricscollector/1.0`

  - `socket` string

    Socket is the name of the socket the engine should use to communicate with the plugins.
    the socket will be created in `/run/docker/plugins`.

- `entrypoint` string array

   Entrypoint of the plugin, see [`ENTRYPOINT`](https://docs.docker.com/reference/dockerfile/#entrypoint)

- `workdir` string

   Working directory of the plugin, see [`WORKDIR`](https://docs.docker.com/reference/dockerfile/#workdir)

- `network` PluginNetwork

  Network of the plugin, struct consisting of the following fields:

  - `type` string

    Network type.

    Supported types:

    - `bridge`
    - `host`
    - `none`

- `mounts` PluginMount array

  Mount of the plugin, struct consisting of the following fields.
  See [`MOUNTS`](https://github.com/opencontainers/runtime-spec/blob/master/config.md#mounts).

  - `name` string

    Name of the mount.

  - `description` string

    Description of the mount.

  - `source` string

    Source of the mount.

  - `destination` string

    Destination of the mount.

  - `type` string

    Mount type.

  - `options` string array

    Options of the mount.

- `ipchost` Boolean

   Access to host ipc namespace.

- `pidhost` Boolean

   Access to host PID namespace.

- `propagatedMount` string

   Path to be mounted as rshared, so that mounts under that path are visible to
   Docker. This is useful for volume plugins. This path will be bind-mounted
   outside of the plugin rootfs so it's contents are preserved on upgrade.

- `env` PluginEnv array

  Environment variables of the plugin, struct consisting of the following fields:

  - `name` string

    Name of the environment variable.

  - `description` string

    Description of the environment variable.

  - `value` string

    Value of the environment variable.

- `args` PluginArgs

  Arguments of the plugin, struct consisting of the following fields:

  - `name` string

    Name of the arguments.

  - `description` string

    Description of the arguments.

  - `value` string array

    Values of the arguments.

- `linux` PluginLinux

  - `capabilities` string array

    Capabilities of the plugin (Linux only), see list [`here`](https://github.com/opencontainers/runc/blob/master/libcontainer/SPEC.md#security)

  - `allowAllDevices` Boolean

    If `/dev` is bind mounted from the host, and allowAllDevices is set to true, the plugin will have `rwm` access to all devices on the host.

  - `devices` PluginDevice array

    Device of the plugin, (Linux only), struct consisting of the following fields.
    See [`DEVICES`](https://github.com/opencontainers/runtime-spec/blob/master/config-linux.md#devices).

    - `name` string

      Name of the device.

    - `description` string

      Description of the device.

    - `path` string

      Path of the device.

## Example Config

The following example shows the 'tiborvass/sample-volume-plugin' plugin config.

```json
{
  "Args": {
    "Description": "",
    "Name": "",
    "Settable": null,
    "Value": null
  },
  "Description": "A sample volume plugin for Docker",
  "Documentation": "https://docs.docker.com/engine/extend/plugins/",
  "Entrypoint": [
    "/usr/bin/sample-volume-plugin",
    "/data"
  ],
  "Env": [
    {
      "Description": "",
      "Name": "DEBUG",
      "Settable": [
        "value"
      ],
      "Value": "0"
    }
  ],
  "Interface": {
    "Socket": "plugin.sock",
    "Types": [
      "docker.volumedriver/1.0"
    ]
  },
  "Linux": {
    "Capabilities": null,
    "AllowAllDevices": false,
    "Devices": null
  },
  "Mounts": null,
  "Network": {
    "Type": ""
  },
  "PropagatedMount": "/data",
  "User": {},
  "Workdir": ""
}
```

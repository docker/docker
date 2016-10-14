---
aliases: [
"/engine/extend/"
]
title: "Plugin manifest"
description: "How develop and use a plugin with the managed plugin system"
keywords: ["API, Usage, plugins, documentation, developer"]
advisory: "experimental"
---

# Plugin Manifest Version 0 of Plugin V2

This document outlines the format of the V0 plugin manifest. The plugin
manifest described herein was introduced in the Docker daemon (experimental version) in the [v1.12.0
release](https://github.com/docker/docker/commit/f37117045c5398fd3dca8016ea8ca0cb47e7312b).

Plugin manifests describe the various constituents of a docker plugin. Plugin
manifests can be serialized to JSON format with the following media types:

Manifest Type  | Media Type
------------- | -------------
manifest  | "application/vnd.docker.plugin.v0+json"


## *Manifest* Field Descriptions

Manifest provides the base accessible fields for working with V0 plugin format
 in the registry.

- **`manifestVersion`** *string*

	version of the plugin manifest (This version uses V0)

- **`description`** *string*

	description of the plugin

- **`documentation`** *string*

  	link to the documentation about the plugin

- **`interface`** *PluginInterface*

   interface implemented by the plugins, struct consisting of the following fields

    - **`types`** *string array*

      types indicate what interface(s) the plugin currently implements.

      currently supported:

      	- **docker.volumedriver/1.0**

    - **`socket`** *string*

      socket is the name of the socket the engine should use to communicate with the plugins.
      the socket will be created in `/run/docker/plugins`.


- **`entrypoint`** *string array*

   entrypoint of the plugin, see [`ENTRYPOINT`](../reference/builder.md#entrypoint)

- **`workdir`** *string*

   workdir of the plugin, see [`WORKDIR`](../reference/builder.md#workdir)

- **`network`** *PluginNetwork*

   network of the plugin, struct consisting of the following fields

    - **`type`** *string*

      network type.

      currently supported:

      	- **bridge**
      	- **host**
      	- **none**

- **`capabilities`** *array*

   capabilities of the plugin (*Linux only*), see list [`here`](https://github.com/opencontainers/runc/blob/master/libcontainer/SPEC.md#security)

- **`mounts`** *PluginMount array*

   mount of the plugin, struct consisting of the following fields, see [`MOUNTS`](https://github.com/opencontainers/runtime-spec/blob/master/config.md#mounts)

    - **`name`** *string*

	  name of the mount.

    - **`description`** *string*

      description of the mount.

    - **`source`** *string*

	  source of the mount.

    - **`destination`** *string*

	  destination of the mount.

    - **`type`** *string*

      mount type.

    - **`options`** *string array*

	  options of the mount.

- **`devices`** *PluginDevice array*

    device of the plugin, (*Linux only*), struct consisting of the following fields, see [`DEVICES`](https://github.com/opencontainers/runtime-spec/blob/master/config-linux.md#devices)

    - **`name`** *string*

	  name of the device.

    - **`description`** *string*

      description of the device.

    - **`path`** *string*

	  path of the device.

- **`env`** *PluginEnv array*

   env of the plugin, struct consisting of the following fields

    - **`name`** *string*

	  name of the env.

    - **`description`** *string*

      description of the env.

    - **`value`** *string*

	  value of the env.

- **`args`** *PluginArgs*

   args of the plugin, struct consisting of the following fields

    - **`name`** *string*

	  name of the env.

    - **`description`** *string*

      description of the env.

    - **`value`** *string array*

	  values of the args.


## Example Manifest

*Example showing the 'tiborvass/no-remove' plugin manifest.*

```
{
       	"manifestVersion": "v0",
       	"description": "A test plugin for Docker",
       	"documentation": "https://docs.docker.com/engine/extend/plugins/",
       	"entrypoint": ["plugin-no-remove", "/data"],
       	"interface" : {
       		"types": ["docker.volumedriver/1.0"],
       		"socket": "plugins.sock"
       	},
       	"network": {
       		"type": "host"
       	},

       	"mounts": [
       		{
       			"source": "/data",
       			"destination": "/data",
       			"type": "bind",
       			"options": ["shared", "rbind"]
       		},
       		{
       			"destination": "/foobar",
       			"type": "tmpfs"
       		}
       	],

       	"args": {
       		"name": "args",
       		"description": "command line arguments",
       		"value": []
       	},

       	"env": [
       		{
       			"name": "DEBUG",
       			"description": "If set, prints debug messages",
       			"value": "1"
       		}
       	],

       	"devices": [
       		{
       			"name": "device",
       			"description": "a host device to mount",
       			"path": "/dev/cpu_dma_latency"
       		}
       	]
}

```

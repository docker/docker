---
aliases: [
"/engine/extend/"
]
title: "Managed plugin system"
description: "How develop and use a plugin with the managed plugin system"
keywords: ["API, Usage, plugins, documentation, developer"]
advisory: "experimental"
---

# Docker Engine managed plugin system

This document describes the plugin system available today in the **experimental
build** of Docker 1.12:

* [How to operate an existing plugin](#how-to-operate-a-plugin)
* [How to develop a plugin](#how-to-develop-a-plugin)

Unlike the legacy plugin system, you now manage plugins using Docker Engine:

* install plugins
* start plugins
* stop plugins
* remove plugins

The current Docker Engine plugin system only supports volume drivers. We are
adding more plugin driver types in the future releases.

For information on Docker Engine plugins generally available in Docker Engine
1.12 and earlier, refer to [Understand legacy Docker Engine plugins](legacy_plugins.md).

## How to operate a plugin

Plugins are distributed as Docker images, so develpers can host them on Docker
Hub or on a private registry.

You install the plugin using a single command: `docker plugin install <PLUGIN>`.
The `plugin install` command pulls the plugin from the Docker Hub or private
registry. If necessary the CLI prompts you to accept any privilige requriements.
For example the plugin may require access to a device on the host system.
Finally it enables the plugin.

Run `docker plugin ls` to check the status of installed plugins. The Engine
markes plugins that are started without issues as `ENABLED`.

After you install a plugin, the plugin behavior is the same as legacy plugins.
The following example demonstrates how to install the `sshfs` plugin and use it
to create a volume.

1.  Install the `sshfs` plugin.

    ```bash
    $ docker plugin install vieux/sshfs

    Plugin "vieux/sshfs" is requesting the following privileges:
    - network: [host]
    - capabilities: [CAP_SYS_ADMIN]
    Do you grant the above permissions? [y/N] y

    vieux/sshfs
    ```

    The plugin requests 2 privileges, the `CAP_SYS_ADMIN` capability to be able
    to do mount inside the plugin and `host networking`.

2. Check for a value of `true` the `ENABLED` column to verify the plugin
started without error.

    ```bash
    $ docker plugin ls

    NAME                TAG                 ENABLED
    vieux/sshfs         latest              true
    ```

3. Create a volume using the plugin.

    ```bash
    $ docker volume create \
      -d vieux/sshfs \
      --name sshvolume \
      -o sshcmd=user@1.2.3.4:/remote

    sshvolume
    ```

4.  Use the volume `sshvolume`.

    ```bash
    $ docker run -v sshvolume:/data busybox ls /data

    <content of /remote on machine 1.2.3.4>
    ```

5. Verify the plugin successfully created the volume.

    ```bash
    $ docker volume ls

    DRIVER              NAME
    vieux/sshfs         sshvolume
    ```

    You can stop a plugin with the `docker plugin disable`
    command or remove a plugin with `docker plugin remove`.

See the [command line reference](../reference/commandline/index.md) for more
information.

## How to develop a plugin

Plugin creation is currently a manual process. We plan to add automation in a
future release with a command such as `docker plugin build`.

This section describes the format of an existing enabled plugin. You have to
create and format the plugin files by hand.

Plugins are stored in `/var/lib/docker/plugins`. For instance:

```bash
# ls -la /var/lib/docker/plugins
total 20
drwx------  4 root root 4096 Aug  8 18:03 .
drwx--x--x 12 root root 4096 Aug  8 17:53 ..
drwxr-xr-x  3 root root 4096 Aug  8 17:56 cd851ce43a403
-rw-------  1 root root 2107 Aug  8 18:03 plugins.json
```

`plugins.json` is an inventory of all installed plugins. For example:

```bash
# cat plugins.json
{
  "cd851ce43a403": {
    "plugin": {
      "Manifest": {
        "Args": {
          "Value": null,
          "Settable": null,
          "Description": "",
          "Name": ""
        },
        "Env": null,
        "Devices": null,
        "Mounts": null,
        "Capabilities": [
          "CAP_SYS_ADMIN"
        ],
        "ManifestVersion": "v0",
        "Description": "sshFS plugin for Docker",
        "Documentation": "https://docs.docker.com/engine/extend/plugins/",
        "Interface": {
          "Socket": "sshfs.sock",
          "Types": [
            "docker.volumedriver/1.0"
          ]
        },
        "Entrypoint": [
          "/go/bin/docker-volume-sshfs"
        ],
        "Workdir": "",
        "User": {},
        "Network": {
          "Type": "host"
        }
      },
      "Config": {
        "Devices": null,
        "Args": null,
        "Env": [],
        "Mounts": []
      },
      "Active": true,
      "Tag": "latest",
      "Name": "vieux/sshfs",
      "Id": "cd851ce43a403"
    }
  }
}
```

Each folder represents a plugin. For example:

```bash
# ls -la /var/lib/docker/plugins/cd851ce43a403
total 12
drwx------ 19 root root 4096 Aug  8 17:56 rootfs
-rw-r--r--  1 root root   50 Aug  8 17:56 plugin-config.json
-rw-------  1 root root  347 Aug  8 17:56 manifest.json
```

`rootfs` represents the root filesystem of the plugin. In this example, it was
created from a Dockerfile as follows:

>**Note:** `/run/docker/plugins` is mandatory for docker to communicate with
the plugin._

```bash
$ git clone https://github.com/vieux/docker-volume-sshfs
$ cd docker-volume-sshfs
$ docker build -t rootfs .
$ id=$(docker create rootfs true) # id was cd851ce43a403 when the image was created
$ mkdir -p /var/lib/docker/plugins/$id/rootfs
$ docker export "$id" | tar -x -C /var/lib/docker/plugins/$id/rootfs
$ docker rm -vf "$id"
$ docker rmi rootfs
```

`manifest.json` describes the plugin and `plugin-config.json` contains some
runtime parameters. [See the Plugins Manifest reference](manifest.md). For example:

```bash
# cat manifest.json
{
	"manifestVersion": "v0",
	"description": "sshFS plugin for Docker",
	"documentation": "https://docs.docker.com/engine/extend/plugins/",
	"entrypoint": ["/go/bin/docker-volume-sshfs"],
	"network": {
		   "type": "host"
		   },
	"interface" : {
		   "types": ["docker.volumedriver/1.0"],
		   "socket": "sshfs.sock"
	},
	"capabilities": ["CAP_SYS_ADMIN"]
}
```

In this example, you can see the plugin is a volume driver, requires the
`CAP_SYS_ADMIN` capability, `host networking`, `/go/bin/docker-volume-sshfs` as
entrypoint and is going to use `/run/docker/plugins/sshfs.sock` to communicate
with the Docker Engine.

```bash
# cat plugin-config.json
{
  "Devices": null,
  "Args": null,
  "Env": [],
  "Mounts": []
}
```

This plugin doesn't require runtime parameters.

Both `manifest.json` and `plugin-config.json` are part of the `plugins.json`.
`manifest.json` is read-only and `plugin-config.json` is read-write.

To summarize, follow the steps below to create a plugin:

0. Choose a name for the plugin. Plugin name uses the same format as images,
for example: `<repo_name>/<name>`.
1. Create a rootfs in `/var/lib/docker/plugins/$id/rootfs`.
2. Create manifest.json file in `/var/lib/docker/plugins/$id/`.
3. Create a `plugin-config.json` if needed.
4. Create or add a section to `/var/lib/docker/plugins/plugins.json`. Use
   `<user>/<name>` as “Name” and `$id` as “Id”.
5. Restart the Docker Engine.
6. Run `docker plugin ls`.
    * If your plugin is listed as `ENABLED=true`, you can push it to the
    registry.
    * If the plugin is not listed or if `ENABLED=false`, something went wrong.
    Check the daemon logs for errors.
7. If you are not already logged in, use `docker login` to authenticate against
   a registry.
8. Run `docker plugin push <repo_name>/<name>` to push the plugin.

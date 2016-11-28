---
description: Develop and use a plugin with the managed plugin system
keywords: "API, Usage, plugins, documentation, developer"
title: Managed plugin system
---

<!-- This file is maintained within the docker/docker Github
     repository at https://github.com/docker/docker/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Docker Engine managed plugin system

* [Installing and using a plugin](index.md#installing-and-using-a-plugin)
* [Developing a plugin](index.md#developing-a-plugin)

Docker Engine's plugins system allows you to install, start, stop, and remove
plugins using Docker Engine. This mechanism is currently only available for
volume drivers, but more plugin driver types will be available in future releases.

For information about the legacy plugin system available in Docker Engine 1.12
and earlier, see [Understand legacy Docker Engine plugins](legacy_plugins.md).

> **Note**: Docker Engine managed plugins are currently not supported
on Windows daemons.

## Installing and using a plugin

Plugins are distributed as Docker images and can be hosted on Docker Hub or on
a private registry.

To install a plugin, use the `docker plugin install` command, which pulls the
plugin from Docker hub or your private registry, prompts you to grant
permissions or capabilities if necessary, and enables the plugin.

To check the status of installed plugins, use the `docker plugin ls` command.
Plugins that start successfully are listed as enabled in the output.

After a plugin is installed, you can use it as an option for another Docker
operation, such as creating a volume.

In the following example, you install the `sshfs` plugin, verify that it is
enabled, and use it to create a volume.

1.  Install the `sshfs` plugin.

    ```bash
    $ docker plugin install vieux/sshfs

    Plugin "vieux/sshfs" is requesting the following privileges:
    - network: [host]
    - capabilities: [CAP_SYS_ADMIN]
    Do you grant the above permissions? [y/N] y

    vieux/sshfs
    ```

    The plugin requests 2 privileges:
    - It needs access to the `host` network.
    - It needs the `CAP_SYS_ADMIN` capability, which allows the plugin to run
    the `mount` command.

2.  Check that the plugin is enabled in the output of `docker plugin ls`.

    ```bash
    $ docker plugin ls

    NAME                TAG                 ENABLED
    vieux/sshfs         latest              true
    ```

3.  Create a volume using the plugin.
    This example mounts the `/remote` directory on host `1.2.3.4` into a
    volume named `sshvolume`. This volume can now be mounted into containers.

    ```bash
    $ docker volume create \
      -d vieux/sshfs \
      --name sshvolume \
      -o sshcmd=user@1.2.3.4:/remote

    sshvolume
    ```
4.  Verify that the volume was created successfully.

    ```bash
    $ docker volume ls

    DRIVER              NAME
    vieux/sshfs         sshvolume
    ```

5.  Start a container that uses the volume `sshvolume`.

    ```bash
    $ docker run -v sshvolume:/data busybox ls /data

    <content of /remote on machine 1.2.3.4>
    ```

To disable a plugin, use the `docker plugin disable` command. To completely
remove it, use the `docker plugin remove` command. For other available
commands and options, see the
[command line reference](../reference/commandline/index.md).

## Developing a plugin

Currently, there are no CLI commands available to help you develop a plugin.
This is expected to change in a future release. The manual process for creating
plugins is described in this section.

### Plugin location and files

Plugins are stored in `/var/lib/docker/plugins`. The `plugins.json` file lists
each plugin's configuration, and each plugin is stored in a directory with a
unique identifier.

```bash
# ls -la /var/lib/docker/plugins
total 20
drwx------  4 root root 4096 Aug  8 18:03 .
drwx--x--x 12 root root 4096 Aug  8 17:53 ..
drwxr-xr-x  3 root root 4096 Aug  8 17:56 cd851ce43a403
-rw-------  1 root root 2107 Aug  8 18:03 plugins.json
```

### Format of plugins.json

The `plugins.json` is an inventory of all installed plugins. This example shows
a `plugins.json` with a single plugin installed.

```json
# cat plugins.json
{
  "cd851ce43a403": {
    "plugin": {
      "Config": {
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

### Contents of a plugin directory

Each directory within `/var/lib/docker/plugins/` contains a `rootfs` directory
and two JSON files.

```bash
# ls -la /var/lib/docker/plugins/cd851ce43a403
total 12
drwx------ 19 root root 4096 Aug  8 17:56 rootfs
-rw-r--r--  1 root root   50 Aug  8 17:56 plugin-settings.json
-rw-------  1 root root  347 Aug  8 17:56 config.json
```

#### The rootfs directory
The `rootfs` directory represents the root filesystem of the plugin. In this
example, it was created from a Dockerfile:

>**Note:** The `/run/docker/plugins` directory is mandatory inside of the
plugin's filesystem for docker to communicate with the plugin.

```bash
$ git clone https://github.com/vieux/docker-volume-sshfs
$ cd docker-volume-sshfs
$ docker build -t rootfs .
$ id=$(docker create rootfs true) # id was cd851ce43a403 when the image was created
$ sudo mkdir -p /var/lib/docker/plugins/$id/rootfs
$ sudo docker export "$id" | sudo tar -x -C /var/lib/docker/plugins/$id/rootfs
$ sudo chgrp -R docker /var/lib/docker/plugins/
$ docker rm -vf "$id"
$ docker rmi rootfs
```

#### The config.json and plugin-settings.json files

The `config.json` file describes the plugin. The `plugin-settings.json` file
contains runtime parameters and is only required if your plugin has runtime
parameters. [See the Plugins Config reference](config.md).

Consider the following `config.json` file.

```json
{
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

This plugin is a volume driver. It requires a `host` network and the
`CAP_SYS_ADMIN` capability. It depends upon the `/go/bin/docker-volume-sshfs`
entrypoint and uses the `/run/docker/plugins/sshfs.sock` socket to communicate
with Docker Engine.


Consider the following `plugin-settings.json` file.

```json
{
  "Devices": null,
  "Args": null,
  "Env": [],
  "Mounts": []
}
```

This plugin has no runtime parameters.

Each of these JSON files is included as part of `plugins.json`, as you can see
by looking back at the example above. After a plugin is installed, `config.json`
is read-only, but `plugin-settings.json` is read-write, and includes all runtime
configuration options for the plugin.

### Creating the plugin

Follow these steps to create a plugin:

1. Choose a name for the plugin. Plugin name uses the same format as images,
   for example: `<repo_name>/<name>`.

2. Create a `rootfs` and export it to `/var/lib/docker/plugins/$id/rootfs`
   using `docker export`. See [The rootfs directory](#the-rootfs-directory) for
   an example of creating a `rootfs`.

3. Create a `config.json` file in `/var/lib/docker/plugins/$id/`.

4. Create a `plugin-settings.json` file if needed.

5. Create or add a section to `/var/lib/docker/plugins/plugins.json`. Use
   `<user>/<name>` as “Name” and `$id` as “Id”.

6. Restart the Docker Engine service.

7. Run `docker plugin ls`.
    * If your plugin is enabled, you can push it to the
      registry.
    * If the plugin is not listed or is disabled, something went wrong.
      Check the daemon logs for errors.

8. If you are not already logged in, use `docker login` to authenticate against
   the registry so that you can push to it.

9. Run `docker plugin push <repo_name>/<name>` to push the plugin.

The plugin can now be used by any user with access to your registry.

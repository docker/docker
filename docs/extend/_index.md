---
title: Docker Engine managed plugin system
linkTitle: Docker Engine plugins
description: Develop and use a plugin with the managed plugin system
keywords: "API, Usage, plugins, documentation, developer"
aliases:
  - "/engine/extend/plugins_graphdriver/"
---

- [Installing and using a plugin](index.md#installing-and-using-a-plugin)
- [Developing a plugin](index.md#developing-a-plugin)
- [Debugging plugins](index.md#debugging-plugins)

Docker Engine's plugin system lets you install, start, stop, and remove
plugins using Docker Engine.

For information about legacy (non-managed) plugins, refer to
[Understand legacy Docker Engine plugins](legacy_plugins.md).

> [!NOTE]
> Docker Engine managed plugins are currently not supported on Windows daemons.

## Installing and using a plugin

Plugins are distributed as Docker images and can be hosted on Docker Hub or on
a private registry.

To install a plugin, use the `docker plugin install` command, which pulls the
plugin from Docker Hub or your private registry, prompts you to grant
permissions or capabilities if necessary, and enables the plugin.

To check the status of installed plugins, use the `docker plugin ls` command.
Plugins that start successfully are listed as enabled in the output.

After a plugin is installed, you can use it as an option for another Docker
operation, such as creating a volume.

In the following example, you install the `sshfs` plugin, verify that it is
enabled, and use it to create a volume.

> [!NOTE]
> This example is intended for instructional purposes only. Once the volume is
> created, your SSH password to the remote host is exposed as plaintext when
> inspecting the volume. Delete the volume as soon as you are done with the
> example.

1. Install the `sshfs` plugin.

   ```console
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

2. Check that the plugin is enabled in the output of `docker plugin ls`.

   ```console
   $ docker plugin ls

   ID                    NAME                  TAG                 DESCRIPTION                   ENABLED
   69553ca1d789          vieux/sshfs           latest              the `sshfs` plugin            true
   ```

3. Create a volume using the plugin.
   This example mounts the `/remote` directory on host `1.2.3.4` into a
   volume named `sshvolume`.

   This volume can now be mounted into containers.

   ```console
   $ docker volume create \
     -d vieux/sshfs \
     --name sshvolume \
     -o sshcmd=user@1.2.3.4:/remote \
     -o password=$(cat file_containing_password_for_remote_host)

   sshvolume
   ```

4. Verify that the volume was created successfully.

   ```console
   $ docker volume ls

   DRIVER              NAME
   vieux/sshfs         sshvolume
   ```

5. Start a container that uses the volume `sshvolume`.

   ```console
   $ docker run --rm -v sshvolume:/data busybox ls /data

   <content of /remote on machine 1.2.3.4>
   ```

6. Remove the volume `sshvolume`

   ```console
   $ docker volume rm sshvolume

   sshvolume
   ```

To disable a plugin, use the `docker plugin disable` command. To completely
remove it, use the `docker plugin remove` command. For other available
commands and options, see the
[command line reference](https://docs.docker.com/reference/cli/docker/).

## Developing a plugin

#### The rootfs directory

The `rootfs` directory represents the root filesystem of the plugin. In this
example, it was created from a Dockerfile:

> [!NOTE]
> The `/run/docker/plugins` directory is mandatory inside of the
> plugin's filesystem for Docker to communicate with the plugin.

```console
$ git clone https://github.com/vieux/docker-volume-sshfs
$ cd docker-volume-sshfs
$ docker build -t rootfsimage .
$ id=$(docker create rootfsimage true) # id was cd851ce43a403 when the image was created
$ sudo mkdir -p myplugin/rootfs
$ sudo docker export "$id" | sudo tar -x -C myplugin/rootfs
$ docker rm -vf "$id"
$ docker rmi rootfsimage
```

#### The config.json file

The `config.json` file describes the plugin. See the [plugins config reference](config.md).

Consider the following `config.json` file.

```json
{
  "description": "sshFS plugin for Docker",
  "documentation": "https://docs.docker.com/engine/extend/plugins/",
  "entrypoint": ["/docker-volume-sshfs"],
  "network": {
    "type": "host"
  },
  "interface": {
    "types": ["docker.volumedriver/1.0"],
    "socket": "sshfs.sock"
  },
  "linux": {
    "capabilities": ["CAP_SYS_ADMIN"]
  }
}
```

This plugin is a volume driver. It requires a `host` network and the
`CAP_SYS_ADMIN` capability. It depends upon the `/docker-volume-sshfs`
entrypoint and uses the `/run/docker/plugins/sshfs.sock` socket to communicate
with Docker Engine. This plugin has no runtime parameters.

#### Creating the plugin

A new plugin can be created by running
`docker plugin create <plugin-name> ./path/to/plugin/data` where the plugin
data contains a plugin configuration file `config.json` and a root filesystem
in subdirectory `rootfs`.

After that the plugin `<plugin-name>` will show up in `docker plugin ls`.
Plugins can be pushed to remote registries with
`docker plugin push <plugin-name>`.

## Debugging plugins

Stdout of a plugin is redirected to dockerd logs. Such entries have a
`plugin=<ID>` suffix. Here are a few examples of commands for pluginID
`f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62` and their
corresponding log entries in the docker daemon logs.

```console
$ docker plugin install tiborvass/sample-volume-plugin

INFO[0036] Starting...       Found 0 volumes on startup  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
```

```console
$ docker volume create -d tiborvass/sample-volume-plugin samplevol

INFO[0193] Create Called...  Ensuring directory /data/samplevol exists on host...  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0193] open /var/lib/docker/plugin-data/local-persist.json: no such file or directory  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0193]                   Created volume samplevol with mountpoint /data/samplevol  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0193] Path Called...    Returned path /data/samplevol  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
```

```console
$ docker run -v samplevol:/tmp busybox sh

INFO[0421] Get Called...     Found samplevol                plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0421] Mount Called...   Mounted samplevol              plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0421] Path Called...    Returned path /data/samplevol  plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
INFO[0421] Unmount Called... Unmounted samplevol            plugin=f52a3df433b9aceee436eaada0752f5797aab1de47e5485f1690a073b860ff62
```

#### Using runc to obtain logfiles and shell into the plugin.

Use `runc`, the default docker container runtime, for debugging plugins by
collecting plugin logs redirected to a file.

```console
$ sudo runc --root /run/docker/runtime-runc/plugins.moby list

ID                                                                 PID         STATUS      BUNDLE                                                                                                                                       CREATED                          OWNER
93f1e7dbfe11c938782c2993628c895cf28e2274072c4a346a6002446c949b25   15806       running     /run/docker/containerd/daemon/io.containerd.runtime.v1.linux/moby-plugins/93f1e7dbfe11c938782c2993628c895cf28e2274072c4a346a6002446c949b25   2018-02-08T21:40:08.621358213Z   root
9b4606d84e06b56df84fadf054a21374b247941c94ce405b0a261499d689d9c9   14992       running     /run/docker/containerd/daemon/io.containerd.runtime.v1.linux/moby-plugins/9b4606d84e06b56df84fadf054a21374b247941c94ce405b0a261499d689d9c9   2018-02-08T21:35:12.321325872Z   root
c5bb4b90941efcaccca999439ed06d6a6affdde7081bb34dc84126b57b3e793d   14984       running     /run/docker/containerd/daemon/io.containerd.runtime.v1.linux/moby-plugins/c5bb4b90941efcaccca999439ed06d6a6affdde7081bb34dc84126b57b3e793d   2018-02-08T21:35:12.321288966Z   root
```

```console
$ sudo runc --root /run/docker/runtime-runc/plugins.moby exec 93f1e7dbfe11c938782c2993628c895cf28e2274072c4a346a6002446c949b25 cat /var/log/plugin.log
```

If the plugin has a built-in shell, then exec into the plugin can be done as
follows:

```console
$ sudo runc --root /run/docker/runtime-runc/plugins.moby exec -t 93f1e7dbfe11c938782c2993628c895cf28e2274072c4a346a6002446c949b25 sh
```

#### Using curl to debug plugin socket issues.

To verify if the plugin API socket that the docker daemon communicates with
is responsive, use curl. In this example, we will make API calls from the
docker host to volume and network plugins using curl 7.47.0 to ensure that
the plugin is listening on the said socket. For a well functioning plugin,
these basic requests should work. Note that plugin sockets are available on the host under `/var/run/docker/plugins/<pluginID>`

```console
$ curl -H "Content-Type: application/json" -XPOST -d '{}' --unix-socket /var/run/docker/plugins/e8a37ba56fc879c991f7d7921901723c64df6b42b87e6a0b055771ecf8477a6d/plugin.sock http:/VolumeDriver.List

{"Mountpoint":"","Err":"","Volumes":[{"Name":"myvol1","Mountpoint":"/data/myvol1"},{"Name":"myvol2","Mountpoint":"/data/myvol2"}],"Volume":null}
```

```console
$ curl -H "Content-Type: application/json" -XPOST -d '{}' --unix-socket /var/run/docker/plugins/45e00a7ce6185d6e365904c8bcf62eb724b1fe307e0d4e7ecc9f6c1eb7bcdb70/plugin.sock http:/NetworkDriver.GetCapabilities

{"Scope":"local"}
```

When using curl 7.5 and above, the URL should be of the form
`http://hostname/APICall`, where `hostname` is the valid hostname where the
plugin is installed and `APICall` is the call to the plugin API.

For example, `http://localhost/VolumeDriver.List`

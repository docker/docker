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

> **Note**: This example is intended for instructional purposes only. Once the volume is created, your SSH password to the remote host will be exposed as plaintext when inspecting the volume. You should delete the volume as soon as you are done with the example.

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

    ID                    NAME                  TAG                 DESCRIPTION                   ENABLED
    69553ca1d789          vieux/sshfs           latest              the `sshfs` plugin            true
    ```

3.  Create a volume using the plugin.
    This example mounts the `/remote` directory on host `1.2.3.4` into a
    volume named `sshvolume`.   
   
    This volume can now be mounted into containers.

    ```bash
    $ docker volume create \
      -d vieux/sshfs \
      --name sshvolume \
      -o sshcmd=user@1.2.3.4:/remote \
      -o password=$(cat file_containing_password_for_remote_host)

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
    $ docker run --rm -v sshvolume:/data busybox ls /data

    <content of /remote on machine 1.2.3.4>
    ```

6.  Remove the volume `sshvolume`
    ```bash
    docker volume rm sshvolume
    
    sshvolume
    ```
To disable a plugin, use the `docker plugin disable` command. To completely
remove it, use the `docker plugin remove` command. For other available
commands and options, see the
[command line reference](../reference/commandline/index.md).

## Service creation using plugins

In swarm mode, it is possible to create a service that allows for attaching
to networks or mounting volumes. Swarm schedules services based on plugin availability
on a node. In this example, a volume plugin is installed on a swarm worker and a volume 
is created using the plugin. In the manager, a service is created with the relevant
mount options. It can be observed that the service is scheduled to run on the worker
node with the said volume plugin and volume. 

In the following example, node1 is the manager and node2 is the worker.

1.  Prepare manager. In node 1:

    ```bash
    $ docker swarm init
    Swarm initialized: current node (dxn1zf6l61qsb1josjja83ngz) is now a manager.
    ```

2. Join swarm, install plugin and create volume on worker. In node 2:

    ```bash
    $ docker swarm join \
    --token SWMTKN-1-49nj1cmql0jkz5s954yi3oex3nedyz0fb0xx14ie39trti4wxv-8vxv8rssmk743ojnwacrr2e7c \
    192.168.99.100:2377
    ```

    ```bash
    $ docker plugin install tiborvass/sample-volume-plugin
    latest: Pulling from tiborvass/sample-volume-plugin
    eb9c16fbdc53: Download complete
    Digest: sha256:00b42de88f3a3e0342e7b35fa62394b0a9ceb54d37f4c50be5d3167899994639
    Status: Downloaded newer image for tiborvass/sample-volume-plugin:latest
    Installed plugin tiborvass/sample-volume-plugin
    ```
	
    ```bash
    $ docker volume create -d tiborvass/sample-volume-plugin --name pluginVol
    ```

3. Create a service using the plugin and volume. In node1:

    ```bash
    $ docker service create --name my-service --mount type=volume,volume-driver=tiborvass/sample-volume-plugin,source=pluginVol,destination=/tmp busybox top

    $ docker service ls
    z1sj8bb8jnfn  my-service   replicated  1/1       busybox:latest 
    ```
    docker service ls shows service 1 instance of service running.

4. Observe the task getting scheduled in node 2:

    ```bash
    $ docker ps --format '{{.ID}}\t {{.Status}} {{.Names}} {{.Command}}' 
    83fc1e842599     Up 2 days my-service.1.9jn59qzn7nbc3m0zt1hij12xs "top"
    ```

## Developing a plugin

#### The rootfs directory
The `rootfs` directory represents the root filesystem of the plugin. In this
example, it was created from a Dockerfile:

>**Note:** The `/run/docker/plugins` directory is mandatory inside of the
plugin's filesystem for docker to communicate with the plugin.

```bash
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
with Docker Engine. This plugin has no runtime parameters.

#### Creating the plugin

A new plugin can be created by running
`docker plugin create <plugin-name> ./path/to/plugin/data` where the plugin
data contains a plugin configuration file `config.json` and a root filesystem
in subdirectory `rootfs`. 

After that the plugin `<plugin-name>` will show up in `docker plugin ls`.
Plugins can be pushed to remote registries with
`docker plugin push <plugin-name>`.

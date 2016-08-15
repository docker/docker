<!--[metadata]>
+++
title = "New Plugin System"
description = "How to operate and create a plugin with the new system"
keywords = ["API, Usage, plugins, documentation, developer"]
advisory = "experimental"
[menu.main]
parent = "engine_extend"
weight=1
+++
<![end-metadata]-->

# New Plugin System

The goal of this document is to describe the current state of the new plugin system available today in the **experimental build** of Docker 1.12.

The main difference, compared to legacy plugins, is that plugins are now managed by Docker: plugins are installed, started, stopped and removed by docker. 

Only volume drivers are currently supported but more types will be added in the next release.

This document is split in two parts, the user perspective, “how to operate a plugin” and the developer perspective “how to create a plugin”


## How to operate a plugin

Plugins are distributed as docker images, so they can be hosted on the Docker Hub or on a private registry.
Installing a plugin is very easy, it’s a simple command: `docker plugin install`
This command is going to pull the plugin from the Docker Hub / Private registry, ask the operator to accept privileges (for example, plugin requires access to a device on the host system), if necessary and enable it.
You can then check the status of the plugin with the docker plugin ls command, the plugin will be marked as ENABLED if it was started without issue.

Then, the plugin behavior is the same as legacy plugins, here is a full example using a sshfs plugin:

### install the plugin
```bash
$ docker plugin install vieux/sshfs
Plugin "vieux/sshfs" is requesting the following privileges:
 - network: [host]
 - capabilities: [CAP_SYS_ADMIN]
Do you grant the above permissions? [y/N] y
vieux/sshfs
```

Here the plugin requests 2 privileges, the `CAP_SYS_ADMIN` capability to be able to do mount inside the plugin and `host networking`.

### verify that the plugin has correctly started 
##### by looking at the ENABLED column. (The value should be true)

```bash
$ docker plugin ls
NAME                TAG                 ENABLED
vieux/sshfs         latest              true
```

### create a volume using the plugin installed above

```bash
$ docker volume create -d vieux/sshfs --name sshvolume -o sshcmd=user@1.2.3.4:/remote 
sshvolume
```

### use the volume created above

```bash
$ docker run -v sshvolume:/data busybox ls /data
<content of /remote on machine 1.2.3.4>
```

### verify that the plugin was created successfully

```bash
$ docker volume ls
DRIVER              NAME
vieux/sshfs         sshvolume
```

It’s also possible to stop a plugin with the `docker plugin disable` command and to remove a plugin with `docker plugin remove`.

See the [command line reference](../engine/reference/commandline/) for more information.

## How to create a plugin

The creation of plugin is currently a manual process, in the future release, a command such as `docker plugin build` will be added to automate the process. So here we are going to describe the format of an existing enabled plugin, to create a plugin you have to manually craft all those files by hand.

Plugins are stored in `/var/lib/docker/plugins`. See this example:

```bash
# ls -la /var/lib/docker/plugins
total 20
drwx------  4 root root 4096 Aug  8 18:03 .
drwx--x--x 12 root root 4096 Aug  8 17:53 ..
drwxr-xr-x  3 root root 4096 Aug  8 17:56 cd851ce43a403
-rw-------  1 root root 2107 Aug  8 18:03 plugins.json
```

The file `plugins.json` is an inventory of all installed plugins, see an example of the content:

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
        "ManifestVersion": "v0.1",
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

Each folder represents a plugin, for example:

```bash
# ls -la /var/lib/docker/plugins/cd851ce43a403
total 12
drwx------ 19 root root 4096 Aug  8 17:56 rootfs
-rw-r--r--  1 root root   50 Aug  8 17:56 plugin-config.json
-rw-------  1 root root  347 Aug  8 17:56 manifest.json
```

rootfs represents the root filesystem of the plugin, in this example, it was created from this Dockerfile as follows:

_Note: `/run/docker/plugins` is mandatory for docker to communicate with the plugin._

```bash
$ git clone github.com/vieux/docker-volume-sshfs
$ cd docker-volume-sshfs
$ docker build -t rootfs .
$ id=$(docker create rootfs true) # id was cd851ce43a403 when the image was created
$ mkdir -p /var/lib/docker/plugins/$id/rootfs
$ docker export "$id" | tar -x -C /var/lib/docker/plugins/$id/rootfs
$ docker rm -vf "$id"
$ docker rmi rootfs
```

`manifest.json` describe the plugin and `plugin-config.json` contains some runtime parameters, see for example:

```bash
# cat manifest.json
{
	"manifestVersion": "v0.1",
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

In this example, you can see the plugin is a volume driver, requires the `CAP_SYS_ADMIN` capability, `host networking`, `/go/bin/docker-volume-sshfs` as entrypoint and is going to use `/run/docker/plugins/sshfs.sock` to communicate with the docker engine.

```bash
# cat plugin-config.json
{
  "Devices": null,
  "Args": null,
  "Env": [],
  "Mounts": []
}
```

No runtime parameters are needed for this plugin.

Both `manifest.json` and `plugin-config.json` are part of the `plugins.json`.
`manifest.json` is read-only and `plugin-config.json` is read-write.



To sum up, here are the steps required to create a plugin today:

0. choose the name of the plugins, same format as images, for example `<repo_name>/<name>`
1. create a rootfs as showed above in `/var/lib/docker/plugins/$id/rootfs`
2. create manifest.json file in `/var/lib/docker/plugins/$id/` as shown above
3. create a `plugin-config.json` if needed, as shown above.
4. create or add a section to `/var/lib/docker/plugins/plugins.json` as shown above, use 
`<user>/<name>` as “Name” and `$id` as “Id”
5. restart docker
6. `docker plugin ls` 
  a. if your plugin is listed as `ENABLED=true`, go to 7.
  b. if the plugins is not listed or listed as `ENABLED=false` something went wrong, look at the daemon logs.
7. if not logged in already, use `docker login` to authenticate against a registry.
8. push the plugin with `docker plugin push <repo_name>/<name>`



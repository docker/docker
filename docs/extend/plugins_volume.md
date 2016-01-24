<!--[metadata]>
+++
title = "Volume plugins"
description = "How to manage data with external volume plugins"
keywords = ["Examples, Usage, volume, docker, data, volumes, plugin, api"]
[menu.main]
parent = "engine_extend"
+++
<![end-metadata]-->

# Write a volume plugin

Docker Engine volume plugins enable Engine deployments to be integrated with
external storage systems, such as Amazon EBS, and enable data volumes to persist
beyond the lifetime of a single Engine host. See the [plugin
documentation](plugins.md) for more information.

## Command-line changes

A volume plugin makes use of the `-v`and `--volume-driver` flag on the `docker run` command.  The `-v` flag accepts a volume name and the `--volume-driver` flag a driver type, for example:

    $ docker run -ti -v volumename:/data --volume-driver=flocker   busybox sh

This command passes the `volumename` through to the volume plugin as a
user-given name for the volume. The `volumename` must not begin with a `/`.

By having the user specify a  `volumename`, a plugin can associate the volume
with an external volume beyond the lifetime of a single container or container
host. This can be used, for example, to move a stateful container from one
server to another.

By specifying a `volumedriver` in conjunction with a `volumename`, users can use plugins such as [Flocker](https://clusterhq.com/docker-plugin/) to manage volumes external to a single host, such as those on EBS.


## Create a VolumeDriver

The container creation endpoint (`/containers/create`) accepts a `VolumeDriver`
field of type `string` allowing to specify the name of the driver. It's default
value of `"local"` (the default driver for local volumes).

## Volume plugin protocol

If a plugin registers itself as a `VolumeDriver` when activated, then it is
expected to provide writeable paths on the host filesystem for the Docker
daemon to provide to containers to consume.

The Docker daemon handles bind-mounting the provided paths into user
containers.

> **Note**: Volume plugins should *not* write data to the `/var/lib/docker/`
> directory, including `/var/lib/docker/volumes`. The `/var/lib/docker/`
> directory is reserved for Docker.

### /VolumeDriver.Create

**Request**:
```json
{
    "Name": "volume_name",
    "Opts": {}
}
```

Instruct the plugin that the user wants to create a volume, given a user
specified volume name.  The plugin does not need to actually manifest the
volume on the filesystem yet (until Mount is called).
Opts is a map of driver specific options passed through from the user request.

**Response**:
```json
{
    "Err": ""
}
```

Respond with a string error if an error occurred.

### /VolumeDriver.Remove

**Request**:
```json
{
    "Name": "volume_name"
}
```

Delete the specified volume from disk. This request is issued when a user invokes `docker rm -v` to remove volumes associated with a container.

**Response**:
```json
{
    "Err": ""
}
```

Respond with a string error if an error occurred.

### /VolumeDriver.Mount

**Request**:
```json
{
    "Name": "volume_name"
}
```

Docker requires the plugin to provide a volume, given a user specified volume
name. This is called once per container start. If the same volume_name is requested
more than once, the plugin may need to keep track of each new mount request and provision
at the first mount request and deprovision at the last corresponding unmount request.

**Response**:
```json
{
    "Mountpoint": "/path/to/directory/on/host",
    "Err": ""
}
```

Respond with the path on the host filesystem where the volume has been made
available, and/or a string error if an error occurred.

### /VolumeDriver.Path

**Request**:
```json
{
    "Name": "volume_name"
}
```

Docker needs reminding of the path to the volume on the host.

**Response**:
```json
{
    "Mountpoint": "/path/to/directory/on/host",
    "Err": ""
}
```

Respond with the path on the host filesystem where the volume has been made
available, and/or a string error if an error occurred.

### /VolumeDriver.Unmount

**Request**:
```json
{
    "Name": "volume_name"
}
```

Indication that Docker no longer is using the named volume. This is called once
per container stop.  Plugin may deduce that it is safe to deprovision it at
this point.

**Response**:
```json
{
    "Err": ""
}
```

Respond with a string error if an error occurred.


### /VolumeDriver.Get

**Request**:
```json
{
    "Name": "volume_name"
}
```

Get the volume info.


**Response**:
```json
{
  "Volume": {
    "Name": "volume_name",
    "Mountpoint": "/path/to/directory/on/host",
  },
  "Err": ""
}
```

Respond with a string error if an error occurred.


### /VolumeDriver.List

**Request**:
```json
{}
```

Get the list of volumes registered with the plugin.

**Response**:
```json
{
  "Volumes": [
    {
      "Name": "volume_name",
      "Mountpoint": "/path/to/directory/on/host"
    }
  ],
  "Err": ""
}
```

Respond with a string error if an error occurred.

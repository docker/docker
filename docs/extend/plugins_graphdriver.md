---
title: "Graphdriver plugins"
description: "How to manage image and container filesystems with external plugins"
keywords: "Examples, Usage, storage, image, docker, data, graph, plugin, api"
advisory: experimental
---

<!-- This file is maintained within the docker/docker Github
     repository at https://github.com/docker/docker/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->


## Changelog

### 1.13.0

- Support v2 plugins

# Docker graph driver plugins

Docker graph driver plugins enable admins to use an external/out-of-process
graph driver for use with Docker engine. This is an alternative to using the
built-in storage drivers, such as aufs/overlay/devicemapper/btrfs.

You need to install and enable the plugin and then restart the Docker daemon
before using the plugin. See the following example for the correct ordering
of steps.

```
$ docker plugin install cpuguy83/docker-overlay2-graphdriver-plugin # this command also enables the driver
<output suppressed>
$ pkill dockerd
$ dockerd --experimental -s cpuguy83/docker-overlay2-graphdriver-plugin
```

# Write a graph driver plugin

See the [plugin documentation](https://docs.docker.com/engine/extend/) for detailed information
on the underlying plugin protocol.


## Graph Driver plugin protocol

If a plugin registers itself as a `GraphDriver` when activated, then it is
expected to provide the rootfs for containers as well as image layer storage.

### /GraphDriver.Init

**Request**:
```json
{
  "Home": "/graph/home/path",
  "Opts": [],
  "UIDMaps": [],
  "GIDMaps": []
}
```

Initialize the graph driver plugin with a home directory and array of options.
These are passed through from the user, but the plugin is not required to parse
or honor them.

The request also includes a list of UID and GID mappings, structed as follows:
```json
{
  "ContainerID": 0,
  "HostID": 0,
  "Size": 0
}
```

**Response**:
```json
{
  "Err": ""
}
```

Respond with a non-empty string error if an error occurred.


### /GraphDriver.Create

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Parent": "2cd9c322cb78a55e8212aa3ea8425a4180236d7106938ec921d0935a4b8ca142",
  "MountLabel": "",
  "StorageOpt": {}
}
```

Create a new, empty, read-only filesystem layer with the specified
`ID`, `Parent` and `MountLabel`. If `Parent` is an empty string, there is no
parent layer. `StorageOpt` is map of strings which indicate storage options.

**Response**:
```json
{
  "Err": ""
}
```

Respond with a non-empty string error if an error occurred.

### /GraphDriver.CreateReadWrite

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Parent": "2cd9c322cb78a55e8212aa3ea8425a4180236d7106938ec921d0935a4b8ca142",
  "MountLabel": "",
  "StorageOpt": {}
}
```

Similar to `/GraphDriver.Create` but creates a read-write filesystem layer.

### /GraphDriver.Remove

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187"
}
```

Remove the filesystem layer with this given `ID`.

**Response**:
```json
{
  "Err": ""
}
```

Respond with a non-empty string error if an error occurred.

### /GraphDriver.Get

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "MountLabel": ""
}
```

Get the mountpoint for the layered filesystem referred to by the given `ID`.

**Response**:
```json
{
  "Dir": "/var/mygraph/46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Err": ""
}
```

Respond with the absolute path to the mounted layered filesystem.
Respond with a non-empty string error if an error occurred.

### /GraphDriver.Put

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187"
}
```

Release the system resources for the specified `ID`, such as unmounting the
filesystem layer.

**Response**:
```json
{
  "Err": ""
}
```

Respond with a non-empty string error if an error occurred.

### /GraphDriver.Exists

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187"
}
```

Determine if a filesystem layer with the specified `ID` exists.

**Response**:
```json
{
  "Exists": true
}
```

Respond with a boolean for whether or not the filesystem layer with the specified
`ID` exists.

### /GraphDriver.Status

**Request**:
```json
{}
```

Get low-level diagnostic information about the graph driver.

**Response**:
```json
{
  "Status": [[]]
}
```

Respond with a 2-D array with key/value pairs for the underlying status
information.


### /GraphDriver.GetMetadata

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187"
}
```

Get low-level diagnostic information about the layered filesystem with the
with the specified `ID`

**Response**:
```json
{
  "Metadata": {},
  "Err": ""
}
```

Respond with a set of key/value pairs containing the low-level diagnostic
information about the layered filesystem.
Respond with a non-empty string error if an error occurred.

### /GraphDriver.Cleanup

**Request**:
```json
{}
```

Perform necessary tasks to release resources help by the plugin, such as
unmounting all the layered file systems.

**Response**:
```json
{
  "Err": ""
}
```

Respond with a non-empty string error if an error occurred.


### /GraphDriver.Diff

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Parent": "2cd9c322cb78a55e8212aa3ea8425a4180236d7106938ec921d0935a4b8ca142"
}
```

Get an archive of the changes between the filesystem layers specified by the `ID`
and `Parent`. `Parent` may be an empty string, in which case there is no parent.

**Response**:
```
{{ TAR STREAM }}
```

### /GraphDriver.Changes

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Parent": "2cd9c322cb78a55e8212aa3ea8425a4180236d7106938ec921d0935a4b8ca142"
}
```

Get a list of changes between the filesystem layers specified by the `ID` and
`Parent`. If `Parent` is an empty string, there is no parent.

**Response**:
```json
{
  "Changes": [{}],
  "Err": ""
}
```

Respond with a list of changes. The structure of a change is:
```json
  "Path": "/some/path",
  "Kind": 0,
```

Where the `Path` is the filesystem path within the layered filesystem that is
changed and `Kind` is an integer specifying the type of change that occurred:

- 0 - Modified
- 1 - Added
- 2 - Deleted

Respond with a non-empty string error if an error occurred.

### /GraphDriver.ApplyDiff

**Request**:
```
{{ TAR STREAM }}
```

Extract the changeset from the given diff into the layer with the specified `ID`
and `Parent`

**Query Parameters**:

- id (required)- the `ID` of the new filesystem layer to extract the diff to
- parent (required)- the `Parent` of the given `ID`

**Response**:
```json
{
  "Size": 512366,
  "Err": ""
}
```

Respond with the size of the new layer in bytes.
Respond with a non-empty string error if an error occurred.

### /GraphDriver.DiffSize

**Request**:
```json
{
  "ID": "46fe8644f2572fd1e505364f7581e0c9dbc7f14640bd1fb6ce97714fb6fc5187",
  "Parent": "2cd9c322cb78a55e8212aa3ea8425a4180236d7106938ec921d0935a4b8ca142"
}
```

Calculate the changes between the specified `ID`

**Response**:
```json
{
  "Size": 512366,
  "Err": ""
}
```

Respond with the size changes between the specified `ID` and `Parent`
Respond with a non-empty string error if an error occurred.

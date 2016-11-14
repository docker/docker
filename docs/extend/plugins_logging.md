---
title: "Docker log driver plugins"
description: "Log driver plugins."
keywords: "Examples, Usage, plugins, docker, documentation, user guide, logging"
---

<!-- This file is maintained within the docker/docker Github
     repository at https://github.com/docker/docker/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# Logging driver plugins

This document describes logging driver plugins for Docker.

Logging drivers enables users to forward container logs to another service for
processing. Docker includes several logging drivers as built-ins, however can
never hope to support all use-cases with built-in drivers. Plugins allow Docker
to support a wide range of logging services without requiring to embed client
libraries for these services in the main Docker codebase. See the
[plugin documentation](legacy_plugins.md) for more information.

## Create a logging plugin

The main interface for logging plugins uses the same JSON+HTTP RPC protocol used
by other plugin types. See the
[example](https://github.com/cpuguy83/docker-log-driver-test) plugin for a
reference implementation of a logging plugin. The example wraps the built-in
`jsonfilelog` log driver.

## LogDriver protocol

Logging plugins must register as a `LogDriver` during plugin activation. Once
activated users can specify the plugin as a log driver.

There are two HTTP endpoints that logging plugins must implement:

### `/LogDriver.StartLogging`

Signals to the plugin that a container is starting that the plugin should start
receiving logs for.

Logs will be streamed over the defined file in the request. On Linux this file
is a FIFO. Logging plugins are not currently supported on Windows.

**Request**:
```json
{
		"File": "/path/to/file/stream",
		"Info": {
			"ContainerID": "123456"
		}
}
```

`File` is the path to the log stream that needs to be consumed. Each call to
`StartLogging` should provide a different file path, even if it's a container
that the plugin has already received logs for prior. The file is created by
docker with a randomly generated name.

`Info` is details about the container that's being logged. This is fairly
free-form, but is defined by the following struct definition:

```go
type Info struct {
	Config              map[string]string
	ContainerID         string
	ContainerName       string
	ContainerEntrypoint string
	ContainerArgs       []string
	ContainerImageID    string
	ContainerImageName  string
	ContainerCreated    time.Time
	ContainerEnv        []string
	ContainerLabels     map[string]string
	LogPath             string
	DaemonName          string
}
```


`ContainerID` will always be supplied with this struct, but other fields may be
empty or missing.

**Response**
```json
{
	"Err": ""
}
```

If an error occurred during this request, add an error message to the `Err` field
in the response. If no error then you can either send an empty response (`{}`)
or an empty value for the `Err` field.

The driver should at this point be consuming log messages from the passed in file.
If messages are unconsumed, it may cause the contaier to block while trying to
write to its stdio streams.

Log stream messages are encoded as protocol buffers. The protobuf definitions are
in the
[docker repository](https://github.com/docker/docker/blob/master/api/types/plugins/logdriver/entry.proto).

Since protocol buffers are not self-delimited you must decode them from the stream
using the following stream format:

```
[size][message]
```

Where `size` is a 4-byte big endian binary encoded uint32. `size` in this case
defines the size of the next message. `message` is the actual log entry.

A reference golang implementation of a stream encoder/decoder can be found
[here](https://github.com/docker/docker/blob/master/api/types/plugins/logdriver/io.go)

### `/LogDriver.StopLogging`

Signals to the plugin to stop collecting logs from the defined file.
Once a response is received, the file will be removed by Docker. You must make
sure to collect all logs on the stream before responding to this request or risk
losing log data.

Requests on this endpoint does not mean that the container has been removed
only that it has stopped.

**Request**:
```json
{
		"File": "/path/to/file/stream"
}
```

**Response**:
```json
{
	"Err": ""
}
```

If an error occurred during this request, add an error message to the `Err` field
in the response. If no error then you can either send an empty response (`{}`)
or an empty value for the `Err` field.

## Optional endpoints

Logging plugins can implement two extra logging endpoints:

### `/LogDriver.Capabilities`

Defines the capabilities of the log driver. You must implement this endpoint for
Docker to be able to take advantage of any of the defined capabilities.

**Request**:
```json
{}
```

**Response**:
```json
{
	"ReadLogs": true
}
```

Supported capabilities:

- `ReadLogs` - this tells Docker that the plugin is capable of reading back logs
to clients. Plugins that report that they support `ReadLogs` must implement the
`/LogDriver.ReadLogs` endpoint

### `/LogDriver.ReadLogs`

Reads back logs to the client. This is used when `docker logs <container>` is
called.

In order for Docker to use this endpoint, the plugin must specify as much when
`/LogDriver.Capabilities` is called.


**Request**:
```json
{
	"ReadConfig": {},
	"Info": {
		"ContainerID": "123456"
	}
}
```

`ReadConfig` is the list of options for reading, it is defined with the following
golang struct:

```go
type ReadConfig struct {
	Since  time.Time
	Tail   int
	Follow bool
}
```

- `Since` defines the oldest log that should be sent.
- `Tail` defines the number of lines to read (e.g. like the command `tail -n 10`)
- `Follow` signals that the client wants to stay attached to receive new log messages
as they come in once the existing logs have been read.

`Info` is the same type defined in `/LogDriver.StartLogging`. It should be used
to determine what set of logs to read.

**Response**:
```
{{ log stream }}
```

The response should be the encoded log message using the same format as the
messages that the plugin consumed from Docker.

# Runtime v2

Runtime v2 introduces a first class shim API for runtime authors to integrate with containerd.
The shim API is minimal and scoped to the execution lifecycle of a container.

## Binary Naming

Users specify the runtime they wish to use when creating a container.
The runtime can also be changed via a container update.

```bash
> ctr run --runtime io.containerd.runc.v1
```

When a user specifies a runtime name, `io.containerd.runc.v1`, they will specify the name and version of the runtime.
This will be translated by containerd into a binary name for the shim.

`io.containerd.runc.v1` -> `containerd-shim-runc-v1`

containerd keeps the `containerd-shim-*` prefix so that users can `ps aux | grep containerd-shim` to see running shims on their system.

## Shim Authoring

This section is dedicated to runtime authors wishing to build a shim.
It will detail how the API works and different considerations when building shim.

### Commands

Container information is provided to a shim in two ways.
The OCI Runtime Bundle and on the `Create` rpc request.

#### `start`

Each shim MUST implement a `start` subcommand.
This command will launch new shims.
The start command MUST accept the following flags:

* `-namespace` the namespace for the container
* `-address` the address of the containerd's main socket
* `-publish-binary` the binary path to publish events back to containerd
* `-id` the id of the container

The start command, as well as all binary calls to the shim, has the bundle for the container set as the `cwd`.

The start command MUST return an address to a shim for containerd to issue API requests for container operations.

The start command can either start a new shim or return an address to an existing shim based on the shim's logic.

#### `delete`

Each shim MUST implement a `delete` subcommand.
This command allows containerd to delete any container resources created, mounted, and/or run by a shim when containerd can no longer communicate over rpc.
This happens if a shim is SIGKILL'd with a running container.
These resources will need to be cleaned up when containerd looses the connection to a shim.
This is also used when containerd boots and reconnects to shims.
If a bundle is still on disk but containerd cannot connect to a shim, the delete command is invoked.

The delete command MUST accept the following flags:

* `-namespace` the namespace for the container
* `-address` the address of the containerd's main socket
* `-publish-binary` the binary path to publish events back to containerd
* `-id` the id of the container
* `-bundle` the path to the bundle to delete. On non-Windows platforms this will match `cwd`

The delete command will be executed in the container's bundle as its `cwd` except for on the Windows platform.

### Host Level Shim Configuration

containerd does not provide any host level configuration for shims via the API.
If a shim needs configuration from the user with host level information across all instances, a shim specific configuration file can be setup.

### Container Level Shim Configuration

On the create request, there is a generic `*protobuf.Any` that allows a user to specify container level configuration for the shim.

```proto
message CreateTaskRequest {
	string id = 1;
	...
	google.protobuf.Any options = 10;
}
```

A shim author can create their own protobuf message for configuration and clients can import and provide this information is needed.

### I/O

I/O for a container is provided by the client to the shim via fifo on Linux, named pipes on Windows, or log files on disk.
The paths to these files are provided on the `Create` rpc for the initial creation and on the `Exec` rpc for additional processes.

```proto
message CreateTaskRequest {
	string id = 1;
	bool terminal = 4;
	string stdin = 5;
	string stdout = 6;
	string stderr = 7;
}
```

```proto
message ExecProcessRequest {
	string id = 1;
	string exec_id = 2;
	bool terminal = 3;
	string stdin = 4;
	string stdout = 5;
	string stderr = 6;
}
```

Containers that are to be launched with an interactive terminal will have the `terminal` field set to `true`, data is still copied over the files(fifos,pipes) in the same way as non interactive containers.

### Root Filesystems

The root filesystem for the containers is provided by on the `Create` rpc.
Shims are responsible for managing the lifecycle of the filesystem mount during the lifecycle of a container.

```proto
message CreateTaskRequest {
	string id = 1;
	string bundle = 2;
	repeated containerd.types.Mount rootfs = 3;
	...
}
```

The mount protobuf message is:

```proto
message Mount {
	// Type defines the nature of the mount.
	string type = 1;
	// Source specifies the name of the mount. Depending on mount type, this
	// may be a volume name or a host path, or even ignored.
	string source = 2;
	// Target path in container
	string target = 3;
	// Options specifies zero or more fstab style mount options.
	repeated string options = 4;
}
```

Shims are responsible for mounting the filesystem into the `rootfs/` directory of the bundle.
Shims are also responsible for unmounting of the filesystem.
During a `delete` binary call, the shim MUST ensure that filesystem is also unmounted.
Filesystems are provided by the containerd snapshotters.

### Events

The Runtime v2 supports an async event model. In order for the an upstream caller (such as Docker) to get these events in the correct order a Runtime v2 shim MUST implement the following events where `Compliance=MUST`. This avoids race conditions between the shim and shim client where for example a call to `Start` can signal a `TaskExitEventTopic` before even returning the results from the `Start` call. With these guarantees of a Runtime v2 shim a call to `Start` is required to have published the async event `TaskStartEventTopic` before the shim can publish the `TaskExitEventTopic`.

#### Tasks

| Topic | Compliance | Description |
| ----- | ---------- | ----------- |
| `runtime.TaskCreateEventTopic`       | MUST                                                                          | When a task is successfully created |
| `runtime.TaskStartEventTopic`        | MUST (follow `TaskCreateEventTopic`)                                          | When a task is successfully started |
| `runtime.TaskExitEventTopic`         | MUST (follow `TaskStartEventTopic`)                                           | When a task exits expected or unexpected |
| `runtime.TaskDeleteEventTopic`       | MUST (follow `TaskExitEventTopic` or `TaskCreateEventTopic` if never started) | When a task is removed from a shim |
| `runtime.TaskPausedEventTopic`       | SHOULD                                                                        | When a task is successfully paused |
| `runtime.TaskResumedEventTopic`      | SHOULD (follow `TaskPausedEventTopic`)                                        | When a task is successfully resumed |
| `runtime.TaskCheckpointedEventTopic` | SHOULD                                                                        | When a task is checkpointed |
| `runtime.TaskOOMEventTopic`          | SHOULD                                                                        | If the shim collects Out of Memory events |

#### Execs

| Topic | Compliance | Description |
| ----- | ---------- | ----------- |
| `runtime.TaskExecAddedEventTopic`   | MUST (follow `TaskCreateEventTopic` )     | When an exec is successfully added |
| `runtime.TaskExecStartedEventTopic` | MUST (follow `TaskExecAddedEventTopic`)   | When an exec is successfully started |
| `runtime.TaskExitEventTopic`        | MUST (follow `TaskExecStartedEventTopic`) | When an exec (other than the init exec) exits expected or unexpected |
| `runtime.TaskDeleteEventTopic`      | SHOULD (follow `TaskExitEventTopic` or `TaskExecAddedEventTopic` if never started) | When an exec is removed from a shim |

#### Logging

Shims may support pluggable logging via STDIO URIs.
Current supported schemes for logging are:

* fifo - Linux
* binary - Linux & Windows
* file - Linux & Windows
* npipe - Windows

Binary logging has the abilty to forward a container's STDIO to an external binary for consumption.
A sample logging driver that forwards the container's STDOUT and STDERR to `journald` is:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/runtime/v2/logging"
	"github.com/coreos/go-systemd/journal"
)

func main() {
	logging.Run(log)
}

func log(ctx context.Context, config *logging.Config, ready func() error) error {
	// construct any log metadata for the container
	vars := map[string]string{
		"SYSLOG_IDENTIFIER": fmt.Sprintf("%s:%s", config.Namespace, config.ID),
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// forward both stdout and stderr to the journal
	go copy(&wg, config.Stdout, journal.PriInfo, vars)
	go copy(&wg, config.Stderr, journal.PriErr, vars)

	// signal that we are ready and setup for the container to be started
	if err := ready(); err != nil {
		return err
	}
	wg.Wait()
	return nil
}

func copy(wg *sync.WaitGroup, r io.Reader, pri journal.Priority, vars map[string]string) {
	defer wg.Done()
	s := bufio.NewScanner(r)
	for s.Scan() {
		if s.Err() != nil {
			return
		}
		journal.Send(s.Text(), pri, vars)
	}
}
```

### Other

#### Unsupported rpcs

If a shim does not or cannot implement an rpc call, it MUST return a `github.com/containerd/containerd/errdefs.ErrNotImplemented` error.

#### Debugging and Shim Logs

A fifo on unix or named pipe on Windows will be provided to the shim.
It can be located inside the `cwd` of the shim named "log".
The shims can use the existing `github.com/containerd/containerd/log` package to log debug messages.
Messages will automatically be output in the containerd's daemon logs with the correct fields and runtime set.

#### ttrpc

[ttrpc](https://github.com/containerd/ttrpc) is the only currently supported protocol for shims.
It works with standard protobufs and GRPC services as well as generating clients.
The only difference between grpc and ttrpc is the wire protocol.
ttrpc removes the http stack in order to save memory and binary size to keep shims small.
It is recommended to use ttrpc in your shim but grpc support is also in development.

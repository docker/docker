---
title: "dockerd"
description: "The daemon command description and usage"
keywords: "container, daemon, runtime"
redirect_from:
- /engine/reference/commandline/daemon/
---

<!-- This file is maintained within the docker/cli GitHub
     repository at https://github.com/docker/cli/. Make all
     pull requests against that repo. If you see this file in
     another repository, consider it read-only there, as it will
     periodically be overwritten by the definitive file. Pull
     requests which include edits to this file in other repositories
     will be rejected.
-->

# daemon

```markdown
Usage: dockerd COMMAND

A self-sufficient runtime for containers.

Options:
      --add-runtime runtime                   Register an additional OCI compatible runtime (default [])
      --allow-nondistributable-artifacts list Allow push of nondistributable artifacts to registry
      --api-cors-header string                Set CORS headers in the Engine API
      --authorization-plugin list             Authorization plugins to load
      --bip string                            Specify network bridge IP
  -b, --bridge string                         Attach containers to a network bridge
      --cgroup-parent string                  Set parent cgroup for all containers
      --config-file string                    Daemon configuration file (default "/etc/docker/daemon.json")
      --containerd string                     containerd grpc address
      --containerd-namespace string           Containerd namespace to use (default "moby")
      --containerd-plugins-namespace string   Containerd namespace to use for plugins (default "plugins.moby")
      --cpu-rt-period int                     Limit the CPU real-time period in microseconds for the
                                              parent cgroup for all containers
      --cpu-rt-runtime int                    Limit the CPU real-time runtime in microseconds for the
                                              parent cgroup for all containers
      --cri-containerd                        start containerd with cri
      --data-root string                      Root directory of persistent Docker state (default "/var/lib/docker")
  -D, --debug                                 Enable debug mode
      --default-address-pool pool-options     Default address pools for node specific local networks
      --default-cgroupns-mode string          Default mode for containers cgroup namespace ("host" | "private") (default "host")
      --default-gateway ip                    Container default gateway IPv4 address
      --default-gateway-v6 ip                 Container default gateway IPv6 address
      --default-ipc-mode string               Default mode for containers ipc ("shareable" | "private") (default "private")
      --default-runtime string                Default OCI runtime for containers (default "runc")
      --default-shm-size bytes                Default shm size for containers (default 64MiB)
      --default-ulimit ulimit                 Default ulimits for containers (default [])
      --dns list                              DNS server to use
      --dns-opt list                          DNS options to use
      --dns-search list                       DNS search domains to use
      --exec-opt list                         Runtime execution options
      --exec-root string                      Root directory for execution state files (default "/var/run/docker")
      --experimental                          Enable experimental features
      --fixed-cidr string                     IPv4 subnet for fixed IPs
      --fixed-cidr-v6 string                  IPv6 subnet for fixed IPs
  -G, --group string                          Group for the unix socket (default "docker")
      --help                                  Print usage
  -H, --host list                             Daemon socket(s) to connect to
      --host-gateway-ip ip                    IP address that the special 'host-gateway' string in --add-host resolves to.
                                              Defaults to the IP address of the default bridge
      --icc                                   Enable inter-container communication (default true)
      --init                                  Run an init in the container to forward signals and reap processes
      --init-path string                      Path to the docker-init binary
      --insecure-registry list                Enable insecure registry communication
      --ip ip                                 Default IP when binding container ports (default 0.0.0.0)
      --ip-forward                            Enable net.ipv4.ip_forward (default true)
      --ip-masq                               Enable IP masquerading (default true)
      --iptables                              Enable addition of iptables rules (default true)
      --ip6tables                             Enable addition of ip6tables rules (default false)
      --ipv6                                  Enable IPv6 networking
      --label list                            Set key=value labels to the daemon
      --live-restore                          Enable live restore of docker when containers are still running
      --log-driver string                     Default driver for container logs (default "json-file")
  -l, --log-level string                      Set the logging level ("debug"|"info"|"warn"|"error"|"fatal") (default "info")
      --log-opt map                           Default log driver options for containers (default map[])
      --max-concurrent-downloads int          Set the max concurrent downloads for each pull (default 3)
      --max-concurrent-uploads int            Set the max concurrent uploads for each push (default 5)
      --max-download-attempts int             Set the max download attempts for each pull (default 5)
      --metrics-addr string                   Set default address and port to serve the metrics api on
      --mtu int                               Set the containers network MTU
      --network-control-plane-mtu int         Network Control plane MTU (default 1500)
      --no-new-privileges                     Set no-new-privileges by default for new containers
      --node-generic-resource list            Advertise user-defined resource
      --oom-score-adjust int                  Set the oom_score_adj for the daemon (default -500)
  -p, --pidfile string                        Path to use for daemon PID file (default "/var/run/docker.pid")
      --raw-logs                              Full timestamps without ANSI coloring
      --registry-mirror list                  Preferred registry mirror
      --rootless                              Enable rootless mode; typically used with RootlessKit
      --seccomp-profile string                Path to seccomp profile
      --selinux-enabled                       Enable selinux support
      --shutdown-timeout int                  Set the default shutdown timeout (default 15)
  -s, --storage-driver string                 Storage driver to use
      --storage-opt list                      Storage driver options
      --swarm-default-advertise-addr string   Set default address or interface for swarm advertised address
      --tls                                   Use TLS; implied by --tlsverify
      --tlscacert string                      Trust certs signed only by this CA (default "~/.docker/ca.pem")
      --tlscert string                        Path to TLS certificate file (default "~/.docker/cert.pem")
      --tlskey string                         Path to TLS key file (default "~/.docker/key.pem")
      --tlsverify                             Use TLS and verify the remote
      --userland-proxy                        Use userland proxy for loopback traffic (default true)
      --userland-proxy-path string            Path to the userland proxy binary
      --userns-remap string                   User/Group setting for user namespaces
      --validate                              Validate daemon configuration and exit
  -v, --version                               Print version information and quit
```

Options with [] may be specified multiple times.

## Description

`dockerd` is the persistent process that manages containers. Docker
uses different binaries for the daemon and client. To run the daemon you
type `dockerd`.

To run the daemon with debug output, use `dockerd --debug` or add `"debug": true`
to [the `daemon.json` file](#daemon-configuration-file).

> **Enabling experimental features**
> 
> Enable experimental features by starting `dockerd` with the `--experimental`
> flag or adding `"experimental": true` to the `daemon.json` file.

### Environment variables

For easy reference, the following list of environment variables are supported
by the `dockerd` command line:

* `DOCKER_DRIVER` The graph driver to use.
* `DOCKER_NOWARN_KERNEL_VERSION` Prevent warnings that your Linux kernel is
  unsuitable for Docker.
* `DOCKER_RAMDISK` If set this will disable 'pivot_root'.
* `DOCKER_TMPDIR` Location for temporary Docker files.
* `MOBY_DISABLE_PIGZ` Do not use [`unpigz`](https://linux.die.net/man/1/pigz) to
  decompress layers in parallel when pulling images, even if it is installed.

## Examples

### Daemon socket option

The Docker daemon can listen for [Docker Engine API](https://docs.docker.com/engine/api/)
requests via three different types of Socket: `unix`, `tcp`, and `fd`.

By default, a `unix` domain socket (or IPC socket) is created at
`/var/run/docker.sock`, requiring either `root` permission, or `docker` group
membership.

If you need to access the Docker daemon remotely, you need to enable the `tcp`
Socket. Beware that the default setup provides un-encrypted and
un-authenticated direct access to the Docker daemon - and should be secured
either using the [built in HTTPS encrypted socket](https://docs.docker.com/engine/security/https/), or by
putting a secure web proxy in front of it. You can listen on port `2375` on all
network interfaces with `-H tcp://0.0.0.0:2375`, or on a particular network
interface using its IP address: `-H tcp://192.168.59.103:2375`. It is
conventional to use port `2375` for un-encrypted, and port `2376` for encrypted
communication with the daemon.

> **Note**
>
> If you're using an HTTPS encrypted socket, keep in mind that only
> TLS1.0 and greater are supported. Protocols SSLv3 and under are not
> supported anymore for security reasons.

On Systemd based systems, you can communicate with the daemon via
[Systemd socket activation](https://0pointer.de/blog/projects/socket-activation.html),
use `dockerd -H fd://`. Using `fd://` will work perfectly for most setups but
you can also specify individual sockets: `dockerd -H fd://3`. If the
specified socket activated files aren't found, then Docker will exit. You can
find examples of using Systemd socket activation with Docker and Systemd in the
[Docker source tree](https://github.com/docker/docker/tree/master/contrib/init/systemd/).

You can configure the Docker daemon to listen to multiple sockets at the same
time using multiple `-H` options:

The example below runs the daemon listenin on the default unix socket, and
on 2 specific IP addresses on this host:

```console
$ sudo dockerd -H unix:///var/run/docker.sock -H tcp://192.168.59.106 -H tcp://10.10.10.2
```

The Docker client will honor the `DOCKER_HOST` environment variable to set the
`-H` flag for the client. Use **one** of the following commands:

```console
$ docker -H tcp://0.0.0.0:2375 ps
```

```console
$ export DOCKER_HOST="tcp://0.0.0.0:2375"

$ docker ps
```

Setting the `DOCKER_TLS_VERIFY` environment variable to any value other than
the empty string is equivalent to setting the `--tlsverify` flag. The following
are equivalent:

```console
$ docker --tlsverify ps
# or
$ export DOCKER_TLS_VERIFY=1
$ docker ps
```

The Docker client will honor the `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`
environment variables (or the lowercase versions thereof). `HTTPS_PROXY` takes
precedence over `HTTP_PROXY`.

The Docker client supports connecting to a remote daemon via SSH:

```console
$ docker -H ssh://me@example.com:22 ps
$ docker -H ssh://me@example.com ps
$ docker -H ssh://example.com ps
```

To use SSH connection, you need to set up `ssh` so that it can reach the
remote host with public key authentication. Password authentication is not
supported. If your key is protected with passphrase, you need to set up
`ssh-agent`.

#### Bind Docker to another host/port or a Unix socket

> **Warning**
>
> Changing the default `docker` daemon binding to a
> TCP port or Unix *docker* user group will increase your security risks
> by allowing non-root users to gain *root* access on the host. Make sure
> you control access to `docker`. If you are binding
> to a TCP port, anyone with access to that port has full Docker access;
> so it is not advisable on an open network.
{: .warning :}

With `-H` it is possible to make the Docker daemon to listen on a
specific IP and port. By default, it will listen on
`unix:///var/run/docker.sock` to allow only local connections by the
*root* user. You *could* set it to `0.0.0.0:2375` or a specific host IP
to give access to everybody, but that is **not recommended** because
then it is trivial for someone to gain root access to the host where the
daemon is running.

Similarly, the Docker client can use `-H` to connect to a custom port.
The Docker client will default to connecting to `unix:///var/run/docker.sock`
on Linux, and `tcp://127.0.0.1:2376` on Windows.

`-H` accepts host and port assignment in the following format:

    tcp://[host]:[port][path] or unix://path

For example:

-   `tcp://` -> TCP connection to `127.0.0.1` on either port `2376` when TLS encryption
    is on, or port `2375` when communication is in plain text.
-   `tcp://host:2375` -> TCP connection on
    host:2375
-   `tcp://host:2375/path` -> TCP connection on
    host:2375 and prepend path to all requests
-   `unix://path/to/socket` -> Unix socket located
    at `path/to/socket`

`-H`, when empty, will default to the same value as
when no `-H` was passed in.

`-H` also accepts short form for TCP bindings: `host:` or `host:port` or `:port`

Run Docker in daemon mode:

```console
$ sudo <path to>/dockerd -H 0.0.0.0:5555 &
```

Download an `ubuntu` image:

```console
$ docker -H :5555 pull ubuntu
```

You can use multiple `-H`, for example, if you want to listen on both
TCP and a Unix socket

```console
$ sudo dockerd -H tcp://127.0.0.1:2375 -H unix:///var/run/docker.sock &
# Download an ubuntu image, use default Unix socket
$ docker pull ubuntu
# OR use the TCP port
$ docker -H tcp://127.0.0.1:2375 pull ubuntu
```

### Daemon storage-driver

On Linux, the Docker daemon has support for several different image layer storage
drivers: `aufs`, `devicemapper`, `btrfs`, `zfs`, `overlay`, `overlay2`, and `fuse-overlayfs`.

The `aufs` driver is the oldest, but is based on a Linux kernel patch-set that
is unlikely to be merged into the main kernel. These are also known to cause
some serious kernel crashes. However `aufs` allows containers to share
executable and shared library memory, so is a useful choice when running
thousands of containers with the same program or libraries.

The `devicemapper` driver uses thin provisioning and Copy on Write (CoW)
snapshots. For each devicemapper graph location – typically
`/var/lib/docker/devicemapper` – a thin pool is created based on two block
devices, one for data and one for metadata. By default, these block devices
are created automatically by using loopback mounts of automatically created
sparse files. Refer to [Devicemapper options](#devicemapper-options) below
for a way how to customize this setup.
[~jpetazzo/Resizing Docker containers with the Device Mapper plugin](https://jpetazzo.github.io/2014/01/29/docker-device-mapper-resize/)
article explains how to tune your existing setup without the use of options.

The `btrfs` driver is very fast for `docker build` - but like `devicemapper`
does not share executable memory between devices. Use
`dockerd --storage-driver btrfs --data-root /mnt/btrfs_partition`.

The `zfs` driver is probably not as fast as `btrfs` but has a longer track record
on stability. Thanks to `Single Copy ARC` shared blocks between clones will be
cached only once. Use `dockerd -s zfs`. To select a different zfs filesystem
set `zfs.fsname` option as described in [ZFS options](#zfs-options).

The `overlay` is a very fast union filesystem. It is now merged in the main
Linux kernel as of [3.18.0](https://lkml.org/lkml/2014/10/26/137). `overlay`
also supports page cache sharing, this means multiple containers accessing
the same file can share a single page cache entry (or entries), it makes
`overlay` as efficient with memory as `aufs` driver. Call `dockerd -s overlay`
to use it.

The `overlay2` uses the same fast union filesystem but takes advantage of
[additional features](https://lkml.org/lkml/2015/2/11/106) added in Linux
kernel 4.0 to avoid excessive inode consumption. Call `dockerd -s overlay2`
to use it.

> **Note**
>
> The `overlay` storage driver can cause excessive inode consumption (especially
> as the number of images grows). We recommend using the `overlay2` storage
> driver instead.


> **Note**
>
> Both `overlay` and `overlay2` are currently unsupported on `btrfs`
> or any Copy on Write filesystem and should only be used over `ext4` partitions.

The `fuse-overlayfs` driver is similar to `overlay2` but works in userspace.
The `fuse-overlayfs` driver is expected to be used for [Rootless mode](https://docs.docker.com/engine/security/rootless/).

On Windows, the Docker daemon only supports the `windowsfilter` storage driver.

### Options per storage driver

Particular storage-driver can be configured with options specified with
`--storage-opt` flags. Options for `devicemapper` are prefixed with `dm`,
options for `zfs` start with `zfs`, and options for `btrfs` start with `btrfs`.

#### Devicemapper options

This is an example of the configuration file for devicemapper on Linux:

```json
{
  "storage-driver": "devicemapper",
  "storage-opts": [
    "dm.thinpooldev=/dev/mapper/thin-pool",
    "dm.use_deferred_deletion=true",
    "dm.use_deferred_removal=true"
  ]
}
```

##### `dm.thinpooldev`

Specifies a custom block storage device to use for the thin pool.

If using a block device for device mapper storage, it is best to use `lvm`
to create and manage the thin-pool volume. This volume is then handed to Docker
to exclusively create snapshot volumes needed for images and containers.

Managing the thin-pool outside of Engine makes for the most feature-rich
method of having Docker utilize device mapper thin provisioning as the
backing storage for Docker containers. The highlights of the lvm-based
thin-pool management feature include: automatic or interactive thin-pool
resize support, dynamically changing thin-pool features, automatic thinp
metadata checking when lvm activates the thin-pool, etc.

As a fallback if no thin pool is provided, loopback files are
created. Loopback is very slow, but can be used without any
pre-configuration of storage. It is strongly recommended that you do
not use loopback in production. Ensure your Engine daemon has a
`--storage-opt dm.thinpooldev` argument provided.

###### Example:

```console
$ sudo dockerd --storage-opt dm.thinpooldev=/dev/mapper/thin-pool
```

##### `dm.directlvm_device`

As an alternative to providing a thin pool as above, Docker can setup a block
device for you.

###### Example:

```console
$ sudo dockerd --storage-opt dm.directlvm_device=/dev/xvdf
```

##### `dm.thinp_percent`

Sets the percentage of passed in block device to use for storage.

###### Example:

```console
$ sudo dockerd --storage-opt dm.thinp_percent=95
```

##### `dm.thinp_metapercent`

Sets the percentage of the passed in block device to use for metadata storage.

###### Example:

```console
$ sudo dockerd --storage-opt dm.thinp_metapercent=1
```

##### `dm.thinp_autoextend_threshold`

Sets the value of the percentage of space used before `lvm` attempts to
autoextend the available space [100 = disabled]

###### Example:

```console
$ sudo dockerd --storage-opt dm.thinp_autoextend_threshold=80
```

##### `dm.thinp_autoextend_percent`

Sets the value percentage value to increase the thin pool by when `lvm`
attempts to autoextend the available space [100 = disabled]

###### Example:

```console
$ sudo dockerd --storage-opt dm.thinp_autoextend_percent=20
```


##### `dm.basesize`

Specifies the size to use when creating the base device, which limits the
size of images and containers. The default value is 10G. Note, thin devices
are inherently "sparse", so a 10G device which is mostly empty doesn't use
10 GB of space on the pool. However, the filesystem will use more space for
the empty case the larger the device is.

The base device size can be increased at daemon restart which will allow
all future images and containers (based on those new images) to be of the
new base device size.

###### Examples

```console
$ sudo dockerd --storage-opt dm.basesize=50G
```

This will increase the base device size to 50G. The Docker daemon will throw an
error if existing base device size is larger than 50G. A user can use
this option to expand the base device size however shrinking is not permitted.

This value affects the system-wide "base" empty filesystem
that may already be initialized and inherited by pulled images. Typically,
a change to this value requires additional steps to take effect:

```console
$ sudo service docker stop

$ sudo rm -rf /var/lib/docker

$ sudo service docker start
```


##### `dm.loopdatasize`

> **Note**
>
> This option configures devicemapper loopback, which should not
> be used in production.

Specifies the size to use when creating the loopback file for the
"data" device which is used for the thin pool. The default size is
100G. The file is sparse, so it will not initially take up this
much space.

###### Example

```console
$ sudo dockerd --storage-opt dm.loopdatasize=200G
```

##### `dm.loopmetadatasize`

> **Note**
>
> This option configures devicemapper loopback, which should not
> be used in production.

Specifies the size to use when creating the loopback file for the
"metadata" device which is used for the thin pool. The default size
is 2G. The file is sparse, so it will not initially take up
this much space.

###### Example

```console
$ sudo dockerd --storage-opt dm.loopmetadatasize=4G
```

##### `dm.fs`

Specifies the filesystem type to use for the base device. The supported
options are "ext4" and "xfs". The default is "xfs"

###### Example

```console
$ sudo dockerd --storage-opt dm.fs=ext4
```

##### `dm.mkfsarg`

Specifies extra mkfs arguments to be used when creating the base device.

###### Example

```console
$ sudo dockerd --storage-opt "dm.mkfsarg=-O ^has_journal"
```

##### `dm.mountopt`

Specifies extra mount options used when mounting the thin devices.

###### Example

```console
$ sudo dockerd --storage-opt dm.mountopt=nodiscard
```

##### `dm.datadev`

(Deprecated, use `dm.thinpooldev`)

Specifies a custom blockdevice to use for data for the thin pool.

If using a block device for device mapper storage, ideally both `datadev` and
`metadatadev` should be specified to completely avoid using the loopback
device.

###### Example

```console
$ sudo dockerd \
      --storage-opt dm.datadev=/dev/sdb1 \
      --storage-opt dm.metadatadev=/dev/sdc1
```

##### `dm.metadatadev`

(Deprecated, use `dm.thinpooldev`)

Specifies a custom blockdevice to use for metadata for the thin pool.

For best performance the metadata should be on a different spindle than the
data, or even better on an SSD.

If setting up a new metadata pool it is required to be valid. This can be
achieved by zeroing the first 4k to indicate empty metadata, like this:

```console
$ dd if=/dev/zero of=$metadata_dev bs=4096 count=1
```

###### Example

```console
$ sudo dockerd \
      --storage-opt dm.datadev=/dev/sdb1 \
      --storage-opt dm.metadatadev=/dev/sdc1
```

##### `dm.blocksize`

Specifies a custom blocksize to use for the thin pool. The default
blocksize is 64K.

###### Example

```console
$ sudo dockerd --storage-opt dm.blocksize=512K
```

##### `dm.blkdiscard`

Enables or disables the use of `blkdiscard` when removing devicemapper
devices. This is enabled by default (only) if using loopback devices and is
required to resparsify the loopback file on image/container removal.

Disabling this on loopback can lead to *much* faster container removal
times, but will make the space used in `/var/lib/docker` directory not be
returned to the system for other use when containers are removed.

###### Examples

```console
$ sudo dockerd --storage-opt dm.blkdiscard=false
```

##### `dm.override_udev_sync_check`

Overrides the `udev` synchronization checks between `devicemapper` and `udev`.
`udev` is the device manager for the Linux kernel.

To view the `udev` sync support of a Docker daemon that is using the
`devicemapper` driver, run:

```console
$ docker info
<...>
Udev Sync Supported: true
<...>
```

When `udev` sync support is `true`, then `devicemapper` and udev can
coordinate the activation and deactivation of devices for containers.

When `udev` sync support is `false`, a race condition occurs between
the`devicemapper` and `udev` during create and cleanup. The race condition
results in errors and failures. (For information on these failures, see
[docker#4036](https://github.com/docker/docker/issues/4036))

To allow the `docker` daemon to start, regardless of `udev` sync not being
supported, set `dm.override_udev_sync_check` to true:

```console
$ sudo dockerd --storage-opt dm.override_udev_sync_check=true
```

When this value is `true`, the  `devicemapper` continues and simply warns
you the errors are happening.

> **Note**
>
> The ideal is to pursue a `docker` daemon and environment that does
> support synchronizing with `udev`. For further discussion on this
> topic, see [docker#4036](https://github.com/docker/docker/issues/4036).
> Otherwise, set this flag for migrating existing Docker daemons to
> a daemon with a supported environment.

##### `dm.use_deferred_removal`

Enables use of deferred device removal if `libdm` and the kernel driver
support the mechanism.

Deferred device removal means that if device is busy when devices are
being removed/deactivated, then a deferred removal is scheduled on
device. And devices automatically go away when last user of the device
exits.

For example, when a container exits, its associated thin device is removed.
If that device has leaked into some other mount namespace and can't be
removed, the container exit still succeeds and this option causes the
system to schedule the device for deferred removal. It does not wait in a
loop trying to remove a busy device.

###### Example

```console
$ sudo dockerd --storage-opt dm.use_deferred_removal=true
```

##### `dm.use_deferred_deletion`

Enables use of deferred device deletion for thin pool devices. By default,
thin pool device deletion is synchronous. Before a container is deleted,
the Docker daemon removes any associated devices. If the storage driver
can not remove a device, the container deletion fails and daemon returns.

```console
Error deleting container: Error response from daemon: Cannot destroy container
```

To avoid this failure, enable both deferred device deletion and deferred
device removal on the daemon.

```console
$ sudo dockerd \
      --storage-opt dm.use_deferred_deletion=true \
      --storage-opt dm.use_deferred_removal=true
```

With these two options enabled, if a device is busy when the driver is
deleting a container, the driver marks the device as deleted. Later, when
the device isn't in use, the driver deletes it.

In general it should be safe to enable this option by default. It will help
when unintentional leaking of mount point happens across multiple mount
namespaces.

##### `dm.min_free_space`

Specifies the min free space percent in a thin pool require for new device
creation to succeed. This check applies to both free data space as well
as free metadata space. Valid values are from 0% - 99%. Value 0% disables
free space checking logic. If user does not specify a value for this option,
the Engine uses a default value of 10%.

Whenever a new a thin pool device is created (during `docker pull` or during
container creation), the Engine checks if the minimum free space is
available. If sufficient space is unavailable, then device creation fails
and any relevant `docker` operation fails.

To recover from this error, you must create more free space in the thin pool
to recover from the error. You can create free space by deleting some images
and containers from the thin pool. You can also add more storage to the thin
pool.

To add more space to a LVM (logical volume management) thin pool, just add
more storage to the volume group container thin pool; this should automatically
resolve any errors. If your configuration uses loop devices, then stop the
Engine daemon, grow the size of loop files and restart the daemon to resolve
the issue.

###### Example

```console
$ sudo dockerd --storage-opt dm.min_free_space=10%
```

##### `dm.xfs_nospace_max_retries`

Specifies the maximum number of retries XFS should attempt to complete
IO when ENOSPC (no space) error is returned by underlying storage device.

By default XFS retries infinitely for IO to finish and this can result
in unkillable process. To change this behavior one can set
xfs_nospace_max_retries to say 0 and XFS will not retry IO after getting
ENOSPC and will shutdown filesystem.

###### Example

```console
$ sudo dockerd --storage-opt dm.xfs_nospace_max_retries=0
```

##### `dm.libdm_log_level`

Specifies the maxmimum `libdm` log level that will be forwarded to the
`dockerd` log (as specified by `--log-level`). This option is primarily
intended for debugging problems involving `libdm`. Using values other than the
defaults may cause false-positive warnings to be logged.

Values specified must fall within the range of valid `libdm` log levels. At the
time of writing, the following is the list of `libdm` log levels as well as
their corresponding levels when output by `dockerd`.

| `libdm` Level | Value | `--log-level` |
|---------------|------:|---------------|
| `_LOG_FATAL`  |     2 | error         |
| `_LOG_ERR`    |     3 | error         |
| `_LOG_WARN`   |     4 | warn          |
| `_LOG_NOTICE` |     5 | info          |
| `_LOG_INFO`   |     6 | info          |
| `_LOG_DEBUG`  |     7 | debug         |

###### Example

```console
$ sudo dockerd \
      --log-level debug \
      --storage-opt dm.libdm_log_level=7
```

#### ZFS options

##### `zfs.fsname`

Set zfs filesystem under which docker will create its own datasets.
By default docker will pick up the zfs filesystem where docker graph
(`/var/lib/docker`) is located.

###### Example

```console
$ sudo dockerd -s zfs --storage-opt zfs.fsname=zroot/docker
```

#### Btrfs options

##### `btrfs.min_space`

Specifies the minimum size to use when creating the subvolume which is used
for containers. If user uses disk quota for btrfs when creating or running
a container with **--storage-opt size** option, docker should ensure the
**size** cannot be smaller than **btrfs.min_space**.

###### Example

```console
$ sudo dockerd -s btrfs --storage-opt btrfs.min_space=10G
```

#### Overlay2 options

##### `overlay2.size`

Sets the default max size of the container. It is supported only when the
backing fs is `xfs` and mounted with `pquota` mount option. Under these
conditions the user can pass any size less than the backing fs size.

###### Example

```console
$ sudo dockerd -s overlay2 --storage-opt overlay2.size=1G
```


#### Windowsfilter options

##### `size`

Specifies the size to use when creating the sandbox which is used for containers.
Defaults to 20G.

###### Example

```powershell
C:\> dockerd --storage-opt size=40G
```

### Docker runtime execution options

The Docker daemon relies on a
[OCI](https://github.com/opencontainers/runtime-spec) compliant runtime
(invoked via the `containerd` daemon) as its interface to the Linux
kernel `namespaces`, `cgroups`, and `SELinux`.

By default, the Docker daemon automatically starts `containerd`. If you want to
control `containerd` startup, manually start `containerd` and pass the path to
the `containerd` socket using the `--containerd` flag. For example:

```console
$ sudo dockerd --containerd /var/run/dev/docker-containerd.sock
```

Runtimes can be registered with the daemon either via the
configuration file or using the `--add-runtime` command line argument.

The following is an example adding 2 runtimes via the configuration:

```json
{
  "default-runtime": "runc",
  "runtimes": {
    "custom": {
      "path": "/usr/local/bin/my-runc-replacement",
      "runtimeArgs": [
        "--debug"
      ]
    },
    "runc": {
      "path": "runc"
    }
  }
}
```

This is the same example via the command line:

```console
$ sudo dockerd --add-runtime runc=runc --add-runtime custom=/usr/local/bin/my-runc-replacement
```

> **Note**
>
> Defining runtime arguments via the command line is not supported.

#### Options for the runtime

You can configure the runtime using options specified
with the `--exec-opt` flag. All the flag's options have the `native` prefix. A
single `native.cgroupdriver` option is available.

The `native.cgroupdriver` option specifies the management of the container's
cgroups. You can only specify `cgroupfs` or `systemd`. If you specify
`systemd` and it is not available, the system errors out. If you omit the
`native.cgroupdriver` option,` cgroupfs` is used on cgroup v1 hosts, `systemd`
is used on cgroup v2 hosts with systemd available.

This example sets the `cgroupdriver` to `systemd`:

```console
$ sudo dockerd --exec-opt native.cgroupdriver=systemd
```

Setting this option applies to all containers the daemon launches.

Also Windows Container makes use of `--exec-opt` for special purpose. Docker user
can specify default container isolation technology with this, for example:

```console
> dockerd --exec-opt isolation=hyperv
```

Will make `hyperv` the default isolation technology on Windows. If no isolation
value is specified on daemon start, on Windows client, the default is
`hyperv`, and on Windows server, the default is `process`.

### Daemon DNS options

To set the DNS server for all Docker containers, use:

```console
$ sudo dockerd --dns 8.8.8.8
```

To set the DNS search domain for all Docker containers, use:

```console
$ sudo dockerd --dns-search example.com
```

### Allow push of nondistributable artifacts

Some images (e.g., Windows base images) contain artifacts whose distribution is
restricted by license. When these images are pushed to a registry, restricted
artifacts are not included.

To override this behavior for specific registries, use the
`--allow-nondistributable-artifacts` option in one of the following forms:

* `--allow-nondistributable-artifacts myregistry:5000` tells the Docker daemon
  to push nondistributable artifacts to myregistry:5000.
* `--allow-nondistributable-artifacts 10.1.0.0/16` tells the Docker daemon to
  push nondistributable artifacts to all registries whose resolved IP address
  is within the subnet described by the CIDR syntax.

This option can be used multiple times.

This option is useful when pushing images containing nondistributable artifacts
to a registry on an air-gapped network so hosts on that network can pull the
images without connecting to another server.

> **Warning**: Nondistributable artifacts typically have restrictions on how
> and where they can be distributed and shared. Only use this feature to push
> artifacts to private registries and ensure that you are in compliance with
> any terms that cover redistributing nondistributable artifacts.

### Insecure registries

Docker considers a private registry either secure or insecure. In the rest of
this section, *registry* is used for *private registry*, and `myregistry:5000`
is a placeholder example for a private registry.

A secure registry uses TLS and a copy of its CA certificate is placed on the
Docker host at `/etc/docker/certs.d/myregistry:5000/ca.crt`. An insecure
registry is either not using TLS (i.e., listening on plain text HTTP), or is
using TLS with a CA certificate not known by the Docker daemon. The latter can
happen when the certificate was not found under
`/etc/docker/certs.d/myregistry:5000/`, or if the certificate verification
failed (i.e., wrong CA).

By default, Docker assumes all, but local (see local registries below),
registries are secure. Communicating with an insecure registry is not possible
if Docker assumes that registry is secure. In order to communicate with an
insecure registry, the Docker daemon requires `--insecure-registry` in one of
the following two forms:

* `--insecure-registry myregistry:5000` tells the Docker daemon that
  myregistry:5000 should be considered insecure.
* `--insecure-registry 10.1.0.0/16` tells the Docker daemon that all registries
  whose domain resolve to an IP address is part of the subnet described by the
  CIDR syntax, should be considered insecure.

The flag can be used multiple times to allow multiple registries to be marked
as insecure.

If an insecure registry is not marked as insecure, `docker pull`,
`docker push`, and `docker search` will result in an error message prompting
the user to either secure or pass the `--insecure-registry` flag to the Docker
daemon as described above.

Local registries, whose IP address falls in the 127.0.0.0/8 range, are
automatically marked as insecure as of Docker 1.3.2. It is not recommended to
rely on this, as it may change in the future.

Enabling `--insecure-registry`, i.e., allowing un-encrypted and/or untrusted
communication, can be useful when running a local registry.  However,
because its use creates security vulnerabilities it should ONLY be enabled for
testing purposes.  For increased security, users should add their CA to their
system's list of trusted CAs instead of enabling `--insecure-registry`.

#### Legacy Registries

Operations against registries supporting only the legacy v1 protocol are no longer
supported. Specifically, the daemon will not attempt `push`, `pull` and `login`
to v1 registries. The exception to this is `search` which can still be performed
on v1 registries.


### Running a Docker daemon behind an HTTPS_PROXY

When running inside a LAN that uses an `HTTPS` proxy, the Docker Hub
certificates will be replaced by the proxy's certificates. These certificates
need to be added to your Docker host's configuration:

1. Install the `ca-certificates` package for your distribution
2. Ask your network admin for the proxy's CA certificate and append them to
   `/etc/pki/tls/certs/ca-bundle.crt`
3. Then start your Docker daemon with `HTTPS_PROXY=http://username:password@proxy:port/ dockerd`.
   The `username:` and `password@` are optional - and are only needed if your
   proxy is set up to require authentication.

This will only add the proxy and authentication to the Docker daemon's requests -
your `docker build`s and running containers will need extra configuration to
use the proxy

### Default `ulimit` settings

`--default-ulimit` allows you to set the default `ulimit` options to use for
all containers. It takes the same options as `--ulimit` for `docker run`. If
these defaults are not set, `ulimit` settings will be inherited, if not set on
`docker run`, from the Docker daemon. Any `--ulimit` options passed to
`docker run` will overwrite these defaults.

Be careful setting `nproc` with the `ulimit` flag as `nproc` is designed by Linux to
set the maximum number of processes available to a user, not to a container. For details
please check the [run](run.md) reference.

### Access authorization

Docker's access authorization can be extended by authorization plugins that your
organization can purchase or build themselves. You can install one or more
authorization plugins when you start the Docker `daemon` using the
`--authorization-plugin=PLUGIN_ID` option.

```console
$ sudo dockerd --authorization-plugin=plugin1 --authorization-plugin=plugin2,...
```

The `PLUGIN_ID` value is either the plugin's name or a path to its specification
file. The plugin's implementation determines whether you can specify a name or
path. Consult with your Docker administrator to get information about the
plugins available to you.

Once a plugin is installed, requests made to the `daemon` through the
command line or Docker's Engine API are allowed or denied by the plugin.
If you have multiple plugins installed, each plugin, in order, must
allow the request for it to complete.

For information about how to create an authorization plugin, refer to the
[authorization plugin](../../extend/plugins_authorization.md) section.


### Daemon user namespace options

The Linux kernel
[user namespace support](https://man7.org/linux/man-pages/man7/user_namespaces.7.html)
provides additional security by enabling a process, and therefore a container,
to have a unique range of user and group IDs which are outside the traditional
user and group range utilized by the host system. Potentially the most important
security improvement is that, by default, container processes running as the
`root` user will have expected administrative privilege (with some restrictions)
inside the container but will effectively be mapped to an unprivileged `uid` on
the host.

For details about how to use this feature, as well as limitations, see
[Isolate containers with a user namespace](https://docs.docker.com/engine/security/userns-remap/).

### Miscellaneous options

IP masquerading uses address translation to allow containers without a public
IP to talk to other machines on the Internet. This may interfere with some
network topologies and can be disabled with `--ip-masq=false`.

Docker supports softlinks for the Docker data directory (`/var/lib/docker`) and
for `/var/lib/docker/tmp`. The `DOCKER_TMPDIR` and the data directory can be
set like this:

```console
$ DOCKER_TMPDIR=/mnt/disk2/tmp /usr/local/bin/dockerd --data-root /var/lib/docker -H unix:// > /var/lib/docker-machine/docker.log 2>&1
```

or

```console
$ export DOCKER_TMPDIR=/mnt/disk2/tmp
$ /usr/local/bin/dockerd --data-root /var/lib/docker -H unix:// > /var/lib/docker-machine/docker.log 2>&1
````

#### Default cgroup parent

The `--cgroup-parent` option allows you to set the default cgroup parent
to use for containers. If this option is not set, it defaults to `/docker` for
fs cgroup driver and `system.slice` for systemd cgroup driver.

If the cgroup has a leading forward slash (`/`), the cgroup is created
under the root cgroup, otherwise the cgroup is created under the daemon
cgroup.

Assuming the daemon is running in cgroup `daemoncgroup`,
`--cgroup-parent=/foobar` creates a cgroup in
`/sys/fs/cgroup/memory/foobar`, whereas using `--cgroup-parent=foobar`
creates the cgroup in `/sys/fs/cgroup/memory/daemoncgroup/foobar`

The systemd cgroup driver has different rules for `--cgroup-parent`. Systemd
represents hierarchy by slice and the name of the slice encodes the location in
the tree. So `--cgroup-parent` for systemd cgroups should be a slice name. A
name can consist of a dash-separated series of names, which describes the path
to the slice from the root slice. For example, `--cgroup-parent=user-a-b.slice`
means the memory cgroup for the container is created in
`/sys/fs/cgroup/memory/user.slice/user-a.slice/user-a-b.slice/docker-<id>.scope`.

This setting can also be set per container, using the `--cgroup-parent`
option on `docker create` and `docker run`, and takes precedence over
the `--cgroup-parent` option on the daemon.

#### Daemon metrics

The `--metrics-addr` option takes a tcp address to serve the metrics API.
This feature is still experimental, therefore, the daemon must be running in experimental
mode for this feature to work.

To serve the metrics API on `localhost:9323` you would specify `--metrics-addr 127.0.0.1:9323`,
allowing you to make requests on the API at `127.0.0.1:9323/metrics` to receive metrics in the
[prometheus](https://prometheus.io/docs/instrumenting/exposition_formats/) format.

Port `9323` is the [default port associated with Docker
metrics](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)
to avoid collisions with other prometheus exporters and services.

If you are running a prometheus server you can add this address to your scrape configs
to have prometheus collect metrics on Docker.  For more information
on prometheus refer to the [prometheus website](https://prometheus.io/).

```yaml
scrape_configs:
  - job_name: 'docker'
    static_configs:
      - targets: ['127.0.0.1:9323']
```

Please note that this feature is still marked as experimental as metrics and metric
names could change while this feature is still in experimental.  Please provide
feedback on what you would like to see collected in the API.

#### Node Generic Resources

The `--node-generic-resources` option takes a list of key-value
pair (`key=value`) that allows you to advertise user defined resources
in a swarm cluster.

The current expected use case is to advertise NVIDIA GPUs so that services
requesting `NVIDIA-GPU=[0-16]` can land on a node that has enough GPUs for
the task to run.

Example of usage:
```json
{
  "node-generic-resources": [
    "NVIDIA-GPU=UUID1",
    "NVIDIA-GPU=UUID2"
  ]
}
```

### Daemon configuration file

The `--config-file` option allows you to set any configuration option
for the daemon in a JSON format. This file uses the same flag names as keys,
except for flags that allow several entries, where it uses the plural
of the flag name, e.g., `labels` for the `label` flag.

The options set in the configuration file must not conflict with options set
via flags. The docker daemon fails to start if an option is duplicated between
the file and the flags, regardless their value. We do this to avoid
silently ignore changes introduced in configuration reloads.
For example, the daemon fails to start if you set daemon labels
in the configuration file and also set daemon labels via the `--label` flag.
Options that are not present in the file are ignored when the daemon starts.

The `--validate` option allows to validate a configuration file without
starting the Docker daemon. A non-zero exit code is returned for invalid
configuration files.

```console
$ dockerd --validate --config-file=/tmp/valid-config.json
configuration OK

$ echo $?
0

$ dockerd --validate --config-file /tmp/invalid-config.json
unable to configure the Docker daemon with file /tmp/invalid-config.json: the following directives don't match any configuration option: unknown-option

$ echo $?
1
```


##### On Linux

The default location of the configuration file on Linux is
`/etc/docker/daemon.json`. The `--config-file` flag can be used to specify a
 non-default location.

This is a full example of the allowed configuration options on Linux:

```json
{
  "allow-nondistributable-artifacts": [],
  "api-cors-header": "",
  "authorization-plugins": [],
  "bip": "",
  "bridge": "",
  "cgroup-parent": "",
  "containerd": "/run/containerd/containerd.sock",
  "containerd-namespace": "docker",
  "containerd-plugin-namespace": "docker-plugins",
  "data-root": "",
  "debug": true,
  "default-address-pools": [
    {
      "base": "172.30.0.0/16",
      "size": 24
    },
    {
      "base": "172.31.0.0/16",
      "size": 24
    }
  ],
  "default-cgroupns-mode": "private",
  "default-gateway": "",
  "default-gateway-v6": "",
  "default-runtime": "runc",
  "default-shm-size": "64M",
  "default-ulimits": {
    "nofile": {
      "Hard": 64000,
      "Name": "nofile",
      "Soft": 64000
    }
  },
  "dns": [],
  "dns-opts": [],
  "dns-search": [],
  "exec-opts": [],
  "exec-root": "",
  "experimental": false,
  "features": {},
  "fixed-cidr": "",
  "fixed-cidr-v6": "",
  "group": "",
  "hosts": [],
  "icc": false,
  "init": false,
  "init-path": "/usr/libexec/docker-init",
  "insecure-registries": [],
  "ip": "0.0.0.0",
  "ip-forward": false,
  "ip-masq": false,
  "iptables": false,
  "ip6tables": false,
  "ipv6": false,
  "labels": [],
  "live-restore": true,
  "log-driver": "json-file",
  "log-level": "",
  "log-opts": {
    "cache-disabled": "false",
    "cache-max-file": "5",
    "cache-max-size": "20m",
    "cache-compress": "true",
    "env": "os,customer",
    "labels": "somelabel",
    "max-file": "5",
    "max-size": "10m"
  },
  "max-concurrent-downloads": 3,
  "max-concurrent-uploads": 5,
  "max-download-attempts": 5,
  "mtu": 0,
  "no-new-privileges": false,
  "node-generic-resources": [
    "NVIDIA-GPU=UUID1",
    "NVIDIA-GPU=UUID2"
  ],
  "oom-score-adjust": -500,
  "pidfile": "",
  "raw-logs": false,
  "registry-mirrors": [],
  "runtimes": {
    "cc-runtime": {
      "path": "/usr/bin/cc-runtime"
    },
    "custom": {
      "path": "/usr/local/bin/my-runc-replacement",
      "runtimeArgs": [
        "--debug"
      ]
    }
  },
  "seccomp-profile": "",
  "selinux-enabled": false,
  "shutdown-timeout": 15,
  "storage-driver": "",
  "storage-opts": [],
  "swarm-default-advertise-addr": "",
  "tls": true,
  "tlscacert": "",
  "tlscert": "",
  "tlskey": "",
  "tlsverify": true,
  "userland-proxy": false,
  "userland-proxy-path": "/usr/libexec/docker-proxy",
  "userns-remap": ""
}
```

> **Note:**
>
> You cannot set options in `daemon.json` that have already been set on
> daemon startup as a flag.
> On systems that use `systemd` to start the Docker daemon, `-H` is already set, so
> you cannot use the `hosts` key in `daemon.json` to add listening addresses.
> See ["custom Docker daemon options"](https://docs.docker.com/config/daemon/systemd/#custom-docker-daemon-options) for how
> to accomplish this task with a systemd drop-in file.

##### On Windows

The default location of the configuration file on Windows is
 `%programdata%\docker\config\daemon.json`. The `--config-file` flag can be
 used to specify a non-default location.

This is a full example of the allowed configuration options on Windows:

```json
{
  "allow-nondistributable-artifacts": [],
  "authorization-plugins": [],
  "bridge": "",
  "containerd": "\\\\.\\pipe\\containerd-containerd",
  "containerd-namespace": "docker",
  "containerd-plugin-namespace": "docker-plugins",
  "data-root": "",
  "debug": true,
  "default-runtime": "",
  "default-ulimits": {},
  "dns": [],
  "dns-opts": [],
  "dns-search": [],
  "exec-opts": [],
  "experimental": false,
  "features": {},
  "fixed-cidr": "",
  "group": "",
  "hosts": [],
  "insecure-registries": [],
  "labels": [],
  "log-driver": "",
  "log-level": "",
  "max-concurrent-downloads": 3,
  "max-concurrent-uploads": 5,
  "max-download-attempts": 5,
  "mtu": 0,
  "pidfile": "",
  "raw-logs": false,
  "registry-mirrors": [],
  "shutdown-timeout": 15,
  "storage-driver": "",
  "storage-opts": [],
  "swarm-default-advertise-addr": "",
  "tlscacert": "",
  "tlscert": "",
  "tlskey": "",
  "tlsverify": true
}
```

The `default-runtime` option is by default unset, in which case dockerd will auto-detect the runtime. This detection is currently based on if the `containerd` flag is set.

Accepted values:

- `com.docker.hcsshim.v1` - This is the built-in runtime that Docker has used since Windows supported was first added and uses the v1 HCS API's in Windows.
- `io.containerd.runhcs.v1` - This is uses the containerd `runhcs` shim to run the container and uses the v2 HCS API's in Windows.

#### Feature options
The optional field `features` in `daemon.json` allows users to enable or disable specific
daemon features. For example, `{"features":{"buildkit": true}}` enables `buildkit` as the
default docker image builder.

The list of currently supported feature options:
- `buildkit`: It enables `buildkit` as default builder when set to `true` or disables it by
`false`. Note that if this option is not explicitly set in the daemon config file, then it
is up to the cli to determine which builder to invoke.

#### Configuration reload behavior

Some options can be reconfigured when the daemon is running without requiring
to restart the process. We use the `SIGHUP` signal in Linux to reload, and a global event
in Windows with the key `Global\docker-daemon-config-$PID`. The options can
be modified in the configuration file but still will check for conflicts with
the provided flags. The daemon fails to reconfigure itself
if there are conflicts, but it won't stop execution.

The list of currently supported options that can be reconfigured is this:

- `debug`: it changes the daemon to debug mode when set to true.
- `labels`: it replaces the daemon labels with a new set of labels.
- `live-restore`: Enables [keeping containers alive during daemon downtime](https://docs.docker.com/config/containers/live-restore/).
- `max-concurrent-downloads`: it updates the max concurrent downloads for each pull.
- `max-concurrent-uploads`: it updates the max concurrent uploads for each push.
- `max-download-attempts`: it updates the max download attempts for each pull.
- `default-runtime`: it updates the runtime to be used if not is
  specified at container creation. It defaults to "default" which is
  the runtime shipped with the official docker packages.
- `runtimes`: it updates the list of available OCI runtimes that can
  be used to run containers.
- `authorization-plugin`: it specifies the authorization plugins to use.
- `allow-nondistributable-artifacts`: Replaces the set of registries to which the daemon will push nondistributable artifacts with a new set of registries.
- `insecure-registries`: it replaces the daemon insecure registries with a new set of insecure registries. If some existing insecure registries in daemon's configuration are not in newly reloaded insecure registries, these existing ones will be removed from daemon's config.
- `registry-mirrors`: it replaces the daemon registry mirrors with a new set of registry mirrors. If some existing registry mirrors in daemon's configuration are not in newly reloaded registry mirrors, these existing ones will be removed from daemon's config.
- `shutdown-timeout`: it replaces the daemon's existing configuration timeout with a new timeout for shutting down all containers.
- `features`: it explicitly enables or disables specific features.

### Run multiple daemons

> **Note:**
>
> Running multiple daemons on a single host is considered as "experimental". The user should be aware of
> unsolved problems. This solution may not work properly in some cases. Solutions are currently under development
> and will be delivered in the near future.

This section describes how to run multiple Docker daemons on a single host. To
run multiple daemons, you must configure each daemon so that it does not
conflict with other daemons on the same host. You can set these options either
by providing them as flags, or by using a [daemon configuration file](#daemon-configuration-file).

The following daemon options must be configured for each daemon:

```console
-b, --bridge=                          Attach containers to a network bridge
--exec-root=/var/run/docker            Root of the Docker execdriver
--data-root=/var/lib/docker            Root of persisted Docker data
-p, --pidfile=/var/run/docker.pid      Path to use for daemon PID file
-H, --host=[]                          Daemon socket(s) to connect to
--iptables=true                        Enable addition of iptables rules
--config-file=/etc/docker/daemon.json  Daemon configuration file
--tlscacert="~/.docker/ca.pem"         Trust certs signed only by this CA
--tlscert="~/.docker/cert.pem"         Path to TLS certificate file
--tlskey="~/.docker/key.pem"           Path to TLS key file
```

When your daemons use different values for these flags, you can run them on the same host without any problems.
It is very important to properly understand the meaning of those options and to use them correctly.

- The `-b, --bridge=` flag is set to `docker0` as default bridge network. It is created automatically when you install Docker.
If you are not using the default, you must create and configure the bridge manually or just set it to 'none': `--bridge=none`
- `--exec-root` is the path where the container state is stored. The default value is `/var/run/docker`. Specify the path for
your running daemon here.
- `--data-root` is the path where persisted data such as images, volumes, and
cluster state are stored. The default value is `/var/lib/docker`. To avoid any
conflict with other daemons, set this parameter separately for each daemon.
- `-p, --pidfile=/var/run/docker.pid` is the path where the process ID of the daemon is stored. Specify the path for your
pid file here.
- `--host=[]` specifies where the Docker daemon will listen for client connections. If unspecified, it defaults to `/var/run/docker.sock`.
-  `--iptables=false` prevents the Docker daemon from adding iptables rules. If
multiple daemons manage iptables rules, they may overwrite rules set by another
daemon. Be aware that disabling this option requires you to manually add
iptables rules to expose container ports. If you prevent Docker from adding
iptables rules, Docker will also not add IP masquerading rules, even if you set
`--ip-masq` to `true`. Without IP masquerading rules, Docker containers will not be
able to connect to external hosts or the internet when using network other than
default bridge.
- `--config-file=/etc/docker/daemon.json` is the path where configuration file is stored. You can use it instead of
daemon flags. Specify the path for each daemon.
- `--tls*` Docker daemon supports `--tlsverify` mode that enforces encrypted and authenticated remote connections.
The `--tls*` options enable use of specific certificates for individual daemons.

Example script for a separate “bootstrap” instance of the Docker daemon without network:

```console
$ sudo dockerd \
        -H unix:///var/run/docker-bootstrap.sock \
        -p /var/run/docker-bootstrap.pid \
        --iptables=false \
        --ip-masq=false \
        --bridge=none \
        --data-root=/var/lib/docker-bootstrap \
        --exec-root=/var/run/docker-bootstrap
```
